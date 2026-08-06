package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting"
)

// 生图结果转存 GCS：转存原语（设计文档 docs/superpowers/specs/2026-08-06-image-gen-cdn-design.md 4.1）。
//
// 与视频转存的本质差异：生图是同步请求，转存必须在响应下发前完成，没有状态机、
// 没有退款语义。因此本文件的任何失败都只返回 error，由调用方回退透传上游原始结果——
// 绝不允许转成 relay error（会触发渠道重试，最终失败还会退款）。

// gcsImageExtByMime 图片 MIME → 对象扩展名白名单。
// 扩展名一律取自该映射，禁止把上游 URL 的路径/查询串拼进对象名（设计 4.1）。
var gcsImageExtByMime = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// gcsImageMimeAliases 归一化上游的非规范 MIME 写法。
var gcsImageMimeAliases = map[string]string{
	"image/jpg": "image/jpeg",
}

// normalizeImageMime 去掉参数、转小写并归一别名；不在白名单内返回 ""。
func normalizeImageMime(raw string) string {
	mime := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if alias, ok := gcsImageMimeAliases[mime]; ok {
		mime = alias
	}
	if _, ok := gcsImageExtByMime[mime]; !ok {
		return ""
	}
	return mime
}

// resolveImageMime 按 上游 Content-Type → 调用方 hint → 内容嗅探 的顺序判定图片
// MIME 与对象扩展名。三者都无法判定（或判出非白名单类型）时 ok=false，
// 调用方按转存失败处理并回退透传——绝不上传未知类型。
func resolveImageMime(upstreamCT, hint string, head []byte) (string, string, bool) {
	for _, candidate := range []string{upstreamCT, hint} {
		if mime := normalizeImageMime(candidate); mime != "" {
			return mime, gcsImageExtByMime[mime], true
		}
	}
	if len(head) > 0 {
		if mime := normalizeImageMime(http.DetectContentType(head)); mime != "" {
			return mime, gcsImageExtByMime[mime], true
		}
	}
	return "", "", false
}

// ImageSource 一张待转存的图片：base64 优先、URL 兜底。
// 优先用 base64 的理由：字节已在手里，省一次跨境下载与一次 SSRF 风险面（设计 3.1）。
type ImageSource struct {
	B64      string // 上游 base64（可带 data:<mime>;base64, 前缀，内部剥离）
	URL      string // 上游直链
	MimeHint string // 上游 MIME 提示（可空）
}

// ImageTransferResult 一次转存的结果。
type ImageTransferResult struct {
	ObjectName string
	SignedURL  string // opts.Sign 为 false 时为空
	ExpiresAt  int64  // Unix 秒，真实签名过期时刻，不得虚标；不签名时为 0
	Raw        []byte // 仅 opts.WantBytes 为 true 时非空（供 b64_json 回传）
}

// ObjectNamer 由调用方按入口语义提供对象命名规则（各入口命名差异见设计 4.2.4 / 4.6 / 4.7）。
type ObjectNamer func(index int, ext string) string

// TransferOpts 一次转存的选项。
type TransferOpts struct {
	// WantBytes 需要回传 base64：上传的同时 tee 一份字节到内存（受体积上限约束）
	WantBytes bool
	// ReuseExisting 固定对象名入口（MJ / Responses）：条件写命中已存在对象时，
	// 在同一体积上限与 timeout 内 drain 到 EOF 取精确 size/CRC32C 校验后复用。
	// 一次性对象名入口必须为 false——412 可能在 Close() 才返回，reader 已消费无法重放。
	ReuseExisting bool
	// Timeout 单张预算，经 context 强制
	//（不能依赖 client.Timeout：RelayTimeout=0 时共享 client 无 Timeout）
	Timeout time.Duration
	// Sign 是否现签。false = 只上传（MJ 写入阶段只存 gs://，读时才签）
	Sign bool
	// SignPolicy Sign 为 true 时使用；CacheTag 为空表示不入签名缓存
	SignPolicy SignPolicy
	// ChannelTypeLabel 仅用于指标标签（渠道类型），不参与任何业务判断
	ChannelTypeLabel string
}

// gcsImageDeps 外部依赖的可替换钩子——GCS 上传/校验/签名与 HTTP 下载都需要真实
// 凭证与网络，单测通过替换本结构体来覆盖全部分支。
type gcsImageDeps struct {
	upload   func(ctx context.Context, objectName string, r io.Reader, contentType string) error
	verify   func(ctx context.Context, objectName string, expectedSize int64, expectedCRC32C uint32) error
	sign     func(objectName string, p SignPolicy) (string, int64, error)
	download func(ctx context.Context, url string, reason ...string) (*http.Response, error)
}

// imageDeps 生产实现。
var imageDeps = gcsImageDeps{
	upload:   GCSUploadObject,
	verify:   GCSVerifyExistingObject,
	sign:     GCSSignObjectURL,
	download: DoDownloadRequestWithContext,
}

// gcsImageSniffLen 内容嗅探所需的头部字节数（http.DetectContentType 只看前 512 字节）。
const gcsImageSniffLen = 512

// TransferImage 转存单张图片：取流/解码 → 判定 MIME → 流式上传 GCS →（可选）现签。
//
// 返回 error 时调用方**必须**回退透传上游原始结果，不得转成 relay error（设计 决策 4）。
func TransferImage(ctx context.Context, namer ObjectNamer, index int, src ImageSource, opts TransferOpts) (*ImageTransferResult, error) {
	if namer == nil {
		return nil, errors.New("gcs image transfer: object namer is nil")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	reader, upstreamCT, closer, err := openImageSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}

	// 嗅探需要头部字节：先读出来，再用 MultiReader 还原完整流（保持流式上传）
	head := make([]byte, gcsImageSniffLen)
	n, err := io.ReadFull(reader, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("gcs image transfer: read head failed: %w", err)
	}
	head = head[:n]
	if len(head) == 0 {
		return nil, errors.New("gcs image transfer: empty image payload")
	}

	mime, ext, ok := resolveImageMime(upstreamCT, src.MimeHint, head)
	if !ok {
		return nil, fmt.Errorf("gcs image transfer: unsupported image mime (upstream=%q hint=%q)", upstreamCT, src.MimeHint)
	}
	objectName := namer(index, ext)

	// 体积上限：LimitReader(N+1) + 字节计数 + CRC32C 累计。裸 LimitReader 会在 N 字节
	// 静默 EOF，把超限文件截断成"成功"对象——那是不可自愈的数据损坏。
	counter := newGCSCountingReader(io.MultiReader(bytes.NewReader(head), reader), setting.GCSImageMaxSize)
	var body io.Reader = counter
	var buf *bytes.Buffer
	if opts.WantBytes {
		buf = bytes.NewBuffer(make([]byte, 0, len(head)))
		body = io.TeeReader(counter, buf)
	}

	uploadErr := imageDeps.upload(ctx, objectName, body, mime)
	switch {
	case uploadErr == nil:
		// 上传成功
	case errors.Is(uploadErr, errGCSAssetOversize):
		return nil, fmt.Errorf("gcs image transfer: object %s exceeds limit %d bytes: %w",
			objectName, setting.GCSImageMaxSize, errGCSAssetOversize)
	case errors.Is(uploadErr, ErrGCSObjectExists):
		if !opts.ReuseExisting {
			return nil, fmt.Errorf("gcs image transfer: object %s already exists: %w", objectName, ErrGCSObjectExists)
		}
		// 幂等命中：必须拿到精确 size/CRC32C 再校验。412 可能在 io.Copy 阶段就返回，
		// 此时源流未读完 —— drain 到 EOF 补齐计数（仍受同一体积上限与 ctx 约束）。
		if err := drainCountingReader(counter, buf, opts.WantBytes); err != nil {
			return nil, fmt.Errorf("gcs image transfer: drain for integrity check failed on %s: %w", objectName, err)
		}
		if err := imageDeps.verify(ctx, objectName, counter.n, counter.crc.Sum32()); err != nil {
			return nil, err
		}
	default:
		return nil, uploadErr
	}

	result := &ImageTransferResult{ObjectName: objectName}
	if opts.WantBytes && buf != nil {
		result.Raw = buf.Bytes()
	}
	if opts.Sign {
		signedURL, expiresAt, err := imageDeps.sign(objectName, opts.SignPolicy)
		if err != nil {
			return nil, err
		}
		result.SignedURL, result.ExpiresAt = signedURL, expiresAt
	}
	return result, nil
}

// openImageSource 打开图片源：base64 优先（字节已在手里），URL 兜底。
// 返回的 closer 可能为 nil（base64 路径无需关闭）。
func openImageSource(ctx context.Context, src ImageSource) (io.Reader, string, io.Closer, error) {
	if b64 := strings.TrimSpace(src.B64); b64 != "" {
		raw, err := decodeImageBase64(b64)
		if err == nil {
			return bytes.NewReader(raw), "", nil, nil
		}
		// base64 解码失败时退回 URL（设计 3.1 末尾）
		if strings.TrimSpace(src.URL) == "" {
			return nil, "", nil, fmt.Errorf("gcs image transfer: decode base64 failed: %w", err)
		}
	}
	rawURL := strings.TrimSpace(src.URL)
	if rawURL == "" {
		return nil, "", nil, errors.New("gcs image transfer: image source has neither base64 nor url")
	}
	// SSRF 校验与共享 http client 由 DoDownloadRequestWithContext 强制
	resp, err := imageDeps.download(ctx, rawURL, "gcs image transfer")
	if err != nil {
		return nil, "", nil, fmt.Errorf("gcs image transfer: download failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, "", nil, fmt.Errorf("gcs image transfer: upstream returned %d: %s", resp.StatusCode, string(snippet))
	}
	return resp.Body, resp.Header.Get("Content-Type"), resp.Body, nil
}

// decodeImageBase64 剥离 data:<mime>;base64, 前缀后解码，兼容标准与无填充字母表。
func decodeImageBase64(s string) ([]byte, error) {
	if strings.HasPrefix(s, "data:") {
		if i := strings.Index(s, ";base64,"); i >= 0 {
			s = s[i+len(";base64,"):]
		} else if i := strings.Index(s, ","); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.TrimSpace(s)
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// drainCountingReader 把剩余字节读完，使 counter 的 size/CRC32C 成为完整值。
// WantBytes 时必须继续 tee 进 buf，否则回传的 base64 会缺尾。
func drainCountingReader(counter *gcsCountingReader, buf *bytes.Buffer, wantBytes bool) error {
	var sink io.Writer = io.Discard
	if wantBytes && buf != nil {
		sink = buf
	}
	if _, err := io.Copy(sink, counter); err != nil {
		return err
	}
	if !counter.eof {
		return errors.New("source stream not fully read")
	}
	return nil
}

// TransferImages 并发转存一组图片，逐张独立成败——某张失败只让该位置为 nil，
// 不拖累其余（设计 4.9：用户始终要能拿到图）。并发上限 GCS_IMAGE_CONCURRENCY。
func TransferImages(ctx context.Context, namer ObjectNamer, srcs []ImageSource, opts TransferOpts) []*ImageTransferResult {
	results := make([]*ImageTransferResult, len(srcs))
	if len(srcs) == 0 {
		return results
	}
	limit := setting.GCSImageConcurrency
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range srcs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := TransferImage(ctx, namer, idx, srcs[idx], opts)
			if err != nil {
				recordImageTransferFailure(ctx, idx, err)
				return
			}
			results[idx] = res
		}(i)
	}
	wg.Wait()
	return results
}

// recordImageTransferFailure 生图转存失败的统一观测点。
// Task 7 会在此基础上补上按 kind 的指标计数——失败绝不重试（同步路径，重试只放大
// 用户等待），也绝不转成 relay error（会触发渠道重试 + 最终退款）。
func recordImageTransferFailure(ctx context.Context, index int, err error) {
	logger.LogError(ctx, fmt.Sprintf("gcs-image transfer fail index=%d err=%s", index, err.Error()))
}
