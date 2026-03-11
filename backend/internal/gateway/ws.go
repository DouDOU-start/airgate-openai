// WebSocket 连接与事件处理，供网关转发和 cmd/chat 共用
package gateway

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
	Token      string
	AccountID  string
	ProxyURL   string
	SessionID  string // prompt 缓存 key，同 SSE 的 session_id
	TurnState  string // 粘性路由令牌，从上次握手响应获取
	Originator string // 客户端标识，如 "codex_cli_rs"
}

// WSResult 事件解析结果
type WSResult struct {
	Text              string
	Reasoning         string
	StopReason        string
	ToolUses          []ToolUseBlock
	ResponseID        string
	Model             string
	InputTokens       int
	OutputTokens      int
	CacheTokens       int
	CompletedEventRaw []byte
	Duration          time.Duration
	Err               error
}

// ToolUseBlock 表示从 Responses 流中聚合出的工具调用块。
type ToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  *string         `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
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
	if cfg.SessionID != "" {
		headers.Set("session_id", cfg.SessionID)
	}
	if cfg.TurnState != "" {
		headers.Set("x-codex-turn-state", cfg.TurnState)
	}
	if cfg.Originator != "" {
		headers.Set("originator", cfg.Originator)
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
	var reasoningBuilder strings.Builder

	for {
		// 检查 context
		select {
		case <-ctx.Done():
			result.Err = ctx.Err()
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
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
				mergeResponseMetadata(&result, resp)
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
				reasoningBuilder.WriteString(delta)
				if handler != nil {
					handler.OnReasoningDelta(delta)
				}
			}

		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok {
				appendToolUseBlock(&result, item)
			}

		case "response.completed", "response.done":
			result.CompletedEventRaw = append([]byte(nil), msg...)
			if resp, ok := ev["response"].(map[string]any); ok {
				mergeResponseMetadata(&result, resp)
				result.StopReason = jsonString(resp["stop_reason"])
				extractUsageFromResponseMap(&result, resp)
			}
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
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
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
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
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
			return result

		case "error":
			errMsg := string(msg)
			if errObj, ok := ev["error"].(map[string]any); ok {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
			}
			result.Err = fmt.Errorf("WebSocket 错误: %s", errMsg)
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
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

	finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
	return result
}

func finalizeWSResult(result *WSResult, textBuilder, reasoningBuilder *strings.Builder, start time.Time) {
	result.Text = textBuilder.String()
	result.Reasoning = reasoningBuilder.String()
	result.Duration = time.Since(start)
}

func mergeResponseMetadata(result *WSResult, response map[string]any) {
	if id := jsonString(response["id"]); id != "" {
		result.ResponseID = id
	}
	if model := jsonString(response["model"]); model != "" {
		result.Model = model
	}
}

func appendToolUseBlock(result *WSResult, item map[string]any) {
	block := buildToolUseBlock(item)
	if block == nil {
		return
	}
	result.ToolUses = append(result.ToolUses, *block)
}

func buildToolUseBlock(item map[string]any) *ToolUseBlock {
	switch jsonString(item["type"]) {
	case "function_call":
		return buildFunctionCallToolUse(item)
	case "web_search_call":
		return buildWebSearchToolUse(item)
	default:
		return nil
	}
}

func buildFunctionCallToolUse(item map[string]any) *ToolUseBlock {
	name := jsonString(item["name"])
	if name == "" {
		return nil
	}

	id := jsonString(item["call_id"])
	if id == "" {
		id = jsonString(item["id"])
	}

	return &ToolUseBlock{
		Type:  "tool_use",
		ID:    id,
		Name:  stringPointer(name),
		Input: normalizeToolUseInput(jsonString(item["arguments"])),
	}
}

func buildWebSearchToolUse(item map[string]any) *ToolUseBlock {
	name := "web_search"
	return &ToolUseBlock{
		Type:  "tool_use",
		ID:    jsonString(item["id"]),
		Name:  stringPointer(name),
		Input: marshalToolUseInput(item["action"]),
	}
}

func normalizeToolUseInput(raw string) json.RawMessage {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(encoded)
}

func marshalToolUseInput(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(encoded)
}

func jsonString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	v := value
	return &v
}

// JsonInt 从 map[string]any 安全提取 int
func JsonInt(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

// extractUsageFromResponseMap 从 Responses API response 对象中提取 usage 到 WSResult
func extractUsageFromResponseMap(result *WSResult, resp map[string]any) {
	if usage, ok := resp["usage"].(map[string]any); ok {
		result.InputTokens = JsonInt(usage, "input_tokens")
		result.OutputTokens = JsonInt(usage, "output_tokens")
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			result.CacheTokens = JsonInt(details, "cached_tokens")
		}
	}
}
