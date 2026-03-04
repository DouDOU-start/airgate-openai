package gateway

import "strings"

// ──────────────────────────────────────────────────────
// Claude → OpenAI 模型映射
// Claude Code 发送 Claude 模型名，翻译为 OpenAI 模型 + 额外参数
// ──────────────────────────────────────────────────────

// anthropicModelMapping 单条模型映射规则
type anthropicModelMapping struct {
	// OpenAIModel 映射到的 OpenAI 模型名
	OpenAIModel string
	// ReasoningEffort 注入的 reasoning_effort 参数（空则不注入）
	ReasoningEffort string
}

// anthropicModelMappings Claude 模型名 → OpenAI 模型映射表
// 精确匹配优先，通配符匹配其次
var anthropicModelMappings = map[string]anthropicModelMapping{
	// Opus → 高推理
	"claude-opus-4-6":     {OpenAIModel: "gpt-5.3-codex", ReasoningEffort: "xhigh"},
	"claude-opus-4-5":     {OpenAIModel: "gpt-5.3-codex", ReasoningEffort: "xhigh"},

	// Sonnet → 标准
	"claude-sonnet-4-6":   {OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""},
	"claude-sonnet-4-5":   {OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""},

	// Haiku → 低推理（精确版本号）
	"claude-haiku-4-5":    {OpenAIModel: "gpt-5.3-codex", ReasoningEffort: "low"},
}

// anthropicWildcardMappings 通配符映射（前缀匹配，按优先级排序）
var anthropicWildcardMappings = []struct {
	Prefix  string
	Mapping anthropicModelMapping
}{
	// claude-haiku-4-5-* 所有变体（如 claude-haiku-4-5-20251001）
	{"claude-haiku-4-5", anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: "low"}},
	// claude-sonnet-4- 所有变体
	{"claude-sonnet-4-", anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""}},
	// claude-opus-4- 所有变体
	{"claude-opus-4-", anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: "xhigh"}},
	// claude-3.5/3 系列兜底
	{"claude-3", anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""}},
	// 兜底：所有 claude- 前缀
	{"claude-", anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""}},
}

// defaultModelMapping 兜底映射：不认识的模型统一用 gpt-5.3-codex
var defaultModelMapping = anthropicModelMapping{OpenAIModel: "gpt-5.3-codex", ReasoningEffort: ""}

// resolveAnthropicModelMapping 解析 Claude 模型名的映射
// 精确匹配 → 通配符前缀匹配 → 兜底默认映射，始终返回非 nil
func resolveAnthropicModelMapping(claudeModel string) *anthropicModelMapping {
	// 精确匹配
	if m, ok := anthropicModelMappings[claudeModel]; ok {
		return &m
	}

	// 通配符前缀匹配
	for _, wm := range anthropicWildcardMappings {
		if strings.HasPrefix(claudeModel, wm.Prefix) {
			m := wm.Mapping
			return &m
		}
	}

	// 兜底：不认识的模型统一映射
	m := defaultModelMapping
	return &m
}
