package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// Anthropic Messages API 转发入口（纯 gjson/sjson，零 struct）
// ──────────────────────────────────────────────────────

// forwardAnthropicMessage 处理 Anthropic Messages API 请求
// 流程：原始 JSON → 验证 → 模型映射 → 缓存优化 → 一步直转 Responses API → 转发上游
func (g *OpenAIGateway) forwardAnthropicMessage(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account
	body := req.Body

	g.logger.Info("[客户端→Anthropic] 收到请求",
		"model", gjson.GetBytes(body, "model").String(),
		"messages", gjson.GetBytes(body, "messages.#").Int(),
		"tools", gjson.GetBytes(body, "tools.#").Int(),
		"stream", gjson.GetBytes(body, "stream").Bool(),
		"last_msg", truncate(gjson.GetBytes(body, "messages.@last.content").String(), 200),
	)

	// 1. 验证请求（纯 gjson）
	if statusCode, errType, errMsg := validateAnthropicRequestJSON(body); statusCode != 0 {
		if req.Writer != nil {
			writeAnthropicErrorJSON(req.Writer, statusCode, errType, errMsg)
		}
		return &sdk.ForwardResult{StatusCode: statusCode, Duration: time.Since(start)}, nil
	}

	// 2. 同步 model/stream
	if req.Model != "" && gjson.GetBytes(body, "model").String() != req.Model {
		body, _ = sjson.SetBytes(body, "model", req.Model)
	}
	if req.Stream && !gjson.GetBytes(body, "stream").Bool() {
		body, _ = sjson.SetBytes(body, "stream", true)
	}

	// 3. 模型映射
	originalModel := gjson.GetBytes(body, "model").String()
	var mappingEffort string
	if mapping := resolveAnthropicModelMapping(originalModel); mapping != nil {
		g.logger.Info("Anthropic 模型映射",
			"from", originalModel,
			"to", mapping.OpenAIModel,
			"reasoning_effort", mapping.ReasoningEffort)
		body, _ = sjson.SetBytes(body, "model", mapping.OpenAIModel)
		mappingEffort = mapping.ReasoningEffort
	}
	modelName := gjson.GetBytes(body, "model").String()

	// 4. 缓存优化（在原始 Anthropic JSON 上操作）
	body = optimizeCacheControlJSON(body)

	// 5. 一步直转为 Responses API JSON
	responsesBody := convertAnthropicRequestToResponses(body, modelName, mappingEffort)

	// 6. 按需注入 web_search 工具
	if hasWebSearchTool(body) {
		responsesBody = injectWebSearchToolJSON(responsesBody)
	}

	g.logger.Info("[Anthropic→Responses] 转换完成",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"reasoning_effort", gjson.GetBytes(responsesBody, "reasoning.effort").String(),
	)

	// 7. 根据账号类型选择转发方式（统一走 Responses API）
	if account.Credentials["access_token"] != "" {
		return g.forwardAnthropicViaOAuthResponses(ctx, req, responsesBody, originalModel, start)
	}
	return g.forwardAnthropicViaAPIKeyResponses(ctx, req, responsesBody, originalModel, start)
}

// forwardAnthropicViaOAuthResponses OAuth 模式：Responses API SSE → Anthropic SSE
func (g *OpenAIGateway) forwardAnthropicViaOAuthResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	account := req.Account

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatGPTSSEURL, bytes.NewReader(responsesBody))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")
	upstreamReq.Header.Set("Authorization", "Bearer "+account.Credentials["access_token"])
	upstreamReq.Header.Set("OpenAI-Beta", SSEBetaHeader)
	if aid := account.Credentials["chatgpt_account_id"]; aid != "" {
		upstreamReq.Header.Set("ChatGPT-Account-ID", aid)
	}

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return g.handleAnthropicUpstreamError(resp, req.Writer, start)
	}

	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && req.Writer != nil {
		return translateResponsesSSEToAnthropicSSE(ctx, resp, req.Writer, originalModel, req.Body, start)
	}

	// 非流式：聚合 Responses SSE，用 response.completed 做完整回译
	return g.handleAnthropicNonStreamFromResponses(resp, req.Writer, originalModel, req.Body, start)
}

// forwardAnthropicViaAPIKeyResponses API Key 模式：也统一走 Responses API
func (g *OpenAIGateway) forwardAnthropicViaAPIKeyResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	account := req.Account

	targetURL := buildAPIKeyURL(account, "/v1/responses")
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(responsesBody))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	setAuthHeaders(upstreamReq, account)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")
	passHeaders(req.Headers, upstreamReq.Header)
	if isSub2APIAccount(account) {
		// sub2api 仅走 /v1/responses，清理仅官方链路使用的透传头
		upstreamReq.Header.Del("OpenAI-Beta")
		upstreamReq.Header.Del("ChatGPT-Account-ID")
	}

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return g.handleAnthropicUpstreamError(resp, req.Writer, start)
	}

	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && req.Writer != nil {
		return translateResponsesSSEToAnthropicSSE(ctx, resp, req.Writer, originalModel, req.Body, start)
	}

	// 非流式
	return g.handleAnthropicNonStreamFromResponses(resp, req.Writer, originalModel, req.Body, start)
}

// handleAnthropicNonStreamFromResponses 非流式：聚合 Responses SSE → Anthropic JSON
func (g *OpenAIGateway) handleAnthropicNonStreamFromResponses(
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	originalRequest []byte,
	start time.Time,
) (*sdk.ForwardResult, error) {
	wsResult := ParseSSEStream(resp.Body, nil)
	if wsResult.Err != nil {
		return nil, wsResult.Err
	}
	if len(wsResult.CompletedEventRaw) == 0 {
		return nil, fmt.Errorf("未收到 response.completed 事件")
	}

	anthropicJSON := convertResponsesCompletedToAnthropicJSON(wsResult.CompletedEventRaw, originalRequest, model)
	if anthropicJSON == "" {
		return nil, fmt.Errorf("Responses 非流回译失败")
	}

	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicJSON))
	}

	return &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		Model:        gjson.Get(anthropicJSON, "model").String(),
		InputTokens:  wsResult.InputTokens,
		OutputTokens: wsResult.OutputTokens,
		CacheTokens:  wsResult.CacheTokens,
		Duration:     time.Since(start),
	}, nil
}

// handleAnthropicUpstreamError 处理上游错误，转换为 Anthropic 错误格式
func (g *OpenAIGateway) handleAnthropicUpstreamError(
	resp *http.Response,
	w http.ResponseWriter,
	start time.Time,
) (*sdk.ForwardResult, error) {
	body, _ := io.ReadAll(resp.Body)
	statusCode := resp.StatusCode

	g.logger.Error("上游返回错误", "status", statusCode, "body", string(body))

	errMsg := truncate(string(body), 200)
	if msg := extractOpenAIErrorMessage(body); msg != "" {
		errMsg = msg
	}

	errType := anthropicErrorType(statusCode)

	if w != nil {
		writeAnthropicErrorJSON(w, statusCode, errType, errMsg)
	}

	result := &sdk.ForwardResult{
		StatusCode: statusCode,
		Duration:   time.Since(start),
	}

	if statusCode >= 500 || statusCode == 429 {
		return result, fmt.Errorf("上游返回 %d: %s", statusCode, errMsg)
	}

	return result, nil
}

// extractOpenAIErrorMessage 从上游错误响应中提取错误消息（纯 gjson）
func extractOpenAIErrorMessage(body []byte) string {
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	return ""
}
