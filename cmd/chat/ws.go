package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// wsSession WebSocket 对话会话
type wsSession struct {
	token              string
	accountID          string
	model              string
	proxy              string
	conn               *websocket.Conn
	history            []any
	previousResponseID string
}

// connect 建立 WebSocket 连接
func (s *wsSession) connect() error {
	// Responses WebSocket 端点（OAuth token 走 ChatGPT 后端）
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"

	dialer := &websocket.Dialer{
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		HandshakeTimeout: 30 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		EnableCompression: true,
	}

	if s.proxy != "" {
		if u, err := url.Parse(s.proxy); err == nil {
			dialer.Proxy = http.ProxyURL(u)
		}
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+s.token)
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-04")
	if s.accountID != "" {
		headers.Set("ChatGPT-Account-ID", s.accountID)
	}

	conn, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("WebSocket 握手失败 (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	// 从握手响应头捕获信息
	if resp != nil {
		if model := resp.Header.Get("openai-model"); model != "" {
			fmt.Fprintf(os.Stderr, "[服务端模型: %s]\n", model)
		}
	}

	s.conn = conn
	return nil
}

// close 关闭 WebSocket 连接
func (s *wsSession) close() {
	if s.conn != nil {
		s.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		s.conn.Close()
		s.conn = nil
	}
}

// chat 通过 WebSocket 发送消息并接收响应
func (s *wsSession) chat(input string) error {
	if s.conn == nil {
		if err := s.connect(); err != nil {
			return err
		}
	}

	// 构建用户消息
	userMsg := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": input},
		},
	}

	// 构建 response.create 消息
	createReq := map[string]any{
		"type":         "response.create",
		"model":        s.model,
		"instructions": codexInstructions,
		"stream":       true,
		"store":        false,
	}

	if s.previousResponseID != "" {
		createReq["previous_response_id"] = s.previousResponseID
		createReq["input"] = []any{userMsg}
	} else {
		allInput := make([]any, 0, len(s.history)+1)
		allInput = append(allInput, s.history...)
		allInput = append(allInput, userMsg)
		createReq["input"] = allInput
	}

	// 发送
	msg, _ := json.Marshal(createReq)
	if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		// 连接断开，尝试重连
		s.conn = nil
		return fmt.Errorf("发送失败（连接可能断开）: %w", err)
	}

	// 接收并解析响应
	result := s.receiveResponse()

	// 更新历史
	s.history = append(s.history, userMsg)
	if result.text != "" {
		s.history = append(s.history, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "output_text", "text": result.text},
			},
		})
	}
	if result.responseID != "" {
		s.previousResponseID = result.responseID
	}

	// 打印统计
	if result.inputTokens > 0 || result.outputTokens > 0 {
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			result.model, result.inputTokens, result.outputTokens, result.cacheTokens,
			result.duration.Round(time.Millisecond))
	}

	return result.err
}

// receiveResponse 从 WebSocket 接收完整响应
func (s *wsSession) receiveResponse() sseResult {
	start := time.Now()
	result := sseResult{}
	var textBuilder strings.Builder

	for {
		// 设置读取超时
		s.conn.SetReadDeadline(time.Now().Add(300 * time.Second))

		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			result.err = fmt.Errorf("读取 WebSocket 消息失败: %w", err)
			s.conn = nil // 标记连接失效
			break
		}

		var ev map[string]any
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}

		eventType, _ := ev["type"].(string)

		switch eventType {
		case "response.created":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.responseID = id
				}
			}

		case "response.output_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				fmt.Print(delta)
				textBuilder.WriteString(delta)
			}

		case "response.reasoning_summary_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", delta)
			}

		case "response.completed", "response.done":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.responseID = id
				}
				if m, ok := resp["model"].(string); ok {
					result.model = m
				}
				if usage, ok := resp["usage"].(map[string]any); ok {
					result.inputTokens = jsonInt(usage, "input_tokens")
					result.outputTokens = jsonInt(usage, "output_tokens")
					if details, ok := usage["input_tokens_details"].(map[string]any); ok {
						result.cacheTokens = jsonInt(details, "cached_tokens")
					}
				}
			}
			result.text = textBuilder.String()
			result.duration = time.Since(start)
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
			result.err = fmt.Errorf("上游错误: %s", errMsg)
			result.text = textBuilder.String()
			result.duration = time.Since(start)
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
			result.err = fmt.Errorf("响应不完整: %s", reason)
			result.text = textBuilder.String()
			result.duration = time.Since(start)
			return result

		case "error":
			// WebSocket 级别错误
			errMsg := string(msg)
			if errObj, ok := ev["error"].(map[string]any); ok {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
			}
			result.err = fmt.Errorf("WebSocket 错误: %s", errMsg)
			result.text = textBuilder.String()
			result.duration = time.Since(start)
			return result

		case "codex.rate_limits":
			// 速率限制信息，仅日志
			if rateLimits, ok := ev["rate_limits"].(map[string]any); ok {
				if primary, ok := rateLimits["primary"].(map[string]any); ok {
					if used, ok := primary["used_percent"].(float64); ok && used > 80 {
						fmt.Fprintf(os.Stderr, "\n[速率限制: %.1f%%]", used)
					}
				}
			}
		}
	}

	result.text = textBuilder.String()
	result.duration = time.Since(start)
	return result
}
