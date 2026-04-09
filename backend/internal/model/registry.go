package model

import (
	"sort"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// 集中模型注册表
// 新增模型只需在 registry 中加一行，所有引用点自动生效
// ──────────────────────────────────────────────────────

// Spec 单个模型的完整元数据
type Spec struct {
	Name            string  // 展示名称
	ContextWindow   int     // 上下文窗口（tokens）
	MaxOutputTokens int     // 最大输出 tokens
	InputPrice      float64 // 输入价格（$/1M tokens）
	CachedPrice     float64 // 缓存输入价格（$/1M tokens）
	OutputPrice     float64 // 输出价格（$/1M tokens）
}

// registry 全局模型注册表（按模型 ID 索引）
// ─── 新增模型只需在此处加一行 ───
// 字段顺序：Name, ContextWindow, MaxOutputTokens, InputPrice, CachedPrice, OutputPrice
var registry = map[string]Spec{
	// ── GPT-5.4 ──
	"gpt-5.4": {"GPT 5.4", 272000, 128000, 2.5, 0.25, 15.0},

	// ── Codex 5.x ──
	"gpt-5.3-codex":       {"GPT 5.3 Codex", 272000, 128000, 1.75, 0.175, 14.0},
	"gpt-5.3-codex-spark": {"GPT 5.3 Codex Spark", 128000, 128000, 1.75, 0.175, 14.0},
	"gpt-5.2-codex":       {"GPT 5.2 Codex", 272000, 128000, 1.75, 0.175, 14.0},
	"gpt-5.1-codex":       {"GPT 5.1 Codex", 272000, 128000, 1.25, 0.125, 10.0},
	"gpt-5.1-codex-max":   {"GPT 5.1 Codex Max", 272000, 128000, 1.25, 0.125, 10.0},
	"gpt-5.1-codex-mini":  {"GPT 5.1 Codex Mini", 128000, 128000, 0.25, 0.025, 2.0},
	"gpt-5-codex":         {"GPT 5 Codex", 272000, 128000, 1.25, 0.125, 10.0},
	"gpt-5-codex-mini":    {"GPT 5 Codex Mini", 128000, 128000, 0.25, 0.025, 2.0},

	// ── GPT 基础系列 ──
	"gpt-5":      {"GPT 5", 272000, 128000, 1.25, 0.125, 10.0},
	"gpt-5.1":    {"GPT 5.1", 272000, 128000, 1.25, 0.125, 10.0},
	"gpt-5.2":    {"GPT 5.2", 272000, 128000, 1.75, 0.175, 14.0},
	"gpt-5-mini": {"GPT 5 Mini", 128000, 16384, 0.125, 0.025, 1.0},
}

// DefaultSpec 未注册模型的兜底值
var DefaultSpec = Spec{
	Name:            "Unknown",
	ContextWindow:   272000,
	MaxOutputTokens: 128000,
}

// Lookup 查询模型元数据，未找到返回默认值
func Lookup(modelID string) Spec {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if spec, ok := registry[id]; ok {
		return spec
	}
	return DefaultSpec
}

// IsKnown 判断给定 model ID 是否在注册表内（大小写不敏感、忽略首尾空白）。
// 用于请求入口的 model 兜底：未注册的 model 会被换成默认值，
// 避免把"不支持的模型"推到上游账号。
func IsKnown(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := registry[id]
	return ok
}

// AllSpecs 返回所有注册模型的 SDK ModelInfo 列表（按 ID 排序）
func AllSpecs() []sdk.ModelInfo {
	models := make([]sdk.ModelInfo, 0, len(registry))
	for id, spec := range registry {
		models = append(models, sdk.ModelInfo{
			ID:               id,
			Name:             spec.Name,
			ContextWindow:    spec.ContextWindow,
			MaxOutputTokens:  spec.MaxOutputTokens,
			InputPrice:       spec.InputPrice,
			OutputPrice:      spec.OutputPrice,
			CachedInputPrice: spec.CachedPrice,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}
