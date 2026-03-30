package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

type responsesFailureKind string

const (
	responsesFailureKindUnknown            responsesFailureKind = "unknown"
	responsesFailureKindClient             responsesFailureKind = "client"
	responsesFailureKindContinuationAnchor responsesFailureKind = "continuation_anchor"
	responsesFailureKindRateLimited        responsesFailureKind = "rate_limited"
	responsesFailureKindServer             responsesFailureKind = "server"
)

type responsesFailureError struct {
	Kind               responsesFailureKind
	StatusCode         int
	AnthropicErrorType string
	AccountStatus      string
	Message            string
	RetryAfter         time.Duration
}

func (e *responsesFailureError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case responsesFailureKindContinuationAnchor:
		return "上游续链锚点失效: " + e.Message
	case responsesFailureKindClient:
		return "上游请求无效: " + e.Message
	case responsesFailureKindRateLimited:
		if e.RetryAfter > 0 {
			return fmt.Sprintf("上游速率限制(建议 %s 后重试): %s", e.RetryAfter, e.Message)
		}
		return "上游速率限制: " + e.Message
	default:
		return "上游错误: " + e.Message
	}
}

func (e *responsesFailureError) shouldReturnClientError() bool {
	return e != nil && e.Kind == responsesFailureKindClient
}

func (e *responsesFailureError) isContinuationAnchorError() bool {
	return e != nil && e.Kind == responsesFailureKindContinuationAnchor
}

func classifyResponsesFailure(data []byte) *responsesFailureError {
	eventType := gjson.GetBytes(data, "type").String()
	if eventType != "response.failed" {
		return nil
	}

	errNode := gjson.GetBytes(data, "response.error")
	msg := strings.TrimSpace(errNode.Get("message").String())
	if msg == "" {
		msg = "上游返回 response.failed"
	}
	errType := strings.ToLower(strings.TrimSpace(errNode.Get("type").String()))
	errCode := strings.ToLower(strings.TrimSpace(errNode.Get("code").String()))

	switch {
	case containsAny(errType, errCode, msg, "previous_response_not_found", "previous response", "response not found"):
		return &responsesFailureError{
			Kind:               responsesFailureKindContinuationAnchor,
			StatusCode:         http.StatusConflict,
			AnthropicErrorType: "invalid_request_error",
			Message:            msg,
		}
	case containsAny(errType, errCode, msg, "context_length", "context window", "max_tokens", "max_input_tokens", "max_output_tokens", "token limit", "too many tokens"):
		return &responsesFailureError{
			Kind:               responsesFailureKindClient,
			StatusCode:         http.StatusBadRequest,
			AnthropicErrorType: "invalid_request_error",
			Message:            msg,
		}
	case containsAny(errType, errCode, msg, "invalid_prompt", "invalid_request", "input_too_long"):
		return &responsesFailureError{
			Kind:               responsesFailureKindClient,
			StatusCode:         http.StatusBadRequest,
			AnthropicErrorType: "invalid_request_error",
			Message:            msg,
		}
	case containsAny(errType, errCode, msg, "rate_limit"):
		return &responsesFailureError{
			Kind:               responsesFailureKindRateLimited,
			StatusCode:         http.StatusTooManyRequests,
			AnthropicErrorType: "rate_limit_error",
			AccountStatus:      sdk.AccountStatusRateLimited,
			Message:            msg,
			RetryAfter:         parseRetryDelay(msg),
		}
	default:
		return &responsesFailureError{
			Kind:               responsesFailureKindServer,
			StatusCode:         http.StatusBadGateway,
			AnthropicErrorType: mapResponsesErrorType(errType, errCode),
			Message:            msg,
		}
	}
}
