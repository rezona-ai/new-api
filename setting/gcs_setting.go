package setting

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// GCS 视频任务结果转存配置（环境变量驱动，进程启动时经 InitGCSSettings 一次性加载）。
// 设计文档：gcs-video-transfer-design.md 4.6。
//
// 配置约束（见设计文档 4.4 重试模型 / 风险 4）：
//   - GCSTransferDeadline 必须远大于 GCSTransferTimeout 与最坏排队时间，且小于各渠道直链最短时效；
//   - TASK_TIMEOUT_MINUTES 必须显著大于「最长上游生成时间 + GCSTransferDeadline」，
//     否则全局 sweep 会先于 transferDeadline 误杀在途转存。
var (
	// GCSTransferEnabled 紧急开关：关闭后回退直链透传（GCS 故障时止血，切换语义见设计文档 4.6）
	GCSTransferEnabled bool
	// GCSResultBucket 转存目标 bucket
	GCSResultBucket string
	// GCSResultPrefix 对象前缀（首尾不含斜杠）
	GCSResultPrefix string
	// GCSSignedURLTTL V4 签名链接有效期（V4 上限 7 天）
	GCSSignedURLTTL time.Duration
	// GCSResultRetentionDays 结果保留期（天），必须与 bucket 生命周期规则保持一致；
	// 读取侧据此判过期，属对外 API 契约的一部分
	GCSResultRetentionDays int
	// GCSTransferDeadline 转存墙钟截止：now - UpstreamDoneAt 超过该值，
	// 轮询侧 CAS 翻 FAILURE、CAS 赢才退款
	GCSTransferDeadline time.Duration
	// GCSTransferConcurrency worker 并发转存数
	GCSTransferConcurrency int
	// GCSTransferTimeout 单次转存（整任务全部对象）超时，必须经 context 强制
	GCSTransferTimeout time.Duration
	// GCSMaxObjectSize 单对象体积上限（字节）
	GCSMaxObjectSize int64
	// GCSSignCacheTTL 签名缓存 TTL（Workload Identity/SignBlob 路径防止高频轮询放大签名调用）
	GCSSignCacheTTL time.Duration

	// ── 生图结果转存（image-gen-cdn 设计 4.8）──

	// GCSImageTransferEnabled 生图转存总开关，独立于视频侧 GCSTransferEnabled
	GCSImageTransferEnabled bool
	// GCSReadOnlyEnabled 两个写开关都关闭时仍初始化 GCS client，
	// 以便读取历史 gs:// 结果（否则存量 MJ 结果会变成裸 gs:// 泄露给用户）
	GCSReadOnlyEnabled bool
	// GCSImagePrefix 生图对象前缀（首尾不含斜杠），与视频 api/video 同 bucket 不同前缀
	GCSImagePrefix string
	// GCSImageSignedURLTTL 生图签名链接有效期。/v1/images/* 无任务记录、无法二次现签，
	// 故取 V4 协议上限 7 天（超限会被钳制到 GCSMaxV4SignedURLTTL）
	GCSImageSignedURLTTL time.Duration
	// GCSImageMaxSize 单图体积上限（字节）
	GCSImageMaxSize int64
	// GCSImageTransferTimeout 同步 HTTP 路径的单图转存预算，必须经 context 强制
	GCSImageTransferTimeout time.Duration
	// GCSImageStreamTimeout 流式路径的单图转存预算。必须严格小于 stream_scanner 的
	// 10s ping 等待上限，否则会打掉 keepalive（设计 4.6.1）
	GCSImageStreamTimeout time.Duration
	// GCSImageMJTimeout Midjourney 后台转存的单图预算
	GCSImageMJTimeout time.Duration
	// GCSImageConcurrency 并发转存数上限（同一响应内 / MJ 批内共用该语义）
	GCSImageConcurrency int
	// GCSImageCaptureMax 生图响应缓冲上限（字节），超过即放弃改写、切直通
	GCSImageCaptureMax int64
	// GCSImageStripB64WhenURL 客户端未传 response_format 且转存成功时删除 b64_json
	//（响应瘦身，属行为变更，默认关闭）
	GCSImageStripB64WhenURL bool
	// GCSImageDropAliMetadata 仅 Ali 渠道：转存成功时删除顶层 metadata
	//（Ali 把完整上游响应写进该字段，内含上游直链）。其他渠道 metadata 一律保留
	GCSImageDropAliMetadata bool
	// GCSSignCacheMaxEntries 签名缓存条目上限 + 惰性清扫阈值。
	// 现有缓存是无容量上限的 sync.Map，一次性对象名会导致条目无界增长
	GCSSignCacheMaxEntries int
)

// InitGCSSettings 从环境变量加载 GCS 转存配置，必须在 common.InitEnv 之后、
// service.InitGCSStorage 之前调用（见 main.go InitResources）。
func InitGCSSettings() {
	GCSTransferEnabled = common.GetEnvOrDefaultBool("GCS_TRANSFER_ENABLED", false)
	GCSResultBucket = common.GetEnvOrDefaultString("GCS_RESULT_BUCKET", "taluna-api-result")
	GCSResultPrefix = strings.Trim(common.GetEnvOrDefaultString("GCS_RESULT_PREFIX", "api/video"), "/")
	GCSSignedURLTTL = getEnvDuration("GCS_SIGNED_URL_TTL", 12*time.Hour)
	GCSResultRetentionDays = common.GetEnvOrDefault("GCS_RESULT_RETENTION_DAYS", 30)
	GCSTransferDeadline = getEnvDuration("GCS_TRANSFER_DEADLINE", 2*time.Hour)
	GCSTransferConcurrency = common.GetEnvOrDefault("GCS_TRANSFER_CONCURRENCY", 4)
	GCSTransferTimeout = getEnvDuration("GCS_TRANSFER_TIMEOUT", 10*time.Minute)
	GCSMaxObjectSize = getEnvByteSize("GCS_MAX_OBJECT_SIZE", 2<<30) // 2 GiB
	GCSSignCacheTTL = getEnvDuration("GCS_SIGN_CACHE_TTL", 10*time.Minute)

	GCSImageTransferEnabled = common.GetEnvOrDefaultBool("GCS_IMAGE_TRANSFER_ENABLED", false)
	GCSReadOnlyEnabled = common.GetEnvOrDefaultBool("GCS_READ_ONLY_ENABLED", false)
	GCSImagePrefix = strings.Trim(common.GetEnvOrDefaultString("GCS_IMAGE_PREFIX", "api/image"), "/")
	GCSImageSignedURLTTL = clampV4SignedURLTTL("GCS_IMAGE_SIGNED_URL_TTL", getEnvDuration("GCS_IMAGE_SIGNED_URL_TTL", GCSMaxV4SignedURLTTL))
	GCSImageMaxSize = getEnvByteSize("GCS_IMAGE_MAX_SIZE", 32<<20) // 32 MiB
	GCSImageTransferTimeout = getEnvDuration("GCS_IMAGE_TRANSFER_TIMEOUT", 60*time.Second)
	GCSImageStreamTimeout = getEnvDuration("GCS_IMAGE_STREAM_TIMEOUT", 5*time.Second)
	GCSImageMJTimeout = getEnvDuration("GCS_IMAGE_MJ_TIMEOUT", 30*time.Second)
	GCSImageConcurrency = common.GetEnvOrDefault("GCS_IMAGE_CONCURRENCY", 4)
	GCSImageCaptureMax = getEnvByteSize("GCS_IMAGE_CAPTURE_MAX", 64<<20) // 64 MiB
	GCSImageStripB64WhenURL = common.GetEnvOrDefaultBool("GCS_IMAGE_STRIP_B64_WHEN_URL", false)
	GCSImageDropAliMetadata = common.GetEnvOrDefaultBool("GCS_IMAGE_DROP_ALI_METADATA", true)
	GCSSignCacheMaxEntries = common.GetEnvOrDefault("GCS_SIGN_CACHE_MAX_ENTRIES", 10000)

	// 视频侧 TTL 同样钳制：超过 V4 上限时 GCS 直接拒签，签出来也是废的
	GCSSignedURLTTL = clampV4SignedURLTTL("GCS_SIGNED_URL_TTL", GCSSignedURLTTL)
}

// getEnvDuration 解析 time.ParseDuration 格式的环境变量（如 "12h"、"10m"），
// 解析失败时记录错误并使用默认值（与 common.GetEnvOrDefault 行为一致）。
func getEnvDuration(env string, defaultValue time.Duration) time.Duration {
	raw := os.Getenv(env)
	if raw == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		common.SysError(fmt.Sprintf("failed to parse %s: %q is not a valid positive duration, using default value: %s", env, raw, defaultValue))
		return defaultValue
	}
	return d
}

// getEnvByteSize 解析体积环境变量，支持纯字节数或 KiB/MiB/GiB/KB/MB/GB 后缀（如 "2GiB"、"512MiB"），
// 解析失败时记录错误并使用默认值。
func getEnvByteSize(env string, defaultValue int64) int64 {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return defaultValue
	}
	n, err := parseByteSize(raw)
	if err != nil || n <= 0 {
		common.SysError(fmt.Sprintf("failed to parse %s: %q is not a valid positive byte size, using default value: %d", env, raw, defaultValue))
		return defaultValue
	}
	return n
}

func parseByteSize(s string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(s))
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"GIB", 1 << 30}, {"GB", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, unit.suffix) {
			multiplier = unit.factor
			upper = strings.TrimSpace(strings.TrimSuffix(upper, unit.suffix))
			break
		}
	}
	num, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, err
	}
	return num * multiplier, nil
}

// GCSMaxV4SignedURLTTL V4 签名 URL 的协议上限（7 天）。超过该值 GCS 直接拒签，
// 因此配置解析阶段就要钳制——getEnvDuration 只校验正数，不认这个上限。
const GCSMaxV4SignedURLTTL = 7 * 24 * time.Hour

// clampV4SignedURLTTL 把签名 TTL 钳制到 V4 协议上限，并对超限配置记录错误日志。
func clampV4SignedURLTTL(env string, d time.Duration) time.Duration {
	if d > GCSMaxV4SignedURLTTL {
		common.SysError(fmt.Sprintf("%s=%s exceeds the V4 signed URL limit %s, clamped to the limit", env, d, GCSMaxV4SignedURLTTL))
		return GCSMaxV4SignedURLTTL
	}
	return d
}
