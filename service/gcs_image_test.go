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
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveImageMime(t *testing.T) {
	pngHead := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	gifHead := []byte("GIF89a-------")

	cases := []struct {
		name       string
		upstreamCT string
		hint       string
		head       []byte
		wantMime   string
		wantExt    string
		wantOK     bool
	}{
		{"上游 Content-Type 优先", "image/png", "image/jpeg", gifHead, "image/png", "png", true},
		{"带参数的 Content-Type 需归一化", "image/JPEG; charset=utf-8", "", nil, "image/jpeg", "jpg", true},
		{"image/jpg 归一到 jpg", "image/jpg", "", nil, "image/jpeg", "jpg", true},
		{"上游缺失时用 hint", "", "image/webp", nil, "image/webp", "webp", true},
		{"上游与 hint 都缺失时嗅探", "", "", pngHead, "image/png", "png", true},
		{"嗅探 gif", "", "", gifHead, "image/gif", "gif", true},
		{"octet-stream 不可信，回退嗅探", "application/octet-stream", "", pngHead, "image/png", "png", true},
		{"非图片类型判失败", "text/html", "", nil, "", "", false},
		{"三者都无法判定则失败", "", "", []byte("not an image at all"), "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, ext, ok := resolveImageMime(tc.upstreamCT, tc.hint, tc.head)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantMime, mime)
			assert.Equal(t, tc.wantExt, ext)
		})
	}
}

// 扩展名只能来自白名单映射，绝不能受上游 URL 影响
func TestResolveImageMime_ExtNeverFromUpstreamString(t *testing.T) {
	mime, ext, ok := resolveImageMime("image/png", "", nil)
	assert.True(t, ok)
	assert.Equal(t, "image/png", mime)
	assert.Equal(t, "png", ext)
	assert.False(t, bytes.ContainsAny([]byte(ext), "/?.&"), "扩展名不得含路径或查询串字符")
}

// ── TransferImage ──

// pngPayload 构造一个能被 http.DetectContentType 识别为 image/png 的载荷。
func pngPayload(extra int) []byte {
	head := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if extra <= 0 {
		return head
	}
	return append(head, bytes.Repeat([]byte{7}, extra)...)
}

// withStubbedImageDeps 替换 GCS/HTTP 依赖，用例结束后恢复。
func withStubbedImageDeps(t *testing.T, stub gcsImageDeps) {
	t.Helper()
	orig := imageDeps
	imageDeps = stub
	t.Cleanup(func() { imageDeps = orig })
}

// withImageSettings 设置生图相关配置并在用例结束后恢复。
func withImageSettings(t *testing.T, maxSize int64, concurrency int) {
	t.Helper()
	origMax, origConc := setting.GCSImageMaxSize, setting.GCSImageConcurrency
	setting.GCSImageMaxSize, setting.GCSImageConcurrency = maxSize, concurrency
	t.Cleanup(func() {
		setting.GCSImageMaxSize, setting.GCSImageConcurrency = origMax, origConc
	})
}

func testNamer(prefix string) ObjectNamer {
	return func(index int, ext string) string {
		return fmt.Sprintf("%s/%d.%s", prefix, index, ext)
	}
}

func TestTransferImage_FromBase64(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	payload := pngPayload(4)
	var gotObject, gotCT string
	var gotBody []byte

	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, objectName string, r io.Reader, contentType string) error {
			gotObject, gotCT = objectName, contentType
			b, err := io.ReadAll(r)
			gotBody = b
			return err
		},
		sign: func(objectName string, p SignPolicy) (string, int64, error) {
			return "https://signed/" + objectName, 4242, nil
		},
	})

	res, err := TransferImage(context.Background(), testNamer("img/req1"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(payload)},
		TransferOpts{Sign: true, Timeout: time.Second})

	require.NoError(t, err)
	assert.Equal(t, "img/req1/0.png", gotObject)
	assert.Equal(t, "image/png", gotCT)
	assert.Equal(t, payload, gotBody)
	assert.Equal(t, "https://signed/img/req1/0.png", res.SignedURL)
	assert.Equal(t, int64(4242), res.ExpiresAt)
	assert.Nil(t, res.Raw, "未要求回传 base64 时不应驻留字节")
}

// data: 前缀必须被剥离，否则解出来的是乱码
func TestTransferImage_StripsDataURIPrefix(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	payload := pngPayload(1)
	var gotBody []byte
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			b, err := io.ReadAll(r)
			gotBody = b
			return err
		},
	})

	src := ImageSource{B64: "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)}
	_, err := TransferImage(context.Background(), testNamer("p"), 0, src, TransferOpts{Timeout: time.Second})

	require.NoError(t, err)
	assert.Equal(t, payload, gotBody)
}

func TestTransferImage_FromURLWantBytes(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	payload := pngPayload(600)
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		},
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, err := io.Copy(io.Discard, r)
			return err
		},
	})

	res, err := TransferImage(context.Background(), testNamer("p"), 1,
		ImageSource{URL: "https://upstream.example/a.png"},
		TransferOpts{WantBytes: true, Timeout: time.Second})

	require.NoError(t, err)
	assert.Equal(t, payload, res.Raw, "WantBytes 时必须 tee 出完整字节（含预读的 head）")
	assert.Equal(t, "p/1.png", res.ObjectName)
}

func TestTransferImage_UpstreamNon200(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("expired")),
			}, nil
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{URL: "https://upstream.example/gone.png"}, TransferOpts{Timeout: time.Second})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestTransferImage_RejectsNonImageMime(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader("<html>nope</html>")),
			}, nil
		},
		upload: func(_ context.Context, _ string, _ io.Reader, _ string) error {
			t.Error("非白名单 MIME 绝不能上传")
			return nil
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{URL: "https://upstream.example/x"}, TransferOpts{Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported image mime")
}

func TestTransferImage_Oversize(t *testing.T) {
	withImageSettings(t, 1024, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader(pngPayload(4096))),
			}, nil
		},
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			// 模拟真实 GCSUploadObject：io.Copy 出错即返回错误、不 finalize
			_, err := io.Copy(io.Discard, r)
			return err
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{URL: "https://upstream.example/big.png"}, TransferOpts{Timeout: time.Second})

	require.Error(t, err)
	assert.ErrorIs(t, err, errGCSAssetOversize)
}

// ReuseExisting=false：命中已存在对象直接判失败，绝不做随机后缀重试
//（412 可能在 Close() 才返回，此时 reader 已被消费、无法重放）
func TestTransferImage_ExistsWithoutReuseFails(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, _ = io.Copy(io.Discard, r)
			return ErrGCSObjectExists
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(pngPayload(0))},
		TransferOpts{Timeout: time.Second})
	assert.ErrorIs(t, err, ErrGCSObjectExists)
}

// ReuseExisting=true：412 后必须 drain 到 EOF 拿到精确 size/CRC 再校验
func TestTransferImage_ReuseExistingVerifiesIntegrity(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	payload := pngPayload(100)
	var verifiedSize int64
	var verifiedCRC uint32
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			// 只读一部分就返回 412，模拟 io.Copy 阶段命中条件写失败
			buf := make([]byte, 8)
			_, _ = r.Read(buf)
			return ErrGCSObjectExists
		},
		verify: func(_ context.Context, _ string, size int64, crc uint32) error {
			verifiedSize, verifiedCRC = size, crc
			return nil
		},
		sign: func(objectName string, _ SignPolicy) (string, int64, error) {
			return "https://signed/" + objectName, 1, nil
		},
	})

	res, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(payload)},
		TransferOpts{ReuseExisting: true, Sign: true, Timeout: time.Second})

	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), verifiedSize, "必须 drain 到 EOF 才能拿到完整 size")
	assert.NotZero(t, verifiedCRC)
	assert.Equal(t, "https://signed/p/0.png", res.SignedURL)
}

// 完整性校验不通过时禁止复用
func TestTransferImage_ReuseExistingCorruptedRejected(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, _ = io.Copy(io.Discard, r)
			return ErrGCSObjectExists
		},
		verify: func(_ context.Context, _ string, _ int64, _ uint32) error {
			return ErrGCSObjectCorrupted
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(pngPayload(0))},
		TransferOpts{ReuseExisting: true, Timeout: time.Second})
	assert.ErrorIs(t, err, ErrGCSObjectCorrupted)
}

// Sign=false 时只上传不签名（MJ 写入阶段：只存 gs://，读时才签）
func TestTransferImage_NoSign(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, err := io.Copy(io.Discard, r)
			return err
		},
		sign: func(string, SignPolicy) (string, int64, error) {
			t.Error("Sign=false 时不得调用签名")
			return "", 0, nil
		},
	})

	res, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(pngPayload(0))},
		TransferOpts{Timeout: time.Second})

	require.NoError(t, err)
	assert.Empty(t, res.SignedURL)
	assert.Zero(t, res.ExpiresAt)
	assert.Equal(t, "p/0.png", res.ObjectName)
}

// base64 解码失败时退回 URL（设计 3.1 末尾的源优先级）
func TestTransferImage_BadBase64FallsBackToURL(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	payload := pngPayload(16)
	downloaded := false
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			downloaded = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}, nil
		},
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, err := io.Copy(io.Discard, r)
			return err
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: "!!!not-base64!!!", URL: "https://upstream.example/a.png"},
		TransferOpts{Timeout: time.Second})

	require.NoError(t, err)
	assert.True(t, downloaded, "base64 解码失败后必须退回 URL")
}

// 既无 base64 也无 URL 时直接失败
func TestTransferImage_EmptySource(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{})
	_, err := TransferImage(context.Background(), testNamer("p"), 0, ImageSource{}, TransferOpts{Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither base64 nor url")
}

// 空载荷判失败，绝不上传空对象。真实场景是上游返回 200 但 body 为空
//（空 base64 字符串与"没传 base64"无法区分，那条走 EmptySource 用例）。
func TestTransferImage_EmptyPayload(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		download: func(_ context.Context, _ string, _ ...string) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		},
		upload: func(_ context.Context, _ string, _ io.Reader, _ string) error {
			t.Error("空载荷不得上传")
			return nil
		},
	})
	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{URL: "https://upstream.example/empty.png"}, TransferOpts{Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty image payload")
}

// namer 为 nil 时直接失败，而不是 panic
func TestTransferImage_NilNamer(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{})
	_, err := TransferImage(context.Background(), nil, 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(pngPayload(0))}, TransferOpts{Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namer is nil")
}

// 签名失败必须整体判失败（对象已上传，属浪费但不影响用户——调用方回退透传）
func TestTransferImage_SignFailure(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			_, err := io.Copy(io.Discard, r)
			return err
		},
		sign: func(string, SignPolicy) (string, int64, error) {
			return "", 0, errors.New("signblob 403")
		},
	})

	_, err := TransferImage(context.Background(), testNamer("p"), 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(pngPayload(0))},
		TransferOpts{Sign: true, Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signblob 403")
}

// ── TransferImages ──

// 逐张独立成败：失败位置返回 nil，不拖累其余
func TestTransferImages_PartialFailure(t *testing.T) {
	withImageSettings(t, 1<<20, 2)
	okB64 := base64.StdEncoding.EncodeToString(pngPayload(0))
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, objectName string, r io.Reader, _ string) error {
			_, _ = io.Copy(io.Discard, r)
			if strings.HasPrefix(objectName, "p/1.") {
				return errors.New("gcs down")
			}
			return nil
		},
	})

	results := TransferImages(context.Background(), testNamer("p"), []ImageSource{
		{B64: okB64}, {B64: okB64}, {B64: okB64},
	}, TransferOpts{Timeout: time.Second})

	require.Len(t, results, 3)
	assert.NotNil(t, results[0])
	assert.Nil(t, results[1], "失败位置必须是 nil")
	assert.NotNil(t, results[2])
	assert.Equal(t, "p/0.png", results[0].ObjectName)
	assert.Equal(t, "p/2.png", results[2].ObjectName)
}

// 并发上限必须生效
func TestTransferImages_RespectsConcurrencyLimit(t *testing.T) {
	withImageSettings(t, 1<<20, 2)
	okB64 := base64.StdEncoding.EncodeToString(pngPayload(0))
	var mu sync.Mutex
	var cur, peak int
	withStubbedImageDeps(t, gcsImageDeps{
		upload: func(_ context.Context, _ string, r io.Reader, _ string) error {
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			_, err := io.Copy(io.Discard, r)
			return err
		},
	})

	srcs := make([]ImageSource, 6)
	for i := range srcs {
		srcs[i] = ImageSource{B64: okB64}
	}
	results := TransferImages(context.Background(), testNamer("p"), srcs, TransferOpts{Timeout: time.Second})

	require.Len(t, results, 6)
	assert.LessOrEqual(t, peak, 2, "并发数不得超过 GCS_IMAGE_CONCURRENCY")
	for i, r := range results {
		assert.NotNil(t, r, "index %d 应成功", i)
	}
}

// 空输入不 panic
func TestTransferImages_Empty(t *testing.T) {
	withImageSettings(t, 1<<20, 4)
	withStubbedImageDeps(t, gcsImageDeps{})
	results := TransferImages(context.Background(), testNamer("p"), nil, TransferOpts{})
	assert.Empty(t, results)
}
