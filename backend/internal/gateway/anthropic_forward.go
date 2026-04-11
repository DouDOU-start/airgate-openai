package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Anthropic Messages API 转发入口（纯 gjson/sjson，零 struct）
// ──────────────────────────────────────────────────────

// forwardAnthropicMessage 处理 Anthropic Messages API 请求
// 流程：原始 JSON → 验证 → 模型映射 → 一步直转 Responses API → 转发上游（含模型降级重试）
func (g *OpenAIGateway) forwardAnthropicMessage(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	body := req.Body
	strategy := resolveAnthropicUpstreamStrategy(req.Account)
	session := resolveOpenAISession(req.Headers, req.Body)
	session.DigestChain = buildAnthropicDigestChain(body)
	if session.SessionKey == "" {
		if reusedSessionID, matchedChain, ok := findAnthropicDigestSession(req.Account.ID, session.DigestChain); ok {
			session.SessionID = reusedSessionID
			session.SessionKey = sessionStateKeyFromValues(reusedSessionID, "", "")
			session.MatchedDigest = matchedChain
			session.FromStoredState = true
			session.SessionSource = "anthropic_digest_match"
		} else if session.DigestChain != "" {
			session.SessionID = deterministicUUIDFromSeed(fmt.Sprintf("anthropic:%d:%d:%s", req.Account.ID, time.Now().UnixNano(), session.DigestChain))
			session.SessionKey = sessionStateKeyFromValues(session.SessionID, "", "")
			session.SessionSource = "anthropic_digest_new"
		}
	}
	updateSessionStateFromRequest(session)

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
		mappingEffort = mapping.ReasoningEffort
		body, _ = sjson.SetBytes(body, "model", mapping.OpenAIModel)
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
	replaySourceBody, replayTrimmed := applyAnthropicFullReplayGuard(body)
	fullResponsesBody := convertAnthropicRequestToResponses(replaySourceBody, modelName, mappingEffort)
	responsesBody := fullResponsesBody
	requestMode := "full_replay"
	requestReason := "no_session_anchor"
	if session.SessionKey != "" {
		requestReason = "session_anchor_present"
	}
	if strategy.allowsContinuation() {
		if continuationBody, ok := convertAnthropicRequestToResponsesContinuation(body, modelName, mappingEffort, session.PreviousRespID); ok {
			responsesBody = continuationBody
			requestMode = "continuation"
			requestReason = "previous_response_id_available"
		} else if session.PreviousRespID != "" {
			requestReason = "previous_response_id_unusable"
		}
	} else if session.PreviousRespID != "" {
		requestReason = "continuation_disabled"
	}
	if requestMode == "full_replay" && replayTrimmed {
		requestReason = "history_trimmed"
	}

	// 5. 按需注入 web_search 工具
	responsesBody = finalizeAnthropicResponsesBody(responsesBody, body, req.Headers.Get("X-Airgate-Service-Tier"), sparkOverride)
	fullResponsesBody = finalizeAnthropicResponsesBody(fullResponsesBody, body, req.Headers.Get("X-Airgate-Service-Tier"), sparkOverride)
	responsesBody = injectAnthropicPromptCacheKey(responsesBody, strategy, session)
	fullResponsesBody = injectAnthropicPromptCacheKey(fullResponsesBody, strategy, session)

	g.logger.Info("[Anthropic→Responses] 转换完成",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"reasoning_effort", gjson.GetBytes(responsesBody, "reasoning.effort").String(),
		"client_effort_raw", gjson.GetBytes(body, "output_config.effort").String(),
		"client_thinking_type", gjson.GetBytes(body, "thinking.type").String(),
		"client_thinking_budget", gjson.GetBytes(body, "thinking.budget_tokens").Int(),
		"client_max_tokens", gjson.GetBytes(body, "max_tokens").Int(),
		"verbosity", gjson.GetBytes(responsesBody, "text.verbosity").String(),
		"spark_override", sparkOverride,
		"request_mode", requestMode,
		"request_reason", requestReason,
		"session_key", session.SessionKey,
		"session_present", session.SessionID != "" || session.ConversationID != "" || session.PromptCacheKey != "",
		"session_source", session.SessionSource,
		"previous_response_id", session.PreviousRespID,
		"history_trimmed", replayTrimmed,
		"prompt_cache_key", session.PromptCacheKey,
		"request_body_bytes", len(body),
		"responses_body_bytes", len(responsesBody),
		"messages_hash", shortHashBytes([]byte(gjson.GetBytes(body, "messages").Raw)),
		"system_hash", shortHashBytes([]byte(gjson.GetBytes(body, "system").Raw)),
		"responses_input_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "input").Raw)),
		"tool_choice_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tool_choice").Raw)),
		"tools_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tools").Raw)),
		"body_has_prompt_cache_key", gjson.GetBytes(responsesBody, "prompt_cache_key").Exists(),
		"digest_chain", session.DigestChain,
		"digest_matched", session.MatchedDigest,
	)
	appendCacheDebugLog(
		"anthropic_request",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"request_mode", requestMode,
		"request_reason", requestReason,
		"session_key", session.SessionKey,
		"session_source", session.SessionSource,
		"prompt_cache_key", session.PromptCacheKey,
		"previous_response_id", session.PreviousRespID,
		"history_trimmed", replayTrimmed,
		"messages_hash", shortHashBytes([]byte(gjson.GetBytes(body, "messages").Raw)),
		"system_hash", shortHashBytes([]byte(gjson.GetBytes(body, "system").Raw)),
		"responses_input_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "input").Raw)),
		"tool_choice_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tool_choice").Raw)),
		"tools_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tools").Raw)),
		"body_has_prompt_cache_key", gjson.GetBytes(responsesBody, "prompt_cache_key").Exists(),
		"request_body_bytes", len(body),
		"responses_body_bytes", len(responsesBody),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_prefix", summarizeResponsesInputPrefix(responsesBody, 16),
	)

	// 6. 转发上游（含模型降级重试）
	fallbackModel := ""
	if sparkOverride {
		// Spark 路由：失败时回退到原始映射模型
		fallbackModel = originalMappedModel
	} else if mapping != nil && mapping.FallbackModel != "" && mapping.FallbackModel != mapping.OpenAIModel {
		fallbackModel = mapping.FallbackModel
	}

	replayBody := []byte(nil)
	if requestMode == "continuation" {
		replayBody = fullResponsesBody
	}

	return g.doAnthropicForward(ctx, req, responsesBody, replayBody, originalModel, modelName, fallbackModel, start, session)
}

// doAnthropicForward 执行 Anthropic 转发，支持模型降级重试
// mappedModel: 映射后的 GPT 模型名，用于 Core 计费（写入 result.Model）
func (g *OpenAIGateway) doAnthropicForward(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	replayBody []byte,
	originalModel string,
	mappedModel string,
	fallbackModel string,
	start time.Time,
	session openAISessionResolution,
) (*sdk.ForwardResult, error) {
	hasFallback := fallbackModel != ""

	// 第一次转发（有 fallback 时抑制错误写入客户端）
	result, errBody, err := g.forwardAnthropicResponses(ctx, req, responsesBody, originalModel, mappedModel, start, req.Writer, hasFallback, session)
	if err != nil {
		if len(replayBody) > 0 {
			var failure *responsesFailureError
			if errors.As(err, &failure) && failure.isContinuationAnchorError() {
				g.logger.Warn("Anthropic continuation 锚点失效，降级为 full replay 重试一次",
					"session", session.SessionKey,
					"digest_chain", session.DigestChain)
				clearSessionStateResponseID(session.SessionKey)
				result, errBody, err = g.forwardAnthropicResponses(ctx, req, replayBody, originalModel, mappedModel, start, req.Writer, hasFallback, session)
				if err == nil {
					goto fallbackCheck
				}
			}
		}
		return result, err
	}

	// 检查是否需要模型降级
fallbackCheck:
	if hasFallback && errBody != nil {
		if isModelNotFoundError(result.StatusCode, errBody) {
			g.logger.Info("模型降级重试",
				"primary", gjson.GetBytes(responsesBody, "model").String(),
				"fallback", fallbackModel,
				"status", result.StatusCode)

			// 替换模型后重试，降级模型同时作为计费模型
			responsesBody, _ = sjson.SetBytes(responsesBody, "model", fallbackModel)
			fallbackStart := time.Now()
			result, _, err = g.forwardAnthropicResponses(ctx, req, responsesBody, originalModel, fallbackModel, fallbackStart, req.Writer, false, session)
			return result, err
		}

		// 非模型错误，写回原始错误
		return g.writeAnthropicUpstreamError(req.Writer, result.StatusCode, errBody, start)
	}

	return result, nil
}

func finalizeAnthropicResponsesBody(responsesBody []byte, originalBody []byte, serviceTier string, sparkOverride bool) []byte {
	result := responsesBody
	if tier := normalizeOpenAIServiceTier(serviceTier); tier != "" {
		result, _ = sjson.SetBytes(result, "service_tier", tier)
	}
	if hasWebSearchTool(originalBody) {
		result = injectWebSearchToolJSON(result)
	}
	if sparkOverride {
		result, _ = sjson.SetBytes(result, "text.verbosity", "low")
	}
	return result
}

func injectAnthropicPromptCacheKey(responsesBody []byte, strategy anthropicUpstreamStrategy, session openAISessionResolution) []byte {
	if strategy == anthropicStrategyOAuth {
		return responsesBody
	}
	if strings.TrimSpace(session.PromptCacheKey) == "" {
		return responsesBody
	}
	if gjson.GetBytes(responsesBody, "prompt_cache_key").Exists() {
		return responsesBody
	}
	next, err := sjson.SetBytes(responsesBody, "prompt_cache_key", session.PromptCacheKey)
	if err != nil {
		return responsesBody
	}
	return next
}

// ──────────────────────────────────────────────────────
// 统一的 Anthropic → Responses API 转发（合并 OAuth/APIKey 路径）
// ──────────────────────────────────────────────────────

// forwardAnthropicResponses 统一的 Anthropic 转发函数
// mappedModel: 映射后的 GPT 模型名，用于 Core 计费（写入 result.Model）
// suppressErrorWrite: true 时上游错误不写入客户端，通过 errBody 返回供降级判断
// 返回值: result, errBody（仅 suppress 时非 nil）, error
func (g *OpenAIGateway) forwardAnthropicResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	mappedModel string,
	start time.Time,
	w http.ResponseWriter,
	suppressErrorWrite bool,
	session openAISessionResolution,
) (*sdk.ForwardResult, []byte, error) {
	account := req.Account

	// 强制覆盖 instructions（如果分组配置了）
	responsesBody = applyForceInstructions(responsesBody, req.Headers)

	// 构建上游 HTTP 请求（OAuth/APIKey 差异化处理）
	upstreamReq, err := g.buildAnthropicUpstreamRequest(ctx, req, account, responsesBody, session)
	if err != nil {
		return nil, nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 发送请求
	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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

	// 捕获上游 Codex 用量头
	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	// 流式 / 非流式响应处理
	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && w != nil {
		if turnState := decodeTurnStateHeader(resp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
		result, err := translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, mappedModel, req.Body, start, session)
		return result, nil, err
	}

	// 非流式：聚合 Responses SSE → Anthropic JSON
	if turnState := decodeTurnStateHeader(resp.Header); turnState != "" {
		updateSessionStateTurnState(session.SessionKey, turnState)
	}
	result, err := g.handleAnthropicNonStreamFromResponses(resp, w, originalModel, mappedModel, req.Body, start, session, req.Account.ID)
	return result, nil, err
}

// buildAnthropicUpstreamRequest 构建 Anthropic 转发的上游 HTTP 请求
// 根据 OAuth/APIKey 设置不同的 URL、认证头和特殊处理
func (g *OpenAIGateway) buildAnthropicUpstreamRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	account *sdk.Account,
	responsesBody []byte,
	session openAISessionResolution,
) (*http.Request, error) {
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
		return nil, err
	}

	// 公共头
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")

	if isOAuth {
		// OAuth 模式：手动设置认证头（SSE 模式不需要 OpenAI-Beta 头）
		upstreamReq.Header.Set("Authorization", "Bearer "+account.Credentials["access_token"])
		if aid := account.Credentials["chatgpt_account_id"]; aid != "" {
			upstreamReq.Header.Set("ChatGPT-Account-ID", aid)
		}
		if session.SessionID != "" {
			upstreamReq.Header.Set("session_id", isolateSessionID(session.SessionID))
		}
		if session.ConversationID != "" {
			upstreamReq.Header.Set("conversation_id", isolateSessionID(session.ConversationID))
		}
		if session.LastTurnState != "" {
			upstreamReq.Header.Set("x-codex-turn-state", session.LastTurnState)
		}
	} else {
		// API Key 模式
		setAuthHeaders(upstreamReq, account)
		passHeadersForAccount(req.Headers, upstreamReq.Header, account)
	}

	return upstreamReq, nil
}

// handleAnthropicNonStreamFromResponses 非流式：聚合 Responses SSE → Anthropic JSON
// mappedModel: 映射后的 GPT 模型名，用于 Core 计费（写入 result.Model）
func (g *OpenAIGateway) handleAnthropicNonStreamFromResponses(
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	mappedModel string,
	originalRequest []byte,
	start time.Time,
	session openAISessionResolution,
	accountID int64,
) (*sdk.ForwardResult, error) {
	wsResult := ParseSSEStream(resp.Body, nil)
	if wsResult.Err != nil {
		var failure *responsesFailureError
		if errors.As(wsResult.Err, &failure) && failure.shouldReturnClientError() {
			if w != nil {
				writeAnthropicErrorJSON(w, failure.StatusCode, failure.AnthropicErrorType, failure.Message)
			}
			return &sdk.ForwardResult{
				StatusCode:    failure.StatusCode,
				Model:         mappedModel,
				Duration:      time.Since(start),
				AccountStatus: failure.AccountStatus,
				ErrorMessage:  failure.Message,
				RetryAfter:    failure.RetryAfter,
			}, nil
		}
		return nil, wsResult.Err
	}
	if len(wsResult.CompletedEventRaw) == 0 {
		return nil, fmt.Errorf("未收到 response.completed 事件")
	}

	// 客户端响应体使用原始 Claude 模型名（model）
	anthropicJSON := convertResponsesCompletedToAnthropicJSON(wsResult.CompletedEventRaw, originalRequest, model)
	if anthropicJSON == "" {
		return nil, fmt.Errorf("responses 非流回译失败")
	}
	if session.SessionKey != "" && wsResult.ResponseID != "" {
		updateSessionStateResponseID(session.SessionKey, wsResult.ResponseID)
	}
	if session.SessionID != "" && session.DigestChain != "" {
		saveAnthropicDigestSession(accountID, session.DigestChain, session.SessionID, session.MatchedDigest)
	}

	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicJSON))
	}

	// result.Model 使用映射后的 GPT 模型名供 Core 计费
	billingModel := mappedModel
	if billingModel == "" {
		billingModel = gjson.Get(anthropicJSON, "model").String()
	}

	// 从上游 response.completed 事件中提取 service_tier
	serviceTier := normalizeOpenAIServiceTier(
		gjson.GetBytes(wsResult.CompletedEventRaw, "response.service_tier").String(),
	)

	elapsed := time.Since(start)
	result := &sdk.ForwardResult{
		StatusCode:            http.StatusOK,
		Model:                 billingModel,
		InputTokens:           wsResult.InputTokens,
		OutputTokens:          wsResult.OutputTokens,
		CachedInputTokens:     wsResult.CachedInputTokens,
		ReasoningOutputTokens: wsResult.ReasoningOutputTokens,
		ServiceTier:           serviceTier,
		Duration:              elapsed,
		FirstTokenMs:          elapsed.Milliseconds(),
	}
	fillCost(result)
	return result, nil
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
		ErrorMessage:  errMsg,
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
