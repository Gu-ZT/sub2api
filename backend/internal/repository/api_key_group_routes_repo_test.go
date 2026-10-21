package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 多分组路由「关不掉」链路的最后一环：仓储层必须能真正删掉关联表记录。
// 服务层把显式 [] 透传下来后，这里验证数据库里那一行确实消失，而不只是内存态变化。

func newGroupRoutesProbeKey(t *testing.T, repo *apiKeyRepository) int64 {
	t.Helper()
	ctx := context.Background()

	user := mustCreateAPIKeyRepoUser(t, ctx, repo.client, "group-routes-probe@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-group-routes-probe",
		Name:   "Group Routes Probe",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	return key.ID
}

func countGroupRoutes(t *testing.T, repo *apiKeyRepository, keyID int64) int {
	t.Helper()
	ctx := context.Background()
	rows, err := repo.sql.QueryContext(ctx, `SELECT COUNT(*) FROM api_key_group_routes WHERE key_id = $1`, keyID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var n int
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&n))
	return n
}

// 写完再用空切片清空：行必须被删除（这正是「关闭多分组」的效果）。
func TestSetGroupRoutes_EmptySliceDeletesRows(t *testing.T) {
	repo, _ := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	keyID := newGroupRoutesProbeKey(t, repo)

	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{
		{GroupID: 28, Priority: 1},
		{GroupID: 30, Priority: 2},
	}))
	require.Equal(t, 2, countGroupRoutes(t, repo, keyID), "写入后应有两行路由")

	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{}))
	require.Equal(t, 0, countGroupRoutes(t, repo, keyID), "空切片必须清空关联表")
}

// 清空后再加载：GroupRoutes 必须为空，前端据此把开关回填为关闭。
func TestSetGroupRoutes_ClearedRoutesReloadAsEmpty(t *testing.T) {
	repo, _ := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	keyID := newGroupRoutesProbeKey(t, repo)

	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{{GroupID: 28, Priority: 1}}))

	loaded, err := repo.GetByID(ctx, keyID)
	require.NoError(t, err)
	require.Len(t, loaded.GroupRoutes, 1, "清空前应能读回路由")

	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{}))

	reloaded, err := repo.GetByID(ctx, keyID)
	require.NoError(t, err)
	require.Empty(t, reloaded.GroupRoutes, "清空后重新加载必须为空，否则弹窗开关会再次回填为开启")
}

// 替换语义：整体覆盖，而不是追加。
func TestSetGroupRoutes_ReplacesNotAppends(t *testing.T) {
	repo, _ := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	keyID := newGroupRoutesProbeKey(t, repo)

	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{
		{GroupID: 28, Priority: 1},
		{GroupID: 30, Priority: 2},
	}))
	require.NoError(t, repo.SetGroupRoutes(ctx, keyID, []service.APIKeyGroupRoute{{GroupID: 31, Priority: 1}}))

	require.Equal(t, 1, countGroupRoutes(t, repo, keyID), "替换后只应剩一行")

	loaded, err := repo.GetByID(ctx, keyID)
	require.NoError(t, err)
	require.Len(t, loaded.GroupRoutes, 1)
	require.EqualValues(t, 31, loaded.GroupRoutes[0].GroupID)
}
