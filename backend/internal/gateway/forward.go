package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

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

	account := req.Account

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req)
	}
	if account.Credentials["access_token"] != "" {
		return g.forwardOAuth(ctx, req)
	}
	return nil, fmt.Errorf("账号缺少 api_key 或 access_token")
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
	// 预处理请求体（含 model 同步与上下文预算守卫）
	body := req.Body
	if methodAllowsBody(reqMethod) {
		body = preprocessRequestBody(body, req.Model, reqPath)
		body = applyForceInstructions(body, req.Headers)
	}

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
		upstreamReq.Header.Set("Content-Type", "application/json")
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
			AccountStatus: accountStatusFromCode(resp.StatusCode),
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
		if wsResp != nil && (wsResp.StatusCode == 401 || wsResp.StatusCode == 403) {
			return &sdk.ForwardResult{
				StatusCode:    wsResp.StatusCode,
				Duration:      time.Since(start),
				AccountStatus: accountStatusFromCode(wsResp.StatusCode),
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

	var lastHandler *sseEventWriter
	runAttempt := func(msg []byte, w http.ResponseWriter) (WSResult, error) {
		if err := conn.WriteJSON(json.RawMessage(msg)); err != nil {
			return WSResult{}, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
		}
		handler := &sseEventWriter{w: w, accountID: account.ID, sessionKey: session.SessionKey, start: start}
		if f, ok := w.(http.Flusher); ok {
			handler.flusher = f
		}
		lastHandler = handler
		return ReceiveWSResponse(ctx, conn, handler), nil
	}

	// 设置 SSE 响应头
	w := req.Writer
	if w != nil {
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
	if result.Err != nil && (strings.Contains(result.Err.Error(), "上游续链锚点失效") || strings.Contains(strings.ToLower(result.Err.Error()), "previous response")) {
		if retried, changed := dropPreviousResponseIDFromJSON(createMsg); changed {
			g.logger.Warn("检测到 previous_response_id 失效，降级为 full create 重试一次", "session", session.SessionKey)
			result, err = runAttempt(retried, w)
			if err != nil {
				return nil, err
			}
		}
	}
	if session.SessionKey != "" {
		if result.ResponseID != "" {
			updateSessionStateResponseID(session.SessionKey, result.ResponseID)
		}
	}

	// 发送 SSE 结束标记
	if w != nil {
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}

	elapsed := time.Since(start)
	firstTokenMs := elapsed.Milliseconds()
	if lastHandler != nil && lastHandler.firstTokenMs > 0 {
		firstTokenMs = lastHandler.firstTokenMs
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
	if result.Err != nil {
		fwdResult.StatusCode = http.StatusBadGateway
		return fwdResult, result.Err
	}
	fillCost(fwdResult)
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
func (g *OpenAIGateway) buildHTTPClient(account *sdk.Account) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	if account.ProxyURL != "" {
		if proxyURL, err := url.Parse(account.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   g.requestTimeout(),
	}
}
