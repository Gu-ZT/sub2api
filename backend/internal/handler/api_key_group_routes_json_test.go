package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 多分组路由「关不掉」的根因在 wire 契约上：
//   - 字段缺失（JSON 里没有 group_routes）  → GroupRoutes == nil        → 服务层视为「不修改」
//   - 显式空数组 []                          → GroupRoutes == &[]{}       → 服务层视为「清空」
//
// 前端曾只在开关打开时赋值，导致关闭时字段缺失、后端跳过 SetGroupRoutes，
// 关联表旧路由残留，重新打开弹窗又把开关回填为开启。这些用例把该契约钉死在
// 反序列化边界上，避免前端再次「省掉」空数组而静默退化。

func decodeUpdateRequest(t *testing.T, body string) UpdateAPIKeyRequest {
	t.Helper()
	var req UpdateAPIKeyRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	return req
}

func TestUpdateAPIKeyRequest_GroupRoutesNilMeansNoChange(t *testing.T) {
	// 前端只提交单分组字段（历史行为）：group_routes 缺失。
	req := decodeUpdateRequest(t, `{"name":"Usage","group_id":28}`)

	require.Nil(t, req.GroupRoutes, "缺失 group_routes 必须解出 nil，服务层据此跳过关联表写入")
}

func TestUpdateAPIKeyRequest_ExplicitEmptyArrayClearsRoutes(t *testing.T) {
	// 前端关闭多分组时必须提交空数组。
	req := decodeUpdateRequest(t, `{"name":"Usage","group_id":28,"group_routes":[]}`)

	require.NotNil(t, req.GroupRoutes, "显式 [] 必须解出非 nil 指针，服务层据此清空关联表")
	require.Empty(t, *req.GroupRoutes)
}

func TestUpdateAPIKeyRequest_ParsesRoutesForMultiGroup(t *testing.T) {
	req := decodeUpdateRequest(t, `{"group_id":28,"group_routes":[{"group_id":30,"priority":2},{"group_id":28,"priority":1}]}`)

	require.NotNil(t, req.GroupRoutes)
	require.Equal(t, []service.APIKeyGroupRoute{
		{GroupID: 30, Priority: 2},
		{GroupID: 28, Priority: 1},
	}, *req.GroupRoutes)
}

func TestUpdateAPIKeyRequest_JSONNullMeansNoChange(t *testing.T) {
	// 显式 null 也按「不修改」处理，与缺失等价。
	req := decodeUpdateRequest(t, `{"group_id":28,"group_routes":null}`)

	require.Nil(t, req.GroupRoutes)
}
