package model

import (
	"sort"
	"strings"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────
// 集中模型注册表
// 新增模型只需在 registry 中加一行，所有引用点自动生效
// ──────────────────────────────────────────────────────

// Spec 单个模型的完整元数据
//
// 定价对齐 OpenAI 官方规则：
//   - 标准档：Input / Cached / Output
//   - Priority 档：*Priority 字段（通常标准 × 2；gpt-5.5 为 × 2.5），缺省时 SDK 以 × 2 兜底
//   - Flex / Batch 档：*Flex 字段（= 标准 × 0.5），缺省时 SDK 以 × 0.5 兜底
//   - 长上下文档（仅 gpt-5.4 家族）：完整 input_tokens 超过 LongContextThreshold
//     时，整次请求全量按倍率计费
type Spec struct {
	Name            string
	ContextWindow   int
	MaxOutputTokens int

	// 图像模型能力标记价（$/张）。> 0 表示具备图像生成能力；实际 token 标准价仍
	// 使用 InputPrice / OutputPrice，分组固定单张价由 Core 通过 metadata 覆盖。
	ImagePrice float64

	// 标准档单价（$/1M tokens）
	InputPrice  float64
	CachedPrice float64
	OutputPrice float64

	// Priority 档单价（$/1M tokens）。零值表示未配置，由 SDK 以标准 × 2 兜底。
	InputPricePriority  float64
	CachedPricePriority float64
	OutputPricePriority float64

	// Fast 档单价（$/1M tokens）。当前未使用，保持零值。
	InputPriceFast  float64
	CachedPriceFast float64
	OutputPriceFast float64

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

// std 快捷构造 standard / priority / flex 价格齐全的 Spec，
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

func withPriorityMultiplier(s Spec, multiplier float64) Spec {
	s.InputPricePriority = s.InputPrice * multiplier
	s.CachedPricePriority = s.CachedPrice * multiplier
	s.OutputPricePriority = s.OutputPrice * multiplier
	return s
}

// imgSpec 构造图像模型 Spec。ImagePrice 只作为图像能力标记；
// 标准计费仍按 token 单价计算，Core 可用分组图片单张价替代最终计费。
func imgSpec(name string, input, cached, output, pricePerImage float64) Spec {
	spec := std(name, 32000, 0, input, cached, output)
	spec.ImagePrice = pricePerImage
	return spec
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
	"gpt-5.5": withPriorityMultiplier(std("GPT 5.5", 400000, 128000, 5.0, 0.5, 30.0), 2.5),

	// ── GPT-5.4（唯一具备长上下文阶梯的家族）──
	"gpt-5.4": withLongCtx(std("GPT 5.4", 272000, 128000, 2.5, 0.25, 15.0)),

	// ── Codex 5.x ──
	"gpt-5.3-codex":       std("GPT 5.3 Codex", 272000, 128000, 1.75, 0.175, 14.0),
	"gpt-5.3-codex-spark": std("GPT 5.3 Codex Spark", 128000, 128000, 1.75, 0.175, 14.0),
	"gpt-5.4-mini":        std("GPT 5.4 Mini", 128000, 128000, 0.75, 0.075, 4.5),

	// ── GPT 基础系列 ──
	"gpt-5.2": std("GPT 5.2", 272000, 128000, 1.75, 0.175, 14.0),

	// ── 图像生成：标准成本按 token 计，分组图片固定单价由 Core 替代最终计费 ──
	"gpt-image-1":   imgSpec("GPT Image 1", 5.0, 0.5, 30.0, 0.20),
	"gpt-image-1.5": imgSpec("GPT Image 1.5", 5.0, 0.5, 30.0, 0.20),
	"gpt-image-2":   imgSpec("GPT Image 2", 5.0, 0.5, 30.0, 0.20),
}

// DefaultSpec 未注册模型的最终兜底值。按 gpt-5.4 标准档计价——宁可略高也不能 0。
// （0 价格会导致免费流量，之前一个 bug 来源。）
var DefaultSpec = withLongCtx(std("Unknown (billed as gpt-5.4)", 272000, 128000, 2.5, 0.25, 15.0))

// Lookup 查询模型元数据。未命中注册表时按关键字推断到最接近的系列，仍无法匹配再落 DefaultSpec。
//
// 这避免了"客户端请求未知模型 → Spec 全 0 → cost=0 免费使用"的坑：只要能看出系列
// （mini / codex / image / gpt-5 等），就按对应系列定价；彻底不认识的兜底到 GPT-5.4 标准价。
func Lookup(modelID string) Spec {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if spec, ok := registry[id]; ok {
		return spec
	}
	if spec, ok := fallbackByKeyword(id); ok {
		return spec
	}
	return DefaultSpec
}

// fallbackByKeyword 从模型 ID 关键字推断最接近的已注册系列。未命中返回 (_, false)。
func fallbackByKeyword(id string) (Spec, bool) {
	if id == "" {
		return Spec{}, false
	}
	// 顺序敏感：先细分（codex / mini / image）后粗分（gpt-5 / gpt-4）
	switch {
	case strings.Contains(id, "codex"):
		return registry["gpt-5.3-codex"], true
	case strings.Contains(id, "image"):
		return registry["gpt-image-1.5"], true
	case strings.Contains(id, "mini") || strings.Contains(id, "nano"):
		return registry["gpt-5.4-mini"], true
	case strings.Contains(id, "gpt-5") || strings.HasPrefix(id, "gpt5") ||
		strings.Contains(id, "o1") || strings.Contains(id, "o3") || strings.Contains(id, "o4"):
		return registry["gpt-5.4"], true
	case strings.Contains(id, "gpt-4") || strings.HasPrefix(id, "gpt4"):
		// gpt-4 系列未显式注册，按 gpt-5.4 标准价计（偏保守）
		return registry["gpt-5.4"], true
	}
	return Spec{}, false
}

// IsImageOnly 判断给定 model 是否为纯图像生成模型（ImagePrice > 0）。
func IsImageOnly(modelID string) bool {
	return Lookup(modelID).ImagePrice > 0
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

// AllSpecs 返回注册模型的 SDK ModelInfo 列表（按 ID 排序）。
// includeImages=true 时返回对话模型和图像模型，false 时只返回对话模型。
func AllSpecs(includeImages bool) []sdk.ModelInfo {
	models := make([]sdk.ModelInfo, 0, len(registry))
	for id, spec := range registry {
		isImage := spec.ImagePrice > 0
		if isImage && !includeImages {
			continue
		}
		models = append(models, toModelInfo(id, spec))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// AllModels 返回所有注册模型（不过滤），用于插件运行时声明。
func AllModels() []sdk.ModelInfo {
	models := make([]sdk.ModelInfo, 0, len(registry))
	for id, spec := range registry {
		models = append(models, toModelInfo(id, spec))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// AllPricingSpecs 返回所有注册模型的插件私有规格（按 ID 排序）。
//
// SDK 的 ModelInfo 不承载价格；manifest 如需展示标准价格，应从这里读取插件自己的
// 计费规格，而不是把价格重新塞回 SDK 结构。
func AllPricingSpecs() []NamedSpec {
	items := make([]NamedSpec, 0, len(registry))
	for id, spec := range registry {
		items = append(items, NamedSpec{ID: id, Spec: spec})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

// NamedSpec 是带模型 ID 的插件私有规格。
type NamedSpec struct {
	ID   string
	Spec Spec
}

// toModelInfo 将内部 Spec 映射为 SDK ModelInfo。
func toModelInfo(id string, spec Spec) sdk.ModelInfo {
	return sdk.ModelInfo{
		ID:              id,
		Name:            spec.Name,
		ContextWindow:   spec.ContextWindow,
		MaxOutputTokens: spec.MaxOutputTokens,
		Capabilities:    modelCapabilities(spec),
	}
}

func modelCapabilities(spec Spec) []string {
	if spec.ImagePrice > 0 {
		return []string{sdk.ModelCapImageGeneration}
	}
	return []string{sdk.ModelCapChat, sdk.ModelCapReasoning}
}
