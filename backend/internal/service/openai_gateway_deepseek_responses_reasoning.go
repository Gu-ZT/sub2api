package service

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// deepSeekResponsesReasoningPlaceholderText 是 Responses 侧 thinking-mode 占位
// 明文，取值与 Chat Completions 侧一致（单个空格）：DeepSeek 拒绝空串，非空即可
// 通过，LiteLLM 同样注入单个空格。
const deepSeekResponsesReasoningPlaceholderText = " "

// isDeepSeekResponsesReasoningItem 报告 Responses input item 是否是 reasoning
// item。
func isDeepSeekResponsesReasoningItem(item gjson.Result) bool {
	return item.IsObject() && strings.TrimSpace(item.Get("type").String()) == "reasoning"
}

// ensureDeepSeekResponsesReasoningPassback 给 Responses 出站请求补齐 thinking
// mode 要求的 reasoning 明文回传。
//
// DeepSeek 系上游（见 RequiresDeepSeekReasoningPassback）要求历史里产生过思维的
// 轮次把 reasoning 明文回传，缺失时直接 400
// "The `reasoning_content` in the thinking mode must be passed back to the API"。
// 走到这里的请求已经过 OpenAI 兼容化归一化，明文可能只剩 encrypted_content，
// 甚至被整条删除，因此按以下顺序补齐，且只填缺口、不覆盖已有明文：
//
//  1. item 自带明文（客户端原样回传 summary）：保留，并按 id 顺手回写缓存；
//  2. 只有 id、明文被剥离成 encrypted-only：用网关侧缓存按 id 回查；
//  3. 都没有：写单个空格占位，保证上游的「必须回传」校验通过。
//
// s 为 nil 时退化为无缓存的纯占位模式（缓存只是优化，不是正确性前提）。
// 非 DeepSeek 系目标原样返回同一个字节切片（大请求不做任何拷贝）。
func (s *OpenAIGatewayService) ensureDeepSeekResponsesReasoningPassback(account *Account, body []byte) []byte {
	if !requiresDeepSeekReasoningPassback(account, body) {
		return body
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body
	}

	updated := body
	changed := false
	for index, item := range input.Array() {
		if !isDeepSeekResponsesReasoningItem(item) {
			continue
		}
		id, text, _ := apicompat.ExtractResponsesReasoningItem([]byte(item.Raw))
		if strings.TrimSpace(text) != "" {
			// 客户端回传了明文：借机刷新缓存 TTL，供后续只回传密文的历史复用。
			if s != nil && strings.TrimSpace(id) != "" {
				s.setReasoningContent(id, strings.TrimSpace(text))
			}
			continue
		}

		restored := ""
		if s != nil && strings.TrimSpace(id) != "" {
			restored = strings.TrimSpace(s.reasoningContentByID(id))
		}
		if restored == "" {
			restored = deepSeekResponsesReasoningPlaceholderText
		}
		next, err := sjson.SetBytes(updated, "input."+strconv.Itoa(index)+".summary",
			[]map[string]string{{"type": "summary_text", "text": restored}})
		if err != nil {
			return body
		}
		updated = next
		changed = true
	}
	if !changed {
		return body
	}
	return updated
}

// isDeepSeekReasoningPassbackRejection 判定上游 400 是否是「reasoning 明文必须回
// 传」这一类拒绝。只匹配两个稳定的片段（字段名 + 动作），不匹配整句：报错文案由
// 上游逐字回吐，中转站可能改写措辞或附加前缀。
func isDeepSeekReasoningPassbackRejection(statusCode int, responseBody []byte) bool {
	if statusCode != 400 || len(responseBody) == 0 {
		return false
	}
	message := strings.ToLower(extractUpstreamErrorMessage(responseBody))
	if !strings.Contains(message, "reasoning_content") {
		return false
	}
	return strings.Contains(message, "must be passed back")
}

// repairDeepSeekResponsesReasoningRejection 在命中「reasoning 明文必须回传」的
// 400 后重写请求体，供调用方用既有重试预算重发一次。
//
// 先按 ensureDeepSeekResponsesReasoningPassback 补齐已有 reasoning item 的明文；
// 若历史里一条 reasoning item 都不剩（被前置归一化删空，或客户端从未回传），再按
// Chat Completions 侧 ensureDeepSeekChatReasoningPlaceholders 的同一策略，为每个
// 缺 reasoning 的 assistant 轮次补一条占位 reasoning item。
//
// 非 DeepSeek 系目标一律不改写：真实 OpenAI 上游不认这种合成位置。
func (s *OpenAIGatewayService) repairDeepSeekResponsesReasoningRejection(account *Account, body, responseBody []byte) ([]byte, bool) {
	if !isDeepSeekReasoningPassbackRejection(400, responseBody) {
		return nil, false
	}
	if !requiresDeepSeekReasoningPassback(account, body) {
		return nil, false
	}

	repaired := s.ensureDeepSeekResponsesReasoningPassback(account, body)
	if !bytes.Equal(repaired, body) {
		return repaired, true
	}
	return synthesizeDeepSeekResponsesReasoningItems(body)
}

// synthesizeDeepSeekResponsesReasoningItems 为缺 reasoning 的 assistant 轮次补
// 占位 reasoning item，判定口径与 Chat Completions 侧的 reasoning_content 占位
// 一致：一轮 assistant 响应（含它的全部工具调用）之前没有 reasoning item 时补一条。
//
// 这里必须按「轮次」而不是「紧邻的上一项」判定：同一轮 assistant 响应可以有多个
// function_call，桥接会把这些调用归并进同一条 assistant 消息。若在两个调用之间插
// reasoning item，就会把一轮切成两轮，历史与上游实际产生的内容不再对应。
func synthesizeDeepSeekResponsesReasoningItems(body []byte) ([]byte, bool) {
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return nil, false
	}

	placeholder := `{"type":"reasoning","summary":[{"type":"summary_text","text":"` + deepSeekResponsesReasoningPlaceholderText + `"}]}`
	items := make([]string, 0, len(input.Array())+4)
	changed := false
	// assistantTurnOpen 表示当前已经进入某一轮 assistant 响应（该轮的 reasoning 已
	// 存在，或是前一个工具调用已经开启这一轮）。
	assistantTurnOpen := false
	for _, item := range input.Array() {
		if isDeepSeekResponsesReasoningItem(item) {
			items = append(items, item.Raw)
			assistantTurnOpen = true
			continue
		}
		switch classifyDeepSeekResponsesTurnItem(item) {
		case deepSeekTurnItemAssistantStart, deepSeekTurnItemAssistantContinue:
			// 轮次可能以 assistant 消息开始，也可能直接以工具调用开始（助手只回工具
			// 调用、没有文本）；两种形态都要求这一轮之前有 reasoning item。
			if !assistantTurnOpen {
				items = append(items, placeholder)
				changed = true
			}
			assistantTurnOpen = true
		default:
			assistantTurnOpen = false
		}
		items = append(items, item.Raw)
	}
	if !changed {
		return nil, false
	}
	return replaceOpenAIRawInput(body, input, items), true
}

// deepSeekTurnItemKind 把 input item 归类为「开启一轮 assistant 响应」「继续本轮
// assistant 响应的后续工具调用」或「与本轮无关（用户消息、工具结果等）」。
type deepSeekTurnItemKind int

const (
	deepSeekTurnItemOther deepSeekTurnItemKind = iota
	deepSeekTurnItemAssistantStart
	deepSeekTurnItemAssistantContinue
)

// classifyDeepSeekResponsesTurnItem 判定 item 在 assistant 轮次中的位置。工具输出
// 属于上一轮的结果，会把轮次关闭；工具调用本身延续当前轮次。
func classifyDeepSeekResponsesTurnItem(item gjson.Result) deepSeekTurnItemKind {
	if !item.IsObject() {
		return deepSeekTurnItemOther
	}
	itemType := strings.TrimSpace(item.Get("type").String())
	switch itemType {
	case "message":
		if strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
			return deepSeekTurnItemAssistantStart
		}
		return deepSeekTurnItemOther
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "mcp_tool_call_output", "tool_search_output":
		return deepSeekTurnItemOther
	}
	if isCodexToolCallInputType(itemType) {
		return deepSeekTurnItemAssistantContinue
	}
	return deepSeekTurnItemOther
}
