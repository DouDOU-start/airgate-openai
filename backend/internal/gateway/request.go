package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
	"github.com/DouDOU-start/airgate-openai/backend/resources"
	sdk "github.com/DouDOU-start/airgate-sdk"
)

// modelMetadataOverrides 仅用于 /v1/models 响应补齐。
// 某些上游模型需要保留历史上下文元数据，但不应出现在插件主动声明的支持模型列表中。
var modelMetadataOverrides = map[string]model.Spec{
	"gpt-4o": {
		Name:            "GPT-4o",
		ContextWindow:   128000,
		MaxOutputTokens: 16384,
	},
}

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

		if hasMaxTokens && hasMessages && !hasInput {
			return true
		}
	}

	return false
}

func isAnthropicCountTokensRequest(req *sdk.ForwardRequest) bool {
	path := extractForwardedPath(req.Headers)
	return strings.Contains(path, "/messages/count_tokens")
}

// ──────────────────────────────────────────────────────
// URL 构建与路由
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

	// 兼容不带 /v1 前缀的路径，自动补全
	if !strings.HasPrefix(reqPath, "/v1/") && reqPath != "/v1" {
		reqPath = "/v1" + reqPath
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

// preprocessRequestBody 统一预处理请求体。
//
// 在 forwardHTTP 入口调用，保证 API Key / OAuth / Anthropic 等所有路径
// 拿到的 body 格式一致。当前处理步骤：
//  1. model 同步（body 中的 model 与核心调度选定的 model 对齐）
//  2. 剔除客户端 previous_response_id（多账号调度下不可靠，会话接续由网关内部管理）
//  3. 上下文守卫（/v1/chat/completions 超长 messages 裁剪）
//  4. input 规范化（/v1/responses 的 string input → list，messages → input 转换）
func preprocessRequestBody(body []byte, model, reqPath string) []byte {
	if len(body) == 0 {
		return body
	}

	// Images API 请求体只有 prompt/n/size/quality/model 等字段，后续的
	// previous_response_id / context_guard / normalizeResponsesInput 对它都应
	// 无作用。/images/edits 更是可能是 multipart/form-data（非 JSON），
	// 提前 bypass 避免 sjson 把 multipart body 损坏成畸形 JSON。
	if isImagesRequest(reqPath) {
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

	// 剔除客户端传入的 previous_response_id。
	// AirGate 在多个上游账号之间做负载均衡，客户端的 previous_response_id
	// 可能指向另一个账号的 response，上游会返回 "not found"。
	// 会话接续由网关内部的 session 机制（OAuth sessionState / Anthropic digestChain）管理。
	result, _ = dropPreviousResponseIDFromJSON(result)

	result = applyContextGuard(result, reqPath)
	result = normalizeResponsesInput(result, reqPath)
	return result
}

// normalizeResponsesInput 对 Responses API 请求的 input 字段做格式规范化。
//
// OpenAI 官方 Responses API 接受两种 input 形式：
//  1. string：简写，等价于单条 user message
//  2. ResponseInputItem 数组：完整形式
//
// 但部分兼容上游（代理、私有部署）只接受 2）。为了让 airgate 对客户端保持宽松
// 同时兼容严格上游，这里把 string 形式自动包装成标准的单条 user input item 列表。
//
// 同时处理一种历史兼容场景：客户端把 Chat Completions 风格的 messages 字段发到
// /v1/responses，本函数把 messages 翻译成 Responses API 的 input 列表（复用
// convertChatMessagesToResponsesInput）。
func normalizeResponsesInput(body []byte, reqPath string) []byte {
	if !strings.Contains(reqPath, "/v1/responses") && !strings.HasSuffix(reqPath, "/responses") {
		return body
	}

	inputNode := gjson.GetBytes(body, "input")

	// 情况 1：input 是 string → 包装成单条 user message item 列表
	if inputNode.Exists() && inputNode.Type == gjson.String {
		text := inputNode.String()
		item := []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": text},
				},
			},
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return body
		}
		if modified, err := sjson.SetRawBytes(body, "input", encoded); err == nil {
			return modified
		}
		return body
	}

	// 情况 2：没有 input 但有 Chat Completions 风格的 messages → 翻译
	if !inputNode.Exists() {
		if msgs := gjson.GetBytes(body, "messages"); msgs.Exists() && msgs.IsArray() {
			input, instructions := convertChatMessagesToResponsesInput(msgs.Array())
			if input == nil {
				input = []any{}
			}
			encoded, err := json.Marshal(input)
			if err != nil {
				return body
			}
			result := body
			if modified, err := sjson.SetRawBytes(result, "input", encoded); err == nil {
				result = modified
			}
			if modified, err := sjson.DeleteBytes(result, "messages"); err == nil {
				result = modified
			}
			if instructions != "" && !gjson.GetBytes(result, "instructions").Exists() {
				if modified, err := sjson.SetBytes(result, "instructions", instructions); err == nil {
					result = modified
				}
			}
			return result
		}
	}

	return body
}

// getModelMetadataByID 返回网关内置模型元信息，用于 /v1/models 字段补齐与上下文预算估算
func getModelMetadataByID(modelID string) map[string]any {
	id := strings.ToLower(strings.TrimSpace(modelID))
	spec, ok := modelMetadataOverrides[id]
	if !ok {
		spec = model.Lookup(id)
	}
	if spec.ContextWindow <= 0 {
		return nil
	}
	meta := map[string]any{
		"context_length":   spec.ContextWindow,
		"context_window":   spec.ContextWindow,
		"max_input_tokens": spec.ContextWindow,
	}
	if spec.MaxOutputTokens > 0 {
		meta["max_output_tokens"] = spec.MaxOutputTokens
	}
	return meta
}

// ──────────────────────────────────────────────────────
// WebSocket 请求构建
// ──────────────────────────────────────────────────────

// buildWSRequest 构建 WebSocket response.create 消息
func (g *OpenAIGateway) buildWSRequest(req *sdk.ForwardRequest, session openAISessionResolution) ([]byte, error) {
	var (
		body []byte
		err  error
	)
	if isCodexCLI(req.Headers) {
		body, err = buildCodexWSRequest(req.Body, req.Model, session)
	} else {
		body, err = buildSimulatedWSRequest(req.Body, req.Model, session)
	}
	if err != nil {
		return nil, err
	}
	// applyForceInstructions 已在 forwardHTTP 入口统一处理
	return body, nil
}

// applyForceInstructions 若请求头中指定了 X-Airgate-Force-Instructions 则强制覆盖 instructions 字段。
// 支持内置别名 "default" / "simple" / "nsfw"，也可直接填入完整 instructions 文本。
func applyForceInstructions(body []byte, headers http.Header) []byte {
	if len(body) == 0 || headers == nil {
		return body
	}
	raw := headers.Get("X-Airgate-Force-Instructions")
	if raw == "" {
		return body
	}
	resolved := resources.ResolveInstructions(raw)
	if modified, err := sjson.SetBytes(body, "instructions", resolved); err == nil {
		return modified
	}
	return body
}

// buildCodexWSRequest Codex CLI 透传模式
func buildCodexWSRequest(body []byte, model string, session openAISessionResolution) ([]byte, error) {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	reqData = applyContinuationState(reqData, session)

	// 如果已有 type=response.create，直接使用
	if t, _ := reqData["type"].(string); t == "response.create" {
		reqData["model"] = resolveEffectiveModel(model, reqData["model"])
		reqData["store"] = false
		reqData["stream"] = true
		reqData = applySessionFields(reqData, session)
		return json.Marshal(reqData)
	}

	// 否则包装为 response.create
	return wrapResponseCreate(reqData, model, session)
}

// resolveEffectiveModel 决定最终送到上游的 model 字段。
// 优先级：显式 reqModel > body 里已有的 model > Codex 兜底默认值。
// 只要候选值不在 model.registry 里（包括空串、"None"、"null"、或者任何不认识的
// 模型名），就直接换成 codexDefaultModel —— 避免把"不支持的模型"推到上游账号，
// 触发 "The 'None' model is not supported..." 这类错误。
func resolveEffectiveModel(reqModel string, existing any) string {
	if model.IsKnown(reqModel) {
		return strings.TrimSpace(reqModel)
	}
	if s, ok := existing.(string); ok && model.IsKnown(s) {
		return strings.TrimSpace(s)
	}
	return codexDefaultModel
}

// buildSimulatedWSRequest 模拟客户端模式
func buildSimulatedWSRequest(body []byte, model string, session openAISessionResolution) ([]byte, error) {
	wrapped, err := wrapAsResponsesAPI(body, model)
	if err != nil {
		return nil, err
	}

	var reqData map[string]any
	if err := json.Unmarshal(wrapped, &reqData); err != nil {
		return nil, fmt.Errorf("解析包装后请求体失败: %w", err)
	}
	reqData = applyContinuationState(reqData, session)

	return wrapResponseCreate(reqData, model, session)
}

// wrapResponseCreate 将请求数据包装为 response.create WS 消息
func wrapResponseCreate(data map[string]any, model string, session openAISessionResolution) ([]byte, error) {
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
	createReq["model"] = resolveEffectiveModel(model, createReq["model"])
	createReq = applySessionFields(createReq, session)
	return json.Marshal(createReq)
}

func applySessionFields(reqData map[string]any, session openAISessionResolution) map[string]any {
	if reqData == nil {
		return reqData
	}
	if session.PromptCacheKey != "" {
		reqData["prompt_cache_key"] = session.PromptCacheKey
	}
	return reqData
}

func applyContinuationState(reqData map[string]any, session openAISessionResolution) map[string]any {
	if reqData == nil {
		return reqData
	}

	// 不再从 session 回填 previous_response_id。
	// 多账号调度下，上一轮 response 可能在另一个账号上，注入后上游会返回 "not found"；
	// 且 function_call_output 自带 call_id，上游可以靠 call_id 匹配，不依赖 previous_response_id。
	// 客户端的 previous_response_id 已在 preprocessRequestBody 统一剔除。
	return reqData
}

func dropPreviousResponseIDFromJSON(body []byte) ([]byte, bool) {
	if len(body) == 0 || !gjson.GetBytes(body, "previous_response_id").Exists() {
		return body, false
	}
	next, err := sjson.DeleteBytes(body, "previous_response_id")
	if err != nil {
		return body, false
	}
	return next, true
}
