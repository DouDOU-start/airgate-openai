package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// upstreamSSEMaxLineBytes 是上游 SSE 单行最大字节数。
// 上游某些事件（例如 response.output_item.done 携带完整输出 / 大段 reasoning summary）
// 可能远超 1 MB，过小会触发 bufio.Scanner: token too long 中断流。
const upstreamSSEMaxLineBytes = 8 * 1024 * 1024

// largeSSEEventThreshold 触发大事件诊断日志的阈值。
// 超过这个长度的单行/翻译输出会被打印 type 与长度，便于追踪谁在膨胀。
const largeSSEEventThreshold = 512 * 1024

// handleStreamResponse 处理 SSE 流式响应
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	// 设置 SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 透传上游 Codex 速率限制头
	passCodexRateLimitHeaders(resp.Header, w.Header())

	w.WriteHeader(resp.StatusCode)

	result := &sdk.ForwardResult{
		StatusCode: resp.StatusCode,
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)
	var streamErr error
	firstTokenRecorded := false

	for scanner.Scan() {
		line := scanner.Text()

		// 写入到客户端
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		// 提取 SSE data 行
		data, ok := extractSSEData(line)
		if !ok || len(data) == 0 || data == "[DONE]" {
			continue
		}

		// 记录首 token 延迟（首次收到含数据的 SSE 行）
		if !firstTokenRecorded {
			result.FirstTokenMs = time.Since(start).Milliseconds()
			firstTokenRecorded = true
		}

		// 解析 usage（仅在 response.completed 事件中）
		parseSSEUsage([]byte(data), result)

		// 捕获上游 SSE 失败事件
		if streamErr == nil {
			streamErr = parseSSEFailureEvent([]byte(data))
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	result.Duration = time.Since(start)
	if streamErr != nil {
		var failure *responsesFailureError
		if errors.As(streamErr, &failure) {
			result.StatusCode = failure.StatusCode
			result.AccountStatus = failure.AccountStatus
			result.ErrorMessage = failure.Message
			result.RetryAfter = failure.RetryAfter
			if failure.shouldReturnClientError() {
				return result, nil
			}
			return result, streamErr
		}
		if result.StatusCode < http.StatusBadRequest {
			result.StatusCode = http.StatusBadGateway
		}
		return result, streamErr
	}
	fillCost(result)
	return result, nil
}

// handleNonStreamResponse 处理非流式响应
func handleNonStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	usage := parseUsage(body)

	if w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		passCodexRateLimitHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	elapsed := time.Since(start)
	result := &sdk.ForwardResult{
		StatusCode:            resp.StatusCode,
		InputTokens:           usage.inputTokens,
		OutputTokens:          usage.outputTokens,
		CachedInputTokens:     usage.cachedInputTokens,
		ReasoningOutputTokens: usage.reasoningOutputTokens,
		ServiceTier:           normalizeOpenAIServiceTier(gjson.GetBytes(body, "service_tier").String()),
		Model:                 gjson.GetBytes(body, "model").String(),
		Duration:              elapsed,
		FirstTokenMs:          elapsed.Milliseconds(),
	}
	// Writer 不可用时，通过 Body/Headers 返回给 Core 写响应
	if w == nil {
		result.Body = body
		result.Headers = resp.Header.Clone()
	}
	fillCost(result)
	return result, nil
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
			}

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
				if details, ok := resp["incomplete_details"].(map[string]any); ok {
					if r, ok := details["reason"].(string); ok {
						reason = r
					}
				}
			}
			result.Err = fmt.Errorf("响应不完整: %s", reason)
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

// parseSSEUsage 从 SSE 数据中提取 usage 信息
func parseSSEUsage(data []byte, result *sdk.ForwardResult) {
	eventType := gjson.GetBytes(data, "type").String()

	switch eventType {
	case "response.completed", "response.done":
		resp := gjson.GetBytes(data, "response")
		if !resp.Exists() {
			return
		}
		result.Model = resp.Get("model").String()
		// 从上游 response.completed 事件中提取 service_tier
		if tier := normalizeOpenAIServiceTier(resp.Get("service_tier").String()); tier != "" {
			result.ServiceTier = tier
		}
		usage := resp.Get("usage")
		if usage.Exists() {
			result.InputTokens = int(usage.Get("input_tokens").Int())
			result.OutputTokens = int(usage.Get("output_tokens").Int())
			result.CachedInputTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
			result.ReasoningOutputTokens = int(usage.Get("output_tokens_details.reasoning_tokens").Int())
			// 从 input_tokens 中扣除缓存部分，避免计费重复计算
			if result.CachedInputTokens > 0 && result.InputTokens >= result.CachedInputTokens {
				result.InputTokens -= result.CachedInputTokens
			}
		}

	default:
		usage := gjson.GetBytes(data, "usage")
		if !usage.Exists() {
			return
		}
		result.InputTokens = int(usage.Get("prompt_tokens").Int())
		result.OutputTokens = int(usage.Get("completion_tokens").Int())
		result.Model = gjson.GetBytes(data, "model").String()
		result.CachedInputTokens = int(usage.Get("prompt_tokens_details.cached_tokens").Int())
		result.ReasoningOutputTokens = int(usage.Get("completion_tokens_details.reasoning_tokens").Int())
		// 从 prompt_tokens 中扣除缓存部分，避免计费重复计算
		if result.CachedInputTokens > 0 && result.InputTokens >= result.CachedInputTokens {
			result.InputTokens -= result.CachedInputTokens
		}
	}
}

// parseSSEFailureEvent 解析 Responses API 的失败事件并映射为错误
func parseSSEFailureEvent(data []byte) error {
	if failure := classifyResponsesFailure(data); failure != nil {
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
}

// parseUsage 从完整响应体解析 usage
func parseUsage(body []byte) openaiUsage {
	usage := openaiUsage{}
	usageNode := gjson.GetBytes(body, "usage")
	if !usageNode.Exists() {
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

	return usage
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
