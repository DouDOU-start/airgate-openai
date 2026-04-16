package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// 统一错误处理工具（跨 OpenAI / Anthropic 协议共用）
// ──────────────────────────────────────────────────────

// accountStatusFromCode 根据 HTTP 状态码推断账号状态（供核心调度使用）
func accountStatusFromCode(statusCode int) sdk.AccountStatus {
	switch statusCode {
	case 429:
		return sdk.AccountStatusRateLimited
	case 401:
		return sdk.AccountStatusExpired
	case 403:
		return sdk.AccountStatusDisabled
	default:
		return sdk.AccountStatusOK
	}
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

func accountStatusFromMessage(statusCode int, message string) sdk.AccountStatus {
	base := accountStatusFromCode(statusCode)
	if statusCode != 400 && statusCode != 403 {
		return base
	}
	if isTemporaryRateLimitText(message) {
		return sdk.AccountStatusRateLimited
	}
	if isDisabledAccountText(message) {
		return sdk.AccountStatusDisabled
	}
	return base
}

// accountStatusFromAnthropicBody 从 Anthropic 错误响应体推断账号状态。
// Anthropic 某些账号级错误（如组织被封禁）走 400，accountStatusFromCode 无法识别，
// 需额外检查 error.message 内容。
func accountStatusFromAnthropicBody(statusCode int, body []byte) sdk.AccountStatus {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = string(body)
	}
	return accountStatusFromMessage(statusCode, msg)
}

// anthropicErrorType 根据 HTTP 状态码返回 Anthropic 错误类型
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

// writeAnthropicErrorJSON 纯 sjson 构建并写入 Anthropic 格式错误响应
func writeAnthropicErrorJSON(w http.ResponseWriter, statusCode int, errType, message string) {
	out := `{"type":"error","error":{"type":"","message":""}}`
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(out))
}

// extractRetryAfterHeader 从响应头提取 Retry-After
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := headers.Get("Retry-After")
	if val == "" {
		return 0
	}
	return parseRetryDelay("try again in " + val + "s")
}

// truncate 截断字符串到指定长度
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
