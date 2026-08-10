package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/require"
)

// 真实 GCS 往返验证（设计文档 4.12「上线前检查」的第 2 项：实签一个 GET URL 并验证 200）。
//
// 默认跳过——需要真实凭证与 bucket 写权限。跑法：
//
//	GCS_E2E=1 \
//	GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json \
//	GCS_RESULT_BUCKET=taluna-api-result \
//	go test ./service/ -run TestTransferImageE2E -v
//
// 覆盖两条源路径：base64 上传 + 从签名 URL 回读再上传（证明 URL 取流也通）。
func TestTransferImageE2E(t *testing.T) {
	if os.Getenv("GCS_E2E") != "1" {
		t.Skip("需要真实 GCS 凭证，设置 GCS_E2E=1 才跑")
	}

	// 用真实配置初始化真实 client（不 stub imageDeps）
	os.Setenv("GCS_IMAGE_TRANSFER_ENABLED", "true")
	os.Setenv("GCS_IMAGE_PREFIX", "api/image/e2e-verify")
	setting.InitGCSSettings()
	InitGCSStorage()
	InitHttpClient() // 生产里由 main.go 启动时初始化；不调则 GetHttpClient() 返回 nil
	require.True(t, GCSImageTransferReady(), "GCS client 未就绪，检查凭证与 bucket")

	original := buildVerificationPNG(t)
	t.Logf("原始图片: %d 字节 PNG", len(original))

	// 保存一份到磁盘，供人工比对
	outDir := os.Getenv("GCS_E2E_OUT")
	if outDir != "" {
		require.NoError(t, os.WriteFile(outDir+"/e2e-original.png", original, 0o644))
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	namer := func(index int, ext string) string {
		return fmt.Sprintf("%s/%s_%d.%s", setting.GCSImagePrefix, stamp, index, ext)
	}

	// ── 路径 1：base64 源（gpt-image / Gemini / Zhipu 的形态）──
	res1, err := TransferImage(context.Background(), namer, 0,
		ImageSource{B64: base64.StdEncoding.EncodeToString(original)},
		TransferOpts{
			Sign:       true,
			WantBytes:  true,
			Timeout:    setting.GCSImageTransferTimeout,
			SignPolicy: SignPolicy{TTL: setting.GCSImageSignedURLTTL},
		})
	require.NoError(t, err)
	require.Equal(t, original, res1.Raw, "WantBytes 回传的字节必须与原图一致")

	t.Logf("【路径1 base64源】对象: gs://%s/%s", setting.GCSResultBucket, res1.ObjectName)
	t.Logf("【路径1 base64源】签名URL: %s", res1.SignedURL)
	t.Logf("【路径1 base64源】过期时刻: %s", time.Unix(res1.ExpiresAt, 0).Format(time.RFC3339))

	// ── 路径 2：URL 源（dall-e / Ali / MiniMax / Jimeng 的形态）──
	// 直接拿路径 1 的签名 URL 当上游直链，走完整的下载→嗅探→上传链路。
	//
	// 本机若挂了 fake-IP 代理（Clash 等），storage.googleapis.com 会被解析到
	// 198.18.x.x 保留段，SSRF 防护会正确地拒绝它。那是防护在正常工作，不是被测
	// 代码的问题——这里临时放开私网限制，让 URL 取流链路本身可被验证。
	fs := system_setting.GetFetchSetting()
	origAllowPrivate := fs.AllowPrivateIp
	fs.AllowPrivateIp = true
	defer func() { fs.AllowPrivateIp = origAllowPrivate }()

	res2, err := TransferImage(context.Background(), namer, 1,
		ImageSource{URL: res1.SignedURL},
		TransferOpts{
			Sign:       true,
			Timeout:    setting.GCSImageTransferTimeout,
			SignPolicy: SignPolicy{TTL: setting.GCSImageSignedURLTTL},
		})
	require.NoError(t, err)

	t.Logf("【路径2 URL源】对象: gs://%s/%s", setting.GCSResultBucket, res2.ObjectName)
	t.Logf("【路径2 URL源】签名URL: %s", res2.SignedURL)

	// 机器可读输出，供外层脚本 curl 校验
	if outDir != "" {
		lines := fmt.Sprintf("%s\n%s\n", res1.SignedURL, res2.SignedURL)
		require.NoError(t, os.WriteFile(outDir+"/e2e-urls.txt", []byte(lines), 0o644))
	}
}

// buildVerificationPNG 生成一张肉眼可辨的验证图：128x128，青紫渐变 + 白色对角线
// + 左上角实心方块，方便人工确认下载回来的就是同一张。
func buildVerificationPNG(t *testing.T) []byte {
	t.Helper()
	const size = 128
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := color.RGBA{
				R: uint8(x * 2),
				G: uint8(y * 2),
				B: uint8(255 - x),
				A: 255,
			}
			if x == y || x == size-1-y { // 白色对角线
				c = color.RGBA{255, 255, 255, 255}
			}
			if x < 24 && y < 24 { // 左上角实心方块
				c = color.RGBA{255, 80, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}
