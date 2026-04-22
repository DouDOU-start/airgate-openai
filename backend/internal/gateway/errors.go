package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// 统一错误分类工具（跨 OpenAI / Anthropic 协议共用）。
//
// 新契约：插件用 OutcomeKind 向 Core 表达判决。classifyHTTPFailure / classifyMessage
// 是所有错误路径的唯一分类入口。

// classifyHTTPFailure 把 HTTP 状态码 + 错误文本归一化为 OutcomeKind。
// 返回的 Kind 决定 Core 如何处置账号：
//
//	429 → AccountRateLimited
//	401 / 403 → AccountDead（附加消息关键词检查"usage limit" / "rate limit" 等会降级为 RateLimited）
//	400 + 消息含限流关键词 → AccountRateLimited（部分上游用 400 返回 usage_limit_reached）
//	400 + 消息含 disabled/deactivated → AccountDead
//	5xx → UpstreamTransient
//	其它 4xx → ClientError（客户端请求自己的问题，账号无辜）
func classifyHTTPFailure(statusCode int, message string) sdk.OutcomeKind {
	if isTemporaryRateLimitText(message) && (statusCode == 400 || statusCode == 403 || statusCode == 429) {
		return sdk.OutcomeAccountRateLimited
	}
	if isDisabledAccountText(message) && (statusCode == 400 || statusCode == 403) {
		return sdk.OutcomeAccountDead
	}
	switch statusCode {
	case 429:
		return sdk.OutcomeAccountRateLimited
	case 401, 403:
		return sdk.OutcomeAccountDead
	}
	if statusCode >= 500 {
		return sdk.OutcomeUpstreamTransient
	}
	if statusCode >= 400 {
		return sdk.OutcomeClientError
	}
	return sdk.OutcomeSuccess
}

// classifyAnthropicBody 从 Anthropic 错误响应体（400 可能是账号级）归一化 OutcomeKind。
func classifyAnthropicBody(statusCode int, body []byte) sdk.OutcomeKind {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = string(body)
	}
	return classifyHTTPFailure(statusCode, msg)
}

func isTemporaryRateLimitText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "usage limit") ||
		strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "too many requests") ||
		strings.Contains(combined, "quota exceeded") ||
		strings.Contains(combined, "insufficient quota") ||
		strings.Contains(combined, "insufficient_quota") ||
		strings.Contains(combined, "billing hard limit") ||
		strings.Contains(combined, "billing_hard_limit_reached") ||
		strings.Contains(combined, "slow down") ||
		strings.Contains(combined, "slow_down") ||
		strings.Contains(combined, "try again later") ||
		strings.Contains(combined, "try again in") ||
		strings.Contains(combined, "retry after")
}

func isDisabledAccountText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "disabled") ||
		strings.Contains(combined, "deactivated") ||
		strings.Contains(combined, "suspended")
}

// anthropicErrorType 根据 HTTP 状态码返回 Anthropic 错误类型。
func anthropicErrorType(statusCode int) string {
	switch statusCode {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 422:
		return "invalid_model_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicErrorJSON 纯 sjson 构建并写入 Anthropic 格式错误响应。
func writeAnthropicErrorJSON(w http.ResponseWriter, statusCode int, errType, message string) {
	out := `{"type":"error","error":{"type":"","message":""}}`
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(out))
}

// extractRetryAfterHeader 从响应头提取 Retry-After。
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := headers.Get("Retry-After")
	if val == "" {
		return 0
	}
	return parseRetryDelay("try again in " + val + "s")
}

// truncate 截断字符串到指定长度。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
