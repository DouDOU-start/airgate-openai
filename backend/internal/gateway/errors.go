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

// accountStatusFromAnthropicBody 从 Anthropic 错误响应体推断账号状态。
// Anthropic 某些账号级错误（如组织被封禁）走 400，accountStatusFromCode 无法识别，
// 需额外检查 error.message 内容。
func accountStatusFromAnthropicBody(statusCode int, body []byte) sdk.AccountStatus {
	base := accountStatusFromCode(statusCode)
	if base != sdk.AccountStatusOK || statusCode != 400 {
		return base
	}
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "disabled") || strings.Contains(msg, "deactivated") || strings.Contains(msg, "suspended") {
		return sdk.AccountStatusDisabled
	}
	return base
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
