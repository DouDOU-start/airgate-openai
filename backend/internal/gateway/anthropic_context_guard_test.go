package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyAnthropicFullReplayGuardTrimsOldMessages(t *testing.T) {
	body := buildAnthropicGuardBody(16, -1)

	trimmed, changed := applyAnthropicFullReplayGuard([]byte(body))
	if !changed {
		t.Fatalf("expected body to be trimmed")
	}

	count := int(gjson.GetBytes(trimmed, "messages.#").Int())
	if count > anthropicContextGuardMaxTailMessages {
		t.Fatalf("messages count after trim = %d, want <= %d", count, anthropicContextGuardMaxTailMessages)
	}
	if strings.Contains(string(trimmed), "message-00") {
		t.Fatalf("expected earliest messages to be trimmed")
	}
}

func TestApplyAnthropicFullReplayGuardKeepsToolBoundaryIntact(t *testing.T) {
	body := buildAnthropicGuardBody(16, anthropicContextGuardMaxTailMessages)

	trimmed, changed := applyAnthropicFullReplayGuard([]byte(body))
	if !changed {
		t.Fatalf("expected body to be trimmed")
	}

	if !strings.Contains(string(trimmed), `"tool_use"`) {
		t.Fatalf("expected tool_use block to remain after trim")
	}
	if !strings.Contains(string(trimmed), `"tool_result"`) {
		t.Fatalf("expected tool_result block to remain after trim")
	}
}

func buildAnthropicGuardBody(messageCount int, toolBoundaryIndex int) string {
	var b strings.Builder
	b.WriteString(`{"system":[{"type":"text","text":"sys"}],"messages":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		content := fmt.Sprintf(`[{"type":"text","text":"message-%02d"}]`, i)
		if i%2 == 1 {
			role = "assistant"
		}
		if i == toolBoundaryIndex-1 {
			role = "assistant"
			content = `[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"a.txt"}}]`
		}
		if i == toolBoundaryIndex {
			role = "user"
			content = `[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]`
		}
		fmt.Fprintf(&b, `{"role":"%s","content":%s}`, role, content)
	}
	b.WriteString(`]}`)
	return b.String()
}
