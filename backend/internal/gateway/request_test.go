package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// TestIsAnthropicRequest 只认两个权威信号：X-Forwarded-Path + Anthropic-Version 头。
// body 启发式已废除（见 isAnthropicRequest 注释）。
func TestIsAnthropicRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    []byte
		want    bool
	}{
		// path 命中 Anthropic
		{
			name:    "path=/v1/messages",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages/count_tokens（子路径）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages/count_tokens"}},
			body:    []byte(`{"model":"claude","messages":[]}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages?foo=bar（带 query）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages?foo=bar"}},
			body:    nil,
			want:    true,
		},
		// 子串匹配防漏点
		{
			name:    "path=/v1/messages-custom 非 Anthropic 派生前缀",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages-custom"}},
			body:    nil,
			want:    false,
		},
		{
			name:    "query 里夹杂 /v1/messages 字样不应触发",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions?referer=/v1/messages"}},
			body:    nil,
			want:    false,
		},
		// path 命中 OpenAI
		{
			name:    "path=/v1/chat/completions",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions"}},
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "path=/v1/responses",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/responses"}},
			body:    []byte(`{"model":"gpt-5.4","input":"hi"}`),
			want:    false,
		},
		// 头部兜底
		{
			name:    "Anthropic-Version 头",
			headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		// 不再依靠 body 启发——body 有 Anthropic 风味但没 path/header 信号时，默认 OpenAI
		{
			name:    "body 有 top-level system 但无 path/header → 默认 OpenAI",
			headers: nil,
			body:    []byte(`{"model":"x","system":"You are helpful","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "OpenAI chat.completions 无 path/header（之前会被误判，回归用例）",
			headers: nil,
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
			want:    false,
		},
		{
			name:    "OpenAI vision 带 content block 数组（以前会误判）",
			headers: nil,
			body:    []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"..."}}]}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "空 body + 无 headers",
			headers: nil,
			body:    nil,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{Headers: tc.headers, Body: tc.body}
			if got := isAnthropicRequest(req); got != tc.want {
				t.Errorf("isAnthropicRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyContinuationStateDoesNotBackfillPreviousResponseID(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "" {
		t.Fatalf("expected previous_response_id to NOT be backfilled, got %q", got)
	}
}

func TestDropPreviousResponseIDFromJSON(t *testing.T) {
	next, changed := dropPreviousResponseIDFromJSON([]byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}`))
	if !changed {
		t.Fatalf("expected previous_response_id to be removed")
	}
	if string(next) == `{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}` {
		t.Fatalf("expected updated payload")
	}
}

func TestNormalizeOpenAIServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIServiceTier(fast) = %q, want empty", got)
	}
}

func TestNormalizeOpenAIWireServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIWireServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIWireServiceTier(fast) = %q, want empty", got)
	}
}

func TestEnsureResponsesDefaultsWithTier_FastIgnored(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	result := ensureResponsesDefaultsWithTier(body, "fast")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be omitted for fast, got %v", payload["service_tier"])
	}
}

func TestApplyOpenAIWireServiceTier_FastRemoved(t *testing.T) {
	result := applyOpenAIWireServiceTier([]byte(`{"model":"gpt-5.5","input":"hi","service_tier":"fast"}`))

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be removed for fast, got %v", payload["service_tier"])
	}
}

func TestFirstNonEmptyTier_RequestFastFallsBackToUpstreamPriority(t *testing.T) {
	if got := firstNonEmptyTier("fast", "priority"); got != "priority" {
		t.Fatalf("firstNonEmptyTier(fast, priority) = %q, want %q", got, "priority")
	}
}
