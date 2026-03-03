// 公共 WebSocket 连接与事件处理，供插件主包和 cmd/chat 共用
package openai

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// ChatGPTWSURL OAuth 账号的 WebSocket 端点
	ChatGPTWSURL = "wss://chatgpt.com/backend-api/codex/responses"
	// ChatGPTSSEURL OAuth 账号的 SSE 端点
	ChatGPTSSEURL = "https://chatgpt.com/backend-api/codex/responses"
	// WSBetaHeader WebSocket 协议的 OpenAI-Beta 头
	WSBetaHeader = "responses_websockets=2026-02-04"
	// SSEBetaHeader SSE 协议的 OpenAI-Beta 头
	SSEBetaHeader = "responses=experimental"
)

// WSConfig WebSocket 连接配置
type WSConfig struct {
	Token     string
	AccountID string
	ProxyURL  string
}

// WSResult 事件解析结果
type WSResult struct {
	Text         string
	ResponseID   string
	Model        string
	InputTokens  int
	OutputTokens int
	CacheTokens  int
	Duration     time.Duration
	Err          error
}

// WSEventHandler 事件回调接口，不同场景实现不同输出
type WSEventHandler interface {
	OnTextDelta(delta string)
	OnReasoningDelta(delta string)
	OnRawEvent(eventType string, data []byte) // 插件用来做 SSE 转发
	OnRateLimits(usedPercent float64)
}

// DialWebSocket 建立到上游的 WebSocket 连接
func DialWebSocket(cfg WSConfig) (*websocket.Conn, *http.Response, error) {
	dialer := &websocket.Dialer{
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		HandshakeTimeout: 30 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		EnableCompression: true,
	}

	if cfg.ProxyURL != "" {
		if u, err := url.Parse(cfg.ProxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(u)
		}
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+cfg.Token)
	headers.Set("OpenAI-Beta", WSBetaHeader)
	if cfg.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", cfg.AccountID)
	}

	conn, resp, err := dialer.Dial(ChatGPTWSURL, headers)
	if err != nil {
		if resp != nil {
			return nil, resp, fmt.Errorf("WebSocket 握手失败 (HTTP %d): %w", resp.StatusCode, err)
		}
		return nil, nil, fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	return conn, resp, nil
}

// ReceiveWSResponse 从 WebSocket 读取完整响应，通过 handler 回调输出
func ReceiveWSResponse(ctx context.Context, conn *websocket.Conn, handler WSEventHandler) WSResult {
	start := time.Now()
	result := WSResult{}
	var textBuilder strings.Builder

	for {
		// 检查 context
		select {
		case <-ctx.Done():
			result.Err = ctx.Err()
			result.Text = textBuilder.String()
			result.Duration = time.Since(start)
			return result
		default:
		}

		conn.SetReadDeadline(time.Now().Add(300 * time.Second))

		_, msg, err := conn.ReadMessage()
		if err != nil {
			result.Err = fmt.Errorf("读取 WebSocket 消息失败: %w", err)
			break
		}

		var ev map[string]any
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}

		eventType, _ := ev["type"].(string)

		// 通知 handler 原始事件
		if handler != nil {
			handler.OnRawEvent(eventType, msg)
		}

		switch eventType {
		case "response.created":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.ResponseID = id
				}
			}

		case "response.output_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				textBuilder.WriteString(delta)
				if handler != nil {
					handler.OnTextDelta(delta)
				}
			}

		case "response.reasoning_summary_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				if handler != nil {
					handler.OnReasoningDelta(delta)
				}
			}

		case "response.completed", "response.done":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.ResponseID = id
				}
				if m, ok := resp["model"].(string); ok {
					result.Model = m
				}
				if usage, ok := resp["usage"].(map[string]any); ok {
					result.InputTokens = JsonInt(usage, "input_tokens")
					result.OutputTokens = JsonInt(usage, "output_tokens")
					if details, ok := usage["input_tokens_details"].(map[string]any); ok {
						result.CacheTokens = JsonInt(details, "cached_tokens")
					}
				}
			}
			result.Text = textBuilder.String()
			result.Duration = time.Since(start)
			return result

		case "response.failed":
			errMsg := string(msg)
			if resp, ok := ev["response"].(map[string]any); ok {
				if errObj, ok := resp["error"].(map[string]any); ok {
					if m, ok := errObj["message"].(string); ok {
						errMsg = m
					}
				}
			}
			result.Err = fmt.Errorf("上游错误: %s", errMsg)
			result.Text = textBuilder.String()
			result.Duration = time.Since(start)
			return result

		case "response.incomplete":
			reason := "unknown"
			if resp, ok := ev["response"].(map[string]any); ok {
				if details, ok := resp["incomplete_details"].(map[string]any); ok {
					if r, ok := details["reason"].(string); ok {
						reason = r
					}
				}
			}
			result.Err = fmt.Errorf("响应不完整: %s", reason)
			result.Text = textBuilder.String()
			result.Duration = time.Since(start)
			return result

		case "error":
			errMsg := string(msg)
			if errObj, ok := ev["error"].(map[string]any); ok {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
			}
			result.Err = fmt.Errorf("WebSocket 错误: %s", errMsg)
			result.Text = textBuilder.String()
			result.Duration = time.Since(start)
			return result

		case "codex.rate_limits":
			if handler != nil {
				if rateLimits, ok := ev["rate_limits"].(map[string]any); ok {
					if primary, ok := rateLimits["primary"].(map[string]any); ok {
						if used, ok := primary["used_percent"].(float64); ok {
							handler.OnRateLimits(used)
						}
					}
				}
			}
		}
	}

	result.Text = textBuilder.String()
	result.Duration = time.Since(start)
	return result
}

// JsonInt 从 map[string]any 安全提取 int
func JsonInt(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}
