package service

import (
	"testing"

	"cloud.google.com/go/storage"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
)

// storageClientStub 仅用于让 gcsClient != nil 成立，不会发起任何 GCS 调用。
var storageClientStub = storage.Client{}

// 三态就绪必须相互独立：只开图片开关时视频 worker 绝不能被启动
// （gcs_transfer.go 用 GCSVideoTransferReady 作为 Submit 的前置条件）。
func TestGCSReadinessTriState(t *testing.T) {
	origVideo, origImage := setting.GCSTransferEnabled, setting.GCSImageTransferEnabled
	origClient := gcsClient
	defer func() {
		setting.GCSTransferEnabled, setting.GCSImageTransferEnabled = origVideo, origImage
		gcsClient = origClient
	}()

	// client 未初始化：三者全 false
	gcsClient = nil
	setting.GCSTransferEnabled, setting.GCSImageTransferEnabled = true, true
	assert.False(t, GCSClientReady())
	assert.False(t, GCSVideoTransferReady())
	assert.False(t, GCSImageTransferReady())

	// client 可用但两个写开关都关：只有读就绪
	gcsClient = &storageClientStub
	setting.GCSTransferEnabled, setting.GCSImageTransferEnabled = false, false
	assert.True(t, GCSClientReady())
	assert.False(t, GCSVideoTransferReady())
	assert.False(t, GCSImageTransferReady())

	// 只开图片：视频 worker 必须保持关闭
	setting.GCSImageTransferEnabled = true
	assert.True(t, GCSImageTransferReady())
	assert.False(t, GCSVideoTransferReady())

	// 只开视频：图片路径必须保持关闭
	setting.GCSTransferEnabled, setting.GCSImageTransferEnabled = true, false
	assert.True(t, GCSVideoTransferReady())
	assert.False(t, GCSImageTransferReady())
}
