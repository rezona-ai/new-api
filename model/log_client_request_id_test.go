package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestClientRequestIDFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("nil context returns empty", func(t *testing.T) {
		if got := clientRequestIDFromContext(nil); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("missing header returns empty", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if got := clientRequestIDFromContext(c); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("stores trimmed X-Request-Id", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("x-request-id", "  req-abc-123  ")
		if got := clientRequestIDFromContext(c); got != "req-abc-123" {
			t.Fatalf("unexpected value: %q", got)
		}
	})

	t.Run("case-insensitive header", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-REQUEST-ID", "mixed-case")
		if got := clientRequestIDFromContext(c); got != "mixed-case" {
			t.Fatalf("unexpected value: %q", got)
		}
	})

	t.Run("truncates overlong values", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		long := strings.Repeat("a", clientRequestIDMaxLen+50)
		c.Request.Header.Set("X-Request-Id", long)
		got := clientRequestIDFromContext(c)
		if len(got) != clientRequestIDMaxLen {
			t.Fatalf("expected len %d, got %d", clientRequestIDMaxLen, len(got))
		}
		if got != long[:clientRequestIDMaxLen] {
			t.Fatalf("truncated value mismatch")
		}
	})

	t.Run("whitespace-only header returns empty", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-Request-Id", "   ")
		if got := clientRequestIDFromContext(c); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}
