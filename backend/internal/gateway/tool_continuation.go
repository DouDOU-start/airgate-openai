package gateway

import "strings"

type toolContinuationSignals struct {
	hasToolOutput      bool
	hasToolCallContext bool
}

func analyzeToolContinuationSignalsFromMap(reqData map[string]any) toolContinuationSignals {
	var signals toolContinuationSignals
	if reqData == nil {
		return signals
	}
	input, ok := reqData["input"].([]any)
	if !ok {
		return signals
	}
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := itemMap["type"].(string)
		switch {
		case isOpenAIToolOutputItemType(itemType):
			signals.hasToolOutput = true
		case isOpenAIToolCallContextItemType(itemType):
			if strings.TrimSpace(jsonString(itemMap["call_id"])) != "" {
				signals.hasToolCallContext = true
			}
		}
	}
	return signals
}

func isOpenAIToolCallContextItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "tool_call", "function_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

func isOpenAIToolOutputItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call_output", "tool_search_output", "custom_tool_call_output", "mcp_tool_call_output":
		return true
	default:
		return false
	}
}

func requestNeedsPreviousResponseID(reqData map[string]any) bool {
	signals := analyzeToolContinuationSignalsFromMap(reqData)
	return signals.hasToolOutput && !signals.hasToolCallContext
}
