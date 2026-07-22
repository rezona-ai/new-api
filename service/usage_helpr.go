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
