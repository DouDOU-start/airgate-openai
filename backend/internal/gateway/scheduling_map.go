package gateway

import (
	"encoding/json"
	"os"
	"strings"
)

// anthropicRouteMetadata 构建 Anthropic 协议翻译路由的 Metadata 声明：
//   - error_format：对外错误体按 Anthropic 格式输出；
//   - scheduling_model_map：claude-* 请求模型 → OpenAI 平台调度模型的前缀映射表，
//     Core 据此选号（声明式，取代 Core 侧的平台/路径硬编码，见 core 的
//     docs/architecture/current/plugin-contract.md 约定表）。
func anthropicRouteMetadata() map[string]string {
	return map[string]string{
		"error_format":         "anthropic",
		"scheduling_model_map": anthropicSchedulingModelMapJSON(),
	}
}

// anthropicSchedulingModelMapJSON 生成前缀映射表 JSON。
// 键为模型 ID 前缀（Core 按最长前缀匹配），值为调度候选模型（按优先级）。
// 部署可经环境变量覆盖（与历史 Core 侧变量同名，平滑迁移）。
func anthropicSchedulingModelMapJSON() string {
	defaultTarget := envModel("gpt-5.5", "AIRGATE_DEFAULT_CLAUDE_MODEL")
	m := map[string][]string{
		"claude-haiku-": {
			envModel("gpt-5.3-codex-spark", "AIRGATE_MODEL_HAIKU", "ANTHROPIC_DEFAULT_HAIKU_MODEL"),
			envModel("gpt-5.4-mini", "AIRGATE_MODEL_HAIKU_FALLBACK"),
		},
		"claude-sonnet-": {
			envModel(defaultTarget, "AIRGATE_MODEL_SONNET", "ANTHROPIC_DEFAULT_SONNET_MODEL"),
			envModel("gpt-5.4", "AIRGATE_MODEL_SONNET_FALLBACK"),
		},
		"claude-opus-": {
			envModel(defaultTarget, "AIRGATE_MODEL_OPUS", "ANTHROPIC_DEFAULT_OPUS_MODEL"),
			envModel("gpt-5.4", "AIRGATE_MODEL_OPUS_FALLBACK"),
		},
		"claude-": {
			defaultTarget,
			envModel("gpt-5.4", "AIRGATE_MODEL_DEFAULT_FALLBACK"),
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// envModel 依次读取环境变量，取首个非空值，否则用默认值。
func envModel(fallback string, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return fallback
}
