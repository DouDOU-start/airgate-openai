package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/DouDOU-start/airgate-openai/internal/openai"
)

// ──────────────────────────────────────────────────────
// OAuth 模式：WebSocket 连上游，SSE 写回客户端
// ──────────────────────────────────────────────────────

// forwardOAuth 使用 WebSocket 连接上游，将响应以 SSE 格式写回客户端
func (g *OpenAIGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 建立 WebSocket 连接
	cfg := openai.WSConfig{
		Token:     account.Credentials["access_token"],
		AccountID: account.Credentials["chatgpt_account_id"],
		ProxyURL:  account.ProxyURL,
	}
	conn, _, err := openai.DialWebSocket(cfg)
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
	result := openai.ReceiveWSResponse(ctx, conn, handler)

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
func (s *sseEventWriter) OnReasoningDelta(string)  {}
func (s *sseEventWriter) OnRateLimits(float64)     {}

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
// 请求构建
// ──────────────────────────────────────────────────────

// buildWSRequest 构建 WebSocket response.create 消息
func (g *OpenAIGateway) buildWSRequest(req *sdk.ForwardRequest) ([]byte, error) {
	if isCodexCLI(req.Headers) {
		return buildCodexWSRequest(req.Body, req.Model)
	}
	return buildSimulatedWSRequest(req.Body, req.Model)
}

// buildCodexWSRequest Codex CLI 透传模式
func buildCodexWSRequest(body []byte, model string) ([]byte, error) {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}

	// 如果已有 type=response.create，直接使用
	if t, _ := reqData["type"].(string); t == "response.create" {
		if model != "" {
			reqData["model"] = model
		}
		reqData["store"] = false
		reqData["stream"] = true
		return json.Marshal(reqData)
	}

	// 否则包装为 response.create
	return wrapResponseCreate(reqData, model)
}

// buildSimulatedWSRequest 模拟客户端模式
func buildSimulatedWSRequest(body []byte, model string) ([]byte, error) {
	wrapped, err := wrapAsResponsesAPI(body, model)
	if err != nil {
		return nil, err
	}

	var reqData map[string]any
	if err := json.Unmarshal(wrapped, &reqData); err != nil {
		return nil, fmt.Errorf("解析包装后请求体失败: %w", err)
	}

	return wrapResponseCreate(reqData, model)
}

// wrapResponseCreate 将请求数据包装为 response.create WS 消息
func wrapResponseCreate(data map[string]any, model string) ([]byte, error) {
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
	return json.Marshal(createReq)
}
