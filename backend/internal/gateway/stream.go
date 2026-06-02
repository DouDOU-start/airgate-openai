package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// upstreamSSEMaxLineBytes 是上游 SSE 单行最大字节数。
// 上游某些事件（例如 response.output_item.done 携带完整输出 / 大段 reasoning summary）
// 可能远超 1 MB，过小会触发 bufio.Scanner: token too long 中断流。
const upstreamSSEMaxLineBytes = 8 * 1024 * 1024

// largeSSEEventThreshold 触发大事件诊断日志的阈值。
// 超过这个长度的单行/翻译输出会被打印 type 与长度，便于追踪谁在膨胀。
const largeSSEEventThreshold = 512 * 1024

const streamDiagnosticPreviewBytes = 1200

const debugUpstreamSSEEnv = "AIRGATE_OPENAI_DEBUG_UPSTREAM_SSE"

// handleStreamResponse 处理 SSE 流式响应。调用者保证 resp.StatusCode 是 2xx
// （4xx/5xx 由调用者预先归类，不会进到这里）。
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time, reqServiceTier string) (sdk.ForwardOutcome, error) {
	return handleStreamResponseWithLogger(slog.Default(), resp, w, start, reqServiceTier)
}

func handleStreamResponseWithLogger(logger *slog.Logger, resp *http.Response, w http.ResponseWriter, start time.Time, reqServiceTier string) (sdk.ForwardOutcome, error) {
	if logger == nil {
		logger = slog.Default()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	passCodexRateLimitHeaders(resp.Header, w.Header())

	usage := newTokenUsage("", reqServiceTier, 0, 0, 0, 0, 0)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)
	var streamErr error
	firstTokenRecorded := false
	streamStarted := false
	completed := false
	streamErrLogged := false
	debugUpstreamSSE := os.Getenv(debugUpstreamSSEEnv) == "1"
	diagnostics := streamResponseDiagnostics{}
	var pending strings.Builder
	var toolImageIn, toolImageOut int // 接收 response.tool_usage.image_gen，用于图像工具计费。
	imageGenCounter := newImageGenCallCounter()

	for scanner.Scan() {
		line := scanner.Text()
		diagnostics.observeLine(line)
		data, ok := extractSSEData(line)
		if ok {
			data = strings.TrimSpace(data)
			diagnostics.observeData(data)
			if debugUpstreamSSE && data != "" {
				logUpstreamSSEDataDebug(logger, diagnostics, data)
			}
			if data == "[DONE]" {
				completed = true
				diagnostics.completionEvent = "[DONE]"
			} else if data != "" {
				if streamErr = parseSSEFailureEvent([]byte(data)); streamErr != nil {
					logStreamFailure(logger, streamErr, resp, streamStarted, diagnostics)
					streamErrLogged = true
					if streamStarted {
						writeSanitizedSSEError(w)
					}
					break
				}
				imageGenCounter.AddSSEData([]byte(data))
				if isStreamCompletionEvent(data) {
					completed = true
					diagnostics.completionEvent = gjson.Get(data, "type").String()
				}
			}
		} else if raw := strings.TrimSpace(line); raw != "" {
			diagnostics.observeRaw(raw)
			if streamErr = parseSSEFailureEvent([]byte(raw)); streamErr != nil {
				logStreamFailure(logger, streamErr, resp, streamStarted, diagnostics)
				streamErrLogged = true
				if streamStarted {
					writeSanitizedSSEError(w)
				}
				break
			}
		}

		lineForClient := line
		if ok && data != "" && data != "[DONE]" {
			if patched := normalizeResponsesImageGenerationSSEData(data); patched != data {
				lineForClient = "data: " + patched
			}
		}

		if !ok || data == "" || data == "[DONE]" {
			if err := writeOrBufferSSELine(w, resp.StatusCode, lineForClient, &pending, &streamStarted, diagnostics.hasOutput()); err != nil {
				streamErr = fmt.Errorf("写入客户端 SSE 失败: %w", err)
				break
			}
			continue
		}
		if !firstTokenRecorded {
			usage.FirstTokenMs = time.Since(start).Milliseconds()
			firstTokenRecorded = true
		}
		parseSSEUsage([]byte(data), usage, &toolImageIn, &toolImageOut)
		if err := writeOrBufferSSELine(w, resp.StatusCode, lineForClient, &pending, &streamStarted, diagnostics.hasOutput()); err != nil {
			streamErr = fmt.Errorf("写入客户端 SSE 失败: %w", err)
			break
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}
	if streamErr == nil && !completed {
		streamErr = fmt.Errorf("未收到上游流式完成事件")
	}
	if streamErr == nil && completed && !diagnostics.hasOutput() {
		streamErr = fmt.Errorf("上游流式响应为空：已收到完成事件但没有文本、工具调用或响应输出")
	}

	elapsed := time.Since(start)
	if streamErr != nil {
		if !streamErrLogged {
			logStreamFailure(logger, streamErr, resp, streamStarted, diagnostics)
		}
		partialUsage := usageWithImageTool(usage, imageGenCounter.Count(), imageGenCounter.Size(), toolImageIn, toolImageOut)
		var failure *responsesFailureError
		if errors.As(streamErr, &failure) {
			kind := failure.outcomeKind()
			if streamStarted && kind != sdk.OutcomeClientError {
				kind = sdk.OutcomeStreamAborted
			}
			errBody := openAIErrorJSON(openAIErrorTypeForStatus(failure.StatusCode), string(failure.Kind), failure.Message)
			return sdk.ForwardOutcome{
				Kind:       kind,
				Upstream:   sdk.UpstreamResponse{StatusCode: failure.StatusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
				Reason:     failure.Message,
				RetryAfter: failure.RetryAfter,
				Duration:   elapsed,
				Usage:      partialUsage,
			}, nil
		}
		kind := sdk.OutcomeStreamAborted
		statusCode := resp.StatusCode
		if !streamStarted {
			kind = sdk.OutcomeUpstreamTransient
			statusCode = http.StatusBadGateway
		}
		returnErr := streamErr
		if partialUsage != nil && streamStarted {
			returnErr = nil
		}
		return sdk.ForwardOutcome{
			Kind:     kind,
			Upstream: sdk.UpstreamResponse{StatusCode: statusCode},
			Reason:   streamErr.Error(),
			Duration: elapsed,
			Usage:    partialUsage,
		}, returnErr
	}

	fillUsageCostWithImageTool(usage, imageCountForToolUsage(imageGenCounter.Count(), toolImageOut), imageGenCounter.Size(), toolImageIn, toolImageOut)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

func usageWithImageTool(usage *sdk.Usage, numImages int, size string, imageInputTokens, imageOutputTokens int) *sdk.Usage {
	if usage == nil || numImages <= 0 {
		return nil
	}
	fillUsageCostWithImageTool(usage, imageCountForToolUsage(numImages, imageOutputTokens), size, imageInputTokens, imageOutputTokens)
	return usage
}

func imageCountForToolUsage(numImages, imageOutputTokens int) int {
	if numImages > 0 {
		return numImages
	}
	return estimateImageCountFromTokens(imageOutputTokens)
}

func normalizeResponsesImageGenerationSSEData(data string) string {
	if strings.TrimSpace(data) == "" || !gjson.Valid(data) {
		return data
	}
	eventType := gjson.Get(data, "type").String()
	switch eventType {
	case "response.output_item.done":
		return normalizeImageGenerationCallStatus(data, "item")
	case "response.completed", "response.done":
		return string(normalizeResponsesImageGenerationBody([]byte(data)))
	default:
		return data
	}
}

func normalizeResponsesImageGenerationBody(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	result := body
	for _, path := range []string{"output", "response.output"} {
		output := gjson.GetBytes(result, path)
		if !output.Exists() || !output.IsArray() {
			continue
		}
		for i, item := range output.Array() {
			if !isCompletedImageGenerationCall(item) {
				continue
			}
			patched, err := sjson.SetBytes(result, fmt.Sprintf("%s.%d.status", path, i), "completed")
			if err != nil {
				continue
			}
			result = patched
		}
	}
	return result
}

func normalizeImageGenerationCallStatus(data, itemPath string) string {
	item := gjson.Get(data, itemPath)
	if !isCompletedImageGenerationCall(item) {
		return data
	}
	patched, err := sjson.Set(data, itemPath+".status", "completed")
	if err != nil {
		return data
	}
	return patched
}

func isCompletedImageGenerationCall(item gjson.Result) bool {
	if !item.Exists() || !item.IsObject() {
		return false
	}
	if item.Get("type").String() != "image_generation_call" {
		return false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return false
	}
	status := strings.TrimSpace(item.Get("status").String())
	return status == "" || status == "in_progress" || status == "generating"
}

func logUpstreamSSEDataDebug(logger *slog.Logger, diagnostics streamResponseDiagnostics, data string) {
	logger.Debug("上游 SSE data 诊断",
		"data_event_index", diagnostics.dataEventCount,
		"non_empty_data_event_index", diagnostics.nonEmptyDataEventCount,
		"event_type", streamDiagnosticEventType(data),
		"finish_reason", streamDiagnosticFinishReason(data),
		"has_output", streamDataHasOutput(data),
		"data_preview", truncate(data, streamDiagnosticPreviewBytes),
	)
}

type streamResponseDiagnostics struct {
	lineCount              int
	dataEventCount         int
	nonEmptyDataEventCount int
	rawLineCount           int
	forwardedBytes         int
	outputEventCount       int
	firstDataPreview       string
	lastDataPreview        string
	firstRawPreview        string
	lastRawPreview         string
	completionEvent        string
	finishReason           string
	eventTypes             []string
}

func (d *streamResponseDiagnostics) observeLine(line string) {
	d.lineCount++
	d.forwardedBytes += len(line) + 1
}

func (d *streamResponseDiagnostics) observeData(data string) {
	d.dataEventCount++
	if data == "" {
		return
	}
	d.nonEmptyDataEventCount++
	preview := truncate(data, streamDiagnosticPreviewBytes)
	if d.firstDataPreview == "" {
		d.firstDataPreview = preview
	}
	d.lastDataPreview = preview
	if eventType := streamDiagnosticEventType(data); eventType != "" {
		d.appendEventType(eventType)
	}
	if finishReason := streamDiagnosticFinishReason(data); finishReason != "" {
		d.finishReason = finishReason
	}
	if streamDataHasOutput(data) {
		d.outputEventCount++
	}
}

func (d *streamResponseDiagnostics) observeRaw(raw string) {
	d.rawLineCount++
	preview := truncate(raw, streamDiagnosticPreviewBytes)
	if d.firstRawPreview == "" {
		d.firstRawPreview = preview
	}
	d.lastRawPreview = preview
}

func (d *streamResponseDiagnostics) hasOutput() bool {
	return d.outputEventCount > 0
}

func (d *streamResponseDiagnostics) appendEventType(eventType string) {
	for _, existing := range d.eventTypes {
		if existing == eventType {
			return
		}
	}
	if len(d.eventTypes) >= 8 {
		return
	}
	d.eventTypes = append(d.eventTypes, eventType)
}

func (d streamResponseDiagnostics) logAttrs(resp *http.Response, streamStarted bool) []any {
	statusCode := 0
	contentType := ""
	transferEncoding := ""
	upstreamRequestID := ""
	if resp != nil {
		statusCode = resp.StatusCode
		contentType = resp.Header.Get("Content-Type")
		transferEncoding = strings.Join(resp.TransferEncoding, ",")
		upstreamRequestID = firstNonEmptyHeader(resp.Header, "X-Request-Id", "Openai-Request-Id", "Cf-Ray")
	}
	attrs := []any{
		sdk.LogFieldStatus, statusCode,
		"stream_started", streamStarted,
		"line_count", d.lineCount,
		"data_event_count", d.dataEventCount,
		"non_empty_data_event_count", d.nonEmptyDataEventCount,
		"raw_line_count", d.rawLineCount,
		"output_event_count", d.outputEventCount,
		"forwarded_bytes", d.forwardedBytes,
		"completion_event", d.completionEvent,
		"finish_reason", d.finishReason,
		"event_types", strings.Join(d.eventTypes, ","),
		"content_type", contentType,
		"transfer_encoding", transferEncoding,
	}
	if upstreamRequestID != "" {
		attrs = append(attrs, "upstream_request_id", upstreamRequestID)
	}
	if d.firstDataPreview != "" {
		attrs = append(attrs, "first_data_preview", d.firstDataPreview)
	}
	if d.lastDataPreview != "" && d.lastDataPreview != d.firstDataPreview {
		attrs = append(attrs, "last_data_preview", d.lastDataPreview)
	}
	if d.firstRawPreview != "" {
		attrs = append(attrs, "first_raw_preview", d.firstRawPreview)
	}
	if d.lastRawPreview != "" && d.lastRawPreview != d.firstRawPreview {
		attrs = append(attrs, "last_raw_preview", d.lastRawPreview)
	}
	return attrs
}

func logStreamFailure(logger *slog.Logger, err error, resp *http.Response, streamStarted bool, diagnostics streamResponseDiagnostics) {
	if logger == nil {
		logger = slog.Default()
	}
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	attrs := diagnostics.logAttrs(resp, streamStarted)
	var failure *responsesFailureError
	if errors.As(err, &failure) {
		attrs = append(attrs,
			"kind", failure.Kind,
			"classified_status_code", firstNonZero(failure.StatusCode, statusCode),
			"message", failure.Message)
		logger.Warn("上游 SSE 返回错误，已记录诊断详情", attrs...)
		return
	}
	attrs = append(attrs, "error", err)
	logger.Warn("上游 SSE 返回异常或空响应，已记录诊断详情", attrs...)
}

func writeOrBufferSSELine(w http.ResponseWriter, statusCode int, line string, pending *strings.Builder, streamStarted *bool, shouldFlush bool) error {
	if *streamStarted {
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return err
		}
		flushResponseWriter(w)
		return nil
	}

	pending.WriteString(line)
	pending.WriteByte('\n')
	if !shouldFlush {
		return nil
	}

	w.WriteHeader(statusCode)
	if _, err := fmt.Fprint(w, pending.String()); err != nil {
		return err
	}
	pending.Reset()
	*streamStarted = true
	flushResponseWriter(w)
	return nil
}

func flushResponseWriter(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeSanitizedSSEError(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"请求暂时无法完成，请稍后重试\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n"))
	flushResponseWriter(w)
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// handleNonStreamResponse 处理非流式响应。resp.StatusCode 预设 2xx。
func handleNonStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time, reqServiceTier string) (sdk.ForwardOutcome, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取上游响应失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	body = normalizeResponsesImageGenerationBody(body)

	parsed := parseUsage(body)

	headers := resp.Header.Clone()
	if w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		passCodexRateLimitHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	elapsed := time.Since(start)
	usage := newTokenUsage(
		gjson.GetBytes(body, "model").String(),
		firstNonEmptyTier(reqServiceTier, normalizeOpenAIServiceTier(gjson.GetBytes(body, "service_tier").String())),
		parsed.inputTokens,
		parsed.outputTokens,
		parsed.cachedInputTokens,
		parsed.reasoningOutputTokens,
		elapsed.Milliseconds(),
	)
	numImages := parsed.imageGenCallCount
	if numImages <= 0 {
		numImages = estimateImageCountFromTokens(parsed.toolImageOutputTokens)
	}
	fillUsageCostWithImageTool(usage, numImages, parsed.imageGenCallSize, parsed.toolImageInputTokens, parsed.toolImageOutputTokens)

	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode, Headers: headers},
		Usage:    usage,
		Duration: elapsed,
	}
	// Writer 不可用时通过 Body 透传给 Core 写响应
	if w == nil {
		outcome.Upstream.Body = body
	}
	return outcome, nil
}

// ParseSSEStream 从 SSE 流中解析事件，通过 handler 回调输出，返回统一的 WSResult
// 供 cmd/chat 等外部调用者复用，与 ReceiveWSResponse 签名对齐
func ParseSSEStream(reader io.Reader, handler WSEventHandler) WSResult {
	start := time.Now()
	result := WSResult{}
	var textBuilder strings.Builder
	var reasoningBuilder strings.Builder

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)

	for scanner.Scan() {
		line := scanner.Text()

		data, ok := extractSSEData(line)
		if !ok || len(data) == 0 || data == "[DONE]" {
			continue
		}

		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}

		eventType, _ := ev["type"].(string)

		// 通知 handler 原始事件
		if handler != nil {
			handler.OnRawEvent(eventType, []byte(data))
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

		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok {
				collectImageGenCallMetadata(&result, item)
			}

		case "response.image_generation_call.partial_image":
			collectImageGenCallPartial(&result, ev)

		case "response.completed", "response.done":
			result.CompletedEventRaw = append([]byte(nil), []byte(data)...)
			if resp, ok := ev["response"].(map[string]any); ok {
				mergeResponseMetadata(&result, resp)
				result.StopReason = jsonString(resp["stop_reason"])
				extractUsageFromResponseMap(&result, resp)
			}
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
			return result

		case "response.failed":
			result.FailedEventRaw = append([]byte(nil), []byte(data)...)
			if resp, ok := ev["response"].(map[string]any); ok {
				extractUsageFromResponseMap(&result, resp)
				mergeResponseMetadata(&result, resp)
			}
			if failure := classifyResponsesFailure([]byte(data)); failure != nil {
				result.Err = failure
			} else {
				result.Err = fmt.Errorf("上游错误: %s", data)
			}
			finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
			return result

		case "response.incomplete":
			reason := "unknown"
			if resp, ok := ev["response"].(map[string]any); ok {
				extractUsageFromResponseMap(&result, resp)
				mergeResponseMetadata(&result, resp)
				if details, ok := resp["incomplete_details"].(map[string]any); ok {
					if r, ok := details["reason"].(string); ok {
						reason = r
					}
				}
			}
			if reason == "max_output_tokens" {
				result.CompletedEventRaw = append([]byte(nil), []byte(data)...)
				result.StopReason = reason
			} else {
				result.Err = fmt.Errorf("响应不完整: %s", reason)
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

	if err := scanner.Err(); err != nil && result.Err == nil {
		result.Err = fmt.Errorf("读取 SSE 失败: %w", err)
	}

	finalizeWSResult(&result, &textBuilder, &reasoningBuilder, start)
	return result
}

// isStreamCompletionEvent 判断 SSE data 是否为流式完成事件。
// Responses API 以 response.completed / response.done 结束，
// Chat Completions API 以 [DONE] 结束（调用方在解析 data 前已单独处理）。
func isStreamCompletionEvent(data string) bool {
	eventType := gjson.Get(data, "type").String()
	return eventType == "response.completed" || eventType == "response.done"
}

func streamDiagnosticEventType(data string) string {
	eventType := strings.TrimSpace(gjson.Get(data, "type").String())
	if eventType != "" {
		return eventType
	}
	if gjson.Get(data, "choices").Exists() {
		return "chat.completion.chunk"
	}
	if gjson.Get(data, "error").Exists() {
		return "error"
	}
	if data == "[DONE]" {
		return "[DONE]"
	}
	return ""
}

func streamDiagnosticFinishReason(data string) string {
	if reason := strings.TrimSpace(gjson.Get(data, "response.status").String()); reason != "" {
		return reason
	}
	if reason := strings.TrimSpace(gjson.Get(data, "response.incomplete_details.reason").String()); reason != "" {
		return reason
	}
	for _, choice := range gjson.Get(data, "choices").Array() {
		if reason := strings.TrimSpace(choice.Get("finish_reason").String()); reason != "" {
			return reason
		}
	}
	return ""
}

func streamDataHasOutput(data string) bool {
	if data == "" || data == "[DONE]" {
		return false
	}
	if streamChatChoicesHaveOutput(data) {
		return true
	}

	eventType := gjson.Get(data, "type").String()
	switch eventType {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.refusal.delta":
		return strings.TrimSpace(gjson.Get(data, "delta").String()) != ""
	case "response.output_text.done":
		return strings.TrimSpace(gjson.Get(data, "text").String()) != ""
	case "response.function_call_arguments.delta":
		return gjson.Get(data, "delta").Exists()
	case "response.function_call_arguments.done":
		return gjson.Get(data, "arguments").Exists()
	case "response.content_part.added", "response.content_part.done":
		return responseContentPartHasOutput(firstExistingGJSON(
			gjson.Get(data, "part"),
			gjson.Get(data, "content_part"),
			gjson.Get(data, "content"),
		))
	case "response.image_generation_call.partial_image":
		return strings.TrimSpace(firstNonEmptyString(
			gjson.Get(data, "partial_image_b64").String(),
			gjson.Get(data, "partial_image").String(),
			gjson.Get(data, "result").String(),
		)) != ""
	case "response.output_item.added", "response.output_item.done":
		return responseItemHasOutput(gjson.Get(data, "item"))
	case "response.completed", "response.done":
		return responseOutputHasContent(gjson.Get(data, "response.output"))
	default:
		return false
	}
}

func streamChatChoicesHaveOutput(data string) bool {
	choices := gjson.Get(data, "choices")
	if !choices.Exists() {
		return false
	}
	for _, choice := range choices.Array() {
		delta := choice.Get("delta")
		if strings.TrimSpace(delta.Get("content").String()) != "" {
			return true
		}
		if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && len(toolCalls.Array()) > 0 {
			return true
		}
		if delta.Get("function_call").Exists() {
			return true
		}
		message := choice.Get("message")
		if strings.TrimSpace(message.Get("content").String()) != "" {
			return true
		}
		if message.Get("tool_calls").Exists() || message.Get("function_call").Exists() {
			return true
		}
	}
	return false
}

func responseOutputHasContent(output gjson.Result) bool {
	for _, item := range output.Array() {
		if responseItemHasOutput(item) {
			return true
		}
	}
	return false
}

func responseItemHasOutput(item gjson.Result) bool {
	itemType := item.Get("type").String()
	switch itemType {
	case "message":
		content := item.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				if responseContentPartHasOutput(part) {
					return true
				}
			}
			return false
		}
		return responseContentPartHasOutput(content)
	case "function_call", "web_search_call", "image_generation_call", "code_interpreter_call":
		return true
	default:
		return strings.Contains(itemType, "call")
	}
}

func responseContentPartHasOutput(part gjson.Result) bool {
	if !part.Exists() {
		return false
	}
	if part.IsArray() {
		for _, item := range part.Array() {
			if responseContentPartHasOutput(item) {
				return true
			}
		}
		return false
	}
	if !part.IsObject() {
		return strings.TrimSpace(part.String()) != ""
	}

	partType := strings.TrimSpace(firstNonEmptyString(part.Get("type").String(), part.Get("content_type").String()))
	switch partType {
	case "output_text", "text", "input_text", "refusal":
		return strings.TrimSpace(firstNonEmptyString(part.Get("text").String(), part.Get("refusal").String())) != ""
	}

	for _, path := range []string{
		"text",
		"refusal",
		"image_url",
		"image_url.url",
		"asset_pointer",
		"file_id",
		"b64_json",
		"data",
		"url",
		"result",
		"partial_image_b64",
		"partial_image",
	} {
		if strings.TrimSpace(part.Get(path).String()) != "" {
			return true
		}
	}

	switch {
	case strings.Contains(partType, "image"),
		strings.Contains(partType, "asset"),
		strings.Contains(partType, "file"),
		strings.Contains(partType, "audio"),
		strings.Contains(partType, "video"):
		return responseContentPartHasPayload(part)
	default:
		return false
	}
}

func responseContentPartHasPayload(part gjson.Result) bool {
	hasPayload := false
	part.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "type", "content_type", "status", "index", "output_index", "content_index":
			return true
		}
		if responseContentPartHasOutput(value) {
			hasPayload = true
			return false
		}
		return true
	})
	return hasPayload
}

func firstExistingGJSON(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func firstNonEmptyHeader(headers http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// extractSSEData 从 SSE 行中提取 data 内容
func extractSSEData(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	s := line[5:]
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s, true
}

// parseSSEUsage 把 SSE 事件中的 usage 字段累加到 sdk.Usage。
// toolImageIn/toolImageOut 可选累加器（响应 tool_usage.image_gen）。
func parseSSEUsage(data []byte, out *sdk.Usage, toolImageIn, toolImageOut *int) {
	eventType := gjson.GetBytes(data, "type").String()

	switch eventType {
	case "response.completed", "response.done":
		resp := gjson.GetBytes(data, "response")
		if !resp.Exists() {
			return
		}
		out.Model = resp.Get("model").String()
		setUsageModelAttribute(out, out.Model)
		if usageServiceTier(out) == "" {
			setUsageServiceTier(out, resp.Get("service_tier").String())
		}
		usage := resp.Get("usage")
		if usage.Exists() {
			inputTokens := int(usage.Get("input_tokens").Int())
			outputTokens := int(usage.Get("output_tokens").Int())
			cachedInputTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
			reasoningOutputTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
			if cachedInputTokens > 0 && inputTokens >= cachedInputTokens {
				inputTokens -= cachedInputTokens
			}
			setUsageTokens(out, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens)
		}
		if imgTool := resp.Get("tool_usage.image_gen"); imgTool.Exists() {
			if toolImageIn != nil {
				*toolImageIn = int(imgTool.Get("input_tokens").Int())
			}
			if toolImageOut != nil {
				*toolImageOut = int(imgTool.Get("output_tokens").Int())
			}
		}

	default:
		usage := gjson.GetBytes(data, "usage")
		if !usage.Exists() {
			return
		}
		inputTokens := int(usage.Get("prompt_tokens").Int())
		outputTokens := int(usage.Get("completion_tokens").Int())
		cachedInputTokens := int(usage.Get("prompt_tokens_details.cached_tokens").Int())
		reasoningOutputTokens := int(usage.Get("completion_tokens_details.reasoning_tokens").Int())
		out.Model = gjson.GetBytes(data, "model").String()
		setUsageModelAttribute(out, out.Model)
		if cachedInputTokens > 0 && inputTokens >= cachedInputTokens {
			inputTokens -= cachedInputTokens
		}
		setUsageTokens(out, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens)
	}
}

// parseSSEFailureEvent 解析 Responses API 的失败事件并映射为错误
func parseSSEFailureEvent(data []byte) error {
	if failure := classifyResponsesFailure(data); failure != nil {
		return failure
	}
	if failure := classifyWSErrorEvent(data); failure != nil {
		return failure
	}
	if failure := classifyGenericSSEErrorEvent(data); failure != nil {
		return failure
	}
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "response.failed":
		errNode := gjson.GetBytes(data, "response.error")
		msg := strings.TrimSpace(errNode.Get("message").String())
		if msg == "" {
			msg = "上游返回 response.failed"
		}
		errType := strings.ToLower(errNode.Get("type").String())
		errCode := strings.ToLower(errNode.Get("code").String())

		switch {
		case containsAny(errType, errCode, msg, "previous_response_not_found", "previous response", "response not found"):
			return fmt.Errorf("上游续链锚点失效: %s", msg)
		case containsAny(errType, errCode, msg, "context_length", "context window", "max_tokens", "max_input_tokens", "max_output_tokens", "token limit", "too many tokens"):
			return fmt.Errorf("上游上下文窗口超限: %s", msg)
		case containsAny(errType, errCode, msg, "quota", "insufficient_quota"):
			return fmt.Errorf("上游配额不足: %s", msg)
		case containsAny(errType, errCode, msg, "usage_not_included"):
			return fmt.Errorf("上游使用权不包含: %s", msg)
		case containsAny(errType, errCode, msg, "invalid_prompt", "invalid_request"):
			return fmt.Errorf("上游请求无效: %s", msg)
		case containsAny(errType, errCode, msg, "server_overloaded", "overloaded", "slow_down"):
			return fmt.Errorf("上游服务繁忙: %s", msg)
		case containsAny(errType, errCode, msg, "rate_limit"):
			delay := parseRetryDelay(msg)
			if delay > 0 {
				return fmt.Errorf("上游速率限制(建议 %s 后重试): %s", delay, msg)
			}
			return fmt.Errorf("上游速率限制: %s", msg)
		default:
			return fmt.Errorf("上游流式失败(type=%s, code=%s): %s", errType, errCode, msg)
		}

	case "response.incomplete":
		reason := gjson.GetBytes(data, "response.incomplete_details.reason").String()
		if reason == "" {
			reason = "unknown"
		}
		return fmt.Errorf("上游返回不完整响应: %s", reason)
	}
	return nil
}

// openaiUsage 非流式响应的 usage 解析结果
type openaiUsage struct {
	inputTokens           int
	outputTokens          int
	cachedInputTokens     int
	reasoningOutputTokens int
	// image_generation tool 的用量，从 response.tool_usage.image_gen 提取。
	// 按本次对话模型单价单独归集，便于 Core 对生成图片输出做固定价覆盖。
	toolImageInputTokens  int
	toolImageOutputTokens int
	imageGenCallCount     int
	imageGenCallSize      string
}

// parseUsage 从完整响应体解析 usage
func parseUsage(body []byte) openaiUsage {
	usage := openaiUsage{}
	usageNode := gjson.GetBytes(body, "usage")
	if !usageNode.Exists() {
		forEachSSEDataPayload(body, func(data []byte) {
			if usageNode.Exists() {
				return
			}
			eventType := gjson.GetBytes(data, "type").String()
			if eventType == "response.completed" || eventType == "response.done" {
				usageNode = gjson.GetBytes(data, "response.usage")
			}
		})
	}
	if !usageNode.Exists() {
		usage.imageGenCallCount, usage.imageGenCallSize = collectImageGenCallSummaryFromBody(body)
		return usage
	}

	usage.inputTokens = int(usageNode.Get("input_tokens").Int())
	usage.outputTokens = int(usageNode.Get("output_tokens").Int())

	if usage.inputTokens == 0 {
		usage.inputTokens = int(usageNode.Get("prompt_tokens").Int())
	}
	if usage.outputTokens == 0 {
		usage.outputTokens = int(usageNode.Get("completion_tokens").Int())
	}

	// 仅提取 cache read（缓存命中）token，不含 cache creation
	// cache_creation 按正常输入价计费，已包含在 input_tokens 中无需额外处理
	cacheRead := int(usageNode.Get("cache_read_input_tokens").Int())
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("input_tokens_details.cached_tokens").Int())
	}
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("prompt_tokens_details.cached_tokens").Int())
	}
	usage.cachedInputTokens = cacheRead

	// 从 input_tokens 中扣除缓存部分，避免计费器重复计算
	if cacheRead > 0 && usage.inputTokens >= cacheRead {
		usage.inputTokens -= cacheRead
	}

	// 提取推理 token（o1/o3 等模型）
	usage.reasoningOutputTokens = int(usageNode.Get("output_tokens_details.reasoning_tokens").Int())
	if usage.reasoningOutputTokens == 0 {
		usage.reasoningOutputTokens = int(usageNode.Get("completion_tokens_details.reasoning_tokens").Int())
	}

	// tool_usage.image_gen 位于 response 顶层（与 usage 平级），Responses 响应体里可能
	// 在 "response" 子对象下，也可能直接在根对象下。两条路径都试。
	toolUsage := gjson.GetBytes(body, "tool_usage.image_gen")
	if !toolUsage.Exists() {
		toolUsage = gjson.GetBytes(body, "response.tool_usage.image_gen")
	}
	if toolUsage.Exists() {
		usage.toolImageInputTokens = int(toolUsage.Get("input_tokens").Int())
		usage.toolImageOutputTokens = int(toolUsage.Get("output_tokens").Int())
	}
	usage.imageGenCallCount, usage.imageGenCallSize = collectImageGenCallSummaryFromBody(body)

	return usage
}

func collectImageGenCallSummary(data []byte) (bool, string) {
	item := gjson.GetBytes(data, "item")
	if !item.Exists() {
		return false, ""
	}
	return collectImageGenCallSummaryFromItem(item)
}

func collectImageGenCallSummaryFromBody(body []byte) (int, string) {
	counter := newImageGenCallCounter()
	for _, path := range []string{"output", "response.output"} {
		counter.AddOutputArray(gjson.GetBytes(body, path))
	}
	forEachSSEDataPayload(body, func(data []byte) {
		counter.AddSSEData(data)
	})
	return counter.Count(), counter.Size()
}

func collectImageGenCallSummaryFromItem(item gjson.Result) (bool, string) {
	counter := newImageGenCallCounter()
	if !counter.AddItem(item) {
		return false, ""
	}
	return true, counter.Size()
}

type imageGenCallCounter struct {
	seen  map[string]struct{}
	count int
	size  string
}

func newImageGenCallCounter() *imageGenCallCounter {
	return &imageGenCallCounter{seen: make(map[string]struct{})}
}

func (c *imageGenCallCounter) Count() int {
	if c == nil {
		return 0
	}
	return c.count
}

func (c *imageGenCallCounter) Size() string {
	if c == nil {
		return ""
	}
	return c.size
}

func (c *imageGenCallCounter) AddSSEData(data []byte) {
	if c == nil || len(data) == 0 || !gjson.ValidBytes(data) {
		return
	}
	root := gjson.ParseBytes(data)
	switch root.Get("type").String() {
	case "response.output_item.done":
		c.AddItem(root.Get("item"))
	case "response.completed", "response.done":
		c.AddOutputArray(root.Get("response.output"))
	}
}

func (c *imageGenCallCounter) AddOutputArray(output gjson.Result) {
	if c == nil || !output.Exists() || !output.IsArray() {
		return
	}
	for _, item := range output.Array() {
		c.AddItem(item)
	}
}

func (c *imageGenCallCounter) AddItem(item gjson.Result) bool {
	if c == nil || !item.Exists() || !item.IsObject() {
		return false
	}
	if item.Get("type").String() != "image_generation_call" {
		return false
	}
	result := item.Get("result").String()
	if result == "" {
		return false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("call_id").String())
	}
	if key == "" {
		key = result
	}
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = struct{}{}
	size := item.Get("size").String()
	if size == "" {
		if actualSize, ok := imageActualSizeFromBase64(result); ok {
			size = actualSize
		}
	}
	if c.size == "" && size != "" {
		c.size = size
	}
	c.count++
	return true
}

func forEachSSEDataPayload(body []byte, fn func([]byte)) {
	if len(body) == 0 || fn == nil {
		return
	}
	for _, line := range strings.Split(string(body), "\n") {
		data, ok := extractSSEData(line)
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		fn([]byte(data))
	}
}

// retryDelayPattern 匹配 "try again in Ns" / "try again in Nms" 格式
var retryDelayPattern = regexp.MustCompile(`(?i)try again in\s*(\d+(?:\.\d+)?)\s*(s|ms|seconds?)`)

// parseRetryDelay 从错误消息中提取建议重试延迟
func parseRetryDelay(msg string) time.Duration {
	matches := retryDelayPattern.FindStringSubmatch(msg)
	if len(matches) < 3 {
		return 0
	}
	val, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(matches[2])
	if unit == "ms" {
		return time.Duration(val * float64(time.Millisecond))
	}
	return time.Duration(val * float64(time.Second))
}

func containsAny(values ...string) bool {
	if len(values) < 4 {
		return false
	}
	haystacks := []string{
		strings.ToLower(values[0]),
		strings.ToLower(values[1]),
		strings.ToLower(values[2]),
	}
	for i := 3; i < len(values); i++ {
		kw := strings.ToLower(values[i])
		if kw == "" {
			continue
		}
		for _, h := range haystacks {
			if strings.Contains(h, kw) {
				return true
			}
		}
	}
	return false
}
