package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
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
	Message            string
	RetryAfter         time.Duration
}

// outcomeKind 把内部 responsesFailureKind 映射到 SDK 的 OutcomeKind。
// continuationAnchor 本质是客户端传了失效的 previous_response_id，归到 ClientError。
// Unknown 兜底走 UpstreamTransient —— Core 能尝试 failover，而不是把账号标死。
func (e *responsesFailureError) outcomeKind() sdk.OutcomeKind {
	if e == nil {
		return sdk.OutcomeUnknown
	}
	switch e.Kind {
	case responsesFailureKindClient, responsesFailureKindContinuationAnchor:
		return sdk.OutcomeClientError
	case responsesFailureKindRateLimited:
		return sdk.OutcomeAccountRateLimited
	case responsesFailureKindServer:
		return sdk.OutcomeUpstreamTransient
	default:
		return sdk.OutcomeUpstreamTransient
	}
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

	failure := classifyResponsesError(errType, errCode, msg)
	applyOpenAIRateLimitReset(failure, errNode)
	return failure
}

// classifyWSErrorEvent 处理 WebSocket "error" 事件（区别于 "response.failed"）。
// 上游有些校验失败（如 model 不被支持、字段无效）走的是这条事件，错误对象通常长这样：
//
//	{"type":"error","error":{"message":"...","type":"invalid_request_error","code":"..."}}
//
// 与 response.failed 共用同一套关键词分类，确保客户端侧的错误（unsupported model
// 等）被识别成 Kind=Client，避免归罪到账号。
func classifyWSErrorEvent(data []byte) *responsesFailureError {
	errNode := gjson.GetBytes(data, "error")
	if !errNode.Exists() {
		return nil
	}
	if eventType := gjson.GetBytes(data, "type").String(); eventType != "" && eventType != "error" {
		return nil
	}
	msg := strings.TrimSpace(errNode.Get("message").String())
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	errType := strings.ToLower(strings.TrimSpace(errNode.Get("type").String()))
	errCode := strings.ToLower(strings.TrimSpace(errNode.Get("code").String()))
	failure := classifyResponsesError(errType, errCode, msg)
	applyOpenAIRateLimitReset(failure, errNode)
	return failure
}

// applyOpenAIRateLimitReset 把 OpenAI OAuth 错误体里的 resets_at / resets_in_seconds
// 转换成精确的 RetryAfter，覆盖 parseRetryDelay 的文本估算。
// 只在失败被归类为限流（Kind=RateLimited）时起作用。
//
// OpenAI usage_limit_reached / rate_limit_exceeded 错误体示例：
//
//	{"error": {
//	    "type": "usage_limit_reached",
//	    "message": "The usage limit has been reached",
//	    "resets_at": 1769404154,
//	    "resets_in_seconds": 133107}}
func applyOpenAIRateLimitReset(failure *responsesFailureError, errNode gjson.Result) {
	if failure == nil || failure.Kind != responsesFailureKindRateLimited {
		return
	}
	if secs := errNode.Get("resets_in_seconds"); secs.Exists() {
		if v := secs.Int(); v > 0 {
			failure.RetryAfter = time.Duration(v) * time.Second
			return
		}
	}
	if resetsAt := errNode.Get("resets_at"); resetsAt.Exists() {
		if ts := resetsAt.Int(); ts > 0 {
			until := time.Until(time.Unix(ts, 0))
			if until > 0 {
				failure.RetryAfter = until
			}
		}
	}
}

// classifyResponsesError 根据 type/code/message 关键词归类错误。
// 是 classifyResponsesFailure / classifyWSErrorEvent 的共用实现。
func classifyResponsesError(errType, errCode, msg string) *responsesFailureError {
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
	case containsAny(errType, errCode, msg, "invalid_prompt", "invalid_request", "input_too_long", "is not supported", "unsupported", "model_not_found", "invalid model", "invalid_model"):
		return &responsesFailureError{
			Kind:               responsesFailureKindClient,
			StatusCode:         http.StatusBadRequest,
			AnthropicErrorType: "invalid_request_error",
			Message:            msg,
		}
	case isTemporaryRateLimitText(errType, errCode, msg) || containsAny(errType, errCode, msg, "usage_limit_reached", "rate_limit_exceeded"):
		return &responsesFailureError{
			Kind:               responsesFailureKindRateLimited,
			StatusCode:         http.StatusTooManyRequests,
			AnthropicErrorType: "rate_limit_error",
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
