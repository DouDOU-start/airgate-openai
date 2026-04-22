package gateway

import (
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// 构造 ForwardOutcome 的小 helper，避免各路径散落一堆 struct literal。
//
// Success 必带 Usage，fillCost 会基于 Model / ServiceTier 补成本字段。
// ClientError / AccountRateLimited / AccountDead / UpstreamTransient 都从
// Upstream + 上游错误消息推出 Kind。

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
	return sdk.ForwardOutcome{
		Kind: kind,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Reason:     message,
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

// fillUsageCost 用主 model 定价填充 Usage 的成本字段。
//
// 使用 SDK.CalculateCost 统一处理 standard / priority / flex 三档和 gpt-5.4 家族的长上下文阶梯。
func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}
	spec := model.Lookup(usage.Model)
	modelInfo := sdk.ModelInfo{
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
	cost := sdk.CalculateCost(sdk.CostInput{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		ServiceTier:       usage.ServiceTier,
	}, modelInfo)
	usage.InputCost = cost.InputCost
	usage.OutputCost = cost.OutputCost
	usage.CachedInputCost = cost.CachedInputCost

	// 回填标准档单价用于 usage_log 展示（$/1M token）
	usage.InputPrice = spec.InputPrice
	usage.OutputPrice = spec.OutputPrice
	usage.CachedInputPrice = spec.CachedPrice
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
	usage.OutputCost = float64(numImages) * spec.ImagePrice
	usage.InputCost = 0
	usage.OutputPrice = spec.ImagePrice
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
	usage.OutputCost += float64(numImages) * imageSpec.ImagePrice
}
