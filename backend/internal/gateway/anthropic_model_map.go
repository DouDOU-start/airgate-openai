package gateway

import (
	"os"
	"strings"
)

// ──────────────────────────────────────────────────────
// Claude → OpenAI 模型映射
// Claude Code 发送 Claude 模型名，翻译为 OpenAI 模型 + 额外参数
// ──────────────────────────────────────────────────────

// anthropicModelMapping 单条模型映射规则
type anthropicModelMapping struct {
	// OpenAIModel 映射到的 OpenAI 模型名
	OpenAIModel string
	// FallbackModel 当主模型不可用时降级使用的模型（空则不降级）
	FallbackModel string
	// ReasoningEffort 默认的 reasoning_effort（客户端 thinking 配置优先）
	ReasoningEffort string
}

var (
	defaultClaudeTargetModel = normalizeMappedModelID(
		firstNonEmptyEnv("AIRGATE_DEFAULT_CLAUDE_MODEL"),
		"gpt-5.4",
	)
	opusTargetModel = resolveRoleTargetModel(
		defaultClaudeTargetModel,
		"AIRGATE_MODEL_OPUS",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
	)
	sonnetTargetModel = resolveRoleTargetModel(
		defaultClaudeTargetModel,
		"AIRGATE_MODEL_SONNET",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	)
	haikuTargetModel = resolveRoleTargetModel(
		"gpt-5.3-codex-spark",
		"AIRGATE_MODEL_HAIKU",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	)
	// haikuFallbackModel Haiku 降级模型：当主模型不可用时回退
	haikuFallbackModel = resolveRoleTargetModel(
		"gpt-5-mini",
		"AIRGATE_MODEL_HAIKU_FALLBACK",
	)
	// opusFallbackModel Opus 降级模型：普通账号不支持 gpt-5.4 时回退
	opusFallbackModel = resolveRoleTargetModel(
		"gpt-5.2",
		"AIRGATE_MODEL_OPUS_FALLBACK",
	)
	// sonnetFallbackModel Sonnet 降级模型
	sonnetFallbackModel = resolveRoleTargetModel(
		"gpt-5.2",
		"AIRGATE_MODEL_SONNET_FALLBACK",
	)
	// defaultFallbackModel 兜底降级模型
	defaultFallbackModel = resolveRoleTargetModel(
		"gpt-5.2",
		"AIRGATE_MODEL_DEFAULT_FALLBACK",
	)
	// sparkTargetModel 简单操作加速模型（Read/Grep/Glob 结果处理时自动路由）
	// 空字符串表示禁用 Spark 路由
	sparkTargetModel = resolveRoleTargetModel(
		"gpt-5.3-codex-spark",
		"AIRGATE_MODEL_SPARK",
	)
	// codexDefaultModel Codex CLI 透传路径的兜底模型。
	// 当客户端请求体里 model 字段为空、null 或字面量 "None" 时使用这个值，
	// 避免把无效 model 发到上游触发 "The 'None' model is not supported" 错误。
	codexDefaultModel = resolveRoleTargetModel(
		"gpt-5.2",
		"AIRGATE_CODEX_DEFAULT_MODEL",
	)
	enableAnthropicContinuation = strings.EqualFold(firstNonEmptyEnv("AIRGATE_ENABLE_ANTHROPIC_CONTINUATION"), "true")
)

// anthropicModelMappings Claude 模型名 → OpenAI 模型映射表
// 精确匹配优先，通配符匹配其次
var anthropicModelMappings = map[string]anthropicModelMapping{
	// Opus → 最高推理，普通账号降级到 gpt-5.2
	"claude-opus-4-6": {OpenAIModel: opusTargetModel, FallbackModel: opusFallbackModel, ReasoningEffort: "xhigh"},
	"claude-opus-4-5": {OpenAIModel: opusTargetModel, FallbackModel: opusFallbackModel, ReasoningEffort: "xhigh"},

	// Sonnet → 高推理，普通账号降级到 gpt-5.2
	"claude-sonnet-4-6": {OpenAIModel: sonnetTargetModel, FallbackModel: sonnetFallbackModel, ReasoningEffort: "high"},
	"claude-sonnet-4-5": {OpenAIModel: sonnetTargetModel, FallbackModel: sonnetFallbackModel, ReasoningEffort: "high"},

	// Haiku → 快速模型，不可用时降级到 gpt-5.3-codex-spark
	"claude-haiku-4-6": {OpenAIModel: haikuTargetModel, FallbackModel: haikuFallbackModel, ReasoningEffort: "low"},
	"claude-haiku-4-5": {OpenAIModel: haikuTargetModel, FallbackModel: haikuFallbackModel, ReasoningEffort: "low"},
}

// anthropicWildcardMappings 通配符映射（前缀匹配，按优先级排序）
var anthropicWildcardMappings = []struct {
	Prefix  string
	Mapping anthropicModelMapping
}{
	// claude-haiku-4-* 所有变体
	{"claude-haiku-4-", anthropicModelMapping{OpenAIModel: haikuTargetModel, FallbackModel: haikuFallbackModel, ReasoningEffort: "low"}},
	// claude-haiku-4-5-* 所有变体（如 claude-haiku-4-5-20251001）
	{"claude-haiku-4-5", anthropicModelMapping{OpenAIModel: haikuTargetModel, FallbackModel: haikuFallbackModel, ReasoningEffort: "low"}},
	// claude-sonnet-4- 所有变体
	{"claude-sonnet-4-", anthropicModelMapping{OpenAIModel: sonnetTargetModel, FallbackModel: sonnetFallbackModel, ReasoningEffort: "high"}},
	// claude-opus-4- 所有变体
	{"claude-opus-4-", anthropicModelMapping{OpenAIModel: opusTargetModel, FallbackModel: opusFallbackModel, ReasoningEffort: "xhigh"}},
	// claude-haiku- 所有变体
	{"claude-haiku-", anthropicModelMapping{OpenAIModel: haikuTargetModel, FallbackModel: haikuFallbackModel, ReasoningEffort: "low"}},
	// claude-3.5/3 系列兜底
	{"claude-3", anthropicModelMapping{OpenAIModel: defaultClaudeTargetModel, FallbackModel: defaultFallbackModel, ReasoningEffort: ""}},
	// 兜底：所有 claude- 前缀
	{"claude-", anthropicModelMapping{OpenAIModel: defaultClaudeTargetModel, FallbackModel: defaultFallbackModel, ReasoningEffort: ""}},
}

// defaultModelMapping 兜底映射：不认识的模型统一用 gpt-5.4，普通账号降级到 gpt-5.2
var defaultModelMapping = anthropicModelMapping{OpenAIModel: defaultClaudeTargetModel, FallbackModel: defaultFallbackModel, ReasoningEffort: ""}

func resolveRoleTargetModel(fallback string, keys ...string) string {
	return normalizeMappedModelID(firstNonEmptyEnv(keys...), fallback)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func normalizeMappedModelID(raw string, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	if idx := strings.LastIndex(value, "@"); idx >= 0 && idx+1 < len(value) {
		value = strings.TrimSpace(value[idx+1:])
	}
	value = strings.TrimPrefix(value, "openai/")
	value = strings.TrimPrefix(value, "oai/")
	if value == "" {
		return fallback
	}
	return value
}

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

// isModelNotFoundError 判断上游错误是否为模型不可用（用于 fallback 降级）
func isModelNotFoundError(statusCode int, body []byte) bool {
	if statusCode == 404 {
		return true
	}
	if statusCode == 400 {
		msg := strings.ToLower(string(body))
		return strings.Contains(msg, "model") &&
			(strings.Contains(msg, "not found") ||
				strings.Contains(msg, "does not exist") ||
				strings.Contains(msg, "not available") ||
				strings.Contains(msg, "invalid model") ||
				strings.Contains(msg, "not supported"))
	}
	return false
}
