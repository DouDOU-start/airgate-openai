package gateway

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertAnthropicRequestToResponsesToolChoiceAnyDefaultsToAuto(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"1+1=?"}]}
		],
		"tools":[
			{"name":"update_todos","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"any"}
	}`)

	out := convertAnthropicRequestToResponses(raw, "gpt-5.4", "")
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want %q", got, "auto")
	}
}

func TestConvertAnthropicRequestToResponsesToolChoiceAnyStaysRequiredInToolLoop(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"update_todos","input":{"items":["answer math"]}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
		],
		"tools":[
			{"name":"update_todos","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"any"}
	}`)

	out := convertAnthropicRequestToResponses(raw, "gpt-5.4", "")
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "required" {
		t.Fatalf("tool_choice = %q, want %q", got, "required")
	}
}
