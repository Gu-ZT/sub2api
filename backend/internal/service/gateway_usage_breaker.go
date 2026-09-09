package service

// CommandCode 网关账号的用量熔断与恢复。
//
// CommandCode 的滚动窗口用量由管理端轮询驱动（AccountUsageService.getCommandCodeUsage），
// 本文件把轮询结果回写到调度状态，并处理 CommandCode 的 credits 耗尽错误：
//
//   - 熔断：任一窗口利用率达到 100% 且重置时间在未来 → SetRateLimited 到最晚的
//     耗尽窗口重置点（多窗口同时耗尽时，较早窗口恢复后请求仍会撞较晚窗口的 429）。
//     CommandCode 的 30d 窗口由 credits 余额推导（充值即可恢复，不必等到周期末），
//     因此其熔断时长收敛到 gatewayCreditsLowCooldown，由请求错误路径/轮询共同续期。
//   - 恢复：轮询发现所有窗口利用率回落到 100% 以下 → 提前 ClearRateLimit；
//     我们写入的 credits 耗尽临时停调（reason 前缀匹配）也一并 ClearTempUnschedulable。
//     时间驱动的自然到期（IsRateLimited / TempUnschedulableUntil 过期）作为兜底，
//     不依赖管理端页面是否打开。
//   - 反应式：CommandCode 上游返回 "insufficient credits"（400/402/429）时
//     立即临时停调，语义对齐国产供应商余额不足（ratelimit_cn_providers.go）。
//
// OpenCode Go 已是原生平台（PlatformOpenCodeGo），其用量由上游
// OpenCodeGoUsageService 后台周期刷新；其 429 精确冷却由
// ratelimit_service.handle429 的网关响应体解析分支覆盖。

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// gatewayWindowExhaustedPercent 窗口利用率达到该值视为耗尽。
	gatewayWindowExhaustedPercent = 100

	// gatewayCreditsLowReasonPrefix 是网关 credits 耗尽临时停调 reason 的稳定前缀。
	// 轮询恢复路径据此识别「是我们停调的」并安全清除——不会误清其他子系统
	// （自定义规则 / 401 / 健康熔断）写入的临时停调。
	gatewayCreditsLowReasonPrefix = "gateway_credits_low"

	// gatewayCreditsLowCooldown 是 credits 耗尽临时停调的兜底时长。充值恢复后
	// 由轮询提前解除；页面未打开时到期自然恢复，下一个请求若仍耗尽会重新停调。
	gatewayCreditsLowCooldown = 10 * time.Minute

	// gatewayRateLimitWriteTolerance 避免轮询期间对同一重置点重复写库。
	gatewayRateLimitWriteTolerance = 2 * time.Minute
)

// gatewayWindowExhaustedReset 返回耗尽窗口中最晚的未来重置时间；无耗尽窗口返回 nil。
func gatewayWindowExhaustedReset(account *Account, usage *UsageInfo, now time.Time) *time.Time {
	if usage == nil {
		return nil
	}
	windows := []struct {
		progress *UsageProgress
		monthly  bool
	}{
		{usage.FiveHour, false},
		{usage.SevenDay, false},
		{usage.ThirtyDay, true},
	}
	var latest *time.Time
	for _, w := range windows {
		p := w.progress
		if p == nil || p.ResetsAt == nil {
			continue
		}
		if p.Utilization < gatewayWindowExhaustedPercent || !p.ResetsAt.After(now) {
			continue
		}
		resetAt := *p.ResetsAt
		// CommandCode 30d 窗口由余额推导：充值即恢复，收敛到短冷却，
		// 避免把账号冻结到计费周期末（可能长达数周）。
		if w.monthly && account != nil && account.IsCommandCode() {
			if capped := now.Add(gatewayCreditsLowCooldown); capped.Before(resetAt) {
				resetAt = capped
			}
		}
		if latest == nil || resetAt.After(*latest) {
			latest = &resetAt
		}
	}
	return latest
}

// reconcileGatewayUsageCircuit 在一次成功的 CommandCode 用量查询后回写调度状态：
// 窗口耗尽 → 熔断到重置点；窗口恢复 → 提前解除我们负责的限流/临时停调。
// 仅在管理端轮询（含批量查询）驱动下运行。
func (s *AccountUsageService) reconcileGatewayUsageCircuit(ctx context.Context, account *Account, usage *UsageInfo) {
	if s == nil || s.accountRepo == nil || account == nil || usage == nil {
		return
	}
	if !account.IsCommandCode() {
		return
	}
	now := time.Now()

	if resetAt := gatewayWindowExhaustedReset(account, usage, now); resetAt != nil {
		// 已有不早于目标的冷却：跳过写入，避免轮询期间重复写库/缩短既有冷却。
		if account.RateLimitResetAt != nil && !account.RateLimitResetAt.Before(resetAt.Add(-gatewayRateLimitWriteTolerance)) {
			return
		}
		if err := s.accountRepo.SetRateLimited(ctx, account.ID, *resetAt); err != nil {
			slog.Warn("gateway_usage_breaker_set_rate_limited_failed", "account_id", account.ID, "error", err)
			return
		}
		cloned := *resetAt
		account.RateLimitResetAt = &cloned
		rateLimitedAt := now
		account.RateLimitedAt = &rateLimitedAt
		slog.Info("gateway_usage_window_exhausted",
			"account_id", account.ID,
			"platform", account.Platform,
			"reset_at", resetAt.UTC(),
		)
		return
	}

	// 恢复路径：所有窗口均回落到 100% 以下。
	if account.IsRateLimited() {
		if err := s.accountRepo.ClearRateLimit(ctx, account.ID); err != nil {
			slog.Warn("gateway_usage_breaker_clear_rate_limit_failed", "account_id", account.ID, "error", err)
		} else {
			account.RateLimitResetAt = nil
			account.RateLimitedAt = nil
			slog.Info("gateway_usage_window_recovered", "account_id", account.ID, "platform", account.Platform)
		}
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) &&
		strings.HasPrefix(account.TempUnschedulableReason, gatewayCreditsLowReasonPrefix) {
		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			slog.Warn("gateway_usage_breaker_clear_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		} else {
			account.TempUnschedulableUntil = nil
			account.TempUnschedulableReason = ""
			slog.Info("gateway_credits_recovered", "account_id", account.ID, "platform", account.Platform)
		}
	}
}

// isGatewayInsufficientCreditsError 识别网关账号的 credits 耗尽错误。
// CommandCode 余额耗尽时返回（状态码可能为 400/402/429）：
//
//	{"error":{"message":"You have insufficient credits to make this request. ...","type":"invalid_request_error"}}
func isGatewayInsufficientCreditsError(statusCode int, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusTooManyRequests:
	default:
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "insufficient credits")
}

// handleGatewayInsufficientCredits 把 credits 耗尽标记为可恢复的临时停调：
// SetTempUnschedulable 一个兜底周期，由轮询在充值恢复后提前清除
// （reconcileGatewayUsageCircuit），到期也会自然恢复。
func (s *RateLimitService) handleGatewayInsufficientCredits(ctx context.Context, account *Account, upstreamMsg string) {
	until := time.Now().Add(gatewayCreditsLowCooldown)
	reason := gatewayCreditsLowReasonPrefix
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		reason += ": " + msg
	}
	s.notifyAccountSchedulingBlocked(account, until, gatewayCreditsLowReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("gateway_credits_low_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("gateway_insufficient_credits",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}
