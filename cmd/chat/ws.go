package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/DouDOU-start/airgate-openai/internal/openai"
	"github.com/gorilla/websocket"
)

// wsSession WebSocket 对话会话
type wsSession struct {
	cfg                openai.WSConfig
	model              string
	conn               *websocket.Conn
	history            []any
	previousResponseID string
}

// connect 建立 WebSocket 连接
func (s *wsSession) connect() error {
	conn, resp, err := openai.DialWebSocket(s.cfg)
	if err != nil {
		return err
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
		s.conn = nil
		return fmt.Errorf("发送失败（连接可能断开）: %w", err)
	}

	// 接收并解析响应
	handler := &terminalHandler{}
	result := openai.ReceiveWSResponse(context.Background(), s.conn, handler)

	// 连接断开标记
	if result.Err != nil && strings.Contains(result.Err.Error(), "读取 WebSocket 消息失败") {
		s.conn = nil
	}

	// 更新历史
	s.history = append(s.history, userMsg)
	if result.Text != "" {
		s.history = append(s.history, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "output_text", "text": result.Text},
			},
		})
	}
	if result.ResponseID != "" {
		s.previousResponseID = result.ResponseID
	}

	// 打印统计
	if result.InputTokens > 0 || result.OutputTokens > 0 {
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			result.Model, result.InputTokens, result.OutputTokens, result.CacheTokens,
			result.Duration.Round(time.Millisecond))
	}

	return result.Err
}

// terminalHandler 终端输出的事件处理器
type terminalHandler struct{}

func (h *terminalHandler) OnTextDelta(delta string) {
	fmt.Print(delta)
}

func (h *terminalHandler) OnReasoningDelta(delta string) {
	fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", delta)
}

func (h *terminalHandler) OnRawEvent(string, []byte) {}

func (h *terminalHandler) OnRateLimits(used float64) {
	if used > 80 {
		fmt.Fprintf(os.Stderr, "\n[速率限制: %.1f%%]", used)
	}
}
