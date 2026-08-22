package service

// OpenCode Zen Go 网关账号用量探测服务。
//
// 背景：OpenCode Go（https://opencode.ai/zen/go）作为 OpenAI 或 Anthropic 协议的
// 上游网关接入，sub2api 侧仅以自定义 base_url（credentials["base_url"]）标识，
// 平台类型仍为 openai / anthropic。官方提供与转发协议无关的
// GET https://opencode.ai/zen/go/v1/usage 端点（Bearer API Key 鉴权），返回
// rolling(5h) / weekly / monthly 三个滚动窗口的已用百分比与重置时间：
//
//	{"usage":{"rolling":{"percent":42,"resetsAt":"..."},"weekly":{...},"monthly":{...}}}
//
// 用量窗口映射到通用 UsageInfo：rolling → five_hour、weekly → seven_day、
// monthly → thirty_day，由管理端账号列表的「用量窗口」单元格直接复用渲染。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

const (
	// openCodeUsageTimeout 是上游请求超时。
	openCodeUsageTimeout = 15 * time.Second
	openCodeMaxBodyBytes = 256 * 1024
)

// openCodeUsageURL 是 OpenCode Zen Go 官方用量端点。
// 固定使用官方域名：识别条件已要求 base_url 指向 opencode.ai/zen/go，
// 转发流量本来就发往该主机，这里不引入任何新的外发信任域。
// var 仅为单测注入 httptest 端点；生产路径恒为官方地址。
var openCodeUsageURL = "https://opencode.ai/zen/go/v1/usage"

// isOpenCodeGoBaseURL 判断 base_url 是否指向 opencode.ai/zen/go。
// 严格按 URL 解析后比对主机名与路径前缀，不做子串匹配，
// 避免攻击者用 opencode.ai/zen/go.evil.com 之类的路径伪装触发。
func isOpenCodeGoBaseURL(raw string) bool {
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
	if host != "opencode.ai" && host != "www.opencode.ai" {
		return false
	}
	path := strings.TrimRight(strings.ToLower(u.Path), "/")
	return path == "/zen/go" || strings.HasPrefix(path, "/zen/go/")
}

// IsOpenCodeGo 报告账号是否为 OpenCode Zen Go 网关账号
// （base_url 指向 opencode.ai/zen/go 的任意协议/类型账号）。
func (a *Account) IsOpenCodeGo() bool {
	if a == nil {
		return false
	}
	return isOpenCodeGoBaseURL(a.GetCredential("base_url"))
}

// openCodeUsageWindow 是 /v1/usage 返回的单个滚动窗口档位。
type openCodeUsageWindow struct {
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

// openCodeUsageResponse 是 OpenCode Zen Go /v1/usage 的响应体。
type openCodeUsageResponse struct {
	Usage struct {
		Rolling openCodeUsageWindow `json:"rolling"`
		Weekly  openCodeUsageWindow `json:"weekly"`
		Monthly openCodeUsageWindow `json:"monthly"`
	} `json:"usage"`
}

// getOpenCodeGoUsage 拉取 OpenCode Go 账号的滚动窗口用量。
// 成功缓存 apiCacheTTL / 错误负缓存 apiErrorCacheTTL，singleflight 防击穿；
// force 为 true 时绕过缓存读取（管理端「查询」按钮）。
func (s *AccountUsageService) getOpenCodeGoUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	accountID := account.ID

	// 1. 检查缓存（成功响应 3 分钟 / 错误响应 1 分钟）
	if !force {
		if cached, ok := s.cache.openCodeCache.Load(accountID); ok {
			if cache, ok := cached.(*openCodeUsageCache); ok {
				age := time.Since(cache.timestamp)
				if cache.err != nil && age < apiErrorCacheTTL {
					return nil, cache.err
				}
				if cache.usage != nil && cache.err == nil && age < apiCacheTTL {
					usage := *cache.usage
					recalcOpenCodeRemainingSeconds(&usage)
					return &usage, nil
				}
			}
		}
	}

	// 2. singleflight 防止并发击穿
	flightKey := fmt.Sprintf("opencode-usage:%d", accountID)
	result, flightErr, _ := s.cache.openCodeFlight.Do(flightKey, func() (any, error) {
		// 再次检查缓存（可能在等待 singleflight 期间被其他请求填充）
		if cached, ok := s.cache.openCodeCache.Load(accountID); ok {
			if cache, ok := cached.(*openCodeUsageCache); ok {
				age := time.Since(cache.timestamp)
				if cache.err != nil && age < apiErrorCacheTTL {
					return nil, cache.err
				}
				if cache.usage != nil && cache.err == nil && age < apiCacheTTL {
					return cache.usage, nil
				}
			}
		}
		usage, fetchErr := s.fetchOpenCodeUsage(ctx, account)
		s.cache.openCodeCache.Store(accountID, &openCodeUsageCache{
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
		return nil, fmt.Errorf("opencode usage: invalid result for account %d", accountID)
	}
	cloned := *usage
	recalcOpenCodeRemainingSeconds(&cloned)
	return &cloned, nil
}

// fetchOpenCodeUsage 发起真实的上游请求并解析为 UsageInfo。
func (s *AccountUsageService) fetchOpenCodeUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		return nil, fmt.Errorf("opencode usage: account %d has no api_key", account.ID)
	}

	var proxyURL string
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	reqCtx, cancel := context.WithTimeout(ctx, openCodeUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, openCodeUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("opencode usage: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               openCodeUsageTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
		// 固定官方域名，但保留 DNS Rebinding 校验以与 Anthropic 用量拉取一致。
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, fmt.Errorf("opencode usage: build client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode usage: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, openCodeMaxBodyBytes))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("opencode usage: authentication failed (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opencode usage: API error (HTTP %d): %s", resp.StatusCode, truncateForLog(bodyBytes, 240))
	}
	if readErr != nil {
		return nil, fmt.Errorf("opencode usage: read response: %w", readErr)
	}

	var parsed openCodeUsageResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("opencode usage: parse response: %w", err)
	}

	now := time.Now()
	info := &UsageInfo{UpdatedAt: &now}
	info.FiveHour = buildOpenCodeProgress(parsed.Usage.Rolling)
	info.SevenDay = buildOpenCodeProgress(parsed.Usage.Weekly)
	info.ThirtyDay = buildOpenCodeProgress(parsed.Usage.Monthly)
	return info, nil
}

// buildOpenCodeProgress 将单个窗口档位转换为通用 UsageProgress；
// percent 与 resetsAt 均缺失时返回 nil（前端不渲染该行）。
func buildOpenCodeProgress(w openCodeUsageWindow) *UsageProgress {
	progress := &UsageProgress{Utilization: w.Percent}
	if w.ResetsAt != "" {
		if resetAt, err := parseTime(w.ResetsAt); err == nil {
			progress.ResetsAt = &resetAt
			progress.RemainingSeconds = int(time.Until(resetAt).Seconds())
			if progress.RemainingSeconds < 0 {
				progress.RemainingSeconds = 0
			}
		} else {
			slog.Warn("opencode_usage_reset_parse_failed", "resets_at", w.ResetsAt, "error", err)
		}
	}
	// 窗口已过期（resetAt 在 now 之前）→ 额度已重置，归零；
	// 与 Codex / Setup Token 分支保持一致，避免 UI 渲染矛盾状态。
	if progress.ResetsAt != nil && !time.Now().Before(*progress.ResetsAt) {
		progress.Utilization = 0
	}
	if progress.Utilization == 0 && progress.ResetsAt == nil {
		return nil
	}
	return progress
}

// recalcOpenCodeRemainingSeconds 从缓存取出时重算剩余秒数，避免返回过时倒计时。
func recalcOpenCodeRemainingSeconds(info *UsageInfo) {
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
