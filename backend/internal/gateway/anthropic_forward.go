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
		mappingEffort = "medium"
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

	// 5.5 Spark 路由覆盖 verbosity（搜索结果只需简短决策）
	if sparkOverride {
		responsesBody, _ = sjson.SetBytes(responsesBody, "text.verbosity", "low")
	}

	g.logger.Info("[Anthropic→Responses] 转换完成",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"reasoning_effort", gjson.GetBytes(responsesBody, "reasoning.effort").String(),
		"verbosity", gjson.GetBytes(responsesBody, "text.verbosity").String(),
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

	return g.doAnthropicForward(ctx, req, responsesBody, originalModel, fallbackModel, start)
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
	hasFallback := fallbackModel != ""

	// 第一次转发（有 fallback 时抑制错误写入客户端）
	result, errBody, err := g.forwardAnthropicResponses(ctx, req, responsesBody, originalModel, start, req.Writer, hasFallback)
	if err != nil {
		return result, err
	}

	// 检查是否需要模型降级
	if hasFallback && errBody != nil {
		if isModelNotFoundError(result.StatusCode, errBody) {
			g.logger.Info("模型降级重试",
				"primary", gjson.GetBytes(responsesBody, "model").String(),
				"fallback", fallbackModel,
				"status", result.StatusCode)

			// 替换模型后重试，不再降级
			responsesBody, _ = sjson.SetBytes(responsesBody, "model", fallbackModel)
			fallbackStart := time.Now()
			result, _, err = g.forwardAnthropicResponses(ctx, req, responsesBody, originalModel, fallbackStart, req.Writer, false)
			return result, err
		}

		// 非模型错误，写回原始错误
		return g.writeAnthropicUpstreamError(req.Writer, result.StatusCode, errBody, start)
	}

	return result, nil
}

// ──────────────────────────────────────────────────────
// 统一的 Anthropic → Responses API 转发（合并 OAuth/APIKey 路径）
// ──────────────────────────────────────────────────────

// forwardAnthropicResponses 统一的 Anthropic 转发函数
// suppressErrorWrite: true 时上游错误不写入客户端，通过 errBody 返回供降级判断
// 返回值: result, errBody（仅 suppress 时非 nil）, error
func (g *OpenAIGateway) forwardAnthropicResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	start time.Time,
	w http.ResponseWriter,
	suppressErrorWrite bool,
) (*sdk.ForwardResult, []byte, error) {
	account := req.Account

	// 构建上游 HTTP 请求（OAuth/APIKey 差异化处理）
	upstreamReq, responsesBody, err := g.buildAnthropicUpstreamRequest(ctx, req, account, responsesBody)
	if err != nil {
		return nil, nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 发送请求
	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	// 错误处理
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		g.logger.Error("上游返回错误", "status", resp.StatusCode, "body", truncate(string(body), 300))

		if suppressErrorWrite {
			// fallback 模式：不写入客户端，返回错误体供调用方判断
			return &sdk.ForwardResult{
				StatusCode:    resp.StatusCode,
				Duration:      time.Since(start),
				AccountStatus: accountStatusFromCode(resp.StatusCode),
				RetryAfter:    extractRetryAfterHeader(resp.Header),
			}, body, nil
		}
		result, err := g.writeAnthropicUpstreamError(w, resp.StatusCode, body, start)
		return result, nil, err
	}

	// 流式 / 非流式响应处理
	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && w != nil {
		result, err := translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, req.Body, start)
		return result, nil, err
	}

	// 非流式：聚合 Responses SSE → Anthropic JSON
	result, err := g.handleAnthropicNonStreamFromResponses(resp, w, originalModel, req.Body, start)
	return result, nil, err
}

// buildAnthropicUpstreamRequest 构建 Anthropic 转发的上游 HTTP 请求
// 根据 OAuth/APIKey 设置不同的 URL、认证头和特殊处理
func (g *OpenAIGateway) buildAnthropicUpstreamRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	account *sdk.Account,
	responsesBody []byte,
) (*http.Request, []byte, error) {
	isOAuth := account.Credentials["access_token"] != ""

	// 确定目标 URL
	var targetURL string
	if isOAuth {
		targetURL = ChatGPTSSEURL
	} else {
		targetURL = buildAPIKeyURL(account, "/v1/responses")
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(responsesBody))
	if err != nil {
		return nil, responsesBody, err
	}

	// 公共头
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")

	if isOAuth {
		// OAuth 模式：手动设置认证头
		upstreamReq.Header.Set("Authorization", "Bearer "+account.Credentials["access_token"])
		upstreamReq.Header.Set("OpenAI-Beta", SSEBetaHeader)
		if aid := account.Credentials["chatgpt_account_id"]; aid != "" {
			upstreamReq.Header.Set("ChatGPT-Account-ID", aid)
		}
	} else {
		// API Key 模式
		setAuthHeaders(upstreamReq, account)
		passHeaders(req.Headers, upstreamReq.Header)

		// sub2api 特殊处理
		if isSub2APIAccount(account) {
			upstreamReq.Header.Del("OpenAI-Beta")
			upstreamReq.Header.Del("ChatGPT-Account-ID")
			setCodexClientHeaders(upstreamReq)

			// 注入 session 缓存标识
			responsesBody = injectSub2APISession(upstreamReq, responsesBody, account)
		}
	}

	return upstreamReq, responsesBody, nil
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

// ──────────────────────────────────────────────────────
// 错误处理
// ──────────────────────────────────────────────────────

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
		StatusCode:    statusCode,
		Duration:      time.Since(start),
		AccountStatus: accountStatusFromCode(statusCode),
	}

	if statusCode >= 500 || statusCode == 429 {
		if statusCode == 429 {
			result.RetryAfter = parseRetryDelay(errMsg)
		}
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

