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
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	if isAnthropicCountTokensRequest(req) {
		return g.forwardAnthropicCountTokens(ctx, req)
	}

	// 检测 Anthropic Messages API 请求，走协议翻译
	if isAnthropicRequest(req) {
		return g.forwardAnthropicMessage(ctx, req)
	}

	// GET /v1/models 直接用插件内置模型清单本地返回，不走账号分发。
	// OAuth 账号无法转发 /v1/models（上游是 ChatGPT WS responses 端点），
	// API Key 账号即使上游支持，也没必要为一份静态清单多打一次外网。
	if isModelsListingRequest(req) {
		return buildLocalModelsResponse(), nil
	}

	// 统一预处理请求体：model 同步、上下文守卫、input 规范化、force instructions。
	// 在 API Key / OAuth 分发之前执行，保证两条路径拿到的 body 格式一致。
	// 注：Anthropic 路径有自己的 applyForceInstructions（在格式转换之后），不走这里。
	// 注：multipart 请求（如 images/edits 上传图片）body 是二进制数据，不能按 JSON 处理，
	//     否则 sjson 会把整个 body 替换成一个小 JSON 对象，丢失全部图片数据。
	_, reqPath := resolveAPIKeyRoute(req)
	if !strings.HasPrefix(req.Headers.Get("Content-Type"), "multipart/") {
		req.Body = preprocessRequestBody(req.Body, req.Model, reqPath)
		req.Body = applyForceInstructions(req.Body, req.Headers)
	}

	account := req.Account

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req)
	}
	if account.Credentials["access_token"] != "" {
		// OAuth 账号收到 Images REST 请求时：
		//   - model == "gpt-image-2" → 走 chatgpt.com 网页端逆向（imgen 子包）
		//   - 其它 / 为空 → 翻译为 Responses API + image_generation tool
		//     （与 Codex $imagegen 一致），把结果打包回 Images REST 响应。
		if isImagesRequest(reqPath) {
			if isImagesWebReverseModel(req.Model) {
				return g.forwardImagesViaWebReverse(ctx, req)
			}
			return g.forwardImagesViaResponsesTool(ctx, req)
		}
		return g.forwardOAuth(ctx, req)
	}
	return nil, fmt.Errorf("账号缺少 api_key 或 access_token")
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
func buildLocalModelsResponse() *sdk.ForwardResult {
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
	return &sdk.ForwardResult{
		StatusCode:    http.StatusOK,
		Body:          body,
		Headers:       headers,
		AccountStatus: sdk.AccountStatusOK,
	}
}

// ──────────────────────────────────────────────────────
// API Key 模式：HTTP/SSE 直连上游
// ──────────────────────────────────────────────────────

func (g *OpenAIGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 解析上游请求方法与路径
	reqMethod, reqPath := resolveAPIKeyRoute(req)
	targetURL := buildAPIKeyURL(account, reqPath)
	body := req.Body

	var bodyReader io.Reader
	if methodAllowsBody(reqMethod) && len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	// 构建 HTTP 请求
	upstreamReq, err := http.NewRequestWithContext(ctx, reqMethod, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 设置认证头
	setAuthHeaders(upstreamReq, account)
	if methodAllowsBody(reqMethod) {
		// /v1/images/edits 在 SDK 侧是 multipart/form-data，必须保留 boundary，
		// 否则上游解析 body 会失败。其它路径一律 application/json（Core 侧已把
		// 请求体归一化成 JSON 文本）。
		if ct := req.Headers.Get("Content-Type"); isImagesEditRequest(reqPath) &&
			strings.HasPrefix(strings.ToLower(ct), "multipart/") {
			upstreamReq.Header.Set("Content-Type", ct)
		} else {
			upstreamReq.Header.Set("Content-Type", "application/json")
		}
	}

	// 透传白名单头
	passHeadersForAccount(req.Headers, upstreamReq.Header, account)

	// 发送请求
	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// 上游返回错误时，返回 error 让核心决定是否 failover
	if resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		respBody, _ := io.ReadAll(resp.Body)
		// 优先提取 JSON error.message，回退到截断的原始响应
		errDetail := ""
		if msg := gjson.GetBytes(respBody, "error.message").String(); msg != "" {
			errDetail = msg
		} else {
			errDetail = truncate(string(respBody), 200)
		}
		return &sdk.ForwardResult{
			StatusCode:    resp.StatusCode,
			Duration:      time.Since(start),
			AccountStatus: accountStatusFromMessage(resp.StatusCode, errDetail),
			ErrorMessage:  errDetail,
			RetryAfter:    extractRetryAfterHeader(resp.Header),
		}, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, errDetail)
	}

	// /v1/models 路径补齐上下文元信息（不影响其它路由）
	if reqMethod == http.MethodGet && strings.HasPrefix(reqPath, "/v1/models") {
		resp = enrichModelsResponse(resp)
	}

	// 捕获上游 Codex 用量头
	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	// Images API 响应体无 model 字段，另走专用处理器回填模型后再 fillCost
	if isImagesRequest(reqPath) {
		return handleImagesResponse(resp, req.Writer, start, req.Model)
	}

	// 流式 / 非流式响应处理
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
func (g *OpenAIGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account
	session := resolveOpenAISession(req.Headers, req.Body)
	updateSessionStateFromRequest(session)

	// 建立 WebSocket 连接，透传客户端的缓存与路由相关头
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
		// WS 握手失败时，根据 HTTP 状态码设置 AccountStatus，让核心正确处理账号状态
		if wsResp != nil && (wsResp.StatusCode == 401 || wsResp.StatusCode == 403 || wsResp.StatusCode == 429) {
			return &sdk.ForwardResult{
				StatusCode:    wsResp.StatusCode,
				Duration:      time.Since(start),
				AccountStatus: accountStatusFromMessage(wsResp.StatusCode, err.Error()),
				ErrorMessage:  err.Error(),
			}, err
		}
		return nil, err
	}
	defer func() {
		_ = conn.Close()
	}()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	// 构建 response.create 消息
	createMsg, err := g.buildWSRequest(req, session)
	if err != nil {
		return nil, fmt.Errorf("构建 WebSocket 请求失败: %w", err)
	}

	// 协议分叉：客户端如果走的是 /v1/chat/completions（而不是原生 /v1/responses），
	// OAuth 上游依然只吐 Responses API 的 SSE 事件——需要把这些事件翻译回 Chat
	// Completions 协议，否则标准 OpenAI SDK 的 chat.completions.create 无法解析。
	isChatCompletions := isChatCompletionsRequest(req)
	chatStreamInclude := isChatCompletions && req.Stream &&
		gjson.GetBytes(req.Body, "stream_options.include_usage").Bool()

	var (
		lastSSEHandler *sseEventWriter
		lastChatWriter *chatCompletionsStreamWriter
		lastSilent     *chatCompletionsSilentHandler
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
			lastSilent = silent
			handler = silent
		default:
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

	// 流式响应头：chat.completions 流式 + 原生 responses 都走 SSE；
	// 非流式 chat.completions 延后到 result 就绪后再写 application/json。
	if w != nil && (!isChatCompletions || req.Stream) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	result, err := runAttempt(createMsg, w)
	if err != nil {
		return nil, err
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
	default:
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
	case lastSilent != nil && lastSilent.firstTokenMs > 0:
		firstTokenMs = lastSilent.firstTokenMs
	}
	fwdResult := &sdk.ForwardResult{
		StatusCode:            http.StatusOK,
		InputTokens:           result.InputTokens,
		OutputTokens:          result.OutputTokens,
		CachedInputTokens:     result.CachedInputTokens,
		ReasoningOutputTokens: result.ReasoningOutputTokens,
		ServiceTier:           normalizeOpenAIServiceTier(gjson.GetBytes(createMsg, "service_tier").String()),
		Model:                 result.Model,
		Duration:              elapsed,
		FirstTokenMs:          firstTokenMs,
	}
	toolImageIn := result.ToolImageInputTokens
	toolImageOut := result.ToolImageOutputTokens
	// ChatGPT OAuth 下 tool_usage.image_gen 永远为 0；只要流里出现了
	// image_generation_call output item，就按 size×quality 估算 token 计入账单。
	if toolImageOut == 0 && len(result.ImageGenCalls) > 0 {
		toolImageOut = estimateImageGenOutputTokens(result.ImageGenCalls)
	}
	if result.Err != nil {
		// 客户端侧错误（如不支持的 model、context 超长、参数无效）：
		// 这是用户请求本身的问题，与账号无关，不能让 core 把账号惩罚停用。
		// 显式标记 AccountStatus=OK + 4xx，core 据此既不 failover 也不计入失败。
		var failure *responsesFailureError
		if errors.As(result.Err, &failure) && failure.shouldReturnClientError() {
			fwdResult.StatusCode = failure.StatusCode
			fwdResult.AccountStatus = sdk.AccountStatusOK
			fwdResult.ErrorMessage = failure.Message
			return fwdResult, result.Err
		}
		fwdResult.StatusCode = http.StatusBadGateway
		return fwdResult, result.Err
	}
	fillCostWithImageTool(fwdResult, toolImageIn, toolImageOut)
	return fwdResult, nil
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
