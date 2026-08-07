package relay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 多来源真实往返验证：把各家生图渠道的**真实响应形态**（带真实图片字节）
// 跑一遍真实的改写 + 真实 GCS 上传 + 真实签名，再把签名 URL 下载回来逐字节比对。
//
// 默认跳过。跑法：
//
//	GCS_E2E=1 GCS_E2E_FIXTURES=<放置图片的目录> \
//	GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json \
//	GCS_RESULT_BUCKET=taluna-api-result \
//	go test ./relay/ -run TestMultiSourceE2E -v
//
// fixtures 目录需含：xai-upstream.jpg、gemini-style.png、mj-style.webp、xai-resp.json
func TestMultiSourceE2E(t *testing.T) {
	if os.Getenv("GCS_E2E") != "1" {
		t.Skip("需要真实 GCS 凭证，设置 GCS_E2E=1 才跑")
	}
	fixtures := os.Getenv("GCS_E2E_FIXTURES")
	require.NotEmpty(t, fixtures, "需要 GCS_E2E_FIXTURES 指向图片目录")

	os.Setenv("GCS_IMAGE_TRANSFER_ENABLED", "true")
	os.Setenv("GCS_IMAGE_PREFIX", "api/image/e2e-verify")
	setting.InitGCSSettings()
	service.InitGCSStorage()
	service.InitHttpClient()
	require.True(t, service.GCSImageTransferReady(), "GCS client 未就绪")

	// 本机 fake-IP 代理会把公网域名解析到 198.18.x.x 保留段，SSRF 防护会正确拒绝。
	// 那是防护在正常工作——这里放开，让取流链路本身可验证。
	fs := system_setting.GetFetchSetting()
	origPrivate, origPorts := fs.AllowPrivateIp, fs.AllowedPorts
	fs.AllowPrivateIp = true
	fs.AllowedPorts = []string{"80", "443", "1024-65535"}
	defer func() { fs.AllowPrivateIp, fs.AllowedPorts = origPrivate, origPorts }()

	jpegBytes := mustRead(t, fixtures+"/xai-upstream.jpg")
	pngBytes := mustRead(t, fixtures+"/gemini-style.png")
	webpBytes := mustRead(t, fixtures+"/mj-style.webp")
	xaiLiveBody := mustRead(t, fixtures+"/xai-resp.json")

	cases := []struct {
		name string
		// body 上游响应体（该渠道的真实形态）
		body []byte
		// format 客户端传入的 response_format
		format string
		// wantBytes 期望最终能从 CDN 下载回来的原始图片字节；nil 表示不比对
		wantBytes []byte
		// preserved 必须被无损保留的顶层字段
		preserved []string
		// preservedInItem 必须被无损保留的 data[] 内字段
		preservedInItem []string
	}{
		{
			// xAI 真实 live 响应：url 形态 + 非标准 mime_type + 顶层 usage
			name:            "xAI grok-imagine-image（真实 live 响应，url 源）",
			body:            xaiLiveBody,
			format:          "url",
			wantBytes:       jpegBytes,
			preserved:       []string{"usage"},
			preservedInItem: []string{"mime_type"},
		},
		{
			// Gemini 走 /v1/images 时恒定 b64_json（relay-gemini.go:1646-1652）
			name:      "Gemini 形态（恒定 b64_json，真实 PNG）",
			body:      buildB64Body(t, pngBytes, `"created":1754500000`),
			format:    "url",
			wantBytes: pngBytes,
			preserved: []string{"created"},
		},
		{
			// gpt-image-1：OpenAI 原样透传，带 usage/size/quality/output_format
			name: "GPT Image 形态（b64_json + usage/size/quality，真实 PNG）",
			body: buildB64Body(t, pngBytes,
				`"created":1754500001,"size":"1024x1024","quality":"high","output_format":"png",`+
					`"usage":{"total_tokens":1590,"input_tokens":6,"output_tokens":1584,`+
					`"input_tokens_details":{"text_tokens":6,"image_tokens":0}}`),
			format:    "url",
			wantBytes: pngBytes,
			preserved: []string{"created", "size", "quality", "output_format", "usage"},
		},
		{
			// 未传 response_format 时，b64_json 必须原样保留且补上 url
			name:            "GPT Image 形态 + 未传 response_format（须同时有 url 与 b64_json）",
			body:            buildB64Body(t, pngBytes, `"created":1754500002`),
			format:          "",
			wantBytes:       pngBytes,
			preserved:       []string{"created"},
			preservedInItem: []string{"b64_json"},
		},
		{
			// Midjourney 的图是 webp——第 3 期才接线，这里先验转存原语能吃 webp
			name:      "Midjourney 形态（webp 图片字节；MJ 接线属第 3 期）",
			body:      buildB64Body(t, webpBytes, `"created":1754500003`),
			format:    "url",
			wantBytes: webpBytes,
			preserved: []string{"created"},
		},
	}

	// 真实 live 响应（若 fixture 存在）：经 taluna 网关调 gpt-image-2 拿到的原始响应体，
	// 顶层带 background/moderation/output_format/quality/size/usage 六个非标准字段，
	// 是"无损改写"最硬的验收样本。
	if live, err := os.ReadFile(fixtures + "/live-gpt-image-2.json"); err == nil {
		var top map[string]json.RawMessage
		require.NoError(t, common.Unmarshal(live, &top))
		var data []map[string]json.RawMessage
		require.NoError(t, common.Unmarshal(top["data"], &data))
		var b64 string
		require.NoError(t, common.Unmarshal(data[0]["b64_json"], &b64))
		raw, err := base64.StdEncoding.DecodeString(b64)
		require.NoError(t, err)

		cases = append(cases, struct {
			name            string
			body            []byte
			format          string
			wantBytes       []byte
			preserved       []string
			preservedInItem []string
		}{
			name:      "GPT Image 2（真实 live 响应，经 taluna 网关）",
			body:      live,
			format:    "url",
			wantBytes: raw,
			preserved: []string{"created", "background", "moderation", "output_format", "quality", "size", "usage"},
		})
	}

	// 真实 Midjourney 任务结果（若 fixture 存在）：MJ 接线属第 3 期，这里先验
	// 转存原语能吃 MJ 的真实 CDN 直链（阿里云 OSS，2048x2048 PNG）。
	if mjFetch, err := os.ReadFile(fixtures + "/mj-fetch.json"); err == nil {
		var task struct {
			ImageURL   string `json:"imageUrl"`
			FinishTime int64  `json:"finishTime"`
		}
		require.NoError(t, common.Unmarshal(mjFetch, &task))
		if task.ImageURL != "" {
			// MJ 的 finishTime 是毫秒（13 位）——直接当 Unix 秒传给保留期检查会让
			// 截止时间落到公元 5 万年，过期检查形同虚设。第 3 期必须 /1000。
			assert.Greater(t, task.FinishTime, int64(1e12), "MJ finishTime 应为 13 位毫秒")

			upstream := httpGet(t, task.ImageURL)
			cases = append(cases, struct {
				name            string
				body            []byte
				format          string
				wantBytes       []byte
				preserved       []string
				preservedInItem []string
			}{
				name:      "Midjourney（真实任务结果，url 源；MJ 接线属第 3 期）",
				body:      []byte(fmt.Sprintf(`{"created":1,"data":[{"url":%q}]}`, task.ImageURL)),
				format:    "url",
				wantBytes: upstream,
				preserved: []string{"created"},
			})
		}
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	report, _ := os.Create(fixtures + "/multi-source-report.txt")
	if report != nil {
		defer report.Close()
	}

	for ci, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			namer := func(index int, ext string) string {
				return fmt.Sprintf("%s/%s_case%d_%d.%s", setting.GCSImagePrefix, stamp, ci, index, ext)
			}
			transfer := func(index int, src service.ImageSource, wantBytes bool) *service.ImageTransferResult {
				res, err := service.TransferImage(context.Background(), namer, index, src, service.TransferOpts{
					WantBytes:  wantBytes,
					Sign:       true,
					Timeout:    setting.GCSImageTransferTimeout,
					SignPolicy: service.SignPolicy{TTL: setting.GCSImageSignedURLTTL},
				})
				if err != nil {
					t.Logf("转存失败: %v", err)
					return nil
				}
				return res
			}

			newBody, rewrote, err := rewriteImageResponseBody(tc.body, tc.format, false, transfer)
			require.NoError(t, err)
			require.True(t, rewrote, "必须完成改写")

			var top map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(newBody, &top))
			for _, key := range tc.preserved {
				assert.Contains(t, top, key, "顶层字段 %s 必须无损保留", key)
			}

			var data []map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(top["data"], &data))
			require.NotEmpty(t, data)
			for _, key := range tc.preservedInItem {
				assert.Contains(t, data[0], key, "data[] 内字段 %s 必须无损保留", key)
			}

			var signedURL string
			require.NoError(t, common.Unmarshal(data[0]["url"], &signedURL))
			assert.Contains(t, signedURL, "storage.googleapis.com", "url 必须已替换为我们自己的签名链接")

			// 真实下载并逐字节比对
			if tc.wantBytes != nil {
				got := httpGet(t, signedURL)
				assert.Equal(t, sha256hex(tc.wantBytes), sha256hex(got), "CDN 上的图必须与原图逐字节一致")
				if report != nil {
					fmt.Fprintf(report, "%s\n  签名URL: %s\n  字节数: %d  sha256: %s\n\n",
						tc.name, signedURL, len(got), sha256hex(got)[:16])
				}
			}
			t.Logf("✅ %s\n   签名URL: %s", tc.name, signedURL)
		})
	}
}

// buildB64Body 构造 {"<extra>","data":[{"b64_json":"..."}]} 形态的响应体。
func buildB64Body(t *testing.T, img []byte, extraTopFields string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`{%s,"data":[{"b64_json":%q}]}`,
		extraTopFields, base64.StdEncoding.EncodeToString(img)))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err, "读取 fixture 失败: %s", path)
	return b
}

func httpGet(t *testing.T, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "签名 URL 必须返回 200")
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
