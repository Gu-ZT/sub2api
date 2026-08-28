package service

// CommandCode 网关账号用量探测服务。
//
// 背景：CommandCode（https://api.commandcode.ai）作为 OpenAI 兼容协议的上游
// 网关接入，sub2api 侧仅以自定义 base_url（credentials["base_url"]）标识，
// 平台类型仍为 openai / anthropic。官方提供协议无关的
// GET https://api.commandcode.ai/alpha/billing/credits 端点（Bearer API Key
// 鉴权），返回 rolling(5h) / weekly / monthly 三个滚动窗口的已用量、上限与
// 重置时间，以及月度 credits 余额：
//
//	{
//	  "credits": {"monthlyCredits": 12.34},
//	  "windowLimits": {
//	    "fiveHour": {"used": 42, "cap": 100, "resetAt": "..."},
//	    "weekly":   {"used": 10, "cap": 100, "resetAt": "..."}
//	  }
//	}
//
// 用量窗口映射到通用 UsageInfo：fiveHour → five_hour、weekly → seven_day、
// monthly → thirty_day（monthly 无独立窗口时按 credits 余额推导），由管理端
// 账号列表的「用量窗口」单元格直接复用渲染。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

const (
	// commandCodeUsageTimeout 是上游请求超时。
	commandCodeUsageTimeout = 15 * time.Second
	commandCodeMaxBodyBytes = 256 * 1024
	// commandCodeMonthlyCap 是 CommandCode GOAT 计划的月度 credits 上限。
	// 官方 /alpha/billing/credits 不返回月度 cap，按客户端脚本同源常量推导：
	// 月度已用 = cap - credits.monthlyCredits。
	commandCodeMonthlyCap = 70.0
)

// commandCodeUsageURL 是 CommandCode 官方用量端点。
// 固定使用官方域名：识别条件已要求 base_url 指向 api.commandcode.ai，
// 转发流量本来就发往该主机，这里不引入任何新的外发信任域。
// var 仅为单测注入 httptest 端点；生产路径恒为官方地址。
var commandCodeUsageURL = "https://api.commandcode.ai/alpha/billing/credits"

// commandCodeSubscriptionsURL 是 CommandCode 官方订阅端点，
// 提供 currentPeriodEnd（月度窗口重置时间，GOAT 计划）。
// var 仅为单测注入 httptest 端点；生产路径恒为官方地址。
var commandCodeSubscriptionsURL = "https://api.commandcode.ai/alpha/billing/subscriptions"

// isCommandCodeBaseURL 判断 base_url 是否指向 api.commandcode.ai。
// 严格按 URL 解析后比对主机名与路径前缀，不做子串匹配，
// 避免攻击者用 api.commandcode.ai.evil.com 之类的路径伪装触发。
func isCommandCodeBaseURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "api.commandcode.ai" {
		return false
	}
	path := strings.TrimRight(strings.ToLower(u.Path), "/")
	return path == "" || path == "/" || path == "/provider" || path == "/provider/v1" ||
		strings.HasPrefix(path, "/provider/v1/")
}

// IsCommandCode 报告账号是否为 CommandCode 网关账号
// （base_url 指向 api.commandcode.ai 的任意协议/类型账号）。
func (a *Account) IsCommandCode() bool {
	if a == nil {
		return false
	}
	return isCommandCodeBaseURL(a.GetCredential("base_url"))
}

// commandCodeWindow 是 /alpha/billing/credits 返回的单个滚动窗口档位。
type commandCodeWindow struct {
	Used    float64 `json:"used"`
	Cap     float64 `json:"cap"`
	ResetAt any     `json:"resetAt"` // 官方返回毫秒时间戳（数字），兼容字符串
}

// commandCodeCredits 是 credits 余额信息。
type commandCodeCredits struct {
	MonthlyCredits any `json:"monthlyCredits"` // 数字或字符串均兼容
}

// commandCodeMonthlyCreditsValue 提取月度 credits 余额（容错解析）。
func commandCodeMonthlyCreditsValue(v any) float64 {
	switch val := v.(type) {
	case nil:
		return 0
	case float64:
		return val
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			return 0
		}
		return f
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return 0
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// commandCodeUsageResponse 是 CommandCode /alpha/billing/credits 的响应体。
type commandCodeUsageResponse struct {
	Credits      commandCodeCredits `json:"credits"`
	WindowLimits struct {
		FiveHour *commandCodeWindow `json:"fiveHour"`
		Weekly   *commandCodeWindow `json:"weekly"`
	} `json:"windowLimits"`
}

// commandCodeSubscription 是 /alpha/billing/subscriptions 返回的订阅信息。
type commandCodeSubscription struct {
	Status           string `json:"status"`
	PlanID           string `json:"planId"`
	CurrentPeriodEnd string `json:"currentPeriodEnd"` // RFC3339 月度窗口结束时间
}

// commandCodeSubscriptionsResponse 是 /alpha/billing/subscriptions 的响应体。
type commandCodeSubscriptionsResponse struct {
	Success bool                    `json:"success"`
	Data    commandCodeSubscription `json:"data"`
}

// getCommandCodeUsage 拉取 CommandCode 账号的滚动窗口用量。
// 成功缓存 apiCacheTTL / 错误负缓存 apiErrorCacheTTL，singleflight 防击穿；
// force 为 true 时绕过缓存读取（管理端「查询」按钮）。
func (s *AccountUsageService) getCommandCodeUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	accountID := account.ID

	// 1. 检查缓存（成功响应 3 分钟 / 错误响应 1 分钟）
	if !force {
		if cached, ok := s.cache.commandCodeCache.Load(accountID); ok {
			if cache, ok := cached.(*commandCodeUsageCache); ok {
				age := time.Since(cache.timestamp)
				if cache.err != nil && age < apiErrorCacheTTL {
					return nil, cache.err
				}
				if cache.usage != nil && cache.err == nil && age < apiCacheTTL {
					usage := *cache.usage
					recalcCommandCodeRemainingSeconds(&usage)
					return &usage, nil
				}
			}
		}
	}

	// 2. singleflight 防止并发击穿
	flightKey := fmt.Sprintf("commandcode-usage:%d", accountID)
	result, flightErr, _ := s.cache.commandCodeFlight.Do(flightKey, func() (any, error) {
		// 再次检查缓存（可能在等待 singleflight 期间被其他请求填充）；
		// force=true 时跳过，确保主动查询真实打到上游。
		if !force {
			if cached, ok := s.cache.commandCodeCache.Load(accountID); ok {
				if cache, ok := cached.(*commandCodeUsageCache); ok {
					age := time.Since(cache.timestamp)
					if cache.err != nil && age < apiErrorCacheTTL {
						return nil, cache.err
					}
					if cache.usage != nil && cache.err == nil && age < apiCacheTTL {
						return cache.usage, nil
					}
				}
			}
		}
		usage, fetchErr := s.fetchCommandCodeUsage(ctx, account)
		s.cache.commandCodeCache.Store(accountID, &commandCodeUsageCache{
			usage:     usage,
			err:       fetchErr,
			timestamp: time.Now(),
		})
		if fetchErr != nil {
			return nil, fetchErr
		}
		return usage, nil
	})
	if flightErr != nil {
		return nil, flightErr
	}
	usage, ok := result.(*UsageInfo)
	if !ok || usage == nil {
		return nil, fmt.Errorf("commandcode usage: invalid result for account %d", accountID)
	}
	cloned := *usage
	recalcCommandCodeRemainingSeconds(&cloned)
	return &cloned, nil
}

// fetchCommandCodeUsage 发起真实的上游请求并解析为 UsageInfo。
func (s *AccountUsageService) fetchCommandCodeUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		return nil, fmt.Errorf("commandcode usage: account %d has no api_key", account.ID)
	}

	var proxyURL string
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	// 1. credits 端点：5h / weekly 窗口 + 月度余额
	bodyBytes, err := s.fetchCommandCodeEndpoint(ctx, apiKey, proxyURL, commandCodeUsageURL)
	if err != nil {
		return nil, err
	}
	var parsed commandCodeUsageResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("commandcode usage: parse response: %w", err)
	}

	// 2. subscriptions 端点：月度窗口重置时间（currentPeriodEnd）。
	// 失败不致命——降级为无重置时间，仅影响 30d 倒计时显示。
	periodEnd := s.fetchCommandCodePeriodEnd(ctx, apiKey, proxyURL)

	now := time.Now()
	info := &UsageInfo{UpdatedAt: &now}
	info.FiveHour = buildCommandCodeProgress(parsed.WindowLimits.FiveHour)
	info.SevenDay = buildCommandCodeProgress(parsed.WindowLimits.Weekly)

	// 月度窗口：CommandCode 不提供 monthly 档位，按 credits 余额推导——
	// GOAT 计划月度上限 commandCodeMonthlyCap credits，已用 = 上限 - 剩余余额。
	// 余额耗尽（=0）或未下发时同样渲染（显示 100% / 空行由 build 函数归一）。
	monthlyCredits := commandCodeMonthlyCreditsValue(parsed.Credits.MonthlyCredits)
	monthlyUsed := commandCodeMonthlyCap - monthlyCredits
	if monthlyUsed < 0 {
		monthlyUsed = 0
	}
	monthlyWindow := &commandCodeWindow{
		Used: monthlyUsed,
		Cap:  commandCodeMonthlyCap,
	}
	if periodEnd != nil {
		monthlyWindow.ResetAt = *periodEnd
	}
	info.ThirtyDay = buildCommandCodeProgress(monthlyWindow)
	return info, nil
}

// fetchCommandCodeEndpoint 请求 CommandCode 官方端点并返回响应体（Bearer API Key）。
func (s *AccountUsageService) fetchCommandCodeEndpoint(ctx context.Context, apiKey, proxyURL, endpoint string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, commandCodeUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("commandcode usage: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               commandCodeUsageTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
		// 固定官方域名，但保留 DNS Rebinding 校验以与 Anthropic 用量拉取一致。
		ValidateResolvedIP: true,
		// 测试注入 httptest 回环地址时置 true（与 claudeUsageService 同款可测试性设计）。
		AllowPrivateHosts: s.allowCommandCodePrivateHosts,
	})
	if err != nil {
		return nil, fmt.Errorf("commandcode usage: build client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("commandcode usage: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, commandCodeMaxBodyBytes))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("commandcode usage: authentication failed (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("commandcode usage: API error (HTTP %d): %s", resp.StatusCode, truncateForLog(bodyBytes, 240))
	}
	if readErr != nil {
		return nil, fmt.Errorf("commandcode usage: read response: %w", readErr)
	}
	return bodyBytes, nil
}

// fetchCommandCodePeriodEnd 拉取 subscriptions 端点的 currentPeriodEnd（RFC3339）。
// 任何失败都返回 nil（降级：30d 进度条无重置倒计时，不阻塞主用量展示）。
func (s *AccountUsageService) fetchCommandCodePeriodEnd(ctx context.Context, apiKey, proxyURL string) *string {
	bodyBytes, err := s.fetchCommandCodeEndpoint(ctx, apiKey, proxyURL, commandCodeSubscriptionsURL)
	if err != nil {
		slog.Warn("commandcode_subscriptions_fetch_failed", "error", err)
		return nil
	}
	var parsed commandCodeSubscriptionsResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		slog.Warn("commandcode_subscriptions_parse_failed", "error", err)
		return nil
	}
	periodEnd := strings.TrimSpace(parsed.Data.CurrentPeriodEnd)
	if periodEnd == "" {
		return nil
	}
	if _, err := parseTime(periodEnd); err != nil {
		slog.Warn("commandcode_subscriptions_period_end_parse_failed", "period_end", periodEnd, "error", err)
		return nil
	}
	return &periodEnd
}

// buildCommandCodeProgress 将单个窗口档位转换为通用 UsageProgress。
// 有 cap（可计算百分比）时始终渲染，即使 used=0 也显示 0%；
// 无 cap 且无 resetAt 时返回 nil（前端不渲染该行）。
func buildCommandCodeProgress(w *commandCodeWindow) *UsageProgress {
	if w == nil {
		return nil
	}
	progress := &UsageProgress{}
	if w.Cap > 0 {
		progress.Utilization = (w.Used / w.Cap) * 100
	}
	if resetAt, err := parseCommandCodeResetAt(w.ResetAt); err == nil {
		progress.ResetsAt = &resetAt
		progress.RemainingSeconds = int(time.Until(resetAt).Seconds())
		if progress.RemainingSeconds < 0 {
			progress.RemainingSeconds = 0
		}
	}
	// 窗口已过期（resetAt 在 now 之前）→ 额度已重置，归零；
	// 与 Codex / Setup Token 分支保持一致，避免 UI 渲染矛盾状态。
	if progress.ResetsAt != nil && !time.Now().Before(*progress.ResetsAt) {
		progress.Utilization = 0
	}
	// 有 cap 即可渲染（0% 也要显示）；无 cap 且无 resetAt 则无信息可展示。
	if w.Cap > 0 {
		return progress
	}
	if progress.Utilization == 0 && progress.ResetsAt == nil {
		return nil
	}
	return progress
}

// parseCommandCodeResetAt 解析 CommandCode 的 resetAt。
// 官方返回毫秒级 Unix 时间戳（JSON 数字，JS 侧 Date.now() 同量纲），
// 同时也兼容字符串形态（毫秒/秒时间戳或 RFC3339），避免上游格式变化时直接失效。
func parseCommandCodeResetAt(v any) (time.Time, error) {
	switch val := v.(type) {
	case nil:
		return time.Time{}, fmt.Errorf("empty resetAt")
	case float64:
		return commandCodeResetAtFromNumber(val)
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return commandCodeResetAtFromNumber(f)
		}
		return time.Time{}, fmt.Errorf("invalid resetAt number: %s", val.String())
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return time.Time{}, fmt.Errorf("empty resetAt")
		}
		if ms, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return commandCodeResetAtFromInt(ms)
		}
		return parseTime(trimmed)
	default:
		return time.Time{}, fmt.Errorf("unsupported resetAt type %T", v)
	}
}

func commandCodeResetAtFromNumber(f float64) (time.Time, error) {
	if f <= 0 {
		return time.Time{}, fmt.Errorf("non-positive resetAt: %v", f)
	}
	return commandCodeResetAtFromInt(int64(f))
}

func commandCodeResetAtFromInt(ms int64) (time.Time, error) {
	if ms <= 0 {
		return time.Time{}, fmt.Errorf("non-positive resetAt: %d", ms)
	}
	// 毫秒时间戳：13 位；若为秒级（10 位）也兼容。
	if ms < 1e12 {
		return time.Unix(ms, 0), nil
	}
	return time.UnixMilli(ms), nil
}

// recalcCommandCodeRemainingSeconds 从缓存取出时重算剩余秒数，避免返回过时倒计时。
func recalcCommandCodeRemainingSeconds(info *UsageInfo) {
	if info == nil {
		return
	}
	now := time.Now()
	for _, progress := range []*UsageProgress{info.FiveHour, info.SevenDay, info.ThirtyDay} {
		if progress == nil || progress.ResetsAt == nil {
			continue
		}
		remaining := int(progress.ResetsAt.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		progress.RemainingSeconds = remaining
	}
}
