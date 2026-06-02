package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
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

func TestClassifyResponsesFailureSafetyRejected(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID 916c6516-5f37-9121-b05a-a604888c0055."}}}`)
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
	if failure.Code != "safety_rejected" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
}

// TestIsSafetyRejectionTextNearMisses 守住关键词收紧后的边界：
// 提示性文案、把 policy 当名词解释的 400 不应被误判成"安全拒绝"。
func TestIsSafetyRejectionTextNearMisses(t *testing.T) {
	negatives := []string{
		"please ensure your prompt follows our safety policy guidelines",
		"see the safety policy for details",
		"input violates the company policy",
		"this field requires the safety token",
		"",
	}
	for _, msg := range negatives {
		if isSafetyRejectionText(msg) {
			t.Fatalf("did not expect safety match for %q", msg)
		}
	}

	positives := []string{
		"Your request was rejected by the safety system",
		"content_policy_violation",
		"blocked by policy",
		"prompt was blocked by our safety filter",
		"moderation_blocked",
	}
	for _, msg := range positives {
		if !isSafetyRejectionText(msg) {
			t.Fatalf("expected safety match for %q", msg)
		}
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

func TestClassifyHTTPFailureTreatsUsageLimit400AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(400, "The usage limit has been reached. Please try again later.")
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

func TestClassifyHTTPFailureTreatsDisabled400AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(400, "Organization disabled due to policy violation")
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

func TestClassifyGenericSSEErrorEventTopLevelModelNotFound(t *testing.T) {
	raw := []byte(`{"message":"The model gpt-5.3-codex-spark does not exist.","type":"invalid_request_error","code":"model_not_found"}`)
	failure := classifyGenericSSEErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("expected client kind, got %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d", failure.StatusCode)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeClientError {
		t.Fatalf("expected OutcomeClientError, got %v", kind)
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

func TestHandleStreamResponseTreatsCompletedEmptyStreamAsFailure(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err == nil {
		t.Fatalf("expected empty stream error")
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	if !strings.Contains(outcome.Reason, "上游流式响应为空") {
		t.Fatalf("unexpected reason %q", outcome.Reason)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("empty stream should not be forwarded before validation, got %q", w.Body.String())
	}
}

func TestHandleStreamResponseFlushesBufferedPreludeWhenOutputArrives(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, `"content":"ok"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("buffered stream was not forwarded completely: %q", got)
	}
}

func TestHandleStreamResponseTreatsResponsesImageContentAsOutput(t *testing.T) {
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","output":[]}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","content":[]}}`,
		"",
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":""}`,
		"",
		`event: response.content_part.done`,
		`data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","status":"completed","usage":{"input_tokens":10,"output_tokens":2},"output":[{"id":"rs_1","type":"reasoning","encrypted_content":"secret"},{"id":"msg_1","type":"message","content":[{"type":"output_image","image_url":"data:image/png;base64,AAA"}]}]}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"output_image"`) || !strings.Contains(got, "data:image/png;base64,AAA") {
		t.Fatalf("image content stream was not forwarded completely: %q", got)
	}
}

func TestHandleStreamResponseNormalizesImageGenerationCallStatus(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"generating","result":"aGVsbG8=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","status":"completed","usage":{"input_tokens":10,"output_tokens":2},"output":[{"id":"ig_1","type":"image_generation_call","status":"generating","result":"aGVsbG8=","size":"1024x1024"}]}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if strings.Contains(got, `"status":"generating"`) {
		t.Fatalf("image_generation_call status should be normalized: %q", got)
	}
	if !strings.Contains(got, `"item":{"id":"ig_1","type":"image_generation_call","status":"completed"`) {
		t.Fatalf("output_item.done should be normalized: %q", got)
	}
	if !strings.Contains(got, `"output":[{"id":"ig_1","type":"image_generation_call","status":"completed"`) {
		t.Fatalf("response.completed output should be normalized: %q", got)
	}
}

func TestHandleStreamResponseTreatsPartialImageB64AsOutput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_1","output_index":0,"partial_image_b64":"aGVsbG8=","partial_image_index":0}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("expected OutcomeStreamAborted, got %v", outcome.Kind)
	}
	if err == nil {
		t.Fatalf("expected missing completion event error")
	}
	if !strings.Contains(w.Body.String(), `"partial_image_b64":"aGVsbG8="`) {
		t.Fatalf("partial image event should be forwarded: %q", w.Body.String())
	}
}

func TestHandleStreamResponseRecordsDeliveredImagesWhenStreamAborts(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aGVsbG8=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.output_item.done","item":{"id":"ig_2","type":"image_generation_call","status":"completed","result":"d29ybGQ=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.output_item.done","item":{"id":"ig_3","type":"image_generation_call","status":"completed","result":"YWlyZ2F0ZQ==","size":"1024x1024"}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("expected OutcomeStreamAborted, got %v", outcome.Kind)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage = nil, want delivered image usage")
	}
	if got := usageMetricInt(outcome.Usage, usageMetricImages); got != 3 {
		t.Fatalf("images metric = %d, want 3", got)
	}
	if got := usageCostMetadata(outcome.Usage, usageCostImageTool, "image_count"); got != "3" {
		t.Fatalf("image_count metadata = %q, want 3", got)
	}
	if got := usageCostMetadata(outcome.Usage, usageCostImageTool, "size"); got != "1024x1024" {
		t.Fatalf("size metadata = %q, want 1024x1024", got)
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
