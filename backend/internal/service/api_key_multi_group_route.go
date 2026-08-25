package service

// API Key 多分组路由（Multi-Group Routing）。
//
// 需求：API Key 可绑定多个分组并设置优先级。请求路由规则：
//  1. 优先级高的分组优先使用（数字小的 priority 优先级高）；
//  2. 高优先级分组不可用（分组非激活 / 组内无可用账号）时，降级到下一优先级分组；
//  3. 同一优先级内存在多个分组时，按会话 ID 决定分组，且相同会话 ID 尽量稳定
//     落在同一分组（会话粘性）；
//  4. 密钥列表展示时只显示优先级最高分组中的第一个分组。
//
// 本文件只包含与存储无关的纯选择逻辑，便于独立单测；存储与认证接入
// 见 api_key_service.go / api_key_auth_cache_impl.go。

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"
)

// APIKeyGroupRoute 表示 API Key 的一条分组路由。
type APIKeyGroupRoute struct {
	GroupID  int64 `json:"group_id"` // 分组 ID
	Priority int   `json:"priority"` // 优先级：数字越小优先级越高（1 最高）
}

// SortAPIKeyGroupRoutes 按优先级升序（高优先级在前）稳定排序；同优先级按 GroupID 升序。
func SortAPIKeyGroupRoutes(routes []APIKeyGroupRoute) []APIKeyGroupRoute {
	sorted := make([]APIKeyGroupRoute, len(routes))
	copy(sorted, routes)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].GroupID < sorted[j].GroupID
	})
	return sorted
}

// FirstAPIKeyGroupRoute 返回优先级最高的分组路由（用于密钥列表展示）。
// 空路由返回 ok=false。
func FirstAPIKeyGroupRoute(routes []APIKeyGroupRoute) (APIKeyGroupRoute, bool) {
	if len(routes) == 0 {
		return APIKeyGroupRoute{}, false
	}
	sorted := SortAPIKeyGroupRoutes(routes)
	// 同优先级中取 GroupID 最小的"第一个"分组。
	first := sorted[0]
	for _, r := range sorted {
		if r.Priority == first.Priority && r.GroupID < first.GroupID {
			first = r
		}
	}
	return first, true
}

// groupAvailability 报告某分组是否可用（active 且有可用账号）。
// 由调用方注入真实实现，便于纯逻辑单测。
type groupAvailability func(groupID int64) bool

// SelectGroupForSession 在多分组路由中为当前会话选择应使用的分组。
//
// 参数：
//   - routes：该 API Key 的分组路由（未排序亦可，内部会排序）。
//   - sessionID：客户端会话标识（x-session-id），可为空。
//   - available：分组可用性判定。nil 时视为所有分组可用。
//
// 规则：
//   - 按优先级从高到低遍历；若某优先级内存在可用分组，则在该优先级内选择，不再降级。
//   - 同一优先级内：若存在会话 ID，按 hash(sessionID+groupID) 稳定选择——同一会话
//     每次都落在同一分组（在分组数量不变时）；无会话 ID 时按 GroupID 升序取第一个可用。
//   - 所有分组都不可用时返回 0（表示无可用分组，调用方走无可用账号错误路径）。
func SelectGroupForSession(routes []APIKeyGroupRoute, sessionID string, available groupAvailability) int64 {
	if len(routes) == 0 {
		return 0
	}
	sorted := SortAPIKeyGroupRoutes(routes)
	if available == nil {
		available = func(int64) bool { return true }
	}

	for i := 0; i < len(sorted); {
		// 收集同一优先级的分组。
		priority := sorted[i].Priority
		tier := make([]APIKeyGroupRoute, 0, 4)
		for i < len(sorted) && sorted[i].Priority == priority {
			tier = append(tier, sorted[i])
			i++
		}
		if selected := selectAvailableInTier(tier, sessionID, available); selected != 0 {
			return selected
		}
		// 该优先级内没有可用分组 → 降级到下一优先级。
	}
	return 0
}

// selectAvailableInTier 在同一优先级分组内按会话粘性选择可用分组。
func selectAvailableInTier(tier []APIKeyGroupRoute, sessionID string, available groupAvailability) int64 {
	// 先筛出可用分组。
	usable := make([]APIKeyGroupRoute, 0, len(tier))
	for _, r := range tier {
		if available(r.GroupID) {
			usable = append(usable, r)
		}
	}
	if len(usable) == 0 {
		return 0
	}
	if sessionID == "" || len(usable) == 1 {
		// 无会话 ID 或仅一个可用分组：取 GroupID 最小者，行为稳定。
		sort.SliceStable(usable, func(i, j int) bool { return usable[i].GroupID < usable[j].GroupID })
		return usable[0].GroupID
	}
	// 会话粘性：hash(sessionID+groupID) 决定选择，同一会话稳定在同一分组。
	best := usable[0]
	bestScore := sessionGroupScore(sessionID, best.GroupID)
	for _, r := range usable[1:] {
		score := sessionGroupScore(sessionID, r.GroupID)
		if score > bestScore {
			best, bestScore = r, score
		}
	}
	return best.GroupID
}

// sessionGroupScore 计算会话-分组稳定得分（64 位无符号，值越大越优先）。
// 使用 SHA-256 取前 8 字节，同一 (sessionID, groupID) 恒定。
func sessionGroupScore(sessionID string, groupID int64) uint64 {
	h := sha256.New()
	_, _ = h.Write([]byte(sessionID))
	var gidBuf [8]byte
	binary.BigEndian.PutUint64(gidBuf[:], uint64(groupID))
	_, _ = h.Write(gidBuf[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// routeCandidateIDs 返回按优先级升序（高优先级在前）的有序候选分组 ID 序列，
// 用于网关在首选分组无可用账号时降级遍历。同优先级内按 GroupID 升序保证确定性。
func routeCandidateIDs(routes []APIKeyGroupRoute) []int64 {
	sorted := SortAPIKeyGroupRoutes(routes)
	ids := make([]int64, 0, len(sorted))
	seen := make(map[int64]struct{}, len(sorted))
	for _, r := range sorted {
		if _, ok := seen[r.GroupID]; ok {
			continue
		}
		seen[r.GroupID] = struct{}{}
		ids = append(ids, r.GroupID)
	}
	return ids
}

// ResolveAPIKeyRoutingGroup 为 API Key 的多分组路由解析本次请求应使用的分组。
// 返回：
//   - groupID：选中的分组 ID（0 表示解析失败/无可用分组）
//   - group：选中的分组（尽力加载完整对象，可能为 nil）
//   - candidates：按优先级升序的有序候选分组 ID（供网关降级遍历）
//
// 规则：高优先级分组优先；高优先级分组不可用（删除/停用）时降级到下一优先级；
// 同一优先级内按会话 ID 稳定选择（会话粘性）。
// 当 API Key 未配置多分组路由（GroupRoutes 为空）时，直接返回原单分组。
func (s *APIKeyService) ResolveAPIKeyRoutingGroup(ctx context.Context, apiKey *APIKey, sessionID string) (int64, *Group, []int64, error) {
	if apiKey == nil || len(apiKey.GroupRoutes) == 0 {
		if apiKey == nil {
			return 0, nil, nil, nil
		}
		var candidates []int64
		if apiKey.GroupID != nil && *apiKey.GroupID > 0 {
			candidates = []int64{*apiKey.GroupID}
		}
		return derefGroupID(apiKey.GroupID), apiKey.Group, candidates, nil
	}

	availability := func(groupID int64) bool {
		g, err := s.groupRepo.GetByIDLite(ctx, groupID)
		if err != nil || g == nil {
			return false
		}
		if strings.EqualFold(g.Status, "deleted") {
			return false
		}
		return g.IsActive()
	}

	selected := SelectGroupForSession(apiKey.GroupRoutes, sessionID, availability)
	candidates := routeCandidateIDs(apiKey.GroupRoutes)
	if selected == 0 {
		// 所有路由分组均不可用：退回原单分组（若存在）以保持现有行为。
		if apiKey.GroupID != nil && *apiKey.GroupID > 0 {
			return *apiKey.GroupID, apiKey.Group, candidates, nil
		}
		return 0, nil, candidates, nil
	}

	var group *Group
	if g, err := s.groupRepo.GetByID(ctx, selected); err == nil {
		group = g
	}
	return selected, group, candidates, nil
}