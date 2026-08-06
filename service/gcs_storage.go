package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/storage"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting"
	"google.golang.org/api/googleapi"
)

// GCS 视频任务结果转存：存储层（上传 + V4 签名）。
// 设计文档：gcs-video-transfer-design.md 4.3 / 4.5 / 4.6。
//
// 职责边界：本文件只负责 GCS 对象的条件写、属性校验与 V4 签名；
// 取流、worker 调度、状态机（gcs_transfer.go / task_polling.go）不在此处。

var (
	// ErrGCSObjectExists 条件写命中已存在对象（If-GenerationMatch: 0 前置条件失败）。
	// 调用方应视为"该对象已完成转存"，经 GCSVerifyExistingObject 校验后直接复用。
	ErrGCSObjectExists = errors.New("gcs object already exists")
	// ErrGCSResultExpired 任务结果已超保留期（对象已被 bucket 生命周期规则删除，
	// V4 签名是离线计算、不校验对象存在性，签出的会是必 404 的死链，因此拒绝签名）。
	ErrGCSResultExpired = errors.New("gcs result expired: object retention period exceeded")
	// ErrGCSObjectCorrupted 已存在对象的 size/CRC32C 与预期不符（疑似半截 finalize 的损坏对象），
	// 调用方禁止复用，应按转存失败处理并告警（corrupt-object 指标）。
	ErrGCSObjectCorrupted = errors.New("gcs object corrupted: size/crc32c mismatch")
	// ErrGCSNotInitialized GCS client 未初始化（GCS_TRANSFER_ENABLED 未开启或初始化未执行）。
	ErrGCSNotInitialized = errors.New("gcs storage client not initialized")
)

// gcsSignSafetyMargin 签名 TTL 按剩余保留期收口时的安全余量：
// 剩余保留期不足该余量时直接返回过期错误，避免签出在有效期内可能被生命周期删除的 URL
// （删除按天批处理只会延后不会提前，但不能依赖该无上界的延迟），也避免 expires_at 虚标。
const gcsSignSafetyMargin = 10 * time.Minute

var gcsClient *storage.Client

// gcsSignCacheEntry 签名缓存条目。必须存 (signedURL, expiresAt) 二元组：
// 响应里的 expires_at 取真实签名过期时刻，不得虚标（设计文档 4.6）。
type gcsSignCacheEntry struct {
	signedURL string
	expiresAt time.Time
	signedAt  time.Time
}

// 签名缓存分区标签：不同 TTL 策略必须分区，否则视频 12h 与图片 7d 会互相污染
// 同一个 objectName 的缓存条目。空标签 = 不入缓存（设计 4.4）。
const (
	GCSSignCacheTagVideo = "video"
	GCSSignCacheTagImage = "image"
	GCSSignCacheTagMJ    = "mj"
)

var (
	gcsSignCache sync.Map // cacheKey -> gcsSignCacheEntry
	// gcsSignCacheCount 缓存条目计数：sync.Map 没有 Len()，而容量上限是防止
	// 一次性对象名把缓存撑成无界内存泄漏的唯一手段（设计 4.4）。
	gcsSignCacheCount atomic.Int64
)

// InitGCSStorage 进程启动时初始化 GCS client，必须在 setting.InitGCSSettings 之后调用。
// 仅当 GCS_TRANSFER_ENABLED 开启时初始化；初始化失败（凭证缺失/格式错/网络不可达）
// 直接 fatal 退出、阻止进程启动——转存是计费关键路径，绝不静默带病启动（设计文档 4.3）。
func InitGCSStorage() {
	// 指标 reporter 与开关无关：计费失败计数（清单项 14）在开关关闭时同样需要上报，
	// 紧急开关关闭期间的降级完成（degrade_complete）也由它观测。
	startGCSMetricsReporter()
	// client 初始化条件 = 视频写 || 图片写 || 只读（设计 4.5）。
	// 只读开关的存在理由：MJ 一旦存了 gs://，关闭写开关后仍必须能签名读出，
	// 否则存量结果会变成裸 gs:// 泄露给用户。
	if !setting.GCSTransferEnabled && !setting.GCSImageTransferEnabled && !setting.GCSReadOnlyEnabled {
		common.SysLog("GCS transfer disabled (video/image/read-only all off), skip GCS storage client initialization")
		return
	}
	if setting.GCSResultBucket == "" {
		common.FatalLog("GCS transfer enabled but GCS_RESULT_BUCKET is empty")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := storage.NewClient(ctx)
	if err != nil {
		common.FatalLog(fmt.Sprintf("GCS transfer enabled but storage client initialization failed: %s", err.Error()))
		return
	}
	// 启动期自检：实签一个探针对象，验证凭证可用于 V4 签名
	// （SA key 路径为本地 RSA 签名；Workload Identity 路径会发起一次 SignBlob 调用，同时验证连通性）。
	probeObject := GCSObjectName("startup-probe", 0, "bin")
	if _, err := client.Bucket(setting.GCSResultBucket).SignedURL(probeObject, &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  http.MethodGet,
		Expires: time.Now().Add(time.Minute),
	}); err != nil {
		common.FatalLog(fmt.Sprintf("GCS transfer enabled but V4 URL signing self-check failed (check credentials / iam.serviceAccountTokenCreator): %s", err.Error()))
		return
	}
	gcsClient = client
	common.SysLog(fmt.Sprintf("GCS storage client initialized, bucket: %s, prefix: %s", setting.GCSResultBucket, setting.GCSResultPrefix))
}

// GCSClientReady 返回 GCS client 是否可用（读取/签名的前置条件）。
// 与任何写开关无关——历史 gs:// 结果在写开关关闭后仍需可读（设计 4.5）。
func GCSClientReady() bool {
	return gcsClient != nil
}

// GCSVideoTransferReady 视频任务转存是否就绪（视频 worker 的启动条件）。
func GCSVideoTransferReady() bool {
	return setting.GCSTransferEnabled && gcsClient != nil
}

// GCSImageTransferReady 生图转存是否就绪（生图写入侧的启动条件）。
func GCSImageTransferReady() bool {
	return setting.GCSImageTransferEnabled && gcsClient != nil
}

// GCSObjectName 按设计的命名规则生成对象名：{prefix}/{task_id}_{index}.{ext}。
// ext 必须来自暂存时定死的 UpstreamAsset.Ext，跨重试稳定，禁止随下载的 Content-Type 漂移。
func GCSObjectName(taskID string, index int, ext string) string {
	ext = strings.TrimPrefix(ext, ".")
	if ext == "" {
		ext = "bin"
	}
	return fmt.Sprintf("%s/%s_%d.%s", setting.GCSResultPrefix, taskID, index, ext)
}

// GCSObjectURL 返回对象的 gs:// 路径（存入 PrivateData.ResultURL 的形式，绝不直接返回给用户）。
func GCSObjectURL(objectName string) string {
	return fmt.Sprintf("gs://%s/%s", setting.GCSResultBucket, objectName)
}

// IsGCSResultURL 判断 URL 是否为 gs:// 对象路径。
func IsGCSResultURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "gs://")
}

// ParseGCSObjectURL 解析 gs://bucket/object 路径，返回 bucket 与对象名。
func ParseGCSObjectURL(gsURL string) (bucket string, objectName string, err error) {
	if !IsGCSResultURL(gsURL) {
		return "", "", fmt.Errorf("not a gs:// url: %s", gsURL)
	}
	rest := strings.TrimPrefix(gsURL, "gs://")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed gs:// url: %s", gsURL)
	}
	return parts[0], parts[1], nil
}

// GCSUploadObject 以 If-GenerationMatch: 0（DoesNotExist）条件写流式上传对象。
// 对象已存在时返回 ErrGCSObjectExists，调用方按"该对象已完成转存"处理（逐对象幂等语义）。
//
// 上传放弃纪律（设计文档 4.3，load-bearing）：「已存在 = 已完成」成立的前提是对象
// 只可能在完整写入后才存在。storage.Writer 在 Close() 时 finalize——任何错误路径
// 必须通过 cancel context 放弃上传、禁止错误后调用 Close()，否则半截数据会被
// finalize 成合法的截断对象，被后续所有重试永久复用，无自愈。因此本函数不使用 defer w.Close()。
func GCSUploadObject(ctx context.Context, objectName string, reader io.Reader, contentType string) error {
	if gcsClient == nil {
		return ErrGCSNotInitialized
	}
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	obj := gcsClient.Bucket(setting.GCSResultBucket).Object(objectName).
		If(storage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(writeCtx)
	if contentType != "" {
		w.ContentType = contentType
	}
	if _, err := io.Copy(w, reader); err != nil {
		// 错误路径：cancel context 放弃上传，绝不调用 Close()（会 finalize 半截对象）
		cancel()
		if isGCSPreconditionFailed(err) {
			return ErrGCSObjectExists
		}
		return fmt.Errorf("gcs upload copy failed for %s: %w", objectName, err)
	}
	if err := w.Close(); err != nil {
		if isGCSPreconditionFailed(err) {
			return ErrGCSObjectExists
		}
		return fmt.Errorf("gcs upload finalize failed for %s: %w", objectName, err)
	}
	return nil
}

// GCSVerifyExistingObject 复用已存在对象前的防御性校验（设计文档 4.3）：
// size 必须与下载侧字节计数一致；expectedCRC32C 非零时一并核对。
// 不一致返回 ErrGCSObjectCorrupted（包装后含详情），调用方禁止复用、按转存失败处理并告警。
func GCSVerifyExistingObject(ctx context.Context, objectName string, expectedSize int64, expectedCRC32C uint32) error {
	if gcsClient == nil {
		return ErrGCSNotInitialized
	}
	attrs, err := gcsClient.Bucket(setting.GCSResultBucket).Object(objectName).Attrs(ctx)
	if err != nil {
		return fmt.Errorf("gcs object attrs failed for %s: %w", objectName, err)
	}
	if expectedSize >= 0 && attrs.Size != expectedSize {
		return fmt.Errorf("%w: object %s size %d != expected %d", ErrGCSObjectCorrupted, objectName, attrs.Size, expectedSize)
	}
	if expectedCRC32C != 0 && attrs.CRC32C != expectedCRC32C {
		return fmt.Errorf("%w: object %s crc32c %d != expected %d", ErrGCSObjectCorrupted, objectName, attrs.CRC32C, expectedCRC32C)
	}
	return nil
}

// GCSObjectAttrs 返回对象属性（size/CRC32C 等），供调用方做存在性与一致性判断。
func GCSObjectAttrs(ctx context.Context, objectName string) (*storage.ObjectAttrs, error) {
	if gcsClient == nil {
		return nil, ErrGCSNotInitialized
	}
	return gcsClient.Bucket(setting.GCSResultBucket).Object(objectName).Attrs(ctx)
}

// SignPolicy 一次签名的策略。CacheTag 为空表示不入签名缓存——一次性对象
//（/v1/images/*、Responses）基本只访问一次，入缓存等于无界内存泄漏（设计 4.4）。
type SignPolicy struct {
	TTL             time.Duration // 期望 TTL；<=0 时回退到 setting.GCSSignedURLTTL
	RetentionAnchor int64         // 保留期锚点（Unix 秒，0 = 不做保留期检查）
	CacheTag        string        // 缓存分区标签，空 = 不缓存
}

// resolveSignTTL 计算最终 TTL：按剩余保留期收口，保证签出的 URL 在 expiresAt 前
// 对象不会被生命周期规则删除、expires_at 不虚标。超保留期返回 ErrGCSResultExpired。
// 纯函数，便于单测（签名本身需要凭证，无法在单测里跑）。
func resolveSignTTL(now time.Time, p SignPolicy) (time.Duration, error) {
	ttl := p.TTL
	if ttl <= 0 {
		ttl = setting.GCSSignedURLTTL
	}
	if p.RetentionAnchor > 0 {
		retentionEnd := time.Unix(p.RetentionAnchor, 0).Add(time.Duration(setting.GCSResultRetentionDays) * 24 * time.Hour)
		ttlCap := retentionEnd.Sub(now) - gcsSignSafetyMargin
		if ttlCap <= 0 {
			return 0, ErrGCSResultExpired
		}
		if ttlCap < ttl {
			ttl = ttlCap
		}
	}
	return ttl, nil
}

// gcsSignCacheKey 缓存 key 必须覆盖 policy：同一对象在不同 TTL / 锚点下
// 是不同的签名结果，只按 objectName 分区会互相污染（设计 4.4）。
func gcsSignCacheKey(objectName string, p SignPolicy, ttl time.Duration) string {
	if p.CacheTag == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%d", objectName, p.CacheTag, ttl, p.RetentionAnchor)
}

// gcsSignCacheStore 写入缓存并维护计数；超过容量上限时先惰性清扫过期条目，
// 清扫后仍超限则跳过本次写入（缓存是优化，不是正确性依赖）。key 为空时直接跳过。
func gcsSignCacheStore(key string, e gcsSignCacheEntry) {
	if key == "" {
		return
	}
	max := int64(setting.GCSSignCacheMaxEntries)
	if max > 0 && gcsSignCacheCount.Load() >= max {
		gcsSignCacheSweep()
		if gcsSignCacheCount.Load() >= max {
			return
		}
	}
	if _, loaded := gcsSignCache.Swap(key, e); !loaded {
		gcsSignCacheCount.Add(1)
	}
}

// gcsSignCacheSweep 惰性清扫：删除缓存年龄超过 GCSSignCacheTTL 或签名已过期的条目。
func gcsSignCacheSweep() {
	now := time.Now()
	gcsSignCache.Range(func(k, v any) bool {
		entry := v.(gcsSignCacheEntry)
		if now.After(entry.signedAt.Add(setting.GCSSignCacheTTL)) || !entry.expiresAt.After(now) {
			gcsSignCacheDelete(k)
		}
		return true
	})
}

// gcsSignCacheDelete 删除缓存条目并维护计数。
func gcsSignCacheDelete(key any) {
	if _, loaded := gcsSignCache.LoadAndDelete(key); loaded {
		gcsSignCacheCount.Add(-1)
	}
}

// GCSSignObjectURL 对对象现签 V4 GET URL（读时现签，不存库——设计文档 4.5）。
//
//   - p.RetentionAnchor 非零时执行保留期检查：超保留期返回 ErrGCSResultExpired，
//     不签必 404 的死链；TTL 按剩余保留期收口。
//   - p.CacheTag 非空时启用签名缓存：key 覆盖 (objectName, tag, ttl, anchor)，
//     命中时额外校验缓存的 expiresAt 未超过当前 policy 允许的上限。
//
// 返回 (signedURL, expiresAt Unix 秒, error)。
func GCSSignObjectURL(objectName string, p SignPolicy) (string, int64, error) {
	if gcsClient == nil {
		return "", 0, ErrGCSNotInitialized
	}
	now := time.Now()
	ttl, err := resolveSignTTL(now, p)
	if err != nil {
		if errors.Is(err, ErrGCSResultExpired) {
			// 契约行为计数（非失败），观测用户命中过期结果的频度
			gcsMetrics.resultExpired.Add(1)
		}
		return "", 0, err
	}

	cacheKey := gcsSignCacheKey(objectName, p, ttl)
	if cacheKey != "" {
		// 签名缓存：命中时直接复用（真实 expiresAt 不变，客户端契约允许命中期内返回相同 URL）
		if v, ok := gcsSignCache.Load(cacheKey); ok {
			entry := v.(gcsSignCacheEntry)
			fresh := now.Before(entry.signedAt.Add(setting.GCSSignCacheTTL))
			usable := entry.expiresAt.After(now.Add(time.Minute))
			withinPolicy := !entry.expiresAt.After(now.Add(ttl))
			if fresh && usable && withinPolicy {
				return entry.signedURL, entry.expiresAt.Unix(), nil
			}
			gcsSignCacheDelete(cacheKey)
		}
	}

	expiresAt := now.Add(ttl)
	signedURL, err := gcsClient.Bucket(setting.GCSResultBucket).SignedURL(objectName, &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  http.MethodGet,
		Expires: expiresAt,
	})
	if err != nil {
		// 签名失败计数（4.8，auth 与 service 区分；SignBlob 路径尤其需要）。
		// 统一在此处计数，覆盖全部读取侧出口（task_result_url/relay_task/video_proxy）。
		gcsMetrics.recordSignFailure(err)
		return "", 0, fmt.Errorf("gcs sign url failed for %s: %w", objectName, err)
	}
	gcsSignCacheStore(cacheKey, gcsSignCacheEntry{
		signedURL: signedURL,
		expiresAt: expiresAt,
		signedAt:  now,
	})
	return signedURL, expiresAt.Unix(), nil
}

// GCSSignResultURL 视频任务结果签名（保留原签名与行为，现有调用点零改动）：
// TTL 取全局 GCSSignedURLTTL，保留期锚点为任务 FinishTime（Unix 秒），走 video 缓存分区。
func GCSSignResultURL(objectName string, finishTime int64) (string, int64, error) {
	return GCSSignObjectURL(objectName, SignPolicy{
		TTL:             setting.GCSSignedURLTTL,
		RetentionAnchor: finishTime,
		CacheTag:        GCSSignCacheTagVideo,
	})
}

// isGCSPreconditionFailed 判断错误是否为 412 Precondition Failed（If-GenerationMatch: 0 命中已存在对象）。
func isGCSPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusPreconditionFailed
	}
	return false
}
