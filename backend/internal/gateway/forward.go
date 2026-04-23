package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// 转发入口（三模式分发）
// ──────────────────────────────────────────────────────

// forwardHTTP 根据账号凭证类型分发到不同转发模式
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	if isAnthropicCountTokensRequest(req) {
		return g.forwardAnthropicCountTokens(ctx, req)
	}

	// 检测 Anthropic Messages API 请求，走协议翻译
	if isAnthropicRequest(req) {
		return g.forwardAnthropicMessage(ctx, req)
	}

	// GET /v1/models：用插件内置模型清单本地返回。
	if isModelsListingRequest(req) {
		return buildLocalModelsResponse(), nil
	}

	// 统一预处理请求体。multipart 请求（images/edits 上传图片）body 是二进制，
	// 不能按 JSON 处理否则会被 sjson 覆盖丢失数据。
	_, reqPath := resolveAPIKeyRoute(req)
	if !strings.HasPrefix(req.Headers.Get("Content-Type"), "multipart/") {
		req.Body = preprocessRequestBody(req.Body, req.Model, reqPath)
		req.Body = applyForceInstructions(req.Body, req.Headers)
	}

	account := req.Account

	if isImagesRequest(reqPath) && !isImageEnabled(req.Headers) {
		body := jsonError("当前分组未开启图片生成功能")
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusForbidden)
			_, _ = req.Writer.Write(body)
		}
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusForbidden,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason: "分组未开启 image_enabled",
		}, nil
	}

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req)
	}
	if account.Credentials["access_token"] != "" {
		if isImagesRequest(reqPath) {
			if isImagesWebReverseModel(req.Model) {
				return g.forwardImagesViaWebReverse(ctx, req)
			}
			return g.forwardImagesViaResponsesTool(ctx, req)
		}
		return g.forwardOAuth(ctx, req)
	}
	reason := "账号缺少 api_key 或 access_token"
	return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
}

// isImageEnabled 检查分组是否开启了图片生成功能。
// Core 通过 X-Airgate-Plugin-Openai-Image-Enabled 头传递分组的 plugin_settings。
func isImageEnabled(headers http.Header) bool {
	return strings.EqualFold(headers.Get("X-Airgate-Plugin-Openai-Image-Enabled"), "true")
}

// isModelsListingRequest 判断当前请求是否为 GET /v1/models。
//
// Core 不会透传原始方法和路径，只能用既有线索推断：
//  1. 优先看 X-Forwarded-Path（如果 core 设置了）
//  2. 回退到 resolveAPIKeyRoute 的兜底推断（空 body + 非 stream → /v1/models）
//
// 这保持了与 resolveAPIKeyRoute 一致的推断逻辑，避免两处判据漂移。
func isModelsListingRequest(req *sdk.ForwardRequest) bool {
	if req == nil {
		return false
	}
	method, path := resolveAPIKeyRoute(req)
	return method == http.MethodGet && (path == "/v1/models" || strings.HasPrefix(path, "/v1/models?"))
}

// buildLocalModelsResponse 用插件内置模型注册表合成 OpenAI 兼容的 /v1/models 响应。
func buildLocalModelsResponse() sdk.ForwardOutcome {
	specs := model.AllSpecs()
	data := make([]map[string]any, 0, len(specs))
	created := time.Now().Unix()
	for _, spec := range specs {
		entry := map[string]any{
			"id":       spec.ID,
			"object":   "model",
			"created":  created,
			"owned_by": "airgate",
		}
		if spec.ContextWindow > 0 {
			entry["context_window"] = spec.ContextWindow
			entry["context_length"] = spec.ContextWindow
			entry["max_input_tokens"] = spec.ContextWindow
		}
		if spec.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = spec.MaxOutputTokens
		}
		data = append(data, entry)
	}
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
	})
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return successOutcome(http.StatusOK, body, headers, nil)
}

// ──────────────────────────────────────────────────────
// API Key 模式：HTTP/SSE 直连上游
// ──────────────────────────────────────────────────────

func (g *OpenAIGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account

	reqMethod, reqPath := resolveAPIKeyRoute(req)
	targetURL := buildAPIKeyURL(account, reqPath)

	var bodyReader io.Reader
	if methodAllowsBody(reqMethod) && len(req.Body) > 0 {
		bodyReader = bytes.NewReader(req.Body)
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, reqMethod, targetURL, bodyReader)
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	setAuthHeaders(upstreamReq, account)
	if methodAllowsBody(reqMethod) {
		// /v1/images/edits 是 multipart/form-data，必须保留 boundary。其它路径
		// Core 侧已把 body 归一化成 JSON 文本，统一 application/json。
		if ct := req.Headers.Get("Content-Type"); isImagesEditRequest(reqPath) &&
			strings.HasPrefix(strings.ToLower(ct), "multipart/") {
			upstreamReq.Header.Set("Content-Type", ct)
		} else {
			upstreamReq.Header.Set("Content-Type", "application/json")
		}
	}
	passHeadersForAccount(req.Headers, upstreamReq.Header, account)

	var ka *imageKeepAlive
	if isImagesRequest(reqPath) {
		ka = startImageKeepAlive(req.Writer)
	}

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		if ka != nil {
			ka.Finish(http.StatusBadGateway, buildImagesErrorBody(http.StatusBadGateway, err.Error()))
		}
		// 网络层错误，无上游 HTTP 响应
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 非 2xx 统一走 failureOutcome 归类。包含 4xx（客户端错）/ 429 / 401 / 403 / 5xx。
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		if ka != nil {
			ka.Finish(resp.StatusCode, respBody)
		}
		errDetail := gjson.GetBytes(respBody, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(respBody), 200)
		}
		outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), errDetail, extractRetryAfterHeader(resp.Header))
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	// /v1/models 路径补齐上下文元信息
	if reqMethod == http.MethodGet && strings.HasPrefix(reqPath, "/v1/models") {
		resp = enrichModelsResponse(resp)
	}

	// 捕获上游 Codex 用量头
	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	// Images API 响应体无 model 字段，另走专用处理器回填后再 fillCost
	if isImagesRequest(reqPath) {
		return handleImagesResponse(resp, req.Writer, ka, start, req.Model)
	}

	if req.Stream && req.Writer != nil {
		return handleStreamResponse(resp, req.Writer, start)
	}
	return handleNonStreamResponse(resp, req.Writer, start)
}

func enrichModelsResponse(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}

	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(raw) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if len(raw) > 0 {
			resp.ContentLength = int64(len(raw))
		}
		return resp
	}

	dataNode := gjson.GetBytes(raw, "data")
	if !dataNode.Exists() || !dataNode.IsArray() {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		return resp
	}

	updated := raw
	changed := false
	for idx, item := range dataNode.Array() {
		modelID := strings.TrimSpace(item.Get("id").String())
		if modelID == "" {
			continue
		}

		meta := getModelMetadataByID(modelID)
		if len(meta) == 0 {
			continue
		}
		for key, value := range meta {
			path := fmt.Sprintf("data.%d.%s", idx, key)
			if gjson.GetBytes(updated, path).Exists() {
				continue
			}
			patched, setErr := sjson.SetBytes(updated, path, value)
			if setErr != nil {
				continue
			}
			updated = patched
			changed = true
		}
	}

	if !changed {
		updated = raw
	}

	resp.Body = io.NopCloser(bytes.NewReader(updated))
	resp.ContentLength = int64(len(updated))
	if resp.Header != nil {
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(updated)))
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp
}

// ──────────────────────────────────────────────────────
// OAuth 模式：WebSocket 连上游，SSE 写回客户端
// ──────────────────────────────────────────────────────

// forwardOAuth 使用 WebSocket 连接上游，将响应以 SSE 格式写回客户端
func (g *OpenAIGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	session := resolveOpenAISession(req.Headers, req.Body)
	updateSessionStateFromRequest(session)

	cfg := WSConfig{
		Token:          account.Credentials["access_token"],
		AccountID:      account.Credentials["chatgpt_account_id"],
		ProxyURL:       account.ProxyURL,
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnState:      session.LastTurnState,
		Originator:     req.Headers.Get("originator"),
	}
	conn, wsResp, err := DialWebSocket(cfg)
	if err != nil {
		// WS 握手失败：按 HTTP 响应码归类。无响应则视为网络层 transient。
		if wsResp != nil {
			outcome := failureOutcome(wsResp.StatusCode, nil, wsResp.Header.Clone(), err.Error(), 0)
			outcome.Duration = time.Since(start)
			return outcome, err
		}
		return transientOutcome(err.Error()), err
	}
	defer func() { _ = conn.Close() }()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	// 构建 response.create 消息
	createMsg, err := g.buildWSRequest(req, session)
	if err != nil {
		reason := fmt.Sprintf("构建 WebSocket 请求失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	// 协议分叉：客户端如果走的是 /v1/chat/completions（而不是原生 /v1/responses），
	// OAuth 上游依然只吐 Responses API 的 SSE 事件——需要把这些事件翻译回 Chat
	// Completions 协议，否则标准 OpenAI SDK 的 chat.completions.create 无法解析。
	isChatCompletions := isChatCompletionsRequest(req)
	chatStreamInclude := isChatCompletions && req.Stream &&
		gjson.GetBytes(req.Body, "stream_options.include_usage").Bool()

	// 是否走静默缓冲路径（等整条响应就绪再吐 JSON）。两种场景触发：
	//   - /v1/chat/completions 非流式
	//   - /v1/responses 非流式
	silentBuffered := !req.Stream

	var (
		lastSSEHandler      *sseEventWriter
		lastChatWriter      *chatCompletionsStreamWriter
		lastChatSilent      *chatCompletionsSilentHandler
		lastResponsesSilent *responsesSilentHandler
	)

	runAttempt := func(msg []byte, w http.ResponseWriter) (WSResult, error) {
		if err := conn.WriteJSON(json.RawMessage(msg)); err != nil {
			return WSResult{}, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
		}
		var handler WSEventHandler
		switch {
		case isChatCompletions && req.Stream:
			writer := newChatCompletionsStreamWriter(
				w, req.Model, account.ID, session.SessionKey, chatStreamInclude, start,
			)
			lastChatWriter = writer
			handler = writer
		case isChatCompletions && !req.Stream:
			silent := &chatCompletionsSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			lastChatSilent = silent
			handler = silent
		case !isChatCompletions && !req.Stream:
			// 原生 /v1/responses 非流式：缓冲事件，末尾一次性吐 JSON
			silent := &responsesSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			lastResponsesSilent = silent
			handler = silent
		default:
			// 原生 /v1/responses 流式：SSE 透传
			sseHandler := &sseEventWriter{
				w:          w,
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			if f, ok := w.(http.Flusher); ok {
				sseHandler.flusher = f
			}
			lastSSEHandler = sseHandler
			handler = sseHandler
		}
		return ReceiveWSResponse(ctx, conn, handler), nil
	}

	w := req.Writer

	// 流式响应头在请求开始就写；非流式（chat.completions 或 responses）等 result 就绪后写 JSON。
	if w != nil && !silentBuffered {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	result, err := runAttempt(createMsg, w)
	if err != nil {
		return transientOutcome(err.Error()), err
	}
	if session.SessionKey != "" {
		if result.ResponseID != "" {
			updateSessionStateResponseID(session.SessionKey, result.ResponseID)
		}
	}

	// 结束标记 / 响应体写回
	switch {
	case isChatCompletions && req.Stream:
		if lastChatWriter != nil {
			lastChatWriter.finalize()
		}
	case isChatCompletions && !req.Stream:
		if result.Err == nil && w != nil {
			body := buildNonStreamChatCompletion(result, req.Model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	case !isChatCompletions && !req.Stream:
		// /v1/responses 非流式：从 WSResult 抽 response 字段回写 JSON
		if result.Err == nil && w != nil {
			body := buildNonStreamResponses(result)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	default:
		// /v1/responses 流式：补 [DONE] 标记
		if w != nil {
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}

	elapsed := time.Since(start)
	firstTokenMs := elapsed.Milliseconds()
	switch {
	case lastChatWriter != nil && lastChatWriter.firstTokenMs > 0:
		firstTokenMs = lastChatWriter.firstTokenMs
	case lastSSEHandler != nil && lastSSEHandler.firstTokenMs > 0:
		firstTokenMs = lastSSEHandler.firstTokenMs
	case lastChatSilent != nil && lastChatSilent.firstTokenMs > 0:
		firstTokenMs = lastChatSilent.firstTokenMs
	case lastResponsesSilent != nil && lastResponsesSilent.firstTokenMs > 0:
		firstTokenMs = lastResponsesSilent.firstTokenMs
	}

	usage := &sdk.Usage{
		InputTokens:           result.InputTokens,
		OutputTokens:          result.OutputTokens,
		CachedInputTokens:     result.CachedInputTokens,
		ReasoningOutputTokens: result.ReasoningOutputTokens,
		ServiceTier:           normalizeOpenAIServiceTier(gjson.GetBytes(createMsg, "service_tier").String()),
		Model:                 result.Model,
		FirstTokenMs:          firstTokenMs,
	}
	numImages := len(result.ImageGenCalls)

	if result.Err != nil {
		var failure *responsesFailureError
		kind := sdk.OutcomeUpstreamTransient
		statusCode := http.StatusBadGateway
		message := result.Err.Error()
		var retryAfter time.Duration
		if errors.As(result.Err, &failure) {
			kind = failure.outcomeKind()
			statusCode = failure.StatusCode
			message = failure.Message
			retryAfter = failure.RetryAfter
		}
		// 流已开写的场景：Client 错误仍按 ClientError 透传；其它非账号错误视为 StreamAborted。
		if req.Stream && kind != sdk.OutcomeClientError {
			kind = sdk.OutcomeStreamAborted
		}
		return sdk.ForwardOutcome{
			Kind:       kind,
			Upstream:   sdk.UpstreamResponse{StatusCode: statusCode},
			Reason:     message,
			RetryAfter: retryAfter,
			Duration:   elapsed,
		}, result.Err
	}

	fillUsageCostWithImageTool(usage, numImages)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

// ──────────────────────────────────────────────────────
// SSE 事件写入器（WSEventHandler 实现）
// ──────────────────────────────────────────────────────

// sseEventWriter 将 WS 事件转为 SSE 格式写入 http.ResponseWriter
type sseEventWriter struct {
	w              http.ResponseWriter
	flusher        http.Flusher
	accountID      int64 // 用于存储 Codex 用量快照
	sessionKey     string
	start          time.Time // 请求开始时间，用于计算首 token 延迟
	firstTokenMs   int64     // 首 token 到达时间（毫秒）
	firstTokenOnce sync.Once // 确保只记录一次
}

func (s *sseEventWriter) OnTextDelta(string)      {}
func (s *sseEventWriter) OnReasoningDelta(string) {}
func (s *sseEventWriter) OnRateLimits(used float64) {
	if s.accountID > 0 {
		StoreCodexUsage(s.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: used,
			CapturedAt:         time.Now(),
		})
	}
}

func (s *sseEventWriter) OnRawEvent(eventType string, data []byte) {
	if s.w == nil || eventType == "" {
		return
	}
	// 记录首 token 延迟（第一个有效事件到达客户端的时间）
	s.firstTokenOnce.Do(func() {
		s.firstTokenMs = time.Since(s.start).Milliseconds()
	})
	// 过滤不需要转发给客户端的内部事件，并捕获用量
	switch eventType {
	case "codex.rate_limits":
		if s.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(s.accountID, snapshot)
			}
		}
		return
	case "response.created", "response.completed", "response.done":
		if s.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(s.sessionKey, responseID)
			}
		}
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, strings.ReplaceAll(string(data), "\n", "")); err != nil {
		return
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ──────────────────────────────────────────────────────
// HTTP 客户端
// ──────────────────────────────────────────────────────

func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// requestTimeout 获取插件默认请求超时配置
func (g *OpenAIGateway) requestTimeout() time.Duration {
	const fallback = 300 * time.Second
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return fallback
	}
	timeout := g.ctx.Config().GetDuration("default_timeout")
	if timeout <= 0 {
		return fallback
	}
	return timeout
}

// buildHTTPClient 构建带代理支持的 HTTP 客户端
// 使用 TransportPool 按账户+代理隔离连接，同一账户复用连接
func (g *OpenAIGateway) buildHTTPClient(account *sdk.Account) *http.Client {
	transport := g.transportPool.GetTransport(account.ID, account.ProxyURL)

	return &http.Client{
		Transport: transport,
		Timeout:   g.requestTimeout(),
	}
}
