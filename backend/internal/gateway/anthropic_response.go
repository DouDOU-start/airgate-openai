package gateway

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// Responses API → Anthropic SSE 流式转换（轻量状态机）
// 参考 CLIProxyAPI translator/codex/claude/codex_claude_response.go
// ──────────────────────────────────────────────────────

// anthropicStreamState 轻量流式状态（从 20+ 字段缩减为 4 个核心字段）
type anthropicStreamState struct {
	HasToolCall               bool
	BlockIndex                int
	HasReceivedArgumentsDelta bool
	InputTokens               int
	OutputTokens              int
	CacheTokens               int
}

// convertResponsesEventToAnthropic 将单条 Responses API SSE 事件转换为 Anthropic SSE 事件字符串
// 返回空字符串表示该事件不需要输出
func convertResponsesEventToAnthropic(rawLine []byte, originalRequest []byte, state *anthropicStreamState) string {
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
		template, _ = sjson.Set(template, "message.model", root.Get("response.model").String())
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

			// 还原工具短名
			name := item.Get("name").String()
			rev := buildReverseMapFromAnthropicOriginalShortToOriginal(originalRequest)
			if orig, ok := rev[name]; ok {
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
		} else if stopReason == "max_tokens" || stopReason == "stop" {
			template, _ = sjson.Set(template, "delta.stop_reason", stopReason)
		} else {
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
		// 转为错误日志，不生成 Anthropic 事件
		return ""

	case "response.incomplete":
		return ""
	}

	// 忽略未知事件（web_search_call.* 等）
	return ""
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

	revNames := buildReverseMapFromAnthropicOriginalShortToOriginal(originalRequest)

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", responseData.Get("id").String())
	if m := responseData.Get("model").String(); m != "" {
		out, _ = sjson.Set(out, "model", m)
	} else {
		out, _ = sjson.Set(out, "model", model)
	}

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

	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		out, _ = sjson.Set(out, "stop_reason", stopReason.String())
	} else if hasToolCall {
		out, _ = sjson.Set(out, "stop_reason", "tool_use")
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
func translateResponsesSSEToAnthropicSSE(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	model string,
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
	responseModel := model

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

			// 捕获模型名
			if rm := gjson.Get(data, "response.model").String(); rm != "" {
				responseModel = rm
			}

			// 检查错误事件
			if eventType == "response.failed" {
				errMsg := gjson.Get(data, "response.error.message").String()
				if errMsg == "" {
					errMsg = "上游返回 response.failed"
				}
				streamErr = fmt.Errorf("上游错误: %s", errMsg)
				goto done
			}
			if eventType == "response.incomplete" {
				reason := gjson.Get(data, "response.incomplete_details.reason").String()
				streamErr = fmt.Errorf("响应不完整: %s", reason)
				goto done
			}
		}

		output := convertResponsesEventToAnthropic(line, originalRequest, state)
		if output != "" {
			_, _ = fmt.Fprint(w, output)
			if flusher != nil {
				flusher.Flush()
			}
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
		Model:        responseModel,
		Duration:     time.Since(start),
	}
	if streamErr != nil {
		result.StatusCode = http.StatusBadGateway
		return result, streamErr
	}
	return result, nil
}
