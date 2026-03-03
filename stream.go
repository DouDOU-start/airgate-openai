package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

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
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var streamErr error

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
		if result.StatusCode < http.StatusBadRequest {
			result.StatusCode = http.StatusBadGateway
		}
		return result, streamErr
	}
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

	return &sdk.ForwardResult{
		StatusCode:   resp.StatusCode,
		InputTokens:  usage.inputTokens,
		OutputTokens: usage.outputTokens,
		CacheTokens:  usage.cacheTokens,
		Model:        gjson.GetBytes(body, "model").String(),
		Duration:     time.Since(start),
	}, nil
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
		usage := resp.Get("usage")
		if usage.Exists() {
			result.InputTokens = int(usage.Get("input_tokens").Int())
			result.OutputTokens = int(usage.Get("output_tokens").Int())
			result.CacheTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
		}

	default:
		usage := gjson.GetBytes(data, "usage")
		if !usage.Exists() {
			return
		}
		result.InputTokens = int(usage.Get("prompt_tokens").Int())
		result.OutputTokens = int(usage.Get("completion_tokens").Int())
		result.Model = gjson.GetBytes(data, "model").String()
		result.CacheTokens = int(usage.Get("prompt_tokens_details.cached_tokens").Int())
	}
}

// parseSSEFailureEvent 解析 Responses API 的失败事件并映射为错误
func parseSSEFailureEvent(data []byte) error {
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
		case containsAny(errType, errCode, msg, "context_length", "context window", "max_tokens"):
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
	inputTokens  int
	outputTokens int
	cacheTokens  int
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

	cacheCreation := int(usageNode.Get("cache_creation_input_tokens").Int())
	cacheRead := int(usageNode.Get("cache_read_input_tokens").Int())
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("input_tokens_details.cached_tokens").Int())
	}
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("prompt_tokens_details.cached_tokens").Int())
	}
	usage.cacheTokens = cacheCreation + cacheRead

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
