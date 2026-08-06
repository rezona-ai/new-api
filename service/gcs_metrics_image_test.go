package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 失败分类必须可区分：上游 CDN 提前过期表现为 download-fail 上升，
// GCS 故障表现为 gcs-service-fail 上升，两者的处置动作完全不同。
func TestImageFailKind(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"超体积", fmt.Errorf("wrap: %w", errGCSAssetOversize), "oversize"},
		{"损坏对象", fmt.Errorf("wrap: %w", ErrGCSObjectCorrupted), "corrupt-object"},
		{"对象已存在", fmt.Errorf("wrap: %w", ErrGCSObjectExists), "gcs-service-fail"},
		{"下载失败", errors.New("gcs image transfer: download failed: connection reset"), "download-fail"},
		{"上游非 200", errors.New("gcs image transfer: upstream returned 403: expired"), "download-fail"},
		{"解码失败", errors.New("gcs image transfer: decode base64 failed: illegal data"), "decode-fail"},
		{"MIME 拒绝", errors.New(`gcs image transfer: unsupported image mime (upstream="text/html" hint="")`), "mime-reject"},
		{"其他", errors.New("boom"), "gcs-service-fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, imageFailKind(tc.err))
		})
	}
}

func TestRecordImageFailure_IncrementsCorrectCounter(t *testing.T) {
	before := gcsMetrics.imageDownloadFail.Load()
	gcsMetrics.recordImageFailure("download-fail")
	assert.Equal(t, before+1, gcsMetrics.imageDownloadFail.Load())

	beforeMime := gcsMetrics.imageMimeReject.Load()
	gcsMetrics.recordImageFailure("mime-reject")
	assert.Equal(t, beforeMime+1, gcsMetrics.imageMimeReject.Load())

	beforeSvc := gcsMetrics.imageGCSServiceFail.Load()
	gcsMetrics.recordImageFailure("unknown-kind")
	assert.Equal(t, beforeSvc+1, gcsMetrics.imageGCSServiceFail.Load(), "未知分类归入 service-fail")
}

// 生图计数器必须进 countersTotal，否则 reporter 不会因为它们变化而打日志
func TestImageCountersIncludedInTotal(t *testing.T) {
	before := gcsMetrics.countersTotal()
	gcsMetrics.imagePassthrough.Add(1)
	assert.Equal(t, before+1, gcsMetrics.countersTotal())
}

func TestRecordImageDuration(t *testing.T) {
	gcsMetrics.recordImageDuration("17", 3*time.Second)
	v, ok := gcsMetrics.imageDurations.Load("17")
	assert.True(t, ok)
	h := v.(*gcsDurationHist)
	assert.Equal(t, int64(1), h.count.Load())
	assert.Equal(t, int64(3000), h.sumMs.Load())

	// 空标签归入 unknown，不能丢样本
	gcsMetrics.recordImageDuration("", time.Second)
	_, ok = gcsMetrics.imageDurations.Load("unknown")
	assert.True(t, ok)
}

// 日志行必须包含全部生图计数器名，否则采集侧抽不到
func TestImageCountersLogLine(t *testing.T) {
	line := gcsMetrics.imageCountersLogLine()
	for _, key := range []string{
		"gcs-metrics image", "success=", "download_fail=", "decode_fail=", "mime_reject=",
		"oversize=", "gcs_auth_fail=", "gcs_service_fail=", "corrupt_object=",
		"sign_fail=", "passthrough=", "bytes_downloaded=", "bytes_uploaded=",
	} {
		assert.Contains(t, line, key)
	}
}
