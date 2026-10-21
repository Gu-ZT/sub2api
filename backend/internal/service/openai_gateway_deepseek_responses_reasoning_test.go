package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// nopCloserString 把字符串包成上游响应体。
func nopCloserString(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

// deepSeekPassbackRecordingCache 记录 reasoning 缓存写入。
type deepSeekPassbackRecordingCache struct {
	stubGatewayCache
	sets map[string]string
}

func (c *deepSeekPassbackRecordingCache) SetReasoningContent(_ context.Context, itemID string, content string, _ time.Duration) error {
	if c.sets == nil {
		c.sets = make(map[string]string)
	}
	c.sets[itemID] = content
	return nil
}

func (c *deepSeekPassbackRecordingCache) GetReasoningContent(_ context.Context, itemID string) (string, error) {
	if v, ok := c.sets[itemID]; ok {
		return v, nil
	}
	return "", ErrReasoningContentNotFound
}

// deepSeekPassbackOpenCodeAccount 是官方 OpenCode Go 网关账号，用于验证
// 「聚合网关 + deepseek-* 模型」这一条判定。
func deepSeekPassbackOpenCodeAccount() *Account {
	return &Account{
		ID:       301,
		Name:     "opencode-go",
		Platform: PlatformOpenCodeGo,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"account_mode": AccountModeGo,
		},
	}
}

// deepSeekPassbackCommandCodeAccount 是官方 Command Code 网关账号。其目录模型名
// 带厂商前缀（deepseek/deepseek-v4-flash）。
func deepSeekPassbackCommandCodeAccount() *Account {
	return &Account{
		ID:       302,
		Name:     "command-code",
		Platform: PlatformCommandCode,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}
}

func deepSeekPassbackPlainOpenAIAccount() *Account {
	return &Account{
		ID:       303,
		Name:     "openai-official",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
	}
}

// encryptedOnlyReasoningHistory 是「明文已被剥离、只剩 encrypted_content」的
// 历史，正是触发上游 400 的典型形态。
func encryptedOnlyReasoningHistory() []byte {
	return []byte(`{"model":"deepseek-v4-flash","store":false,"input":[` +
		`{"type":"reasoning","id":"rs_enc1","summary":[],"encrypted_content":"opaque"},` +
		`{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]}`)
}

func TestRequiresDeepSeekReasoningPassback(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
		model   string
		want    bool
	}{
		{"deepseek 官方平台", &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}, "deepseek-chat", true},
		{"openai 映射到 deepseek 主机", openaiPlatformDeepSeekAccount(), "gpt-5.6-sol", true},
		{"官方 OpenCode + deepseek 模型", deepSeekPassbackOpenCodeAccount(), "deepseek-v4-flash", true},
		{"官方 OpenCode + 带前缀 deepseek 模型", deepSeekPassbackOpenCodeAccount(), "opencode/deepseek-v4-pro", true},
		{"官方 OpenCode + 非 deepseek 模型", deepSeekPassbackOpenCodeAccount(), "gpt-5.6-luna", false},
		{"Command Code + deepseek 模型", deepSeekPassbackCommandCodeAccount(), "deepseek/deepseek-v4-flash", true},
		{"Command Code + 非 deepseek 模型", deepSeekPassbackCommandCodeAccount(), "gpt-5.6-sol", false},
		{"真实 OpenAI", deepSeekPassbackPlainOpenAIAccount(), "gpt-5.6-sol", false},
		{"真实 OpenAI 上的 deepseek 模型名", deepSeekPassbackPlainOpenAIAccount(), "deepseek-v4-flash", false},
		{"未知中转站", &Account{
			Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://relay.example.com/v1"},
		}, "deepseek-v4-flash", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RequiresDeepSeekReasoningPassback(tc.account, tc.model))
		})
	}
	require.False(t, RequiresDeepSeekReasoningPassback(nil, "deepseek-v4-flash"))
}

func TestEnsureDeepSeekResponsesReasoningPassback(t *testing.T) {
	deepSeek := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	svc := &OpenAIGatewayService{}

	t.Run("缓存未命中补单空格占位", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		out := svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		require.Equal(t, deepSeekResponsesReasoningPlaceholderText, gjson.GetBytes(out, "input.0.summary.0.text").String())
		require.Equal(t, "summary_text", gjson.GetBytes(out, "input.0.summary.0.type").String())
		// 恒等改写：id 与密文原样保留，其余 item 不动。
		require.Equal(t, "rs_enc1", gjson.GetBytes(out, "input.0.id").String())
		require.Equal(t, "opaque", gjson.GetBytes(out, "input.0.encrypted_content").String())
		require.Equal(t, "call_1", gjson.GetBytes(out, "input.1.call_id").String())
	})

	t.Run("缓存命中回注真实明文", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		withCache := &OpenAIGatewayService{cache: &reasoningHitCache{values: map[string]string{"rs_enc1": "cached thinking"}}}
		out := withCache.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		require.Equal(t, "cached thinking", gjson.GetBytes(out, "input.0.summary.0.text").String())
	})

	t.Run("已有明文不覆盖并按 id 回写缓存", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","input":[{"type":"reasoning","id":"rs_plain","summary":[{"type":"summary_text","text":"real thinking"}]}]}`)
		cache := &deepSeekPassbackRecordingCache{}
		withCache := &OpenAIGatewayService{cache: cache}
		out := withCache.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		require.Equal(t, "real thinking", gjson.GetBytes(out, "input.0.summary.0.text").String())
		require.Equal(t, "real thinking", cache.sets["rs_plain"])
	})

	t.Run("content 明文同样视为已有", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","input":[{"type":"reasoning","id":"rs_c","summary":[],"content":[{"type":"reasoning_text","text":"visible"}]}]}`)
		out := svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		require.True(t, bytes.Equal(body, out), "已有明文时不得改写字节")
	})

	t.Run("非 DeepSeek 目标字节完全不变", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		out := svc.ensureDeepSeekResponsesReasoningPassback(deepSeekPassbackPlainOpenAIAccount(), body)
		require.True(t, bytes.Equal(body, out))
	})

	t.Run("无 input 时不 panic", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash"}`)
		require.True(t, bytes.Equal(body, svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)))
	})

	t.Run("非 reasoning item 不受影响", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
		out := svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		require.True(t, bytes.Equal(body, out))
	})

	t.Run("账号映射到 deepseek 模型时同样命中", func(t *testing.T) {
		// 客户端发 gpt-5.6-sol，账号 model_mapping 把它映射到 opencode 的 deepseek
		// 模型：判定必须按映射后的上游模型走，否则归一化仍会删掉 reasoning item。
		account := deepSeekPassbackOpenCodeAccount()
		account.Credentials["model_mapping"] = map[string]any{"gpt-5.6-sol": "deepseek-v4-flash"}
		require.True(t, RequiresDeepSeekReasoningPassback(account, account.GetMappedModel("gpt-5.6-sol")))
		require.False(t, RequiresDeepSeekReasoningPassback(account, "gpt-5.6-sol"), "未映射的原始名本身不是 deepseek 模型")
	})

	t.Run("幂等", func(t *testing.T) {
		// 重试循环会对同一 body 反复调用；不幂等会为每次都算出新 body 而耗尽预算。
		body := encryptedOnlyReasoningHistory()
		once := svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, body)
		twice := svc.ensureDeepSeekResponsesReasoningPassback(deepSeek, once)
		require.True(t, bytes.Equal(once, twice), "第二次补齐必须字节不变")

		placement := []byte(`{"model":"deepseek-v4-flash","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}`)
		firstSynth, changed := synthesizeDeepSeekResponsesReasoningItems(placement)
		require.True(t, changed)
		_, changedAgain := synthesizeDeepSeekResponsesReasoningItems(firstSynth)
		require.False(t, changedAgain, "已补过占位时不得再补一条")
	})
}

func TestIsDeepSeekReasoningPassbackRejection(t *testing.T) {
	rejection := []byte(`{"error":{"code":"invalid_request_error","message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API","type":"invalid_request_error"}}`)
	require.True(t, isDeepSeekReasoningPassbackRejection(400, rejection))

	// 上游包裹一层 server_error 并加前缀，仍应命中（片段匹配而非整句）。
	wrapped := []byte(`{"error":{"code":"server_error","message":"Upstream request failed: [invalid_request_error] The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API. (request_id: abc)"}}`)
	require.True(t, isDeepSeekReasoningPassbackRejection(400, wrapped))

	require.False(t, isDeepSeekReasoningPassbackRejection(500, rejection), "非 400 不触发")
	require.False(t, isDeepSeekReasoningPassbackRejection(400, []byte(`{"error":{"message":"model not found"}}`)))
	require.False(t, isDeepSeekReasoningPassbackRejection(400, nil))
}

func TestRepairDeepSeekResponsesReasoningRejection(t *testing.T) {
	deepSeek := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	rejection := []byte(`{"error":{"code":"invalid_request_error","message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API"}}`)
	svc := &OpenAIGatewayService{}

	t.Run("补齐已有 reasoning item 的明文", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		repaired, changed := svc.repairDeepSeekResponsesReasoningRejection(deepSeek, body, rejection)
		require.True(t, changed)
		require.Equal(t, deepSeekResponsesReasoningPlaceholderText, gjson.GetBytes(repaired, "input.0.summary.0.text").String())
	})

	t.Run("历史里没有 reasoning item 时为 assistant 轮次补占位", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}`)
		repaired, changed := svc.repairDeepSeekResponsesReasoningRejection(deepSeek, body, rejection)
		require.True(t, changed)
		require.Equal(t, "reasoning", gjson.GetBytes(repaired, "input.1.type").String())
		require.Equal(t, deepSeekResponsesReasoningPlaceholderText, gjson.GetBytes(repaired, "input.1.summary.0.text").String())
		require.Equal(t, "assistant", gjson.GetBytes(repaired, "input.2.role").String())
		require.Equal(t, "user", gjson.GetBytes(repaired, "input.0.role").String())
	})

	t.Run("已有 reasoning item 时不再合成", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-flash","input":[` +
			`{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"t"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}`)
		_, changed := svc.repairDeepSeekResponsesReasoningRejection(deepSeek, body, rejection)
		require.False(t, changed, "已是合规历史时不得改写")
	})

	t.Run("同一轮的多个工具调用只补一条占位", func(t *testing.T) {
		// 一轮 assistant 响应可以并发发起多个工具调用；在这些调用之间插 reasoning
		// item 会把一轮切成两轮，历史与上游实际产生的内容不再对应。
		body := []byte(`{"model":"deepseek-v4-flash","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},` +
			`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
			`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"},` +
			`{"type":"function_call_output","call_id":"c1","output":"1"},` +
			`{"type":"function_call_output","call_id":"c2","output":"2"}]}`)
		repaired, changed := synthesizeDeepSeekResponsesReasoningItems(body)
		require.True(t, changed)
		require.Equal(t, int64(6), gjson.GetBytes(repaired, "input.#").Int(), "5 个原有 item 加 1 条占位")
		require.Equal(t, "reasoning", gjson.GetBytes(repaired, "input.1.type").String())
		require.Equal(t, "function_call", gjson.GetBytes(repaired, "input.2.type").String())
		require.Equal(t, "c1", gjson.GetBytes(repaired, "input.2.call_id").String())
		require.Equal(t, "c2", gjson.GetBytes(repaired, "input.3.call_id").String())
	})

	t.Run("非 DeepSeek 目标不改写", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		_, changed := svc.repairDeepSeekResponsesReasoningRejection(deepSeekPassbackPlainOpenAIAccount(), body, rejection)
		require.False(t, changed)
	})

	t.Run("非这类 400 不改写", func(t *testing.T) {
		body := encryptedOnlyReasoningHistory()
		_, changed := svc.repairDeepSeekResponsesReasoningRejection(deepSeek, body, []byte(`{"error":{"message":"unknown parameter: foo"}}`))
		require.False(t, changed)
	})
}

func newDeepSeekResponsesHistoryContext(t *testing.T, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func deepSeekResponsesOKBody() string {
	return `{"id":"resp_ph","object":"response","created_at":0,"status":"completed","model":"deepseek-v4-flash","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
}

// TestForwardResponses_DeepSeekPassbackOnResponsesUpstream 覆盖非透传的
// Responses→Responses 出站：此前这条路径完全没有 reasoning 回传补齐。
func TestForwardResponses_DeepSeekPassbackOnResponsesUpstream(t *testing.T) {
	body := encryptedOnlyReasoningHistory()
	c := newDeepSeekResponsesHistoryContext(t, body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ds_resp"}},
		Body:       nopCloserString(deepSeekResponsesOKBody()),
	}}
	svc := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: upstream,
		cache:        &reasoningHitCache{values: map[string]string{"rs_enc1": "cached thinking"}},
	}

	result, err := svc.Forward(context.Background(), c, deepSeekNativeResponsesImageAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, upstream.lastReq.URL.String(), "/responses")
	require.Equal(t, "cached thinking", gjson.GetBytes(upstream.lastBody, "input.0.summary.0.text").String())
	require.Equal(t, "rs_enc1", gjson.GetBytes(upstream.lastBody, "input.0.id").String())
}

// TestForwardResponses_PassthroughDeepSeekPassback 覆盖透传分支：同一份历史在
// openai_passthrough 账号上也要补齐 reasoning 明文。
func TestForwardResponses_PassthroughDeepSeekPassback(t *testing.T) {
	body := encryptedOnlyReasoningHistory()
	c := newDeepSeekResponsesHistoryContext(t, body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ds_pass"}},
		Body:       nopCloserString(deepSeekResponsesOKBody()),
	}}
	account := openaiPlatformDeepSeekAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses),
	}
	account.Extra["openai_passthrough"] = true
	svc := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: upstream,
		cache:        &reasoningHitCache{values: map[string]string{"rs_enc1": "cached thinking"}},
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "cached thinking", gjson.GetBytes(upstream.lastBody, "input.0.summary.0.text").String())
}

// TestForwardResponses_RealOpenAIDoesNotGetReasoningPlaceholder 是反向护栏：
// 同样的历史发给真实 OpenAI 上游时必须一个字节都不改。
func TestForwardResponses_RealOpenAIDoesNotGetReasoningPlaceholder(t *testing.T) {
	body := encryptedOnlyReasoningHistory()
	c := newDeepSeekResponsesHistoryContext(t, body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_openai"}},
		Body:       nopCloserString(deepSeekResponsesOKBody()),
	}}
	svc := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: upstream,
	}

	_, err := svc.Forward(context.Background(), c, deepSeekPassbackPlainOpenAIAccount(), body)
	require.NoError(t, err)
	require.Equal(t, float64(0), gjson.GetBytes(upstream.lastBody, "input.0.summary.#").Float())
	require.False(t, strings.Contains(string(upstream.lastBody), deepSeekResponsesReasoningPlaceholderText+`"}]`))
}

// TestNormalizeOpenAIResponsesCompatibilityBodyKeepsDeepSeekReasoning 验证 store=false
// 归一化在 DeepSeek 系目标上不再删掉 reasoning item：restore 后它会补明文，但如果
// item 先被归一化整条删掉，任何补明文都无从谈起。
func TestNormalizeOpenAIResponsesCompatibilityBodyKeepsDeepSeekReasoning(t *testing.T) {
	// 无 encrypted_content 的 reasoning item 是 store=false 归一化会整条删除的形态。
	body := []byte(`{"model":"deepseek-v4-flash","store":false,"input":[` +
		`{"type":"reasoning","id":"rs_bare","summary":[],"content":[{"type":"reasoning_text","text":"visible"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	deepSeekAccount := openaiPlatformDeepSeekAccount()
	normalized, _, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, deepSeekAccount, openAIResponsesCompatibilityOptions{})
	require.NoError(t, err)
	require.Equal(t, "reasoning", gjson.GetBytes(normalized, "input.0.type").String(), "DeepSeek 目标必须保留 reasoning item")
	require.Equal(t, "visible", gjson.GetBytes(normalized, "input.0.content.0.text").String(), "DeepSeek 目标必须保留 reasoning 明文")

	otherAccount := deepSeekPassbackPlainOpenAIAccount()
	normalizedOther, _, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, otherAccount, openAIResponsesCompatibilityOptions{})
	require.NoError(t, err)
	require.Equal(t, "message", gjson.GetBytes(normalizedOther, "input.0.type").String(), "真实 OpenAI 目标仍走原归一化（reasoning item 被删）")
}
