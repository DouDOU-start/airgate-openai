package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"

	"github.com/DevilGenius/airgate-openai/backend/internal/gateway/imgen"
)

// decodeImageRefs 把 parseImagesRequest 返回的 data URL / http URL 字符串列表
// 转为 imgen.ImageInput 切片（内存中的原始二进制）。
// 目前只支持 data URL（base64 编码）；http(s) URL 会跳过并 log 警告。
func decodeImageRefs(refs []string) ([]imgen.ImageInput, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]imgen.ImageInput, 0, len(refs))
	client := &http.Client{Timeout: 30 * time.Second}
	for _, ref := range refs {
		if strings.HasPrefix(ref, "data:") {
			input, err := decodeDataImageRef(ref)
			if err != nil {
				return nil, err
			}
			out = append(out, input)
			continue
		}
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			input, err := downloadImageRef(client, ref)
			if err != nil {
				return nil, err
			}
			out = append(out, input)
		}
	}
	return out, nil
}

func decodeDataImageRef(ref string) (imgen.ImageInput, error) {
	commaIdx := strings.Index(ref, ",")
	if commaIdx < 0 {
		return imgen.ImageInput{}, fmt.Errorf("data URL 缺少 base64 数据")
	}
	header := ref[:commaIdx]
	b64 := ref[commaIdx+1:]
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return imgen.ImageInput{}, fmt.Errorf("base64 解码失败: %w", err)
		}
	}
	mimeType := strings.TrimPrefix(header, "data:")
	if semi := strings.Index(mimeType, ";"); semi >= 0 {
		mimeType = mimeType[:semi]
	}
	data, mimeType, err = shrinkImageBytes(data, mimeType, maxEditInputImageBytes)
	if err != nil {
		return imgen.ImageInput{}, fmt.Errorf("压缩参考图片失败: %w", err)
	}
	return imgen.ImageInput{Data: data, MimeType: mimeType}, nil
}

func downloadImageRef(client *http.Client, ref string) (imgen.ImageInput, error) {
	resp, err := client.Get(ref)
	if err != nil {
		return imgen.ImageInput{}, fmt.Errorf("下载参考图片失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return imgen.ImageInput{}, fmt.Errorf("下载参考图片返回 HTTP %d", resp.StatusCode)
	}
	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return imgen.ImageInput{}, fmt.Errorf("参考图片 Content-Type 不是 image/*: %s", contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes+1))
	if err != nil {
		return imgen.ImageInput{}, fmt.Errorf("读取参考图片失败: %w", err)
	}
	if len(data) > maxRemoteImageBytes {
		return imgen.ImageInput{}, fmt.Errorf("参考图片过大")
	}
	data, contentType, err = shrinkImageBytes(data, contentType, maxEditInputImageBytes)
	if err != nil {
		return imgen.ImageInput{}, fmt.Errorf("压缩参考图片失败: %w", err)
	}
	return imgen.ImageInput{Data: data, MimeType: contentType}, nil
}

// imagesWebReverseModel 专用于触发 chatgpt.com 网页端逆向通道的 model id。
// 客户端在 /v1/images/generations 的 body 里把 "model" 设成这个值，OAuth 账号
// 就会走 forwardImagesViaWebReverse，绕开 Responses API + image_generation tool。
const imagesWebReverseModel = "gpt-image-2"

// isImagesWebReverseModel 判断请求是否显式指定了 gpt-image-2。
// 现已没有 call site（shouldUseImagesWebReverse 恒 false），保留供网页端方案
// 再次启用时复用，避免重复实现 model 名归一化。
//
//nolint:unused
func isImagesWebReverseModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), imagesWebReverseModel)
}

// shouldUseImagesWebReverse 历史上用于把 free OAuth 账号生图请求重定向到
// chatgpt.com 网页端逆向通道。该通道协议结构与 /v1/images/edits 完全不一致，
// 对图生图/局部图的支持度也低于 Responses API + image_generation tool 路径，
// 容易在 studio 这类同时支持文生图/图生图/局部图的入口出现"链路看上去通了
// 但参考图没生效"的退化。
//
// 现已统一回 forwardImagesViaResponsesTool —— free OAuth 账号也走 Responses
// API。本函数保留并恒返回 false，对应的 forwardImagesViaWebReverse / imgen
// 子包不删除，后续若网页端逆向方案再次成熟，只需把判定打开即可。
func shouldUseImagesWebReverse(account *sdk.Account, model string) bool {
	_ = account
	_ = model
	return false
}

// forwardImagesViaWebReverse 把一个 OpenAI Images REST 请求翻译成 imgen
// 调用链，再把 PNG 结果打包回 Images REST 响应（b64_json）。
//
// 支持的字段（/v1/images/generations）：
//   - prompt：必填
//   - n：忽略，chatgpt.com 一次只返回 1 张（灰度桶可能返回 2 张，都算终稿）
//   - size：通过在 prompt 前注入尺寸提示引导模型输出对应分辨率
//   - quality / background / output_format：忽略，Web 逆向不支持这些参数
//
// /v1/images/edits 目前不支持（网页端需要 attach image 的入口，协议结构不同，
// 后续若要支持单独开一个 forwardImagesEditsViaWebReverse 入口）。
func (g *OpenAIGateway) forwardImagesViaWebReverse(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account

	_, reqPath := resolveAPIKeyRoute(req)
	isEdit := isImagesEditRequest(reqPath)

	imgReq, err := parseImagesRequest(req.Body, req.Headers.Get("Content-Type"), isEdit)
	if err != nil {
		return webReverseImagesError(start, http.StatusBadRequest, req.Writer,
			fmt.Sprintf("解析 Images 请求失败: %v", err))
	}
	// Web 逆向必然走 gpt-image-2（imagesWebReverseModel），统一启用严格 size 校验，
	// 提前挡住上游 chatgpt.com 必拒的请求，避免浪费一次 PoW + 30s 轮询。
	if err := validateImageSize(imgReq.Size, imagesWebReverseModel); err != nil {
		return webReverseImagesError(start, http.StatusBadRequest, req.Writer, err.Error())
	}
	g.logger.Debug("Images WebReverse request",
		"path", reqPath,
		"request_model", imgReq.Model,
		"is_edit", isEdit,
		"size", imgReq.Size,
		"n", imgReq.N,
	)

	inputEstimate := imagesInputTokenEstimate{TextTokens: estimatePromptTokens(imgReq.Prompt)}
	if isEdit {
		inputEstimate.ImageTokens = estimateGPTImageInputTokensForImages(imgReq.Images, imgReq.Size)
	}

	var imageInputs []imgen.ImageInput
	if isEdit && len(imgReq.Images) > 0 {
		imageInputs, err = decodeImageRefs(imgReq.Images)
		if err != nil {
			return webReverseImagesError(start, http.StatusBadRequest, req.Writer,
				fmt.Sprintf("解码参考图片失败: %v", err))
		}
	}

	accessToken := account.Credentials["access_token"]
	if accessToken == "" {
		return webReverseImagesError(start, http.StatusUnauthorized, req.Writer, "OAuth 账号缺少 access_token")
	}
	var proxyURL *url.URL
	if account.ProxyURL != "" {
		if u, perr := url.Parse(account.ProxyURL); perr == nil {
			proxyURL = u
		}
	}
	client := imgen.NewClient(accessToken, proxyURL)

	var sseKA *ssePingKeepAlive
	if req.Stream {
		sseKA = startSSEPingKeepAlive(req.Writer)
	}

	prompt := applyWebReverseSizeHint(imgReq.Prompt, imgReq.Size)
	imgRes, err := client.GenerateImage(ctx, prompt, imageInputs)
	if err != nil {
		if imgRes == nil || len(imgRes.Images) == 0 {
			status := classifyWebReverseError(err)
			if sseKA != nil {
				sseKA.Stop()
				g.logger.Warn("Images WebReverse 流式请求失败，已脱敏响应",
					"model", imgReq.Model, "status_code", status, "error", err)
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			outcome := failureOutcome(status, nil, nil, err.Error(), parseRetryDelay(err.Error()))
			outcome.Duration = time.Since(start)
			if outcome.Usage == nil {
				outcome.Usage = newTokenUsage(imagesWebReverseModel, "", 0, 0, 0, 0, 0)
			}
			return outcome, forwardErrForOutcome(outcome, err)
		}
		// 有部分图片已下载：降级为成功响应继续下游流程
		g.logger.Warn("imgen 生成部分失败，使用已下载图片", "err", err, "count", len(imgRes.Images))
	}

	numImages := len(imgRes.Images)
	g.logger.Debug("Images WebReverse result",
		"path", reqPath,
		"request_model", imgReq.Model,
		"model_slug", imgRes.ModelSlug,
		"conversation_id", imgRes.ConversationID,
		"num_images", numImages,
	)

	respBody := buildWebReverseImagesResponse(imgRes, inputEstimate.TextTokens, inputEstimate.ImageTokens, 0)
	if sseKA != nil {
		sseKA.Stop()
		writeImagesRESTSSE(req.Writer, respBody)
	}

	elapsed := time.Since(start)
	usage := newTokenUsage(imagesWebReverseModel, "", inputEstimate.Total(), 0, 0, 0, elapsed.Milliseconds())
	setUsageInputTokenDetails(usage, inputEstimate.TextTokens, inputEstimate.ImageTokens)
	// Web 逆向上游不返 size 字段，直接解码生成的 PNG header 拿真实宽高（O(1)）。
	// 解码失败 fallback 到请求 size（auto/空时 imagePriceForSize 兜底 1K）。
	billingSize := imgReq.Size
	if numImages > 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(imgRes.Images[0].Data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			billingSize = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
		}
	}
	// 图片尺寸写入 Usage 标准字段和 metadata，后台费用明细可用它解释分档。
	fillUsageCostPerImageBySize(usage, numImages, billingSize)

	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}
	if sseKA != nil {
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
	} else {
		outcome.Upstream.Body = respBody
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	return outcome, nil
}

// buildWebReverseImagesResponse 按 OpenAI Images API 官方契约打包 Web 逆向响应。
//
// 每张图：
//   - b64_json：PNG 二进制的 base64
//   - revised_prompt：网页端没有 revised_prompt 字段暴露给下游，这里留空
//   - model："gpt-image-2"
func buildWebReverseImagesResponse(res *imgen.Result, textInputTokens, imageInputTokens, outputTokens int) []byte {
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
	inputTokens := textInputTokens + imageInputTokens
	if inputTokens+outputTokens > 0 {
		payload["usage"] = map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  inputTokens + outputTokens,
			"input_tokens_details": map[string]any{
				"text_tokens":  textInputTokens,
				"image_tokens": imageInputTokens,
			},
		}
	}
	b, _ := json.Marshal(payload)
	return b
}

// webReverseImagesError 把 WebReverse 阶段的错误打包为 Outcome。
// 400 类客户端错误直接透传；账号 / 上游类错误保留 err，让 core 继续脱敏和 failover。
func webReverseImagesError(start time.Time, status int, _ http.ResponseWriter, msg string) (sdk.ForwardOutcome, error) {
	body := buildImagesErrorBody(status, msg)
	outcome := failureOutcome(status, body, http.Header{"Content-Type": []string{"application/json"}}, msg, parseRetryDelay(msg))
	outcome.Duration = time.Since(start)
	if outcome.Usage == nil {
		outcome.Usage = newTokenUsage(imagesWebReverseModel, "", 0, 0, 0, 0, 0)
	}
	return outcome, forwardErrForOutcome(outcome, fmt.Errorf("%s", msg))
}

const webReverseMaxImageEdge = 3840

func normalizeImageSizeForUpstream(size string) string {
	width, height, ok := parseImageSize(size)
	if !ok {
		return strings.TrimSpace(size)
	}
	width, height = clampImageSize(width, height, webReverseMaxImageEdge)
	return fmt.Sprintf("%dx%d", width, height)
}

func applyWebReverseSizeHint(prompt, size string) string {
	width, height, ok := parseImageSize(size)
	if !ok {
		return prompt
	}

	width, height = clampImageSize(width, height, webReverseMaxImageEdge)
	orientation := "square"
	if width > height {
		orientation = "landscape"
	} else if width < height {
		orientation = "portrait"
	}
	return fmt.Sprintf("Generate a %s image at %dx%d resolution. %s", orientation, width, height, prompt)
}

func parseImageSize(size string) (int, int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}

	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

func clampImageSize(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, scaleImageDimension(height, width, maxEdge)
	}
	return scaleImageDimension(width, height, maxEdge), maxEdge
}

func scaleImageDimension(value, oldEdge, newEdge int) int {
	return int((int64(value)*int64(newEdge) + int64(oldEdge)/2) / int64(oldEdge))
}

// classifyWebReverseError 根据 err.Error() 文本判定 HTTP 状态码。
//
// imgen 底层的错误都是 fmt.Errorf("...HTTP %d: ...")，通过文本嗅探定位上游状态码。
// 上游有时把 429 包成 502 + "rate limit reached" 文案下发，单看 "HTTP 502" 会误判，
// 所以默认分支再用 isTemporaryRateLimitText 兜一遍，命中限流关键词就归 429。
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
	case isTemporaryRateLimitText(msg):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}
