package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// wsDialResult 封装 DialWebSocket 的认证失败信息
type wsDialResult struct {
	statusCode   int
	errorMessage string
}

// HandleWebSocket 处理入站 WebSocket 连接（实现 sdk.GatewayPlugin）
// 流程：客户端 WS <-> gRPC 双向流 <-> 插件 <-> 上游 WS
func (g *OpenAIGateway) HandleWebSocket(ctx context.Context, conn sdk.WebSocketConn) (*sdk.ForwardResult, error) {
	start := time.Now()
	info := conn.ConnectInfo()
	if info.Account == nil {
		return nil, fmt.Errorf("未提供账户信息")
	}

	account := info.Account

	// 根据凭证类型选择上游连接方式
	var err error
	var dialInfo *wsDialResult
	if account.Credentials["access_token"] != "" {
		dialInfo, err = g.handleWSWithOAuth(ctx, conn, account)
	} else if account.Credentials["api_key"] != "" {
		dialInfo, err = g.handleWSWithAPIKey(ctx, conn, account)
	} else {
		return nil, fmt.Errorf("账号缺少 api_key 或 access_token")
	}

	result := &sdk.ForwardResult{
		StatusCode: http.StatusOK,
		Duration:   time.Since(start),
	}
	if err != nil {
		result.StatusCode = http.StatusBadGateway
		// 如果是认证失败，设置 AccountStatus 让核心正确处理
		if dialInfo != nil && (dialInfo.statusCode == 401 || dialInfo.statusCode == 403 || dialInfo.statusCode == 429) {
			result.StatusCode = dialInfo.statusCode
			result.AccountStatus = accountStatusFromMessage(dialInfo.statusCode, dialInfo.errorMessage)
			result.ErrorMessage = dialInfo.errorMessage
		}
	}
	return result, err
}

// handleWSWithOAuth 使用上游 WebSocket 直通（端到端 WS 桥接）
func (g *OpenAIGateway) handleWSWithOAuth(ctx context.Context, clientConn sdk.WebSocketConn, account *sdk.Account) (*wsDialResult, error) {
	cfg := WSConfig{
		Token:     account.Credentials["access_token"],
		AccountID: account.Credentials["chatgpt_account_id"],
		ProxyURL:  account.ProxyURL,
	}
	upstreamConn, wsResp, err := DialWebSocket(cfg)
	if err != nil {
		var info *wsDialResult
		if wsResp != nil {
			info = &wsDialResult{statusCode: wsResp.StatusCode, errorMessage: err.Error()}
		}
		return info, fmt.Errorf("连接上游 WebSocket 失败: %w", err)
	}
	defer func() {
		_ = upstreamConn.Close()
	}()

	g.logger.Info("上游 WebSocket 连接已建立", "account_id", account.ID)

	return nil, bridgeWebSocket(ctx, clientConn, upstreamConn)
}

// handleWSWithAPIKey API Key 模式下的 WS 桥接
func (g *OpenAIGateway) handleWSWithAPIKey(ctx context.Context, clientConn sdk.WebSocketConn, account *sdk.Account) (*wsDialResult, error) {
	cfg := WSConfig{
		Token:    account.Credentials["api_key"],
		ProxyURL: account.ProxyURL,
	}
	upstreamConn, wsResp, err := DialWebSocket(cfg)
	if err != nil {
		var info *wsDialResult
		if wsResp != nil {
			info = &wsDialResult{statusCode: wsResp.StatusCode, errorMessage: err.Error()}
		}
		return info, fmt.Errorf("连接上游 WebSocket 失败: %w", err)
	}
	defer func() {
		_ = upstreamConn.Close()
	}()

	g.logger.Info("上游 WebSocket 连接已建立（API Key）", "account_id", account.ID)

	return nil, bridgeWebSocket(ctx, clientConn, upstreamConn)
}

// bridgeWebSocket 双向桥接客户端和上游的 WebSocket 消息
func bridgeWebSocket(ctx context.Context, clientConn sdk.WebSocketConn, upstreamConn *websocket.Conn) error {
	errCh := make(chan error, 2)

	// 客户端 → 上游
	go func() {
		for {
			msgType, data, err := clientConn.ReadMessage()
			if err != nil {
				errCh <- fmt.Errorf("读取客户端消息: %w", err)
				return
			}
			wsType := websocket.TextMessage
			if msgType == sdk.WSMessageBinary {
				wsType = websocket.BinaryMessage
			}
			if err := upstreamConn.WriteMessage(wsType, data); err != nil {
				errCh <- fmt.Errorf("写入上游消息: %w", err)
				return
			}
		}
	}()

	// 上游 → 客户端
	go func() {
		for {
			wsType, data, err := upstreamConn.ReadMessage()
			if err != nil {
				errCh <- fmt.Errorf("读取上游消息: %w", err)
				return
			}
			msgType := sdk.WSMessageText
			if wsType == websocket.BinaryMessage {
				msgType = sdk.WSMessageBinary
			}
			if err := clientConn.WriteMessage(msgType, data); err != nil {
				errCh <- fmt.Errorf("写入客户端消息: %w", err)
				return
			}
		}
	}()

	// 等待任一方向结束或 context 取消
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return nil
		}
		return err
	}
}
