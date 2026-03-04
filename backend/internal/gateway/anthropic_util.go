package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ──────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────

// generateMessageID 生成 Anthropic 消息 ID（msg_ 前缀）
func generateMessageID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "msg_unknown"
	}
	return "msg_" + hex.EncodeToString(b)
}

// parseDataURL 解析 data:mime;base64,xxx 格式的 URL
func parseDataURL(rawURL string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(rawURL, "data:") {
		return "", "", false
	}
	rest := rawURL[5:]
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return "", "", false
	}
	mediaType = rest[:semicolonIdx]
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return "", "", false
	}
	data = rest[7:]
	return mediaType, data, true
}

// safeJSONRawMessage 安全解析 JSON 字符串，invalid JSON 返回 {}
func safeJSONRawMessage(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage("{}")
}

// thinkingBudgetToReasoningEffort 将 thinking budget_tokens 映射为 reasoning_effort
func thinkingBudgetToReasoningEffort(budget int64) string {
	switch {
	case budget <= 5000:
		return "low"
	case budget <= 15000:
		return "medium"
	case budget <= 30000:
		return "high"
	default:
		return ""
	}
}

// convertFinishReasonToAnthropic 将 OpenAI finish_reason 转为 Anthropic stop_reason
func convertFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return reason
	}
}

// ptrStr 返回字符串指针
func ptrStr(s string) *string { return &s }

// ptrInt64 返回 int64 指针
func ptrInt64(i int64) *int64 { return &i }

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

// writeAnthropicErrorResponse 写入 Anthropic 格式的错误响应到 http.ResponseWriter
func writeAnthropicErrorResponse(w http.ResponseWriter, statusCode int, errType, message string) {
	resp := AnthropicErrorResponse{
		Type: "error",
		Error: AnthropicErrorDetail{
			Type:    errType,
			Message: message,
		},
	}
	data, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(data)
}
