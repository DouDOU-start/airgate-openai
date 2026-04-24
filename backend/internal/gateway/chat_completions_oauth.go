package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// OAuth/Codex 上游永远返回 Responses API 的 SSE 流，但客户端走
// /v1/chat/completions 时期望的是 Chat Completions 的 JSON / chunk 协议。
// 本文件把 Responses 事件翻译成 Chat Completions 协议，让标准 OpenAI SDK
// 的 client.chat.completions.create(...) 在 OAuth 账号上也能直接可用。

// isChatCompletionsRequest 判断客户端请求的是不是 /v1/chat/completions。
// 优先看 X-Forwarded-Path（由 core 透传的原始路径），回退到请求体特征：
// 有 messages 且不是 Anthropic 风格（无 max_tokens 关键字段）。
func isChatCompletionsRequest(req *sdk.ForwardRequest) bool {
	if req == nil {
		return false
	}
	path := extractForwardedPath(req.Headers)
	if path != "" {
		return strings.Contains(path, "/chat/completions")
	}
	// 兜底：body 里有 messages 但没有 input，且不是 Anthropic 请求
	if len(req.Body) == 0 {
		return false
	}
	hasMessages := gjson.GetBytes(req.Body, "messages").Exists()
	hasInput := gjson.GetBytes(req.Body, "input").Exists()
	if !hasMessages || hasInput {
		return false
	}
	// Anthropic 请求会被 forwardHTTP 上层更早分支拦掉，这里走到说明是 OpenAI 风格。
	return true
}

// generateChatCmplID 生成 Chat Completions 规定的 "chatcmpl-" 前缀 ID。
func generateChatCmplID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

// ──────────────────────────────────────────────────────
// 流式：Responses SSE 事件 → Chat Completions chunk
// ──────────────────────────────────────────────────────

// chatCompletionsStreamWriter 把 OAuth 上游的 Responses SSE 事件实时翻译成
// Chat Completions 的 chat.completion.chunk SSE，写回客户端。
// 实现 WSEventHandler 接口，可以直接挂到 ReceiveWSResponse。
type chatCompletionsStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string

	id                 string
	created            int64
	sentRole           bool
	sawToolCall        bool
	finalized          bool
	outputIdxToToolIdx map[int]int
	nextToolIdx        int
	includeUsage       bool
	usage              map[string]any

	firstTokenOnce sync.Once
	firstTokenMs   int64
	start          time.Time

	accountID  int64
	sessionKey string
}

func newChatCompletionsStreamWriter(
	w http.ResponseWriter,
	model string,
	accountID int64,
	sessionKey string,
	includeUsage bool,
	start time.Time,
) *chatCompletionsStreamWriter {
	s := &chatCompletionsStreamWriter{
		w:                  w,
		model:              model,
		id:                 generateChatCmplID(),
		created:            time.Now().Unix(),
		outputIdxToToolIdx: make(map[int]int),
		start:              start,
		accountID:          accountID,
		sessionKey:         sessionKey,
		includeUsage:       includeUsage,
	}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	return s
}

func (s *chatCompletionsStreamWriter) OnTextDelta(string)      {}
func (s *chatCompletionsStreamWriter) OnReasoningDelta(string) {}

func (s *chatCompletionsStreamWriter) OnRateLimits(usedPercent float64) {
	if s.accountID > 0 {
		StoreCodexUsage(s.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: usedPercent,
			CapturedAt:         time.Now(),
		})
	}
}

func (s *chatCompletionsStreamWriter) OnRawEvent(eventType string, data []byte) {
	if s.w == nil || eventType == "" {
		return
	}
	s.firstTokenOnce.Do(func() {
		s.firstTokenMs = time.Since(s.start).Milliseconds()
	})

	// 捕获内部事件：用量快照与会话状态，但不转发给客户端
	switch eventType {
	case "codex.rate_limits":
		if s.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(s.accountID, snapshot)
			}
		}
		return
	case "response.created", "response.completed", "response.done":
		if s.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(s.sessionKey, responseID)
			}
		}
	}

	chunks := s.translateEvent(eventType, data)
	for _, chunk := range chunks {
		if _, err := fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
			return
		}
	}
	if s.flusher != nil && len(chunks) > 0 {
		s.flusher.Flush()
	}
}

// translateEvent 把单条 Responses 事件转成零条或多条 Chat Completions chunk 原始 JSON。
func (s *chatCompletionsStreamWriter) translateEvent(eventType string, data []byte) [][]byte {
	switch eventType {
	case "response.created":
		if id := gjson.GetBytes(data, "response.id").String(); id != "" {
			s.id = id
		}
		if s.sentRole {
			return nil
		}
		s.sentRole = true
		return [][]byte{s.makeDeltaChunk(map[string]any{"role": "assistant"})}

	case "response.output_text.delta":
		delta := gjson.GetBytes(data, "delta").String()
		if delta == "" {
			return nil
		}
		return [][]byte{s.makeDeltaChunk(map[string]any{"content": delta})}

	case "response.reasoning_summary_text.delta":
		delta := gjson.GetBytes(data, "delta").String()
		if delta == "" {
			return nil
		}
		return [][]byte{s.makeDeltaChunk(map[string]any{"reasoning_content": delta})}

	case "response.output_item.added":
		itemType := gjson.GetBytes(data, "item.type").String()
		if itemType != "function_call" {
			return nil
		}
		s.sawToolCall = true
		outputIdx := int(gjson.GetBytes(data, "output_index").Int())
		idx := s.nextToolIdx
		s.outputIdxToToolIdx[outputIdx] = idx
		s.nextToolIdx++
		callID := gjson.GetBytes(data, "item.call_id").String()
		name := gjson.GetBytes(data, "item.name").String()
		return [][]byte{s.makeDeltaChunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index": idx,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": "",
				},
			}},
		})}

	case "response.function_call_arguments.delta":
		argsDelta := gjson.GetBytes(data, "delta").String()
		if argsDelta == "" {
			return nil
		}
		outputIdx := int(gjson.GetBytes(data, "output_index").Int())
		idx, ok := s.outputIdxToToolIdx[outputIdx]
		if !ok {
			return nil
		}
		return [][]byte{s.makeDeltaChunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index": idx,
				"function": map[string]any{
					"arguments": argsDelta,
				},
			}},
		})}

	case "response.completed", "response.done":
		s.finalized = true
		finishReason := "stop"
		status := gjson.GetBytes(data, "response.status").String()
		switch status {
		case "incomplete":
			if gjson.GetBytes(data, "response.incomplete_details.reason").String() == "max_output_tokens" {
				finishReason = "length"
			}
		case "completed":
			if s.sawToolCall {
				finishReason = "tool_calls"
			}
		}
		if s.includeUsage {
			s.usage = extractChatUsageFromResponse(data)
		}
		return [][]byte{s.makeFinishChunk(finishReason)}

	case "response.incomplete":
		s.finalized = true
		finishReason := "stop"
		if gjson.GetBytes(data, "response.incomplete_details.reason").String() == "max_output_tokens" {
			finishReason = "length"
		}
		return [][]byte{s.makeFinishChunk(finishReason)}
	}
	return nil
}

// finalize 在 ReceiveWSResponse 返回后调用：如果上游没触发 response.completed
// （例如上游断流），补一个兜底 finish chunk；最后写入 [DONE] 终止符。
func (s *chatCompletionsStreamWriter) finalize() {
	if s.w == nil {
		return
	}
	if !s.finalized {
		finishReason := "stop"
		if s.sawToolCall {
			finishReason = "tool_calls"
		}
		if _, err := fmt.Fprintf(s.w, "data: %s\n\n", s.makeFinishChunk(finishReason)); err != nil {
			return
		}
	}
	if s.includeUsage && s.usage != nil {
		usageChunk := s.makeChunkBase()
		usageChunk["choices"] = []any{}
		usageChunk["usage"] = s.usage
		if b, err := json.Marshal(usageChunk); err == nil {
			_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
		}
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *chatCompletionsStreamWriter) makeChunkBase() map[string]any {
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
	}
}

func (s *chatCompletionsStreamWriter) makeDeltaChunk(delta map[string]any) []byte {
	base := s.makeChunkBase()
	base["choices"] = []map[string]any{{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}}
	b, _ := json.Marshal(base)
	return b
}

func (s *chatCompletionsStreamWriter) makeFinishChunk(finishReason string) []byte {
	base := s.makeChunkBase()
	base["choices"] = []map[string]any{{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": finishReason,
	}}
	b, _ := json.Marshal(base)
	return b
}

// ──────────────────────────────────────────────────────
// 非流式：WSResult → chat.completion JSON
// ──────────────────────────────────────────────────────

// buildNonStreamChatCompletion 把 ReceiveWSResponse 聚合出的 WSResult 合成
// 一个标准的 chat.completion 响应体。
func buildNonStreamChatCompletion(result WSResult, model string) []byte {
	// Chat Completions 规范要求 id 带 "chatcmpl-" 前缀，否则严格 SDK 会校验失败。
	// 上游 Responses API 给的 "resp_..." ID 只在网关内部用于 session 维护（见
	// updateSessionStateResponseID），不能直接透出给客户端。
	id := generateChatCmplID()
	if strings.TrimSpace(model) == "" {
		model = result.Model
	}

	message := map[string]any{"role": "assistant"}

	var toolCalls []map[string]any
	for _, tu := range result.ToolUses {
		if tu.Type != "tool_use" {
			continue
		}
		if tu.Name == nil || *tu.Name == "" {
			continue
		}
		// web_search 等服务端工具不对外暴露成 function tool_call
		if *tu.Name == "web_search" {
			continue
		}
		args := string(tu.Input)
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   tu.ID,
			"type": "function",
			"function": map[string]any{
				"name":      *tu.Name,
				"arguments": args,
			},
		})
	}

	if result.Text != "" {
		message["content"] = result.Text
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	// ReceiveWSResponse 为了计费已经把 cached_tokens 从 InputTokens 里扣掉了，
	// Chat Completions 的 prompt_tokens 应当是上游原始输入总量，所以这里加回去。
	promptTokens := result.InputTokens + result.CachedInputTokens
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      promptTokens + result.OutputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": result.CachedInputTokens,
			},
		},
	}

	b, _ := json.Marshal(resp)
	return b
}

// extractChatUsageFromResponse 从 response.completed 的 response.usage 里提取
// Chat Completions 格式的 usage（流式 include_usage 场景用）。
func extractChatUsageFromResponse(data []byte) map[string]any {
	usageNode := gjson.GetBytes(data, "response.usage")
	if !usageNode.Exists() {
		return nil
	}
	input := int(usageNode.Get("input_tokens").Int())
	output := int(usageNode.Get("output_tokens").Int())
	cached := int(usageNode.Get("input_tokens_details.cached_tokens").Int())
	// Responses 的 input_tokens 是包含 cached 的总量，Chat Completions 的
	// prompt_tokens 也是总量，这里直接用。
	return map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
	}
}

// ──────────────────────────────────────────────────────
// 非流式兜底 WSEventHandler
// ──────────────────────────────────────────────────────

// chatCompletionsSilentHandler 在非流式 Chat Completions 场景使用：
// 不往客户端写任何东西，只捕获用量快照和会话状态，真正的响应体由
// forwardOAuth 在 ReceiveWSResponse 完成后用 WSResult 合成并一次性写回。
type chatCompletionsSilentHandler struct {
	accountID      int64
	sessionKey     string
	start          time.Time
	firstTokenOnce sync.Once
	firstTokenMs   int64
}

func (h *chatCompletionsSilentHandler) OnTextDelta(string)      {}
func (h *chatCompletionsSilentHandler) OnReasoningDelta(string) {}

func (h *chatCompletionsSilentHandler) OnRateLimits(usedPercent float64) {
	if h.accountID > 0 {
		StoreCodexUsage(h.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: usedPercent,
			CapturedAt:         time.Now(),
		})
	}
}

func (h *chatCompletionsSilentHandler) OnRawEvent(eventType string, data []byte) {
	if eventType == "" {
		return
	}
	h.firstTokenOnce.Do(func() {
		h.firstTokenMs = time.Since(h.start).Milliseconds()
	})
	switch eventType {
	case "codex.rate_limits":
		if h.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(h.accountID, snapshot)
			}
		}
	case "response.created", "response.completed", "response.done":
		if h.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(h.sessionKey, responseID)
			}
		}
	}
}

// ──────────────────────────────────────────────────────
// /v1/responses 非流式
// ──────────────────────────────────────────────────────

// responsesSilentHandler 在非流式 /v1/responses 场景用：不向客户端写 SSE，
// 等 ReceiveWSResponse 完成后由 forwardOAuth 用 WSResult.CompletedEventRaw
// 抽出 response 字段一次性回写 JSON。行为和 chatCompletionsSilentHandler 完全一致，
// 单独起个类型只是为了在 forward.go 里按场景区分时更显眼。
type responsesSilentHandler struct {
	accountID      int64
	sessionKey     string
	start          time.Time
	firstTokenOnce sync.Once
	firstTokenMs   int64
}

func (h *responsesSilentHandler) OnTextDelta(string)      {}
func (h *responsesSilentHandler) OnReasoningDelta(string) {}

func (h *responsesSilentHandler) OnRateLimits(usedPercent float64) {
	if h.accountID > 0 {
		StoreCodexUsage(h.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: usedPercent,
			CapturedAt:         time.Now(),
		})
	}
}

func (h *responsesSilentHandler) OnRawEvent(eventType string, data []byte) {
	if eventType == "" {
		return
	}
	h.firstTokenOnce.Do(func() {
		h.firstTokenMs = time.Since(h.start).Milliseconds()
	})
	switch eventType {
	case "codex.rate_limits":
		if h.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(h.accountID, snapshot)
			}
		}
	case "response.created", "response.completed", "response.done":
		if h.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(h.sessionKey, responseID)
			}
		}
	}
}

// buildNonStreamResponses 从 WSResult 聚合出 Responses API 非流式响应体。
//
// Responses API 非流式响应的 JSON 结构就是最终 `response.completed` SSE 事件里
// `response` 字段的那坨对象——直接抽出来返回即可，无需自己拼装。拼装反而会丢失
// 上游新加字段（OpenAI 常常悄悄扩展 Responses 的 output / tool_usage 等字段）。
//
// 上游没给 `response.completed`（典型：被中途 cancel）时回退到一个最小占位对象，
// 避免空体返回。
func buildNonStreamResponses(result WSResult) []byte {
	if len(result.CompletedEventRaw) > 0 {
		if respNode := gjson.GetBytes(result.CompletedEventRaw, "response"); respNode.Exists() {
			return []byte(respNode.Raw)
		}
	}
	// 兜底：空体不好看，给客户端一个最小但合法的 Responses 对象
	fallback := map[string]any{
		"object": "response",
		"status": "incomplete",
	}
	if result.ResponseID != "" {
		fallback["id"] = result.ResponseID
	}
	if result.Model != "" {
		fallback["model"] = result.Model
	}
	b, _ := json.Marshal(fallback)
	return b
}
