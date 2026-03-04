package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/gorilla/websocket"
)

// ProxyHandler 将 /v1/* 请求代理给插件
type ProxyHandler struct {
	gateway sdk.SimpleGatewayPlugin
	store   *AccountStore
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// WebSocket 升级检测
	if isWebSocketUpgrade(r) {
		p.handleWebSocket(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleHTTP 处理 HTTP/SSE 请求
func (p *ProxyHandler) handleHTTP(w http.ResponseWriter, r *http.Request) {
	account := p.selectAccount()
	if account == nil {
		http.Error(w, `{"error":"no accounts configured, add one at http://localhost:8080"}`, http.StatusServiceUnavailable)
		return
	}

	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
		return
	}

	// 判断是否流式
	stream := strings.Contains(string(body), `"stream":true`) ||
		strings.Contains(string(body), `"stream": true`)

	headers := r.Header.Clone()
	// 注入转发路径，供插件识别请求类型（如 /v1/messages → Anthropic）
	headers.Set("X-Forwarded-Path", r.URL.Path)

	fwdReq := &sdk.ForwardRequest{
		Account: &sdk.Account{
			ID:          account.ID,
			Credentials: account.Credentials,
			ProxyURL:    account.ProxyURL,
		},
		Body:    body,
		Headers: headers,
		Stream:  stream,
		Writer:  w,
	}

	result, err := p.gateway.Forward(r.Context(), fwdReq)
	if err != nil {
		log.Printf("Forward 失败: %v", err)
		// 如果 Writer 还没被写入，返回错误
		if result == nil || result.StatusCode == 0 {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		}
		return
	}

	log.Printf("Forward 完成: status=%d model=%s input=%d output=%d duration=%s",
		result.StatusCode, result.Model, result.InputTokens, result.OutputTokens, result.Duration)
}

// handleWebSocket 处理 WebSocket 请求
func (p *ProxyHandler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 检查插件是否支持 WebSocket
	wsHandler, ok := p.gateway.(sdk.WebSocketHandler)
	if !ok {
		http.Error(w, `{"error":"plugin does not support websocket"}`, http.StatusNotImplemented)
		return
	}

	account := p.selectAccount()
	if account == nil {
		http.Error(w, `{"error":"no accounts configured"}`, http.StatusServiceUnavailable)
		return
	}

	// 升级 WebSocket 连接
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	defer wsConn.Close()

	// 构建 sdk.WebSocketConn 适配器
	conn := &devWebSocketConn{
		conn: wsConn,
		info: &sdk.WebSocketConnectInfo{
			Path:       r.URL.Path,
			Query:      r.URL.RawQuery,
			Headers:    r.Header,
			RemoteAddr: r.RemoteAddr,
			Account: &sdk.Account{
				ID:          account.ID,
				Credentials: account.Credentials,
				ProxyURL:    account.ProxyURL,
			},
		},
	}

	log.Printf("WebSocket 连接建立: %s, account=%d", r.URL.Path, account.ID)

	if err := wsHandler.HandleWebSocket(context.Background(), conn); err != nil {
		log.Printf("WebSocket 处理结束: %v", err)
	}
}

func (p *ProxyHandler) selectAccount() *DevAccount {
	return p.store.First()
}

// devWebSocketConn 包装 gorilla/websocket.Conn 为 sdk.WebSocketConn
type devWebSocketConn struct {
	conn *websocket.Conn
	info *sdk.WebSocketConnectInfo
}

func (c *devWebSocketConn) ReadMessage() (int, []byte, error) {
	msgType, data, err := c.conn.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	sdkType := sdk.WSMessageText
	if msgType == websocket.BinaryMessage {
		sdkType = sdk.WSMessageBinary
	}
	return sdkType, data, nil
}

func (c *devWebSocketConn) WriteMessage(messageType int, data []byte) error {
	wsType := websocket.TextMessage
	if messageType == sdk.WSMessageBinary {
		wsType = websocket.BinaryMessage
	}
	return c.conn.WriteMessage(wsType, data)
}

func (c *devWebSocketConn) ConnectInfo() *sdk.WebSocketConnectInfo {
	return c.info
}

func (c *devWebSocketConn) Close(code int, reason string) error {
	msg := websocket.FormatCloseMessage(code, reason)
	_ = c.conn.WriteMessage(websocket.CloseMessage, msg)
	return c.conn.Close()
}
