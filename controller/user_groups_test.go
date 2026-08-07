package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type userGroupsResponse struct {
	Success bool                              `json:"success"`
	Data    map[string]map[string]interface{} `json:"data"`
}

func setupUserGroupsTest(t *testing.T, userGroup string) {
	t.Helper()

	// model 包的列名（commonGroupCol 等）由 InitDB 惰性初始化，不先跑一遍
	// GetUserGroup 会生成 `SELECT  FROM users`。
	initModelListColumnNames(t)

	gin.SetMode(gin.TestMode)
	common.UsingSQLite = true
	common.UsingMySQL = false
	common.UsingPostgreSQL = false
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.User{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "u1", Group: userGroup}).Error)

	// 定价分组 + 可用分组（进程级全局，跑完必须还原，否则污染同包其他测试）
	originalGroupRatio := ratio_setting.GroupRatio2JSONString()
	originalGroupGroupRatio := ratio_setting.GroupGroupRatio2JSONString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalHiddenGroups := setting.HiddenGroupsToString()

	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"premium":0.8}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"默认分组","premium":"高级分组"}`))
	setting.HiddenGroupsFromString("")

	t.Cleanup(func() {
		_ = ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatio)
		_ = ratio_setting.UpdateGroupGroupRatioByJSONString(originalGroupGroupRatio)
		_ = setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups)
		setting.HiddenGroupsFromString(originalHiddenGroups)
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

func callGetUserGroups(t *testing.T) userGroupsResponse {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/self/groups", nil)
	c.Set("id", 1)

	GetUserGroups(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp userGroupsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	return resp
}

// 用户分组配置了目标分组：只列这些目标分组，并带上各自的倍率。
func TestGetUserGroupsListsConfiguredTargetGroups(t *testing.T) {
	setupUserGroupsTest(t, "bigco")
	require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(
		`{"bigco":{"premium":0.3}}`))

	resp := callGetUserGroups(t)

	require.Len(t, resp.Data, 1)
	require.Contains(t, resp.Data, "premium")
	require.Equal(t, 0.3, resp.Data["premium"]["ratio"])
	require.Equal(t, "高级分组", resp.Data["premium"]["desc"])
}

// 目标分组被隐藏时不下发。
func TestGetUserGroupsSkipsHiddenTargetGroups(t *testing.T) {
	setupUserGroupsTest(t, "bigco")
	require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(
		`{"bigco":{"premium":0.3,"default":0.9}}`))
	setting.HiddenGroupsFromString("premium")

	resp := callGetUserGroups(t)

	require.NotContains(t, resp.Data, "premium")
	require.Contains(t, resp.Data, "default")
}

// 用户分组没有配置目标分组：回退到可用分组，且不下发 ratio——此时倍率只会回落到
// 扁平 GroupRatio 或 1，对用户没有折扣含义。
func TestGetUserGroupsOmitsRatioWithoutTargetGroups(t *testing.T) {
	setupUserGroupsTest(t, "default")

	resp := callGetUserGroups(t)

	require.Contains(t, resp.Data, "default")
	require.Contains(t, resp.Data, "premium")
	for group, info := range resp.Data {
		require.NotContains(t, info, "ratio", "group %s should not carry a ratio", group)
	}
}

// 用户分组在 GroupGroupRatio 里存在但目标分组为空对象，同样按未配置处理。
func TestGetUserGroupsOmitsRatioWhenTargetGroupsEmpty(t *testing.T) {
	setupUserGroupsTest(t, "bigco")
	require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(`{"bigco":{}}`))

	resp := callGetUserGroups(t)

	require.NotEmpty(t, resp.Data)
	for group, info := range resp.Data {
		require.NotContains(t, info, "ratio", "group %s should not carry a ratio", group)
	}
}
