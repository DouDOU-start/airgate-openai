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
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// 转发入口（三模式分发）
// ──────────────────────────────────────────────────────

// forwardHTTP 根据账号凭证类型分发到不同转发模式
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
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
	passHeaders(req.Headers, upstreamReq.Header)

	// 发送请求
	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	// 上游返回错误时，返回 error 让核心决定是否 failover
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		respBody, _ := io.ReadAll(resp.Body)
		return &sdk.ForwardResult{
			StatusCode: resp.StatusCode,
			Duration:   time.Since(start),
		}, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	// /v1/models 路径补齐上下文元信息（不影响其它路由）
	if reqMethod == http.MethodGet && strings.HasPrefix(reqPath, "/v1/models") {
		resp = enrichModelsResponse(resp)
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

	// 建立 WebSocket 连接，透传客户端的缓存与路由相关头
	cfg := WSConfig{
		Token:      account.Credentials["access_token"],
		AccountID:  account.Credentials["chatgpt_account_id"],
		ProxyURL:   account.ProxyURL,
		SessionID:  req.Headers.Get("session_id"),
		TurnState:  req.Headers.Get("x-codex-turn-state"),
		Originator: req.Headers.Get("originator"),
	}
	conn, _, err := DialWebSocket(cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 构建 response.create 消息
	createMsg, err := g.buildWSRequest(req)
	if err != nil {
		return nil, fmt.Errorf("构建 WebSocket 请求失败: %w", err)
	}

	// 发送请求
	if err := conn.WriteJSON(json.RawMessage(createMsg)); err != nil {
		return nil, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
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

	// 读取 WS 消息，转为 SSE 写回客户端
	handler := &sseEventWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		handler.flusher = f
	}
	result := ReceiveWSResponse(ctx, conn, handler)

	// 发送 SSE 结束标记
	if w != nil {
		fmt.Fprint(w, "data: [DONE]\n\n")
		if handler.flusher != nil {
			handler.flusher.Flush()
		}
	}

	fwdResult := &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
		CacheTokens:  result.CacheTokens,
		Model:        result.Model,
		Duration:     time.Since(start),
	}
	if result.Err != nil {
		fwdResult.StatusCode = http.StatusBadGateway
		return fwdResult, result.Err
	}
	return fwdResult, nil
}

// ──────────────────────────────────────────────────────
// SSE 事件写入器（WSEventHandler 实现）
// ──────────────────────────────────────────────────────

// sseEventWriter 将 WS 事件转为 SSE 格式写入 http.ResponseWriter
type sseEventWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseEventWriter) OnTextDelta(string)      {}
func (s *sseEventWriter) OnReasoningDelta(string) {}
func (s *sseEventWriter) OnRateLimits(float64)    {}

func (s *sseEventWriter) OnRawEvent(eventType string, data []byte) {
	if s.w == nil || eventType == "" {
		return
	}
	// 过滤不需要转发给客户端的内部事件
	switch eventType {
	case "codex.rate_limits":
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, strings.ReplaceAll(string(data), "\n", ""))
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

// ──────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────

func isSub2APIAccount(account *sdk.Account) bool {
	if account == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(account.Credentials["provider"]))
	return provider == "sub2api"
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
