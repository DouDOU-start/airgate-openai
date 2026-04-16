package gateway

import (
	"net/http"
	"testing"
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
