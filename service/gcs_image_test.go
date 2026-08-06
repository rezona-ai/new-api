package service

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
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
