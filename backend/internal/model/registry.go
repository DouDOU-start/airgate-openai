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
	OutputPrice     float64 // 输出价格（$/1M tokens）
}

// registry 全局模型注册表（按模型 ID 索引）
// ─── 新增模型只需在此处加一行 ───
var registry = map[string]Spec{
	// ── GPT-5.4 ──
	"gpt-5.4":     {"GPT 5.4", 1050000, 128000, 2.5, 15.0},
	"gpt-5.4-pro": {"GPT 5.4 Pro", 1050000, 128000, 30.0, 180.0},

	// ── Codex 5.x ──
	"gpt-5.3-codex":       {"GPT 5.3 Codex", 400000, 128000, 2.0, 8.0},
	"gpt-5.3-codex-spark": {"GPT 5.3 Codex Spark", 128000, 128000, 0.5, 2.0},
	"gpt-5.2-codex":       {"GPT 5.2 Codex", 400000, 128000, 2.0, 8.0},
	"gpt-5.1-codex":       {"GPT 5.1 Codex", 400000, 128000, 2.0, 8.0},
	"gpt-5.1-codex-max":   {"GPT 5.1 Codex Max", 400000, 128000, 2.0, 8.0},
	"gpt-5.1-codex-mini":  {"GPT 5.1 Codex Mini", 400000, 128000, 1.0, 4.0},
	"gpt-5-codex":         {"GPT 5 Codex", 400000, 128000, 2.0, 8.0},
	"gpt-5-codex-mini":    {"GPT 5 Codex Mini", 400000, 128000, 1.0, 4.0},

	// ── Codex 旧版 ──
	"codex-mini-latest": {"Codex Mini", 128000, 16384, 1.5, 6.0},

	// ── GPT 基础系列 ──
	"gpt-5":   {"GPT 5", 400000, 128000, 2.0, 8.0},
	"gpt-5.1": {"GPT 5.1", 400000, 128000, 2.0, 8.0},
	"gpt-5.2": {"GPT 5.2", 400000, 128000, 2.0, 8.0},

	// ── GPT-4.1 ──
	"gpt-4.1":      {"GPT-4.1", 1047576, 32768, 2.0, 8.0},
	"gpt-4.1-mini": {"GPT-4.1 Mini", 1047576, 32768, 0.4, 1.6},
	"gpt-4.1-nano": {"GPT-4.1 Nano", 1047576, 32768, 0.1, 0.4},

	// ── GPT-4o ──
	"gpt-4o":      {"GPT-4o", 128000, 16384, 2.5, 10.0},
	"gpt-4o-mini": {"GPT-4o Mini", 128000, 16384, 0.15, 0.6},

	// ── o 系列推理模型 ──
	"o3":      {"o3", 200000, 32768, 10.0, 40.0},
	"o3-pro":  {"o3 Pro", 200000, 32768, 20.0, 80.0},
	"o4-mini": {"o4-mini", 200000, 32768, 1.1, 4.4},
	"o3-mini": {"o3-mini", 200000, 32768, 1.1, 4.4},

	// ── 图像模型 ──
	"gpt-image-1": {"GPT Image 1", 0, 0, 5.0, 40.0},
}

// DefaultSpec 未注册模型的兜底值
var DefaultSpec = Spec{
	Name:            "Unknown",
	ContextWindow:   200000,
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

// AllSpecs 返回所有注册模型的 SDK ModelInfo 列表（按 ID 排序）
func AllSpecs() []sdk.ModelInfo {
	models := make([]sdk.ModelInfo, 0, len(registry))
	for id, spec := range registry {
		models = append(models, sdk.ModelInfo{
			ID:          id,
			Name:        spec.Name,
			MaxTokens:   spec.ContextWindow,
			InputPrice:  spec.InputPrice,
			OutputPrice: spec.OutputPrice,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}
