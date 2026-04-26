package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
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

func TestClassifyHTTPFailureTreatsUsageLimit403AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(403, "The usage limit has been reached. Please try again later.")
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureKeepsDisabled403AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(403, "Organization disabled due to policy violation")
	if got != sdk.OutcomeAccountDead {
		t.Fatalf("expected AccountDead, got %v", got)
	}
}

func TestClassifyAnthropicBodyTreatsUsageLimit403AsRateLimited(t *testing.T) {
	body := []byte(`{"error":{"message":"The usage limit has been reached. Try again later."}}`)
	got := classifyAnthropicBody(403, body)
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
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
	if kind := failure.outcomeKind(); kind != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected OutcomeAccountRateLimited, got %v", kind)
	}
	if failure.RetryAfter < 59*time.Minute || failure.RetryAfter > 61*time.Minute {
		t.Fatalf("expected RetryAfter~=1h from resets_in_seconds, got %s", failure.RetryAfter)
	}
}

func TestClassifyWSErrorEventOpenAICompatSSEError(t *testing.T) {
	raw := []byte(`{"error":{"message":"An error occurred while processing your request. Please include the request ID 349f8894 in your message.","type":"server_error","code":"upstream_error"}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindServer {
		t.Fatalf("expected server kind, got %q", failure.Kind)
	}
	if failure.Message != "An error occurred while processing your request. Please include the request ID 349f8894 in your message." {
		t.Fatalf("unexpected message %q", failure.Message)
	}
}

func TestHandleStreamResponseSanitizesFirstSSEError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"upstream secret request ID 349f8894\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n")),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	body := w.Body.String()
	if strings.Contains(body, "upstream secret") || strings.Contains(body, "349f8894") {
		t.Fatalf("response leaked upstream error: %q", body)
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
