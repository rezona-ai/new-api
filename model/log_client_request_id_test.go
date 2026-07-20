package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAttachClientRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("nil context leaves other untouched", func(t *testing.T) {
		other := map[string]interface{}{"a": 1}
		got := attachClientRequestID(nil, other)
		if got["a"] != 1 {
			t.Fatalf("expected original map, got %#v", got)
		}
		if _, ok := got["client_request_id"]; ok {
			t.Fatalf("did not expect client_request_id, got %#v", got)
		}
	})

	t.Run("missing header is a no-op", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		got := attachClientRequestID(c, nil)
		if got != nil {
			t.Fatalf("expected nil other, got %#v", got)
		}
	})

	t.Run("stores trimmed X-Request-Id", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("x-request-id", "  req-abc-123  ")
		got := attachClientRequestID(c, nil)
		if got["client_request_id"] != "req-abc-123" {
			t.Fatalf("unexpected value: %#v", got["client_request_id"])
		}
	})

	t.Run("does not overwrite existing value", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-Request-Id", "from-header")
		other := map[string]interface{}{"client_request_id": "already-set"}
		got := attachClientRequestID(c, other)
		if got["client_request_id"] != "already-set" {
			t.Fatalf("expected keep existing, got %#v", got["client_request_id"])
		}
	})

	t.Run("truncates overlong values", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		long := make([]byte, clientRequestIDMaxLen+50)
		for i := range long {
			long[i] = 'a'
		}
		c.Request.Header.Set("X-Request-Id", string(long))
		got := attachClientRequestID(c, map[string]interface{}{})
		v, _ := got["client_request_id"].(string)
		if len(v) != clientRequestIDMaxLen {
			t.Fatalf("expected len %d, got %d", clientRequestIDMaxLen, len(v))
		}
	})
}
