package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHasUpstreamTokenUsage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		usage *dto.Usage
		want  bool
	}{
		{name: "nil", usage: nil, want: false},
		{name: "all zeros", usage: &dto.Usage{}, want: false},
		{name: "prompt only", usage: &dto.Usage{PromptTokens: 10}, want: true},
		{name: "completion only", usage: &dto.Usage{CompletionTokens: 4}, want: true},
		{name: "input tokens only", usage: &dto.Usage{InputTokens: 8}, want: true},
		{name: "output tokens only", usage: &dto.Usage{OutputTokens: 3}, want: true},
		{
			name: "cache read only",
			usage: &dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 12},
			},
			want: true,
		},
		{
			name: "cache creation only",
			usage: &dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: 7},
			},
			want: true,
		},
		{
			name:  "claude 5m cache only",
			usage: &dto.Usage{ClaudeCacheCreation5mTokens: 5},
			want:  true,
		},
		{
			name:  "claude 1h cache only",
			usage: &dto.Usage{ClaudeCacheCreation1hTokens: 2},
			want:  true,
		},
		{
			name: "input nonzero output zero",
			usage: &dto.Usage{
				PromptTokens:     100,
				CompletionTokens: 0,
				TotalTokens:      100,
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, HasUpstreamTokenUsage(tc.usage))
		})
	}
}

func TestValidUsageUnchanged(t *testing.T) {
	t.Parallel()

	require.False(t, ValidUsage(nil))
	require.False(t, ValidUsage(&dto.Usage{}))
	require.True(t, ValidUsage(&dto.Usage{PromptTokens: 1}))
	require.True(t, ValidUsage(&dto.Usage{CompletionTokens: 1}))
	require.False(t, ValidUsage(&dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 9},
	}))
}

func TestUpstreamOutputTokens(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, upstreamOutputTokens(nil))
	require.Equal(t, 0, upstreamOutputTokens(&dto.Usage{}))
	require.Equal(t, 7, upstreamOutputTokens(&dto.Usage{CompletionTokens: 7}))
	require.Equal(t, 5, upstreamOutputTokens(&dto.Usage{OutputTokens: 5}))
	// completion 优先于 output，两者同时存在时不重复计数
	require.Equal(t, 7, upstreamOutputTokens(&dto.Usage{CompletionTokens: 7, OutputTokens: 5}))
}

func TestApplyEmptyResultBillingPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	qs := operation_setting.GetQuotaSetting()
	orig := qs.SkipBillOnEmptyResultUserIds
	t.Cleanup(func() { qs.SkipBillOnEmptyResultUserIds = orig })
	qs.SkipBillOnEmptyResultUserIds = []int{7, 42}

	newCtx := func(localCount bool) *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		if localCount {
			common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		}
		return c
	}

	cases := []struct {
		name        string
		userId      int
		localCount  bool
		usage       *dto.Usage
		wantSkipped bool
		wantQuota   int
		wantExtra   string
	}{
		{
			name:        "名单外用户 output=0 照常计费",
			userId:      99,
			usage:       &dto.Usage{PromptTokens: 1000},
			wantSkipped: false,
			wantQuota:   1000,
		},
		{
			name:        "名单外用户 本地估算 照常计费",
			userId:      99,
			localCount:  true,
			usage:       &dto.Usage{PromptTokens: 1000, CompletionTokens: 20},
			wantSkipped: false,
			wantQuota:   1000,
		},
		{
			name:        "userId=0 不命中",
			userId:      0,
			usage:       &dto.Usage{PromptTokens: 1000},
			wantSkipped: false,
			wantQuota:   1000,
		},
		{
			name:        "名单内用户 有 input 但 output=0 整单不扣",
			userId:      7,
			usage:       &dto.Usage{PromptTokens: 1000, TotalTokens: 1000},
			wantSkipped: true,
			wantQuota:   0,
			wantExtra:   "该用户已配置空结果不计费，上游输出为 0，未扣费",
		},
		{
			name:        "名单内用户 只有 cache 且 output=0 整单不扣",
			userId:      42,
			usage:       &dto.Usage{PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 500}},
			wantSkipped: true,
			wantQuota:   0,
			wantExtra:   "该用户已配置空结果不计费，上游输出为 0，未扣费",
		},
		{
			name:        "名单内用户 本地估算 整单不扣",
			userId:      7,
			localCount:  true,
			usage:       &dto.Usage{PromptTokens: 999, CompletionTokens: 42},
			wantSkipped: true,
			wantQuota:   0,
			wantExtra:   "该用户已配置空结果不计费，无上游 usage，未扣费",
		},
		{
			name:        "名单内用户 output>0 照常计费",
			userId:      7,
			usage:       &dto.Usage{PromptTokens: 1000, CompletionTokens: 20},
			wantSkipped: false,
			wantQuota:   1000,
		},
		{
			name:        "名单内用户 responses 语义 output>0 照常计费",
			userId:      42,
			usage:       &dto.Usage{InputTokens: 1000, OutputTokens: 3},
			wantSkipped: false,
			wantQuota:   1000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtx(tc.localCount)
			info := &relaycommon.RelayInfo{UserId: tc.userId}
			quota := 1000
			var extra []string
			skipped := applyEmptyResultBillingPolicy(c, info, tc.usage, &quota, &extra)
			require.Equal(t, tc.wantSkipped, skipped)
			require.Equal(t, tc.wantQuota, quota)
			if tc.wantExtra == "" {
				require.Empty(t, extra)
			} else {
				require.Equal(t, []string{tc.wantExtra}, extra)
			}
		})
	}
}

func TestApplyEmptyResultBillingPolicy_NilGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	qs := operation_setting.GetQuotaSetting()
	orig := qs.SkipBillOnEmptyResultUserIds
	t.Cleanup(func() { qs.SkipBillOnEmptyResultUserIds = orig })
	qs.SkipBillOnEmptyResultUserIds = []int{7}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	quota := 100

	require.False(t, applyEmptyResultBillingPolicy(nil, nil, nil, &quota, nil))
	require.False(t, applyEmptyResultBillingPolicy(c, nil, nil, nil, nil))
	// relayInfo 为 nil 时视为未命中，不得改动 quota
	require.False(t, applyEmptyResultBillingPolicy(c, nil, nil, &quota, nil))
	require.Equal(t, 100, quota)
}

func TestMarkEmptyResultBillingSkipped(t *testing.T) {
	t.Parallel()

	other := map[string]interface{}{}
	markEmptyResultBillingSkipped(other)
	adminInfo := other["admin_info"].(map[string]interface{})
	require.Equal(t, true, adminInfo["billing_skipped"])
	require.Equal(t, "skip_bill_on_empty_result_user", adminInfo["billing_skip_reason"])

	// 已有 admin_info 时就地补字段，不覆盖原有内容
	other2 := map[string]interface{}{"admin_info": map[string]interface{}{"admin_id": 7}}
	markEmptyResultBillingSkipped(other2)
	adminInfo2 := other2["admin_info"].(map[string]interface{})
	require.Equal(t, 7, adminInfo2["admin_id"])
	require.Equal(t, true, adminInfo2["billing_skipped"])

	require.NotPanics(t, func() { markEmptyResultBillingSkipped(nil) })
}
