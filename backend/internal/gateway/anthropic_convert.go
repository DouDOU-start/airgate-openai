package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// Anthropic → OpenAI 请求转换
// 参考 AxonHub llm/transformer/anthropic/inbound_convert.go
// ──────────────────────────────────────────────────────

// convertAnthropicToOpenAIWithMapping 将 Anthropic Messages 请求转换为 OpenAI Chat Completions 格式
// mappingEffort 为模型映射注入的 reasoning_effort，优先级高于 thinking 推导
func convertAnthropicToOpenAIWithMapping(req *AnthropicMessageRequest, mappingEffort string) ([]byte, error) {
	result := map[string]any{
		"model": req.Model,
	}

	// max_tokens
	if req.MaxTokens > 0 {
		result["max_tokens"] = req.MaxTokens
	}

	// stream
	if req.Stream != nil && *req.Stream {
		result["stream"] = true
		// OpenAI 需要 stream_options 才能返回 usage
		result["stream_options"] = map[string]any{"include_usage": true}
	}

	// temperature / top_p
	if req.Temperature != nil {
		result["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		result["top_p"] = *req.TopP
	}

	// stop sequences
	if len(req.StopSequences) > 0 {
		if len(req.StopSequences) == 1 {
			result["stop"] = req.StopSequences[0]
		} else {
			result["stop"] = req.StopSequences
		}
	}

	// 转换 messages
	messages, err := convertAnthropicMessagesToOpenAI(req)
	if err != nil {
		return nil, err
	}
	result["messages"] = messages

	// 转换 tools
	if len(req.Tools) > 0 {
		tools := convertAnthropicToolsToOpenAI(req.Tools)
		if len(tools) > 0 {
			result["tools"] = tools
		}
	}

	// 转换 tool_choice
	if req.ToolChoice != nil {
		if tc := convertAnthropicToolChoiceToOpenAI(req.ToolChoice); tc != nil {
			result["tool_choice"] = tc
		}
	}

	// reasoning_effort 优先级：模型映射 > thinking 推导
	if mappingEffort != "" {
		result["reasoning_effort"] = mappingEffort
	} else if req.Thinking != nil {
		switch req.Thinking.Type {
		case "enabled":
			if effort := thinkingBudgetToReasoningEffort(req.Thinking.BudgetTokens); effort != "" {
				result["reasoning_effort"] = effort
			}
		case "adaptive":
			if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
				result["reasoning_effort"] = req.OutputConfig.Effort
			}
		}
	}

	return json.Marshal(result)
}

// convertAnthropicMessagesToOpenAI 转换消息列表
func convertAnthropicMessagesToOpenAI(req *AnthropicMessageRequest) ([]map[string]any, error) {
	var messages []map[string]any

	// 1. 转换 system prompt（多条合并为一条，OpenAI 只支持单条 system 消息）
	if req.System != nil {
		if req.System.Prompt != nil {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": *req.System.Prompt,
			})
		} else if len(req.System.MultiplePrompts) > 0 {
			parts := make([]string, 0, len(req.System.MultiplePrompts))
			for _, prompt := range req.System.MultiplePrompts {
				if prompt.Text != "" {
					parts = append(parts, prompt.Text)
				}
			}
			if len(parts) > 0 {
				messages = append(messages, map[string]any{
					"role":    "system",
					"content": strings.Join(parts, "\n\n"),
				})
			}
		}
	}

	// 2. 转换用户/助手消息
	for _, msg := range req.Messages {
		converted := convertAnthropicMsgToOpenAI(msg)
		messages = append(messages, converted...)
	}

	return messages, nil
}

// convertAnthropicMsgToOpenAI 转换单条 Anthropic 消息为 OpenAI 消息（可能产生多条）
func convertAnthropicMsgToOpenAI(msg AnthropicMsgParam) []map[string]any {
	// 简单字符串 content
	if msg.Content.Content != nil {
		return []map[string]any{{
			"role":    msg.Role,
			"content": *msg.Content.Content,
		}}
	}

	// 多 content block
	if len(msg.Content.MultipleContent) == 0 {
		return nil
	}

	var (
		contentParts []map[string]any
		toolCalls    []map[string]any
		toolResults  []map[string]any
		hasContent   bool
		// thinking 相关
		reasoningContent string
		hasReasoning     bool
	)

	for _, block := range msg.Content.MultipleContent {
		switch block.Type {
		case "text":
			if block.Text != nil {
				contentParts = append(contentParts, map[string]any{
					"type": "text",
					"text": *block.Text,
				})
				hasContent = true
			}

		case "image":
			if block.Source != nil {
				imageURL := convertAnthropicImageToOpenAI(block.Source)
				if imageURL != "" {
					contentParts = append(contentParts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]string{"url": imageURL},
					})
					hasContent = true
				}
			}

		case "thinking":
			if block.Thinking != nil && *block.Thinking != "" {
				reasoningContent = *block.Thinking
				hasReasoning = true
			}

		case "redacted_thinking":
			// 透传（上游可能不支持，但保留数据）

		case "tool_use":
			// web_search 是 Responses API 原生工具，由上游自动处理，不转发给 Chat Completions
			if derefStr(block.Name) == "web_search" {
				continue
			}
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      derefStr(block.Name),
					"arguments": args,
				},
			})
			hasContent = true

		case "tool_result":
			toolUseID := derefStr(block.ToolUseID)
			// 过滤 web_search 的 tool_result（客户端无法执行此工具，错误结果不转发上游）
			// web_search 的 tool_use_id 对应 Responses API item id（ws_ 前缀）
			if strings.HasPrefix(toolUseID, "ws_") {
				continue
			}
			content := extractToolResultContent(block.Content)
			// is_error: true 时在内容前加错误标记，让模型感知工具执行失败
			if block.IsError != nil && *block.IsError {
				content = "[tool_error] " + content
			}
			toolResult := map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			}
			toolResults = append(toolResults, toolResult)
		}
	}

	var result []map[string]any

	// 主消息（user/assistant）
	if hasContent || hasReasoning || len(toolCalls) > 0 {
		mainMsg := map[string]any{
			"role": msg.Role,
		}

		// 单文本块优化
		if len(contentParts) == 1 && contentParts[0]["type"] == "text" && len(toolCalls) == 0 {
			mainMsg["content"] = contentParts[0]["text"]
		} else if len(contentParts) > 0 {
			mainMsg["content"] = contentParts
		}

		if len(toolCalls) > 0 {
			mainMsg["tool_calls"] = toolCalls
			// 如果没有文本内容但有 tool_calls，content 可为 null
			if len(contentParts) == 0 && !hasReasoning {
				mainMsg["content"] = nil
			}
		}

		// reasoning_content 放入消息（如上游支持）
		if hasReasoning {
			mainMsg["reasoning_content"] = reasoningContent
		}

		result = append(result, mainMsg)
	}

	// tool_result 消息（独立的 tool 消息）
	result = append(result, toolResults...)

	return result
}

// convertAnthropicImageToOpenAI 将 Anthropic 图片格式转为 OpenAI image_url
func convertAnthropicImageToOpenAI(source *AnthropicImageSource) string {
	if source == nil {
		return ""
	}
	if source.Type == "base64" && source.Data != "" {
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return fmt.Sprintf("data:%s;base64,%s", mediaType, source.Data)
	}
	if source.URL != "" {
		return source.URL
	}
	return ""
}

// anthropicReqHasWebSearch 检查 Anthropic 请求是否包含 web_search 原生工具
func anthropicReqHasWebSearch(req *AnthropicMessageRequest) bool {
	for _, tool := range req.Tools {
		switch tool.Type {
		case "web_search_20250305", "web_search":
			return true
		}
	}
	return false
}

// injectWebSearchTool 向 Responses API JSON 请求体注入 web_search 工具
// 参考 codex-rs/core/src/client_common.rs ToolSpec::WebSearch
func injectWebSearchTool(body []byte) []byte {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}

	// live 模式（external_web_access: true），支持实时联网搜索
	webSearchTool := map[string]any{
		"type":                "web_search",
		"external_web_access": true,
	}

	// 若已有 tools 列表，追加；否则新建
	if existing, ok := req["tools"].([]any); ok {
		for _, t := range existing {
			if tm, ok := t.(map[string]any); ok && tm["type"] == "web_search" {
				return body
			}
		}
		req["tools"] = append(existing, webSearchTool)
	} else {
		req["tools"] = []any{webSearchTool}
	}

	result, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return result
}

// convertAnthropicToolsToOpenAI 转换 Anthropic tools 为 OpenAI 格式
func convertAnthropicToolsToOpenAI(tools []AnthropicTool) []map[string]any {
	var result []map[string]any
	for _, tool := range tools {
		switch tool.Type {
		case "web_search_20250305", "web_search":
			// web_search 在 Responses API 路径由 injectWebSearchTool 单独处理
			// Chat Completions 路径不支持，跳过
			continue
		case "", "custom":
			openaiTool := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": tool.Name,
				},
			}
			fn := openaiTool["function"].(map[string]any)
			if tool.Description != "" {
				fn["description"] = tool.Description
			}
			if len(tool.InputSchema) > 0 {
				fn["parameters"] = json.RawMessage(tool.InputSchema)
			}
			if tool.Strict != nil && *tool.Strict {
				fn["strict"] = true
			}
			result = append(result, openaiTool)
		default:
			// 跳过其他原生工具
			continue
		}
	}
	return result
}

// convertAnthropicToolChoiceToOpenAI 转换 tool_choice
func convertAnthropicToolChoiceToOpenAI(tc *AnthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		if tc.Name != nil {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": *tc.Name,
				},
			}
		}
	}
	return nil
}

// derefStr 安全解引用字符串指针
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// extractToolResultContent 从 tool_result 的 content 中提取文本
func extractToolResultContent(content *AnthropicMessageContent) string {
	if content == nil {
		return ""
	}
	if content.Content != nil {
		return *content.Content
	}
	if len(content.MultipleContent) > 0 {
		var parts []string
		for _, block := range content.MultipleContent {
			if block.Type == "text" && block.Text != nil {
				parts = append(parts, *block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ──────────────────────────────────────────────────────
// OpenAI → Anthropic 非流式响应转换
// 参考 AxonHub llm/transformer/anthropic/inbound_convert.go convertToAnthropicResponse
// ──────────────────────────────────────────────────────

// convertOpenAIResponseToAnthropic 将 OpenAI Chat Completions 响应转为 Anthropic Message 格式
func convertOpenAIResponseToAnthropic(body []byte, requestModel string) (*AnthropicMessage, error) {
	parsed := gjson.ParseBytes(body)

	resp := &AnthropicMessage{
		ID:   parsed.Get("id").String(),
		Type: "message",
		Role: "assistant",
	}

	// model
	if m := parsed.Get("model").String(); m != "" {
		resp.Model = m
	} else {
		resp.Model = requestModel
	}

	if resp.ID == "" {
		resp.ID = generateMessageID()
	}

	// 处理 choices[0]
	choice := parsed.Get("choices.0")
	if !choice.Exists() {
		return resp, nil
	}

	message := choice.Get("message")
	if !message.Exists() {
		// 可能是 delta 格式
		message = choice.Get("delta")
	}

	if message.Exists() {
		var contentBlocks []AnthropicMessageContentBlock

		// reasoning_content → thinking block（放在最前面）
		if rc := message.Get("reasoning_content").String(); rc != "" {
			contentBlocks = append(contentBlocks, AnthropicMessageContentBlock{
				Type:      "thinking",
				Thinking:  ptrStr(rc),
				Signature: ptrStr(""),
			})
		}

		// content
		contentResult := message.Get("content")
		if contentResult.Exists() {
			if contentResult.Type == gjson.String {
				// 简单字符串内容
				text := contentResult.String()
				if text != "" {
					contentBlocks = append(contentBlocks, AnthropicMessageContentBlock{
						Type: "text",
						Text: ptrStr(text),
					})
				}
			} else if contentResult.IsArray() {
				// 数组内容
				for _, part := range contentResult.Array() {
					partType := part.Get("type").String()
					switch partType {
					case "text":
						if t := part.Get("text").String(); t != "" {
							contentBlocks = append(contentBlocks, AnthropicMessageContentBlock{
								Type: "text",
								Text: ptrStr(t),
							})
						}
					case "image_url":
						if u := part.Get("image_url.url").String(); u != "" {
							contentBlocks = append(contentBlocks, convertOpenAIImageToAnthropic(u))
						}
					}
				}
			}
		}

		// tool_calls → tool_use blocks
		toolCalls := message.Get("tool_calls")
		if toolCalls.Exists() && toolCalls.IsArray() {
			for _, tc := range toolCalls.Array() {
				input := safeJSONRawMessage(tc.Get("function.arguments").String())
				contentBlocks = append(contentBlocks, AnthropicMessageContentBlock{
					Type:  "tool_use",
					ID:    tc.Get("id").String(),
					Name:  ptrStr(tc.Get("function.name").String()),
					Input: input,
				})
			}
		}

		resp.Content = contentBlocks
	}

	// finish_reason → stop_reason
	if fr := choice.Get("finish_reason").String(); fr != "" {
		stopReason := convertFinishReasonToAnthropic(fr)
		resp.StopReason = &stopReason
	}

	// usage
	usage := parsed.Get("usage")
	if usage.Exists() {
		resp.Usage = &AnthropicUsage{
			InputTokens:  int(usage.Get("prompt_tokens").Int()),
			OutputTokens: int(usage.Get("completion_tokens").Int()),
		}
		// 缓存 tokens
		if ct := usage.Get("prompt_tokens_details.cached_tokens").Int(); ct > 0 {
			resp.Usage.CacheReadInputTokens = int(ct)
		}
	}

	return resp, nil
}

// convertOpenAIImageToAnthropic 将 OpenAI image_url 转为 Anthropic image content block
func convertOpenAIImageToAnthropic(imageURL string) AnthropicMessageContentBlock {
	if mediaType, data, ok := parseDataURL(imageURL); ok {
		return AnthropicMessageContentBlock{
			Type: "image",
			Source: &AnthropicImageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      data,
			},
		}
	}
	return AnthropicMessageContentBlock{
		Type: "image",
		Source: &AnthropicImageSource{
			Type: "url",
			URL:  imageURL,
		},
	}
}
