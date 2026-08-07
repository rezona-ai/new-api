package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

func GetGroups(c *gin.Context) {
	groupNames := make([]string, 0)
	for groupName := range ratio_setting.GetGroupRatioCopy() {
		groupNames = append(groupNames, groupName)
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    groupNames,
	})
}

// GetUserGroups 返回当前用户在创建密钥时可选的分组。
//
// 分组来源以用户分组在 GroupGroupRatio 里配置的「目标分组」为准：配置了就只列这些
// 目标分组，并带上各自的倍率。用户没有分组、或其分组没有配置任何目标分组时，回退到
// 可用分组列表，且**不返回 ratio 字段**——此时倍率只会回落到扁平 GroupRatio 或 1，
// 对用户没有折扣含义，返回它会让前端展示出并不存在的折扣。
func GetUserGroups(c *gin.Context) {
	usableGroups := make(map[string]map[string]interface{})
	userGroup := ""
	userId := c.GetInt("id")
	userGroup, _ = model.GetUserGroup(userId, false)
	userUsableGroups := service.GetUserUsableGroups(userGroup)

	if targets, ok := ratio_setting.GetGroupGroupRatioTargets(userGroup); ok && len(targets) > 0 {
		for groupName, ratio := range targets {
			if setting.IsGroupHidden(groupName) {
				continue
			}
			desc, ok := userUsableGroups[groupName]
			if !ok {
				desc = setting.GetUsableGroupDescription(groupName)
			}
			group := map[string]interface{}{
				"desc":  desc,
				"ratio": ratio,
			}
			if groupName == "auto" {
				group["ratio"] = "自动"
			}
			usableGroups[groupName] = group
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    usableGroups,
		})
		return
	}

	for groupName := range ratio_setting.GetGroupRatioCopy() {
		if setting.IsGroupHidden(groupName) {
			continue
		}
		// UserUsableGroups contains the groups that the user can use
		if desc, ok := userUsableGroups[groupName]; ok {
			usableGroups[groupName] = map[string]interface{}{
				"desc": desc,
			}
		}
	}
	if _, ok := userUsableGroups["auto"]; ok && !setting.IsGroupHidden("auto") {
		usableGroups["auto"] = map[string]interface{}{
			"ratio": "自动",
			"desc":  setting.GetUsableGroupDescription("auto"),
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    usableGroups,
	})
}
