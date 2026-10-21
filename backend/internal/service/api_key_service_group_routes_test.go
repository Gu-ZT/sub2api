package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 多分组路由「关不掉」链路的服务层环节：Update 必须把「显式空数组」当作清空指令
// 透传给 SetGroupRoutes（仓储层据此 DELETE 关联表），而「nil」必须完全跳过该调用
// （否则任何不涉及多分组的普通编辑都会清空用户的路由配置）。
// 这里用一个只实现所需方法的窄 stub，避免依赖带 //go:build unit 的共享 stub。

type groupRoutesProbeRepo struct {
	APIKeyRepository // 未实现的方法保持 nil，调用即 panic，确保只走预期路径

	key *APIKey

	updateCalls      int
	setRoutesCalls   int
	setRoutesPayload []APIKeyGroupRoute
	setRoutesWasNil  bool
}

func (r *groupRoutesProbeRepo) GetByID(context.Context, int64) (*APIKey, error) {
	clone := *r.key
	return &clone, nil
}

func (r *groupRoutesProbeRepo) Update(context.Context, *APIKey, APIKeyUpdateFields) error {
	r.updateCalls++
	return nil
}

func (r *groupRoutesProbeRepo) SetGroupRoutes(_ context.Context, _ int64, routes []APIKeyGroupRoute) error {
	r.setRoutesCalls++
	r.setRoutesWasNil = routes == nil
	r.setRoutesPayload = append([]APIKeyGroupRoute(nil), routes...)
	return nil
}

func newGroupRoutesProbeService(key *APIKey) (*APIKeyService, *groupRoutesProbeRepo) {
	repo := &groupRoutesProbeRepo{key: key}
	return &APIKeyService{apiKeyRepo: repo}, repo
}

func activeProbeKey() *APIKey {
	return &APIKey{
		ID:          9,
		UserID:      1,
		Key:         "sk-probe",
		Name:        "Usage",
		Status:      StatusActive,
		GroupID:     int64PtrHelper(28),
		GroupRoutes: []APIKeyGroupRoute{{GroupID: 28, Priority: 1}},
		IPWhitelist: nil,
		IPBlacklist: nil,
		Quota:       0,
		RateLimit5h: 0,
		RateLimit1d: 0,
		RateLimit7d: 0,
	}
}

func int64PtrHelper(v int64) *int64 { return &v }

// 关闭多分组：前端提交 group_routes: [] → 服务层必须调用 SetGroupRoutes 且传入空切片。
func TestAPIKeyUpdate_ExplicitEmptyGroupRoutesClearsRoutes(t *testing.T) {
	svc, repo := newGroupRoutesProbeService(activeProbeKey())

	empty := []APIKeyGroupRoute{}
	updated, err := svc.Update(context.Background(), 9, 1, UpdateAPIKeyRequest{GroupRoutes: &empty})
	require.NoError(t, err)

	require.Equal(t, 1, repo.setRoutesCalls, "显式空数组必须触发 SetGroupRoutes 清空关联表")
	require.False(t, repo.setRoutesWasNil, "传入的必须是空切片而非 nil")
	require.Empty(t, repo.setRoutesPayload)
	require.Empty(t, updated.GroupRoutes)
}

// 普通编辑（字段缺失 → nil）不得触碰路由配置，否则会误清用户的多分组设置。
func TestAPIKeyUpdate_NilGroupRoutesLeavesRoutesUntouched(t *testing.T) {
	svc, repo := newGroupRoutesProbeService(activeProbeKey())

	name := "renamed"
	_, err := svc.Update(context.Background(), 9, 1, UpdateAPIKeyRequest{Name: &name})
	require.NoError(t, err)

	require.Equal(t, 0, repo.setRoutesCalls, "nil 表示不修改，绝不能调用 SetGroupRoutes")
	require.Equal(t, 1, repo.updateCalls)
}

// 多分组保存：按请求内容替换。
func TestAPIKeyUpdate_GroupRoutesReplaced(t *testing.T) {
	svc, repo := newGroupRoutesProbeService(activeProbeKey())

	routes := []APIKeyGroupRoute{{GroupID: 28, Priority: 1}, {GroupID: 30, Priority: 2}}
	_, err := svc.Update(context.Background(), 9, 1, UpdateAPIKeyRequest{GroupRoutes: &routes})
	require.NoError(t, err)

	require.Equal(t, 1, repo.setRoutesCalls)
	require.Equal(t, routes, repo.setRoutesPayload)
}
