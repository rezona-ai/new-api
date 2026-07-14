package helper

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// 构造与 capability-gateway 一致的 image edit multipart：model 字段在前，
// 大文件 part 在后。
func buildImageEditMultipart(imgSize int) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "gpt-image-2")
	_ = w.WriteField("size", "2048x1152")
	_ = w.WriteField("quality", "medium")
	_ = w.WriteField("prompt", "expand it")
	_ = w.WriteField("n", "1")
	for _, field := range []string{"image", "mask"} {
		fw, _ := w.CreateFormFile(field, field+".png")
		_, _ = fw.Write(bytes.Repeat([]byte{0x89}, imgSize))
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func TestParseImageEditFormValues_ModelPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, sz := range []int{1024, 2 << 20, 8 << 20} {
		body, ct := buildImageEditMultipart(sz)
		req := httptest.NewRequest("POST", "/v1/images/edits", body)
		req.Header.Set("Content-Type", ct)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		vals, err := parseImageEditFormValues(c)
		if err != nil {
			t.Fatalf("sz=%d: %v", sz, err)
		}
		if vals.Get("model") != "gpt-image-2" {
			t.Fatalf("sz=%d: model = %q, want gpt-image-2", sz, vals.Get("model"))
		}
		if vals.Get("size") != "2048x1152" || vals.Get("prompt") != "expand it" {
			t.Fatalf("sz=%d: other fields wrong: %+v", sz, vals)
		}
	}
}

// 复现修复的核心：前置逻辑先经 body storage 读过一遍（如 distributor 选渠道），
// parseImageEditFormValues 仍须能取到 model —— 而非像旧代码依赖 gin 一次性
// c.Request.PostForm 那样间歇为空。
func TestParseImageEditFormValues_RobustAfterPriorRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body, ct := buildImageEditMultipart(2 << 20)
	req := httptest.NewRequest("POST", "/v1/images/edits", body)
	req.Header.Set("Content-Type", ct)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	// 模拟前置中间件用可复用体读过一次（distributor 就是这么选渠道的）。
	var probe struct {
		Model string `json:"model"`
	}
	if err := common.UnmarshalBodyReusable(c, &probe); err != nil {
		t.Fatalf("prior read: %v", err)
	}
	// 再取一次：必须仍拿到 model。
	vals, err := parseImageEditFormValues(c)
	if err != nil {
		t.Fatalf("after prior read: %v", err)
	}
	if vals.Get("model") != "gpt-image-2" {
		t.Fatalf("model lost after prior body read: %q", vals.Get("model"))
	}
	// 且 body 复位后，gin 仍能解析出文件部分（下游 ConvertImageRequest 依赖）。
	if _, err := c.MultipartForm(); err != nil {
		t.Fatalf("multipart form still parseable: %v", err)
	}
	if c.Request.MultipartForm == nil || len(c.Request.MultipartForm.File["image"]) == 0 {
		t.Fatal("file parts must remain available after value parse")
	}
}
