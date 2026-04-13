package gateway

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpenAIServiceTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "fast", "priority":
		return "priority"
	case "flex":
		return "flex"
	default:
		return ""
	}
}

// wrapAsResponsesAPI 将请求包装为 Responses API 格式（模拟客户端模式）
func wrapAsResponsesAPI(body []byte, model string) ([]byte, error) {
	return wrapAsResponsesAPIWithTier(body, model, "")
}

func wrapAsResponsesAPIWithTier(body []byte, model string, reqServiceTierOverride string) ([]byte, error) {
	// 已是 Responses 格式（有 input 字段），直接补齐默认字段。
	// input 格式规范化（string → list）已在 forwardHTTP 入口统一完成。
	if gjson.GetBytes(body, "input").Exists() {
		return ensureResponsesDefaultsWithTier(body, reqServiceTierOverride), nil
	}

	// Chat Completions 格式（有 messages 字段）→ 转换为 Responses API input
	if gjson.GetBytes(body, "messages").Exists() {
		input, instructions := convertChatMessagesToResponsesInput(gjson.GetBytes(body, "messages").Array())

		wrapped := map[string]any{
			"model":  model,
			"input":  input,
			"stream": true,
			"store":  false,
		}
		if instructions != "" {
			wrapped["instructions"] = instructions
		}
		if effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()); effort != "" {
			wrapped["reasoning"] = map[string]any{
				"effort":  effort,
				"summary": "auto",
			}
		}

		// 转换 tools（Chat Completions → Responses API 格式）
		if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
			wrapped["tools"] = convertChatToolsToResponsesTools(tools.Array())
		}

		// tool_choice：如果历史中有工具调用记录（处于工具循环中），强制 required
		// 避免模型在执行阶段只输出文字确认而不调用工具
		if tc := gjson.GetBytes(body, "tool_choice"); tc.Exists() {
			wrapped["tool_choice"] = normalizeResponsesToolChoice(tc)
		} else if messagesHaveToolCalls(gjson.GetBytes(body, "messages").Array()) {
			wrapped["tool_choice"] = "required"
		}

		out, err := json.Marshal(wrapped)
		if err != nil {
			return nil, err
		}
		return ensureResponsesDefaultsWithTier(out, reqServiceTierOverride), nil
	}

	// 无法识别的格式，原样返回
	return ensureResponsesDefaultsWithTier(body, reqServiceTierOverride), nil
}

func ensureResponsesDefaultsWithTier(body []byte, reqServiceTierOverride string) []byte {
	result := body
	if modified, err := sjson.SetBytes(result, "stream", true); err == nil {
		result = modified
	}
	if modified, err := sjson.SetBytes(result, "store", false); err == nil {
		result = modified
	}
	// ChatGPT codex 端点要求 instructions 字段必须存在
	if !gjson.GetBytes(result, "instructions").Exists() {
		result, _ = sjson.SetBytes(result, "instructions", "")
	}
	if modified, err := sjson.SetBytes(result, "parallel_tool_calls", true); err == nil {
		result = modified
	}
	if gjson.GetBytes(result, "reasoning").Exists() {
		if !gjson.GetBytes(result, "reasoning.summary").Exists() {
			if modified, err := sjson.SetBytes(result, "reasoning.summary", "auto"); err == nil {
				result = modified
			}
		}
	} else if effort := strings.TrimSpace(gjson.GetBytes(result, "reasoning_effort").String()); effort != "" {
		if modified, err := sjson.SetBytes(result, "reasoning.effort", effort); err == nil {
			result = modified
		}
		if modified, err := sjson.SetBytes(result, "reasoning.summary", "auto"); err == nil {
			result = modified
		}
	}
	if modified, err := sjson.SetBytes(result, "include", []string{"reasoning.encrypted_content"}); err == nil {
		result = modified
	}

	// 仅在请求或分组明确指定时才传 service_tier
	if tier := normalizeOpenAIServiceTier(gjson.GetBytes(result, "service_tier").String()); tier != "" {
		result, _ = sjson.SetBytes(result, "service_tier", tier)
	} else if tier := normalizeOpenAIServiceTier(reqServiceTierOverride); tier != "" {
		result, _ = sjson.SetBytes(result, "service_tier", tier)
	} else if gjson.GetBytes(result, "service_tier").Exists() {
		result, _ = sjson.DeleteBytes(result, "service_tier")
	}
	if !gjson.GetBytes(result, "text.verbosity").Exists() {
		result, _ = sjson.SetBytes(result, "text.verbosity", "medium")
	}

	// 剥离 Codex 上游不支持的参数
	for _, field := range []string{
		"context_management",
		"truncation",
		"max_output_tokens",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"user",
	} {
		if gjson.GetBytes(result, field).Exists() {
			result, _ = sjson.DeleteBytes(result, field)
		}
	}

	return result
}

// convertChatMessagesToResponsesInput 将 Chat Completions messages 转换为 Responses API input 列表
// 返回 input 列表和从 system 提取出的 instructions 文本
func convertChatMessagesToResponsesInput(messages []gjson.Result) ([]any, string) {
	var input []any
	var instructionsParts []string
	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "" {
			continue
		}

		switch role {
		case "system":
			// system 消息聚合到 instructions，保留原始语义
			if text := extractChatMessageText(msg); text != "" {
				instructionsParts = append(instructionsParts, text)
			}
			continue

		case "tool":
			// 工具结果消息
			callID := msg.Get("tool_call_id").String()
			if callID == "" {
				continue
			}
			item := map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
			}
			if content := msg.Get("content"); content.Exists() && content.IsArray() {
				var outputParts []map[string]any
				for _, part := range content.Array() {
					ptype := part.Get("type").String()
					switch ptype {
					case "text":
						if text := part.Get("text").String(); text != "" {
							outputParts = append(outputParts, map[string]any{"type": "input_text", "text": text})
						}
					case "image_url":
						url := part.Get("image_url.url").String()
						if url != "" {
							outputParts = append(outputParts, map[string]any{"type": "input_image", "image_url": url})
						}
					}
				}
				if len(outputParts) > 0 {
					item["output"] = outputParts
				} else {
					item["output"] = extractChatMessageText(msg)
				}
			} else {
				item["output"] = extractChatMessageText(msg)
			}
			input = append(input, item)

		case "assistant":
			toolCalls := msg.Get("tool_calls")
			// 文本内容优先输出（可能同时有文本和工具调用）
			if text := extractChatMessageText(msg); text != "" {
				input = append(input, map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []map[string]string{
						{"type": "output_text", "text": text},
					},
				})
			}
			// 工具调用条目
			if toolCalls.Exists() && toolCalls.IsArray() {
				for _, tc := range toolCalls.Array() {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.Get("id").String(),
						"name":      tc.Get("function.name").String(),
						"arguments": tc.Get("function.arguments").String(),
					})
				}
			}

		default:
			// user 及其他角色
			content := extractChatMessageText(msg)
			if content == "" {
				continue
			}
			input = append(input, map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": content},
				},
			})
		}
	}
	return input, strings.Join(instructionsParts, "\n\n")
}

// messagesHaveToolCalls 检查消息历史中是否存在工具调用（判断是否处于工具循环中）
func messagesHaveToolCalls(messages []gjson.Result) bool {
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			return true
		}
	}
	return false
}

// extractChatMessageText 从 Chat Completions 消息中提取文本内容
func extractChatMessageText(msg gjson.Result) string {
	content := msg.Get("content")
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	// 数组格式：提取所有 text 块拼接
	if content.IsArray() {
		var parts []string
		for _, part := range content.Array() {
			if part.Get("type").String() == "text" {
				if t := part.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// convertChatToolsToResponsesTools 将 Chat Completions tools 转为 Responses API tools 格式
func convertChatToolsToResponsesTools(tools []gjson.Result) []any {
	var result []any
	for _, tool := range tools {
		if tool.Get("type").String() != "function" {
			continue
		}
		fn := tool.Get("function")
		if !fn.Exists() {
			continue
		}
		t := map[string]any{
			"type": "function",
			"name": fn.Get("name").String(),
		}
		if desc := fn.Get("description").String(); desc != "" {
			t["description"] = desc
		}
		if params := fn.Get("parameters"); params.Exists() {
			t["parameters"] = fixObjectSchema([]byte(params.Raw))
		}
		if strict := fn.Get("strict"); strict.Exists() && strict.Bool() {
			t["strict"] = true
		}
		result = append(result, t)
	}
	return result
}

// normalizeResponsesToolChoice 将 ChatCompletions 风格 tool_choice 规范化为 Responses 兼容格式
func normalizeResponsesToolChoice(tc gjson.Result) any {
	if !tc.Exists() {
		return nil
	}
	if tc.Type == gjson.String {
		v := strings.TrimSpace(tc.String())
		if v == "" {
			return nil
		}
		switch v {
		case "required":
			return "required"
		case "auto", "none":
			return v
		default:
			return v
		}
	}
	if !tc.IsObject() {
		return json.RawMessage(tc.Raw)
	}

	typeVal := strings.TrimSpace(tc.Get("type").String())
	switch typeVal {
	case "function":
		name := strings.TrimSpace(tc.Get("function.name").String())
		if name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	case "tool":
		name := strings.TrimSpace(tc.Get("name").String())
		if name != "" {
			if name == "web_search" || name == "web_search_20250305" {
				return map[string]any{"type": "web_search"}
			}
			return map[string]any{"type": "function", "name": name}
		}
	case "web_search":
		return map[string]any{"type": "web_search"}
	case "auto", "none", "required":
		return typeVal
	}

	return json.RawMessage(tc.Raw)
}

// fixObjectSchema 递归修复 JSON Schema：为 type=object 但缺少 properties 的节点补充空 properties
func fixObjectSchema(raw []byte) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return json.RawMessage(raw)
	}
	fixSchemaNode(schema)
	fixed, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(fixed)
}

func fixSchemaNode(node map[string]any) {
	// 若 type=object 且没有 properties，补充空 properties
	if t, _ := node["type"].(string); t == "object" {
		if _, has := node["properties"]; !has {
			node["properties"] = map[string]any{}
		}
	}

	// 递归处理 properties 中的每个子 schema
	if props, ok := node["properties"].(map[string]any); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]any); ok {
				fixSchemaNode(sub)
			}
		}
	}

	// 递归处理 items（数组元素 schema）
	if items, ok := node["items"].(map[string]any); ok {
		fixSchemaNode(items)
	}

	// 递归处理 anyOf / oneOf / allOf
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := node[key].([]any); ok {
			for _, v := range arr {
				if sub, ok := v.(map[string]any); ok {
					fixSchemaNode(sub)
				}
			}
		}
	}
}
