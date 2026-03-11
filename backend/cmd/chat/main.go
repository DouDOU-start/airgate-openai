// 交互式测试：用 OAuth access_token 与 ChatGPT Codex Responses API 对话
// 用法: go run ./cmd/chat -token <access_token>
// 支持多轮对话、SSE/WebSocket 双协议、/clear 清空历史
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
)

func main() {
	token := flag.String("token", "", "OAuth access_token")
	accountID := flag.String("account-id", "", "ChatGPT Account ID（可选）")
	model := flag.String("model", "gpt-5.3-codex", "模型名称")
	proxy := flag.String("proxy", "", "代理地址（可选）")
	useWS := flag.Bool("ws", false, "使用 WebSocket 协议（默认 SSE）")
	reasoning := flag.String("reasoning", "medium", "思考强度: none/minimal/low/medium/high/xhigh")
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
			model:     *model,
			cacheKey:  generateCacheKey(),
			reasoning: *reasoning,
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
			reasoning: *reasoning,
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
