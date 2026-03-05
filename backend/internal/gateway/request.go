package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// Anthropic 请求检测
// ──────────────────────────────────────────────────────

// isAnthropicRequest 检测是否为 Anthropic Messages API 请求
func isAnthropicRequest(req *sdk.ForwardRequest) bool {
	// 1. 通过转发路径检测
	path := extractForwardedPath(req.Headers)
	if strings.Contains(path, "/v1/messages") && !strings.Contains(path, "/chat/completions") {
		return true
	}

	// 2. 通过请求头检测
	if req.Headers != nil && req.Headers.Get("Anthropic-Version") != "" {
		return true
	}

	// 3. 通过请求体特征检测：有 max_tokens + messages 但无 input 字段（区分 Responses API）
	if len(req.Body) > 0 {
		trimmed := bytes.TrimSpace(req.Body)
		hasMaxTokens := gjson.GetBytes(trimmed, "max_tokens").Exists()
		hasMessages := gjson.GetBytes(trimmed, "messages").Exists()
		hasInput := gjson.GetBytes(trimmed, "input").Exists()
		// Anthropic 特有字段
		hasSystem := gjson.GetBytes(trimmed, "system").Exists()

		if hasMaxTokens && hasMessages && !hasInput && hasSystem {
			return true
		}
	}

	return false
}

// ──────────────────────────────────────────────────────
// URL 构建
// ──────────────────────────────────────────────────────

// resolveAPIKeyRoute 解析 API Key 模式的上游请求方法与路径
func resolveAPIKeyRoute(req *sdk.ForwardRequest) (string, string) {
	reqPath := extractForwardedPath(req.Headers)
	reqMethod := strings.ToUpper(strings.TrimSpace(req.Headers.Get("X-Forwarded-Method")))

	// 兜底推断
	if reqPath == "" {
		trimmed := bytes.TrimSpace(req.Body)
		switch {
		case len(trimmed) == 0 && !req.Stream:
			reqPath = "/v1/models"
		case gjson.GetBytes(trimmed, "messages").Exists() && !gjson.GetBytes(trimmed, "input").Exists():
			reqPath = "/v1/chat/completions"
		default:
			reqPath = "/v1/responses"
		}
	}

	if reqMethod == "" {
		if reqPath == "/v1/models" {
			reqMethod = http.MethodGet
		} else {
			reqMethod = http.MethodPost
		}
	}

	switch reqMethod {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		reqMethod = http.MethodPost
	}

	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	return reqMethod, reqPath
}

// extractForwardedPath 从透传头中提取原始请求路径
func extractForwardedPath(headers http.Header) string {
	if headers == nil {
		return ""
	}

	candidates := []string{
		"X-Forwarded-Path",
		"X-Request-Path",
		"X-Original-URI",
		"X-Rewrite-URL",
	}
	for _, key := range candidates {
		raw := strings.TrimSpace(headers.Get(key))
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			if u, err := url.Parse(raw); err == nil {
				path := strings.TrimSpace(u.EscapedPath())
				if path != "" {
					if u.RawQuery != "" {
						return path + "?" + u.RawQuery
					}
					return path
				}
			}
		}
		if strings.HasPrefix(raw, "/") {
			return raw
		}
	}
	return ""
}

// buildAPIKeyURL 根据账号 base_url 和请求路径构建上游 URL
func buildAPIKeyURL(account *sdk.Account, reqPath string) string {
	baseURL := strings.TrimRight(account.Credentials["base_url"], "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	if reqPath == "" {
		reqPath = "/v1/responses"
	}

	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + strings.TrimPrefix(reqPath, "/v1")
	}
	return baseURL + reqPath
}

// ──────────────────────────────────────────────────────
// 请求预处理
// ──────────────────────────────────────────────────────

// preprocessRequestBody 预处理请求体（同步 model 字段 + 上下文预算守卫）
func preprocessRequestBody(body []byte, model, reqPath string) []byte {
	if len(body) == 0 {
		return body
	}

	result := body
	if model != "" {
		bodyModel := gjson.GetBytes(result, "model").String()
		if bodyModel != model {
			if modified, err := sjson.SetBytes(result, "model", model); err == nil {
				result = modified
			}
		}
	}

	if shouldApplyContextGuard(reqPath) {
		result = applyContextWindowGuard(result)
	}
	return result
}

// wrapAsResponsesAPI 将请求包装为 Responses API 格式（模拟客户端模式）
func wrapAsResponsesAPI(body []byte, model string) ([]byte, error) {
	// 已是 Responses 格式（有 input 字段），直接补齐默认字段
	if gjson.GetBytes(body, "input").Exists() {
		return ensureResponsesDefaults(body), nil
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
		return ensureResponsesDefaults(out), nil
	}

	// 无法识别的格式，原样返回
	return ensureResponsesDefaults(body), nil
}

// ensureResponsesDefaults 统一补齐 Responses API 请求默认字段，贴近 cliproxy 的 codex 请求策略
func ensureResponsesDefaults(body []byte) []byte {
	result := body
	if modified, err := sjson.SetBytes(result, "stream", true); err == nil {
		result = modified
	}
	if modified, err := sjson.SetBytes(result, "store", false); err == nil {
		result = modified
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
	return result
}

const (
	contextGuardMinChars         = 12000
	contextGuardReserveMinTokens = 2048
	contextGuardReserveRatio     = 0.15
	contextGuardMaxTailMessages  = 18
)

// shouldApplyContextGuard 仅对易触发上下文膨胀的写请求启用守卫
func shouldApplyContextGuard(reqPath string) bool {
	path := strings.ToLower(strings.TrimSpace(reqPath))
	switch path {
	case "/v1/responses", "/v1/chat/completions", "/v1/messages":
		return true
	default:
		return false
	}
}

// applyContextWindowGuard 在超长上下文时做保守裁剪，降低上游 context window 超限概率
func applyContextWindowGuard(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	window := modelContextWindow(gjson.GetBytes(body, "model").String())
	if window <= 0 {
		window = 200000
	}
	if !shouldCompactByChars(body, window) {
		return body
	}

	trimmed := trimRequestConversation(body)
	if len(trimmed) == 0 {
		return body
	}
	return trimmed
}

func shouldCompactByChars(body []byte, contextWindow int) bool {
	textChars := 0
	if input := gjson.GetBytes(body, "input"); input.Exists() {
		textChars += len(input.Raw)
	}
	if messages := gjson.GetBytes(body, "messages"); messages.Exists() {
		textChars += len(messages.Raw)
	}
	if textChars < contextGuardMinChars {
		return false
	}

	estimatedTokens := textChars / 4
	reserve := int(float64(contextWindow) * contextGuardReserveRatio)
	if reserve < contextGuardReserveMinTokens {
		reserve = contextGuardReserveMinTokens
	}
	return estimatedTokens+reserve > contextWindow
}

func trimRequestConversation(body []byte) []byte {
	if gjson.GetBytes(body, "input").Exists() {
		return trimResponsesInput(body)
	}
	if gjson.GetBytes(body, "messages").Exists() {
		return trimChatMessages(body)
	}
	return body
}

func trimResponsesInput(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	arr := input.Array()
	if len(arr) <= contextGuardMaxTailMessages {
		return body
	}

	start := len(arr) - contextGuardMaxTailMessages
	keptRaw := make([]string, 0, contextGuardMaxTailMessages+1)
	for i, item := range arr {
		if i < start && isSystemInputItem(item) {
			keptRaw = append(keptRaw, item.Raw)
		}
		if i >= start {
			keptRaw = append(keptRaw, item.Raw)
		}
	}
	if len(keptRaw) == len(arr) {
		return body
	}

	replacement := []byte("[" + strings.Join(keptRaw, ",") + "]")
	modified, err := sjson.SetRawBytes(body, "input", replacement)
	if err != nil {
		return body
	}
	return modified
}

func isSystemInputItem(item gjson.Result) bool {
	if item.Get("type").String() != "message" {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	return role == "system" || role == "developer"
}

// getModelMetadataByID 返回网关内置模型元信息，用于 /v1/models 字段补齐与上下文预算估算
func getModelMetadataByID(modelID string) map[string]any {
	window := modelContextWindow(modelID)
	if window <= 0 {
		return nil
	}
	meta := map[string]any{
		"context_length":   window,
		"context_window":   window,
		"max_input_tokens": window,
	}
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "codex-mini-latest", "gpt-4o", "gpt-4o-mini":
		meta["max_output_tokens"] = 16384
	case "gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano":
		meta["max_output_tokens"] = 32768
	case "o3", "o3-pro", "o4-mini", "o3-mini":
		meta["max_output_tokens"] = 32768
	}
	return meta
}

func trimChatMessages(body []byte) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body
	}
	arr := messages.Array()
	if len(arr) <= contextGuardMaxTailMessages {
		return body
	}

	start := len(arr) - contextGuardMaxTailMessages
	keptRaw := make([]string, 0, contextGuardMaxTailMessages+2)
	for i, item := range arr {
		if i < start && isSystemChatMessage(item) {
			keptRaw = append(keptRaw, item.Raw)
		}
		if i >= start {
			keptRaw = append(keptRaw, item.Raw)
		}
	}
	if len(keptRaw) == len(arr) {
		return body
	}

	replacement := []byte("[" + strings.Join(keptRaw, ",") + "]")
	modified, err := sjson.SetRawBytes(body, "messages", replacement)
	if err != nil {
		return body
	}
	return modified
}

func isSystemChatMessage(item gjson.Result) bool {
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	return role == "system" || role == "developer"
}

func modelContextWindow(model string) int {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano":
		return 1047576
	case "gpt-4o", "gpt-4o-mini", "codex-mini-latest":
		return 128000
	case "o3", "o3-pro", "o4-mini", "o3-mini":
		return 200000
	default:
		return 200000
	}
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
// Chat Completions: {"type":"function","function":{"name":"...","description":"...","parameters":{...}}}
// Responses API:    {"type":"function","name":"...","description":"...","parameters":{...}}
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

// fixObjectSchema 递归修复 JSON Schema：为 type=object 但缺少 properties 的节点补充空 properties
// Responses API 比 Chat Completions API 更严格，要求 object schema 必须有 properties
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

// ──────────────────────────────────────────────────────
// WebSocket 请求构建
// ──────────────────────────────────────────────────────

// buildWSRequest 构建 WebSocket response.create 消息
func (g *OpenAIGateway) buildWSRequest(req *sdk.ForwardRequest) ([]byte, error) {
	if isCodexCLI(req.Headers) {
		return buildCodexWSRequest(req.Body, req.Model)
	}
	return buildSimulatedWSRequest(req.Body, req.Model)
}

// buildCodexWSRequest Codex CLI 透传模式
func buildCodexWSRequest(body []byte, model string) ([]byte, error) {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}

	// 如果已有 type=response.create，直接使用
	if t, _ := reqData["type"].(string); t == "response.create" {
		if model != "" {
			reqData["model"] = model
		}
		reqData["store"] = false
		reqData["stream"] = true
		return json.Marshal(reqData)
	}

	// 否则包装为 response.create
	return wrapResponseCreate(reqData, model)
}

// buildSimulatedWSRequest 模拟客户端模式
func buildSimulatedWSRequest(body []byte, model string) ([]byte, error) {
	wrapped, err := wrapAsResponsesAPI(body, model)
	if err != nil {
		return nil, err
	}

	var reqData map[string]any
	if err := json.Unmarshal(wrapped, &reqData); err != nil {
		return nil, fmt.Errorf("解析包装后请求体失败: %w", err)
	}

	return wrapResponseCreate(reqData, model)
}

// wrapResponseCreate 将请求数据包装为 response.create WS 消息
func wrapResponseCreate(data map[string]any, model string) ([]byte, error) {
	createReq := map[string]any{
		"type":   "response.create",
		"stream": true,
		"store":  false,
	}
	for k, v := range data {
		if k != "type" {
			createReq[k] = v
		}
	}
	if model != "" {
		createReq["model"] = model
	}
	return json.Marshal(createReq)
}
