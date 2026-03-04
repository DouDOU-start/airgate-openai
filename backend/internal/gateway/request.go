package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/DouDOU-start/airgate-openai/backend/resources"
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

// preprocessRequestBody 预处理请求体（同步 model 字段）
func preprocessRequestBody(body []byte, model string) []byte {
	if len(body) == 0 || model == "" {
		return body
	}

	bodyModel := gjson.GetBytes(body, "model").String()
	if bodyModel != model {
		if modified, err := sjson.SetBytes(body, "model", model); err == nil {
			return modified
		}
	}
	return body
}

// wrapAsResponsesAPI 将请求包装为 Responses API 格式（模拟客户端模式）
func wrapAsResponsesAPI(body []byte, model string) ([]byte, error) {
	// 已是 Responses 格式（有 input 字段），只注入 instructions
	if gjson.GetBytes(body, "input").Exists() {
		result := body
		if !gjson.GetBytes(body, "instructions").Exists() {
			if modified, err := sjson.SetBytes(result, "instructions", resources.Instructions); err == nil {
				result = modified
			}
		}
		return result, nil
	}

	// Chat Completions 格式（有 messages 字段）→ 转换为 Responses API input
	if gjson.GetBytes(body, "messages").Exists() {
		input := convertChatMessagesToResponsesInput(gjson.GetBytes(body, "messages").Array())

		wrapped := map[string]any{
			"model":        model,
			"input":        input,
			"instructions": resources.Instructions,
			"stream":       true,
			"store":        false,
		}

		// 转换 tools（Chat Completions → Responses API 格式）
		if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
			wrapped["tools"] = convertChatToolsToResponsesTools(tools.Array())
		}

		// tool_choice：如果历史中有工具调用记录（处于工具循环中），强制 required
		// 避免模型在执行阶段只输出文字确认而不调用工具
		if tc := gjson.GetBytes(body, "tool_choice"); tc.Exists() {
			wrapped["tool_choice"] = json.RawMessage(tc.Raw)
		} else if messagesHaveToolCalls(gjson.GetBytes(body, "messages").Array()) {
			wrapped["tool_choice"] = "required"
		}

		return json.Marshal(wrapped)
	}

	// 无法识别的格式，原样返回
	return body, nil
}

// convertChatMessagesToResponsesInput 将 Chat Completions messages 转换为 Responses API input 列表
func convertChatMessagesToResponsesInput(messages []gjson.Result) []any {
	var input []any
	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "" {
			continue
		}

		switch role {
		case "system":
			// system 消息在 Responses API 里用 instructions，这里跳过（已在外部设置）
			continue

		case "tool":
			// 工具结果消息
			callID := msg.Get("tool_call_id").String()
			if callID == "" {
				continue
			}
			output := extractChatMessageText(msg)
			item := map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  output,
			}
			// Responses API 没有 is_error 字段，但若工具返回错误，在 output 前加标记让模型感知
			// （OpenAI Chat Completions 路径已在 anthropic_convert.go 中单独处理）
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
	return input
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
