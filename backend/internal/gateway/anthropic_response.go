package gateway

import (
	"bufio"
	"context"
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
	CacheTokens               int
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
		template := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`
		// 使用原始 Claude 模型名，让 Claude Code 正确识别模型能力（上下文按钮等）
		modelName := model
		if modelName == "" {
			modelName = root.Get("response.model").String()
		}
		template, _ = sjson.Set(template, "message.model", modelName)
		template, _ = sjson.Set(template, "message.id", root.Get("response.id").String())
		return "event: message_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_part.added":
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		return "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_text.delta":
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.thinking", root.Get("delta").String())
		return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_part.done":
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.added":
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		return "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_text.delta":
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.text", root.Get("delta").String())
		return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.done":
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_item.added":
		item := root.Get("item")
		itemType := item.Get("type").String()
		if itemType == "function_call" {
			state.HasToolCall = true
			state.HasReceivedArgumentsDelta = false

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

			output := "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

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
		inputTokens, outputTokens, cachedTokens := extractResponsesUsage(root.Get("response.usage"))
		state.InputTokens = int(inputTokens)
		state.OutputTokens = int(outputTokens)
		state.CacheTokens = int(cachedTokens)

		// 构建 message_delta
		template := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`

		stopReason := root.Get("response.stop_reason").String()
		if state.HasToolCall {
			template, _ = sjson.Set(template, "delta.stop_reason", "tool_use")
		} else if stopReason == "max_tokens" {
			template, _ = sjson.Set(template, "delta.stop_reason", "max_tokens")
		} else {
			// "stop" / "" / 其他值统一映射为 Anthropic 的 "end_turn"
			template, _ = sjson.Set(template, "delta.stop_reason", "end_turn")
		}

		template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)
		}

		output := "event: message_delta\n" + fmt.Sprintf("data: %s\n\n", template)
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
func convertResponsesCompletedToAnthropicJSON(completedJSON, originalRequest []byte, model string) string {
	root := gjson.ParseBytes(completedJSON)
	if typeStr := root.Get("type").String(); typeStr != "response.completed" && typeStr != "response.done" {
		return ""
	}

	responseData := root.Get("response")
	if !responseData.Exists() {
		return ""
	}

	revNames := buildReverseToolNameMap(originalRequest)

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", responseData.Get("id").String())
	// 始终使用原始 Claude 模型名，让 Claude Code 正确识别模型能力
	out, _ = sjson.Set(out, "model", model)

	inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)
	}

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
							}
						}
					}
				} else if text := content.String(); text != "" {
					block := `{"type":"text","text":""}`
					block, _ = sjson.Set(block, "text", text)
					out, _ = sjson.SetRaw(out, "content.-1", block)
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

	if hasToolCall {
		out, _ = sjson.Set(out, "stop_reason", "tool_use")
	} else if sr := responseData.Get("stop_reason").String(); sr == "max_tokens" {
		out, _ = sjson.Set(out, "stop_reason", "max_tokens")
	} else {
		out, _ = sjson.Set(out, "stop_reason", "end_turn")
	}

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
) (*sdk.ForwardResult, error) {
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
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var streamErr error

	for scanner.Scan() {
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

			// 捕获上游实际模型名（用于计费）
			if rm := gjson.Get(data, "response.model").String(); rm != "" {
				billingModel = rm
			}

			// 检查错误事件 —— 先让 convertResponsesEventToAnthropic 输出错误事件再终止
			if eventType == "response.failed" {
				errMsg := gjson.Get(data, "response.error.message").String()
				if errMsg == "" {
					errMsg = "上游返回 response.failed"
				}
				streamErr = fmt.Errorf("上游错误: %s", errMsg)
			}
			if eventType == "response.incomplete" {
				reason := gjson.Get(data, "response.incomplete_details.reason").String()
				streamErr = fmt.Errorf("响应不完整: %s", reason)
			}
		}

		output := convertResponsesEventToAnthropic(line, originalRequest, state, model)
		if output != "" {
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
		StatusCode:   http.StatusOK,
		InputTokens:  state.InputTokens,
		OutputTokens: state.OutputTokens,
		CacheTokens:  state.CacheTokens,
		Model:        billingModel,
		Duration:     time.Since(start),
	}
	if streamErr != nil {
		result.StatusCode = http.StatusBadGateway
		return result, streamErr
	}
	return result, nil
}
