// 交互式测试：用 OAuth access_token 与 ChatGPT Codex Responses API 对话
// 用法: go run ./cmd/chat -token <access_token>
// 支持多轮对话、SSE 事件完整解析、/clear 清空历史
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	_ "embed"
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
)

//go:embed instructions.md
var codexInstructions string

// message 对话消息
type message struct {
	Role    string `json:"role"`
	Type    string `json:"type"`
	Content any    `json:"content"`
}

// chatSession 对话会话状态
type chatSession struct {
	client    *http.Client
	token     string
	accountID string
	model     string
	history   []any  // 累积的 input 消息
	turnState string // 粘性路由令牌
	cacheKey  string // prompt 缓存 key，同一会话内保持不变
}

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
			token:     *token,
			accountID: *accountID,
			model:     *model,
			proxy:     *proxy,
		}
		defer ws.close()
		session = ws
	} else {
		session = &chatSession{
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
			case *chatSession:
				s.history = nil
				s.turnState = ""
				s.cacheKey = generateCacheKey()
			case *wsSession:
				s.history = nil
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

func (s *chatSession) chat(input string) error {
	// 构建用户消息
	userMsg := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": input},
		},
	}

	// 构建 input：历史消息 + 当前消息
	allInput := make([]any, 0, len(s.history)+1)
	allInput = append(allInput, s.history...)
	allInput = append(allInput, userMsg)

	reqBody := map[string]any{
		"model":            s.model,
		"instructions":     codexInstructions,
		"input":            allInput,
		"stream":           true,
		"store":            false,
		"prompt_cache_key": s.cacheKey,
	}

	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}

	// 设置请求头
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
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

	// 捕获响应头中的 turn_state
	if ts := resp.Header.Get("x-codex-turn-state"); ts != "" {
		s.turnState = ts
	}

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	// 解析 SSE 流
	result := s.parseSSE(resp.Body)

	// 将用户消息和助手回复追加到历史
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

	// 打印统计信息
	if result.inputTokens > 0 || result.outputTokens > 0 {
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			result.model, result.inputTokens, result.outputTokens, result.cacheTokens,
			result.duration.Round(time.Millisecond))
	}

	if result.err != nil {
		return result.err
	}
	return nil
}

// sseResult SSE 解析结果
type sseResult struct {
	text         string        // 累积的完整文本
	responseID   string        // 响应 ID
	model        string        // 实际使用的模型
	inputTokens  int
	outputTokens int
	cacheTokens  int
	duration     time.Duration
	err          error
}

// parseSSE 解析 SSE 流，返回解析结果
func (s *chatSession) parseSSE(body io.Reader) sseResult {
	start := time.Now()
	result := sseResult{}
	var textBuilder strings.Builder

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// 兼容 "data: xxx" 和 "data:xxx"
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimLeft(data, " ")
		if data == "[DONE]" {
			break
		}

		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}

		eventType, _ := ev["type"].(string)

		switch eventType {
		// 响应创建
		case "response.created":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.responseID = id
				}
			}

		// 文本增量
		case "response.output_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				fmt.Print(delta)
				textBuilder.WriteString(delta)
			}

		// 推理摘要增量
		case "response.reasoning_summary_text.delta":
			if delta, ok := ev["delta"].(string); ok {
				fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", delta) // 灰色输出推理
			}

		// 响应完成
		case "response.completed", "response.done":
			if resp, ok := ev["response"].(map[string]any); ok {
				if id, ok := resp["id"].(string); ok {
					result.responseID = id
				}
				if m, ok := resp["model"].(string); ok {
					result.model = m
				}
				// 提取 usage
				if usage, ok := resp["usage"].(map[string]any); ok {
					result.inputTokens = jsonInt(usage, "input_tokens")
					result.outputTokens = jsonInt(usage, "output_tokens")
					// 缓存 tokens
					if details, ok := usage["input_tokens_details"].(map[string]any); ok {
						result.cacheTokens = jsonInt(details, "cached_tokens")
					}
				}
			}

		// 响应失败
		case "response.failed":
			errMsg := data
			if resp, ok := ev["response"].(map[string]any); ok {
				if errObj, ok := resp["error"].(map[string]any); ok {
					if msg, ok := errObj["message"].(string); ok {
						errMsg = msg
					}
				}
			}
			result.err = fmt.Errorf("上游错误: %s", errMsg)

		// 响应不完整
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
		}
	}

	if err := scanner.Err(); err != nil && result.err == nil {
		result.err = fmt.Errorf("读取 SSE 失败: %w", err)
	}

	result.text = textBuilder.String()
	result.duration = time.Since(start)
	return result
}

// jsonInt 从 map 中安全提取 int
func jsonInt(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

// generateCacheKey 生成随机的 prompt 缓存 key
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
