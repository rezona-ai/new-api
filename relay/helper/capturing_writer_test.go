package helper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCaptureTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	return c, rec
}

// buffered 模式必须吞掉 Flush()：IOCopyBytesGracefully 对普通非流式 JSON 也会
// 无条件调用 Flush（service/http.go:126），把它当作"必须立刻透传"会让主流渠道
// 全部绕过改写。
func TestCapturingWriter_SwallowsFlush(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, err := c.Writer.Write([]byte(`{"data":[]}`))
	require.NoError(t, err)
	c.Writer.Flush()

	assert.False(t, cw.Committed(), "Flush 不得触发提交")
	assert.Empty(t, rec.Body.String(), "提交前底层不应有任何字节")
	assert.Equal(t, `{"data":[]}`, string(cw.Body()))
	assert.Equal(t, http.StatusOK, cw.CapturedStatus())
	assert.True(t, cw.Written(), "逻辑上已写入")
}

func TestCapturingWriter_CommitBodyReplacesPayload(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("ETag", `"upstream-etag"`)
	c.Writer.Header().Set("Content-Length", "11")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte(`{"data":[]}`))

	newBody := []byte(`{"data":[{"url":"https://signed/x"}]}`)
	err := cw.CommitBody(newBody, func(h http.Header) {
		h.Del("ETag")
	})
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(newBody), rec.Body.String())
	assert.Empty(t, rec.Header().Get("ETag"), "改写后实体校验 header 必须删除")
	assert.Equal(t, "37", rec.Header().Get("Content-Length"), "Content-Length 必须按新 body 重设")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

func TestCapturingWriter_CommitPassesThroughUnchanged(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusCreated)
	_, _ = c.Writer.WriteString(`{"raw":true}`)
	require.NoError(t, cw.Commit())

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, `{"raw":true}`, rec.Body.String())
}

// 错误路径：丢弃缓冲后底层 header 一个字节都不能被污染，
// 否则会污染外层的渠道重试与统一错误响应。
func TestCapturingWriter_DiscardLeavesUnderlyingUntouched(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	orig := cw.Unwrap()
	c.Writer = cw

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("X-Channel-Junk", "leak-me")
	c.Writer.WriteHeader(http.StatusBadGateway)
	_, _ = c.Writer.Write([]byte(`{"error":"upstream boom"}`))

	cw.Discard()
	c.Writer = orig

	assert.Empty(t, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Header().Get("X-Channel-Junk"))
	assert.False(t, orig.Written())
}

// 丢弃后外层仍能写出干净的错误响应（模拟 controller/relay.go:89 的 c.JSON）
func TestCapturingWriter_DiscardThenOuterErrorResponse(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	orig := cw.Unwrap()
	c.Writer = cw

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusBadGateway)
	_, _ = c.Writer.Write([]byte(`{"error":"channel body"}`))
	cw.Discard()
	c.Writer = orig

	c.JSON(http.StatusInternalServerError, gin.H{"error": "unified"})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"unified"}`, rec.Body.String())
}

// SSE 必须在三个检查点都能识别：WriteHeader / WriteHeaderNow / 首次 Write
func TestCapturingWriter_SSESwitchesToPassthrough(t *testing.T) {
	for _, trigger := range []string{"WriteHeader", "WriteHeaderNow", "Write"} {
		t.Run(trigger, func(t *testing.T) {
			c, rec := newCaptureTestContext()
			cw := NewCapturingWriter(c.Writer, 1<<20)
			c.Writer = cw
			c.Writer.Header().Set("Content-Type", "text/event-stream")

			switch trigger {
			case "WriteHeader":
				c.Writer.WriteHeader(http.StatusOK)
			case "WriteHeaderNow":
				c.Writer.WriteHeaderNow()
			case "Write":
				_, _ = c.Writer.Write([]byte("data: hi\n\n"))
			}

			assert.True(t, cw.Committed(), "SSE 必须立即切直通")
			_, _ = c.Writer.Write([]byte("data: more\n\n"))
			assert.Contains(t, rec.Body.String(), "data: more")
		})
	}
}

// 缓冲超限：先把已缓冲内容按序写出，再切直通，绝不丢字节
func TestCapturingWriter_OverCapacitySwitchesToPassthrough(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 8)
	c.Writer = cw

	c.Writer.WriteHeader(http.StatusOK)
	_, err := c.Writer.Write([]byte("12345"))
	require.NoError(t, err)
	assert.False(t, cw.Committed())

	_, err = c.Writer.Write([]byte("67890"))
	require.NoError(t, err)
	assert.True(t, cw.Committed(), "超过 captureMax 必须切直通")
	assert.Equal(t, "1234567890", rec.Body.String(), "已缓冲的字节必须按序补写，不能丢")
}

// Size() 必须反映真实写入字节数（中间件与 gin 内部会读它）
func TestCapturingWriter_TracksStatusAndSize(t *testing.T) {
	c, _ := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	assert.Equal(t, http.StatusOK, cw.Status(), "默认状态码与 gin 一致")
	assert.False(t, cw.Written())

	c.Writer.WriteHeader(http.StatusAccepted)
	assert.False(t, cw.Written(), "WriteHeader 只记状态码，与 gin 语义一致")

	_, _ = c.Writer.Write([]byte("abcd"))

	assert.Equal(t, http.StatusAccepted, cw.Status())
	assert.Equal(t, 4, cw.Size())
	assert.True(t, cw.Written())
}

// 二次提交必须是 no-op，避免重复写响应
func TestCapturingWriter_DoubleCommitIsNoop(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte("once"))
	require.NoError(t, cw.Commit())
	require.NoError(t, cw.Commit())

	assert.Equal(t, "once", rec.Body.String())
}

// 已提交后 Discard 无效（响应已发出，无法回滚）
func TestCapturingWriter_DiscardAfterCommitIsNoop(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte("sent"))
	require.NoError(t, cw.Commit())
	cw.Discard()

	assert.Equal(t, "sent", rec.Body.String())
	assert.True(t, cw.Committed())
}

// 未显式 WriteHeader 时按 gin 默认 200 提交（渠道可能只调 Write）
func TestCapturingWriter_DefaultStatusWhenNoWriteHeader(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 1<<20)
	c.Writer = cw

	_, _ = c.Writer.Write([]byte(`{"ok":true}`))
	require.NoError(t, cw.Commit())

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"ok":true}`, rec.Body.String())
}

// captureMax <= 0 表示不限制缓冲
func TestCapturingWriter_UnlimitedCapture(t *testing.T) {
	c, rec := newCaptureTestContext()
	cw := NewCapturingWriter(c.Writer, 0)
	c.Writer = cw

	big := make([]byte, 1<<16)
	for i := range big {
		big[i] = 'a'
	}
	_, err := c.Writer.Write(big)
	require.NoError(t, err)
	assert.False(t, cw.Committed(), "captureMax<=0 时不应因体积切直通")
	assert.Empty(t, rec.Body.String())
	assert.Len(t, cw.Body(), 1<<16)
}
