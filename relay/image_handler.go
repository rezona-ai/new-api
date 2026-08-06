package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func ImageHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	imageReq, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.ImageRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(imageReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to ImageRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	var requestBody io.Reader

	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		requestBody = common.ReaderOnly(storage)
	} else {
		convertedRequest, err := adaptor.ConvertImageRequest(c, info, *request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed)
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

		switch convertedRequest.(type) {
		case *bytes.Buffer:
			requestBody = convertedRequest.(io.Reader)
		default:
			jsonData, err := common.Marshal(convertedRequest)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}

			// apply param override
			if len(info.ParamOverride) > 0 {
				jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
				if err != nil {
					return newAPIErrorFromParamOverride(err)
				}
			}

			logger.LogDebug(c, "image request body: %s", jsonData)
			body, size, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
			defer closer.Close()
			jsonData = nil
			info.UpstreamRequestBodySize = size
			requestBody = body
		}
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			if httpResp.StatusCode == http.StatusCreated && info.ApiType == constant.APITypeReplicate {
				// replicate channel returns 201 Created when using Prefer: wait, treat it as success.
				httpResp.StatusCode = http.StatusOK
			} else {
				newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
				// reset status code 重置状态码
				service.ResetStatusCode(newAPIError, statusCodeMappingStr)
				return newAPIError
			}
		}
	}

	// 生图转存收口（设计 4.2.2）：安装缓冲 writer，defer 兜底恢复。
	// 红线：任何返回路径都必须让 writer 回到可用状态——未提交也未丢弃时按原样提交，
	// 绝不能让用户拿到空响应（额度已预扣）。
	captureWriter, captured := installImageCapture(c, info)
	if captured {
		defer func() {
			if !captureWriter.Committed() {
				if err := captureWriter.Commit(); err != nil {
					logger.LogError(c, "gcs-image commit response failed: "+err.Error())
				}
			}
			c.Writer = captureWriter.Unwrap()
		}()
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		if captured {
			// 渠道返回错误：丢弃缓冲、底层 header 不动，让外层重试循环
			//（controller/relay.go:190）与统一错误响应（:89）拿到干净的 writer
			captureWriter.Discard()
			c.Writer = captureWriter.Unwrap()
			captured = false
		}
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}

	if captured {
		newBody := transferAndRewriteImageResponse(c, info, captureWriter, request.ResponseFormat)
		if err := commitImageResponse(c, captureWriter, newBody); err != nil {
			// 写客户端失败只记日志：响应可能已部分提交，返回 relay error 会触发
			// 渠道重试与重复响应，且必须照常结算（设计 决策 8）
			logger.LogError(c, "gcs-image write response failed: "+err.Error())
		}
		c.Writer = captureWriter.Unwrap()
		captured = false
	}

	imageN := uint(1)
	if request.N != nil {
		imageN = *request.N
	}

	// n is handled via OtherRatio so it is applied exactly once in quota
	// calculation (both price-based and ratio-based paths).
	// Adaptors may have already set a more accurate count from the
	// upstream response; only set the default when they haven't.
	if info.PriceData.UsePrice { // only price model use N ratio
		if _, hasN := info.PriceData.OtherRatios["n"]; !hasN {
			info.PriceData.AddOtherRatio("n", float64(imageN))
		}
	}

	if usage.(*dto.Usage).TotalTokens == 0 {
		usage.(*dto.Usage).TotalTokens = 1
	}
	if usage.(*dto.Usage).PromptTokens == 0 {
		usage.(*dto.Usage).PromptTokens = 1
	}

	quality := request.Quality
	if quality == "" {
		quality = "standard"
	}

	var logContent []string

	if len(request.Size) > 0 {
		logContent = append(logContent, fmt.Sprintf("大小 %s", request.Size))
	}
	if len(quality) > 0 {
		logContent = append(logContent, fmt.Sprintf("品质 %s", quality))
	}
	if imageN > 0 {
		logContent = append(logContent, fmt.Sprintf("生成数量 %d", imageN))
	}

	service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), logContent)
	return nil
}

// ── 生图结果转存 GCS 的接线（设计 4.2）──
//
// 收口方式：给 c.Writer 套一层缓冲 writer，把各渠道 DoResponse 写出的响应体
// 截下来无损改写后再下发。一处收口即覆盖全部图片渠道，无需逐渠道改动。

// imageObjectNamer 生成对象命名器：{prefix}/{yyyymmdd}/{requestID}_{index}_{rand4}.{ext}
//
//   - requestID 便于按请求追溯；
//   - rand4 保证跨渠道重试、请求 id 碰撞都不会撞对象名（配合 ReuseExisting=false，
//     因为条件写的 412 可能在 Close() 才返回，此时 reader 已消费、无法重放）。
//
// 同一个 namer 对同一 index 返回稳定结果。
func imageObjectNamer(prefix, requestID, day string) service.ObjectNamer {
	if requestID == "" {
		requestID = "unknown"
	}
	suffixes := make(map[int]string)
	return func(index int, ext string) string {
		suffix, ok := suffixes[index]
		if !ok {
			suffix = randomHex4()
			suffixes[index] = suffix
		}
		return fmt.Sprintf("%s/%s/%s_%d_%s.%s", prefix, day, requestID, index, suffix, ext)
	}
}

func randomHex4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

// installImageCapture 在满足条件时安装缓冲 writer，返回 (writer, 是否已安装)。
// 流式响应（SSE partial_images）不安装——它无法缓冲改写。
func installImageCapture(c *gin.Context, info *relaycommon.RelayInfo) (*helper.CapturingWriter, bool) {
	if !service.GCSImageTransferReady() || info.IsStream {
		return nil, false
	}
	cw := helper.NewCapturingWriter(c.Writer, setting.GCSImageCaptureMax)
	c.Writer = cw
	return cw, true
}

// commitImageResponse 提交响应：newBody 非 nil 表示改写成功（重设 Content-Length
// 并删除实体校验 header）；nil 表示原样透传。
func commitImageResponse(c *gin.Context, cw *helper.CapturingWriter, newBody []byte) error {
	if newBody == nil {
		return cw.Commit()
	}
	return cw.CommitBody(newBody, func(h http.Header) {
		for _, key := range imageEntityHeadersToStrip {
			h.Del(key)
		}
	})
}

// transferAndRewriteImageResponse 尝试改写缓冲中的生图响应体。
// 返回 nil 表示不改写（调用方原样透传）。任何失败都只记指标与日志。
//
// 转存按张同步调用 service.TransferImage（不走 TransferImages）：改写层需要把
// 每张的结果精确对回它在 data[] 里的下标，而 index 同时决定对象名。
// 指标埋点因此由本层显式调用 Record* 系列——TransferImages 内部的埋点只服务于
// 第 2/3 期的批量路径，两条路径各记一次，不会重复计数。
func transferAndRewriteImageResponse(c *gin.Context, info *relaycommon.RelayInfo, cw *helper.CapturingWriter, responseFormat string) []byte {
	if cw.Committed() || cw.CapturedStatus() != http.StatusOK {
		return nil
	}
	if ct := cw.Header().Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return nil
	}

	namer := imageObjectNamer(setting.GCSImagePrefix, info.RequestId, info.StartTime.Format("20060102"))
	channelLabel := fmt.Sprintf("%d", info.ChannelType)
	baseOpts := service.TransferOpts{
		Timeout: setting.GCSImageTransferTimeout,
		Sign:    true,
		// 一次性对象、只访问一次：CacheTag 留空表示不入签名缓存，
		// 否则每张图都会在缓存里留下一个永不复用的条目（设计 4.4）
		SignPolicy:       service.SignPolicy{TTL: setting.GCSImageSignedURLTTL, CacheTag: ""},
		ChannelTypeLabel: channelLabel,
	}

	transfer := func(index int, src service.ImageSource, wantBytes bool) *service.ImageTransferResult {
		opts := baseOpts
		opts.WantBytes = wantBytes
		start := time.Now()
		res, err := service.TransferImage(c.Request.Context(), namer, index, src, opts)
		if err != nil {
			service.RecordImageTransferFailure(c, index, err)
			return nil
		}
		service.RecordImageTransferSuccess(channelLabel, time.Since(start))
		return res
	}

	dropMetadata := setting.GCSImageDropAliMetadata && info.ChannelType == constant.ChannelTypeAli
	newBody, rewrote, err := rewriteImageResponseBody(cw.Body(), responseFormat, dropMetadata, transfer)
	if err != nil || !rewrote {
		service.RecordImagePassthrough()
		if err != nil {
			logger.LogDebug(c, "gcs-image passthrough: %s", err.Error())
		}
		return nil
	}
	return newBody
}
