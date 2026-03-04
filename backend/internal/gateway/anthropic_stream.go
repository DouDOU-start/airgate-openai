package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// OpenAI SSE → Anthropic SSE 流转换状态机
// 参考 AxonHub llm/transformer/anthropic/inbound_stream.go
// ──────────────────────────────────────────────────────

// anthropicSSEEvent 一个待发送的 Anthropic SSE 事件
type anthropicSSEEvent struct {
	Event string // SSE event: 类型名
	Data  []byte // JSON 数据
}

// toolCallState 跟踪单个工具调用的状态
type toolCallState struct {
	ID   string
	Name string
}

// responsesToolState 跟踪 Responses API 工具调用（按 itemID 索引）
type responsesToolState struct {
	callID string
	name   string
}

// anthropicStreamTranslator OpenAI SSE → Anthropic SSE 的有状态翻译器
type anthropicStreamTranslator struct {
	// 状态标志
	hasStarted                bool
	hasTextContentStarted     bool
	hasThinkingContentStarted bool
	hasToolContentStarted     bool
	hasFinished               bool
	messageStoped             bool

	// 消息元数据
	messageID    string
	model        string
	contentIndex int64

	// 事件队列（一个 OpenAI chunk 可能产生多个 Anthropic 事件）
	eventQueue []*anthropicSSEEvent
	queueIndex int

	// 停止原因
	stopReason *string

	// 工具调用追踪
	toolCalls      map[int]toolCallState
	responsesTools map[string]responsesToolState // Responses API：itemID → state

	// web_search_call 追踪：itemID → 是否已开启 block
	webSearchBlocks map[string]bool

	// Signature 缓冲：当 signature 在 thinking 之前到达时，暂存直到 thinking 结束
	pendingSignature *string

	// 重复事件检测
	lastEventType string

	// Token 统计
	inputTokens  int
	outputTokens int
	cacheTokens  int
}

func newAnthropicStreamTranslator() *anthropicStreamTranslator {
	return &anthropicStreamTranslator{
		toolCalls:       make(map[int]toolCallState),
		responsesTools:  make(map[string]responsesToolState),
		webSearchBlocks: make(map[string]bool),
		messageID:       generateMessageID(),
	}
}

// enqueueEvent 入队一个 Anthropic SSE 事件（带重复 content_block_stop 过滤）
func (t *anthropicStreamTranslator) enqueueEvent(ev *AnthropicStreamEvent) error {
	// 过滤连续重复的 content_block_stop
	if t.lastEventType == "content_block_stop" && ev.Type == "content_block_stop" {
		return nil
	}
	t.lastEventType = ev.Type

	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	t.eventQueue = append(t.eventQueue, &anthropicSSEEvent{
		Event: ev.Type,
		Data:  data,
	})
	return nil
}

// flushPendingSignature 发送缓冲的 signature_delta 事件
func (t *anthropicStreamTranslator) flushPendingSignature() error {
	if t.pendingSignature == nil {
		return nil
	}
	sig := t.pendingSignature
	t.pendingSignature = nil

	return t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: ptrInt64(t.contentIndex),
		Delta: &AnthropicStreamDelta{
			Type:      ptrStr("signature_delta"),
			Signature: sig,
		},
	})
}

// closeCurrentContentBlock 关闭当前打开的内容块
func (t *anthropicStreamTranslator) closeCurrentContentBlock() error {
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: ptrInt64(t.contentIndex),
	})
}

// processChunk 处理一个 OpenAI SSE chunk，生成对应的 Anthropic 事件到队列中
func (t *anthropicStreamTranslator) processChunk(data string) error {
	if data == "" || data == "[DONE]" {
		return nil
	}

	parsed := gjson.Parse(data)
	if !parsed.Exists() {
		return nil
	}

	// 重置事件队列
	t.eventQueue = t.eventQueue[:0]
	t.queueIndex = 0

	// 提取 model
	if m := parsed.Get("model").String(); m != "" {
		t.model = m
	}

	choice := parsed.Get("choices.0")

	// 提取 usage（可能在顶层或 choice 内）
	t.extractUsage(parsed)

	// 首次 chunk：发送 message_start
	if !t.hasStarted {
		t.hasStarted = true
		if err := t.emitMessageStart(); err != nil {
			return err
		}
	}

	if !choice.Exists() {
		return nil
	}

	delta := choice.Get("delta")

	// 1. 处理 reasoning_content（thinking）
	if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
		if err := t.handleReasoningDelta(rc.String()); err != nil {
			return err
		}
	}

	// 2. 处理 reasoning_signature（如上游支持）
	// 注：大部分 OpenAI 兼容后端不支持此字段，但保留处理逻辑
	if sig := delta.Get("reasoning_signature"); sig.Exists() && sig.String() != "" {
		if err := t.handleSignatureDelta(sig.String()); err != nil {
			return err
		}
	}

	// 3. 处理文本内容
	if content := delta.Get("content"); content.Exists() && content.String() != "" {
		if err := t.handleTextDelta(content.String()); err != nil {
			return err
		}
	}

	// 4. 处理 tool_calls
	if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
		for _, tc := range toolCalls.Array() {
			if err := t.handleToolCallDelta(tc); err != nil {
				return err
			}
		}
	}

	// 5. 处理 finish_reason
	if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type != gjson.Null {
		if err := t.handleFinishReason(fr.String()); err != nil {
			return err
		}
	}

	return nil
}

// emitMessageStart 发送 message_start 事件
func (t *anthropicStreamTranslator) emitMessageStart() error {
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicStreamMessage{
			ID:      t.messageID,
			Type:    "message",
			Role:    "assistant",
			Content: []AnthropicMessageContentBlock{},
			Model:   t.model,
			Usage: &AnthropicUsage{
				InputTokens:  t.inputTokens,
				OutputTokens: 0,
			},
		},
	})
}

// handleReasoningDelta 处理 thinking 增量
func (t *anthropicStreamTranslator) handleReasoningDelta(delta string) error {
	if !t.hasThinkingContentStarted {
		// 如果之前有 text block 打开，先关闭
		if t.hasTextContentStarted {
			if err := t.closeCurrentContentBlock(); err != nil {
				return err
			}
			t.contentIndex++
			t.hasTextContentStarted = false
		}

		// 发送 content_block_start（thinking）
		if err := t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: ptrInt64(t.contentIndex),
			ContentBlock: &AnthropicMessageContentBlock{
				Type:     "thinking",
				Thinking: ptrStr(""),
			},
		}); err != nil {
			return err
		}
		t.hasThinkingContentStarted = true
	}

	// 发送 thinking_delta
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: ptrInt64(t.contentIndex),
		Delta: &AnthropicStreamDelta{
			Type:     ptrStr("thinking_delta"),
			Thinking: ptrStr(delta),
		},
	})
}

// handleSignatureDelta 处理 signature 增量
func (t *anthropicStreamTranslator) handleSignatureDelta(sig string) error {
	if t.hasThinkingContentStarted {
		// thinking 已开始，直接发送 signature_delta
		return t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_delta",
			Index: ptrInt64(t.contentIndex),
			Delta: &AnthropicStreamDelta{
				Type:      ptrStr("signature_delta"),
				Signature: ptrStr(sig),
			},
		})
	}
	// thinking 还未开始，缓冲 signature
	t.pendingSignature = ptrStr(sig)
	return nil
}

// handleTextDelta 处理文本增量
func (t *anthropicStreamTranslator) handleTextDelta(delta string) error {
	if !t.hasTextContentStarted {
		// 如果之前有 thinking block 打开，先关闭
		if t.hasThinkingContentStarted {
			if err := t.flushPendingSignature(); err != nil {
				return err
			}
			if err := t.closeCurrentContentBlock(); err != nil {
				return err
			}
			t.contentIndex++
			t.hasThinkingContentStarted = false
		}

		// 发送 content_block_start（text）
		if err := t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: ptrInt64(t.contentIndex),
			ContentBlock: &AnthropicMessageContentBlock{
				Type: "text",
				Text: ptrStr(""),
			},
		}); err != nil {
			return err
		}
		t.hasTextContentStarted = true
	}

	// 发送 text_delta
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: ptrInt64(t.contentIndex),
		Delta: &AnthropicStreamDelta{
			Type: ptrStr("text_delta"),
			Text: ptrStr(delta),
		},
	})
}

// handleToolCallDelta 处理工具调用增量
func (t *anthropicStreamTranslator) handleToolCallDelta(tc gjson.Result) error {
	index := int(tc.Get("index").Int())

	// 新工具调用
	if _, exists := t.toolCalls[index]; !exists {
		// 关闭之前打开的 text/thinking block
		if t.hasTextContentStarted {
			if err := t.closeCurrentContentBlock(); err != nil {
				return err
			}
			t.contentIndex++
			t.hasTextContentStarted = false
		}
		if t.hasThinkingContentStarted {
			if err := t.flushPendingSignature(); err != nil {
				return err
			}
			if err := t.closeCurrentContentBlock(); err != nil {
				return err
			}
			t.contentIndex++
			t.hasThinkingContentStarted = false
		}

		toolID := tc.Get("id").String()
		toolName := tc.Get("function.name").String()
		t.toolCalls[index] = toolCallState{ID: toolID, Name: toolName}

		// 发送 content_block_start（tool_use）
		if err := t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: ptrInt64(t.contentIndex),
			ContentBlock: &AnthropicMessageContentBlock{
				Type:  "tool_use",
				ID:    toolID,
				Name:  ptrStr(toolName),
				Input: json.RawMessage("{}"),
			},
		}); err != nil {
			return err
		}
		t.hasToolContentStarted = true
	}

	// 工具参数增量
	if args := tc.Get("function.arguments").String(); args != "" {
		if err := t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_delta",
			Index: ptrInt64(t.contentIndex),
			Delta: &AnthropicStreamDelta{
				Type:        ptrStr("input_json_delta"),
				PartialJSON: ptrStr(args),
			},
		}); err != nil {
			return err
		}
	}

	return nil
}

// handleResponsesToolStart 处理 Responses API response.output_item.added 事件中的 function_call
// itemID 是 Responses API 的 item.id，callID 是 item.call_id，name 是函数名
func (t *anthropicStreamTranslator) handleResponsesToolStart(itemID, callID, name string) error {
	if _, exists := t.responsesTools[itemID]; exists {
		return nil
	}

	// 关闭之前打开的 text/thinking block
	if t.hasTextContentStarted {
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasTextContentStarted = false
	}
	if t.hasThinkingContentStarted {
		if err := t.flushPendingSignature(); err != nil {
			return err
		}
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasThinkingContentStarted = false
	}

	t.responsesTools[itemID] = responsesToolState{callID: callID, name: name}

	if err := t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: ptrInt64(t.contentIndex),
		ContentBlock: &AnthropicMessageContentBlock{
			Type:  "tool_use",
			ID:    callID,
			Name:  ptrStr(name),
			Input: json.RawMessage("{}"),
		},
	}); err != nil {
		return err
	}
	t.hasToolContentStarted = true
	return nil
}

// handleResponsesToolArgsDelta 处理 response.function_call_arguments.delta 事件
func (t *anthropicStreamTranslator) handleResponsesToolArgsDelta(delta string) error {
	if delta == "" {
		return nil
	}
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: ptrInt64(t.contentIndex),
		Delta: &AnthropicStreamDelta{
			Type:        ptrStr("input_json_delta"),
			PartialJSON: ptrStr(delta),
		},
	})
}

// handleResponsesToolDone 处理 response.output_item.done 事件中的 function_call
func (t *anthropicStreamTranslator) handleResponsesToolDone() error {
	if !t.hasToolContentStarted {
		return nil
	}
	if err := t.closeCurrentContentBlock(); err != nil {
		return err
	}
	t.contentIndex++
	t.hasToolContentStarted = false
	return nil
}

// handleWebSearchCallStart 处理 response.output_item.added item_type=web_search_call
// 翻译为 tool_use block（name=web_search），让客户端显示搜索进度
func (t *anthropicStreamTranslator) handleWebSearchCallStart(itemID string) error {
	if t.webSearchBlocks[itemID] {
		return nil
	}

	// 确保 message_start 已发送
	if !t.hasStarted {
		t.hasStarted = true
		if err := t.emitMessageStart(); err != nil {
			return err
		}
	}

	// 关闭之前打开的 text/thinking block
	if t.hasTextContentStarted {
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasTextContentStarted = false
	}
	if t.hasThinkingContentStarted {
		if err := t.flushPendingSignature(); err != nil {
			return err
		}
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasThinkingContentStarted = false
	}

	t.webSearchBlocks[itemID] = true

	// 发送 server_tool_use block（Anthropic 原生工具类型，客户端只展示不执行）
	if err := t.enqueueEvent(&AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: ptrInt64(t.contentIndex),
		ContentBlock: &AnthropicMessageContentBlock{
			Type:  "server_tool_use",
			ID:    itemID,
			Name:  ptrStr("web_search"),
			Input: json.RawMessage("{}"),
		},
	}); err != nil {
		return err
	}
	t.hasToolContentStarted = true
	return nil
}

// handleWebSearchCallDone 处理 response.output_item.done item_type=web_search_call
// 用 query 填充 input，关闭 tool_use block
func (t *anthropicStreamTranslator) handleWebSearchCallDone(itemID, query string) error {
	if !t.webSearchBlocks[itemID] || !t.hasToolContentStarted {
		return nil
	}

	// 发送 query 作为 input_json_delta
	if query != "" {
		inputJSON := fmt.Sprintf(`{"query":%q}`, query)
		if err := t.enqueueEvent(&AnthropicStreamEvent{
			Type:  "content_block_delta",
			Index: ptrInt64(t.contentIndex),
			Delta: &AnthropicStreamDelta{
				Type:        ptrStr("input_json_delta"),
				PartialJSON: ptrStr(inputJSON),
			},
		}); err != nil {
			return err
		}
	}

	if err := t.closeCurrentContentBlock(); err != nil {
		return err
	}
	t.contentIndex++
	t.hasToolContentStarted = false
	return nil
}

// handleFinishReason 处理完成原因
func (t *anthropicStreamTranslator) handleFinishReason(reason string) error {
	if t.hasFinished {
		return nil
	}
	t.hasFinished = true

	// 关闭所有打开的 content block
	if t.hasThinkingContentStarted {
		if err := t.flushPendingSignature(); err != nil {
			return err
		}
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasThinkingContentStarted = false
	}
	if t.hasTextContentStarted {
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasTextContentStarted = false
	}
	if t.hasToolContentStarted {
		if err := t.closeCurrentContentBlock(); err != nil {
			return err
		}
		t.contentIndex++
		t.hasToolContentStarted = false
	}

	// 转换 stop_reason
	stopReason := convertFinishReasonToAnthropic(reason)
	t.stopReason = &stopReason

	// 发送 message_delta（含 stop_reason 和 usage）
	if err := t.enqueueEvent(&AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &AnthropicStreamDelta{
			StopReason: &stopReason,
		},
		Usage: &AnthropicUsage{
			OutputTokens: t.outputTokens,
		},
	}); err != nil {
		return err
	}

	// 发送 message_stop
	t.messageStoped = true
	return t.enqueueEvent(&AnthropicStreamEvent{
		Type: "message_stop",
	})
}

// extractUsage 从 OpenAI SSE chunk 中提取 token 统计
func (t *anthropicStreamTranslator) extractUsage(parsed gjson.Result) {
	// OpenAI stream_options.include_usage=true 时，usage 在顶层
	usage := parsed.Get("usage")
	if !usage.Exists() {
		return
	}
	if pt := usage.Get("prompt_tokens").Int(); pt > 0 {
		t.inputTokens = int(pt)
	}
	if ct := usage.Get("completion_tokens").Int(); ct > 0 {
		t.outputTokens = int(ct)
	}
	if crt := usage.Get("prompt_tokens_details.cached_tokens").Int(); crt > 0 {
		t.cacheTokens = int(crt)
	}
}

// ──────────────────────────────────────────────────────
// SSE 流转换入口
// ──────────────────────────────────────────────────────

// translateOpenAISSEToAnthropicSSE 读取上游 OpenAI SSE 并翻译为 Anthropic SSE 写回客户端
func translateOpenAISSEToAnthropicSSE(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	// 设置 Anthropic SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	translator := newAnthropicStreamTranslator()
	translator.model = model

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var streamErr error

	for scanner.Scan() {
		// 检查 context 取消
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			goto done
		default:
		}

		line := scanner.Text()

		// 提取 SSE data 行
		data, ok := extractSSEData(line)
		if !ok || data == "" {
			continue
		}

		if data == "[DONE]" {
			// 如果还没发送 message_stop，补发
			if !translator.messageStoped {
				_ = translator.handleFinishReason("stop")
				flushTranslatorEvents(translator, w, flusher)
			}
			continue
		}

		// 处理 chunk
		if err := translator.processChunk(data); err != nil {
			streamErr = err
			break
		}

		// 将队列中的事件写回客户端
		flushTranslatorEvents(translator, w, flusher)
	}

done:
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	// 确保发送了 message_stop
	if !translator.messageStoped && translator.hasStarted {
		_ = translator.handleFinishReason("stop")
		flushTranslatorEvents(translator, w, flusher)
	}

	result := &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		InputTokens:  translator.inputTokens,
		OutputTokens: translator.outputTokens,
		CacheTokens:  translator.cacheTokens,
		Model:        translator.model,
		Duration:     time.Since(start),
	}

	if streamErr != nil {
		result.StatusCode = http.StatusBadGateway
		return result, streamErr
	}
	return result, nil
}

// flushTranslatorEvents 将翻译器事件队列中的事件写入 ResponseWriter
func flushTranslatorEvents(t *anthropicStreamTranslator, w http.ResponseWriter, flusher http.Flusher) {
	for t.queueIndex < len(t.eventQueue) {
		ev := t.eventQueue[t.queueIndex]
		t.queueIndex++

		// 只记录结构性事件，跳过高频的 delta 事件
		if ev.Event != "content_block_delta" && ev.Event != "message_delta" {
			slog.Info("[→客户端SSE]", "event", ev.Event, "data", truncate(string(ev.Data), 300))
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, strings.ReplaceAll(string(ev.Data), "\n", ""))
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// translateResponsesSSEToAnthropicSSE 读取上游 Responses API SSE 并翻译为 Anthropic SSE 写回客户端
// Responses API 事件格式不同于 Chat Completions，需要单独处理
func translateResponsesSSEToAnthropicSSE(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	start time.Time,
) (*sdk.ForwardResult, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	translator := newAnthropicStreamTranslator()
	translator.model = model

	// 定期发送 SSE ping，防止客户端在上游推理/搜索期间超时断开
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				fmt.Fprintf(w, ": ping\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var streamErr error

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			goto done
		default:
		}

		line := scanner.Text()
		data, ok := extractSSEData(line)
		if !ok || data == "" || data == "[DONE]" {
			continue
		}

		parsed := gjson.Parse(data)
		eventType := parsed.Get("type").String()
		// 只记录结构性事件，跳过高频的 delta 事件
		if eventType != "response.output_text.delta" && eventType != "response.reasoning_summary_text.delta" && eventType != "response.function_call_arguments.delta" {
			slog.Info("[上游SSE]", "type", eventType, "data", truncate(data, 300))
		}

		switch eventType {
		case "response.created":
			// 首次事件，发送 message_start
			if !translator.hasStarted {
				translator.hasStarted = true
				if err := translator.emitMessageStart(); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.output_text.delta":
			if !translator.hasStarted {
				translator.hasStarted = true
				if err := translator.emitMessageStart(); err != nil {
					streamErr = err
					goto done
				}
			}
			delta := parsed.Get("delta").String()
			if delta != "" {
				if err := translator.handleTextDelta(delta); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.reasoning_summary_text.delta":
			if !translator.hasStarted {
				translator.hasStarted = true
				if err := translator.emitMessageStart(); err != nil {
					streamErr = err
					goto done
				}
			}
			delta := parsed.Get("delta").String()
			if delta != "" {
				if err := translator.handleReasoningDelta(delta); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.output_item.added":
			// 新输出项开始
			item := parsed.Get("item")
			switch item.Get("type").String() {
			case "function_call":
				if !translator.hasStarted {
					translator.hasStarted = true
					if err := translator.emitMessageStart(); err != nil {
						streamErr = err
						goto done
					}
				}
				itemID := item.Get("id").String()
				callID := item.Get("call_id").String()
				name := item.Get("name").String()
				if err := translator.handleResponsesToolStart(itemID, callID, name); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			case "web_search_call":
				// 开启 web_search tool_use block，让客户端显示搜索进度
				itemID := item.Get("id").String()
				if err := translator.handleWebSearchCallStart(itemID); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.function_call_arguments.delta":
			// 工具参数增量
			delta := parsed.Get("delta").String()
			if delta != "" {
				if err := translator.handleResponsesToolArgsDelta(delta); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.output_item.done":
			// 输出项完成
			switch parsed.Get("item.type").String() {
			case "function_call":
				if err := translator.handleResponsesToolDone(); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			case "web_search_call":
				// 从 item.action.query 或 item.id 取查询词，关闭 web_search tool_use block
				itemID := parsed.Get("item.id").String()
				query := parsed.Get("item.action.query").String()
				if err := translator.handleWebSearchCallDone(itemID, query); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.completed", "response.done":
			// 提取 usage
			if usage := parsed.Get("response.usage"); usage.Exists() {
				if it := usage.Get("input_tokens").Int(); it > 0 {
					translator.inputTokens = int(it)
				}
				if ot := usage.Get("output_tokens").Int(); ot > 0 {
					translator.outputTokens = int(ot)
				}
				if ct := usage.Get("input_tokens_details.cached_tokens").Int(); ct > 0 {
					translator.cacheTokens = int(ct)
				}
			}
			if rm := parsed.Get("response.model").String(); rm != "" {
				translator.model = rm
			}
			if !translator.messageStoped {
				// 若有工具调用，stop_reason 应为 tool_calls
				finishReason := "stop"
				if len(translator.responsesTools) > 0 {
					finishReason = "tool_calls"
				}
				if err := translator.handleFinishReason(finishReason); err != nil {
					streamErr = err
					goto done
				}
				flushTranslatorEvents(translator, w, flusher)
			}

		case "response.failed":
			errMsg := parsed.Get("response.error.message").String()
			if errMsg == "" {
				errMsg = "上游返回 response.failed"
			}
			streamErr = fmt.Errorf("上游错误: %s", errMsg)
			goto done

		case "response.incomplete":
			reason := parsed.Get("response.incomplete_details.reason").String()
			streamErr = fmt.Errorf("响应不完整: %s", reason)
			goto done
		}
	}

done:
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	if !translator.messageStoped && translator.hasStarted {
		_ = translator.handleFinishReason("stop")
		flushTranslatorEvents(translator, w, flusher)
	}

	result := &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		InputTokens:  translator.inputTokens,
		OutputTokens: translator.outputTokens,
		CacheTokens:  translator.cacheTokens,
		Model:        translator.model,
		Duration:     time.Since(start),
	}
	if streamErr != nil {
		result.StatusCode = http.StatusBadGateway
		return result, streamErr
	}
	return result, nil
}

// 注意：extractSSEData 已在 stream.go 中定义，此处直接复用
