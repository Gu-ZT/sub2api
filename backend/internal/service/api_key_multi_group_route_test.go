package service

import (
	"errors"
	"testing"
)

func TestSortAPIKeyGroupRoutes_OrderByPriorityThenGroupID(t *testing.T) {
	routes := []APIKeyGroupRoute{
		{GroupID: 30, Priority: 2},
		{GroupID: 10, Priority: 1},
		{GroupID: 20, Priority: 1},
	}
	sorted := SortAPIKeyGroupRoutes(routes)
	want := []int64{10, 20, 30}
	for i, w := range want {
		if sorted[i].GroupID != w {
			t.Fatalf("sorted[%d].GroupID = %d, want %d", i, sorted[i].GroupID, w)
		}
	}
}

func TestFirstAPIKeyGroupRoute_HighestPriorityFirstGroup(t *testing.T) {
	routes := []APIKeyGroupRoute{
		{GroupID: 30, Priority: 2},
		{GroupID: 20, Priority: 1},
		{GroupID: 10, Priority: 1},
	}
	first, ok := FirstAPIKeyGroupRoute(routes)
	if !ok || first.GroupID != 10 {
		t.Fatalf("FirstAPIKeyGroupRoute = (%d, %v), want (10, true)", first.GroupID, ok)
	}

	if _, ok := FirstAPIKeyGroupRoute(nil); ok {
		t.Fatal("expected ok=false for empty routes")
	}
}

func TestSelectGroupForSession_HighPriorityPreferred(t *testing.T) {
	allAvailable := func(int64) bool { return true }
	routes := []APIKeyGroupRoute{
		{GroupID: 100, Priority: 1},
		{GroupID: 200, Priority: 2},
	}
	got := SelectGroupForSession(routes, "sess-1", allAvailable)
	if got != 100 {
		t.Fatalf("expected highest-priority group 100, got %d", got)
	}
}

func TestSelectGroupForSession_FallbackToLowerPriorityWhenUnavailable(t *testing.T) {
	available := func(gid int64) bool {
		return gid == 200 // 高优先级分组不可用
	}
	routes := []APIKeyGroupRoute{
		{GroupID: 100, Priority: 1},
		{GroupID: 200, Priority: 2},
	}
	got := SelectGroupForSession(routes, "sess-1", available)
	if got != 200 {
		t.Fatalf("expected fallback to 200, got %d", got)
	}
}

func TestSelectGroupForSession_AllUnavailableReturnsZero(t *testing.T) {
	available := func(int64) bool { return false }
	routes := []APIKeyGroupRoute{
		{GroupID: 100, Priority: 1},
		{GroupID: 200, Priority: 2},
	}
	if got := SelectGroupForSession(routes, "sess-1", available); got != 0 {
		t.Fatalf("expected 0 when all groups unavailable, got %d", got)
	}
}

func TestSelectGroupForSession_EmptyRoutesReturnsZero(t *testing.T) {
	if got := SelectGroupForSession(nil, "sess-1", nil); got != 0 {
		t.Fatalf("expected 0 for empty routes, got %d", got)
	}
}

func TestSelectGroupForSession_SamePriorityUsesSessionSticky(t *testing.T) {
	allAvailable := func(int64) bool { return true }
	routes := []APIKeyGroupRoute{
		{GroupID: 10, Priority: 1},
		{GroupID: 20, Priority: 1},
		{GroupID: 30, Priority: 1},
	}
	sid := "session-abc"
	first := SelectGroupForSession(routes, sid, allAvailable)
	if first == 0 {
		t.Fatal("expected a group selection")
	}
	// 同一会话重复调用必须稳定在同一个分组。
	for i := 0; i < 50; i++ {
		if got := SelectGroupForSession(routes, sid, allAvailable); got != first {
			t.Fatalf("session stickiness broken: first=%d got=%d", first, got)
		}
	}
}

func TestSelectGroupForSession_SamePriorityDistributesAcrossSessions(t *testing.T) {
	allAvailable := func(int64) bool { return true }
	routes := []APIKeyGroupRoute{
		{GroupID: 10, Priority: 1},
		{GroupID: 20, Priority: 1},
		{GroupID: 30, Priority: 1},
	}
	seen := map[int64]struct{}{}
	for i := 0; i < 100; i++ {
		sid := "session-" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		got := SelectGroupForSession(routes, sid, allAvailable)
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("expected sessions to distribute across groups, only saw %d group(s): %v", len(seen), seen)
	}
}

func TestSelectGroupForSession_NoSessionIDPicksLowestGroupID(t *testing.T) {
	allAvailable := func(int64) bool { return true }
	routes := []APIKeyGroupRoute{
		{GroupID: 30, Priority: 1},
		{GroupID: 10, Priority: 1},
	}
	if got := SelectGroupForSession(routes, "", allAvailable); got != 10 {
		t.Fatalf("expected 10 without session id, got %d", got)
	}
}

func TestSelectWithGroupRouteFallback_TriesNextGroupAfterUnavailable(t *testing.T) {
	attempts := make([]int64, 0, 3)
	result, err := selectWithGroupRouteFallback([]int64{10, 20, 30}, 20, func(groupID int64) (int64, error) {
		attempts = append(attempts, groupID)
		if groupID == 30 {
			return groupID, nil
		}
		return 0, ErrNoAvailableAccounts
	})
	if err != nil {
		t.Fatalf("selectWithGroupRouteFallback returned error: %v", err)
	}
	if result != 30 {
		t.Fatalf("result = %d, want 30", result)
	}
	want := []int64{20, 10, 30}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempts = %v, want %v", attempts, want)
		}
	}
}

func TestSelectWithGroupRouteFallback_TriesNextGroupAfterCompactUnavailable(t *testing.T) {
	attempts := 0
	result, err := selectWithGroupRouteFallback([]int64{10, 20}, 10, func(groupID int64) (int64, error) {
		attempts++
		if groupID == 20 {
			return groupID, nil
		}
		return 0, ErrNoAvailableCompactAccounts
	})
	if err != nil || result != 20 {
		t.Fatalf("result = %d, error = %v, want group 20", result, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSelectWithGroupRouteFallback_StopsOnNonAvailabilityError(t *testing.T) {
	terminalErr := errors.New("group lookup failed")
	attempts := 0
	_, err := selectWithGroupRouteFallback([]int64{10, 20}, 10, func(groupID int64) (int64, error) {
		attempts++
		return 0, terminalErr
	})
	if !errors.Is(err, terminalErr) {
		t.Fatalf("error = %v, want %v", err, terminalErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestFirstAPIKeyGroupRoute_UnsortedInput(t *testing.T) {
	routes := []APIKeyGroupRoute{
		{GroupID: 99, Priority: 3},
		{GroupID: 55, Priority: 1},
		{GroupID: 42, Priority: 1},
		{GroupID: 88, Priority: 2},
	}
	first, ok := FirstAPIKeyGroupRoute(routes)
	if !ok || first.GroupID != 42 {
		t.Fatalf("expected 42 (priority 1, smallest group id), got (%d, %v)", first.GroupID, ok)
	}
}
