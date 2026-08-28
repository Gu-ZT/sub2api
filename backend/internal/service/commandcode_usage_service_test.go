package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsCommandCodeBaseURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"exact https", "https://api.commandcode.ai", true},
		{"trailing slash", "https://api.commandcode.ai/", true},
		{"provider path", "https://api.commandcode.ai/provider", true},
		{"with v1 suffix", "https://api.commandcode.ai/provider/v1", true},
		{"with deep path", "https://api.commandcode.ai/provider/v1/anything", true},
		{"http scheme", "http://api.commandcode.ai/provider/v1", true},
		{"uppercase host and path", "HTTPS://API.COMMANDCODE.AI/PROVIDER/V1", true},
		{"empty", "", false},
		{"other host", "https://example.com/provider/v1", false},
		{"subdomain confuse", "https://api.commandcode.ai.evil.com/provider/v1", false},
		{"path confuse", "https://evil.com/api.commandcode.ai/provider/v1", false},
		{"similar prefix", "https://api.commandcode.ai/providers", false},
		{"query only", "https://api.commandcode.ai/provider/v1?x=1", true},
		{"no scheme", "api.commandcode.ai/provider/v1", false},
		{"garbage", "not a url", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isCommandCodeBaseURL(c.raw)
			if got != c.want {
				t.Fatalf("isCommandCodeBaseURL(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestAccountIsCommandCode(t *testing.T) {
	acc := &Account{Credentials: map[string]any{"base_url": "https://api.commandcode.ai/provider/v1"}}
	if !acc.IsCommandCode() {
		t.Fatal("expected account with commandcode base_url to be CommandCode")
	}
	other := &Account{Credentials: map[string]any{"base_url": "https://api.openai.com/v1"}}
	if other.IsCommandCode() {
		t.Fatal("expected OpenAI base_url account not to be CommandCode")
	}
	empty := &Account{}
	if empty.IsCommandCode() {
		t.Fatal("expected account without base_url not to be CommandCode")
	}
}

func TestBuildCommandCodeProgress(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	// 完整窗口：used/cap + resetAt（毫秒时间戳）都解析
	full := buildCommandCodeProgress(&commandCodeWindow{Used: 42.5, Cap: 100, ResetAt: strconv.FormatInt(reset.UnixMilli(), 10)})
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

	// RFC3339 字符串也兼容
	rfc := buildCommandCodeProgress(&commandCodeWindow{Used: 10, Cap: 100, ResetAt: reset.Format(time.RFC3339)})
	if rfc == nil || rfc.Utilization != 10 || rfc.ResetsAt == nil {
		t.Fatalf("unexpected progress from RFC3339 resetAt: %#v", rfc)
	}

	// 过期窗口：resetAt 在过去 → utilization 归零
	expired := buildCommandCodeProgress(&commandCodeWindow{Used: 80, Cap: 100, ResetAt: strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10)})
	if expired.Utilization != 0 {
		t.Fatalf("expected expired window utilization to be 0, got %v", expired.Utilization)
	}

	// 无 cap：仅 used 无法计算百分比，但保留 resetAt 仍可渲染
	noCap := buildCommandCodeProgress(&commandCodeWindow{Used: 10, ResetAt: strconv.FormatInt(reset.UnixMilli(), 10)})
	if noCap == nil || noCap.Utilization != 0 || noCap.ResetsAt == nil {
		t.Fatalf("unexpected progress from no-cap window: %#v", noCap)
	}

	// 空窗口：返回 nil（前端不渲染该行）
	emptyWin := buildCommandCodeProgress(&commandCodeWindow{})
	if emptyWin != nil {
		t.Fatalf("expected nil progress for empty window, got %#v", emptyWin)
	}
}

func TestParseCommandCodeResetAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	// 毫秒时间戳
	ms := now.Add(2 * time.Hour).UnixMilli()
	got, err := parseCommandCodeResetAt(strconv.FormatInt(ms, 10))
	if err != nil {
		t.Fatalf("parseCommandCodeResetAt(ms) error = %v", err)
	}
	if got.UnixMilli() != ms {
		t.Fatalf("parseCommandCodeResetAt(ms) = %v, want %d", got.UnixMilli(), ms)
	}

	// 秒级时间戳兼容
	sec := now.Add(time.Hour).Unix()
	got, err = parseCommandCodeResetAt(strconv.FormatInt(sec, 10))
	if err != nil {
		t.Fatalf("parseCommandCodeResetAt(sec) error = %v", err)
	}
	if got.Unix() != sec {
		t.Fatalf("parseCommandCodeResetAt(sec) = %v, want %d", got.Unix(), sec)
	}

	// RFC3339 兼容
	rfc := now.Add(time.Hour).Format(time.RFC3339)
	got, err = parseCommandCodeResetAt(rfc)
	if err != nil {
		t.Fatalf("parseCommandCodeResetAt(rfc) error = %v", err)
	}
	if !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("parseCommandCodeResetAt(rfc) = %v", got)
	}

	// 非法输入
	if _, err := parseCommandCodeResetAt("garbage"); err == nil {
		t.Fatal("expected error for garbage resetAt")
	}
	if _, err := parseCommandCodeResetAt(""); err == nil {
		t.Fatal("expected error for empty resetAt")
	}
	if _, err := parseCommandCodeResetAt("0"); err == nil {
		t.Fatal("expected error for zero resetAt")
	}
}

func TestGetCommandCodeUsage_FetchParseCache(t *testing.T) {
	reset5h := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)

	var hitCount int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"credits": {"monthlyCredits": 12.34},
			"windowLimits": {
				"fiveHour": {"used": 42.0, "cap": 100.0, "resetAt": "` + strconv.FormatInt(reset5h.UnixMilli(), 10) + `"},
				"weekly":   {"used": 10.0, "cap": 100.0, "resetAt": "` + strconv.FormatInt(reset7d.UnixMilli(), 10) + `"}
			}
		}`))
	}))
	defer srv.Close()

	original := commandCodeUsageURL
	commandCodeUsageURL = srv.URL
	defer func() { commandCodeUsageURL = original }()

	acc := &Account{
		ID:       92001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test-123",
			"base_url": "https://api.commandcode.ai/provider/v1",
		},
	}
	svc := &AccountUsageService{
		cache:                         NewUsageCache(),
		allowCommandCodePrivateHosts: true,
	}

	usage, err := svc.getCommandCodeUsage(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("getCommandCodeUsage() error = %v", err)
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
	// 月度：70 - 12.34 = 57.66 → 82.37%
	if usage.ThirtyDay == nil {
		t.Fatal("expected thirty_day derived from credits")
	}
	wantMonthly := (commandCodeMonthlyCap - 12.34) / commandCodeMonthlyCap * 100
	if usage.ThirtyDay.Utilization < wantMonthly-0.01 || usage.ThirtyDay.Utilization > wantMonthly+0.01 {
		t.Fatalf("thirty_day utilization = %v, want ~%v", usage.ThirtyDay.Utilization, wantMonthly)
	}

	// 第二次调用（force=false）应命中缓存，不再请求上游
	_, err = svc.getCommandCodeUsage(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("second getCommandCodeUsage() error = %v", err)
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("expected 1 upstream hit due to caching, got %d", got)
	}

	// force=true 应绕过缓存重新请求
	_, err = svc.getCommandCodeUsage(context.Background(), acc, true)
	if err != nil {
		t.Fatalf("forced getCommandCodeUsage() error = %v", err)
	}
	if got := atomic.LoadInt32(&hitCount); got != 2 {
		t.Fatalf("expected 2 upstream hits after force, got %d", got)
	}
}

func TestGetCommandCodeUsage_AuthErrorNegativeCache(t *testing.T) {
	var hitCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	original := commandCodeUsageURL
	commandCodeUsageURL = srv.URL
	defer func() { commandCodeUsageURL = original }()

	acc := &Account{
		ID:       92002,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-bad",
			"base_url": "https://api.commandcode.ai/provider/v1",
		},
	}
	svc := &AccountUsageService{
		cache:                         NewUsageCache(),
		allowCommandCodePrivateHosts: true,
	}

	if _, err := svc.getCommandCodeUsage(context.Background(), acc, false); err == nil {
		t.Fatal("expected error for 401 response")
	}
	// 负缓存命中：立即的第二次调用不应再打上游（未超过 1 分钟错误 TTL）
	if _, err := svc.getCommandCodeUsage(context.Background(), acc, false); err == nil {
		t.Fatal("expected cached error on second call")
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("expected 1 upstream hit (negative cache), got %d", got)
	}
}

func TestGetCommandCodeUsage_MissingAPIKey(t *testing.T) {
	acc := &Account{
		ID:       92003,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://api.commandcode.ai/provider/v1",
		},
	}
	svc := &AccountUsageService{cache: NewUsageCache()}
	if _, err := svc.getCommandCodeUsage(context.Background(), acc, false); err == nil || !strings.Contains(err.Error(), "no api_key") {
		t.Fatalf("expected no api_key error, got %v", err)
	}
}
