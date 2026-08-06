package relay

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okTransfer(index int, src service.ImageSource, wantBytes bool) *service.ImageTransferResult {
	res := &service.ImageTransferResult{
		ObjectName: "api/image/o.png",
		SignedURL:  "https://signed.example/o.png",
		ExpiresAt:  1900000000,
	}
	if wantBytes {
		res.Raw = []byte{1, 2, 3}
	}
	return res
}

func failTransfer(int, service.ImageSource, bool) *service.ImageTransferResult { return nil }

func parseData(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(body, &top))
	var data []map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(top["data"], &data))
	return data
}

// response_format=url：只填 url，b64_json 键必须被真删除（不是空字符串）
func TestRewrite_FormatURL(t *testing.T) {
	body := []byte(`{"created":1,"data":[{"url":"https://upstream/x.png","b64_json":"QUJD","revised_prompt":"kept"}]}`)

	out, rewrote, err := rewriteImageResponseBody(body, "url", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	require.Len(t, data, 1)
	assert.JSONEq(t, `"https://signed.example/o.png"`, string(data[0]["url"]))
	_, hasB64 := data[0]["b64_json"]
	assert.False(t, hasB64, "response_format=url 时 b64_json 键必须删除")
	assert.JSONEq(t, `"kept"`, string(data[0]["revised_prompt"]), "未知/其他字段必须原样保留")
}

// response_format=b64_json：只填 b64_json，url 键删除；仍然上传 GCS 留存
func TestRewrite_FormatB64(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png"}]}`)

	out, rewrote, err := rewriteImageResponseBody(body, "b64_json", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	_, hasURL := data[0]["url"]
	assert.False(t, hasURL, "response_format=b64_json 时 url 键必须删除")
	assert.JSONEq(t, `"AQID"`, string(data[0]["b64_json"]), "应回传转存时 tee 出的字节")
}

// 未传 response_format + 上游给直链 → url 换成签名链接
func TestRewrite_NoFormat_UpstreamURL(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png"}]}`)

	out, rewrote, err := rewriteImageResponseBody(body, "", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	assert.JSONEq(t, `"https://signed.example/o.png"`, string(data[0]["url"]))
}

// 未传 response_format + 上游给 base64 → 补上 url，且 b64_json 原样保留
func TestRewrite_NoFormat_UpstreamB64KeepsBoth(t *testing.T) {
	body := []byte(`{"data":[{"b64_json":"QUJD"}]}`)

	out, rewrote, err := rewriteImageResponseBody(body, "", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	assert.JSONEq(t, `"https://signed.example/o.png"`, string(data[0]["url"]), "永远补上 url")
	assert.JSONEq(t, `"QUJD"`, string(data[0]["b64_json"]), "现有依赖 b64_json 的客户端不得丢字段")
}

// GCS_IMAGE_STRIP_B64_WHEN_URL=true 时（响应瘦身开关）才删 b64_json
func TestRewrite_NoFormat_StripB64WhenEnabled(t *testing.T) {
	orig := setting.GCSImageStripB64WhenURL
	setting.GCSImageStripB64WhenURL = true
	defer func() { setting.GCSImageStripB64WhenURL = orig }()

	body := []byte(`{"data":[{"b64_json":"QUJD"}]}`)
	out, rewrote, err := rewriteImageResponseBody(body, "", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	_, hasB64 := data[0]["b64_json"]
	assert.False(t, hasB64)
	assert.JSONEq(t, `"https://signed.example/o.png"`, string(data[0]["url"]))
}

// 顶层未知字段（usage / MiniMax metadata 等）必须无损保留
func TestRewrite_PreservesUnknownTopLevelFields(t *testing.T) {
	body := []byte(`{"created":7,"data":[{"url":"https://upstream/x.png"}],"usage":{"total_tokens":9},"metadata":{"vendor":"minimax"},"vendor_ext":[1,2]}`)

	out, _, err := rewriteImageResponseBody(body, "url", false, okTransfer)
	require.NoError(t, err)

	var top map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(out, &top))
	assert.JSONEq(t, `{"total_tokens":9}`, string(top["usage"]))
	assert.JSONEq(t, `{"vendor":"minimax"}`, string(top["metadata"]), "非 Ali 渠道的 metadata 必须保留")
	assert.JSONEq(t, `[1,2]`, string(top["vendor_ext"]))
	assert.JSONEq(t, `7`, string(top["created"]))
}

// dropMetadata=true（仅 Ali 渠道会传 true）时才删除顶层 metadata
func TestRewrite_DropsMetadataOnlyWhenAsked(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png"}],"metadata":{"raw":"含上游直链"}}`)

	out, _, err := rewriteImageResponseBody(body, "url", true, okTransfer)
	require.NoError(t, err)

	var top map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(out, &top))
	_, has := top["metadata"]
	assert.False(t, has)
}

// 全部转存失败 → 不改写，调用方原样透传
func TestRewrite_AllTransfersFailed(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png"}]}`)

	out, rewrote, err := rewriteImageResponseBody(body, "url", false, failTransfer)
	require.NoError(t, err)
	assert.False(t, rewrote)
	assert.Equal(t, body, out, "未改写时必须返回原始字节")
}

// 逐张独立：失败的那张保留上游原值
func TestRewrite_PartialFailureKeepsUpstreamValue(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/a.png"},{"url":"https://upstream/b.png"}]}`)
	transfer := func(index int, _ service.ImageSource, _ bool) *service.ImageTransferResult {
		if index == 1 {
			return nil
		}
		return &service.ImageTransferResult{SignedURL: "https://signed.example/a.png"}
	}

	out, rewrote, err := rewriteImageResponseBody(body, "url", false, transfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	assert.JSONEq(t, `"https://signed.example/a.png"`, string(data[0]["url"]))
	assert.JSONEq(t, `"https://upstream/b.png"`, string(data[1]["url"]), "失败的那张保留上游原值")
}

// 源优先级：同时有 url 与 b64_json 时优先用 b64（省一次跨境下载与 SSRF 风险面）
func TestRewrite_PrefersBase64AsSource(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png","b64_json":"QUJD"}]}`)
	var gotSrc service.ImageSource
	transfer := func(_ int, src service.ImageSource, _ bool) *service.ImageTransferResult {
		gotSrc = src
		return &service.ImageTransferResult{SignedURL: "https://signed.example/x.png"}
	}

	_, _, err := rewriteImageResponseBody(body, "url", false, transfer)
	require.NoError(t, err)
	assert.Equal(t, "QUJD", gotSrc.B64)
	assert.Equal(t, "https://upstream/x.png", gotSrc.URL, "URL 仍要带上，供 base64 解码失败时兜底")
}

// wantBytes 只在「客户端要 b64 但上游只给了 url」时才为 true，避免无谓的内存驻留
func TestRewrite_WantBytesOnlyWhenNeeded(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		format string
		want   bool
	}{
		{"要 b64 且上游只给 url", `{"data":[{"url":"https://u/x.png"}]}`, "b64_json", true},
		{"要 b64 且上游已给 b64", `{"data":[{"b64_json":"QUJD"}]}`, "b64_json", false},
		{"要 url", `{"data":[{"url":"https://u/x.png"}]}`, "url", false},
		{"未指定", `{"data":[{"url":"https://u/x.png"}]}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			transfer := func(_ int, _ service.ImageSource, wantBytes bool) *service.ImageTransferResult {
				got = wantBytes
				return &service.ImageTransferResult{SignedURL: "https://s/x.png", Raw: []byte{1, 2, 3}}
			}
			_, _, err := rewriteImageResponseBody([]byte(tc.body), tc.format, false, transfer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// 大小写与空格不敏感
func TestRewrite_FormatCaseInsensitive(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png","b64_json":"QUJD"}]}`)
	out, rewrote, err := rewriteImageResponseBody(body, "  URL  ", false, okTransfer)
	require.NoError(t, err)
	require.True(t, rewrote)

	data := parseData(t, out)
	_, hasB64 := data[0]["b64_json"]
	assert.False(t, hasB64)
}

// 非生图 JSON / 空 data → 返回 error，调用方原样透传
func TestRewrite_UnrecognizedBody(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"error":{"message":"boom"}}`),
		[]byte(`{"data":[]}`),
		[]byte(`{"data":"not-an-array"}`),
	} {
		_, rewrote, err := rewriteImageResponseBody(body, "url", false, okTransfer)
		assert.Error(t, err, "body=%s", body)
		assert.False(t, rewrote)
	}
}

// 无图可转（既无 url 也无 b64）时不调用转存、不改写
func TestRewrite_NoImageFieldsInData(t *testing.T) {
	body := []byte(`{"data":[{"revised_prompt":"only text"}]}`)
	transfer := func(int, service.ImageSource, bool) *service.ImageTransferResult {
		t.Error("没有图片字段时不应调用转存")
		return nil
	}

	_, rewrote, err := rewriteImageResponseBody(body, "url", false, transfer)
	require.NoError(t, err)
	assert.False(t, rewrote)
}

// b64_json 模式下转存成功但既无原 b64 也无回传字节 → 该张不动
func TestRewrite_B64ModeWithoutBytesKeepsUpstream(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://upstream/x.png"}]}`)
	transfer := func(int, service.ImageSource, bool) *service.ImageTransferResult {
		return &service.ImageTransferResult{SignedURL: "https://s/x.png"} // 无 Raw
	}

	out, rewrote, err := rewriteImageResponseBody(body, "b64_json", false, transfer)
	require.NoError(t, err)
	assert.False(t, rewrote)
	assert.Equal(t, body, out)
}

// 实体校验 header 清理清单必须覆盖描述旧 representation 的全部 header
func TestImageEntityHeadersToStrip(t *testing.T) {
	for _, key := range []string{
		"ETag", "Content-MD5", "Digest", "Content-Digest",
		"Repr-Digest", "Content-Encoding", "Content-Range", "Last-Modified",
	} {
		assert.Contains(t, imageEntityHeadersToStrip, key)
	}
}
