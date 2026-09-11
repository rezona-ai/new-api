package operation_setting

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/setting/config"
)

type QuotaSetting struct {
	EnableFreeModelPreConsume bool `json:"enable_free_model_pre_consume"` // 是否对免费模型启用预消耗
	AllowLocalTokenBilling    bool `json:"allow_local_token_billing"`     // 无上游 usage 时是否允许本地估算扣费
	// SkipBillOnEmptyResultUserIds 空结果不计费用户白名单：名单内用户在「拿不到上游 usage」
	// 或「上游 output token = 0」时整单不扣费（input/cache 也不扣）。
	SkipBillOnEmptyResultUserIds []int `json:"skip_bill_on_empty_result_user_ids"`
}

// 默认配置
var quotaSetting = QuotaSetting{
	EnableFreeModelPreConsume: true,
	AllowLocalTokenBilling:    true, // 默认兼容现网：允许本地估算扣费
	// 默认空名单：不改变任何用户的现有计费行为
	SkipBillOnEmptyResultUserIds: []int{},
}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("quota_setting", &quotaSetting)
}

func GetQuotaSetting() *QuotaSetting {
	return &quotaSetting
}

// IsSkipBillOnEmptyResultUser 判断用户是否在「空结果不计费」白名单内。
// 名单规模是人工维护的个位数到几十，线性扫描足够，且避免维护一份需要跟着
// 配置热更新同步失效的 map。
func IsSkipBillOnEmptyResultUser(userId int) bool {
	if userId == 0 {
		return false
	}
	for _, id := range quotaSetting.SkipBillOnEmptyResultUserIds {
		if id == userId {
			return true
		}
	}
	return false
}

// SkipBillOnEmptyResultUserIdsKey 是该白名单在 options 表中的 key。
const SkipBillOnEmptyResultUserIdsKey = "quota_setting.skip_bill_on_empty_result_user_ids"

// NormalizeSkipBillOnEmptyResultUserIds 把管理员填写的名单规范化为去重升序的 JSON 数组串。
// 同时接受 JSON 数组（[1,2,3]）和逗号/空白分隔（1, 2 3）两种写法，空值表示清空名单。
// 返回错误而不是静默忽略：配置层对非法 JSON 是 continue 掉的，静默失效比报错更危险。
func NormalizeSkipBillOnEmptyResultUserIds(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return "[]", nil
	}

	var ids []int
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
			return "", fmt.Errorf("用户 ID 名单格式错误，应为 [1,2,3] 或 1,2,3：%w", err)
		}
	} else {
		for _, part := range strings.FieldsFunc(trimmed, func(r rune) bool {
			return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r' || r == '，'
		}) {
			id, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return "", fmt.Errorf("用户 ID 名单含非法条目 %q，应为正整数", part)
			}
			ids = append(ids, id)
		}
	}

	seen := make(map[int]struct{}, len(ids))
	unique := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return "", fmt.Errorf("用户 ID 必须为正整数，收到 %d", id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Ints(unique)

	encoded, err := json.Marshal(unique)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
