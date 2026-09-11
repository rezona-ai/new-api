package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

//func GetPromptTokens(textRequest dto.GeneralOpenAIRequest, relayMode int) (int, error) {
//	switch relayMode {
//	case constant.RelayModeChatCompletions:
//		return CountTokenMessages(textRequest.Messages, textRequest.Model)
//	case constant.RelayModeCompletions:
//		return CountTokenInput(textRequest.Prompt, textRequest.Model), nil
//	case constant.RelayModeModerations:
//		return CountTokenInput(textRequest.Input, textRequest.Model), nil
//	}
//	return 0, errors.New("unknown relay mode")
//}

func ResponseText2Usage(c *gin.Context, responseText string, modeName string, promptTokens int) *dto.Usage {
	common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	usage := &dto.Usage{}
	usage.PromptTokens = promptTokens
	usage.CompletionTokens = EstimateTokenByModel(modeName, responseText)
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}

func ValidUsage(usage *dto.Usage) bool {
	return usage != nil && (usage.PromptTokens != 0 || usage.CompletionTokens != 0)
}

// HasUpstreamTokenUsage 表示 usage 带有上游返回的非 0 token 字段。
// CompletionTokens==0 不影响结果：单独 output=0 仍可能是有效上游 usage。
func HasUpstreamTokenUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 {
		return true
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		return true
	}
	if usage.PromptTokensDetails.CachedTokens != 0 || usage.PromptTokensDetails.CachedCreationTokens != 0 {
		return true
	}
	if usage.ClaudeCacheCreation5mTokens != 0 || usage.ClaudeCacheCreation1hTokens != 0 {
		return true
	}
	return false
}

// AllowLocalTokenBilling 判定当前请求是否允许「本地估算 token」扣费。
// 有效策略：global.allow_local_token_billing && !channel.disable_local_token_billing
func AllowLocalTokenBilling(info *relaycommon.RelayInfo) bool {
	if info != nil && info.ChannelMeta != nil && info.ChannelSetting.DisableLocalTokenBilling {
		return false
	}
	return operation_setting.GetQuotaSetting().AllowLocalTokenBilling
}

// applyLocalTokenBillingPolicy 在结算层统一拦截本地估算扣费。
// 当请求被标记为本地估算计费且有效策略为关闭时：quota=0，并追加可读 skip 文案。
// 返回 true 表示已跳过本地扣费（调用方应写 admin_info skip 标记并继续记日志）。
func applyLocalTokenBillingPolicy(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, quota *int, extraContent *[]string) bool {
	if ctx == nil || quota == nil {
		return false
	}
	if !common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens) {
		return false
	}
	if AllowLocalTokenBilling(relayInfo) {
		return false
	}
	*quota = 0
	if extraContent != nil {
		*extraContent = append(*extraContent, "本地计费已关闭，无上游 usage，未扣费")
	}
	return true
}

// upstreamOutputTokens 取上游返回的 output token 数（兼容 completion / output 两种字段名）。
func upstreamOutputTokens(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.CompletionTokens != 0 {
		return usage.CompletionTokens
	}
	return usage.OutputTokens
}

// SkipBillOnEmptyResult 判定当前用户是否开启「空结果不计费」。
// 该开关只能由管理员写入 user.setting，用户自助设置接口不会修改它。
func SkipBillOnEmptyResult(info *relaycommon.RelayInfo) bool {
	return info != nil && info.UserSetting.SkipBillOnEmptyResult
}

// applyEmptyResultBillingPolicy 在结算层按用户级「空结果不计费」拦截扣费。
// 命中条件（开关开启且满足任一）：
//   - 本次走本地估算（完全拿不到上游 usage）；
//   - 上游返回了 usage 但 output token 为 0。
//
// 命中时整单 quota=0（input/cache 也一并不扣），并追加可读文案。
// 返回 true 表示已跳过扣费（调用方应写 admin_info 审计标记并继续记日志）。
func applyEmptyResultBillingPolicy(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.Usage, quota *int, extraContent *[]string) bool {
	if ctx == nil || quota == nil {
		return false
	}
	if !SkipBillOnEmptyResult(relayInfo) {
		return false
	}
	localCount := common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens)
	if !localCount && upstreamOutputTokens(usage) != 0 {
		return false
	}
	*quota = 0
	if extraContent != nil {
		if localCount {
			*extraContent = append(*extraContent, "用户已开启空结果不计费，无上游 usage，未扣费")
		} else {
			*extraContent = append(*extraContent, "用户已开启空结果不计费，上游输出为 0，未扣费")
		}
	}
	return true
}

// markEmptyResultBillingSkipped 在消耗日志 other.admin_info 写入用户级 skip 审计字段。
func markEmptyResultBillingSkipped(other map[string]interface{}) {
	if other == nil {
		return
	}
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok || adminInfo == nil {
		adminInfo = make(map[string]interface{})
		other["admin_info"] = adminInfo
	}
	adminInfo["billing_skipped"] = true
	adminInfo["billing_skip_reason"] = "user_skip_bill_on_empty_result"
}

// markLocalTokenBillingSkipped 在消耗日志 other.admin_info 写入 skip 审计字段。
func markLocalTokenBillingSkipped(other map[string]interface{}) {
	if other == nil {
		return
	}
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok || adminInfo == nil {
		adminInfo = make(map[string]interface{})
		other["admin_info"] = adminInfo
	}
	adminInfo["local_count_tokens"] = true
	adminInfo["local_token_billing_skipped"] = true
	adminInfo["local_token_billing_skip_reason"] = "disabled"
}
