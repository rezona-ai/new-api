package operation_setting

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/require"
)

func configToMapForTest(v interface{}) (map[string]string, error) {
	return config.ConfigToMap(v)
}

func updateConfigFromMapForTest(v interface{}, m map[string]string) error {
	return config.UpdateConfigFromMap(v, m)
}

func TestNormalizeSkipBillOnEmptyResultUserIds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "空串清空名单", raw: "", want: "[]"},
		{name: "空白清空名单", raw: "   ", want: "[]"},
		{name: "空数组", raw: "[]", want: "[]"},
		{name: "null 清空名单", raw: "null", want: "[]"},
		{name: "JSON 数组", raw: "[3,1,2]", want: "[1,2,3]"},
		{name: "逗号分隔", raw: "3, 1 ,2", want: "[1,2,3]"},
		{name: "空格分隔", raw: "3 1 2", want: "[1,2,3]"},
		{name: "中文逗号", raw: "3，1，2", want: "[1,2,3]"},
		{name: "换行分隔", raw: "3\n1\n2", want: "[1,2,3]"},
		{name: "去重", raw: "7,7,42,7", want: "[7,42]"},
		{name: "非法 JSON", raw: "[1,", wantErr: true},
		{name: "JSON 里混字符串", raw: `["a"]`, wantErr: true},
		{name: "逗号分隔含非数字", raw: "1,abc", wantErr: true},
		{name: "零不合法", raw: "0", wantErr: true},
		{name: "负数不合法", raw: "[-1]", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeSkipBillOnEmptyResultUserIds(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestIsSkipBillOnEmptyResultUser(t *testing.T) {
	qs := GetQuotaSetting()
	orig := qs.SkipBillOnEmptyResultUserIds
	t.Cleanup(func() { qs.SkipBillOnEmptyResultUserIds = orig })

	qs.SkipBillOnEmptyResultUserIds = nil
	require.False(t, IsSkipBillOnEmptyResultUser(7))

	qs.SkipBillOnEmptyResultUserIds = []int{7, 42}
	require.True(t, IsSkipBillOnEmptyResultUser(7))
	require.True(t, IsSkipBillOnEmptyResultUser(42))
	require.False(t, IsSkipBillOnEmptyResultUser(8))
	// userId=0 表示未识别用户，永远不命中
	require.False(t, IsSkipBillOnEmptyResultUser(0))
}

// 配置层对 []int 走 JSON 反序列化；这里锁住 option 存取的往返行为。
func TestQuotaSettingSliceRoundTrip(t *testing.T) {
	qs := GetQuotaSetting()
	orig := qs.SkipBillOnEmptyResultUserIds
	t.Cleanup(func() { qs.SkipBillOnEmptyResultUserIds = orig })

	qs.SkipBillOnEmptyResultUserIds = []int{1, 2, 3}
	m, err := configToMapForTest(qs)
	require.NoError(t, err)
	require.Equal(t, "[1,2,3]", m["skip_bill_on_empty_result_user_ids"])

	qs.SkipBillOnEmptyResultUserIds = nil
	require.NoError(t, updateConfigFromMapForTest(qs, map[string]string{
		"skip_bill_on_empty_result_user_ids": "[4,5]",
	}))
	require.Equal(t, []int{4, 5}, qs.SkipBillOnEmptyResultUserIds)
}
