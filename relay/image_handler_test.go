package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newImageTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	return c, rec
}

// 对象名规则：{prefix}/{yyyymmdd}/{requestID}_{index}_{rand4}.{ext}
// requestID 用于按请求追溯；rand4 保证跨渠道重试也不会撞对象名。
func TestImageObjectNamer(t *testing.T) {
	namer := imageObjectNamer("api/image", "req-abc", "20260806")

	first := namer(0, "png")
	second := namer(1, "png")

	assert.Regexp(t, `^api/image/20260806/req-abc_0_[0-9a-f]{4}\.png$`, first)
	assert.Regexp(t, `^api/image/20260806/req-abc_1_[0-9a-f]{4}\.png$`, second)
	assert.NotEqual(t, first, second)

	// 同一 namer 对同一 index 必须稳定（转存内部可能多次调用）
	assert.Equal(t, first, namer(0, "png"))
}

// requestID 缺失时也要生成合法对象名，不能出现空段
func TestImageObjectNamer_EmptyRequestID(t *testing.T) {
	namer := imageObjectNamer("api/image", "", "20260806")
	assert.Regexp(t, `^api/image/20260806/unknown_0_[0-9a-f]{4}\.png$`, namer(0, "png"))
}

// 改写成功：Content-Length 按新 body 重设，实体校验 header 删除
func TestCommitImageResponse_RewriteSuccess(t *testing.T) {
	c, rec := newImageTestContext()
	cw := helper.NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw
	cw.Header().Set("Content-Type", "application/json")
	cw.Header().Set("ETag", `"upstream"`)
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte(`{"data":[{"url":"https://upstream/x.png"}]}`))

	newBody := []byte(`{"data":[{"url":"https://signed/x.png"}]}`)
	require.NoError(t, commitImageResponse(c, cw, newBody))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, string(newBody), rec.Body.String())
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Equal(t, "41", rec.Header().Get("Content-Length"), "Content-Length 必须按新 body 的 41 字节重设")
}

// 未改写：原样提交，header 不动
func TestCommitImageResponse_Passthrough(t *testing.T) {
	c, rec := newImageTestContext()
	cw := helper.NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw
	cw.Header().Set("Content-Type", "application/json")
	cw.Header().Set("ETag", `"upstream"`)
	cw.WriteHeader(http.StatusOK)
	original := `{"data":[{"b64_json":"QUJD"}]}`
	_, _ = cw.Write([]byte(original))

	require.NoError(t, commitImageResponse(c, cw, nil))

	assert.JSONEq(t, original, rec.Body.String())
	assert.Equal(t, `"upstream"`, rec.Header().Get("ETag"), "未改写时不动上游 header")
}

// 已提交（SSE/超限直通）时再提交是 no-op
func TestCommitImageResponse_AlreadyCommitted(t *testing.T) {
	c, rec := newImageTestContext()
	cw := helper.NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw
	cw.Header().Set("Content-Type", "text/event-stream")
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte("data: chunk\n\n"))
	require.True(t, cw.Committed())

	require.NoError(t, commitImageResponse(c, cw, []byte(`{"ignored":true}`)))
	assert.Contains(t, rec.Body.String(), "data: chunk")
	assert.NotContains(t, rec.Body.String(), "ignored")
}
