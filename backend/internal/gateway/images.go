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
	_ "image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
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
// /generations 只支持 application/json；/edits 同时支持 JSON（image 字段是 base64/data URL/数组）
// 与 multipart/form-data（OpenAI SDK 标准）。
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
				req.Images = append(req.Images, normalizeImageRef(s))
			}
		}
	} else if s := strings.TrimSpace(imgNode.String()); s != "" {
		req.Images = append(req.Images, normalizeImageRef(s))
	}
	if mask := strings.TrimSpace(gjson.GetBytes(body, "mask").String()); mask != "" {
		req.Mask = normalizeImageRef(mask)
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
			req.Images = append(req.Images, multipartImageRef(ctype, data, text))
		case "mask":
			req.Mask = multipartImageRef(ctype, data, text)
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

// multipartImageRef 把 multipart part 转成统一的图片引用：
//   - 二进制文件（Content-Type=image/*）→ data URL
//   - 文本字段（Content-Type=text/* 或空 + 看上去像 data URL / http URL）→ 直接使用文本
func multipartImageRef(contentType string, data []byte, text string) string {
	mainType := strings.TrimSpace(strings.Split(strings.ToLower(contentType), ";")[0])
	if strings.HasPrefix(mainType, "image/") {
		return "data:" + mainType + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	// 兜底：无 Content-Type 也按二进制处理（OpenAI SDK 有时只带文件名不带类型）
	if mainType == "" || mainType == "application/octet-stream" {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	}
	return normalizeImageRef(text)
}

// normalizeImageRef 把用户传来的 image 字符串归一化为上游能识别的形式。
// 已经是 data URL / http(s) URL → 原样返回；否则按裸 base64（PNG）处理。
func normalizeImageRef(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	if strings.HasPrefix(s, "data:") {
		return normalizeImageDataURL(s)
	}
	return "data:image/png;base64," + normalizeBase64ImageData(s)
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
	base, _ := decodeDataURLImage(req.Images[0])
	mask, err := decodeDataURLImage(req.Mask)
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

func decodeDataURLImage(ref string) (image.Image, error) {
	if !strings.HasPrefix(ref, "data:") {
		return nil, fmt.Errorf("仅支持 data URL 图片")
	}
	commaIdx := strings.Index(ref, ",")
	if commaIdx < 0 {
		return nil, fmt.Errorf("data URL 缺少 base64 数据")
	}
	data, err := base64.StdEncoding.DecodeString(ref[commaIdx+1:])
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(ref[commaIdx+1:])
		if err != nil {
			return nil, err
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return img, nil
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

func compositeMaskedImageGenCalls(req *imagesRequest, calls []ImageGenCall) ([]ImageGenCall, error) {
	if req == nil || req.Mask == "" || len(req.Images) == 0 || len(calls) == 0 {
		return calls, nil
	}
	base, err := decodeDataURLImage(req.Images[0])
	if err != nil {
		return nil, fmt.Errorf("解码编辑目标图片失败: %w", err)
	}
	mask, err := decodeDataURLImage(req.Mask)
	if err != nil {
		return nil, fmt.Errorf("解码编辑 mask 失败: %w", err)
	}

	out := append([]ImageGenCall(nil), calls...)
	for i := range out {
		if out[i].Result == "" {
			continue
		}
		generated, err := decodeBase64Image(out[i].Result)
		if err != nil {
			return nil, fmt.Errorf("解码编辑结果图片失败: %w", err)
		}
		composited := compositeMaskedImage(base, mask, generated)
		encoded, err := encodePNGBase64(composited)
		if err != nil {
			return nil, fmt.Errorf("编码编辑合成结果失败: %w", err)
		}
		out[i].Result = encoded
	}
	return out, nil
}

func decodeBase64Image(b64 string) (image.Image, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, err
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func compositeMaskedImage(base, mask, generated image.Image) *image.RGBA {
	baseBounds := base.Bounds()
	width, height := baseBounds.Dx(), baseBounds.Dy()
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	maskBounds := mask.Bounds()
	for y := 0; y < height; y++ {
		my := maskBounds.Min.Y + y*maskBounds.Dy()/height
		for x := 0; x < width; x++ {
			mx := maskBounds.Min.X + x*maskBounds.Dx()/width
			_, _, _, alpha := mask.At(mx, my).RGBA()
			if alpha <= 0x7fff {
				out.SetRGBA(x, y, sampleImageRGBA(generated, x, y, width, height))
				continue
			}
			out.SetRGBA(x, y, sampleImageRGBA(base, x, y, width, height))
		}
	}
	return out
}

func sampleImageRGBA(img image.Image, x, y, width, height int) color.RGBA {
	bounds := img.Bounds()
	sx := bounds.Min.X + x*bounds.Dx()/width
	sy := bounds.Min.Y + y*bounds.Dy()/height
	r, g, b, a := img.At(sx, sy).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

func encodePNGBase64(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
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

// buildImagesToolCreateMsg 把 Images REST 请求体翻译成 Responses API 的
// response.create 消息（tools 数组带一个 image_generation 项）。
// 返回：上游消息 bytes；n（当前固定 1）；prompt 估算的 token 数（用于计费）。
//
// contentType 仅在 isEdit=true 时需要（可能是 multipart/form-data）。
func buildImagesToolCreateMsg(
	body []byte,
	contentType string,
	isEdit bool,
	session openAISessionResolution,
) ([]byte, int, int, error) {
	req, err := parseImagesRequest(body, contentType, isEdit)
	if err != nil {
		return nil, 0, 0, err
	}
	// Responses API 的 image_generation tool 每次仅生成 1 张；n>1 在 REST 侧的语义
	// 需要多轮工具调用才能模拟，暂不支持 —— V1 限定 n=1。
	if req.N > 1 {
		return nil, 0, 0, fmt.Errorf("OAuth 模式下 n 只能为 1（REST→tools 翻译路径暂不支持多图）")
	}
	regionAnnotation, err := buildEditRegionAnnotation(req)
	if err != nil {
		return nil, 0, 0, err
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
	if quality := strings.TrimSpace(req.Quality); quality != "" {
		tool["quality"] = quality
	}
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
		return nil, 0, 0, err
	}
	// input token 估算：文本 prompt + 每张参考图按 size 低质档估算（~272 tokens/1024²），
	// 与 image_generation tool 输入图像的 token 级别一致。
	imageInputCount := len(req.Images)
	if regionAnnotation != "" {
		imageInputCount++
	}
	inputTokens := estimatePromptTokens(req.Prompt) + estimateImageInputTokens(imageInputCount, req.Size)
	return msg, req.N, inputTokens, nil
}

// estimateImageInputTokens 估算参考图输入的 token 总数。
// 策略：套用 OpenAI 的 size→low-quality output token 表，近似代表"单张参考图"的 token 体量。
// OpenAI 对图像输入另有单价（gpt-image-1.5 约 $10/1M），但当前注册表里只记了文本 $5/1M，
// 精度损失不到 2×，对总价（输出图像 token 主导）影响 < 5%，V1 可接受。
func estimateImageInputTokens(count int, size string) int {
	if count <= 0 {
		return 0
	}
	return count * lookupImageGenOutputTokens(size, "low")
}

// forwardImagesViaResponsesTool 把 OpenAI Images REST 请求翻译成 Responses API
// 的 image_generation tool 调用，跑 OAuth WS 通道，最后把生成的 base64 图像
// 重新包装成 Images REST 响应返回给客户端。
//
// 只在 OAuth 账号被调度到 /v1/images/generations 时使用；API Key 账号继续走
// 原生 REST 通道（见 handleImagesResponse）。
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
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadRequest)
			_, _ = req.Writer.Write(body)
		}
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
	createMsg, n, promptTokens, err := buildImagesToolCreateMsg(req.Body, contentType, isEdit, session)
	if err != nil {
		body := jsonError(err.Error())
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadRequest)
			_, _ = req.Writer.Write(body)
		}
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
			outcome := failureOutcome(wsResp.StatusCode, nil, wsResp.Header.Clone(), err.Error(), 0)
			outcome.Duration = time.Since(start)
			return outcome, err
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
				body := buildImagesErrorBody(failure.StatusCode, failure.Message)
				if sseKA != nil {
					sseKA.Stop()
					g.logger.Warn("Images OAuth 上游返回客户端错误，已脱敏响应",
						"path", reqPath, "model", imgReq.Model, "status_code", failure.StatusCode, "reason", failure.Message)
					writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
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
				}, wsResult.Err
			}
			// 用 *responsesFailureError 自带的分类驱动 Outcome：
			// rate_limited 走 AccountRateLimited（带 RetryAfter），server 等仍归 UpstreamTransient。
			// 之前这里直接拍成 UpstreamTransient/502，导致 Core 把账号丢进 softExclude 被反复重试。
			if sseKA != nil {
				sseKA.Stop()
				g.logger.Warn("Images OAuth 流式请求失败，已脱敏响应",
					"path", reqPath, "model", imgReq.Model, "status_code", failure.StatusCode,
					"kind", failure.Kind, "retry_after", failure.RetryAfter, "error", wsResult.Err)
				writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
			}
			return sdk.ForwardOutcome{
				Kind:       failure.outcomeKind(),
				Upstream:   sdk.UpstreamResponse{StatusCode: failure.StatusCode},
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
			writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
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
		body := buildImagesErrorBody(http.StatusBadGateway, "上游未返回图像结果")
		if sseKA != nil {
			sseKA.Stop()
			g.logger.Warn("Images OAuth 未返回图像结果，已脱敏响应",
				"path", reqPath, "model", imgReq.Model, "reason", reason)
			writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
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
		var err error
		wsResult.ImageGenCalls, err = compositeMaskedImageGenCalls(imgReq, wsResult.ImageGenCalls)
		if err != nil {
			body := buildImagesErrorBody(http.StatusBadGateway, err.Error())
			if sseKA != nil {
				sseKA.Stop()
				g.logger.Warn("Images OAuth 编辑结果合成失败，已脱敏响应",
					"path", reqPath, "model", imgReq.Model, "error", err)
				writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
			}
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeUpstreamTransient,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadGateway,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       body,
				},
				Reason:   err.Error(),
				Duration: elapsed,
			}, err
		}
		stripImageRevisedPrompts(wsResult.ImageGenCalls)
	}

	numImages := len(wsResult.ImageGenCalls)
	// 计费按 OpenAI Images API 官方口径：prompt tokens + 图像输出 tokens。
	// 上游 instructions / 工具调用包装产生的额外 chat tokens 由内层吸收（OAuth 订阅账号无逐 token 成本）。
	billingModel := imageGenerationBillingModel(wsResult.ToolImageModel, imgReq.Model)
	usage := &sdk.Usage{
		InputTokens:  promptTokens,
		Model:        billingModel,
		FirstTokenMs: handler.firstTokenMs,
	}
	g.logger.Debug("Images OAuth result",
		"path", reqPath,
		"request_model", imgReq.Model,
		"tool_model", wsResult.ToolImageModel,
		"billing_model", billingModel,
		"chat_model", imagesOAuthChatModel,
		"num_images", numImages,
	)

	respBody := buildImagesRESTResponse(wsResult, promptTokens, 0, billingModel)
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

	// 计费 size 优先级（高 → 低）：
	//   1. 上游 image_generation_call event 的 size 字段（telemetry 反馈）
	//   2. 直接解码生成的 base64 图 header 拿真实宽高（auto 时最准的来源——
	//      上游有时不返 size 字段，但图本身永远是诚实的）
	//   3. 客户端请求里的 size（最可能是 "auto" 兜底）
	billingSize := imgReq.Size
	if len(wsResult.ImageGenCalls) > 0 {
		first := wsResult.ImageGenCalls[0]
		if first.Size != "" {
			billingSize = first.Size
		} else if sz, ok := imageActualSizeFromBase64(first.Result); ok {
			billingSize = sz
		}
	}
	// usage.ImageSize 透传到 Core 落到 usage_log.image_size 列，admin 后台用它解释计费
	// 档位（1K/2K/4K）。auto 计费场景特别有用：用户看到 0.40 美元能立即对照 size 知道
	// "上游真出了张 4K 图"，不用怀疑是不是算错了。
	usage.ImageSize = billingSize
	fillUsageCostPerImageBySize(usage, numImages, billingSize)
	return outcome, nil
}

// buildImagesRESTResponse 按 OpenAI Images API 官方契约打包响应。
// 计费口径：prompt tokens + image output tokens，按实际响应的图像模型记录。
// instructions / 工具调用包装产生的额外 chat tokens 由内层吸收，不出现在对外 usage。
// 这样：
//  1. 客户端拿到的 usage 数字语义与 OpenAI 原生 Images API 完全一致
//  2. 外层再套一层 AirGate 时，两级按同一口径独立计算，金额零偏差
func buildImagesRESTResponse(wsResult WSResult, promptTokens, imageOutputTokens int, responseModel string) []byte {
	if responseModel == "" {
		responseModel = imageToolCostModel
	}
	data := make([]map[string]any, 0, len(wsResult.ImageGenCalls))
	for _, call := range wsResult.ImageGenCalls {
		item := map[string]any{
			"b64_json": call.Result,
		}
		if call.RevisedPrompt != "" {
			item["revised_prompt"] = call.RevisedPrompt
		}
		// 透传上游实际生效的 image_generation 工具模型（从 response.tools[].model 提取）。
		// 客户端可据此判断"请求的 model"是否被上游静默降级。
		if wsResult.ToolImageModel != "" {
			item["model"] = wsResult.ToolImageModel
		}
		data = append(data, item)
	}
	payload := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		// root 级 model，供下一级 handleImagesResponse 做 fillCost 查价和 usage 记录。
		"model": responseModel,
	}
	if promptTokens+imageOutputTokens > 0 {
		payload["usage"] = map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": imageOutputTokens,
			"total_tokens":  promptTokens + imageOutputTokens,
			"input_tokens_details": map[string]any{
				"text_tokens":  promptTokens,
				"image_tokens": 0,
			},
		}
	}
	b, _ := json.Marshal(payload)
	return b
}

// buildImagesErrorBody 返回 OpenAI 风格错误 body。
func buildImagesErrorBody(status int, message string) []byte {
	errType := "server_error"
	if status >= 400 && status < 500 {
		errType = "invalid_request_error"
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    fmt.Sprintf("images_%d", status),
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
func handleImagesResponse(resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string) (sdk.ForwardOutcome, error) {
	return handleImagesResponseWithLogger(nil, resp, w, sseKA, start, fallbackModel)
}

func (g *OpenAIGateway) handleImagesResponse(resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string) (sdk.ForwardOutcome, error) {
	return handleImagesResponseWithLogger(g.logger, resp, w, sseKA, start, fallbackModel)
}

func handleImagesResponseWithLogger(logger *slog.Logger, resp *http.Response, w http.ResponseWriter, sseKA *ssePingKeepAlive, start time.Time, fallbackModel string) (sdk.ForwardOutcome, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取 Images 响应失败: %v", err)
		if sseKA != nil {
			sseKA.Stop()
			if logger != nil {
				logger.Warn("Images APIKey 响应读取失败，已脱敏响应", "model", fallbackModel, "error", err)
			}
			writeSSEError(w, sanitizedImageSSEErrorMessage)
		}
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	parsed := parseUsage(body)
	headers := resp.Header.Clone()

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

	modelName := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if modelName == "" {
		modelName = fallbackModel
	}

	numImages := int(gjson.GetBytes(body, "data.#").Int())
	if numImages <= 0 {
		numImages = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("images_native_result_returned",
		"request_model", fallbackModel,
		sdk.LogFieldModel, modelName,
		"num_images", numImages,
	)

	elapsed := time.Since(start)
	usage := &sdk.Usage{
		InputTokens:       parsed.inputTokens,
		OutputTokens:      parsed.outputTokens,
		CachedInputTokens: parsed.cachedInputTokens,
		Model:             modelName,
		FirstTokenMs:      elapsed.Milliseconds(),
	}
	fillUsageCostPerImage(usage, numImages)

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
