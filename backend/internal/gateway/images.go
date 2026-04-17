package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DouDOU-start/airgate-openai/backend/resources"
	sdk "github.com/DouDOU-start/airgate-sdk"
)

// imagesOAuthChatModel OAuth 下 REST→tools 翻译时使用的主 chat 模型。
// Codex 官方 $imagegen 技能同样用 gpt-5.4 作为主 model，image_generation 作为
// tool 内部用 gpt-image-1.5。
const imagesOAuthChatModel = "gpt-5.4"

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

// isImagesRequest 判断给定路径是否为 Images API 请求。
// 用于在 forwardAPIKey 响应解析阶段分流到 handleImagesResponse，
// 以及在 forwardHTTP 入口守卫 OAuth 账号不支持 Images 的场景。
func isImagesRequest(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/generations")
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

// buildImagesToolCreateMsg 把 Images REST 请求体翻译成 Responses API 的
// response.create 消息（tools 数组带一个 image_generation 项）。
// 返回：上游消息 bytes；n（当前固定 1）；prompt 估算的 token 数（用于计费）。
func buildImagesToolCreateMsg(
	body []byte,
	session openAISessionResolution,
) ([]byte, int, int, error) {
	prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if prompt == "" {
		return nil, 0, 0, fmt.Errorf("prompt 不能为空")
	}
	n := int(gjson.GetBytes(body, "n").Int())
	if n <= 0 {
		n = 1
	}
	// Responses API 的 image_generation tool 每次仅生成 1 张；n>1 在 REST 侧的语义
	// 需要多轮工具调用才能模拟，暂不支持 —— V1 限定 n=1。
	if n > 1 {
		return nil, 0, 0, fmt.Errorf("OAuth 模式下 n 只能为 1（REST→tools 翻译路径暂不支持多图）")
	}
	tool := map[string]any{
		"type": "image_generation",
	}
	if v := strings.TrimSpace(gjson.GetBytes(body, "size").String()); v != "" {
		tool["size"] = v
	}
	if v := strings.TrimSpace(gjson.GetBytes(body, "quality").String()); v != "" {
		tool["quality"] = v
	}
	if v := strings.TrimSpace(gjson.GetBytes(body, "background").String()); v != "" {
		tool["background"] = v
	}
	// 客户端 model 字段若是 gpt-image-* 系列，透传到 tool 内部模型；否则让上游默认（gpt-image-1.5）。
	// 这样客户端可以主动尝试 gpt-image-2 等新模型而不用改插件代码。
	if v := strings.TrimSpace(gjson.GetBytes(body, "model").String()); strings.HasPrefix(strings.ToLower(v), "gpt-image-") {
		tool["model"] = v
	}
	if v := strings.TrimSpace(gjson.GetBytes(body, "output_format").String()); v != "" {
		tool["output_format"] = v
	} else if v := strings.TrimSpace(gjson.GetBytes(body, "response_format").String()); v == "url" {
		// Images REST 的 response_format=url 在 OAuth 翻译路径下不可满足（上游返的是 base64），
		// 静默忽略，改由 b64_json 填充 data[]。
		_ = v
	}

	// input 必须是 list 形式（与 normalizeResponsesInput 的 string→list 包装一致），
	// 否则上游返回 "Input must be a list"。
	inputList := []map[string]any{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": prompt},
			},
		},
	}
	payload := map[string]any{
		"model":        imagesOAuthChatModel,
		"instructions": resources.Instructions,
		"input":        inputList,
		"tools":        []any{tool},
		"stream":       true,
		"store":        false,
	}
	msg, err := wrapResponseCreate(payload, imagesOAuthChatModel, session)
	if err != nil {
		return nil, 0, 0, err
	}
	return msg, n, estimatePromptTokens(prompt), nil
}

// forwardImagesViaResponsesTool 把 OpenAI Images REST 请求翻译成 Responses API
// 的 image_generation tool 调用，跑 OAuth WS 通道，最后把生成的 base64 图像
// 重新包装成 Images REST 响应返回给客户端。
//
// 只在 OAuth 账号被调度到 /v1/images/generations 时使用；API Key 账号继续走
// 原生 REST 通道（见 handleImagesResponse）。
func (g *OpenAIGateway) forwardImagesViaResponsesTool(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	session := resolveOpenAISession(req.Headers, req.Body)
	updateSessionStateFromRequest(session)

	createMsg, n, promptTokens, err := buildImagesToolCreateMsg(req.Body, session)
	if err != nil {
		body := jsonError(err.Error())
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadRequest)
			_, _ = req.Writer.Write(body)
		}
		return &sdk.ForwardResult{
			StatusCode:    http.StatusBadRequest,
			AccountStatus: sdk.AccountStatusOK,
			Body:          body,
			Headers:       http.Header{"Content-Type": []string{"application/json"}},
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
		if wsResp != nil && (wsResp.StatusCode == 401 || wsResp.StatusCode == 403 || wsResp.StatusCode == 429) {
			return &sdk.ForwardResult{
				StatusCode:    wsResp.StatusCode,
				Duration:      time.Since(start),
				AccountStatus: accountStatusFromMessage(wsResp.StatusCode, err.Error()),
				ErrorMessage:  err.Error(),
			}, err
		}
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	if err := conn.WriteJSON(json.RawMessage(createMsg)); err != nil {
		return nil, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
	}

	handler := &imagesSilentHandler{accountID: account.ID, start: start}
	wsResult := ReceiveWSResponse(ctx, conn, handler)
	if wsResult.ResponseID != "" && session.SessionKey != "" {
		updateSessionStateResponseID(session.SessionKey, wsResult.ResponseID)
	}

	elapsed := time.Since(start)
	// 计费按 OpenAI Images API 官方口径：只计用户的 prompt tokens（输入）+
	// 图像输出 tokens，统一按 gpt-image-1.5 单价。
	// 上游 instructions / 工具调用包装产生的额外 chat tokens 由内层吸收（OAuth
	// 账号在订阅制下无逐 token 成本，合情合理）；客户端看到的 usage 完全对齐
	// OpenAI 官方 Images API 契约。
	fwdResult := &sdk.ForwardResult{
		StatusCode:   http.StatusOK,
		InputTokens:  promptTokens,
		Model:        imageToolCostModel,
		Duration:     elapsed,
		FirstTokenMs: handler.firstTokenMs,
	}

	if wsResult.Err != nil {
		var failure *responsesFailureError
		if errors.As(wsResult.Err, &failure) && failure.shouldReturnClientError() {
			body := buildImagesErrorBody(failure.StatusCode, failure.Message)
			if req.Writer != nil {
				req.Writer.Header().Set("Content-Type", "application/json")
				req.Writer.WriteHeader(failure.StatusCode)
				_, _ = req.Writer.Write(body)
			}
			fwdResult.StatusCode = failure.StatusCode
			fwdResult.AccountStatus = sdk.AccountStatusOK
			fwdResult.Body = body
			fwdResult.Headers = http.Header{"Content-Type": []string{"application/json"}}
			return fwdResult, wsResult.Err
		}
		fwdResult.StatusCode = http.StatusBadGateway
		return fwdResult, wsResult.Err
	}

	if len(wsResult.ImageGenCalls) == 0 {
		body := buildImagesErrorBody(http.StatusBadGateway, "上游未返回图像结果")
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadGateway)
			_, _ = req.Writer.Write(body)
		}
		fwdResult.StatusCode = http.StatusBadGateway
		fwdResult.Body = body
		fwdResult.Headers = http.Header{"Content-Type": []string{"application/json"}}
		return fwdResult, fmt.Errorf("image_generation_call 为空 (n=%d)", n)
	}

	// ChatGPT OAuth 下 tool_usage.image_gen.output_tokens 永远 0（订阅账号不按 token
	// 计价），用 OpenAI 官方 size×quality→tokens 换算表反推。
	toolOut := wsResult.ToolImageOutputTokens
	if toolOut == 0 {
		toolOut = estimateImageGenOutputTokens(wsResult.ImageGenCalls)
	}
	fwdResult.OutputTokens = toolOut

	respBody := buildImagesRESTResponse(wsResult, promptTokens, toolOut)
	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(respBody)
	} else {
		fwdResult.Body = respBody
		fwdResult.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}

	// 全部按 gpt-image-1.5 单价算：prompt × $5/1M + image × $40/1M，
	// 与 OpenAI 原生 Images API 计费口径一致。
	fillCost(fwdResult)
	return fwdResult, nil
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
func handleImagesResponse(resp *http.Response, w http.ResponseWriter, start time.Time, fallbackModel string) (*sdk.ForwardResult, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 Images 响应失败: %w", err)
	}

	usage := parseUsage(body)

	if w != nil {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		model = fallbackModel
	}

	elapsed := time.Since(start)
	result := &sdk.ForwardResult{
		StatusCode:        resp.StatusCode,
		InputTokens:       usage.inputTokens,
		OutputTokens:      usage.outputTokens,
		CachedInputTokens: usage.cachedInputTokens,
		Model:             model,
		Duration:          elapsed,
		FirstTokenMs:      elapsed.Milliseconds(),
	}
	if w == nil {
		result.Body = body
		result.Headers = resp.Header.Clone()
	}
	fillCost(result)
	return result, nil
}
