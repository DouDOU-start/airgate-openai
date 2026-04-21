package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway/imgen"
)

// imagesWebReverseModel 专用于触发 chatgpt.com 网页端逆向通道的 model id。
// 客户端在 /v1/images/generations 的 body 里把 "model" 设成这个值，OAuth 账号
// 就会走 forwardImagesViaWebReverse，绕开 Responses API + image_generation tool。
const imagesWebReverseModel = "gpt-image-2"

// isImagesWebReverseModel 判断请求是否显式指定了 gpt-image-2。
//
// 对 OAuth 账号 + Images 请求：
//   - model == gpt-image-2 → 走 Web 逆向（本文件）
//   - 其它 / 为空 → 走 Responses tool（现有逻辑）
func isImagesWebReverseModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), imagesWebReverseModel)
}

// forwardImagesViaWebReverse 把一个 OpenAI Images REST 请求翻译成 imgen
// 调用链，再把 PNG 结果打包回 Images REST 响应（b64_json）。
//
// 支持的字段（/v1/images/generations）：
//   - prompt：必填
//   - n：忽略，chatgpt.com 一次只返回 1 张（灰度桶可能返回 2 张，都算终稿）
//   - size / quality / background / output_format：忽略，Web 逆向不支持这些参数
//
// /v1/images/edits 目前不支持（网页端需要 attach image 的入口，协议结构不同，
// 后续若要支持单独开一个 forwardImagesEditsViaWebReverse 入口）。
func (g *OpenAIGateway) forwardImagesViaWebReverse(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 只支持文生图
	_, reqPath := resolveAPIKeyRoute(req)
	if isImagesEditRequest(reqPath) {
		return webReverseImagesError(start, http.StatusBadRequest, req.Writer,
			"gpt-image-2 暂不支持 /v1/images/edits，请使用 /v1/images/generations")
	}

	// 解析 OpenAI Images REST 请求
	imgReq, err := parseImagesRequest(req.Body, req.Headers.Get("Content-Type"), false)
	if err != nil {
		return webReverseImagesError(start, http.StatusBadRequest, req.Writer,
			fmt.Sprintf("解析 Images 请求失败: %v", err))
	}

	// 构造 imgen Client（每次请求新建，bootstrap cookie 不跨请求复用）
	accessToken := account.Credentials["access_token"]
	if accessToken == "" {
		return webReverseImagesError(start, http.StatusUnauthorized, req.Writer,
			"OAuth 账号缺少 access_token")
	}
	var proxyURL *url.URL
	if account.ProxyURL != "" {
		if u, perr := url.Parse(account.ProxyURL); perr == nil {
			proxyURL = u
		}
	}
	client := imgen.NewClient(accessToken, proxyURL)

	// 调用生成
	imgRes, err := client.GenerateImage(ctx, imgReq.Prompt)
	if err != nil {
		// 无 partial images 时按 502 报错 + 允许 Core failover（account 不一定坏）
		if imgRes == nil || len(imgRes.Images) == 0 {
			status := classifyWebReverseError(err)
			return &sdk.ForwardResult{
				StatusCode:    status,
				Duration:      time.Since(start),
				Model:         imagesWebReverseModel,
				AccountStatus: accountStatusFromMessage(status, err.Error()),
				ErrorMessage:  err.Error(),
			}, err
		}
		// 有部分图片已下载成功，降级为成功响应（继续下游流程）
		g.logger.Warn("imgen 生成部分失败，使用已下载图片", "err", err, "count", len(imgRes.Images))
	}

	// 构造 Images REST 响应
	promptTokens := estimatePromptTokens(imgReq.Prompt)
	// 网页端无 size/quality，按 medium 1024x1024 估算每张 output tokens
	perImgOutTokens := lookupImageGenOutputTokens("1024x1024", "medium")
	outputTokens := perImgOutTokens * len(imgRes.Images)

	respBody := buildWebReverseImagesResponse(imgRes, promptTokens, outputTokens)

	// 写回客户端（非流式）
	if w := req.Writer; w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	}

	elapsed := time.Since(start)
	result := &sdk.ForwardResult{
		StatusCode:    http.StatusOK,
		InputTokens:   promptTokens,
		OutputTokens:  outputTokens,
		Model:         imagesWebReverseModel,
		Duration:      elapsed,
		FirstTokenMs:  elapsed.Milliseconds(),
		AccountStatus: sdk.AccountStatusOK,
	}
	if w := req.Writer; w == nil {
		// 非流式且无 writer：把 body 交给 Core 透传
		result.Body = respBody
		result.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	fillCost(result)
	return result, nil
}

// buildWebReverseImagesResponse 按 OpenAI Images API 官方契约打包 Web 逆向响应。
//
// 每张图：
//   - b64_json：PNG 二进制的 base64
//   - revised_prompt：网页端没有 revised_prompt 字段暴露给下游，这里留空
//   - model："gpt-image-2"
func buildWebReverseImagesResponse(res *imgen.Result, promptTokens, outputTokens int) []byte {
	data := make([]map[string]any, 0, len(res.Images))
	for _, img := range res.Images {
		data = append(data, map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(img.Data),
			"model":    imagesWebReverseModel,
		})
	}
	payload := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		// root 级 model 供 handleImagesResponse 或 Core 做费用查价
		"model": imagesWebReverseModel,
	}
	if promptTokens+outputTokens > 0 {
		payload["usage"] = map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": outputTokens,
			"total_tokens":  promptTokens + outputTokens,
			"input_tokens_details": map[string]any{
				"text_tokens":  promptTokens,
				"image_tokens": 0,
			},
		}
	}
	b, _ := json.Marshal(payload)
	return b
}

// webReverseImagesError 把一个错误打包成 ForwardResult，并同步写给客户端。
//
// 返回的 ForwardResult AccountStatus 默认 OK（= 4xx 全部视为客户端问题）。
// 401/403 调用方应在调用前单独处理（走 accountStatusFromMessage），不进这里。
func webReverseImagesError(start time.Time, status int, w http.ResponseWriter, msg string) (*sdk.ForwardResult, error) {
	body := buildImagesErrorBody(status, msg)
	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	result := &sdk.ForwardResult{
		StatusCode:    status,
		Duration:      time.Since(start),
		Model:         imagesWebReverseModel,
		ErrorMessage:  msg,
		AccountStatus: sdk.AccountStatusOK,
	}
	if w == nil {
		result.Body = body
		result.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	return result, fmt.Errorf("%s", msg)
}

// classifyWebReverseError 根据 err.Error() 文本判定 HTTP 状态码。
//
// imgen 底层的错误都是 fmt.Errorf("...HTTP %d: ...")，通过文本嗅探定位上游状态码。
// 匹配不到时统一归 502，让 Core 决定是否 failover。
func classifyWebReverseError(err error) int {
	if err == nil {
		return http.StatusBadGateway
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTP 401"), strings.Contains(msg, "access_token"):
		return http.StatusUnauthorized
	case strings.Contains(msg, "HTTP 403"):
		return http.StatusForbidden
	case strings.Contains(msg, "HTTP 429"):
		return http.StatusTooManyRequests
	case strings.Contains(msg, "PoW"), strings.Contains(msg, "触发风控"):
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}
