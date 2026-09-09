package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// gatewayBreakerRepoStub 记录熔断/恢复相关的写库调用。
type gatewayBreakerRepoStub struct {
	stubOpenAIAccountRepo
	rateLimitedUntil      []time.Time
	clearRateLimitCalls   int
	tempUnschedUntil      []time.Time
	tempUnschedReasons    []string
	clearTempUnschedCalls int
}

func (r *gatewayBreakerRepoStub) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimitedUntil = append(r.rateLimitedUntil, resetAt)
	return nil
}

func (r *gatewayBreakerRepoStub) ClearRateLimit(_ context.Context, _ int64) error {
	r.clearRateLimitCalls++
	return nil
}

func (r *gatewayBreakerRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempUnschedUntil = append(r.tempUnschedUntil, until)
	r.tempUnschedReasons = append(r.tempUnschedReasons, reason)
	return nil
}

func (r *gatewayBreakerRepoStub) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.clearTempUnschedCalls++
	return nil
}

func commandCodeTestAccount() *Account {
	return &Account{
		ID:          9002,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.commandcode.ai/provider/v1", "api_key": "k"},
	}
}

func timePtrFuture(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

func TestGatewayWindowExhaustedReset(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("no usage", func(t *testing.T) {
		require.Nil(t, gatewayWindowExhaustedReset(commandCodeTestAccount(), nil, now))
	})

	t.Run("below threshold", func(t *testing.T) {
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 99.9, ResetsAt: timePtrFuture(time.Hour)}}
		require.Nil(t, gatewayWindowExhaustedReset(commandCodeTestAccount(), usage, now))
	})

	t.Run("expired reset ignored", func(t *testing.T) {
		past := now.Add(-time.Minute)
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 100, ResetsAt: &past}}
		require.Nil(t, gatewayWindowExhaustedReset(commandCodeTestAccount(), usage, now))
	})

	t.Run("picks latest exhausted window", func(t *testing.T) {
		usage := &UsageInfo{
			FiveHour:  &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(2 * time.Hour)},
			SevenDay:  &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(3 * 24 * time.Hour)},
			ThirtyDay: &UsageProgress{Utilization: 40, ResetsAt: timePtrFuture(10 * 24 * time.Hour)},
		}
		got := gatewayWindowExhaustedReset(commandCodeTestAccount(), usage, now)
		require.NotNil(t, got)
		require.WithinDuration(t, now.Add(3*24*time.Hour), *got, time.Second)
	})

	t.Run("commandcode monthly capped to short cooldown", func(t *testing.T) {
		usage := &UsageInfo{
			ThirtyDay: &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(20 * 24 * time.Hour)},
		}
		got := gatewayWindowExhaustedReset(commandCodeTestAccount(), usage, now)
		require.NotNil(t, got)
		require.WithinDuration(t, now.Add(gatewayCreditsLowCooldown), *got, time.Second)
	})

	t.Run("commandcode monthly uncapped when period end sooner", func(t *testing.T) {
		usage := &UsageInfo{
			ThirtyDay: &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(5 * time.Minute)},
		}
		got := gatewayWindowExhaustedReset(commandCodeTestAccount(), usage, now)
		require.NotNil(t, got)
		require.WithinDuration(t, now.Add(5*time.Minute), *got, time.Second)
	})
}

func TestReconcileGatewayUsageCircuit(t *testing.T) {
	t.Parallel()

	t.Run("blocks on exhausted window", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := commandCodeTestAccount()
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(2 * time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Len(t, repo.rateLimitedUntil, 1)
		require.WithinDuration(t, time.Now().Add(2*time.Hour), repo.rateLimitedUntil[0], 5*time.Second)
		require.NotNil(t, account.RateLimitResetAt)
	})

	t.Run("skips write when existing cooldown already covers target", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := commandCodeTestAccount()
		account.RateLimitResetAt = timePtrFuture(3 * time.Hour)
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(2 * time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Empty(t, repo.rateLimitedUntil, "既有冷却不早于目标时不应重复写库")
	})

	t.Run("clears rate limit on recovery", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := commandCodeTestAccount()
		account.RateLimitResetAt = timePtrFuture(2 * time.Hour)
		account.RateLimitedAt = timePtrFuture(-time.Hour)
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 12, ResetsAt: timePtrFuture(2 * time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Equal(t, 1, repo.clearRateLimitCalls)
		require.Nil(t, account.RateLimitResetAt)
		require.Nil(t, account.RateLimitedAt)
	})

	t.Run("clears own credits temp unsched on recovery", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := commandCodeTestAccount()
		account.TempUnschedulableUntil = timePtrFuture(5 * time.Minute)
		account.TempUnschedulableReason = gatewayCreditsLowReasonPrefix + ": insufficient credits"
		usage := &UsageInfo{ThirtyDay: &UsageProgress{Utilization: 20, ResetsAt: timePtrFuture(24 * time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Equal(t, 1, repo.clearTempUnschedCalls)
		require.Nil(t, account.TempUnschedulableUntil)
		require.Empty(t, account.TempUnschedulableReason)
	})

	t.Run("leaves foreign temp unsched reasons untouched", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := commandCodeTestAccount()
		account.TempUnschedulableUntil = timePtrFuture(5 * time.Minute)
		account.TempUnschedulableReason = "maintenance"
		usage := &UsageInfo{ThirtyDay: &UsageProgress{Utilization: 20, ResetsAt: timePtrFuture(24 * time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Zero(t, repo.clearTempUnschedCalls)
	})

	t.Run("ignores non-gateway accounts", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := &AccountUsageService{accountRepo: repo}
		account := &Account{ID: 9003, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 100, ResetsAt: timePtrFuture(time.Hour)}}

		svc.reconcileGatewayUsageCircuit(context.Background(), account, usage)

		require.Empty(t, repo.rateLimitedUntil)
		require.Zero(t, repo.clearRateLimitCalls)
	})
}

func TestIsGatewayInsufficientCreditsError(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"insufficient_credits"}}`)

	for _, status := range []int{http.StatusBadRequest, http.StatusPaymentRequired, http.StatusTooManyRequests} {
		require.True(t, isGatewayInsufficientCreditsError(status, body), "status %d should match", status)
	}
	require.False(t, isGatewayInsufficientCreditsError(http.StatusUnauthorized, body))
	require.False(t, isGatewayInsufficientCreditsError(http.StatusInternalServerError, body))
	require.False(t, isGatewayInsufficientCreditsError(http.StatusBadRequest, nil))
	require.False(t, isGatewayInsufficientCreditsError(http.StatusBadRequest, []byte(`{"error":{"message":"invalid model"}}`)))
}

func TestHandleUpstreamError_CommandCodeInsufficientCredits(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"insufficient_credits"}}`)

	t.Run("commandcode account temp unschedulable", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := commandCodeTestAccount()

		shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusBadRequest, http.Header{}, body)

		require.False(t, shouldDisable, "credits 耗尽可恢复，不应永久禁用")
		require.Len(t, repo.tempUnschedUntil, 1)
		require.WithinDuration(t, time.Now().Add(gatewayCreditsLowCooldown), repo.tempUnschedUntil[0], 5*time.Second)
		require.Len(t, repo.tempUnschedReasons, 1)
		require.Contains(t, repo.tempUnschedReasons[0], gatewayCreditsLowReasonPrefix)
	})

	t.Run("429 variant also handled", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

		shouldDisable := svc.HandleUpstreamError(context.Background(), commandCodeTestAccount(), http.StatusTooManyRequests, http.Header{}, body)

		require.False(t, shouldDisable)
		require.Len(t, repo.tempUnschedUntil, 1)
	})

	t.Run("non-gateway account unaffected", func(t *testing.T) {
		repo := &gatewayBreakerRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := &Account{ID: 9004, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusBadRequest, http.Header{}, body)

		require.False(t, shouldDisable)
		require.Empty(t, repo.tempUnschedUntil, "非网关账号的 400 不临时停调")
	})
}

// OpenCode Go 原生平台（PlatformOpenCodeGo）不走 openai 平台的 429 解析分支，
// 其 GoUsageLimitError 由 handle429 的网关响应体解析分支覆盖。
func TestHandle429_GatewayBodyParsedOnOpenCodeGoPlatform(t *testing.T) {
	t.Parallel()
	repo := &gatewayBreakerRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       9005,
		Platform: PlatformOpenCodeGo,
		Type:     AccountTypeAPIKey,
	}
	body := []byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"5-hour usage limit reached. Resets in 4hr 59min."}}`)

	before := time.Now()
	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)
	after := time.Now()

	require.False(t, shouldDisable)
	require.Len(t, repo.rateLimitedUntil, 1, "opencode_go 平台账号 429 应解析 GoUsageLimitError 响应体")
	expected := 4*time.Hour + 59*time.Minute
	require.False(t, repo.rateLimitedUntil[0].Before(before.Add(expected-time.Second)))
	require.False(t, repo.rateLimitedUntil[0].After(after.Add(expected+time.Second)))
}
