package gateway

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const contextGuardMaxTailMessages = 24

// applyContextGuard 保留前导控制消息，并裁剪过长的历史尾部。
func applyContextGuard(body []byte, reqPath string) []byte {
	if !strings.Contains(reqPath, "/v1/chat/completions") {
		return body
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body
	}

	selected := selectContextGuardMessages(messages.Array())
	if len(selected) == 0 || len(selected) == len(messages.Array()) {
		return body
	}

	rawMessages := make([]json.RawMessage, 0, len(selected))
	for _, msg := range selected {
		rawMessages = append(rawMessages, json.RawMessage(msg.Raw))
	}

	encoded, err := json.Marshal(rawMessages)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "messages", encoded)
	if err != nil {
		return body
	}
	return updated
}

func selectContextGuardMessages(messages []gjson.Result) []gjson.Result {
	if len(messages) <= contextGuardMaxTailMessages+2 {
		return messages
	}

	headCount := 0
	for headCount < len(messages) && headCount < 2 {
		role := strings.ToLower(strings.TrimSpace(messages[headCount].Get("role").String()))
		if role != "system" && role != "developer" {
			break
		}
		headCount++
	}

	remaining := messages[headCount:]
	if len(remaining) <= contextGuardMaxTailMessages {
		return messages
	}

	tailStart := len(remaining) - contextGuardMaxTailMessages
	selected := append([]gjson.Result{}, messages[:headCount]...)
	selected = append(selected, remaining[tailStart:]...)
	return selected
}
