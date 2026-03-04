package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// Anthropic Messages API 转发入口
// 参考 AxonHub llm/transformer/anthropic/inbound.go
// ──────────────────────────────────────────────────────

// forwardAnthropicMessage 处理 Anthropic Messages API 请求，翻译为 OpenAI Chat Completions 格式转发
func (g *OpenAIGateway) forwardAnthropicMessage(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	g.logger.Info("[客户端→Anthropic] 收到请求",
		"model", gjson.GetBytes(req.Body, "model").String(),
		"messages", gjson.GetBytes(req.Body, "messages.#").Int(),
		"tools", gjson.GetBytes(req.Body, "tools.#").Int(),
		"stream", gjson.GetBytes(req.Body, "stream").Bool(),
		"last_msg", truncate(gjson.GetBytes(req.Body, "messages.@last.content").String(), 200),
	)

	// 1. 解析 Anthropic 请求体
	var anthropicReq AnthropicMessageRequest
	if err := json.Unmarshal(req.Body, &anthropicReq); err != nil {
		if req.Writer != nil {
			writeAnthropicErrorResponse(req.Writer, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("请求体解析失败: %v", err))
		}
		return &sdk.ForwardResult{StatusCode: http.StatusBadRequest, Duration: time.Since(start)}, nil
	}

	// 同步 model 字段
	if anthropicReq.Model == "" && req.Model != "" {
		anthropicReq.Model = req.Model
	}

	// 同步 stream 标志
	if req.Stream && (anthropicReq.Stream == nil || !*anthropicReq.Stream) {
		t := true
		anthropicReq.Stream = &t
	}

	// 2. 验证请求
	if errResp := validateAnthropicRequest(&anthropicReq); errResp != nil {
		if req.Writer != nil {
			writeAnthropicErrorResponse(req.Writer, errResp.StatusCode, errResp.Error.Type, errResp.Error.Message)
		}
		return &sdk.ForwardResult{StatusCode: errResp.StatusCode, Duration: time.Since(start)}, nil
	}

	// 3. 模型映射：Claude 模型名 → OpenAI 模型 + 额外参数
	originalModel := anthropicReq.Model
	var mappingEffort string
	if mapping := resolveAnthropicModelMapping(anthropicReq.Model); mapping != nil {
		g.logger.Info("Anthropic 模型映射",
			"from", originalModel,
			"to", mapping.OpenAIModel,
			"reasoning_effort", mapping.ReasoningEffort)
		anthropicReq.Model = mapping.OpenAIModel
		mappingEffort = mapping.ReasoningEffort
	}

	// 4. 优化 cache control
	optimizeCacheControl(&anthropicReq)

	// 5. 转换为 OpenAI Chat Completions 格式
	openaiBody, err := convertAnthropicToOpenAIWithMapping(&anthropicReq, mappingEffort)
	if err != nil {
		if req.Writer != nil {
			writeAnthropicErrorResponse(req.Writer, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("请求转换失败: %v", err))
		}
		return &sdk.ForwardResult{StatusCode: http.StatusBadRequest, Duration: time.Since(start)}, nil
	}
	g.logger.Info("[Anthropic→OpenAI] 转换完成",
		"model", gjson.GetBytes(openaiBody, "model").String(),
		"messages", gjson.GetBytes(openaiBody, "messages.#").Int(),
		"tools", gjson.GetBytes(openaiBody, "tools.#").Int(),
		"reasoning_effort", gjson.GetBytes(openaiBody, "reasoning_effort").String(),
	)

	// 6. 根据账号类型选择转发方式
	if account.Credentials["access_token"] != "" {
		// OAuth 模式：转换为 Responses API 格式，走 HTTP SSE，再翻译回 Anthropic SSE
		return g.forwardAnthropicViaResponsesSSE(ctx, req, openaiBody, originalModel, &anthropicReq, start)
	}

	// API Key 模式：HTTP 转发
	targetURL := buildAPIKeyURL(account, "/v1/chat/completions")
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(openaiBody))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	setAuthHeaders(upstreamReq, account)
	upstreamReq.Header.Set("Content-Type", "application/json")
	passHeaders(req.Headers, upstreamReq.Header)

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return g.handleAnthropicUpstreamError(resp, req.Writer, start)
	}

	// 根据流式/非流式处理响应（返回原始 Claude 模型名给客户端）
	isStream := anthropicReq.Stream != nil && *anthropicReq.Stream
	if isStream && req.Writer != nil {
		return translateOpenAISSEToAnthropicSSE(ctx, resp, req.Writer, originalModel, start)
	}

	return g.handleAnthropicNonStream(resp, req.Writer, originalModel, start)
}

// handleAnthropicNonStream 处理非流式响应（OpenAI JSON → Anthropic JSON）
func (g *OpenAIGateway) handleAnthropicNonStream(
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	// 转换为 Anthropic 格式
	anthropicResp, err := convertOpenAIResponseToAnthropic(body, model)
	if err != nil {
		if w != nil {
			writeAnthropicErrorResponse(w, http.StatusInternalServerError, "api_error",
				fmt.Sprintf("响应转换失败: %v", err))
		}
		return &sdk.ForwardResult{StatusCode: http.StatusInternalServerError, Duration: time.Since(start)}, nil
	}

	result := &sdk.ForwardResult{
		StatusCode: http.StatusOK,
		Model:      anthropicResp.Model,
		Duration:   time.Since(start),
	}

	if anthropicResp.Usage != nil {
		result.InputTokens = anthropicResp.Usage.InputTokens
		result.OutputTokens = anthropicResp.Usage.OutputTokens
		result.CacheTokens = anthropicResp.Usage.CacheReadInputTokens
	}

	// 写回 Anthropic 格式 JSON
	if w != nil {
		respData, _ := json.Marshal(anthropicResp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respData)
	}

	return result, nil
}

// handleAnthropicUpstreamError 处理上游错误，转换为 Anthropic 错误格式
func (g *OpenAIGateway) handleAnthropicUpstreamError(
	resp *http.Response,
	w http.ResponseWriter,
	start time.Time,
) (*sdk.ForwardResult, error) {
	body, _ := io.ReadAll(resp.Body)
	statusCode := resp.StatusCode

	// 打印完整错误响应，便于排查
	g.logger.Error("上游返回错误", "status", statusCode, "body", string(body))

	// 尝试从 OpenAI 错误响应中提取消息
	errMsg := truncate(string(body), 200)
	if msg := extractOpenAIErrorMessage(body); msg != "" {
		errMsg = msg
	}

	errType := anthropicErrorType(statusCode)

	if w != nil {
		writeAnthropicErrorResponse(w, statusCode, errType, errMsg)
	}

	result := &sdk.ForwardResult{
		StatusCode: statusCode,
		Duration:   time.Since(start),
	}

	// 500/429 返回 error 让核心决定是否 failover
	if statusCode >= 500 || statusCode == 429 {
		return result, fmt.Errorf("上游返回 %d: %s", statusCode, errMsg)
	}

	return result, nil
}

// extractOpenAIErrorMessage 从 OpenAI 错误响应中提取错误消息
func extractOpenAIErrorMessage(body []byte) string {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	return ""
}

// forwardAnthropicViaResponsesSSE OAuth 模式下的 Anthropic 请求处理：
// Chat Completions JSON → Responses API SSE → Anthropic SSE
func (g *OpenAIGateway) forwardAnthropicViaResponsesSSE(
	ctx context.Context,
	req *sdk.ForwardRequest,
	openaiBody []byte,
	originalModel string,
	anthropicReq *AnthropicMessageRequest,
	start time.Time,
) (*sdk.ForwardResult, error) {
	account := req.Account

	// 1. Chat Completions JSON → Responses API JSON
	responsesBody, err := wrapAsResponsesAPI(openaiBody, anthropicReq.Model)
	if err != nil {
		return nil, fmt.Errorf("转换 Responses API 格式失败: %w", err)
	}

	// 始终注入 web_search 工具，让上游模型具备联网搜索能力
	// Claude Code 用自定义 provider 时不会主动发 web_search 工具，需要代理层注入
	responsesBody = injectWebSearchTool(responsesBody)
	g.logger.Info("[Anthropic→Responses] 发给上游",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"instructions_len", len(gjson.GetBytes(responsesBody, "instructions").String()),
	)

	// 2. 构建 HTTP 请求到 Responses SSE 端点
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

	// 3. 把 Responses SSE 翻译成 Anthropic SSE 写给客户端
	isStream := anthropicReq.Stream != nil && *anthropicReq.Stream
	if isStream && req.Writer != nil {
		return translateResponsesSSEToAnthropicSSE(ctx, resp, req.Writer, originalModel, start)
	}

	// 非流式：聚合 Responses SSE 后构造 Anthropic 响应
	wsResult := ParseSSEStream(resp.Body, nil)
	if wsResult.Err != nil {
		return nil, wsResult.Err
	}

	msgID := generateMessageID()
	anthropicResp := AnthropicMessage{
		ID:    msgID,
		Type:  "message",
		Role:  "assistant",
		Model: originalModel,
		Content: []AnthropicMessageContentBlock{
			{Type: "text", Text: ptrStr(wsResult.Text)},
		},
		StopReason: ptrStr("end_turn"),
		Usage: &AnthropicUsage{
			InputTokens:  wsResult.InputTokens,
			OutputTokens: wsResult.OutputTokens,
		},
	}
	respData, _ := json.Marshal(anthropicResp)

	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(respData)
	}

	return &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		Model:        originalModel,
		InputTokens:  wsResult.InputTokens,
		OutputTokens: wsResult.OutputTokens,
		Duration:     time.Since(start),
	}, nil
}

// ──────────────────────────────────────────────────────
// 请求验证
// ──────────────────────────────────────────────────────

// anthropicValidationError 验证错误快捷方法
type anthropicValidationError struct {
	StatusCode int
	Error      AnthropicErrorDetail
}

// validateAnthropicRequest 验证 Anthropic 请求
func validateAnthropicRequest(req *AnthropicMessageRequest) *anthropicValidationError {
	if req.Model == "" {
		return &anthropicValidationError{
			StatusCode: http.StatusBadRequest,
			Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "model is required"},
		}
	}

	if len(req.Messages) == 0 {
		return &anthropicValidationError{
			StatusCode: http.StatusBadRequest,
			Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "messages is required"},
		}
	}

	if req.MaxTokens <= 0 {
		return &anthropicValidationError{
			StatusCode: http.StatusBadRequest,
			Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "max_tokens must be greater than 0"},
		}
	}

	// 验证 thinking
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "enabled":
			if req.Thinking.BudgetTokens <= 0 {
				return &anthropicValidationError{
					StatusCode: http.StatusBadRequest,
					Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "budget_tokens is required when thinking type is enabled"},
				}
			}
		case "adaptive":
			// adaptive 需要 output_config
		case "disabled":
			// ok
		default:
			return &anthropicValidationError{
				StatusCode: http.StatusBadRequest,
				Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "thinking type must be one of: enabled, disabled, adaptive"},
			}
		}
	}

	// 验证 tool_choice
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "none", "any":
			// ok
		case "tool":
			if req.ToolChoice.Name == nil || *req.ToolChoice.Name == "" {
				return &anthropicValidationError{
					StatusCode: http.StatusBadRequest,
					Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "name is required when tool_choice type is tool"},
				}
			}
		default:
			return &anthropicValidationError{
				StatusCode: http.StatusBadRequest,
				Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "tool_choice type must be one of: auto, none, any, tool"},
			}
		}
	}

	// 验证 system prompts
	if req.System != nil && len(req.System.MultiplePrompts) > 0 {
		for _, p := range req.System.MultiplePrompts {
			if p.Type != "text" {
				return &anthropicValidationError{
					StatusCode: http.StatusBadRequest,
					Error:      AnthropicErrorDetail{Type: "invalid_request_error", Message: "system prompt type must be text"},
				}
			}
		}
	}

	return nil
}
