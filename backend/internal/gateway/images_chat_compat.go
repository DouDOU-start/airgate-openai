package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
	sdk "github.com/DouDOU-start/airgate-sdk"
)

// forwardChatCompletionsAsImages 拦截下游平台（如 new-api）把图像模型
// 误路由到 /v1/chat/completions 的场景：自动提取 prompt、走 Images API
// 管线生成图片，再把结果包装回 Chat Completions 响应格式。
//
// 支持能力：
//   - 从 messages 提取 prompt 文本与图片附件（image_url → /edits 路径）
//   - 透传请求体中的 size / quality / n / background / output_format
//   - streaming（SSE chunk）与非 streaming 两种响应模式
func (g *OpenAIGateway) forwardChatCompletionsAsImages(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()

	prompt, imageRefs := extractChatImageInputs(req.Body)
	if prompt == "" {
		body := jsonError("messages 中未找到用户消息")
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
			Reason:   "image model via chat completions: no user message",
			Duration: time.Since(start),
		}, nil
	}

	isEdit := len(imageRefs) > 0

	// 透传客户端在 chat completions 请求体里附带的 images 参数
	imagePayload := buildChatCompatImagePayload(req.Body, req.Model, prompt, imageRefs)
	imageBody, _ := json.Marshal(imagePayload)

	imageReq := *req
	imageReq.Body = imageBody
	imageReq.Writer = nil
	imageReq.Headers = req.Headers.Clone()
	if isEdit {
		imageReq.Headers.Set("X-Forwarded-Path", "/v1/images/edits")
	} else {
		imageReq.Headers.Set("X-Forwarded-Path", "/v1/images/generations")
	}
	imageReq.Headers.Set("Content-Type", "application/json")

	streaming := req.Stream && req.Writer != nil

	// keepalive 策略：streaming 用 SSE comment，非 streaming 用 whitespace
	var ka *imageKeepAlive
	var sseKA *sseCommentKeepAlive
	if streaming {
		sseKA = startSSECommentKeepAlive(req.Writer)
	} else {
		ka = startImageKeepAlive(req.Writer)
	}

	outcome, err := g.dispatchImageRequest(ctx, req, &imageReq)

	if err != nil || outcome.Kind != sdk.OutcomeSuccess {
		if ka != nil {
			errBody := outcome.Upstream.Body
			if errBody == nil {
				errBody = buildImagesErrorBody(outcome.Upstream.StatusCode, "image generation failed")
			}
			ka.Finish(outcome.Upstream.StatusCode, errBody)
		}
		if sseKA != nil {
			sseKA.Stop()
			errMsg := outcome.Reason
			if errMsg == "" && err != nil {
				errMsg = err.Error()
			}
			if errMsg == "" {
				errMsg = "image generation failed"
			}
			g.logger.Warn("图片生成流式失败，已脱敏响应", "kind", outcome.Kind, "status_code", outcome.Upstream.StatusCode, "error", errMsg)
			writeSSEError(req.Writer, sanitizedImageSSEErrorMessage)
		}
		return outcome, err
	}

	if streaming {
		sseKA.Stop()
		chunks := imagesToChatCompletionChunks(outcome.Upstream.Body, req.Model)
		for _, chunk := range chunks {
			writeSSEData(req.Writer, chunk)
		}
		writeSSEDone(req.Writer)
		outcome.Upstream.Body = nil
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
	} else {
		chatBody := imagesToChatCompletion(outcome.Upstream.Body, req.Model)
		if ka != nil {
			ka.Finish(http.StatusOK, chatBody)
		}
		outcome.Upstream.Body = chatBody
		outcome.Upstream.Headers = http.Header{"Content-Type": []string{"application/json"}}
	}
	return outcome, nil
}

const sanitizedImageSSEErrorMessage = "请求暂时无法完成，请稍后重试"

// dispatchImageRequest 根据账号凭证类型分发到对应的图像生成管线。
func (g *OpenAIGateway) dispatchImageRequest(ctx context.Context, origReq *sdk.ForwardRequest, imageReq *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	account := origReq.Account
	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, imageReq, "")
	}
	if account.Credentials["access_token"] != "" {
		if shouldUseImagesWebReverse(account, origReq.Model) {
			return g.forwardImagesViaWebReverse(ctx, imageReq)
		}
		return g.forwardImagesViaResponsesTool(ctx, imageReq)
	}
	reason := "账号缺少 api_key 或 access_token"
	return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
}

// ──────────────────────────────────────────────────────
// 请求解析
// ──────────────────────────────────────────────────────

// isChatCompatImageModel 判断是否需要走 chat→images 兼容路径。
func isChatCompatImageModel(reqModel string) bool {
	return model.IsImageOnly(reqModel)
}

// extractChatImageInputs 从 messages 数组中提取最后一条 user 消息的
// 文本 prompt 以及所有 image_url 附件引用。
func extractChatImageInputs(body []byte) (string, []string) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return "", nil
	}
	var lastPrompt string
	var imageRefs []string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				switch part.Get("type").String() {
				case "text":
					lastPrompt = part.Get("text").String()
				case "image_url":
					if u := part.Get("image_url.url").String(); u != "" {
						imageRefs = append(imageRefs, u)
					}
				}
			}
		} else {
			lastPrompt = content.String()
		}
	}
	return strings.TrimSpace(lastPrompt), imageRefs
}

// extractPromptFromMessages 便捷封装，只取 prompt 文本。
func extractPromptFromMessages(body []byte) string {
	prompt, _ := extractChatImageInputs(body)
	return prompt
}

// buildChatCompatImagePayload 构造 /v1/images/generations 或 /edits 的请求体，
// 从 chat completions 请求体中透传 size / quality / n / background / output_format。
func buildChatCompatImagePayload(chatBody []byte, modelID, prompt string, imageRefs []string) map[string]any {
	payload := map[string]any{
		"prompt": prompt,
		"model":  modelID,
	}

	n := int(gjson.GetBytes(chatBody, "n").Int())
	if n <= 0 {
		n = 1
	}
	payload["n"] = n

	if size := gjson.GetBytes(chatBody, "size").String(); size != "" {
		payload["size"] = normalizeImageSizeForUpstream(size)
	}

	if v := gjson.GetBytes(chatBody, "quality").String(); v != "" {
		payload["quality"] = v
	}
	if v := gjson.GetBytes(chatBody, "background").String(); v != "" {
		payload["background"] = v
	}
	if v := gjson.GetBytes(chatBody, "output_format").String(); v != "" {
		payload["output_format"] = v
	}

	if len(imageRefs) > 0 {
		if len(imageRefs) == 1 {
			payload["image"] = imageRefs[0]
		} else {
			payload["image"] = imageRefs
		}
	}
	return payload
}

// ──────────────────────────────────────────────────────
// 响应转换：Images API → Chat Completions
// ──────────────────────────────────────────────────────

// imagesToChatCompletion 把 Images API 响应体转换为非流式 Chat Completions 格式。
func imagesToChatCompletion(imagesBody []byte, modelID string) []byte {
	created := gjson.GetBytes(imagesBody, "created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	content := buildImageContent(imagesBody)
	inputTokens, outputTokens := extractImageUsageTokens(imagesBody)

	resp := map[string]any{
		"id":      generateChatCmplID(),
		"object":  "chat.completion",
		"created": created,
		"model":   modelID,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	result, _ := json.Marshal(resp)
	return result
}

// imagesToChatCompletionChunks 把 Images API 响应体转换为 SSE chunk 序列。
func imagesToChatCompletionChunks(imagesBody []byte, modelID string) [][]byte {
	id := generateChatCmplID()
	created := gjson.GetBytes(imagesBody, "created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	content := buildImageContent(imagesBody)
	inputTokens, outputTokens := extractImageUsageTokens(imagesBody)

	chunk := func(delta map[string]any, finish *string, withUsage bool) []byte {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk",
			"created": created, "model": modelID,
			"choices": []map[string]any{{
				"index": 0,
				"delta": delta,
			}},
		}
		if finish != nil {
			c["choices"].([]map[string]any)[0]["finish_reason"] = *finish
		}
		if withUsage {
			c["usage"] = map[string]any{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			}
		}
		b, _ := json.Marshal(c)
		return b
	}

	stop := "stop"
	return [][]byte{
		chunk(map[string]any{"role": "assistant"}, nil, false),
		chunk(map[string]any{"content": content}, nil, false),
		chunk(map[string]any{}, &stop, true),
	}
}

// buildImageContent 从 Images API data 数组构造 markdown 图片内容。
func buildImageContent(imagesBody []byte) string {
	dataArr := gjson.GetBytes(imagesBody, "data")
	if !dataArr.Exists() || !dataArr.IsArray() {
		return "Image generation completed but no image data returned."
	}
	var parts []string
	for _, item := range dataArr.Array() {
		if b64 := item.Get("b64_json").String(); b64 != "" {
			parts = append(parts, fmt.Sprintf("![image](data:image/png;base64,%s)", b64))
		} else if u := item.Get("url").String(); u != "" {
			parts = append(parts, fmt.Sprintf("![image](%s)", u))
		}
	}
	if len(parts) == 0 {
		return "Image generation completed but no image data returned."
	}
	return strings.Join(parts, "\n\n")
}

// extractImageUsageTokens 从 Images API 响应中提取 token 用量，保证至少为 1。
func extractImageUsageTokens(imagesBody []byte) (inputTokens, outputTokens int) {
	inputTokens = int(gjson.GetBytes(imagesBody, "usage.input_tokens").Int())
	outputTokens = int(gjson.GetBytes(imagesBody, "usage.output_tokens").Int())
	if inputTokens+outputTokens == 0 {
		inputTokens = 1
	}
	return
}

// ──────────────────────────────────────────────────────
// SSE 写入辅助
// ──────────────────────────────────────────────────────

// sseCommentKeepAlive 在 SSE 流中周期发送 comment 行防止 Cloudflare 524 超时。
type sseCommentKeepAlive struct {
	w      http.ResponseWriter
	cancel context.CancelFunc
	done   chan struct{}
}

func startSSECommentKeepAlive(w http.ResponseWriter) *sseCommentKeepAlive {
	if w == nil {
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithCancel(context.Background())
	ka := &sseCommentKeepAlive{w: w, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ka.done)
		t := time.NewTicker(imageKeepAliveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = w.Write([]byte(": keepalive\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}()
	return ka
}

func (ka *sseCommentKeepAlive) Stop() {
	if ka == nil {
		return
	}
	ka.cancel()
	<-ka.done
}

func writeSSEData(w http.ResponseWriter, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEDone(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, _ string) {
	errEvent, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": sanitizedImageSSEErrorMessage,
			"type":    "server_error",
		},
	})
	writeSSEData(w, errEvent)
	writeSSEDone(w)
}
