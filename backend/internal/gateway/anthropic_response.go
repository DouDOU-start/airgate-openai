package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Responses API → Anthropic SSE 流式转换（轻量状态机）
// 参考 CLIProxyAPI translator/codex/claude/codex_claude_response.go
// ──────────────────────────────────────────────────────

// anthropicStreamState 轻量流式状态
type anthropicStreamState struct {
	HasToolCall               bool
	BlockIndex                int
	HasReceivedArgumentsDelta bool
	InputTokens               int
	OutputTokens              int
	CachedInputTokens         int
	ReasoningOutputTokens     int
	TextBlockOpen             bool              // 当前是否已打开 text content block（用于容错上游跳过 content_part.added 的情况）
	ThinkingBlockOpen         bool              // 当前是否已打开 thinking content block
	reverseNameMap            map[string]string // 缓存 short→original 工具名映射，避免每次事件重建
}

// convertResponsesEventToAnthropic 将单条 Responses API SSE 事件转换为 Anthropic SSE 事件字符串
// model: 回传给客户端的模型名（使用原始 Claude 模型名）
// 返回空字符串表示该事件不需要输出
func convertResponsesEventToAnthropic(rawLine []byte, originalRequest []byte, state *anthropicStreamState, model string) string {
	if len(rawLine) == 0 {
		return ""
	}

	// 提取 data: 行
	data, ok := extractSSEData(string(rawLine))
	if !ok || data == "" || data == "[DONE]" {
		return ""
	}

	root := gjson.Parse(data)
	typeStr := root.Get("type").String()

	switch typeStr {
	case "response.created":
		// message_start 的 usage 必须对齐 Claude 官方 schema：
		// - input_tokens / output_tokens / cache_creation_input_tokens / cache_read_input_tokens：4 个核心字段
		// - cache_creation：嵌套对象（ephemeral_5m / ephemeral_1h），新版 Claude API 新增
		// - service_tier：配额档位，固定 "standard"
		// - server_tool_use：服务端工具调用统计，通常为 null
		// 真实 token 数在 message_delta（response.completed）时下发，这里初始化为 0。
		template := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","server_tool_use":null},"content":[],"stop_reason":null}}`
		// 使用原始 Claude 模型名，让 Claude Code 正确识别模型能力（上下文按钮等）
		modelName := model
		if modelName == "" {
			modelName = root.Get("response.model").String()
		}
		template, _ = sjson.Set(template, "message.model", modelName)
		template, _ = sjson.Set(template, "message.id", normalizeAnthropicMessageID(root.Get("response.id").String()))
		// Claude 官方流式序列：message_start 之后紧跟一个 ping 事件，客户端用它确认连接已建立。
		return "event: message_start\n" + fmt.Sprintf("data: %s\n\n", template) +
			"event: ping\ndata: {\"type\":\"ping\"}\n\n"

	case "response.reasoning_summary_part.added":
		// 若仍有未关闭的 text block，先关闭它
		closePrefix := closeOpenTextBlock(state)
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.ThinkingBlockOpen = true
		return closePrefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_text.delta":
		// 容错：上游若跳过 reasoning_summary_part.added，这里按需补开
		var prefix string
		if !state.ThinkingBlockOpen {
			prefix = closeOpenTextBlock(state)
			startTpl := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
			startTpl, _ = sjson.Set(startTpl, "index", state.BlockIndex)
			prefix += "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", startTpl)
			state.ThinkingBlockOpen = true
		}
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.thinking", root.Get("delta").String())
		return prefix + "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_part.done":
		if !state.ThinkingBlockOpen {
			return ""
		}
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		state.ThinkingBlockOpen = false
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.added":
		// 若仍有未关闭的 thinking block，先关闭
		closePrefix := closeOpenThinkingBlock(state)
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.TextBlockOpen = true
		return closePrefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_text.delta":
		// 容错：上游若跳过 content_part.added，这里按需补开 text block
		var prefix string
		if !state.TextBlockOpen {
			prefix = closeOpenThinkingBlock(state)
			startTpl := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
			startTpl, _ = sjson.Set(startTpl, "index", state.BlockIndex)
			prefix += "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", startTpl)
			state.TextBlockOpen = true
		}
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.text", root.Get("delta").String())
		return prefix + "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.done":
		if !state.TextBlockOpen {
			return ""
		}
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		state.TextBlockOpen = false
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_item.added":
		item := root.Get("item")
		itemType := item.Get("type").String()
		if itemType == "function_call" {
			state.HasToolCall = true
			state.HasReceivedArgumentsDelta = false

			// 工具调用前若仍有未关闭的 text/thinking 内容块，先关闭，保证事件序列成对
			closePrefix := closeOpenTextBlock(state)
			closePrefix += closeOpenThinkingBlock(state)

			// 还原工具短名（懒初始化缓存）
			if state.reverseNameMap == nil {
				state.reverseNameMap = buildReverseToolNameMap(originalRequest)
			}
			name := item.Get("name").String()
			if orig, ok := state.reverseNameMap[name]; ok {
				name = orig
			}

			template := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
			template, _ = sjson.Set(template, "index", state.BlockIndex)
			template, _ = sjson.Set(template, "content_block.id", item.Get("call_id").String())
			template, _ = sjson.Set(template, "content_block.name", name)

			output := closePrefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

			// 紧跟一个空 input_json_delta
			deltaTemplate := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
			deltaTemplate, _ = sjson.Set(deltaTemplate, "index", state.BlockIndex)
			output += "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", deltaTemplate)
			return output
		}
		// web_search_call 等原生工具：忽略
		return ""

	case "response.function_call_arguments.delta":
		state.HasReceivedArgumentsDelta = true
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.partial_json", root.Get("delta").String())
		return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.function_call_arguments.done":
		// 某些模型只发 done 不发 delta，补发完整参数
		if !state.HasReceivedArgumentsDelta {
			if args := root.Get("arguments").String(); args != "" {
				template := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
				template, _ = sjson.Set(template, "index", state.BlockIndex)
				template, _ = sjson.Set(template, "delta.partial_json", args)
				return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)
			}
		}
		return ""

	case "response.output_item.done":
		itemType := root.Get("item.type").String()
		if itemType == "function_call" {
			template := `{"type":"content_block_stop","index":0}`
			template, _ = sjson.Set(template, "index", state.BlockIndex)
			state.BlockIndex++
			return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
		}
		return ""

	case "response.completed", "response.done":
		// 提取 usage
		inputTokens, outputTokens, cachedTokens, reasoningTokens := extractResponsesUsage(root.Get("response.usage"))
		state.InputTokens = int(inputTokens)
		state.OutputTokens = int(outputTokens)
		state.CachedInputTokens = int(cachedTokens)
		state.ReasoningOutputTokens = int(reasoningTokens)

		// 先关闭任何未显式关闭的 text/thinking 内容块，避免 SSE 事件序列不成对
		prefix := closeOpenTextBlock(state)
		prefix += closeOpenThinkingBlock(state)

		// 构建 message_delta
		// usage 字段对齐 Claude 官方完整 schema，否则下游解析器（如 sub2api parseSSEUsagePassthrough）
		// 做 Exists() / >0 判断会丢字段。
		// cache_creation_input_tokens 保持 0：OpenAI Responses API 不区分 cache creation vs read，
		// 所有命中缓存的 prompt 都归在 cached_tokens（→ cache_read_input_tokens）。
		template := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","server_tool_use":null}}`

		var finalStop string
		if state.HasToolCall {
			finalStop = "tool_use"
		} else {
			finalStop = normalizeAnthropicStopReason(root.Get("response.stop_reason").String())
		}
		// 最终再过一层白名单校验，只允许 Anthropic 官方合法枚举
		finalStop = ensureAnthropicStopReason(finalStop)
		template, _ = sjson.Set(template, "delta.stop_reason", finalStop)

		// stop_sequence 若上游带了则透传，不会破坏合法性（null 亦合规）
		if seq := root.Get("response.stop_sequence"); seq.Exists() && seq.Type != gjson.Null {
			template, _ = sjson.SetRaw(template, "delta.stop_sequence", seq.Raw)
		}

		template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
		template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)

		output := prefix + "event: message_delta\n" + fmt.Sprintf("data: %s\n\n", template)
		output += "event: message_stop\n" + "data: {\"type\":\"message_stop\"}\n\n"
		return output

	case "response.failed":
		errMsg := root.Get("response.error.message").String()
		if errMsg == "" {
			errMsg = "upstream response failed"
		}
		errType := mapResponsesErrorType(root.Get("response.error.type").String(), root.Get("response.error.code").String())
		return buildAnthropicStreamError(errType, errMsg)

	case "response.incomplete":
		reason := root.Get("response.incomplete_details.reason").String()
		if reason == "" {
			reason = "unknown"
		}
		return buildAnthropicStreamError("api_error", "response incomplete: "+reason)
	}

	// 忽略未知事件（web_search_call.* 等）
	return ""
}

// closeOpenTextBlock 如果当前有未关闭的 text content block，返回对应的 content_block_stop SSE 片段；否则返回空
func closeOpenTextBlock(state *anthropicStreamState) string {
	if !state.TextBlockOpen {
		return ""
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", state.BlockIndex)
	state.BlockIndex++
	state.TextBlockOpen = false
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

// closeOpenThinkingBlock 如果当前有未关闭的 thinking content block，返回对应的 content_block_stop SSE 片段；否则返回空
func closeOpenThinkingBlock(state *anthropicStreamState) string {
	if !state.ThinkingBlockOpen {
		return ""
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", state.BlockIndex)
	state.BlockIndex++
	state.ThinkingBlockOpen = false
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

// normalizeAnthropicMessageID 把 OpenAI Responses API 的 `resp_...` id 规范化为 Anthropic 风格的 `msg_...`。
// Anthropic 官方 message id 固定使用 `msg_` 前缀，部分 SDK / 下游消费方会以此为前缀做类型识别。
// 保持后缀不变，确保和 Core 侧的请求追踪 ID 仍能对应。
func normalizeAnthropicMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	if strings.HasPrefix(id, "resp_") {
		return "msg_" + strings.TrimPrefix(id, "resp_")
	}
	// 未知前缀兜底：直接加 msg_ 前缀，避免返回空或破坏下游解析
	return "msg_" + id
}

// buildAnthropicStreamError 构建 Anthropic SSE 错误事件
// errType: Anthropic 错误类型（invalid_request_error, rate_limit_error, api_error 等）
func buildAnthropicStreamError(errType, message string) string {
	if errType == "" {
		errType = "api_error"
	}
	template := `{"type":"error","error":{"type":"","message":""}}`
	template, _ = sjson.Set(template, "error.type", errType)
	template, _ = sjson.Set(template, "error.message", message)
	return "event: error\n" + fmt.Sprintf("data: %s\n\n", template)
}

// mapResponsesErrorType 将 Responses API 错误类型映射为 Anthropic 错误类型
func mapResponsesErrorType(errType, errCode string) string {
	errType = strings.ToLower(strings.TrimSpace(errType))
	errCode = strings.ToLower(strings.TrimSpace(errCode))

	switch errType {
	case "invalid_request_error":
		return "invalid_request_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "authentication_error":
		return "authentication_error"
	case "not_found_error":
		return "not_found_error"
	}

	// 通过 code 推断类型
	switch errCode {
	case "context_length_exceeded", "max_tokens_exceeded", "input_too_long":
		return "invalid_request_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	}

	return "api_error"
}

// ──────────────────────────────────────────────────────
// 非流式：Responses completed → Anthropic JSON
// ──────────────────────────────────────────────────────

// convertResponsesCompletedToAnthropicJSON 将 Responses completed 事件转为 Anthropic 非流式 JSON 响应
//
// 上游行为说明：
//   - 有的上游（如官方 Responses API）会在 response.completed 事件中带上完整的
//     response.output[] 数组，包含所有 message/reasoning/function_call 内容。
//   - 有的上游（如 ChatGPT WebSocket 会话）只把 output_text/reasoning 通过 delta 事件流式下发，
//     而 response.completed 只带 metadata（id / model / usage），output[] 缺失或为空。
//
// 因此除了尝试从 completed 事件解析 output[] 之外，还必须回退到 ParseSSEStream 聚合出的
// wsResult.Text / wsResult.Reasoning / wsResult.ToolUses，才能保证 Anthropic 客户端
// 拿到非空的 content 数组。
func convertResponsesCompletedToAnthropicJSON(
	completedJSON, originalRequest []byte,
	model string,
	wsResult *WSResult,
) string {
	root := gjson.ParseBytes(completedJSON)
	if typeStr := root.Get("type").String(); typeStr != "response.completed" && typeStr != "response.done" {
		return ""
	}

	responseData := root.Get("response")
	if !responseData.Exists() {
		return ""
	}

	revNames := buildReverseToolNameMap(originalRequest)

	// usage 对齐 Claude 官方完整 schema：4 个 token 字段 + cache_creation 嵌套对象 + service_tier + server_tool_use
	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","server_tool_use":null}}`
	out, _ = sjson.Set(out, "id", normalizeAnthropicMessageID(responseData.Get("id").String()))
	// 始终使用原始 Claude 模型名，让 Claude Code 正确识别模型能力
	out, _ = sjson.Set(out, "model", model)

	inputTokens, outputTokens, cachedTokens, _ := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)

	hasThinking := false
	hasText := false
	hasToolCall := false

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		for _, item := range output.Array() {
			switch item.Get("type").String() {
			case "reasoning":
				thinking := collectReasoningText(item)
				if thinking != "" {
					block := `{"type":"thinking","thinking":""}`
					block, _ = sjson.Set(block, "thinking", thinking)
					out, _ = sjson.SetRaw(out, "content.-1", block)
					hasThinking = true
				}
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "output_text" {
							if text := part.Get("text").String(); text != "" {
								block := `{"type":"text","text":""}`
								block, _ = sjson.Set(block, "text", text)
								out, _ = sjson.SetRaw(out, "content.-1", block)
								hasText = true
							}
						}
					}
				} else if text := content.String(); text != "" {
					block := `{"type":"text","text":""}`
					block, _ = sjson.Set(block, "text", text)
					out, _ = sjson.SetRaw(out, "content.-1", block)
					hasText = true
				}
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", item.Get("call_id").String())
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
			}
		}
	}

	// 回退：completed 事件没带完整 output 时，用 ParseSSEStream 聚合的 delta 内容补齐
	if wsResult != nil {
		if !hasThinking && wsResult.Reasoning != "" {
			block := `{"type":"thinking","thinking":""}`
			block, _ = sjson.Set(block, "thinking", wsResult.Reasoning)
			out, _ = sjson.SetRaw(out, "content.-1", block)
			hasThinking = true
		}
		if !hasText && wsResult.Text != "" {
			block := `{"type":"text","text":""}`
			block, _ = sjson.Set(block, "text", wsResult.Text)
			out, _ = sjson.SetRaw(out, "content.-1", block)
			hasText = true
		}
		if !hasToolCall && len(wsResult.ToolUses) > 0 {
			for _, tu := range wsResult.ToolUses {
				name := ""
				if tu.Name != nil {
					name = *tu.Name
				}
				if original, ok := revNames[name]; ok {
					name = original
				}
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", tu.ID)
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if len(tu.Input) > 0 && gjson.ValidBytes(tu.Input) {
					if parsed := gjson.ParseBytes(tu.Input); parsed.IsObject() {
						inputRaw = parsed.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
				hasToolCall = true
			}
		}
	}

	// 如果最终还是没有任何内容块，至少塞一个空 text block，避免客户端 SDK 访问 content[0] 崩溃
	if !hasThinking && !hasText && !hasToolCall {
		out, _ = sjson.SetRaw(out, "content.-1", `{"type":"text","text":""}`)
	}

	var finalStop string
	if hasToolCall {
		finalStop = "tool_use"
	} else {
		finalStop = normalizeAnthropicStopReason(responseData.Get("stop_reason").String())
	}
	out, _ = sjson.Set(out, "stop_reason", ensureAnthropicStopReason(finalStop))

	if stopSeq := responseData.Get("stop_sequence"); stopSeq.Exists() && stopSeq.Type != gjson.Null {
		out, _ = sjson.SetRaw(out, "stop_sequence", stopSeq.Raw)
	}

	return out
}

// collectReasoningText 从 reasoning output item 中收集思考文本
func collectReasoningText(item gjson.Result) string {
	var b strings.Builder
	if summary := item.Get("summary"); summary.Exists() {
		if summary.IsArray() {
			for _, part := range summary.Array() {
				if txt := part.Get("text"); txt.Exists() {
					b.WriteString(txt.String())
				} else {
					b.WriteString(part.String())
				}
			}
		} else {
			b.WriteString(summary.String())
		}
	}
	if b.Len() == 0 {
		if content := item.Get("content"); content.Exists() {
			if content.IsArray() {
				for _, part := range content.Array() {
					if txt := part.Get("text"); txt.Exists() {
						b.WriteString(txt.String())
					} else {
						b.WriteString(part.String())
					}
				}
			} else {
				b.WriteString(content.String())
			}
		}
	}
	return b.String()
}

// ──────────────────────────────────────────────────────
// SSE 流转换入口
// ──────────────────────────────────────────────────────

// translateResponsesSSEToAnthropicSSE 读取上游 Responses API SSE 并翻译为 Anthropic SSE 写回客户端
// model: 原始 Claude 模型名（写入客户端响应体）
// mappedModel: 映射后的 GPT 模型名（写入 result.Model 供 Core 计费）
func translateResponsesSSEToAnthropicSSE(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	mappedModel string,
	originalRequest []byte,
	start time.Time,
	session openAISessionResolution,
) (*sdk.ForwardResult, error) {
	setAnthropicStyleResponseHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	state := &anthropicStreamState{}
	// billingModel 用于 Core 计费，优先使用映射后的 GPT 模型名
	billingModel := mappedModel

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)

	var streamErr error
	var firstTokenMs int64
	var serviceTier string // 从上游 response.completed 事件中提取
	skipCurrentOutput := false
	firstTokenRecorded := false

	for scanner.Scan() {
		skipCurrentOutput = false
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			goto done
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// 记录结构性事件
		if data, ok := extractSSEData(string(line)); ok && data != "" && data != "[DONE]" {
			eventType := gjson.Get(data, "type").String()
			if eventType != "response.output_text.delta" &&
				eventType != "response.reasoning_summary_text.delta" &&
				eventType != "response.function_call_arguments.delta" {
				slog.Debug("[上游SSE]", "type", eventType, "data", truncate(data, 300))
			}
			// 大事件诊断：上游单行 SSE 超阈值时打印 type 与长度，便于追踪触发 gRPC 上限的源头。
			if len(line) >= largeSSEEventThreshold {
				slog.Warn("[上游SSE 大事件]",
					"type", eventType,
					"line_bytes", len(line),
					"response_id", gjson.Get(data, "response.id").String(),
				)
			}

			// 捕获上游实际模型名（用于计费）
			if rm := gjson.Get(data, "response.model").String(); rm != "" {
				billingModel = rm
			}
			if session.SessionKey != "" {
				if responseID := gjson.Get(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
					updateSessionStateResponseID(session.SessionKey, responseID)
				}
			}
			if eventType == "response.completed" || eventType == "response.done" {
				// 从上游 response.completed 事件中提取 service_tier
				if tier := normalizeOpenAIServiceTier(gjson.Get(data, "response.service_tier").String()); tier != "" {
					serviceTier = tier
				}
				usageNode := gjson.Get(data, "response.usage")
				slog.Info("[Anthropic←Responses] 上游 usage",
					"session", session.SessionKey,
					"response_id", gjson.Get(data, "response.id").String(),
					"usage_raw", usageNode.Raw,
					"input_tokens", usageNode.Get("input_tokens").Int(),
					"cached_tokens", usageNode.Get("input_tokens_details.cached_tokens").Int(),
					"output_tokens", usageNode.Get("output_tokens").Int(),
					"response_model", gjson.Get(data, "response.model").String(),
				)
				appendCacheDebugLog(
					"anthropic_usage",
					"session", session.SessionKey,
					"response_id", gjson.Get(data, "response.id").String(),
					"response_model", gjson.Get(data, "response.model").String(),
					"input_tokens", usageNode.Get("input_tokens").Int(),
					"cached_tokens", usageNode.Get("input_tokens_details.cached_tokens").Int(),
					"output_tokens", usageNode.Get("output_tokens").Int(),
					"usage_raw", usageNode.Raw,
				)
			}

			// 检查错误事件 —— 先让 convertResponsesEventToAnthropic 输出错误事件再终止
			if eventType == "response.failed" {
				if failure := classifyResponsesFailure([]byte(data)); failure != nil {
					streamErr = failure
					skipCurrentOutput = failure.isContinuationAnchorError()
				} else {
					errMsg := gjson.Get(data, "response.error.message").String()
					if errMsg == "" {
						errMsg = "上游返回 response.failed"
					}
					streamErr = fmt.Errorf("上游错误: %s", errMsg)
				}
			}
			if eventType == "response.incomplete" {
				reason := gjson.Get(data, "response.incomplete_details.reason").String()
				streamErr = fmt.Errorf("响应不完整: %s", reason)
			}
		}

		output := ""
		if !skipCurrentOutput {
			output = convertResponsesEventToAnthropic(line, originalRequest, state, model)
		}
		if output != "" {
			// 大事件诊断：翻译后的单条输出超阈值时打印源 type 与长度。
			if len(output) >= largeSSEEventThreshold {
				srcType := ""
				if data, ok := extractSSEData(string(line)); ok {
					srcType = gjson.Get(data, "type").String()
				}
				slog.Warn("[Anthropic SSE 大事件]",
					"src_type", srcType,
					"output_bytes", len(output),
				)
			}
			// 记录首 token 延迟（首次产生有效输出事件）
			if !firstTokenRecorded {
				firstTokenMs = time.Since(start).Milliseconds()
				firstTokenRecorded = true
			}
			_, _ = fmt.Fprint(w, output)
			if flusher != nil {
				flusher.Flush()
			}
		}

		// 错误事件已输出给客户端，现在终止流
		if streamErr != nil {
			goto done
		}
	}

done:
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	result := &sdk.ForwardResult{
		StatusCode:            http.StatusOK,
		InputTokens:           state.InputTokens,
		OutputTokens:          state.OutputTokens,
		CachedInputTokens:     state.CachedInputTokens,
		ReasoningOutputTokens: state.ReasoningOutputTokens,
		ServiceTier:           serviceTier,
		Model:                 billingModel,
		Duration:              time.Since(start),
		FirstTokenMs:          firstTokenMs,
	}
	if streamErr != nil {
		var failure *responsesFailureError
		if errors.As(streamErr, &failure) {
			result.StatusCode = failure.StatusCode
			result.AccountStatus = failure.AccountStatus
			result.ErrorMessage = failure.Message
			result.RetryAfter = failure.RetryAfter
			if failure.shouldReturnClientError() {
				return result, nil
			}
			return result, streamErr
		}
		result.StatusCode = http.StatusBadGateway
		return result, streamErr
	}
	fillCost(result)
	return result, nil
}
