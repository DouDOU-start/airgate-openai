package gateway

import (
	"fmt"
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// 构造 ForwardOutcome 的小 helper，避免各路径散落一堆 struct literal。
//
// Success 必带 Usage，fillCost 会基于 Model / ServiceTier 补成本字段。
// ClientError / AccountRateLimited / AccountDead / UpstreamTransient 都从
// Upstream + 上游错误消息推出 Kind。

const (
	usageCurrencyUSD = "USD"

	usageAttrModel       = "model"
	usageAttrServiceTier = "service_tier"
	usageAttrImageSize   = "image_size"

	usageMetricInputTokens           = "input_tokens"
	usageMetricCachedInputTokens     = "cached_input_tokens"
	usageMetricOutputTokens          = "output_tokens"
	usageMetricReasoningOutputTokens = "reasoning_output_tokens"
	usageMetricTotalTokens           = "total_tokens"
	usageMetricImages                = "images"

	usageCostInput       = "input_tokens"
	usageCostCachedInput = "cached_input_tokens"
	usageCostOutput      = "output_tokens"
	usageCostImage       = "images"
	usageCostImageTool   = "image_tool"
)

// successOutcome 构造 Success 判决，Usage 由调用方填。Duration 由调用方填。
func successOutcome(statusCode int, body []byte, headers http.Header, usage *sdk.Usage) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Usage: usage,
	}
}

// failureOutcome 从 HTTP 状态码 + 错误消息分类并构造非 Success 的 Outcome。
// 会原样保留 Upstream（Body / Headers / StatusCode）供 Core 在 ClientError 路径下透传。
func failureOutcome(statusCode int, body []byte, headers http.Header, message string, retryAfter time.Duration) sdk.ForwardOutcome {
	kind := classifyHTTPFailure(statusCode, message)
	reason := message
	if reason != "" {
		reason = fmt.Sprintf("HTTP %d: %s", statusCode, message)
	}
	return sdk.ForwardOutcome{
		Kind: kind,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// transientOutcome 连接级 / 网络层错误（无上游 HTTP 响应），归类为 UpstreamTransient。
// statusCode 给 0 或 502 均可，Core 不会基于此做判断。
func transientOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeUpstreamTransient,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
		Reason:   reason,
	}
}

// accountDeadOutcome 账号级确定性失败（凭证缺失、账号配置错误等），核心会把账号打入 disabled。
func accountDeadOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeAccountDead,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusUnauthorized},
		Reason:   reason,
	}
}

func newTokenUsage(modelID, serviceTier string, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int, firstTokenMs int64) *sdk.Usage {
	usage := &sdk.Usage{
		Model:        modelID,
		Currency:     usageCurrencyUSD,
		FirstTokenMs: firstTokenMs,
	}
	setUsageModelAttribute(usage, modelID)
	setUsageServiceTier(usage, serviceTier)
	setUsageTokens(usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens)
	return usage
}

func setUsageModelAttribute(usage *sdk.Usage, modelID string) {
	if usage == nil || modelID == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrModel,
		Label: "模型",
		Kind:  "model",
		Value: modelID,
	})
}

func setUsageReasoningEffort(usage *sdk.Usage, effort string) {
	if usage == nil || effort == "" {
		return
	}
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata["reasoning_effort"] = effort
}

func setUsageServiceTier(usage *sdk.Usage, tier string) {
	if usage == nil {
		return
	}
	tier = normalizeOpenAIServiceTier(tier)
	if tier == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrServiceTier,
		Label: "服务档位",
		Kind:  "tier",
		Value: tier,
	})
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata[usageAttrServiceTier] = tier
}

func usageServiceTier(usage *sdk.Usage) string {
	if usage == nil {
		return ""
	}
	for _, attr := range usage.Attributes {
		if attr.Key == usageAttrServiceTier {
			return normalizeOpenAIServiceTier(attr.Value)
		}
	}
	if usage.Metadata != nil {
		return normalizeOpenAIServiceTier(usage.Metadata[usageAttrServiceTier])
	}
	return ""
}

func setUsageImageSize(usage *sdk.Usage, size string) {
	if usage == nil || size == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrImageSize,
		Label: "图片尺寸",
		Kind:  "resolution",
		Value: size,
	})
}

func setUsageTokens(usage *sdk.Usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int) {
	if usage == nil {
		return
	}
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricInputTokens,
		Label: "输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricCachedInputTokens,
		Label: "缓存输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(cachedInputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricOutputTokens,
		Label: "输出 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(outputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricReasoningOutputTokens,
		Label: "推理 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(reasoningOutputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricTotalTokens,
		Label: "总 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens + cachedInputTokens + outputTokens),
	})
}

func usageMetricInt(usage *sdk.Usage, key string) int {
	return int(usageMetricValue(usage, key))
}

func usageMetricValue(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, metric := range usage.Metrics {
		if metric.Key == key {
			return metric.Value
		}
	}
	return 0
}

func setUsageAttribute(usage *sdk.Usage, attr sdk.UsageAttribute) {
	for i := range usage.Attributes {
		if usage.Attributes[i].Key == attr.Key {
			usage.Attributes[i] = attr
			return
		}
	}
	usage.Attributes = append(usage.Attributes, attr)
}

func setUsageMetric(usage *sdk.Usage, metric sdk.UsageMetric) {
	for i := range usage.Metrics {
		if usage.Metrics[i].Key == metric.Key {
			usage.Metrics[i] = metric
			return
		}
	}
	usage.Metrics = append(usage.Metrics, metric)
}

func setUsageCostDetail(usage *sdk.Usage, detail sdk.UsageCostDetail) {
	if detail.AccountCost <= 0 {
		removeUsageCostDetail(usage, detail.Key)
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == detail.Key {
			usage.CostDetails[i] = detail
			recomputeUsageAccountCost(usage)
			return
		}
	}
	usage.CostDetails = append(usage.CostDetails, detail)
	recomputeUsageAccountCost(usage)
}

func removeUsageCostDetail(usage *sdk.Usage, key string) {
	if usage == nil {
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == key {
			usage.CostDetails = append(usage.CostDetails[:i], usage.CostDetails[i+1:]...)
			recomputeUsageAccountCost(usage)
			return
		}
	}
}

func recomputeUsageAccountCost(usage *sdk.Usage) {
	if usage == nil {
		return
	}
	var total float64
	for _, detail := range usage.CostDetails {
		total += detail.AccountCost
	}
	usage.AccountCost = total
	if usage.Currency == "" {
		usage.Currency = usageCurrencyUSD
	}
	if total > 0 {
		usage.Summary = fmt.Sprintf("标准成本 $%.6f", total)
	}
}

type tokenPrices struct {
	input  float64
	cached float64
	output float64
}

func pricesForServiceTier(spec model.Spec, tier string) tokenPrices {
	switch normalizeOpenAIServiceTier(tier) {
	case "priority":
		return tokenPrices{
			input: fallbackPrice(spec.InputPricePriority, spec.InputPrice*2),
			cached: fallbackPrice(
				spec.CachedPricePriority,
				spec.CachedPrice*2,
			),
			output: fallbackPrice(spec.OutputPricePriority, spec.OutputPrice*2),
		}
	case "flex":
		return tokenPrices{
			input:  fallbackPrice(spec.InputPriceFlex, spec.InputPrice*0.5),
			cached: fallbackPrice(spec.CachedPriceFlex, spec.CachedPrice*0.5),
			output: fallbackPrice(spec.OutputPriceFlex, spec.OutputPrice*0.5),
		}
	default:
		return tokenPrices{
			input:  spec.InputPrice,
			cached: spec.CachedPrice,
			output: spec.OutputPrice,
		}
	}
}

func fallbackPrice(value, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func applyLongContextPricing(spec model.Spec, prices tokenPrices, inputTokens, cachedInputTokens int) (tokenPrices, bool) {
	if spec.LongContextThreshold <= 0 {
		return prices, false
	}
	if inputTokens+cachedInputTokens <= spec.LongContextThreshold {
		return prices, false
	}
	if spec.LongContextInputMultiplier > 0 {
		prices.input *= spec.LongContextInputMultiplier
	}
	if spec.LongContextCachedMultiplier > 0 {
		prices.cached *= spec.LongContextCachedMultiplier
	}
	if spec.LongContextOutputMultiplier > 0 {
		prices.output *= spec.LongContextOutputMultiplier
	}
	return prices, true
}

func tokenCost(tokens int, pricePerMillion float64) float64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	return float64(tokens) * pricePerMillion / 1_000_000
}

func priceMetadata(price float64, tier string, longContext bool) map[string]string {
	metadata := map[string]string{
		"unit_price": fmt.Sprintf("%.10g", price),
		"unit":       "USD/1M tokens",
	}
	if tier != "" {
		metadata["service_tier"] = tier
	}
	if longContext {
		metadata["long_context"] = "true"
	}
	return metadata
}

// fillUsageCost 用插件自己的模型规格填充 Usage 的平台标准成本。
//
// SDK 只承载通用 Usage 结构；OpenAI 的标准价格、服务档位和长上下文阶梯都留在
// 插件内部实现，Core 入库后再按用户倍率写入 UserCost。
func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}
	spec := model.Lookup(usage.Model)
	serviceTier := usageServiceTier(usage)
	inputTokens := usageMetricInt(usage, usageMetricInputTokens)
	outputTokens := usageMetricInt(usage, usageMetricOutputTokens)
	cachedInputTokens := usageMetricInt(usage, usageMetricCachedInputTokens)
	prices, longContext := applyLongContextPricing(
		spec,
		pricesForServiceTier(spec, serviceTier),
		inputTokens,
		cachedInputTokens,
	)

	inputCost := tokenCost(inputTokens, prices.input)
	cachedCost := tokenCost(cachedInputTokens, prices.cached)
	outputCost := tokenCost(outputTokens, prices.output)

	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricInputTokens,
		Label:       "输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(inputTokens),
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.input, serviceTier, longContext),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricCachedInputTokens,
		Label:       "缓存输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(cachedInputTokens),
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.cached, serviceTier, longContext),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricOutputTokens,
		Label:       "输出 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(outputTokens),
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.output, serviceTier, longContext),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostInput,
		Label:       "输入 Token",
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.input, serviceTier, longContext),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostCachedInput,
		Label:       "缓存输入 Token",
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.cached, serviceTier, longContext),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostOutput,
		Label:       "输出 Token",
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(prices.output, serviceTier, longContext),
	})
}

// fillUsageCostPerImage 按张计费模式填充 Usage。
// 用于 /v1/images/generations 和 /v1/images/edits 等专用图像端点，
// usage.Model 必须是注册了 ImagePrice 的模型。
func fillUsageCostPerImage(usage *sdk.Usage, numImages int) {
	if usage == nil || usage.Model == "" || numImages <= 0 {
		return
	}
	spec := model.Lookup(usage.Model)
	if spec.ImagePrice <= 0 {
		fillUsageCost(usage)
		return
	}
	addImageCost(usage, usageCostImage, "图片生成", numImages, spec.ImagePrice, "")
}

// fillUsageCostPerImageBySize 按 1K/2K/4K size 分档填充 Usage（USD/张）。
// 用于 OAuth → image_generation tool 路径，价格由 imagePriceForSize 硬编码（详见其注释）。
// 跟 spec.ImagePrice 解耦：plugin.yaml 不需要登记 ImagePrice，分档定价完全由网关侧决定。
func fillUsageCostPerImageBySize(usage *sdk.Usage, numImages int, size string) {
	if usage == nil || numImages <= 0 {
		return
	}
	price := imagePriceForSize(size)
	setUsageImageSize(usage, size)
	addImageCost(usage, usageCostImage, "图片生成", numImages, price, size)
}

// fillUsageCostWithImageTool 先按主 model 定价算 token 成本，再叠加图像按张费用。
// numImages 是本次请求实际生成的图片张数（0 表示无图像工具调用）。
func fillUsageCostWithImageTool(usage *sdk.Usage, numImages int) {
	fillUsageCost(usage)
	if usage == nil || numImages <= 0 {
		return
	}
	imageSpec := model.Lookup(imageToolCostModel)
	if imageSpec.ImagePrice <= 0 {
		return
	}
	addImageCost(usage, usageCostImageTool, "图片工具", numImages, imageSpec.ImagePrice, "")
}

func addImageCost(usage *sdk.Usage, key, label string, numImages int, pricePerImage float64, size string) {
	if usage == nil || numImages <= 0 || pricePerImage <= 0 {
		return
	}
	cost := float64(numImages) * pricePerImage
	metadata := map[string]string{
		"unit_price": fmt.Sprintf("%.10g", pricePerImage),
		"unit":       "USD/image",
	}
	if size != "" {
		metadata["size"] = size
	}
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricImages,
		Label:       "图片数量",
		Kind:        "image",
		Unit:        "image",
		Value:       float64(numImages),
		AccountCost: cost,
		Currency:    usageCurrencyUSD,
		Metadata:    metadata,
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         key,
		Label:       label,
		AccountCost: cost,
		Currency:    usageCurrencyUSD,
		Metadata:    metadata,
	})
}
