package gateway

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyResponsesFailureContextWindow(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.AnthropicErrorType != "invalid_request_error" {
		t.Fatalf("unexpected anthropic error type %q", failure.AnthropicErrorType)
	}
}

func TestClassifyResponsesFailureContinuationAnchor(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if !failure.isContinuationAnchorError() {
		t.Fatalf("expected continuation anchor error")
	}
}

func TestAccountStatusFromMessageTreatsUsageLimit403AsRateLimited(t *testing.T) {
	status := accountStatusFromMessage(403, "The usage limit has been reached. Please try again later.")
	if status != "rate_limited" {
		t.Fatalf("expected rate_limited, got %q", status)
	}
}

func TestAccountStatusFromMessageKeepsDisabled403AsDisabled(t *testing.T) {
	status := accountStatusFromMessage(403, "Organization disabled due to policy violation")
	if status != "disabled" {
		t.Fatalf("expected disabled, got %q", status)
	}
}

func TestAccountStatusFromAnthropicBodyTreatsUsageLimit403AsRateLimited(t *testing.T) {
	body := []byte(`{"error":{"message":"The usage limit has been reached. Try again later."}}`)
	status := accountStatusFromAnthropicBody(403, body)
	if status != "rate_limited" {
		t.Fatalf("expected rate_limited, got %q", status)
	}
}

func TestClassifyWSErrorEventUsageLimitReached(t *testing.T) {
	// ChatGPT OAuth 触发 usage limit 时走 WS error 事件，带 resets_in_seconds。
	raw := []byte(`{"type":"error","error":{"type":"usage_limit_reached","code":"rate_limit_exceeded","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited kind, got %q", failure.Kind)
	}
	if failure.AccountStatus != "rate_limited" {
		t.Fatalf("expected AccountStatus=rate_limited, got %q", failure.AccountStatus)
	}
	if failure.RetryAfter < 59*time.Minute || failure.RetryAfter > 61*time.Minute {
		t.Fatalf("expected RetryAfter~=1h from resets_in_seconds, got %s", failure.RetryAfter)
	}
}

func TestClassifyResponsesFailureResetsAtAbsolute(t *testing.T) {
	// resets_at 是 Unix 时间戳（绝对时间），RetryAfter 应该反推出大致等于
	// future - now；这里留充分的断言窗口避免时钟抖动。
	future := time.Now().Add(2 * time.Hour).Unix()
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + formatInt(future) + `}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil || failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited failure, got %+v", failure)
	}
	if failure.RetryAfter < time.Hour+30*time.Minute || failure.RetryAfter > 2*time.Hour+5*time.Minute {
		t.Fatalf("expected RetryAfter~=2h, got %s", failure.RetryAfter)
	}
}

func formatInt(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
