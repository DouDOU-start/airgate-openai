package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// 工具函数（纯函数，不依赖任何 struct）
// ──────────────────────────────────────────────────────

// generateMessageID 生成 Anthropic 消息 ID（msg_ 前缀）
func generateMessageID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "msg_unknown"
	}
	return "msg_" + hex.EncodeToString(b)
}

// parseDataURL 解析 data:mime;base64,xxx 格式的 URL
func parseDataURL(rawURL string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(rawURL, "data:") {
		return "", "", false
	}
	rest := rawURL[5:]
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return "", "", false
	}
	mediaType = rest[:semicolonIdx]
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return "", "", false
	}
	data = rest[7:]
	return mediaType, data, true
}

// thinkingBudgetToReasoningEffort 将 thinking budget_tokens 映射为 reasoning_effort（与 CLIProxyAPI ConvertBudgetToLevel 对齐）
func thinkingBudgetToReasoningEffort(budget int64) string {
	switch {
	case budget < -1:
		return ""
	case budget == -1:
		return "auto"
	case budget == 0:
		return "none"
	case budget <= 512:
		return "low"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

// convertFinishReasonToAnthropic 将 OpenAI finish_reason 转为 Anthropic stop_reason
func convertFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return reason
	}
}

// anthropicErrorType 根据 HTTP 状态码返回 Anthropic 错误类型
func anthropicErrorType(statusCode int) string {
	switch statusCode {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 422:
		return "invalid_model_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicErrorJSON 纯 sjson 构建并写入 Anthropic 格式错误响应
func writeAnthropicErrorJSON(w http.ResponseWriter, statusCode int, errType, message string) {
	out := `{"type":"error","error":{"type":"","message":""}}`
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(out))
}

// ──────────────────────────────────────────────────────
// 工具名缩短
// ──────────────────────────────────────────────────────

// shortenNameIfNeeded 按与 CLIProxyAPI 一致的规则缩短工具名
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

// buildShortNameMap 保证同一请求内缩短名唯一
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// buildToolShortNameMapFromJSON 基于原始 JSON body 中的 tools 数组构建 original->short 映射
func buildToolShortNameMapFromJSON(body []byte) map[string]string {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return map[string]string{}
	}
	var names []string
	for _, t := range tools.Array() {
		if n := t.Get("name").String(); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return map[string]string{}
	}
	return buildShortNameMap(names)
}

// buildReverseMapFromAnthropicOriginalToShort 基于原始 Anthropic tools 构建 original->short 映射
func buildReverseMapFromAnthropicOriginalToShort(original []byte) map[string]string {
	return buildToolShortNameMapFromJSON(original)
}

// buildReverseMapFromAnthropicOriginalShortToOriginal 基于原始 Anthropic tools 构建 short->original 映射
func buildReverseMapFromAnthropicOriginalShortToOriginal(original []byte) map[string]string {
	rev := map[string]string{}
	m := buildReverseMapFromAnthropicOriginalToShort(original)
	for orig, short := range m {
		rev[short] = orig
	}
	return rev
}

// hasWebSearchTool 检查 Anthropic 请求 JSON 中是否包含 web_search 原生工具
func hasWebSearchTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, t := range tools.Array() {
		switch t.Get("type").String() {
		case "web_search_20250305", "web_search":
			return true
		}
	}
	return false
}

// normalizeToolParametersJSON 确保 object schema 至少包含空 properties（参考 CLIProxyAPI normalizeToolParameters）
func normalizeToolParametersJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	schema := raw
	result := gjson.Parse(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.Set(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRaw(schema, "properties", `{}`)
	}
	return schema
}

// extractResponsesUsage 从 Responses usage 中提取 token 统计（cached_tokens 从 input_tokens 扣除）
func extractResponsesUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}
	return inputTokens, outputTokens, cachedTokens
}

// ──────────────────────────────────────────────────────
// 动态推理 effort 推断（adaptive thinking 优化）
// ──────────────────────────────────────────────────────

// isToolTurnMatching 通用检测：最后一轮是否为工具结果处理轮次，且所有工具都满足 predicate
// 条件：最后一条 user 消息全部是 tool_result，且前一条 assistant 的 tool_use 全部满足 predicate
func isToolTurnMatching(rawJSON []byte, toolMatcher func(string) bool) bool {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.IsArray() {
		return false
	}
	arr := messages.Array()
	if len(arr) < 2 {
		return false
	}

	// 最后一条消息必须是 user 且全部是 tool_result
	lastMsg := arr[len(arr)-1]
	if lastMsg.Get("role").String() != "user" {
		return false
	}
	lastContent := lastMsg.Get("content")
	if !lastContent.IsArray() {
		return false
	}
	for _, block := range lastContent.Array() {
		if block.Get("type").String() != "tool_result" {
			return false
		}
	}

	// 查找前一条 assistant 消息，检查 tool_use 名称
	var prevAssistant gjson.Result
	for i := len(arr) - 2; i >= 0; i-- {
		if arr[i].Get("role").String() == "assistant" {
			prevAssistant = arr[i]
			break
		}
	}
	if !prevAssistant.Exists() {
		return false
	}

	hasToolUse := false
	allMatch := true
	prevContent := prevAssistant.Get("content")
	if prevContent.IsArray() {
		for _, block := range prevContent.Array() {
			if block.Get("type").String() == "tool_use" {
				hasToolUse = true
				if !toolMatcher(block.Get("name").String()) {
					allMatch = false
					break
				}
			}
		}
	}

	return hasToolUse && allMatch
}

// isReadOnlyToolTurn 检测当前请求是否为只读工具结果处理轮次（用于 adaptive effort 降级）
func isReadOnlyToolTurn(rawJSON []byte) bool {
	return isToolTurnMatching(rawJSON, isReadOnlyTool)
}

// isSparkEligibleToolTurn 检测当前请求是否适合路由到 Spark 模型
// 比 isReadOnlyToolTurn 更严格：仅 Grep/Glob/Search/Find 等搜索类工具
// Read/Fetch 返回完整内容可能需要深度分析，不适合 Spark
func isSparkEligibleToolTurn(rawJSON []byte) bool {
	return isToolTurnMatching(rawJSON, isSparkEligibleTool)
}

// inferAdaptiveEffort 基于最后一轮 tool 类型动态推断 reasoning effort
// 当最近一轮是只读工具的结果处理时，降低 effort 以加快响应
// 返回 "" 表示无法推断，调用者应使用模型映射默认值
func inferAdaptiveEffort(rawJSON []byte, modelDefault string) string {
	if !isReadOnlyToolTurn(rawJSON) {
		return ""
	}
	return lowerEffort(modelDefault)
}

// isReadOnlyTool 判断工具是否为只读/信息收集类（含 Read/Fetch，用于 effort 降级）
func isReadOnlyTool(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range []string{
		"read", "grep", "glob", "search", "fetch",
		"list", "find", "taskoutput", "taskget", "tasklist",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isSparkEligibleTool 判断工具是否适合 Spark 快速模型处理
// 仅搜索/索引类工具（结果是路径列表或匹配行，决策简单）
// Read/Fetch 返回完整内容需要深度分析，不走 Spark
func isSparkEligibleTool(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range []string{
		"grep", "glob", "search", "find", "list",
		"taskoutput", "taskget", "tasklist",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// lowerEffort 将 effort 降低一级
func lowerEffort(effort string) string {
	switch effort {
	case "xhigh":
		return "high"
	case "high":
		return "medium"
	case "medium":
		return "low"
	default:
		return effort
	}
}

// injectWebSearchToolJSON 向 Responses API JSON 请求体注入 web_search 工具
func injectWebSearchToolJSON(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, t := range tools.Array() {
			if t.Get("type").String() == "web_search" {
				return body // 已存在
			}
		}
	}
	result, err := sjson.SetRawBytes(body, "tools.-1", []byte(`{"type":"web_search"}`))
	if err != nil {
		return body
	}
	return result
}

