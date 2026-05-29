package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
)

// 构造 ForwardOutcome 的小 helper，避免各路径散落一堆 struct literal。
//
// Success 必带 Usage，fillCost 会基于 Model / Metadata.service_tier 补成本字段。
// ClientError / AccountRateLimited / AccountDead / UpstreamTransient 都从
// Upstream + 上游错误消息推出 Kind。

const (
	usageCurrencyUSD = "USD"

	usageAttrServiceTier = "service_tier"
	usageAttrImageSize   = "openai.image.size"
	usageAttrResponseID  = "openai.response_id"

	usageMetricInputTokens           = "input_tokens"
	usageMetricTextInputTokens       = "openai.image.input_text_tokens"
	usageMetricImageInputTokens      = "openai.image.input_image_tokens"
	usageMetricCachedInputTokens     = "cached_input_tokens"
	usageMetricOutputTokens          = "output_tokens"
	usageMetricReasoningOutputTokens = "reasoning_output_tokens"
	usageMetricTotalTokens           = "total_tokens"
	usageMetricImages                = "openai.image.count"
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

// forwardErrForOutcome 只在非 ClientError 时保留 err。
// 这样 core 才会把客户端 4xx 的原始 body 直接透传回去；
// 账号级 / 上游级失败则继续走统一脱敏和 failover。
func forwardErrForOutcome(outcome sdk.ForwardOutcome, err error) error {
	if outcome.Kind == sdk.OutcomeClientError {
		return nil
	}
	return err
}

func newTokenUsage(modelID, serviceTier string, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int, firstTokenMs int64) *sdk.Usage {
	usage := &sdk.Usage{
		Model:        modelID,
		Currency:     usageCurrencyUSD,
		FirstTokenMs: firstTokenMs,
	}
	setUsageServiceTier(usage, serviceTier)
	setUsageTokens(usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens)
	return usage
}

func setUsageReasoningEffort(usage *sdk.Usage, effort string) {
	if usage == nil || effort == "" {
		return
	}
	usage.ReasoningEffort = effort
}

func setUsageServiceTier(usage *sdk.Usage, tier string) {
	if usage == nil {
		return
	}
	tier = normalizeOpenAIServiceTier(tier)
	if tier == "" {
		return
	}
	setUsageMetadata(usage, usageAttrServiceTier, tier)
}

func usageServiceTier(usage *sdk.Usage) string {
	if usage == nil {
		return ""
	}
	return normalizeOpenAIServiceTier(usageMetadataText(usage, usageAttrServiceTier))
}

func setUsageImageSize(usage *sdk.Usage, size string) {
	if usage == nil || size == "" {
		return
	}
	setUsageMetadata(usage, usageAttrImageSize, size)
}

func setUsageResponseID(usage *sdk.Usage, responseID string) {
	responseID = strings.TrimSpace(responseID)
	if usage == nil || !strings.HasPrefix(responseID, "resp_") {
		return
	}
	setUsageMetadata(usage, usageAttrResponseID, responseID)
}

func setUsageTokens(usage *sdk.Usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int) {
	if usage == nil {
		return
	}
	usage.InputTokens = inputTokens
	usage.OutputTokens = outputTokens
	usage.CachedInputTokens = cachedInputTokens
	usage.ReasoningOutputTokens = reasoningOutputTokens
}

func setUsageInputTokenDetails(usage *sdk.Usage, textInputTokens, imageInputTokens int) {
	if usage == nil || textInputTokens+imageInputTokens <= 0 {
		return
	}
	setUsageMetadataInt(usage, usageMetricTextInputTokens, textInputTokens)
	setUsageMetadataInt(usage, usageMetricImageInputTokens, imageInputTokens)
}

func usageMetricInt(usage *sdk.Usage, key string) int {
	return int(usageMetricValue(usage, key))
}

func usageMetricValue(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	switch key {
	case usageMetricInputTokens:
		return float64(usage.InputTokens)
	case usageMetricTextInputTokens:
		return usageMetadataFloat(usage, usageMetricTextInputTokens)
	case usageMetricImageInputTokens:
		return usageMetadataFloat(usage, usageMetricImageInputTokens)
	case usageMetricCachedInputTokens:
		return float64(usage.CachedInputTokens)
	case usageMetricOutputTokens:
		return float64(usage.OutputTokens)
	case usageMetricReasoningOutputTokens:
		return float64(usage.ReasoningOutputTokens)
	case usageMetricTotalTokens:
		return float64(usage.InputTokens + usage.CachedInputTokens + usage.OutputTokens)
	case usageMetricImages:
		return usageMetadataFloat(usage, usageMetricImages)
	}
	return 0
}

func setUsageMetadata(usage *sdk.Usage, key, value string) {
	if usage == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata[key] = value
}

func setUsageMetadataInt(usage *sdk.Usage, key string, value int) {
	if value <= 0 {
		return
	}
	setUsageMetadata(usage, key, strconv.Itoa(value))
}

func setUsageMetadataFloat(usage *sdk.Usage, key string, value float64) {
	if value <= 0 {
		return
	}
	setUsageMetadata(usage, key, strconv.FormatFloat(value, 'f', -1, 64))
}

func addUsageMetadataInt(usage *sdk.Usage, key string, delta int) {
	if usage == nil || delta <= 0 {
		return
	}
	setUsageMetadataInt(usage, key, int(usageMetadataFloat(usage, key))+delta)
}

func usageMetadataText(usage *sdk.Usage, key string) string {
	if usage == nil {
		return ""
	}
	return strings.TrimSpace(usage.Metadata[key])
}

func usageMetadataFloat(usage *sdk.Usage, key string) float64 {
	raw := usageMetadataText(usage, key)
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

func recomputeUsageAccountCost(usage *sdk.Usage) {
	if usage == nil {
		return
	}
	total := usage.InputCost + usage.OutputCost + usage.CachedInputCost + usage.CacheCreationCost
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
	prices, _ := applyLongContextPricing(
		spec,
		pricesForServiceTier(spec, serviceTier),
		inputTokens,
		cachedInputTokens,
	)

	inputCost := tokenCost(inputTokens, prices.input)
	cachedCost := tokenCost(cachedInputTokens, prices.cached)
	outputCost := tokenCost(outputTokens, prices.output)
	usage.InputPrice = prices.input
	usage.CachedInputPrice = prices.cached
	usage.OutputPrice = prices.output
	usage.InputCost = inputCost
	usage.CachedInputCost = cachedCost
	usage.OutputCost = outputCost
	recomputeUsageAccountCost(usage)

}

// fillUsageCostPerImageBySize 记录 1K/2K/4K 图片 metadata，并按模型 token 标准价
// 填充 Usage。分组固定单张价由 Core 用 metadata 替代最终计费。
func fillUsageCostPerImageBySize(usage *sdk.Usage, numImages int, size string) {
	if usage == nil || numImages <= 0 {
		return
	}
	price := imagePriceForSize(size)
	fillUsageCost(usage)
	setUsageImageSize(usage, size)
	addImageCost(usage, numImages, price)
}

func addUsageCostForModel(
	usage *sdk.Usage,
	modelID, serviceTier string,
	inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int,
) {
	if usage == nil || modelID == "" {
		return
	}
	source := newTokenUsage(modelID, serviceTier, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens, 0)
	fillUsageCost(source)
	if source.InputPrice > 0 && usage.InputPrice == 0 {
		usage.InputPrice = source.InputPrice
	}
	if source.CachedInputPrice > 0 && usage.CachedInputPrice == 0 {
		usage.CachedInputPrice = source.CachedInputPrice
	}
	if source.OutputPrice > 0 && usage.OutputPrice == 0 {
		usage.OutputPrice = source.OutputPrice
	}
	usage.InputCost += source.InputCost
	usage.CachedInputCost += source.CachedInputCost
	usage.OutputCost += source.OutputCost
	usage.CacheCreationCost += source.CacheCreationCost
	recomputeUsageAccountCost(usage)
}

// fillUsageCostWithImageTool 先按主 model 定价算 token 成本，再写入图片分档 metadata。
func fillUsageCostWithImageTool(usage *sdk.Usage, numImages int, size string) {
	fillUsageCost(usage)
	if usage == nil || numImages <= 0 {
		return
	}
	price := imagePriceForSize(size)
	setUsageImageSize(usage, size)
	addImageCost(usage, numImages, price)
}

func addImageCost(usage *sdk.Usage, numImages int, pricePerImage float64) {
	if usage == nil || numImages <= 0 || pricePerImage <= 0 {
		return
	}
	addUsageMetadataInt(usage, usageMetricImages, numImages)
	setUsageMetadataFloat(usage, "openai.image.unit_price", pricePerImage)
	setUsageMetadata(usage, "openai.image.unit", "USD/image")
	recomputeUsageAccountCost(usage)
}
