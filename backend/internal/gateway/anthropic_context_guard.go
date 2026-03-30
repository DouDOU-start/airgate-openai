package gateway

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const anthropicContextGuardMaxTailMessages = 12

// applyAnthropicFullReplayGuard 裁剪 Anthropic full replay 历史，避免长会话持续膨胀。
func applyAnthropicFullReplayGuard(body []byte) ([]byte, bool) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, false
	}

	items := messages.Array()
	if len(items) <= anthropicContextGuardMaxTailMessages {
		return body, false
	}

	start := len(items) - anthropicContextGuardMaxTailMessages
	start = expandAnthropicTrimBoundary(items, start)
	if start <= 0 {
		return body, false
	}

	trimmed := make([]json.RawMessage, 0, len(items)-start)
	for _, item := range items[start:] {
		trimmed = append(trimmed, json.RawMessage(item.Raw))
	}
	encoded, err := json.Marshal(trimmed)
	if err != nil {
		return body, false
	}

	next, err := sjson.SetRawBytes(body, "messages", encoded)
	if err != nil {
		return body, false
	}
	return next, true
}

func expandAnthropicTrimBoundary(messages []gjson.Result, start int) int {
	for start > 0 {
		if !anthropicMessageContainsToolBoundary(messages[start]) && !anthropicMessageContainsToolBoundary(messages[start-1]) {
			break
		}
		start--
	}
	return start
}

func anthropicMessageContainsToolBoundary(message gjson.Result) bool {
	content := message.Get("content")
	if !content.IsArray() {
		return false
	}
	for _, item := range content.Array() {
		switch item.Get("type").String() {
		case "tool_use", "tool_result":
			return true
		}
	}
	return false
}
