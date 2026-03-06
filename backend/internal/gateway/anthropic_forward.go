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
// 流程：原始 JSON → 验证 → 模型映射 → 一步直转 Responses API → 转发上游（含模型降级重试）
func (g *OpenAIGateway) forwardAnthropicMessage(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
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
	var mapping *anthropicModelMapping
	var mappingEffort string
	if mapping = resolveAnthropicModelMapping(originalModel); mapping != nil {
		g.logger.Info("Anthropic 模型映射",
			"from", originalModel,
			"to", mapping.OpenAIModel,
			"fallback", mapping.FallbackModel,
			"reasoning_effort", mapping.ReasoningEffort)
		body, _ = sjson.SetBytes(body, "model", mapping.OpenAIModel)
		mappingEffort = mapping.ReasoningEffort
	}
	modelName := gjson.GetBytes(body, "model").String()

	// 3.5 简单操作 → Spark 模型加速路由
	// 当最后一轮是 Grep/Glob/Search 等搜索类工具结果处理时，
	// 用 Spark（128K 快速模型）替代主模型，失败时回退到原始映射模型
	// 注：Read/Fetch 返回完整内容可能需要深度分析，不走 Spark
	sparkOverride := false
	originalMappedModel := modelName
	const sparkContextGuard = 100 * 1024 // 100KB body 上限（对应 ~32K tokens，Spark 128K 留余量）
	if sparkTargetModel != "" && sparkTargetModel != modelName &&
		isSparkEligibleToolTurn(body) && len(body) < sparkContextGuard {
		g.logger.Info("简单操作路由到 Spark",
			"original", modelName,
			"spark", sparkTargetModel,
			"body_size", len(body))
		modelName = sparkTargetModel
		mappingEffort = "low"
		sparkOverride = true
	}

	// 4. 一步直转为 Responses API JSON
	// 注: Anthropic 的 cache_control 在转换为 Responses API 后不再适用，
	// Responses API 使用 session_id + include 机制实现缓存，无需预处理断点
	responsesBody := convertAnthropicRequestToResponses(body, modelName, mappingEffort)

	// 5. 按需注入 web_search 工具
	if hasWebSearchTool(body) {
		responsesBody = injectWebSearchToolJSON(responsesBody)
	}

	g.logger.Info("[Anthropic→Responses] 转换完成",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"reasoning_effort", gjson.GetBytes(responsesBody, "reasoning.effort").String(),
		"spark_override", sparkOverride,
	)

	// 6. 转发上游（含模型降级重试）
	fallbackModel := ""
	if sparkOverride {
		// Spark 路由：失败时回退到原始映射模型
		fallbackModel = originalMappedModel
	} else if mapping != nil && mapping.FallbackModel != "" && mapping.FallbackModel != mapping.OpenAIModel {
		fallbackModel = mapping.FallbackModel
	}

	result, err := g.doAnthropicForward(ctx, req, responsesBody, originalModel, fallbackModel, start)
	return result, err
}

// doAnthropicForward 执行 Anthropic 转发，支持模型降级重试
func (g *OpenAIGateway) doAnthropicForward(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	fallbackModel string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	account := req.Account

	// 选择转发方式
	isOAuth := account.Credentials["access_token"] != ""

	// 始终传真实 writer（流式响应必须直接写入客户端）
	// 当有 fallback 时，仅抑制错误响应写入（存入 FallbackErrBody 供降级判断）
	hasFallback := fallbackModel != ""

	var result *sdk.ForwardResult
	var err error
	if isOAuth {
		result, err = g.forwardAnthropicViaOAuthResponses(ctx, req, responsesBody, originalModel, start, req.Writer, hasFallback)
	} else {
		result, err = g.forwardAnthropicViaAPIKeyResponses(ctx, req, responsesBody, originalModel, start, req.Writer, hasFallback)
	}

	// 检查是否需要模型降级
	if fallbackModel != "" && result != nil && result.FallbackErrBody != nil {
		if isModelNotFoundError(result.StatusCode, result.FallbackErrBody) {
			g.logger.Info("模型降级重试",
				"primary", gjson.GetBytes(responsesBody, "model").String(),
				"fallback", fallbackModel,
				"status", result.StatusCode)

			// 替换模型后重试，不再降级
			responsesBody, _ = sjson.SetBytes(responsesBody, "model", fallbackModel)
			fallbackStart := time.Now()
			if isOAuth {
				return g.forwardAnthropicViaOAuthResponses(ctx, req, responsesBody, originalModel, fallbackStart, req.Writer, false)
			}
			return g.forwardAnthropicViaAPIKeyResponses(ctx, req, responsesBody, originalModel, fallbackStart, req.Writer, false)
		}

		// 非模型错误，写回原始错误
		return g.writeAnthropicUpstreamError(req.Writer, result.StatusCode, result.FallbackErrBody, start)
	}

	return result, err
}

// forwardAnthropicViaOAuthResponses OAuth 模式：Responses API SSE → Anthropic SSE
// suppressErrorWrite: 当 true 时，上游错误不写入客户端，存入 FallbackErrBody 供降级判断
func (g *OpenAIGateway) forwardAnthropicViaOAuthResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	start time.Time,
	w http.ResponseWriter,
	suppressErrorWrite bool,
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
		errWriter := w
		if suppressErrorWrite {
			errWriter = nil // fallback 模式：不写入客户端，存入 FallbackErrBody
		}
		return g.handleAnthropicUpstreamErrorWithFallback(resp, errWriter, start)
	}

	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && w != nil {
		return translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, req.Body, start)
	}

	// 非流式：聚合 Responses SSE，用 response.completed 做完整回译
	return g.handleAnthropicNonStreamFromResponses(resp, w, originalModel, req.Body, start)
}

// forwardAnthropicViaAPIKeyResponses API Key 模式：也统一走 Responses API
// suppressErrorWrite: 当 true 时，上游错误不写入客户端，存入 FallbackErrBody 供降级判断
func (g *OpenAIGateway) forwardAnthropicViaAPIKeyResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	start time.Time,
	w http.ResponseWriter,
	suppressErrorWrite bool,
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
		// 模拟 Codex CLI 身份，让 sub2api 跳过 instructions 注入和非 Codex 转换
		setCodexClientHeaders(upstreamReq)
		// 注入 session 缓存标识（如客户端未提供），让 sub2api 实现 sticky session 路由
		if upstreamReq.Header.Get("Session_id") == "" {
			modelName := gjson.GetBytes(responsesBody, "model").String()
			sessionID := deriveSessionID(req.Body, account, modelName)
			responsesBody, _ = sjson.SetBytes(responsesBody, "prompt_cache_key", sessionID)
			upstreamReq.Header.Set("Session_id", sessionID)
			upstreamReq.Header.Set("Conversation_id", sessionID)
			// 用注入后的 body 重建请求体
			upstreamReq.Body = io.NopCloser(bytes.NewReader(responsesBody))
			upstreamReq.ContentLength = int64(len(responsesBody))
		}
	}

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errWriter := w
		if suppressErrorWrite {
			errWriter = nil // fallback 模式：不写入客户端，存入 FallbackErrBody
		}
		return g.handleAnthropicUpstreamErrorWithFallback(resp, errWriter, start)
	}

	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && w != nil {
		return translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, req.Body, start)
	}

	// 非流式
	return g.handleAnthropicNonStreamFromResponses(resp, w, originalModel, req.Body, start)
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

// handleAnthropicUpstreamErrorWithFallback 处理上游错误
// 当 w 为 nil 时（fallback 模式），将错误体存入 FallbackErrBody 供调用方判断是否需要降级
// 当 w 不为 nil 时，直接写入 Anthropic 错误格式
func (g *OpenAIGateway) handleAnthropicUpstreamErrorWithFallback(
	resp *http.Response,
	w http.ResponseWriter,
	start time.Time,
) (*sdk.ForwardResult, error) {
	body, _ := io.ReadAll(resp.Body)
	statusCode := resp.StatusCode

	g.logger.Error("上游返回错误", "status", statusCode, "body", truncate(string(body), 300))

	// fallback 模式：不写入客户端，保存错误体让调用方决定
	if w == nil {
		return &sdk.ForwardResult{
			StatusCode:     statusCode,
			Duration:       time.Since(start),
			FallbackErrBody: body,
		}, nil
	}

	return g.writeAnthropicUpstreamError(w, statusCode, body, start)
}

// writeAnthropicUpstreamError 将上游错误写入客户端（Anthropic 格式）
func (g *OpenAIGateway) writeAnthropicUpstreamError(
	w http.ResponseWriter,
	statusCode int,
	body []byte,
	start time.Time,
) (*sdk.ForwardResult, error) {
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
