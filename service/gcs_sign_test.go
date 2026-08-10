package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSignTTL_NoRetentionAnchor(t *testing.T) {
	now := time.Now()
	ttl, err := resolveSignTTL(now, SignPolicy{TTL: 12 * time.Hour})
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, ttl)
}

// 锚点为 0 时不做保留期检查（旧数据/未知完成时间）。
func TestResolveSignTTL_ZeroAnchorSkipsRetentionCheck(t *testing.T) {
	orig := setting.GCSResultRetentionDays
	setting.GCSResultRetentionDays = 30
	defer func() { setting.GCSResultRetentionDays = orig }()

	ttl, err := resolveSignTTL(time.Now(), SignPolicy{TTL: time.Hour, RetentionAnchor: 0})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, ttl)
}

// TTL 必须被剩余保留期收口，避免签出在有效期内就被生命周期规则删除的 URL。
func TestResolveSignTTL_CappedByRemainingRetention(t *testing.T) {
	orig := setting.GCSResultRetentionDays
	setting.GCSResultRetentionDays = 30
	defer func() { setting.GCSResultRetentionDays = orig }()

	now := time.Now()
	// 锚点在 29 天 22 小时前 → 剩余保留期 2 小时，减去安全余量
	anchor := now.Add(-(30*24*time.Hour - 2*time.Hour)).Unix()
	ttl, err := resolveSignTTL(now, SignPolicy{TTL: 168 * time.Hour, RetentionAnchor: anchor})
	require.NoError(t, err)
	assert.InDelta(t, (2*time.Hour - gcsSignSafetyMargin).Seconds(), ttl.Seconds(), 2)
}

func TestResolveSignTTL_ExpiredReturnsErr(t *testing.T) {
	orig := setting.GCSResultRetentionDays
	setting.GCSResultRetentionDays = 30
	defer func() { setting.GCSResultRetentionDays = orig }()

	anchor := time.Now().Add(-31 * 24 * time.Hour).Unix()
	_, err := resolveSignTTL(time.Now(), SignPolicy{TTL: time.Hour, RetentionAnchor: anchor})
	assert.ErrorIs(t, err, ErrGCSResultExpired)
}

// CacheTag 为空 = 不入缓存。生图用一次性对象名，入缓存就是无界内存泄漏。
func TestSignCache_EmptyTagNotCached(t *testing.T) {
	resetSignCacheForTest()
	gcsSignCacheStore("", gcsSignCacheEntry{})
	assert.Equal(t, int64(0), gcsSignCacheCount.Load())
}

func TestSignCache_StoreAndCount(t *testing.T) {
	resetSignCacheForTest()
	now := time.Now()
	gcsSignCacheStore("a|video|1|0", gcsSignCacheEntry{signedURL: "u1", expiresAt: now.Add(time.Hour), signedAt: now})
	gcsSignCacheStore("b|video|1|0", gcsSignCacheEntry{signedURL: "u2", expiresAt: now.Add(time.Hour), signedAt: now})
	assert.Equal(t, int64(2), gcsSignCacheCount.Load())

	// 同 key 覆盖不应重复计数
	gcsSignCacheStore("a|video|1|0", gcsSignCacheEntry{signedURL: "u1b", expiresAt: now.Add(time.Hour), signedAt: now})
	assert.Equal(t, int64(2), gcsSignCacheCount.Load())
}

// 超过容量上限时先惰性清扫过期条目；清扫后仍超限则跳过写入
// （缓存是优化，不是正确性依赖）。
func TestSignCache_EvictsExpiredWhenOverCapacity(t *testing.T) {
	resetSignCacheForTest()
	origMax, origTTL := setting.GCSSignCacheMaxEntries, setting.GCSSignCacheTTL
	setting.GCSSignCacheMaxEntries = 3
	setting.GCSSignCacheTTL = 10 * time.Minute
	defer func() {
		setting.GCSSignCacheMaxEntries, setting.GCSSignCacheTTL = origMax, origTTL
	}()

	now := time.Now()
	// 3 条已过期条目（signedAt 早于 GCSSignCacheTTL）
	for i := 0; i < 3; i++ {
		gcsSignCacheStore(fmt.Sprintf("stale%d|video|1|0", i), gcsSignCacheEntry{
			signedURL: "old", expiresAt: now.Add(-time.Minute), signedAt: now.Add(-time.Hour),
		})
	}
	require.Equal(t, int64(3), gcsSignCacheCount.Load())

	// 触发清扫后新条目应写入成功
	gcsSignCacheStore("fresh|video|1|0", gcsSignCacheEntry{
		signedURL: "new", expiresAt: now.Add(time.Hour), signedAt: now,
	})
	assert.Equal(t, int64(1), gcsSignCacheCount.Load())
	v, ok := gcsSignCache.Load("fresh|video|1|0")
	require.True(t, ok)
	assert.Equal(t, "new", v.(gcsSignCacheEntry).signedURL)
}

// 全是新鲜条目且已满 → 跳过写入，不得无界增长
func TestSignCache_SkipsWriteWhenFull(t *testing.T) {
	resetSignCacheForTest()
	origMax := setting.GCSSignCacheMaxEntries
	setting.GCSSignCacheMaxEntries = 2
	defer func() { setting.GCSSignCacheMaxEntries = origMax }()

	now := time.Now()
	for i := 0; i < 4; i++ {
		gcsSignCacheStore(fmt.Sprintf("k%d|video|1|0", i), gcsSignCacheEntry{
			signedURL: "u", expiresAt: now.Add(time.Hour), signedAt: now,
		})
	}
	assert.Equal(t, int64(2), gcsSignCacheCount.Load())
}

// 缓存 key 必须覆盖 policy：同一对象在不同 TTL / 锚点下是不同的签名结果
func TestSignCacheKey_CoversPolicy(t *testing.T) {
	assert.Empty(t, gcsSignCacheKey("obj", SignPolicy{CacheTag: ""}, time.Hour), "空 tag 不缓存")

	video := gcsSignCacheKey("obj", SignPolicy{CacheTag: GCSSignCacheTagVideo}, 12*time.Hour)
	image := gcsSignCacheKey("obj", SignPolicy{CacheTag: GCSSignCacheTagImage}, 12*time.Hour)
	assert.NotEqual(t, video, image, "不同分区不得共用 key")

	shortTTL := gcsSignCacheKey("obj", SignPolicy{CacheTag: GCSSignCacheTagVideo}, time.Hour)
	assert.NotEqual(t, video, shortTTL, "不同 TTL 不得共用 key")

	withAnchor := gcsSignCacheKey("obj", SignPolicy{CacheTag: GCSSignCacheTagVideo, RetentionAnchor: 123}, 12*time.Hour)
	assert.NotEqual(t, video, withAnchor, "不同保留期锚点不得共用 key")
}

// resetSignCacheForTest 清空签名缓存与计数，保证用例之间互不干扰。
func resetSignCacheForTest() {
	gcsSignCache.Range(func(k, _ any) bool {
		gcsSignCache.Delete(k)
		return true
	})
	gcsSignCacheCount.Store(0)
}
