// WebSocket 连接与事件处理，供网关转发和 cmd/chat 共用
package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const (
	// ChatGPTWSURL OAuth 账号的 WebSocket 端点
	ChatGPTWSURL = "wss://chatgpt.com/backend-api/codex/responses"
	// ChatGPTSSEURL OAuth 账号的 SSE 端点
	ChatGPTSSEURL = "https://chatgpt.com/backend-api/codex/responses"
	// WSBetaHeader WebSocket 协议的 OpenAI-Beta 头（仅 WS 模式需要）
	WSBetaHeader = "responses_websockets=2026-02-06"
)

// WSConfig WebSocket 连接配置
type WSConfig struct {
	URL            string
	Token          string
	AccountID      string
	ProxyURL       string
	SessionID      string // prompt 缓存 key，同 SSE 的 session_id
	ConversationID string
	TurnState      string // 粘性路由令牌，从上次握手响应获取
	Originator     string // 客户端标识，如 "codex_cli_rs"
	UserAgent      string
	Headers        http.Header
}

// WSResult 事件解析结果
type WSResult struct {
	Text                  string
	Reasoning             string
	StopReason            string
	ToolUses              []ToolUseBlock
	ResponseID            string
	Model                 string
	InputTokens           int
	OutputTokens          int
	CachedInputTokens     int
	ReasoningOutputTokens int
	// ToolImageInputTokens / ToolImageOutputTokens 来自 response.usage.tool_usage.image_gen
	// 或 response.completed 的 tool_usage 字段。按 gpt-image-1.5 单价单独计费，
	// 因为主 model（通常是 gpt-5.4）与图像工具内部模型的单价不同。
	ToolImageInputTokens  int
	ToolImageOutputTokens int
	// ImageGenCalls 捕获 Responses API 返回的 image_generation_call output items，
	// 供 REST→tools 翻译路径将 base64 结果打包回 OpenAI Images REST 响应。
	ImageGenCalls []ImageGenCall
	// ToolImageModel 是 response.tools[0].model —— 上游实际为 image_generation
	// 工具选用的内部模型（客户端请求 gpt-image-2 时可能被静默降级为 gpt-image-1.5）。
	ToolImageModel    string
	CompletedEventRaw []byte
	FailedEventRaw    []byte
	Duration          time.Duration
	Err               error
}

// ImageGenCall 对应 Responses API 输出项中 type="image_generation_call" 的一条记录。
type ImageGenCall struct {
	Result        string // base64 编码的图像
	Size          string
	Quality       string
	OutputFormat  string
	Background    string
	RevisedPrompt string
	Model         string // 上游实际使用的图像模型（如 gpt-image-1.5）
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
	targetURL := cfg.URL
	if strings.TrimSpace(targetURL) == "" {
		targetURL = ChatGPTWSURL
	}

	headers := cloneHTTPHeader(cfg.Headers)
	if headers == nil {
		headers = http.Header{}
	}
	if cfg.Token != "" {
		headers.Set("Authorization", "Bearer "+cfg.Token)
	}
	if headers.Get("OpenAI-Beta") == "" {
		headers.Set("OpenAI-Beta", WSBetaHeader)
	}
	if cfg.AccountID != "" && headers.Get("ChatGPT-Account-ID") == "" {
		headers.Set("ChatGPT-Account-ID", cfg.AccountID)
	}
	if cfg.SessionID != "" && headers.Get("session_id") == "" {
		headers.Set("session_id", cfg.SessionID)
	}
	if cfg.ConversationID != "" && headers.Get("conversation_id") == "" {
		headers.Set("conversation_id", cfg.ConversationID)
	}
	if cfg.TurnState != "" && headers.Get("x-codex-turn-state") == "" {
		headers.Set("x-codex-turn-state", cfg.TurnState)
	}
	if cfg.Originator != "" && headers.Get("originator") == "" {
		headers.Set("originator", cfg.Originator)
	}
	if cfg.UserAgent != "" && headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", cfg.UserAgent)
	}

	return dialWebSocket(targetURL, cfg.ProxyURL, headers)
}

func dialWebSocket(targetURL, proxyURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := &websocket.Dialer{
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		HandshakeTimeout: 30 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		EnableCompression: true,
	}

	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(u)
		}
	}

	conn, resp, err := dialer.Dial(targetURL, headers)
	if err != nil {
		return nil, resp, formatWebSocketDialError(resp, err)
	}

	return conn, resp, nil
}

func formatWebSocketDialError(resp *http.Response, err error) error {
	if resp != nil {
		// 尝试读取上游响应体中的错误详情
		upstreamMsg := ""
		if resp.Body != nil {
			if body, readErr := io.ReadAll(resp.Body); readErr == nil && len(body) > 0 {
				// 尝试提取 JSON 中的 error.message
				if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
					upstreamMsg = msg
				} else {
					upstreamMsg = truncate(string(body), 200)
				}
			}
		}
		hint := ""
		switch resp.StatusCode {
		case 401:
			hint = "认证失败，access_token 已过期或账号已被停用"
		case 403:
			hint = "访问被拒绝，账号可能已被禁用或无权限"
		case 429:
			hint = "请求过于频繁，请稍后重试"
		}
		if hint != "" {
			if upstreamMsg != "" {
				return fmt.Errorf("%s: %s (HTTP %d)", hint, upstreamMsg, resp.StatusCode)
			}
			return fmt.Errorf("%s (HTTP %d)", hint, resp.StatusCode)
		}
		if upstreamMsg != "" {
			return fmt.Errorf("WebSocket 握手失败: %s (HTTP %d)", upstreamMsg, resp.StatusCode)
		}
		return fmt.Errorf("WebSocket 握手失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	return fmt.Errorf("WebSocket 连接失败: %w", err)
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	cloned := make(http.Header, len(headers))
	for k, values := range headers {
		copied := append([]string(nil), values...)
		cloned[k] = copied
	}
	return cloned
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

		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Second)); err != nil {
			result.Err = fmt.Errorf("设置 WebSocket 读取超时失败: %w", err)
			break
		}

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
				collectImageGenCall(&result, item)
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
			result.FailedEventRaw = append([]byte(nil), msg...)
			if failure := classifyResponsesFailure(msg); failure != nil {
				result.Err = failure
			} else {
				result.Err = fmt.Errorf("上游错误: %s", string(msg))
			}
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
			result.FailedEventRaw = append([]byte(nil), msg...)
			if failure := classifyWSErrorEvent(msg); failure != nil {
				result.Err = failure
			} else {
				errMsg := string(msg)
				if errObj, ok := ev["error"].(map[string]any); ok {
					if m, ok := errObj["message"].(string); ok {
						errMsg = m
					}
				}
				result.Err = fmt.Errorf("WebSocket 错误: %s", errMsg)
			}
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
	// 从 response.tools[] 里扫出 image_generation 工具的实际 model 字段。
	// 上游会把生效后的 tool 配置回写到 response.tools 里（客户端传的 gpt-image-2
	// 会被替换为上游真正使用的值，比如 gpt-image-1.5）。
	if tools, ok := response["tools"].([]any); ok {
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if jsonString(tm["type"]) != "image_generation" {
				continue
			}
			if m := jsonString(tm["model"]); m != "" {
				result.ToolImageModel = m
			}
			break
		}
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
// cached_tokens 从 input_tokens 中扣除，与 Codex 行为一致
func extractUsageFromResponseMap(result *WSResult, resp map[string]any) {
	if usage, ok := resp["usage"].(map[string]any); ok {
		result.InputTokens = JsonInt(usage, "input_tokens")
		result.OutputTokens = JsonInt(usage, "output_tokens")
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			result.CachedInputTokens = JsonInt(details, "cached_tokens")
		}
		if details, ok := usage["output_tokens_details"].(map[string]any); ok {
			result.ReasoningOutputTokens = JsonInt(details, "reasoning_tokens")
		}
		// 从 input_tokens 中扣除缓存部分，避免计费重复计算
		if result.CachedInputTokens > 0 && result.InputTokens >= result.CachedInputTokens {
			result.InputTokens -= result.CachedInputTokens
		}
	}
	// ChatGPT OAuth 响应把 image_generation tool 的用量放在 response.tool_usage.image_gen，
	// 与主 usage 分开上报。这里提取出来让计费层按 gpt-image-1.5 单价额外计费。
	if toolUsage, ok := resp["tool_usage"].(map[string]any); ok {
		if img, ok := toolUsage["image_gen"].(map[string]any); ok {
			result.ToolImageInputTokens = JsonInt(img, "input_tokens")
			result.ToolImageOutputTokens = JsonInt(img, "output_tokens")
		}
	}
}

// collectImageGenCall 读取 response.output_item.done 里 type=image_generation_call 的 item，
// 把 base64 结果与尺寸/质量等元信息累加到 WSResult。仅在 REST→tools 翻译路径使用。
func collectImageGenCall(result *WSResult, item map[string]any) {
	if jsonString(item["type"]) != "image_generation_call" {
		return
	}
	call := ImageGenCall{
		Result:        jsonString(item["result"]),
		Size:          jsonString(item["size"]),
		Quality:       jsonString(item["quality"]),
		OutputFormat:  jsonString(item["output_format"]),
		Background:    jsonString(item["background"]),
		RevisedPrompt: jsonString(item["revised_prompt"]),
		Model:         jsonString(item["model"]),
	}
	if call.Result == "" {
		return
	}
	result.ImageGenCalls = append(result.ImageGenCalls, call)
}
