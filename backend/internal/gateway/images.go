package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// imagesOAuthChatModel OAuth 下 REST→tools 翻译时使用的主 chat 模型。
// Codex 官方 $imagegen 技能同样用 gpt-5.4 作为主 model，image_generation 作为
// tool 内部用 gpt-image-1.5。
const imagesOAuthChatModel = "gpt-5.4"

// imagesPassthroughInstructions 强制 gpt-5.4 只做"调工具"这一件事，不发挥创意。
// 原因：
//   - 客户端调 /v1/images/generations 期望的是 prompt 直达上游，而 Responses API
//     的调用链必须先过一个 chat 模型再触发 image_generation 工具；
//   - 如果给 gpt-5.4 用通用的 Codex/助理 instructions，它会把 prompt 扩写成一大段
//     "构图/灯光/配色/风格"的创意导演描述（revised_prompt 明显变长），用户体感就是
//     "prompt 被改了"。
//   - 这里用极简指令把 gpt-5.4 的角色压缩成"透传路由"，只会补合规改写（如真实人物
//     换成匿名替身，这是 OpenAI 侧硬编码的安全策略，instruction 拦不住）。
const imagesPassthroughInstructions = "Use the user's image description and appended Image API constraints as the image_generation prompt. Preserve the user's original description verbatim; do not rewrite, elaborate, or add details about style, composition, lighting, color, mood, or any elements the user did not explicitly request. Treat Image API constraints as generation parameters, not visible text. Do not answer with text."

const maxResponsesInputImageBytes = 2 * 1024 * 1024
const maxRemoteImageBytes = 25 * 1024 * 1024

const sanitizedImageSSEErrorMessage = "请求暂时无法完成，请稍后重试"

// 历史变量名保留，但实际对外提示保持统一的重试文案，不再暗示用户压缩图片。
const imageTooLargeSSEErrorMessage = sanitizedImageSSEErrorMessage

// maxEditInputImageBytes 图生图（/images/edits）参考图单张上限。
// API Key 和 Web Reverse 路径在转发前把超限图片压缩到此阈值以内，
// 避免多张大图导致上游超时或拒绝。
const maxEditInputImageBytes = 4 * 1024 * 1024

// imageGenOutputTokenTable 按 OpenAI 官方 image_generation tool 的 token 换算表，
// 用于 ChatGPT OAuth 账号（上游 tool_usage.image_gen 永远为 0）时估算 output token。
// 数值引自 OpenAI Images pricing 文档的 "Image output tokens per image" 一栏。
//
//	quality    1024×1024   1024×1536 / 1536×1024
//	low           272              408
//	medium       1056             1584
//	high         4160             6240
var imageGenOutputTokenTable = map[string]map[string]int{
	"1024x1024": {"low": 272, "medium": 1056, "high": 4160},
	"1024x1536": {"low": 408, "medium": 1584, "high": 6240},
	"1536x1024": {"low": 408, "medium": 1584, "high": 6240},
}

// lookupImageGenOutputTokens 根据 size / quality 估算单张图像的 output token 数。
// quality="auto" / "" 统一当 medium 处理（与 OpenAI 默认一致）。
// 未知 size 回退到 1024×1024 medium（1056）。
func lookupImageGenOutputTokens(size, quality string) int {
	q := strings.ToLower(strings.TrimSpace(quality))
	if q == "" || q == "auto" {
		q = "medium"
	}
	s := strings.ToLower(strings.TrimSpace(size))
	if row, ok := imageGenOutputTokenTable[s]; ok {
		if v, ok := row[q]; ok {
			return v
		}
		return row["medium"]
	}
	return 1056
}

func normalizeImageQualityDefaultMedium(quality string) string {
	q := strings.ToLower(strings.TrimSpace(quality))
	if q == "" || strings.EqualFold(q, "auto") {
		return "medium"
	}
	return q
}

func calculateGPTImage2OutputTokensForImages(modelName, size, quality string, numImages int) int {
	if numImages <= 0 || !isGPTImage2Model(modelName) {
		return 0
	}
	if _, _, ok := parseImageSize(size); !ok {
		size = "1024x1024"
	}
	tokens, err := NewGPTImage2TokenCalculator().Calculate(size, normalizeImageQualityDefaultMedium(quality))
	if err != nil {
		return 0
	}
	return tokens * numImages
}

func estimateGPTImage2InputImageTokens(refs []string, fallbackSize string) int {
	if len(refs) == 0 {
		return 0
	}
	calc := NewGPTImage2TokenCalculator()
	fallbackTokens := -1
	total := 0
	for _, ref := range refs {
		if width, height, ok := imageDimensionsFromRef(ref); ok {
			if tokens, err := calc.CalculateDimensionsDefaultQuality(width, height); err == nil {
				total += tokens
				continue
			}
		}
		if fallbackTokens < 0 {
			fallbackTokens = gptImage2InputImageFallbackTokens(calc, fallbackSize)
		}
		total += fallbackTokens
	}
	return total
}

func imageDimensionsFromRef(ref string) (int, int, bool) {
	if !strings.HasPrefix(ref, "data:") {
		return 0, 0, false
	}
	return imageDimensionsFromDataURL(ref)
}

func imageDimensionsFromDataURL(ref string) (int, int, bool) {
	comma := strings.IndexByte(ref, ',')
	if comma < 0 || !strings.HasPrefix(strings.ToLower(ref[:comma]), "data:image/") {
		return 0, 0, false
	}
	payload := ref[comma+1:]
	if width, height, ok := imageDimensionsFromBase64Reader(base64.StdEncoding, payload); ok {
		return width, height, true
	}
	return imageDimensionsFromBase64Reader(base64.RawStdEncoding, payload)
}

func imageDimensionsFromBase64Reader(enc *base64.Encoding, payload string) (int, int, bool) {
	cfg, _, err := image.DecodeConfig(base64.NewDecoder(enc, strings.NewReader(payload)))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

func imageDimensionsFromBytes(data []byte) (int, int, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

func gptImage2InputImageFallbackTokens(calc GPTImage2TokenCalculator, fallbackSize string) int {
	size := normalizeImagesResponseSize(fallbackSize)
	if size == "" {
		size = "1024x1024"
	}
	tokens, _ := calc.CalculateDefaultQuality(size)
	return tokens
}

// estimateImageGenOutputTokens 汇总所有 image_generation_call 的估算 token 数。
func estimateImageGenOutputTokens(calls []ImageGenCall) int {
	total := 0
	for _, c := range calls {
		total += lookupImageGenOutputTokens(c.Size, c.Quality)
	}
	return total
}

// isImagesRequest 判断给定路径是否为 Images API 请求（含文生图与图生图）。
// 用于在 forwardAPIKey 响应解析阶段分流到 handleImagesResponse，
// 以及在 forwardHTTP 入口守卫 OAuth 账号分流到 forwardImagesViaResponsesTool。
func isImagesRequest(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/generations") ||
		strings.HasSuffix(reqPath, "/images/edits")
}

// isImagesEditRequest 判断是否为 /images/edits（图生图）请求。
// 图生图与文生图的主要差异：用户消息里需要附带一张或多张 input_image，
// 以及可选的 inpainting 掩膜（input_image_mask）。
func isImagesEditRequest(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/edits")
}

// imagesSilentHandler 在 REST→tools 翻译路径下作为 WSEventHandler，只用来
// 记录首 token 与速率限制，不往客户端写任何 SSE 内容（因为我们最后要以
// Images REST JSON 形式一次性写给客户端）。
type imagesSilentHandler struct {
	accountID      int64
	start          time.Time
	firstTokenMs   int64
	firstTokenOnce sync.Once
}

func (h *imagesSilentHandler) OnTextDelta(string)      {}
func (h *imagesSilentHandler) OnReasoningDelta(string) {}
func (h *imagesSilentHandler) OnRateLimits(used float64) {
	if h.accountID > 0 {
		StoreCodexUsage(h.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: used,
			CapturedAt:         time.Now(),
		})
	}
}
func (h *imagesSilentHandler) OnRawEvent(eventType string, _ []byte) {
	if eventType == "" {
		return
	}
	h.firstTokenOnce.Do(func() {
		h.firstTokenMs = time.Since(h.start).Milliseconds()
	})
}

// estimatePromptTokens 对用户 prompt 做粗略 token 估算。
// 不引入 tokenizer 依赖：按 rune 数 / 3 向上取整，中英混合 prompt 都接近 OpenAI
// 实际分词数量级。gpt-image-1.5 input 单价 $5/1M，即便误差 50% 每千字也只有
// 几分钱差异，对总价（图像 output 主导）影响可忽略。
func estimatePromptTokens(prompt string) int {
	runes := len([]rune(prompt))
	if runes == 0 {
		return 0
	}
	return (runes + 2) / 3
}

func imageGenerationBillingModel(responseModel, requestModel string) string {
	for _, candidate := range []string{responseModel, requestModel} {
		m := strings.TrimSpace(candidate)
		if strings.HasPrefix(strings.ToLower(m), "gpt-image-") {
			return m
		}
	}
	return imageToolCostModel
}

// estimateImageCountFromTokens 从 image output token 数反推生成的图片张数。
// 用于 API Key 直通路径，该路径没有显式的图片计数。
// 1024×1024 medium ≈ 1056 tokens/张，取 1000 做除数向上取整。
func estimateImageCountFromTokens(outputTokens int) int {
	if outputTokens <= 0 {
		return 0
	}
	return (outputTokens + 999) / 1000
}

// imagesRequest 归一化后的 Images API 请求（同时承载 /generations 与 /edits）。
// /generations 只需要 Prompt；/edits 额外携带 Images（参考图）与可选 Mask（inpainting 掩膜）。
type imagesRequest struct {
	IsEdit        bool
	Prompt        string
	Model         string
	N             int
	Size          string
	Quality       string
	Background    string
	OutputFormat  string
	InputFidelity string   // 仅 /edits：控制参考图还原度 low/high
	Images        []string // 每项是 data:image/*;base64,... 或 http(s) URL
	Mask          string   // inpainting 掩膜，形式同 Images
}

// parseImagesRequest 把原始请求体按内容类型解析成 imagesRequest。
// /generations 只支持 application/json；/edits 同时支持 JSON（image 字段是 data URL/http(s) URL/数组）
// 与 multipart/form-data（OpenAI SDK 标准）。
func buildAPIKeyImagesEditMultipartBody(body []byte, contentType string) ([]byte, string, error) {
	multipartBody, multipartContentType, _, err := buildAPIKeyImagesEditMultipartBodyWithRequest(body, contentType)
	return multipartBody, multipartContentType, err
}

func buildAPIKeyImagesEditMultipartBodyWithRequest(body []byte, contentType string) ([]byte, string, *imagesRequest, error) {
	req, err := parseImagesRequest(body, contentType, true)
	if err != nil {
		return nil, "", nil, err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"model":          req.Model,
		"prompt":         req.Prompt,
		"size":           req.Size,
		"quality":        req.Quality,
		"background":     req.Background,
		"output_format":  req.OutputFormat,
		"input_fidelity": req.InputFidelity,
	}
	for name, value := range fields {
		if value != "" {
			_ = mw.WriteField(name, value)
		}
	}
	if req.N > 0 {
		_ = mw.WriteField("n", strconv.Itoa(req.N))
	}
	for i, ref := range req.Images {
		fieldName := "image"
		if len(req.Images) > 1 {
			fieldName = "image[]"
		}
		if err := writeMultipartImageRef(mw, fieldName, fmt.Sprintf("image-%d", i+1), ref, maxEditInputImageBytes); err != nil {
			_ = mw.Close()
			return nil, "", nil, err
		}
	}
	if req.Mask != "" {
		// mask 不压缩：透明度信息不能转 JPEG
		if err := writeMultipartImageRef(mw, "mask", "mask", req.Mask, 0); err != nil {
			_ = mw.Close()
			return nil, "", nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", nil, err
	}
	return buf.Bytes(), mw.FormDataContentType(), req, nil
}

func writeMultipartImageRef(mw *multipart.Writer, fieldName, baseName, ref string, shrinkLimit int) error {
	mimeType, data, err := readImageRefBytes(ref, shrinkLimit)
	if err != nil {
		return err
	}
	ext := ".png"
	switch mimeType {
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, baseName+ext))
	header.Set("Content-Type", mimeType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

func readImageRefBytes(ref string, shrinkLimit int) (string, []byte, error) {
	switch {
	case strings.HasPrefix(ref, "data:"):
		mimeType, data, err := decodeDataImageURL(ref)
		if err != nil {
			return "", nil, err
		}
		if shrinkLimit > 0 {
			data, mimeType, err = shrinkImageBytes(data, mimeType, shrinkLimit)
			if err != nil {
				return "", nil, err
			}
		}
		return mimeType, data, nil
	case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
		data, mimeType, err := downloadImageBytes(ref)
		if err != nil {
			return "", nil, err
		}
		if shrinkLimit > 0 {
			data, mimeType, err = shrinkImageBytes(data, mimeType, shrinkLimit)
			if err != nil {
				return "", nil, err
			}
		}
		return mimeType, data, nil
	default:
		return "", nil, fmt.Errorf("image 必须是 data URL 或 http(s) URL")
	}
}

func downloadImageBytes(ref string) ([]byte, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ref, nil)
	if err != nil {
		return nil, "", fmt.Errorf("构建图片下载请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载图片失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("下载图片返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取图片失败: %w", err)
	}
	if len(data) > maxRemoteImageBytes {
		return nil, "", fmt.Errorf("图片过大")
	}
	mimeType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mimeType == "" || !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return nil, "", fmt.Errorf("图片 Content-Type 不是 image/*: %s", mimeType)
	}
	return data, strings.ToLower(mimeType), nil
}

func decodeDataImageURL(ref string) (string, []byte, error) {
	comma := strings.IndexByte(ref, ',')
	if comma < 0 || !strings.HasPrefix(strings.ToLower(ref[:comma]), "data:image/") {
		return "", nil, fmt.Errorf("API key /v1/images/edits 仅支持 data URL 图片")
	}
	mimeType := ref[len("data:"):comma]
	if semi := strings.IndexByte(mimeType, ';'); semi >= 0 {
		mimeType = mimeType[:semi]
	}
	data, err := base64.StdEncoding.DecodeString(ref[comma+1:])
	if err != nil {
		return "", nil, fmt.Errorf("图片 base64 解码失败: %w", err)
	}
	return strings.ToLower(mimeType), data, nil
}

func parseImagesRequest(body []byte, contentType string, isEdit bool) (*imagesRequest, error) {
	if isEdit {
		// 去掉 Content-Type 参数只保留主类型
		ct := strings.ToLower(strings.TrimSpace(contentType))
		if semi := strings.Index(ct, ";"); semi >= 0 {
			ct = strings.TrimSpace(ct[:semi])
		}
		if ct == "multipart/form-data" {
			return parseImagesEditMultipart(body, contentType)
		}
	}
	return parseImagesJSON(body, isEdit)
}

func isMultipartContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/")
}

func imagesResponseOptionsFromRequestBody(body []byte, contentType string, isEdit bool) imagesResponseOptions {
	if len(body) == 0 {
		return imagesResponseOptions{}
	}
	if isEdit && isMultipartContentType(contentType) {
		return imagesResponseOptionsFromMultipartBody(body, contentType)
	}
	size := strings.TrimSpace(gjson.GetBytes(body, "size").String())
	opts := imagesResponseOptions{
		BillingSize:         size,
		RequestSize:         size,
		RequestQuality:      normalizeImageQualityDefaultMedium(gjson.GetBytes(body, "quality").String()),
		RequestOutputFormat: strings.TrimSpace(gjson.GetBytes(body, "output_format").String()),
	}
	if req, err := parseImagesRequest(body, contentType, isEdit); err == nil {
		applyImagesRequestOptions(&opts, req)
	} else if prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String()); prompt != "" {
		opts.RequestTextInputTokens = estimatePromptTokens(prompt)
	}
	return opts
}

func imagesResponseOptionsFromMultipartBody(body []byte, contentType string) imagesResponseOptions {
	opts := imagesResponseOptions{}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return opts
	}

	calc := NewGPTImage2TokenCalculator()
	unknownImageRefs := 0
	fields := map[string]string{}
	marker := []byte("--" + params["boundary"])
	pos := 0
	for {
		startRel := bytes.Index(body[pos:], marker)
		if startRel < 0 {
			break
		}
		partStart := pos + startRel + len(marker)
		if partStart+2 <= len(body) && body[partStart] == '-' && body[partStart+1] == '-' {
			break
		}
		if partStart+2 <= len(body) && body[partStart] == '\r' && body[partStart+1] == '\n' {
			partStart += 2
		} else if partStart < len(body) && body[partStart] == '\n' {
			partStart++
		}

		headerEndRel, sepLen := multipartHeaderEnd(body[partStart:])
		if headerEndRel < 0 {
			break
		}
		contentStart := partStart + headerEndRel + sepLen
		nextRel := bytes.Index(body[contentStart:], marker)
		if nextRel < 0 {
			break
		}
		contentEnd := contentStart + nextRel
		headers := body[partStart : partStart+headerEndRel]
		content := body[contentStart:contentEnd]
		name := multipartPartName(headers)
		switch name {
		case "prompt", "size", "quality", "output_format":
			fields[name] = strings.TrimSpace(string(content))
		case "image", "image[]":
			contentType := strings.ToLower(strings.TrimSpace(strings.Split(multipartPartContentType(headers), ";")[0]))
			switch {
			case strings.HasPrefix(contentType, "image/"):
				if width, height, ok := imageDimensionsFromBytes(content); ok {
					if tokens, err := calc.CalculateDimensionsDefaultQuality(width, height); err == nil {
						opts.RequestImageInputTokens += tokens
					}
				}
			case multipartPartFilename(headers) == "" && len(bytes.TrimSpace(content)) > 0:
				unknownImageRefs++
			}
		}
		pos = contentEnd
	}

	size := strings.TrimSpace(fields["size"])
	opts.BillingSize = size
	opts.RequestSize = size
	opts.RequestQuality = normalizeImageQualityDefaultMedium(fields["quality"])
	opts.RequestOutputFormat = strings.TrimSpace(fields["output_format"])
	opts.RequestTextInputTokens = estimatePromptTokens(fields["prompt"])
	if unknownImageRefs > 0 {
		opts.RequestImageInputTokens += unknownImageRefs * gptImage2InputImageFallbackTokens(calc, size)
	}
	return opts
}

func applyImagesRequestOptions(opts *imagesResponseOptions, req *imagesRequest) {
	if opts == nil || req == nil {
		return
	}
	size := strings.TrimSpace(req.Size)
	opts.BillingSize = size
	opts.RequestSize = size
	opts.RequestQuality = normalizeImageQualityDefaultMedium(req.Quality)
	opts.RequestOutputFormat = strings.TrimSpace(req.OutputFormat)
	opts.RequestTextInputTokens = estimatePromptTokens(req.Prompt)
	if req.IsEdit {
		opts.RequestImageInputTokens = estimateGPTImage2InputImageTokens(req.Images, req.Size)
	}
}

func extractMultipartScalarFields(body []byte, contentType string, names ...string) map[string]string {
	out := make(map[string]string, len(names))
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return out
	}
	marker := []byte("--" + params["boundary"])
	pos := 0
	for len(out) < len(wanted) {
		startRel := bytes.Index(body[pos:], marker)
		if startRel < 0 {
			break
		}
		partStart := pos + startRel + len(marker)
		if partStart+2 <= len(body) && body[partStart] == '-' && body[partStart+1] == '-' {
			break
		}
		if partStart+2 <= len(body) && body[partStart] == '\r' && body[partStart+1] == '\n' {
			partStart += 2
		} else if partStart < len(body) && body[partStart] == '\n' {
			partStart++
		}

		headerEndRel, sepLen := multipartHeaderEnd(body[partStart:])
		if headerEndRel < 0 {
			break
		}
		contentStart := partStart + headerEndRel + sepLen
		nextRel := bytes.Index(body[contentStart:], marker)
		if nextRel < 0 {
			break
		}
		contentEnd := contentStart + nextRel
		name := multipartPartName(body[partStart : partStart+headerEndRel])
		if _, ok := wanted[name]; ok {
			out[name] = strings.TrimSpace(string(body[contentStart:contentEnd]))
		}
		pos = contentEnd
	}
	return out
}

func multipartHeaderEnd(body []byte) (int, int) {
	if idx := bytes.Index(body, []byte("\r\n\r\n")); idx >= 0 {
		return idx, 4
	}
	if idx := bytes.Index(body, []byte("\n\n")); idx >= 0 {
		return idx, 2
	}
	return -1, 0
}

func multipartPartName(headers []byte) string {
	return multipartPartDispositionParam(headers, "name")
}

func multipartPartFilename(headers []byte) string {
	return multipartPartDispositionParam(headers, "filename")
}

func multipartPartDispositionParam(headers []byte, paramName string) string {
	for _, line := range strings.Split(string(headers), "\n") {
		line = strings.TrimRight(line, "\r")
		colon := strings.IndexByte(line, ':')
		if colon < 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), "Content-Disposition") {
			continue
		}
		_, params, err := mime.ParseMediaType(strings.TrimSpace(line[colon+1:]))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(params[paramName])
	}
	return ""
}

func multipartPartContentType(headers []byte) string {
	for _, line := range strings.Split(string(headers), "\n") {
		line = strings.TrimRight(line, "\r")
		colon := strings.IndexByte(line, ':')
		if colon < 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), "Content-Type") {
			continue
		}
		return strings.TrimSpace(line[colon+1:])
	}
	return ""
}

func parseImagesJSON(body []byte, isEdit bool) (*imagesRequest, error) {
	prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	n := int(gjson.GetBytes(body, "n").Int())
	if n <= 0 {
		n = 1
	}
	req := &imagesRequest{
		IsEdit:        isEdit,
		Prompt:        prompt,
		Model:         strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		N:             n,
		Size:          strings.TrimSpace(gjson.GetBytes(body, "size").String()),
		Quality:       strings.TrimSpace(gjson.GetBytes(body, "quality").String()),
		Background:    strings.TrimSpace(gjson.GetBytes(body, "background").String()),
		OutputFormat:  strings.TrimSpace(gjson.GetBytes(body, "output_format").String()),
		InputFidelity: strings.TrimSpace(gjson.GetBytes(body, "input_fidelity").String()),
	}
	if !isEdit {
		return req, nil
	}
	// /edits：image 可以是字符串（单图）或字符串数组（多图）
	imgNode := gjson.GetBytes(body, "image")
	if imgNode.IsArray() {
		for _, item := range imgNode.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				imageRef, err := normalizeImageRef(s)
				if err != nil {
					return nil, err
				}
				req.Images = append(req.Images, imageRef)
			}
		}
	} else if s := strings.TrimSpace(imgNode.String()); s != "" {
		imageRef, err := normalizeImageRef(s)
		if err != nil {
			return nil, err
		}
		req.Images = append(req.Images, imageRef)
	}
	if mask := strings.TrimSpace(gjson.GetBytes(body, "mask").String()); mask != "" {
		maskRef, err := normalizeImageRef(mask)
		if err != nil {
			return nil, err
		}
		req.Mask = maskRef
	}
	if len(req.Images) == 0 {
		return nil, fmt.Errorf("/v1/images/edits 需要至少一张 image")
	}
	return req, nil
}

func parseImagesEditMultipart(body []byte, contentType string) (*imagesRequest, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("multipart content-type 解析失败: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("multipart content-type 缺少 boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	req := &imagesRequest{IsEdit: true, N: 1}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("multipart 读取失败: %w", err)
		}
		name := part.FormName()
		ctype := part.Header.Get("Content-Type")
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取 multipart part %q 失败: %w", name, readErr)
		}
		text := strings.TrimSpace(string(data))
		switch name {
		case "prompt":
			req.Prompt = text
		case "model":
			req.Model = text
		case "n":
			if v, convErr := strconv.Atoi(text); convErr == nil && v > 0 {
				req.N = v
			}
		case "size":
			req.Size = text
		case "quality":
			req.Quality = text
		case "background":
			req.Background = text
		case "output_format":
			req.OutputFormat = text
		case "input_fidelity":
			req.InputFidelity = text
		case "image", "image[]":
			imageRef, refErr := multipartImageRef(ctype, data, text)
			if refErr != nil {
				return nil, refErr
			}
			req.Images = append(req.Images, imageRef)
		case "mask":
			maskRef, refErr := multipartImageRef(ctype, data, text)
			if refErr != nil {
				return nil, refErr
			}
			req.Mask = maskRef
		}
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	if len(req.Images) == 0 {
		return nil, fmt.Errorf("/v1/images/edits 需要至少一张 image")
	}
	return req, nil
}

func multipartImageRef(contentType string, data []byte, text string) (string, error) {
	mainType := strings.TrimSpace(strings.Split(strings.ToLower(contentType), ";")[0])
	if strings.HasPrefix(mainType, "image/") {
		return "data:" + mainType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	if mainType == "" || strings.HasPrefix(mainType, "text/") {
		return normalizeImageRef(text)
	}
	return "", fmt.Errorf("不支持的 multipart 图片 Content-Type: %s", contentType)
}

func normalizeImageRef(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s, nil
	}
	if strings.HasPrefix(s, "data:") {
		return normalizeImageDataURL(s), nil
	}
	return "", fmt.Errorf("image 必须是 data URL 或 http(s) URL")
}

func normalizeImageDataURL(s string) string {
	comma := strings.IndexByte(s, ',')
	if comma < 0 || !strings.Contains(strings.ToLower(s[:comma]), ";base64") {
		return s
	}
	return s[:comma+1] + normalizeBase64ImageData(s[comma+1:])
}

func normalizeBase64ImageData(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	if rem := len(s) % 4; rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	return s
}

func normalizedImageOutputFormat(format string) string {
	f := strings.ToLower(strings.TrimSpace(format))
	if f == "" {
		return "png"
	}
	return f
}

func buildImagesToolPrompt(req *imagesRequest, isEdit, hasRegionAnnotation bool) string {
	lines := imageAPIConstraintLines(req, isEdit, hasRegionAnnotation)
	if len(lines) == 0 {
		return req.Prompt
	}
	var b strings.Builder
	b.WriteString(req.Prompt)
	b.WriteString("\n\nImage API constraints:\n")
	for _, line := range lines {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func buildImagesToolInstructions(req *imagesRequest, isEdit, hasRegionAnnotation bool) string {
	lines := imageAPIConstraintLines(req, isEdit, hasRegionAnnotation)
	if len(lines) == 0 {
		return imagesPassthroughInstructions
	}
	return imagesPassthroughInstructions + "\nHonor these Image API constraints when invoking image_generation: " + strings.Join(lines, " ")
}

func imageAPIConstraintLines(req *imagesRequest, isEdit, hasRegionAnnotation bool) []string {
	if req == nil {
		return nil
	}
	lines := make([]string, 0, 6)
	if size := cleanImageConstraintValue(normalizeImageSizeForUpstream(req.Size)); size != "" {
		lines = append(lines, "Generate the image at "+size+" pixels.")
	}
	if quality := cleanImageConstraintValue(req.Quality); quality != "" {
		lines = append(lines, "Use image quality setting "+quality+".")
	}
	if background := cleanImageConstraintValue(req.Background); background != "" {
		lines = append(lines, "Use background setting "+background+".")
	}
	if model := cleanImageConstraintValue(req.Model); strings.HasPrefix(strings.ToLower(model), "gpt-image-") {
		lines = append(lines, "Use requested image model "+model+".")
	}
	if isEdit {
		lines = append(lines, "Image 1 is the edit target; preserve its framing, identity, geometry, lighting, and all unrequested details.")
		// gpt-image-2 始终用 high fidelity 处理输入图，input_fidelity 是 no-op；
		// 写进 constraints 反而是噪声，可能让 chat 模型输出无效约束词。
		// SKILL.md: "gpt-image-2 always uses high fidelity for image inputs;
		// do not set input_fidelity with this model."
		if !isGPTImage2Model(req.Model) {
			if fidelity := cleanImageConstraintValue(req.InputFidelity); fidelity != "" {
				lines = append(lines, "Preserve the input image with "+fidelity+" fidelity.")
			}
		}
		if hasRegionAnnotation {
			lines = append(lines, "Image 2 is a region annotation derived from the edit mask; change only the red marked area in Image 1 and keep everything outside that region unchanged.")
		}
	}
	return lines
}

// isGPTImage2Model 判断 model 是否走 gpt-image-2 链路。
// 空 model 视为 gpt-image-2，因为客户端不指定时上游默认升到 gpt-image-2。
func isGPTImage2Model(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return true
	}
	return strings.HasPrefix(m, "gpt-image-2")
}

// imageActualSizeFromBase64 从图像 base64 数据里解码 header 拿实际宽高。
// 用于 auto 计费场景兜底——客户端选 auto 时上游 image_generation_call event
// 经常不返 size 字段，falling back 到 imgReq.Size="auto" 会按 1K 兜底，
// 但上游可能实际出 1024² 也可能 1536×1024 也可能更大，估算不准。
//
// image.DecodeConfig 只读 PNG/JPEG 头部，O(1) 时间，不耗 CPU 解整张图。
// 失败（base64 异常 / 非 PNG/JPEG / WebP 没注册解码器）返回 ok=false，
// 调用方继续用 fallback 链。
func imageActualSizeFromBase64(b64 string) (string, bool) {
	if b64 == "" {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return "", false
		}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", false
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", false
	}
	return fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), true
}

// imagePriceForSize 把生成的 size 映射成 USD 单价（1K/2K/4K 三档）。
// 阈值按最长边：≤1536=1K, ≤2048=2K, 否则 4K。当前固定档位价格：
//
//	1K  → $0.10/张  (1024×1024 / 1536×1024 / 1024×1536)
//	2K  → $0.20/张  (2048×2048 / 2048×1152 / 1152×2048)
//	4K  → $0.40/张  (3840×2160 / 2160×3840)
//
// size = "auto" 或为空（上游用默认 1024²）按 1K 计费。
// 跟 plugin.yaml 里 ImagePrice 字段解耦——OAuth → image_generation tool 路径
// 走这一档分档表，不读 spec.ImagePrice。
func imagePriceForSize(size string) float64 {
	width, height, ok := parseImageSize(size)
	if !ok {
		return 0.10
	}
	longest := width
	if height > longest {
		longest = height
	}
	switch {
	case longest <= 1536:
		return 0.10
	case longest <= 2048:
		return 0.20
	default:
		return 0.40
	}
}

// validateImageSize 在 OAuth → image_generation tool 路径上预校验 size，
// 把上游会拒的请求挡在 OAuth/PoW/SSE 整套链路之前，省一次配额 + 30s 延迟。
//
// 校验范围限定 gpt-image-2（含空 model，默认走 2）；显式 gpt-image-1 / 1.5
// 跳过：DALL-E 系列约束完全不同（只接受固定 size 列表），现有 normalize+clamp
// 已经够用。
//
// 硬约束（gpt-image-2 SKILL.md）：
//   - size = "" 或 "auto" → 允许（由上游决定）
//   - 边长 ≤ 3840px（已有 clamp 兜底，此处仍校验是为了配 16 倍数检查）
//   - 两边都是 16 的倍数
//   - 长短边比例 ≤ 3:1
//   - 总像素 ∈ [655360, 8294400]
func validateImageSize(size, model string) error {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" || s == "auto" {
		return nil
	}
	if !isGPTImage2Model(model) {
		return nil
	}
	width, height, ok := parseImageSize(s)
	if !ok {
		return fmt.Errorf("size 格式无效，应为 WIDTHxHEIGHT 或 auto")
	}
	if width > 3840 || height > 3840 {
		return fmt.Errorf("size 边长超过 3840px (%dx%d)", width, height)
	}
	if width%16 != 0 || height%16 != 0 {
		return fmt.Errorf("size 两边必须是 16 的倍数 (%dx%d)", width, height)
	}
	long, short := width, height
	if short > long {
		long, short = height, width
	}
	if long > short*3 {
		return fmt.Errorf("size 长短边比例不能超过 3:1 (%dx%d)", width, height)
	}
	total := width * height
	if total < 655360 {
		return fmt.Errorf("size 总像素数不能少于 655360 (%dx%d=%d)", width, height, total)
	}
	if total > 8294400 {
		return fmt.Errorf("size 总像素数不能超过 8294400 (%dx%d=%d)", width, height, total)
	}
	return nil
}

func cleanImageConstraintValue(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func buildEditRegionAnnotation(req *imagesRequest) (string, error) {
	if req == nil || req.Mask == "" || len(req.Images) == 0 {
		return "", nil
	}
	base, err := decodeImageRefImage(req.Images[0])
	if err != nil {
		return "", fmt.Errorf("解码编辑目标图片失败: %w", err)
	}
	mask, err := decodeImageRefImage(req.Mask)
	if err != nil {
		return "", fmt.Errorf("解码编辑 mask 失败: %w", err)
	}
	if base == nil {
		base = whiteCanvas(mask.Bounds().Dx(), mask.Bounds().Dy())
	}
	annotation, ok := renderMaskRegionAnnotation(base, mask)
	if !ok {
		return "", nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, annotation); err != nil {
		return "", fmt.Errorf("编码编辑区域标注图失败: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func whiteCanvas(width, height int) image.Image {
	if width <= 0 || height <= 0 {
		width, height = 1024, 1024
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	return img
}

func renderMaskRegionAnnotation(base image.Image, mask image.Image) (*image.RGBA, bool) {
	baseBounds := base.Bounds()
	width, height := baseBounds.Dx(), baseBounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, false
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(out, out.Bounds(), base, baseBounds.Min, draw.Src)

	maskBounds := mask.Bounds()
	overlay := color.RGBA{R: 255, A: 95}
	marked := false
	for y := 0; y < height; y++ {
		my := maskBounds.Min.Y + y*maskBounds.Dy()/height
		for x := 0; x < width; x++ {
			mx := maskBounds.Min.X + x*maskBounds.Dx()/width
			_, _, _, alpha := mask.At(mx, my).RGBA()
			if alpha > 0x7fff {
				continue
			}
			out.Set(x, y, blendRGBA(out.RGBAAt(x, y), overlay))
			marked = true
		}
	}
	return out, marked
}

func blendRGBA(dst, src color.RGBA) color.RGBA {
	a := uint32(src.A)
	inv := 255 - a
	return color.RGBA{
		R: uint8((uint32(src.R)*a + uint32(dst.R)*inv) / 255),
		G: uint8((uint32(src.G)*a + uint32(dst.G)*inv) / 255),
		B: uint8((uint32(src.B)*a + uint32(dst.B)*inv) / 255),
		A: 255,
	}
}

func decodeImageRefImage(ref string) (image.Image, error) {
	mimeType, data, err := readImageRefBytes(ref, 0)
	if err != nil {
		return nil, err
	}
	_ = mimeType
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func sampleImageRGBA(img image.Image, x, y, width, height int) color.RGBA {
	bounds := img.Bounds()
	sx := bounds.Min.X + x*bounds.Dx()/width
	sy := bounds.Min.Y + y*bounds.Dy()/height
	r, g, b, a := img.At(sx, sy).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

func shrinkResponsesInputImages(req *imagesRequest) error {
	if req == nil {
		return nil
	}
	for i, ref := range req.Images {
		shrunk, err := shrinkResponsesInputImageRef(ref)
		if err != nil {
			return err
		}
		req.Images[i] = shrunk
	}
	return nil
}

func shrinkResponsesInputImageRef(ref string) (string, error) {
	return shrinkDataImageURL(ref, maxResponsesInputImageBytes)
}

// shrinkDataImageURL 将 data URL 格式的图片压缩到 limit 字节以内。
// 非 data URL（如 http(s)）直接返回不处理。
func shrinkDataImageURL(ref string, limit int) (string, error) {
	if !strings.HasPrefix(ref, "data:") {
		return ref, nil
	}
	commaIdx := strings.IndexByte(ref, ',')
	if commaIdx < 0 {
		return ref, nil
	}
	data, err := base64.StdEncoding.DecodeString(ref[commaIdx+1:])
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(ref[commaIdx+1:])
		if err != nil {
			return "", err
		}
	}
	if len(data) <= limit {
		return ref, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("图片过大且无法压缩: %w", err)
	}
	return encodeJPEGDataURLWithinLimit(img, limit)
}

func encodeJPEGDataURLWithinLimit(img image.Image, limit int) (string, error) {
	data, _, err := encodeJPEGWithinLimit(img, limit)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data), nil
}

func flattenForJPEG(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Over)
	return out
}

func resizeImageNearest(img image.Image, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			out.SetRGBA(x, y, sampleImageRGBA(img, x, y, width, height))
		}
	}
	return out
}

// shrinkImageBytes 把原始图片字节压缩到 limit 以内。
// 已经在限制内的直接返回；超限则转 JPEG 并逐步缩小尺寸。
// 返回压缩后的字节和 MIME 类型。
func shrinkImageBytes(data []byte, mimeType string, limit int) ([]byte, string, error) {
	if len(data) <= limit {
		return data, mimeType, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("图片过大且无法解码进行压缩: %w", err)
	}
	return encodeJPEGWithinLimit(img, limit)
}

// encodeJPEGWithinLimit 把 image.Image 编码为 JPEG，逐步缩小直到字节数 <= limit。
func encodeJPEGWithinLimit(img image.Image, limit int) ([]byte, string, error) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, "", fmt.Errorf("图片尺寸无效")
	}
	current := img
	for range 10 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, flattenForJPEG(current), &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", err
		}
		if buf.Len() <= limit {
			return buf.Bytes(), "image/jpeg", nil
		}
		width = width * 3 / 4
		height = height * 3 / 4
		if width < 256 || height < 256 {
			break
		}
		current = resizeImageNearest(img, width, height)
	}
	return nil, "", fmt.Errorf("图片过大，请压缩到 %dMB 以内后重试", limit/(1024*1024))
}

func stripImageRevisedPrompts(calls []ImageGenCall) {
	for i := range calls {
		calls[i].RevisedPrompt = ""
	}
}

func imageGenCallDiagnosticsDetail(wsResult WSResult) string {
	if len(wsResult.ImageGenCallDiagnostics) > 0 {
		return strings.Join(wsResult.ImageGenCallDiagnostics, "; ")
	}
	if len(wsResult.CompletedEventRaw) > 0 {
		return "completed_event=" + truncate(string(wsResult.CompletedEventRaw), 500)
	}
	if wsResult.ResponseID != "" || wsResult.Model != "" || wsResult.StopReason != "" {
		parts := make([]string, 0, 3)
		if wsResult.ResponseID != "" {
			parts = append(parts, "response_id="+wsResult.ResponseID)
		}
		if wsResult.Model != "" {
			parts = append(parts, "model="+wsResult.Model)
		}
		if wsResult.StopReason != "" {
			parts = append(parts, "stop_reason="+wsResult.StopReason)
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func classifyImageGenCallFailures(failures []ImageGenCallFailure, fallbackDetail string) *responsesFailureError {
	for _, failure := range failures {
		if isSafetyRejectionText(failure.ErrorType, failure.ErrorCode, failure.Message, failure.IncompleteReason) {
			msg := strings.TrimSpace(failure.Message)
			if msg == "" {
				msg = "Your request was rejected by the safety system."
			}
			return &responsesFailureError{
				Kind:               responsesFailureKindClient,
				StatusCode:         http.StatusBadRequest,
				AnthropicErrorType: "invalid_request_error",
				Code:               "safety_rejected",
				Message:            msg,
			}
		}
	}
	if isSafetyRejectionText(fallbackDetail) {
		return &responsesFailureError{
			Kind:               responsesFailureKindClient,
			StatusCode:         http.StatusBadRequest,
			AnthropicErrorType: "invalid_request_error",
			Code:               "safety_rejected",
			Message:            "Your request was rejected by the safety system.",
		}
	}
	return nil
}

// buildImagesToolCreateMsg 把 Images REST 请求体翻译成 Responses API 的
// response.create 消息（tools 数组带一个 image_generation 项）。
// 返回：上游消息 bytes；n（当前固定 1）；input 估算的 token 数（用于计费）。
//
// contentType 仅在 isEdit=true 时需要（可能是 multipart/form-data）。
func buildImagesToolCreateMsg(
	body []byte,
	contentType string,
	isEdit bool,
	session openAISessionResolution,
) ([]byte, int, int, error) {
	msg, n, estimate, err := buildImagesToolCreateMsgWithUsage(body, contentType, isEdit, session)
	if err != nil {
		return nil, 0, 0, err
	}
	return msg, n, estimate.Total(), nil
}

func buildImagesToolCreateMsgWithUsage(
	body []byte,
	contentType string,
	isEdit bool,
	session openAISessionResolution,
) ([]byte, int, imagesInputTokenEstimate, error) {
	req, err := parseImagesRequest(body, contentType, isEdit)
	if err != nil {
		return nil, 0, imagesInputTokenEstimate{}, err
	}
	req.Quality = normalizeImageQualityDefaultMedium(req.Quality)
	// Responses API 的 image_generation tool 每次仅生成 1 张；n>1 在 REST 侧的语义
	// 需要多轮工具调用才能模拟，暂不支持 —— V1 限定 n=1。
	if req.N > 1 {
		return nil, 0, imagesInputTokenEstimate{}, fmt.Errorf("OAuth 模式下 n 只能为 1（REST→tools 翻译路径暂不支持多图）")
	}
	if err := shrinkResponsesInputImages(req); err != nil {
		return nil, 0, imagesInputTokenEstimate{}, err
	}
	regionAnnotation, err := buildEditRegionAnnotation(req)
	if err != nil {
		return nil, 0, imagesInputTokenEstimate{}, err
	}
	if regionAnnotation != "" {
		regionAnnotation, err = shrinkResponsesInputImageRef(regionAnnotation)
		if err != nil {
			return nil, 0, imagesInputTokenEstimate{}, err
		}
	}
	outputFormat := normalizedImageOutputFormat(req.OutputFormat)
	if isEdit && req.Mask != "" {
		outputFormat = "png"
	}
	// image_generation tool 把 size/quality/background 当作"工具默认参数"——
	// 上游每次调用该 tool 都按这些值执行。早先版本只把这些塞到 prompt constraints
	// 文本里，但中间那个 chat 模型（gpt-5.4）在触发 image_generation tool 时不会把
	// prompt 文本翻译成 tool args，结果上游 image_generation 收不到 size，永远按
	// 默认 1024×1024 出图——客户端选 4K 完全失效。
	// constraints 文本仍保留作为兜底（双轨制），但 tool 字段是权威来源。
	//
	// 不写 model 字段：image_generation tool 不接受 model（image 模型由工具自己定，
	// 默认 gpt-image-2），写了会 502。顶层 payload.model 是给 chat 模型用的另一回事。
	tool := map[string]any{
		"type":          "image_generation",
		"output_format": outputFormat,
	}
	if size := normalizeImageSizeForUpstream(req.Size); size != "" && !strings.EqualFold(size, "auto") {
		tool["size"] = size
	}
	tool["quality"] = req.Quality
	if background := strings.TrimSpace(req.Background); background != "" {
		tool["background"] = background
	}
	prompt := buildImagesToolPrompt(req, isEdit, regionAnnotation != "")
	instructions := buildImagesToolInstructions(req, isEdit, regionAnnotation != "")

	// input 必须是 list 形式（与 normalizeResponsesInput 的 string→list 包装一致），
	// 否则上游返回 "Input must be a list"。/edits 在同一条 user message 的 content
	// 里把 input_text 与 input_image 并列，image_generation tool 会拿去做图生图。
	content := []map[string]any{
		{"type": "input_text", "text": prompt},
	}
	for _, url := range req.Images {
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": url,
		})
	}
	if regionAnnotation != "" {
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": regionAnnotation,
		})
	}
	inputList := []map[string]any{
		{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	}
	payload := map[string]any{
		"model":        imagesOAuthChatModel,
		"instructions": instructions,
		"input":        inputList,
		"tools":        []any{tool},
		"tool_choice":  "auto",
		"stream":       true,
		"store":        false,
	}
	msg, err := wrapResponseCreate(payload, imagesOAuthChatModel, session)
	if err != nil {
		return nil, 0, imagesInputTokenEstimate{}, err
	}
	inputImageRefs := append([]string{}, req.Images...)
	if regionAnnotation != "" {
		inputImageRefs = append(inputImageRefs, regionAnnotation)
	}
	estimate := imagesInputTokenEstimate{
		TextTokens:  estimatePromptTokens(req.Prompt),
		ImageTokens: estimateGPTImage2InputImageTokens(inputImageRefs, req.Size),
	}
	return msg, req.N, estimate, nil
}

type imagesInputTokenEstimate struct {
	TextTokens  int
	ImageTokens int
}

func (e imagesInputTokenEstimate) Total() int {
	return e.TextTokens + e.ImageTokens
}

// forwardImagesViaResponsesTool 把 OpenAI Images REST 请求翻译成 Responses API
// 的 image_generation tool 调用，跑 OAuth WS 通道，最后把生成的 base64 图像
// 重新包装成 Images REST 响应返回给客户端。
//
// 只在 OAuth 账号处理 /v1/images/generations 时使用；API Key 账号继续走原生
// REST 通道（见 handleImagesResponse）。
func (g *OpenAIGateway) forwardImagesViaResponsesTool(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account

	session := resolveOpenAISession(req.Headers, req.Body)
	updateSessionStateFromRequest(session)

	_, reqPath := resolveAPIKeyRoute(req)
	isEdit := isImagesEditRequest(reqPath)
	contentType := req.Headers.Get("Content-Type")
	imgReq, err := parseImagesRequest(req.Body, contentType, isEdit)
	if err == nil {
		err = validateImageSize(imgReq.Size, imgReq.Model)
	}
	if err != nil {
		body := jsonError(err.Error())
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   err.Error(),
			Duration: time.Since(start),
		}, nil
	}
	g.logger.Debug("Images OAuth request",
		"path", reqPath,
		"request_model", imgReq.Model,
		"is_edit", isEdit,
		"size", imgReq.Size,
		"n", imgReq.N,
	)
	createMsg, n, inputEstimate, err := buildImagesToolCreateMsgWithUsage(req.Body, contentType, isEdit, session)
	if err != nil {
		body := jsonError(err.Error())
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   err.Error(),
			Duration: time.Since(start),
		}, nil
	}

	cfg := WSConfig{
		Token:          account.Credentials["access_token"],
		AccountID:      account.Credentials["chatgpt_account_id"],
		ProxyURL:       account.ProxyURL,
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnState:      session.LastTurnState,
		Originator:     req.Headers.Get("originator"),
	}
	conn, wsResp, err := DialWebSocket(cfg)
	if err != nil {
		if wsResp != nil {
			wsBody := openAIErrorJSON(openAIErrorTypeForStatus(wsResp.StatusCode), "", err.Error())
			outcome := failureOutcome(wsResp.StatusCode, wsBody, wsResp.Header.Clone(), err.Error(), extractRetryAfterHeader(wsResp.Header))
			outcome.Duration = time.Since(start)
			return outcome, forwardErrForOutcome(outcome, err)
		}
		return transientOutcome(err.Error()), err
	}
	defer func() { _ = conn.Close() }()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	if err := conn.WriteJSON(json.RawMessage(createMsg)); err != nil {
		reason := fmt.Sprintf("发送 WebSocket 消息失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	var sseKA *ssePingKeepAlive
	if req.Stream {
		sseKA = startSSEPingKeepAlive(req.Writer)
	}

	handler := &imagesSilentHandler{accountID: account.ID, start: start}
	wsResult := ReceiveWSResponse(ctx, conn, handler)
	if wsResult.ResponseID != "" && session.SessionKey != "" {
		updateSessionStateResponseID(session.SessionKey, wsResult.ResponseID)
	}

	elapsed := time.Since(start)

	if wsResult.Err != nil {
		var failure *responsesFailureError
		if errors.As(wsResult.Err, &failure) {
			if failure.shouldReturnClientError() {
				body := buildImagesErrorBodyWithCode(failure.StatusCode, failure.Code, failure.Message)
				if sseKA != nil {
					sseKA.Stop()
					g.logger.Warn("Images OAuth 上游返回客户端错误，已脱敏响应",
						"path", reqPath, "model", imgReq.Model, "status_code", failure.StatusCode, "code", failure.Code, "reason", failure.Message)
					clientMsg := sanitizedImageSSEErrorMessage
					if failure.StatusCode == http.StatusRequestEntityTooLarge {
						clientMsg = imageTooLargeSSEErrorMessage
					}
					writeSSEErrorIfStarted(req.Writer, sseKA, clientMsg)
				}
				return sdk.ForwardOutcome{
					Kind: sdk.OutcomeClientError,
					Upstream: sdk.UpstreamResponse{
						StatusCode: failure.StatusCode,
						Headers:    http.Header{"Content-Type": []string{"application/json"}},
						Body:       body,
					},
					Reason:   failure.Message,
					Duration: elapsed,
				}, nil
			}
			// 用 *responsesFailureError 自带的分类驱动 Outcome：
			// rate_limited 走 AccountRateLimited（带 RetryAfter），server 等仍归 UpstreamTransient。
			// 之前这里直接拍成 UpstreamTransient/502，导致 Core 把账号丢进 softExclude 被反复重试。
			if sseKA != nil {
				sseKA.Stop()
				g.logger.Warn("Images OAuth 流式请求失败，已脱敏响应",
					"path", reqPath, "model", imgReq.Model, "status_code", failure.StatusCode,
					"kind", failure.Kind, "retry_after", failure.RetryAfter, "error", wsResult.Err)
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			errBody := openAIErrorJSON(openAIErrorTypeForStatus(failure.StatusCode), string(failure.Kind), failure.Message)
			return sdk.ForwardOutcome{
				Kind:       failure.outcomeKind(),
				Upstream:   sdk.UpstreamResponse{StatusCode: failure.StatusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
				Reason:     failure.Error(),
				RetryAfter: failure.RetryAfter,
				Duration:   elapsed,
			}, wsResult.Err
		}
		// 兜底：网络层 / 解析失败 等无 *responsesFailureError 的情况，保留 UpstreamTransient/502。
		if sseKA != nil {
			sseKA.Stop()
			g.logger.Warn("Images OAuth 流式请求失败，已脱敏响应",
				"path", reqPath, "model", imgReq.Model, "error", wsResult.Err)
			writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
		}
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
			Reason:   wsResult.Err.Error(),
			Duration: elapsed,
		}, wsResult.Err
	}

	if len(wsResult.ImageGenCalls) == 0 {
		reason := fmt.Sprintf("image_generation_call 为空 (n=%d)", n)
		if detail := imageGenCallDiagnosticsDetail(wsResult); detail != "" {
			reason += ": " + detail
		}
		if failure := classifyImageGenCallFailures(wsResult.ImageGenCallFailures, reason); failure != nil && failure.shouldReturnClientError() {
			body := buildImagesErrorBodyWithCode(failure.StatusCode, failure.Code, failure.Message)
			if sseKA != nil {
				sseKA.Stop()
				g.logger.Warn("Images OAuth 图像工具返回客户端错误，已脱敏响应",
					"path", reqPath, "model", imgReq.Model, "status_code", failure.StatusCode,
					"code", failure.Code, "reason", reason)
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: failure.StatusCode,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       body,
				},
				Reason:   failure.Message,
				Duration: elapsed,
			}, nil
		}
		body := buildImagesErrorBody(http.StatusBadGateway, "上游未返回图像结果")
		if sseKA != nil {
			sseKA.Stop()
			g.logger.Warn("Images OAuth 未返回图像结果，已脱敏响应",
				"path", reqPath, "model", imgReq.Model, "reason", reason)
			writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
		}
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadGateway,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   reason,
			Duration: elapsed,
		}, fmt.Errorf("%s", reason)
	}
	if isEdit {
		// 局部绘图只把 mask / 区域标注交给模型，不再把生成结果按 mask 回拼原图。
		// 返回值保持为模型自己的整图输出，避免“拼贴感”。
		stripImageRevisedPrompts(wsResult.ImageGenCalls)
	}

	numImages := len(wsResult.ImageGenCalls)
	inputTokens := inputEstimate.Total()
	// 对外响应仍沿用 Images API 的 usage 口径；
	// 对内账单则拆成两段：
	//   1. Responses 主模型的上下文 token；
	//   2. 生图产出的按张费用。
	billingModel := imageGenerationBillingModel(wsResult.ToolImageModel, imgReq.Model)
	usage := newTokenUsage(billingModel, "", inputTokens, 0, 0, 0, handler.firstTokenMs)
	contextModel := strings.TrimSpace(wsResult.Model)
	if contextModel == "" {
		contextModel = imagesOAuthChatModel
	}
	addUsageCostForModel(
		usage,
		contextModel,
		"",
		wsResult.InputTokens,
		wsResult.OutputTokens,
		wsResult.CachedInputTokens,
		wsResult.ReasoningOutputTokens,
		"responses_context",
		"上下文",
	)
	g.logger.Debug("Images OAuth result",
		"path", reqPath,
		"request_model", imgReq.Model,
		"tool_model", wsResult.ToolImageModel,
		"billing_model", billingModel,
		"chat_model", imagesOAuthChatModel,
		"num_images", numImages,
	)

	// 计费 size 优先级（高 → 低）：
	//   1. 上游 image_generation_call event 的 size 字段（telemetry 反馈）
	//   2. 直接解码生成的 base64 图 header 拿真实宽高（auto 时最准的来源——
	//      上游有时不返 size 字段，但图本身永远是诚实的）
	//   3. 客户端请求里的 size（最可能是 "auto" 兜底）
	//
	// 这里得到的 billingSize 同时用于 response.usage.output_tokens 估算，
	// 避免为了写 usage 再额外解析一次图片。
	billingSize := imgReq.Size
	if len(wsResult.ImageGenCalls) > 0 {
		first := wsResult.ImageGenCalls[0]
		if first.Size != "" {
			billingSize = first.Size
		} else if sz, ok := imageActualSizeFromBase64(first.Result); ok {
			billingSize = sz
		}
	}
	imageOutputTokens := calculateGPTImage2OutputTokensForImages(billingModel, billingSize, imgReq.Quality, numImages)
	respBody := buildImagesRESTResponse(wsResult, inputEstimate.TextTokens, imageOutputTokens, billingModel, imagesResponseOptions{
		BillingSize:             billingSize,
		RequestSize:             imgReq.Size,
		RequestQuality:          imgReq.Quality,
		RequestOutputFormat:     imgReq.OutputFormat,
		RequestImageInputTokens: inputEstimate.ImageTokens,
	})
	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}
	if sseKA != nil {
		sseKA.Stop()
		writeImagesRESTSSE(req.Writer, respBody)
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
	} else {
		outcome.Upstream.Body = respBody
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}

	// 图片尺寸作为通用 UsageAttribute 入库，后台费用明细可用它解释 1K/2K/4K 分档。
	setUsageTokens(usage, inputTokens, imageOutputTokens, 0, 0)
	fillUsageCostPerImageBySize(usage, numImages, billingSize)
	return outcome, nil
}

// buildImagesRESTResponse 按 OpenAI Images API 官方契约打包响应。
// 计费口径：text input tokens + image input tokens + image output tokens，按实际响应的图像模型记录。
// instructions / 工具调用包装产生的额外 chat tokens 由内层吸收，不出现在对外 usage。
// 这样：
//  1. 客户端拿到的 usage 数字语义与 OpenAI 原生 Images API 完全一致
//  2. 外层再套一层 AirGate 时，两级按同一口径独立计算，金额零偏差
func buildImagesRESTResponse(wsResult WSResult, textInputTokens, imageOutputTokens int, responseModel string, options ...imagesResponseOptions) []byte {
	if responseModel == "" {
		responseModel = imageToolCostModel
	}
	opts := imagesResponseOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	data := make([]map[string]any, 0, len(wsResult.ImageGenCalls))
	for _, call := range wsResult.ImageGenCalls {
		item := map[string]any{
			"b64_json": call.Result,
		}
		if call.RevisedPrompt != "" {
			item["revised_prompt"] = call.RevisedPrompt
		}
		data = append(data, item)
	}
	size := normalizeImagesResponseSize(opts.BillingSize)
	if size == "" {
		size = normalizeImagesResponseSize(opts.RequestSize)
	}
	if size == "" && len(wsResult.ImageGenCalls) > 0 {
		size = normalizeImagesResponseSize(wsResult.ImageGenCalls[0].Size)
	}
	background := ""
	if len(wsResult.ImageGenCalls) > 0 {
		background = normalizeImagesResponseBackground(wsResult.ImageGenCalls[0].Background)
	}
	outputFormat := normalizeImagesResponseOutputFormat(opts.RequestOutputFormat)
	if outputFormat == "" && len(wsResult.ImageGenCalls) > 0 {
		outputFormat = normalizeImagesResponseOutputFormat(wsResult.ImageGenCalls[0].OutputFormat)
	}
	if outputFormat == "" {
		outputFormat = "png"
	}
	payload := map[string]any{
		"created":       time.Now().Unix(),
		"data":          data,
		"quality":       normalizeImageQualityDefaultMedium(opts.RequestQuality),
		"output_format": outputFormat,
		// root 级 model，供下一级 handleImagesResponse 做 fillCost 查价和 usage 记录。
		"model": responseModel,
	}
	if size != "" {
		payload["size"] = size
	}
	if background != "" {
		payload["background"] = background
	}
	imageInputTokens := opts.RequestImageInputTokens
	inputTokens := textInputTokens + imageInputTokens
	if inputTokens+imageOutputTokens > 0 {
		payload["usage"] = map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": imageOutputTokens,
			"total_tokens":  inputTokens + imageOutputTokens,
			"input_tokens_details": map[string]any{
				"text_tokens":  textInputTokens,
				"image_tokens": imageInputTokens,
			},
		}
	}
	b, _ := json.Marshal(payload)
	return b
}

func normalizeImagesResponseSize(size string) string {
	width, height, ok := parseImageSize(size)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%dx%d", width, height)
}

func normalizeImagesResponseBackground(background string) string {
	switch strings.ToLower(strings.TrimSpace(background)) {
	case "transparent":
		return "transparent"
	case "opaque":
		return "opaque"
	default:
		return ""
	}
}

func normalizeImagesResponseOutputFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "png"
	case "webp":
		return "webp"
	case "jpeg":
		return "jpeg"
	default:
		return ""
	}
}

func applyImagesResponseMetadata(body []byte, opts imagesResponseOptions, summary imagesResponseSummary) []byte {
	if len(body) == 0 {
		return body
	}

	updated := body
	data := gjson.GetBytes(body, "data")
	if data.Exists() && data.IsArray() {
		for idx, item := range data.Array() {
			if !item.IsObject() {
				continue
			}
			for _, field := range []string{"quality", "size", "background", "output_format"} {
				if !item.Get(field).Exists() {
					continue
				}
				next, err := sjson.DeleteBytes(updated, fmt.Sprintf("data.%d.%s", idx, field))
				if err != nil {
					return body
				}
				updated = next
			}
		}
	}

	rootSize := strings.TrimSpace(gjson.GetBytes(body, "size").String())
	rootBackground := strings.TrimSpace(gjson.GetBytes(body, "background").String())
	rootOutputFormat := strings.TrimSpace(gjson.GetBytes(body, "output_format").String())
	firstDataBackground := ""
	firstDataOutputFormat := ""
	if data.Exists() && data.IsArray() {
		for _, item := range data.Array() {
			if !item.IsObject() {
				continue
			}
			if firstDataBackground == "" {
				firstDataBackground = item.Get("background").String()
			}
			if firstDataOutputFormat == "" {
				firstDataOutputFormat = item.Get("output_format").String()
			}
			if firstDataBackground != "" && firstDataOutputFormat != "" {
				break
			}
		}
	}

	quality := normalizeImageQualityDefaultMedium(opts.RequestQuality)
	size := ""
	if candidate := normalizeImagesResponseSize(summary.BillingSize); candidate != "" {
		size = candidate
	} else if candidate := normalizeImagesResponseSize(rootSize); candidate != "" {
		size = candidate
	} else if candidate := normalizeImagesResponseSize(opts.RequestSize); candidate != "" {
		size = candidate
	}
	background := ""
	if candidate := normalizeImagesResponseBackground(rootBackground); candidate != "" {
		background = candidate
	} else if candidate := normalizeImagesResponseBackground(firstDataBackground); candidate != "" {
		background = candidate
	}
	outputFormat := ""
	if candidate := normalizeImagesResponseOutputFormat(opts.RequestOutputFormat); candidate != "" {
		outputFormat = candidate
	} else if candidate := normalizeImagesResponseOutputFormat(rootOutputFormat); candidate != "" {
		outputFormat = candidate
	} else if candidate := normalizeImagesResponseOutputFormat(firstDataOutputFormat); candidate != "" {
		outputFormat = candidate
	} else {
		outputFormat = "png"
	}

	for _, field := range []string{"size", "background"} {
		if (field == "size" && size != "") || (field == "background" && background != "") || !gjson.GetBytes(body, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(updated, field)
		if err != nil {
			return body
		}
		updated = next
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"background", background},
		{"output_format", outputFormat},
		{"quality", quality},
		{"size", size},
	} {
		if field.value == "" {
			continue
		}
		next, err := sjson.SetBytes(updated, field.name, field.value)
		if err != nil {
			return body
		}
		updated = next
	}

	return updated
}

func applyImagesResponseUsage(body []byte, opts imagesResponseOptions, outputTokens int) []byte {
	if len(body) == 0 {
		return body
	}
	updated := body
	usageNode := gjson.GetBytes(updated, "usage")
	existingInputNode := usageNode.Get("input_tokens")
	existingOutputNode := usageNode.Get("output_tokens")
	existingTextNode := usageNode.Get("input_tokens_details.text_tokens")
	existingImageNode := usageNode.Get("input_tokens_details.image_tokens")

	textTokens := 0
	switch {
	case existingTextNode.Exists():
		textTokens = int(existingTextNode.Int())
	case existingInputNode.Exists():
		textTokens = int(existingInputNode.Int())
		if existingImageNode.Exists() {
			imageTokens := int(existingImageNode.Int())
			if imageTokens > 0 && textTokens >= imageTokens {
				textTokens -= imageTokens
			}
		}
	case opts.RequestTextInputTokens > 0:
		textTokens = opts.RequestTextInputTokens
	}

	imageInputTokens := int(existingImageNode.Int())
	if opts.RequestImageInputTokens > 0 {
		imageInputTokens = opts.RequestImageInputTokens
	}
	if outputTokens <= 0 && existingOutputNode.Exists() {
		outputTokens = int(existingOutputNode.Int())
	}
	inputTokens := textTokens + imageInputTokens
	if inputTokens+outputTokens == 0 && !usageNode.Exists() {
		return body
	}
	var err error
	updated, err = sjson.SetBytes(updated, "usage.input_tokens", inputTokens)
	if err != nil {
		return body
	}
	updated, err = sjson.SetBytes(updated, "usage.output_tokens", outputTokens)
	if err != nil {
		return body
	}
	updated, err = sjson.SetBytes(updated, "usage.total_tokens", inputTokens+outputTokens)
	if err != nil {
		return body
	}
	updated, err = sjson.SetBytes(updated, "usage.input_tokens_details.text_tokens", textTokens)
	if err != nil {
		return body
	}
	updated, err = sjson.SetBytes(updated, "usage.input_tokens_details.image_tokens", imageInputTokens)
	if err != nil {
		return body
	}
	return updated
}

func applyImagesResponseOutputTokens(body []byte, outputTokens int) []byte {
	return applyImagesResponseUsage(body, imagesResponseOptions{}, outputTokens)
}

type imagesResponseSummary struct {
	NumImages   int
	BillingSize string
}

func summarizeImagesResponseForBilling(body []byte, fallbackSize string) imagesResponseSummary {
	summary := imagesResponseSummary{BillingSize: strings.TrimSpace(fallbackSize)}
	sizeResolved := false
	if rootSize := normalizeImagesResponseSize(gjson.GetBytes(body, "size").String()); rootSize != "" {
		summary.BillingSize = rootSize
		sizeResolved = true
	}
	dataArr := gjson.GetBytes(body, "data")
	if !dataArr.Exists() || !dataArr.IsArray() {
		return summary
	}
	for _, item := range dataArr.Array() {
		b64 := item.Get("b64_json").String()
		if b64 != "" {
			summary.NumImages++
		} else if u := item.Get("url").String(); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			summary.NumImages++
		}
		if sizeResolved {
			continue
		}
		if sz := normalizeImagesResponseSize(item.Get("size").String()); sz != "" {
			summary.BillingSize = sz
			sizeResolved = true
			continue
		}
		if b64 != "" {
			if sz, ok := imageActualSizeFromBase64(b64); ok {
				summary.BillingSize = sz
				sizeResolved = true
			}
		}
	}
	return summary
}

// buildImagesErrorBody 返回 OpenAI 风格错误 body。
func buildImagesErrorBody(status int, message string) []byte {
	return buildImagesErrorBodyWithCode(status, "", message)
}

func buildImagesErrorBodyWithCode(status int, code, message string) []byte {
	errType := "server_error"
	if status >= 400 && status < 500 {
		errType = "invalid_request_error"
	}
	if code == "" {
		code = fmt.Sprintf("images_%d", status)
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// handleImagesResponse 处理 OpenAI Images API 的非流式响应。
//
// 与 handleNonStreamResponse 的差异：Images 响应体通常不包含 model 字段，
// 若从 body 读到空串就回退到请求侧传入的 fallbackModel，否则 fillCost 会因
// 查不到定价而把 InputCost / OutputCost 置零，账单会失真。
//
// 计费字段复用 parseUsage：gpt-image-1 / gpt-image-1.5 返回的
// usage.input_tokens / usage.output_tokens / usage.input_tokens_details.cached_tokens
// 与 Responses API 字段同构，parseUsage 已经处理了 cached token 扣减。
type imagesResponseOptions struct {
	BillingSize             string
	RequestSize             string
	RequestQuality          string
	RequestOutputFormat     string
	RequestTextInputTokens  int
	RequestImageInputTokens int
}

func handleImagesResponse(resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string, options ...imagesResponseOptions) (sdk.ForwardOutcome, error) {
	return handleImagesResponseWithLogger(nil, resp, w, sseKA, start, fallbackModel, options...)
}

func (g *OpenAIGateway) handleImagesResponse(resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string, options ...imagesResponseOptions) (sdk.ForwardOutcome, error) {
	return handleImagesResponseWithLogger(g.logger, resp, w, sseKA, start, fallbackModel, options...)
}

func handleImagesResponseWithLogger(logger *slog.Logger, resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string, options ...imagesResponseOptions) (sdk.ForwardOutcome, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取 Images 响应失败: %v", err)
		if sseKA != nil {
			sseKA.Stop()
			if logger != nil {
				logger.Warn("Images APIKey 响应读取失败，已脱敏响应", "model", fallbackModel, "error", err)
			}
			writeSSEErrorIfStarted(w, sseKA, sanitizedImageSSEErrorMessage)
		}
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	opts := imagesResponseOptions{}
	if len(options) > 0 {
		opts = options[0]
	}

	modelName := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if modelName == "" {
		modelName = fallbackModel
	}
	if logger == nil {
		logger = slog.Default()
	}
	summary := summarizeImagesResponseForBilling(body, opts.BillingSize)
	body = applyImagesResponseMetadata(body, opts, summary)
	imageOutputTokens := calculateGPTImage2OutputTokensForImages(modelName, summary.BillingSize, opts.RequestQuality, summary.NumImages)
	if !isGPTImage2Model(modelName) {
		opts.RequestImageInputTokens = 0
	}
	body = applyImagesResponseUsage(body, opts, imageOutputTokens)

	parsed := parseUsage(body)
	headers := resp.Header.Clone()
	if headers.Get("Content-Length") != "" {
		headers.Set("Content-Length", strconv.Itoa(len(body)))
	}

	if sseKA != nil {
		sseKA.Stop()
		writeImagesRESTSSE(w, body)
	} else if w != nil {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	logger.Debug("images_native_result_returned",
		"request_model", fallbackModel,
		sdk.LogFieldModel, modelName,
		"num_images", summary.NumImages,
		"billing_size", summary.BillingSize,
	)

	elapsed := time.Since(start)
	usage := newTokenUsage(modelName, "", parsed.inputTokens, parsed.outputTokens, parsed.cachedInputTokens, 0, elapsed.Milliseconds())
	fillUsageCostPerImageBySize(usage, summary.NumImages, summary.BillingSize)

	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode, Headers: headers},
		Usage:    usage,
		Duration: elapsed,
	}
	if sseKA != nil {
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
	} else {
		outcome.Upstream.Body = body
	}
	return outcome, nil
}

// countUsableImages 统计响应体中实际携带图片数据（b64_json 或 url）的条目数。
// 不含可用图片时返回 0，避免空响应被错误计费。
func countUsableImages(body []byte) int {
	dataArr := gjson.GetBytes(body, "data")
	if !dataArr.Exists() || !dataArr.IsArray() {
		return 0
	}
	n := 0
	for _, item := range dataArr.Array() {
		if item.Get("b64_json").String() != "" {
			n++
		} else if u := item.Get("url").String(); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			n++
		}
	}
	return n
}

// ──────────────────────────────────────────────────────
// 异步图片任务轮询（apimart.ai 等异步上游）
// ──────────────────────────────────────────────────────

const (
	asyncImagePollInitialDelay = 10 * time.Second
	asyncImagePollInterval     = 5 * time.Second
	asyncImagePollMaxAttempts  = 60
)

// isAsyncImageTaskResponse 检测上游响应是否为异步任务模式（返回 task_id 而非图片数据）。
func isAsyncImageTaskResponse(body []byte) (taskID string, ok bool) {
	tid := gjson.GetBytes(body, "data.0.task_id").String()
	if tid != "" {
		return tid, true
	}
	tid = gjson.GetBytes(body, "data.task_id").String()
	if tid != "" {
		return tid, true
	}
	return "", false
}

// pollAsyncImageTask 轮询异步图片任务直到完成，返回转换为 OpenAI Images API 标准格式的响应体。
func (g *OpenAIGateway) pollAsyncImageTask(
	ctx context.Context,
	account *sdk.Account,
	taskID string,
	logger *slog.Logger,
) ([]byte, error) {
	baseURL := strings.TrimRight(account.Credentials["base_url"], "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	baseURL = strings.TrimSuffix(baseURL, "/v1")

	pollURL := baseURL + "/v1/tasks/" + taskID
	client := g.buildHTTPClient(account)

	logger.Debug("images_async_task_poll_start", "task_id", taskID)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(asyncImagePollInitialDelay):
	}

	for attempt := range asyncImagePollMaxAttempts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pollReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, fmt.Errorf("构建轮询请求失败: %w", err)
		}
		setAuthHeaders(pollReq, account)

		pollResp, err := client.Do(pollReq)
		if err != nil {
			logger.Warn("images_async_task_poll_error",
				"task_id", taskID, "attempt", attempt, sdk.LogFieldError, err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(asyncImagePollInterval):
			}
			continue
		}
		body, _ := io.ReadAll(pollResp.Body)
		_ = pollResp.Body.Close()

		status := gjson.GetBytes(body, "data.status").String()
		logger.Debug("images_async_task_poll_status",
			"task_id", taskID, "attempt", attempt, "status", status)

		switch status {
		case "completed":
			return transformAsyncImageResult(body)
		case "failed", "error":
			reason := gjson.GetBytes(body, "data.result.error").String()
			if reason == "" {
				reason = gjson.GetBytes(body, "data.error").String()
			}
			if reason == "" {
				reason = "unknown error"
			}
			return nil, fmt.Errorf("异步图片任务失败: %s", reason)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(asyncImagePollInterval):
		}
	}
	return nil, fmt.Errorf("异步图片任务 %s 超时", taskID)
}

// transformAsyncImageResult 将异步任务完成响应转换为 OpenAI Images API 标准格式。
func transformAsyncImageResult(body []byte) ([]byte, error) {
	images := gjson.GetBytes(body, "data.result.images")
	if !images.Exists() || !images.IsArray() {
		return nil, fmt.Errorf("异步任务结果中未找到图片数据")
	}

	var dataItems []map[string]any
	for _, img := range images.Array() {
		urls := img.Get("url")
		if urls.IsArray() {
			for _, u := range urls.Array() {
				dataItems = append(dataItems, map[string]any{"url": u.String()})
			}
		} else if urls.Type == gjson.String {
			dataItems = append(dataItems, map[string]any{"url": urls.String()})
		}
	}

	if len(dataItems) == 0 {
		return nil, fmt.Errorf("异步任务结果中图片 URL 为空")
	}

	result := map[string]any{
		"created": time.Now().Unix(),
		"data":    dataItems,
	}
	return json.Marshal(result)
}
