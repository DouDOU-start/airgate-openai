package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// fakeResponseWriter 用于单测：捕获写回的 SSE / JSON 响应体。
type fakeResponseWriter struct {
	headers http.Header
	status  int
	buf     bytes.Buffer
}

func newFakeWriter() *fakeResponseWriter {
	return &fakeResponseWriter{headers: http.Header{}}
}

func (f *fakeResponseWriter) Header() http.Header { return f.headers }
func (f *fakeResponseWriter) WriteHeader(code int) {
	if f.status == 0 {
		f.status = code
	}
}
func (f *fakeResponseWriter) Write(b []byte) (int, error) { return f.buf.Write(b) }

// 取得 SSE 流中每条 data: 行的内容（不含 [DONE] 与空行）。
func extractSSEDataLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		out = append(out, payload)
	}
	return out
}

func TestChatCompletionsStreamWriter_TextOnly(t *testing.T) {
	w := newFakeWriter()
	writer := newChatCompletionsStreamWriter(w, "gpt-5.4", 0, "", false, time.Now())

	// 模拟 Codex 上游 SSE 事件序列
	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc","model":"gpt-5.4"}}`))
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"你"}`))
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"好"}`))
	writer.OnRawEvent("response.completed", []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}`))
	writer.finalize()

	lines := extractSSEDataLines(w.buf.String())
	if len(lines) == 0 {
		t.Fatalf("expected chunks, got empty stream: %q", w.buf.String())
	}
	// 最后一行应当是 [DONE]
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("last SSE line should be [DONE], got %q", lines[len(lines)-1])
	}

	// 解析中间的 chunks
	var got []map[string]any
	for _, l := range lines[:len(lines)-1] {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("chunk not valid JSON: %v\n%s", err, l)
		}
		got = append(got, m)
	}

	if len(got) < 4 {
		t.Fatalf("expected at least 4 chunks (role + 2 deltas + finish), got %d", len(got))
	}

	// 第一条应当是 role=assistant
	firstChoice := got[0]["choices"].([]any)[0].(map[string]any)
	firstDelta := firstChoice["delta"].(map[string]any)
	if role, _ := firstDelta["role"].(string); role != "assistant" {
		t.Errorf("first chunk should have role=assistant, got %v", firstDelta)
	}

	// 聚合 content
	var reconstructed strings.Builder
	for _, chunk := range got {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			reconstructed.WriteString(content)
		}
	}
	if reconstructed.String() != "你好" {
		t.Errorf("reconstructed content = %q, want %q", reconstructed.String(), "你好")
	}

	// 最后一个 chunk 应有 finish_reason=stop
	lastChoice := got[len(got)-1]["choices"].([]any)[0].(map[string]any)
	if fr, _ := lastChoice["finish_reason"].(string); fr != "stop" {
		t.Errorf("last chunk finish_reason = %v, want stop", lastChoice["finish_reason"])
	}

	// 每个 chunk 都应当带 object=chat.completion.chunk
	for i, chunk := range got {
		if obj, _ := chunk["object"].(string); obj != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %v, want chat.completion.chunk", i, chunk["object"])
		}
	}
}

func TestChatCompletionsStreamWriter_ToolCall(t *testing.T) {
	w := newFakeWriter()
	writer := newChatCompletionsStreamWriter(w, "gpt-5.4", 0, "", false, time.Now())

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_xyz","model":"gpt-5.4"}}`))
	writer.OnRawEvent("response.output_item.added", []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`))
	writer.OnRawEvent("response.function_call_arguments.delta", []byte(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\""}`))
	writer.OnRawEvent("response.function_call_arguments.delta", []byte(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":":\"北京\"}"}`))
	writer.OnRawEvent("response.completed", []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":6}}}`))
	writer.finalize()

	lines := extractSSEDataLines(w.buf.String())
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("last line should be [DONE], got %q", lines[len(lines)-1])
	}

	var got []map[string]any
	for _, l := range lines[:len(lines)-1] {
		var m map[string]any
		_ = json.Unmarshal([]byte(l), &m)
		got = append(got, m)
	}

	// 应该有 role chunk + tool_call added chunk + 两段 args delta + finish chunk
	var argBuf strings.Builder
	sawToolCallName := false
	var lastFinishReason string
	for _, chunk := range got {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			tc := tcs[0].(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if name, _ := fn["name"].(string); name == "get_weather" {
				sawToolCallName = true
			}
			if args, _ := fn["arguments"].(string); args != "" {
				argBuf.WriteString(args)
			}
		}
		if fr, _ := choice["finish_reason"].(string); fr != "" {
			lastFinishReason = fr
		}
	}
	if !sawToolCallName {
		t.Errorf("tool call name not emitted")
	}
	if argBuf.String() != `{"city":"北京"}` {
		t.Errorf("tool call arguments = %q, want %q", argBuf.String(), `{"city":"北京"}`)
	}
	if lastFinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", lastFinishReason)
	}
}

func TestBuildNonStreamChatCompletion_Text(t *testing.T) {
	result := WSResult{
		ResponseID:        "resp_1",
		Text:              "你好",
		InputTokens:       5,
		OutputTokens:      2,
		CachedInputTokens: 3,
	}
	body := buildNonStreamChatCompletion(result, "gpt-5.4")

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not valid JSON: %v\n%s", err, body)
	}

	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["model"] != "gpt-5.4" {
		t.Errorf("model = %v, want gpt-5.4", got["model"])
	}
	// id 必须带 chatcmpl- 前缀（不是上游的 resp_...），否则严格 SDK 会报错
	if id, _ := got["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %v, want chatcmpl- prefix (never use upstream resp_)", id)
	}

	choices := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("message.role = %v, want assistant", msg["role"])
	}
	if msg["content"] != "你好" {
		t.Errorf("message.content = %v, want 你好", msg["content"])
	}

	usage := got["usage"].(map[string]any)
	// prompt_tokens = input + cached (因为 WSResult 已经把 cached 从 input 里扣掉了)
	if pt := usage["prompt_tokens"].(float64); pt != 8 {
		t.Errorf("prompt_tokens = %v, want 8", pt)
	}
	if ct := usage["completion_tokens"].(float64); ct != 2 {
		t.Errorf("completion_tokens = %v, want 2", ct)
	}
	if tt := usage["total_tokens"].(float64); tt != 10 {
		t.Errorf("total_tokens = %v, want 10", tt)
	}
}

func TestBuildNonStreamChatCompletion_ToolCall(t *testing.T) {
	name := "get_weather"
	result := WSResult{
		ResponseID: "resp_2",
		Text:       "",
		ToolUses: []ToolUseBlock{{
			Type:  "tool_use",
			ID:    "call_1",
			Name:  &name,
			Input: []byte(`{"city":"北京"}`),
		}},
		InputTokens:  10,
		OutputTokens: 6,
	}
	body := buildNonStreamChatCompletion(result, "gpt-5.4")

	var got map[string]any
	_ = json.Unmarshal(body, &got)

	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != nil {
		t.Errorf("message.content = %v, want nil", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call.id = %v, want call_1", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v, want get_weather", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("function.arguments = %v, want {\"city\":\"北京\"}", fn["arguments"])
	}
}

// TestBuildNonStreamResponses 验证 /v1/responses 非流式响应从 CompletedEventRaw 抽出 response 字段。
func TestBuildNonStreamResponses_CompletedEvent(t *testing.T) {
	completedEvent := []byte(`{"type":"response.completed","response":{"id":"resp_abc","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}},"sequence_number":9}`)
	result := WSResult{CompletedEventRaw: completedEvent}

	body := buildNonStreamResponses(result)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if got["id"] != "resp_abc" {
		t.Errorf("id = %v, want resp_abc", got["id"])
	}
	if got["object"] != "response" {
		t.Errorf("object = %v, want response", got["object"])
	}
	if got["status"] != "completed" {
		t.Errorf("status = %v, want completed", got["status"])
	}
	if got["output"] == nil {
		t.Errorf("output 不应为空")
	}
	// 不应该包含 SSE 事件级字段（type、sequence_number）——只抽 response 字段
	if got["type"] == "response.completed" {
		t.Errorf("top-level 不应出现 SSE 事件字段 type=response.completed")
	}
	if _, ok := got["sequence_number"]; ok {
		t.Errorf("top-level 不应出现 SSE 事件字段 sequence_number")
	}
}

// 上游没给 response.completed 时用兜底占位
func TestBuildNonStreamResponses_Fallback(t *testing.T) {
	result := WSResult{ResponseID: "resp_xyz", Model: "gpt-5.4"}
	body := buildNonStreamResponses(result)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if got["object"] != "response" {
		t.Errorf("object = %v, want response", got["object"])
	}
	if got["id"] != "resp_xyz" {
		t.Errorf("id = %v, want resp_xyz", got["id"])
	}
	if got["status"] != "incomplete" {
		t.Errorf("status = %v, want incomplete", got["status"])
	}
}

func TestIsChatCompletionsRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    []byte
		want    bool
	}{
		{
			name:    "forwarded path chat completions",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions"}},
			want:    true,
		},
		{
			name:    "forwarded path responses",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/responses"}},
			want:    false,
		},
		{
			name:    "no header, messages only body",
			headers: http.Header{},
			body:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			want:    true,
		},
		{
			name:    "no header, input-style body",
			headers: http.Header{},
			body:    []byte(`{"input":"hi"}`),
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{Headers: tc.headers, Body: tc.body}
			if got := isChatCompletionsRequest(req); got != tc.want {
				t.Errorf("isChatCompletionsRequest = %v, want %v", got, tc.want)
			}
		})
	}
}
