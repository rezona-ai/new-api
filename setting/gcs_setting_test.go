package setting

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInitGCSSettings_ImageDefaults(t *testing.T) {
	for _, k := range []string{
		"GCS_IMAGE_TRANSFER_ENABLED", "GCS_READ_ONLY_ENABLED", "GCS_IMAGE_PREFIX",
		"GCS_IMAGE_SIGNED_URL_TTL", "GCS_IMAGE_MAX_SIZE", "GCS_IMAGE_TRANSFER_TIMEOUT",
		"GCS_IMAGE_STREAM_TIMEOUT", "GCS_IMAGE_MJ_TIMEOUT", "GCS_IMAGE_CONCURRENCY",
		"GCS_IMAGE_CAPTURE_MAX", "GCS_IMAGE_STRIP_B64_WHEN_URL", "GCS_IMAGE_DROP_ALI_METADATA",
		"GCS_SIGN_CACHE_MAX_ENTRIES",
	} {
		os.Unsetenv(k)
	}

	InitGCSSettings()

	assert.False(t, GCSImageTransferEnabled)
	assert.False(t, GCSReadOnlyEnabled)
	assert.Equal(t, "api/image", GCSImagePrefix)
	assert.Equal(t, 168*time.Hour, GCSImageSignedURLTTL)
	assert.Equal(t, int64(32<<20), GCSImageMaxSize)
	assert.Equal(t, 60*time.Second, GCSImageTransferTimeout)
	assert.Equal(t, 5*time.Second, GCSImageStreamTimeout)
	assert.Equal(t, 30*time.Second, GCSImageMJTimeout)
	assert.Equal(t, 4, GCSImageConcurrency)
	assert.Equal(t, int64(64<<20), GCSImageCaptureMax)
	assert.False(t, GCSImageStripB64WhenURL)
	assert.True(t, GCSImageDropAliMetadata, "默认开启，防止 Ali metadata 泄露上游直链")
	assert.Equal(t, 10000, GCSSignCacheMaxEntries)
}

// V4 签名 URL 上限是 7 天，超过 GCS 会直接拒签，因此解析时必须钳制。
func TestInitGCSSettings_ClampsSignedURLTTL(t *testing.T) {
	os.Setenv("GCS_IMAGE_SIGNED_URL_TTL", "720h") // 30 天，超上限
	os.Setenv("GCS_SIGNED_URL_TTL", "240h")       // 10 天，超上限（视频侧同样钳制）
	defer func() {
		os.Unsetenv("GCS_IMAGE_SIGNED_URL_TTL")
		os.Unsetenv("GCS_SIGNED_URL_TTL")
	}()

	InitGCSSettings()

	assert.Equal(t, GCSMaxV4SignedURLTTL, GCSImageSignedURLTTL)
	assert.Equal(t, GCSMaxV4SignedURLTTL, GCSSignedURLTTL)
	assert.Equal(t, 7*24*time.Hour, GCSMaxV4SignedURLTTL)
}

func TestInitGCSSettings_PrefixTrimsSlashes(t *testing.T) {
	os.Setenv("GCS_IMAGE_PREFIX", "/custom/img/")
	defer os.Unsetenv("GCS_IMAGE_PREFIX")

	InitGCSSettings()

	assert.Equal(t, "custom/img", GCSImagePrefix)
}
