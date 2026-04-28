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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway/imgen"
)

// decodeImageRefs 把 parseImagesRequest 返回的 data URL / http URL 字符串列表
// 转为 imgen.ImageInput 切片（内存中的原始二进制）。
// 目前只支持 data URL（base64 编码）；http(s) URL 会跳过并 log 警告。
func decodeImageRefs(refs []string) ([]imgen.ImageInput, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]imgen.ImageInput, 0, len(refs))
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "data:") {
			continue
		}
		// data:image/png;base64,iVBOR...
		commaIdx := strings.Index(ref, ",")
		if commaIdx < 0 {
			continue
		}
		header := ref[:commaIdx] // "data:image/png;base64"
		b64 := ref[commaIdx+1:]
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("base64 解码失败: %w", err)
			}
		}
		mime := ""
		if strings.HasPrefix(header, "data:") {
			mime = strings.TrimPrefix(header, "data:")
			if semi := strings.Index(mime, ";"); semi >= 0 {
				mime = mime[:semi]
			}
		}
		out = append(out, imgen.ImageInput{
			Data:     data,
			MimeType: mime,
		})
	}
	return out, nil
}

// imagesWebReverseModel 专用于触发 chatgpt.com 网页端逆向通道的 model id。
// 客户端在 /v1/images/generations 的 body 里把 "model" 设成这个值，OAuth 账号
// 就会走 forwardImagesViaWebReverse，绕开 Responses API + image_generation tool。
const imagesWebReverseModel = "gpt-image-2"

// isImagesWebReverseModel 判断请求是否显式指定了 gpt-image-2。
func isImagesWebReverseModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), imagesWebReverseModel)
}

// shouldUseImagesWebReverse 只允许 free OAuth 账号走网页端逆向生图。
func shouldUseImagesWebReverse(account *sdk.Account, model string) bool {
	if account == nil || account.Credentials["access_token"] == "" {
		return false
	}
	if !isImagesWebReverseModel(model) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(account.Credentials["plan_type"]), "free")
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
				writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
			}
			outcome := failureOutcome(status, nil, nil, err.Error(), parseRetryDelay(err.Error()))
			outcome.Duration = time.Since(start)
			if outcome.Usage == nil {
				outcome.Usage = &sdk.Usage{Model: imagesWebReverseModel}
			}
			return outcome, err
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

	respBody := buildWebReverseImagesResponse(imgRes, 0, 0)
	if sseKA != nil {
		sseKA.Stop()
		writeImagesRESTSSE(req.Writer, respBody)
	}

	elapsed := time.Since(start)
	usage := &sdk.Usage{
		Model:        imagesWebReverseModel,
		FirstTokenMs: elapsed.Milliseconds(),
	}
	// Web 逆向上游不返 size 字段，直接解码生成的 PNG header 拿真实宽高（O(1)）。
	// 解码失败 fallback 到请求 size（auto/空时 imagePriceForSize 兜底 1K）。
	billingSize := imgReq.Size
	if numImages > 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(imgRes.Images[0].Data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			billingSize = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
		}
	}
	// 透传到 usage_log.image_size 给 admin 后台展示（详见 forwardImagesViaResponsesTool 的注释）。
	usage.ImageSize = billingSize
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

// webReverseImagesError 把一个客户端错误（请求不合法 / 凭证缺失）打包为 ClientError Outcome，
// 并同步写响应给客户端。调用方应在命中 401/403 等账号级错误前单独处理——这里不归类到账号状态。
func webReverseImagesError(start time.Time, status int, w http.ResponseWriter, msg string) (sdk.ForwardOutcome, error) {
	body := buildImagesErrorBody(status, msg)
	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	outcome := sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: status,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		},
		Reason:   msg,
		Duration: time.Since(start),
	}
	return outcome, fmt.Errorf("%s", msg)
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
