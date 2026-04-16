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
//
// 定价对齐 OpenAI 官方规则：
//   - 标准档：Input / Cached / Output
//   - Priority 档：*Priority 字段（≈ 标准 × 2），缺省时 SDK 以 × 2 兜底
//   - Flex / Batch 档：*Flex 字段（= 标准 × 0.5），缺省时 SDK 以 × 0.5 兜底
//   - 长上下文档（仅 gpt-5.4 家族）：完整 input_tokens 超过 LongContextThreshold
//     且非 priority 档时，整次请求全量按倍率计费
type Spec struct {
	Name            string
	ContextWindow   int
	MaxOutputTokens int

	// 标准档单价（$/1M tokens）
	InputPrice  float64
	CachedPrice float64
	OutputPrice float64

	// Priority 档单价（$/1M tokens）。零值表示未配置，由 SDK 以标准 × 2 兜底。
	InputPricePriority  float64
	CachedPricePriority float64
	OutputPricePriority float64

	// Flex / Batch 档单价（$/1M tokens）。零值表示未配置，由 SDK 以标准 × 0.5 兜底。
	InputPriceFlex  float64
	CachedPriceFlex float64
	OutputPriceFlex float64

	// 长上下文阶梯（只对 gpt-5.4 家族填非零值）。
	LongContextThreshold        int
	LongContextInputMultiplier  float64
	LongContextOutputMultiplier float64
	LongContextCachedMultiplier float64
}

// std 快捷构造一个三档（standard / priority / flex）价格齐全的 Spec，
// 倍率按 OpenAI 官方：priority = 2×standard，flex = 0.5×standard。
func std(name string, ctx, maxOut int, input, cached, output float64) Spec {
	return Spec{
		Name:                name,
		ContextWindow:       ctx,
		MaxOutputTokens:     maxOut,
		InputPrice:          input,
		CachedPrice:         cached,
		OutputPrice:         output,
		InputPricePriority:  input * 2,
		CachedPricePriority: cached * 2,
		OutputPricePriority: output * 2,
		InputPriceFlex:      input * 0.5,
		CachedPriceFlex:     cached * 0.5,
		OutputPriceFlex:     output * 0.5,
	}
}

// withLongCtx 在已构造的 Spec 基础上附加 gpt-5.4 家族的长上下文阶梯。
// OpenAI 官方：input ×2、cached ×2、output ×1.5，阈值 272k input_tokens。
func withLongCtx(s Spec) Spec {
	s.LongContextThreshold = 272_000
	s.LongContextInputMultiplier = 2.0
	s.LongContextOutputMultiplier = 1.5
	s.LongContextCachedMultiplier = 2.0
	return s
}

// registry 全局模型注册表（按模型 ID 索引）
// ─── 新增模型只需在此处加一行 ───
var registry = map[string]Spec{
	// ── GPT-5.4（唯一具备长上下文阶梯的家族）──
	"gpt-5.4": withLongCtx(std("GPT 5.4", 272000, 128000, 2.5, 0.25, 15.0)),

	// ── Codex 5.x ──
	"gpt-5.3-codex":       std("GPT 5.3 Codex", 272000, 128000, 1.75, 0.175, 14.0),
	"gpt-5.3-codex-spark": std("GPT 5.3 Codex Spark", 128000, 128000, 1.75, 0.175, 14.0),
	"gpt-5-codex-mini":    std("GPT 5 Codex Mini", 128000, 128000, 0.25, 0.025, 2.0),

	// ── GPT 基础系列 ──
	"gpt-5.2": std("GPT 5.2", 272000, 128000, 1.75, 0.175, 14.0),
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
		models = append(models, toModelInfo(id, spec))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// toModelInfo 将内部 Spec 映射为 SDK ModelInfo，供 manifest 生成与费用计算共用。
func toModelInfo(id string, spec Spec) sdk.ModelInfo {
	return sdk.ModelInfo{
		ID:                          id,
		Name:                        spec.Name,
		ContextWindow:               spec.ContextWindow,
		MaxOutputTokens:             spec.MaxOutputTokens,
		InputPrice:                  spec.InputPrice,
		OutputPrice:                 spec.OutputPrice,
		CachedInputPrice:            spec.CachedPrice,
		InputPricePriority:          spec.InputPricePriority,
		OutputPricePriority:         spec.OutputPricePriority,
		CachedInputPricePriority:    spec.CachedPricePriority,
		InputPriceFlex:              spec.InputPriceFlex,
		OutputPriceFlex:             spec.OutputPriceFlex,
		CachedInputPriceFlex:        spec.CachedPriceFlex,
		LongContextThreshold:        spec.LongContextThreshold,
		LongContextInputMultiplier:  spec.LongContextInputMultiplier,
		LongContextOutputMultiplier: spec.LongContextOutputMultiplier,
		LongContextCachedMultiplier: spec.LongContextCachedMultiplier,
	}
}
