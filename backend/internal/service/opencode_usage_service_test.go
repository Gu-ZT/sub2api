package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsOpenCodeGoBaseURL(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   bool
	}{
		{"exact https", "https://opencode.ai/zen/go", true},
		{"trailing slash", "https://opencode.ai/zen/go/", true},
		{"with v1 suffix", "https://opencode.ai/zen/go/v1", true},
		{"with deep path", "https://opencode.ai/zen/go/v1/anything", true},
		{"www host", "https://www.opencode.ai/zen/go", true},
		{"http scheme", "http://opencode.ai/zen/go", true},
		{"uppercase host and path", "HTTPS://OPENCODE.AI/ZEN/GO", true},
		{"empty", "", false},
		{"other host", "https://example.com/zen/go", false},
		{"subdomain confuse", "https://opencode.ai.evil.com/zen/go", false},
		{"path confuse", "https://evil.com/opencode.ai/zen/go", false},
		{"similar prefix", "https://opencode.ai/zen/government", false},
		{"query only", "https://opencode.ai/zen/go?x=1", true},
		{"no scheme", "opencode.ai/zen/go", false},
		{"garbage", "not a url", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isOpenCodeGoBaseURL(c.raw)
			if got != c.want {
				t.Fatalf("isOpenCodeGoBaseURL(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestAccountIsOpenCodeGo(t *testing.T) {
	acc := &Account{Credentials: map[string]any{"base_url": "https://opencode.ai/zen/go"}}
	if !acc.IsOpenCodeGo() {
		t.Fatal("expected account with opencode base_url to be OpenCode Go")
	}
	other := &Account{Credentials: map[string]any{"base_url": "https://api.openai.com/v1"}}
	if other.IsOpenCodeGo() {
		t.Fatal("expected OpenAI base_url account not to be OpenCode Go")
	}
	empty := &Account{}
	if empty.IsOpenCodeGo() {
		t.Fatal("expected account without base_url not to be OpenCode Go")
	}
}

func TestBuildOpenCodeProgress(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	// 完整窗口：percent + resetsAt 都解析
	full := buildOpenCodeProgress(openCodeUsageWindow{Percent: 42.5, ResetsAt: reset.Format(time.RFC3339)})
	if full == nil {
		t.Fatal("expected non-nil progress")
	}
	if full.Utilization != 42.5 {
		t.Fatalf("utilization = %v, want 42.5", full.Utilization)
	}
	if full.ResetsAt == nil || !full.ResetsAt.Equal(reset) {
		t.Fatalf("resetsAt = %v, want %v", full.ResetsAt, reset)
	}
	if full.RemainingSeconds <= 0 {
		t.Fatalf("expected positive remaining seconds, got %d", full.RemainingSeconds)
	}

	// 过期窗口：resetAt 在过去 → utilization 归零
	expired := buildOpenCodeProgress(openCodeUsageWindow{Percent: 80, ResetsAt: time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)})
	if expired.Utilization != 0 {
		t.Fatalf("expected expired window utilization to be 0, got %v", expired.Utilization)
	}

	// 无可解析 resetsAt：仅 percent，仍可渲染（倒计时留空）
	noReset := buildOpenCodeProgress(openCodeUsageWindow{Percent: 10})
	if noReset == nil || noReset.Utilization != 10 || noReset.ResetsAt != nil {
		t.Fatalf("unexpected progress from percent-only window: %#v", noReset)
	}

	// 全部为零/空：返回 nil（前端不渲染该行）
	emptyWin := buildOpenCodeProgress(openCodeUsageWindow{})
	if emptyWin != nil {
		t.Fatalf("expected nil progress for empty window, got %#v", emptyWin)
	}
}

func TestGetOpenCodeGoUsage_FetchParseCache(t *testing.T) {
	reset5h := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	reset30d := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)

	var hitCount int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {
				"rolling":  {"percent": 42.0, "resetsAt": "` + reset5h.Format(time.RFC3339) + `"},
				"weekly":   {"percent": 10.0, "resetsAt": "` + reset7d.Format(time.RFC3339) + `"},
				"monthly":  {"percent": 5.0,  "resetsAt": "` + reset30d.Format(time.RFC3339) + `"}
			}
		}`))
	}))
	defer srv.Close()

	original := openCodeUsageURL
	openCodeUsageURL = srv.URL
	defer func() { openCodeUsageURL = original }()

	acc := &Account{
		ID:       91001,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":   "sk-test-123",
			"base_url":  "https://opencode.ai/zen/go",
		},
	}
	svc := &AccountUsageService{cache: NewUsageCache()}

	usage, err := svc.getOpenCodeGoUsage(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("getOpenCodeGoUsage() error = %v", err)
	}
	if gotAuth != "Bearer sk-test-123" {
		t.Fatalf("expected Authorization Bearer sk-test-123, got %q", gotAuth)
	}
	if usage.FiveHour == nil || usage.FiveHour.Utilization != 42.0 {
		t.Fatalf("five_hour = %#v", usage.FiveHour)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 10.0 {
		t.Fatalf("seven_day = %#v", usage.SevenDay)
	}
	if usage.ThirtyDay == nil || usage.ThirtyDay.Utilization != 5.0 {
		t.Fatalf("thirty_day = %#v", usage.ThirtyDay)
	}

	// 第二次调用（force=false）应命中缓存，不再请求上游
	_, err = svc.getOpenCodeGoUsage(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("second getOpenCodeGoUsage() error = %v", err)
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("expected 1 upstream hit due to caching, got %d", got)
	}

	// force=true 应绕过缓存重新请求
	_, err = svc.getOpenCodeGoUsage(context.Background(), acc, true)
	if err != nil {
		t.Fatalf("forced getOpenCodeGoUsage() error = %v", err)
	}
	if got := atomic.LoadInt32(&hitCount); got != 2 {
		t.Fatalf("expected 2 upstream hits after force, got %d", got)
	}
}

func TestGetOpenCodeGoUsage_AuthErrorNegativeCache(t *testing.T) {
	var hitCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	original := openCodeUsageURL
	openCodeUsageURL = srv.URL
	defer func() { openCodeUsageURL = original }()

	acc := &Account{
		ID:       91002,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-bad",
			"base_url": "https://opencode.ai/zen/go",
		},
	}
	svc := &AccountUsageService{cache: NewUsageCache()}

	if _, err := svc.getOpenCodeGoUsage(context.Background(), acc, false); err == nil {
		t.Fatal("expected error for 401 response")
	}
	// 负缓存命中：立即的第二次调用不应再打上游（未超过 1 分钟错误 TTL）
	if _, err := svc.getOpenCodeGoUsage(context.Background(), acc, false); err == nil {
		t.Fatal("expected cached error on second call")
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("expected 1 upstream hit (negative cache), got %d", got)
	}
}

func TestGetOpenCodeGoUsage_MissingAPIKey(t *testing.T) {
	acc := &Account{
		ID:       91003,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go",
		},
	}
	svc := &AccountUsageService{cache: NewUsageCache()}
	if _, err := svc.getOpenCodeGoUsage(context.Background(), acc, false); err == nil || !strings.Contains(err.Error(), "no api_key") {
		t.Fatalf("expected no api_key error, got %v", err)
	}
}
