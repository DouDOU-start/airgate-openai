package gateway

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAnthropicStopReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty default", in: "", want: "end_turn"},
		{name: "stop to end_turn", in: "stop", want: "end_turn"},
		{name: "length to max_tokens", in: "length", want: "max_tokens"},
		{name: "tool_calls to tool_use", in: "tool_calls", want: "tool_use"},
		{name: "max_output_tokens to max_tokens", in: "max_output_tokens", want: "max_tokens"},
		{name: "content_filter to refusal", in: "content_filter", want: "refusal"},
		{name: "preserve unknown normalized", in: "  CUSTOM_REASON  ", want: "custom_reason"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAnthropicStopReason(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeAnthropicStopReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSSEStream_AggregatesReasoningFunctionToolUseAndStopReason(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think-"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Wuhan\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5","stop_reason":"tool_calls","usage":{"input_tokens":12,"output_tokens":34,"input_tokens_details":{"cached_tokens":5}}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if result.ResponseID != "resp_1" {
		t.Fatalf("ResponseID = %q, want %q", result.ResponseID, "resp_1")
	}
	if result.Model != "gpt-5" {
		t.Fatalf("Model = %q, want %q", result.Model, "gpt-5")
	}
	if result.Text != "hello" {
		t.Fatalf("Text = %q, want %q", result.Text, "hello")
	}
	if result.Reasoning != "think-step" {
		t.Fatalf("Reasoning = %q, want %q", result.Reasoning, "think-step")
	}
	if result.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, "tool_calls")
	}
	if result.InputTokens != 12 || result.OutputTokens != 34 || result.CacheTokens != 5 {
		t.Fatalf("usage = (%d,%d,%d), want (12,34,5)", result.InputTokens, result.OutputTokens, result.CacheTokens)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	if result.ToolUses[0].Type != "tool_use" || result.ToolUses[0].ID != "call_1" {
		t.Fatalf("unexpected tool_use block: %+v", result.ToolUses[0])
	}
	if result.ToolUses[0].Name == nil || *result.ToolUses[0].Name != "get_weather" {
		t.Fatalf("tool_use name = %v, want get_weather", result.ToolUses[0].Name)
	}
	if string(result.ToolUses[0].Input) != `{"city":"Wuhan"}` {
		t.Fatalf("tool_use input = %s, want %s", string(result.ToolUses[0].Input), `{"city":"Wuhan"}`)
	}
}

func TestParseSSEStream_AggregatesWebSearchToolUse(t *testing.T) {
	itemID := fmt.Sprintf("ws_%d", time.Now().UnixNano())
	query := fmt.Sprintf("weather-%d", time.Now().UnixNano())

	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		fmt.Sprintf(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","id":%q}}`, itemID),
		fmt.Sprintf(`data: {"type":"response.output_item.done","item":{"type":"web_search_call","id":%q,"action":{"query":%q}}}`, itemID, query),
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5","stop_reason":"stop","usage":{"input_tokens":2,"output_tokens":3}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	tool := result.ToolUses[0]
	if tool.Name == nil || *tool.Name != "web_search" {
		t.Fatalf("websearch tool name = %v, want web_search", tool.Name)
	}
	if tool.ID != itemID {
		t.Fatalf("websearch tool id = %q, want %q", tool.ID, itemID)
	}
	wantInput := fmt.Sprintf(`{"query":%q}`, query)
	if string(tool.Input) != wantInput {
		t.Fatalf("websearch input = %s, want %s", string(tool.Input), wantInput)
	}
}
