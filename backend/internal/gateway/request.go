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

// preprocessRequestBody 预处理请求体（同步 model 字段，并做轻量 context guard）
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

	result = applyContextGuard(result, reqPath)
	return result
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
	if isCodexCLI(req.Headers) {
		return buildCodexWSRequest(req.Body, req.Model, session)
	}
	return buildSimulatedWSRequest(req.Body, req.Model, session)
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
		if model != "" {
			reqData["model"] = model
		}
		reqData["store"] = false
		reqData["stream"] = true
		reqData = applySessionFields(reqData, session)
		return json.Marshal(reqData)
	}

	// 否则包装为 response.create
	return wrapResponseCreate(reqData, model, session)
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
	if model != "" {
		createReq["model"] = model
	}
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

	previousResponseID := strings.TrimSpace(gjson.GetBytes(mustJSON(reqData), "previous_response_id").String())
	if previousResponseID == "" && session.PreviousRespID != "" && hasFunctionCallOutput(reqData) {
		reqData["previous_response_id"] = session.PreviousRespID
	}
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

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
