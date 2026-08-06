package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPermissiveFetchSetting 放开 SSRF 的私网与端口限制，让 httptest 的随机端口
// （127.0.0.1:随机高位端口）能过校验。SSRF 校验本身仍然执行——只是放宽策略，
// 否则用例会因为 "port ... is not allowed" 提前返回，造成"假通过"。
func withPermissiveFetchSetting(t *testing.T) {
	t.Helper()
	fs := system_setting.GetFetchSetting()
	origPrivate, origPorts := fs.AllowPrivateIp, fs.AllowedPorts
	fs.AllowPrivateIp = true
	fs.AllowedPorts = []string{"1024-65535"}
	t.Cleanup(func() {
		fs.AllowPrivateIp, fs.AllowedPorts = origPrivate, origPorts
	})
}

func TestDoDownloadRequestWithContext_Success(t *testing.T) {
	withPermissiveFetchSetting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	InitHttpClient()

	resp, err := DoDownloadRequestWithContext(context.Background(), srv.URL, "test")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
}

// context 取消必须真的中止请求——这是 GCS_IMAGE_TRANSFER_TIMEOUT 生效的前提。
func TestDoDownloadRequestWithContext_CancelAborts(t *testing.T) {
	withPermissiveFetchSetting(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte("too late"))
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	InitHttpClient()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := DoDownloadRequestWithContext(ctx, srv.URL, "test")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "必须是 context 超时导致的失败，而不是 SSRF 拒绝等其他原因")
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "不能在 context 到期前就返回（否则说明失败原因不是超时）")
	assert.Less(t, elapsed, time.Second, "必须在 context 超时时立刻返回，而不是等服务端")
}

// SSRF 校验必须仍然生效：不在白名单内的端口要被拒绝，且不发起真实请求。
func TestDoDownloadRequestWithContext_SSRFStillEnforced(t *testing.T) {
	fs := system_setting.GetFetchSetting()
	origEnabled, origPorts := fs.EnableSSRFProtection, fs.AllowedPorts
	fs.EnableSSRFProtection = true
	fs.AllowedPorts = []string{"443"}
	defer func() {
		fs.EnableSSRFProtection, fs.AllowedPorts = origEnabled, origPorts
	}()
	InitHttpClient()

	_, err := DoDownloadRequestWithContext(context.Background(), "http://example.com:12345/a.png", "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request reject")
}
