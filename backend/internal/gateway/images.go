package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
const imagesPassthroughInstructions = "Forward the user's message verbatim as the prompt argument to the image_generation tool. Do not rewrite, elaborate, or add details about style, composition, lighting, color, mood, or any elements the user did not explicitly request. Do not answer with text."

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
	if strings.HasPrefix(s, "data:") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") {
		return s
	}
	return "data:image/png;base64," + s
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
	tool := map[string]any{
		"type": "image_generation",
	}
	if req.Size != "" {
		tool["size"] = req.Size
	}
	if req.Quality != "" {
		tool["quality"] = req.Quality
	}
	if req.Background != "" {
		tool["background"] = req.Background
	}
	// 客户端 model 字段若是 gpt-image-* 系列，透传到 tool 内部模型；否则让上游默认（gpt-image-1.5）。
	// 这样客户端可以主动尝试 gpt-image-2 等新模型而不用改插件代码。
	if strings.HasPrefix(strings.ToLower(req.Model), "gpt-image-") {
		tool["model"] = req.Model
	}
	if req.OutputFormat != "" {
		tool["output_format"] = req.OutputFormat
	}
	// /edits 独有字段：input_fidelity 控制参考图还原度；mask 走 tool 的 input_image_mask。
	if isEdit {
		if req.InputFidelity != "" {
			tool["input_fidelity"] = req.InputFidelity
		}
		if req.Mask != "" {
			tool["input_image_mask"] = map[string]any{"image_url": req.Mask}
		}
	}

	// input 必须是 list 形式（与 normalizeResponsesInput 的 string→list 包装一致），
	// 否则上游返回 "Input must be a list"。/edits 在同一条 user message 的 content
	// 里把 input_text 与 input_image 并列，image_generation tool 会拿去做图生图。
	content := []map[string]any{
		{"type": "input_text", "text": req.Prompt},
	}
	for _, url := range req.Images {
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": url,
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
		"instructions": imagesPassthroughInstructions,
		"input":        inputList,
		"tools":        []any{tool},
		"stream":       true,
		"store":        false,
	}
	msg, err := wrapResponseCreate(payload, imagesOAuthChatModel, session)
	if err != nil {
		return nil, 0, 0, err
	}
	// input token 估算：文本 prompt + 每张参考图按 size 低质档估算（~272 tokens/1024²），
	// 与 image_generation tool 输入图像的 token 级别一致。计费仍走 gpt-image-1.5
	// input 单价（$5/1M），V1 近似即可。
	inputTokens := estimatePromptTokens(req.Prompt) + estimateImageInputTokens(len(req.Images), req.Size)
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

	ka := startImageKeepAlive(req.Writer)

	handler := &imagesSilentHandler{accountID: account.ID, start: start}
	wsResult := ReceiveWSResponse(ctx, conn, handler)
	if wsResult.ResponseID != "" && session.SessionKey != "" {
		updateSessionStateResponseID(session.SessionKey, wsResult.ResponseID)
	}

	elapsed := time.Since(start)
	// 计费按 OpenAI Images API 官方口径：prompt tokens + 图像输出 tokens，统一按 gpt-image-1.5。
	// 上游 instructions / 工具调用包装产生的额外 chat tokens 由内层吸收（OAuth 订阅账号无逐 token 成本）。
	usage := &sdk.Usage{
		InputTokens:  promptTokens,
		Model:        imageToolCostModel,
		FirstTokenMs: handler.firstTokenMs,
	}

	if wsResult.Err != nil {
		var failure *responsesFailureError
		if errors.As(wsResult.Err, &failure) && failure.shouldReturnClientError() {
			body := buildImagesErrorBody(failure.StatusCode, failure.Message)
			if ka != nil {
				ka.Finish(failure.StatusCode, body)
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
		if ka != nil {
			ka.Stop()
		}
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
			Reason:   wsResult.Err.Error(),
			Duration: elapsed,
		}, wsResult.Err
	}

	if len(wsResult.ImageGenCalls) == 0 {
		body := buildImagesErrorBody(http.StatusBadGateway, "上游未返回图像结果")
		if ka != nil {
			ka.Finish(http.StatusBadGateway, body)
		}
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadGateway,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   fmt.Sprintf("image_generation_call 为空 (n=%d)", n),
			Duration: elapsed,
		}, fmt.Errorf("image_generation_call 为空 (n=%d)", n)
	}

	numImages := len(wsResult.ImageGenCalls)

	respBody := buildImagesRESTResponse(wsResult, promptTokens, 0)
	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}
	if ka != nil {
		ka.Finish(http.StatusOK, respBody)
	} else {
		outcome.Upstream.Body = respBody
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}

	fillUsageCostPerImage(usage, numImages)
	return outcome, nil
}

// buildImagesRESTResponse 按 OpenAI Images API 官方契约打包响应。
// 计费口径：prompt tokens + image output tokens，全部按 gpt-image-1.5 单价。
// instructions / 工具调用包装产生的额外 chat tokens 由内层吸收，不出现在对外 usage。
// 这样：
//  1. 客户端拿到的 usage 数字语义与 OpenAI 原生 Images API 完全一致
//  2. 外层再套一层 AirGate 时，两级按同一口径独立计算，金额零偏差
func buildImagesRESTResponse(wsResult WSResult, promptTokens, imageOutputTokens int) []byte {
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
		// root 级 model，供下一级 handleImagesResponse 做 fillCost 查价。
		"model": imageToolCostModel,
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
func handleImagesResponse(resp *http.Response, w http.ResponseWriter, ka *imageKeepAlive, start time.Time, fallbackModel string) (sdk.ForwardOutcome, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取 Images 响应失败: %v", err)
		if ka != nil {
			ka.Finish(http.StatusBadGateway, buildImagesErrorBody(http.StatusBadGateway, reason))
		}
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	parsed := parseUsage(body)
	headers := resp.Header.Clone()

	if ka != nil {
		ka.Finish(resp.StatusCode, body)
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
	if w == nil {
		outcome.Upstream.Body = body
	}
	return outcome, nil
}
