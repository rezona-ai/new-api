package relay

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
)

// 生图响应体的无损改写（设计文档 docs/superpowers/specs/2026-08-06-image-gen-cdn-design.md 4.3）。
//
// 为什么不能走 dto.ImageResponse 反序列化再重新 marshal：
//   - dto.ImageData 的 url / b64_json 没有 omitempty（dto/openai_image.go:180-184），
//     "删字段"会变成 "url":""；
//   - 顶层 usage、供应商扩展字段（如 MiniMax 的 metadata）会被整体丢弃，而
//     OpenAI 兼容渠道当前是完整原样透传，任何字段丢失都是可见的行为回退。
//
// 因此这里只在 map[string]json.RawMessage 层面增删 url / b64_json 两个键。

// imageEntityHeadersToStrip 改写 body 后必须删除的实体校验类 header——
// 它们描述的是上游那份 representation，与新 body 不再一致。
// service.IOCopyBytesGracefully 的白名单只过滤 Content-Length 与本地 request-id
// 及 provider 内部 header（service/http.go:45），这些会被原样放行。
var imageEntityHeadersToStrip = []string{
	"ETag",
	"Content-MD5",
	"Digest",
	"Content-Digest",
	"Repr-Digest",
	"Content-Encoding",
	"Content-Range",
	"Last-Modified",
}

// errImageBodyNotRecognized body 不是可识别的生图响应，调用方原样透传。
var errImageBodyNotRecognized = errors.New("image response body not recognized")

// imageTransferFunc 由调用方注入的转存实现：返回 nil 表示该张失败（调用方保留上游原值）。
type imageTransferFunc func(index int, src service.ImageSource, wantBytes bool) *service.ImageTransferResult

// rewriteImageResponseBody 对生图响应体做无损改写。
//
// responseFormat 为客户端传入的原始值（"url" / "b64_json" / ""）。
// dropMetadata 为真时（仅 Ali 渠道）在至少一张转存成功后删除顶层 metadata。
//
// 返回 (newBody, rewrote, err)：
//   - err != nil：body 不可识别，调用方原样透传；
//   - rewrote == false：没有任何一张被改写，newBody 等于原始 body。
func rewriteImageResponseBody(body []byte, responseFormat string, dropMetadata bool, transfer imageTransferFunc) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := common.Unmarshal(body, &top); err != nil {
		return body, false, fmt.Errorf("%w: %v", errImageBodyNotRecognized, err)
	}
	rawData, ok := top["data"]
	if !ok {
		return body, false, fmt.Errorf("%w: no data field", errImageBodyNotRecognized)
	}
	var data []map[string]json.RawMessage
	if err := common.Unmarshal(rawData, &data); err != nil {
		return body, false, fmt.Errorf("%w: data is not an array of objects: %v", errImageBodyNotRecognized, err)
	}
	if len(data) == 0 {
		return body, false, fmt.Errorf("%w: empty data array", errImageBodyNotRecognized)
	}

	format := strings.ToLower(strings.TrimSpace(responseFormat))
	rewrote := false
	for i, item := range data {
		src, hasImage := imageSourceFromItem(item)
		if !hasImage {
			continue
		}
		// 只有"客户端要 b64 但上游只给了 url"时才需要把字节带回来
		wantBytes := format == "b64_json" && src.B64 == ""
		res := transfer(i, src, wantBytes)
		if res == nil {
			continue // 该张失败：完全不动，保留上游原值
		}
		if applyTransferResult(item, format, src, res) {
			rewrote = true
		}
	}
	if !rewrote {
		return body, false, nil
	}

	newData, err := common.Marshal(data)
	if err != nil {
		return body, false, fmt.Errorf("marshal rewritten data failed: %w", err)
	}
	top["data"] = newData
	if dropMetadata {
		delete(top, "metadata")
	}
	newBody, err := common.Marshal(top)
	if err != nil {
		return body, false, fmt.Errorf("marshal rewritten body failed: %w", err)
	}
	return newBody, true, nil
}

// imageSourceFromItem 从 data 元素里取转存源。base64 优先——字节已在手里，
// 省一次跨境下载与一次 SSRF 风险面（设计 3.1 末尾）；URL 仍带上供解码失败时兜底。
func imageSourceFromItem(item map[string]json.RawMessage) (service.ImageSource, bool) {
	var src service.ImageSource
	if raw, ok := item["b64_json"]; ok {
		var s string
		if err := common.Unmarshal(raw, &s); err == nil {
			src.B64 = strings.TrimSpace(s)
		}
	}
	if raw, ok := item["url"]; ok {
		var s string
		if err := common.Unmarshal(raw, &s); err == nil {
			src.URL = strings.TrimSpace(s)
		}
	}
	return src, src.B64 != "" || src.URL != ""
}

// applyTransferResult 按 response_format 适配规则（设计 3.1）增删 url / b64_json 两个键。
// 返回是否真的改动了该元素。
func applyTransferResult(item map[string]json.RawMessage, format string, src service.ImageSource, res *service.ImageTransferResult) bool {
	switch format {
	case "url":
		if res.SignedURL == "" {
			return false
		}
		if !setJSONString(item, "url", res.SignedURL) {
			return false
		}
		delete(item, "b64_json")
		return true

	case "b64_json":
		b64 := src.B64
		if b64 == "" && len(res.Raw) > 0 {
			b64 = base64.StdEncoding.EncodeToString(res.Raw)
		}
		if b64 == "" {
			return false
		}
		if !setJSONString(item, "b64_json", b64) {
			return false
		}
		delete(item, "url")
		return true

	default:
		// 未传 response_format：按上游形态适配，且永远补上 url。
		// 上游给 base64 时 b64_json 原样保留——现有依赖它的客户端一个字段都不能丢。
		if res.SignedURL == "" {
			return false
		}
		if !setJSONString(item, "url", res.SignedURL) {
			return false
		}
		if setting.GCSImageStripB64WhenURL {
			delete(item, "b64_json")
		}
		return true
	}
}

// setJSONString 把字符串按 JSON 规则写入指定键。
func setJSONString(item map[string]json.RawMessage, key, value string) bool {
	encoded, err := common.Marshal(value)
	if err != nil {
		return false
	}
	item[key] = encoded
	return true
}
