// 交互式测试：用 OAuth access_token 与 ChatGPT Codex Responses API 对话
// 用法: go run ./cmd/chat -token <access_token>
// 支持多轮对话、SSE/WebSocket 双协议、/clear 清空历史
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
	"github.com/DouDOU-start/airgate-openai/backend/resources"
	"github.com/gorilla/websocket"
)

func main() {
	token := flag.String("token", "", "OAuth access_token")
	accountID := flag.String("account-id", "", "ChatGPT Account ID（可选）")
	model := flag.String("model", "gpt-5.3-codex", "模型名称")
	proxy := flag.String("proxy", "", "代理地址（可选）")
	useWS := flag.Bool("ws", false, "使用 WebSocket 协议（默认 SSE）")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("OPENAI_ACCESS_TOKEN")
	}
	if *accountID == "" {
		*accountID = os.Getenv("CHATGPT_ACCOUNT_ID")
	}
	if *proxy == "" {
		*proxy = os.Getenv("HTTP_PROXY")
	}

	if *token == "" {
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/chat -token <access_token>")
		os.Exit(1)
	}

	// 选择协议模式
	type chatter interface {
		chat(input string) error
	}

	var session chatter
	proto := "SSE"

	if *useWS {
		proto = "WebSocket"
		ws := &wsSession{
			cfg: gateway.WSConfig{
				Token:     *token,
				AccountID: *accountID,
				ProxyURL:  *proxy,
			},
			model:    *model,
			cacheKey: generateCacheKey(),
		}
		defer ws.close()
		session = ws
	} else {
		session = &sseSession{
			client:    buildClient(*proxy),
			token:     *token,
			accountID: *accountID,
			model:     *model,
			cacheKey:  generateCacheKey(),
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("模型: %s | 协议: %s | /clear 清空对话 | Ctrl+C 退出\n\n", *model, proto)

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		// 命令处理
		if input == "/clear" {
			switch s := session.(type) {
			case *sseSession:
				s.history = nil
				s.turnState = ""
				s.cacheKey = generateCacheKey()
			case *wsSession:
				s.history = nil
				s.previousResponseID = ""
				s.cacheKey = generateCacheKey()
				s.turnState = ""
				s.close()
			}
			fmt.Println("对话已清空")
			continue
		}

		err := session.chat(input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		}
		fmt.Println()
	}
}

// ──────────────────────────────────────────────────────
// SSE 模式
// ──────────────────────────────────────────────────────

// sseSession SSE 对话会话状态
type sseSession struct {
	client    *http.Client
	token     string
	accountID string
	model     string
	history   []any  // 累积的 input 消息
	turnState string // 粘性路由令牌
	cacheKey  string // prompt 缓存 key，同一会话内保持不变
}

func (s *sseSession) chat(input string) error {
	userMsg := buildUserMsg(input)

	allInput := make([]any, 0, len(s.history)+1)
	allInput = append(allInput, s.history...)
	allInput = append(allInput, userMsg)

	reqBody := map[string]any{
		"model":            s.model,
		"instructions":     resources.Instructions,
		"input":            allInput,
		"stream":           true,
		"store":            false,
		"prompt_cache_key": s.cacheKey,
	}

	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, gateway.ChatGPTSSEURL, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", gateway.SSEBetaHeader)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("session_id", s.cacheKey)
	req.Host = "chatgpt.com"

	if s.accountID != "" {
		req.Header.Set("chatgpt-account-id", s.accountID)
	}
	if s.turnState != "" {
		req.Header.Set("x-codex-turn-state", s.turnState)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if ts := resp.Header.Get("x-codex-turn-state"); ts != "" {
		s.turnState = ts
	}

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	// 复用 gateway.ParseSSEStream 解析 SSE 流
	handler := &terminalHandler{}
	result := gateway.ParseSSEStream(resp.Body, handler)

	s.history = append(s.history, userMsg)
	if result.Text != "" {
		s.history = append(s.history, buildAssistantMsg(result.Text))
	}

	printStats(result.Model, result.InputTokens, result.OutputTokens, result.CacheTokens, result.Duration)

	return result.Err
}

// ──────────────────────────────────────────────────────
// WebSocket 模式
// ──────────────────────────────────────────────────────

// wsSession WebSocket 对话会话
type wsSession struct {
	cfg                gateway.WSConfig
	model              string
	conn               *websocket.Conn
	history            []any
	previousResponseID string
	cacheKey           string
	turnState          string // 粘性路由令牌
}

func (s *wsSession) connect() error {
	cfg := s.cfg
	cfg.SessionID = s.cacheKey
	cfg.Originator = "codex_cli_rs"
	cfg.TurnState = s.turnState

	conn, resp, err := gateway.DialWebSocket(cfg)
	if err != nil {
		return err
	}

	if resp != nil {
		if model := resp.Header.Get("openai-model"); model != "" {
			fmt.Fprintf(os.Stderr, "[服务端模型: %s]\n", model)
		}
		if ts := resp.Header.Get("x-codex-turn-state"); ts != "" {
			s.turnState = ts
		}
	}

	s.conn = conn
	return nil
}

func (s *wsSession) close() {
	if s.conn != nil {
		s.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		s.conn.Close()
		s.conn = nil
	}
}

func (s *wsSession) chat(input string) error {
	if s.conn == nil {
		if err := s.connect(); err != nil {
			return err
		}
	}

	userMsg := buildUserMsg(input)

	createReq := map[string]any{
		"type":             "response.create",
		"model":            s.model,
		"instructions":     resources.Instructions,
		"stream":           true,
		"store":            false,
		"prompt_cache_key": s.cacheKey,
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

	msg, _ := json.Marshal(createReq)
	if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		s.conn = nil
		return fmt.Errorf("发送失败（连接可能断开）: %w", err)
	}

	// 复用 gateway.ReceiveWSResponse 解析 WebSocket 响应
	handler := &terminalHandler{}
	result := gateway.ReceiveWSResponse(context.Background(), s.conn, handler)

	if result.Err != nil && strings.Contains(result.Err.Error(), "读取 WebSocket 消息失败") {
		s.conn = nil
	}

	s.history = append(s.history, userMsg)
	if result.Text != "" {
		s.history = append(s.history, buildAssistantMsg(result.Text))
	}
	if result.ResponseID != "" {
		s.previousResponseID = result.ResponseID
	}

	printStats(result.Model, result.InputTokens, result.OutputTokens, result.CacheTokens, result.Duration)

	return result.Err
}

// ──────────────────────────────────────────────────────
// 终端事件处理器（SSE 和 WebSocket 共用）
// ──────────────────────────────────────────────────────

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

// ──────────────────────────────────────────────────────
// 公共工具
// ──────────────────────────────────────────────────────

func buildUserMsg(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": text},
		},
	}
}

func buildAssistantMsg(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "output_text", "text": text},
		},
	}
}

func printStats(model string, input, output, cache int, duration time.Duration) {
	if input > 0 || output > 0 {
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			model, input, output, cache, duration.Round(time.Millisecond))
	}
}

func generateCacheKey() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return "chat-" + string(b)
}

func buildClient(proxy string) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport, Timeout: 300 * time.Second}
}
