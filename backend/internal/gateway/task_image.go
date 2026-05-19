package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	taskTypeImageGenerate = "image.generate"
	taskTypeImageEdit     = "image.edit"
)

// ── image.generate handler ──

type imageGenerateHandler struct{}

func (h imageGenerateHandler) Type() string { return taskTypeImageGenerate }

func (h imageGenerateHandler) BuildInput(req *sdk.ForwardRequest, reqPath string) (map[string]any, map[string]string, error) {
	return buildImageTaskInput(req, reqPath, false)
}

func (h imageGenerateHandler) Execute(ctx context.Context, g *OpenAIGateway, task sdk.HostTask, rt *TaskRuntime) error {
	return executeImageTask(ctx, g, task, rt, "/v1/images/generations")
}

func (h imageGenerateHandler) BuildResponse(task *sdk.HostTask) map[string]any {
	return buildImageTaskResponse(task)
}

// ── image.edit handler ──

type imageEditHandler struct{}

func (h imageEditHandler) Type() string { return taskTypeImageEdit }

func (h imageEditHandler) BuildInput(req *sdk.ForwardRequest, _ string) (map[string]any, map[string]string, error) {
	return buildImageTaskInput(req, "", true)
}

func (h imageEditHandler) Execute(ctx context.Context, g *OpenAIGateway, task sdk.HostTask, rt *TaskRuntime) error {
	return executeImageTask(ctx, g, task, rt, "/v1/images/edits")
}

func (h imageEditHandler) BuildResponse(task *sdk.HostTask) map[string]any {
	return buildImageTaskResponse(task)
}

// ── 共享实现 ──

func buildImageTaskInput(req *sdk.ForwardRequest, reqPath string, isEdit bool) (map[string]any, map[string]string, error) {
	parsed, err := parseImagesRequest(req.Body, req.Headers.Get("Content-Type"), isEdit)
	if err != nil {
		return nil, nil, fmt.Errorf("解析图片请求失败: %w", err)
	}

	model := parsed.Model
	if model == "" {
		model = req.Model
	}

	// input 只含纯业务参数
	input := map[string]any{
		"prompt": parsed.Prompt,
		"model":  model,
		"n":      parsed.N,
	}
	if parsed.Size != "" {
		input["size"] = parsed.Size
	}
	if parsed.Quality != "" {
		input["quality"] = parsed.Quality
	}
	if parsed.Background != "" {
		input["background"] = parsed.Background
	}
	if parsed.OutputFormat != "" {
		input["output_format"] = parsed.OutputFormat
	}
	if parsed.InputFidelity != "" {
		input["input_fidelity"] = parsed.InputFidelity
	}
	if len(parsed.Images) > 0 {
		input["images"] = parsed.Images
	}
	if parsed.Mask != "" {
		input["mask"] = parsed.Mask
	}

	// 路由元数据：group_id 由 Core 在创建任务时从上下文获取，
	// 但当前 Core 还需要插件传递，暂时放在 input 内。
	groupID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-Group-ID"), 10, 64)
	if groupID > 0 {
		input["group_id"] = groupID
	}
	apiKeyID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-API-Key-ID"), 10, 64)
	if apiKeyID > 0 {
		input["api_key_id"] = apiKeyID
	}

	// attributes 是少量展示/筛选维度
	attributes := map[string]string{
		"model": model,
	}
	if parsed.Size != "" {
		attributes["size"] = parsed.Size
	}
	if parsed.Quality != "" {
		attributes["quality"] = parsed.Quality
	}

	return input, attributes, nil
}

func executeImageTask(ctx context.Context, g *OpenAIGateway, task sdk.HostTask, rt *TaskRuntime, defaultPath string) error {
	groupID, _ := intFromInput(task.Input, "group_id")
	apiKeyID, _ := intFromInput(task.Input, "api_key_id")
	model, _ := task.Input["model"].(string)

	isRedispatch := task.Attempts > 1
	hasUpstreamID := false
	if task.Execution != nil {
		if uid, ok := task.Execution["upstream_task_id"].(string); ok && uid != "" {
			hasUpstreamID = true
			_ = uid
		}
	}

	// 重启重派：没有上游 task_id 的任务无法恢复，直接标记失败
	if isRedispatch && !hasUpstreamID {
		rt.logger.Info("task_redispatch_no_upstream_id", "task_id", task.ID)
		return rt.Fail(ctx, &TaskError{
			Type:    "upstream_error",
			Message: "任务在服务重启期间中断，请重新发起",
		})
	}

	shrinkTaskInputImages(task.Input)

	reqBody, err := buildImageRequestBody(task.Input)
	if err != nil {
		return rt.Fail(ctx, &TaskError{
			Type:    "invalid_request",
			Message: "构建请求体失败: " + err.Error(),
		})
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set(taskExecHeader, "true")
	headers.Set(taskIDHeader, strconv.FormatInt(task.ID, 10))

	if hasUpstreamID {
		uid := task.Execution["upstream_task_id"].(string)
		headers.Set(upstreamTaskIDHeader, uid)
		rt.logger.Info("task_retry_with_upstream_id", "upstream_task_id", uid)
	}

	if err := rt.SetProgress(ctx, 30); err != nil {
		return err
	}

	resp, err := g.forwardViaHost(ctx, task.UserID, groupID, apiKeyID, model, http.MethodPost, defaultPath, headers, reqBody, false)
	if err != nil {
		if isSafetyRejectionText(err.Error()) {
			return rt.Fail(ctx, &TaskError{
				Type:    "invalid_request",
				Code:    "safety_rejected",
				Message: err.Error(),
			})
		}
		return rt.Fail(ctx, &TaskError{
			Type:      "upstream_error",
			Message:   "upstream 转发失败: " + err.Error(),
			Retryable: !isRedispatch,
		})
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		taskErr := classifyUpstreamTaskError(resp.StatusCode, resp.Body)
		return rt.Fail(ctx, taskErr)
	}

	if err := rt.SetProgress(ctx, 70); err != nil {
		return err
	}

	content, err := storeImageAssetsFromResponse(ctx, g, task.UserID, resp.Body)
	if err != nil {
		rt.logger.Warn("store_image_assets_failed", "error", err, "body_len", len(resp.Body))
		return rt.Fail(ctx, &TaskError{
			Type:    "upstream_error",
			Message: "上游响应中未包含可用图片: " + err.Error(),
		})
	}
	if content == "" {
		rt.logger.Warn("store_image_assets_empty", "body_len", len(resp.Body))
		return rt.Fail(ctx, &TaskError{
			Type:    "upstream_error",
			Message: "图片存储失败，所有图片均未能保存",
		})
	}

	output := map[string]any{
		"content": content,
	}
	if resp.Usage != nil {
		output["model"] = resp.Usage.Model
		if v := usageMetricInt(resp.Usage, usageMetricInputTokens); v > 0 {
			output["input_tokens"] = v
		}
		if v := usageMetricInt(resp.Usage, usageMetricOutputTokens); v > 0 {
			output["output_tokens"] = v
		}
		if resp.Usage.AccountCost > 0 {
			output["cost"] = resp.Usage.AccountCost
		}
	}
	if resp.UsageID > 0 {
		output["usage_id"] = resp.UsageID
	}

	return rt.Complete(ctx, output)
}

// classifyUpstreamTaskError 把上游 HTTP 错误映射为结构化 TaskError。
func classifyUpstreamTaskError(statusCode int, body []byte) *TaskError {
	msg, errType, errCode := parseUpstreamTaskErrorBody(statusCode, body)
	if isSafetyRejectionText(errType, errCode, msg) {
		return &TaskError{Type: "invalid_request", Code: "safety_rejected", Message: msg}
	}

	switch {
	case statusCode == 429 || isTemporaryRateLimitText(msg):
		return &TaskError{Type: "rate_limited", Code: "rate_limited", Message: msg, Retryable: true}
	case statusCode == 401 || statusCode == 403:
		return &TaskError{Type: "auth_error", Code: "auth_failed", Message: msg}
	case statusCode >= 500:
		return &TaskError{Type: "upstream_error", Code: "server_error", Message: msg, Retryable: true}
	case statusCode == 400:
		return &TaskError{Type: "invalid_request", Code: "bad_request", Message: msg}
	default:
		return &TaskError{Type: "upstream_error", Code: fmt.Sprintf("http_%d", statusCode), Message: msg}
	}
}

func parseUpstreamTaskErrorBody(statusCode int, body []byte) (message, errType, errCode string) {
	message = fmt.Sprintf("upstream HTTP %d", statusCode)
	if len(body) == 0 {
		return message, "", ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		if text := strings.TrimSpace(string(body)); text != "" {
			message = truncate(text, 500)
		}
		return message, "", ""
	}
	errValue, ok := payload["error"]
	if !ok {
		return message, "", ""
	}
	if errObj, ok := errValue.(map[string]any); ok {
		if v := strings.TrimSpace(stringFromAny(errObj["message"])); v != "" {
			message = v
		}
		errType = strings.TrimSpace(stringFromAny(errObj["type"]))
		errCode = strings.TrimSpace(stringFromAny(errObj["code"]))
		return message, errType, errCode
	}
	if v := strings.TrimSpace(stringFromAny(errValue)); v != "" {
		message = v
	}
	return message, "", ""
}

// storeImageAssetsFromResponse 解析 OpenAI Images API 响应，把图片存到 Core 资产存储，
// 返回 markdown 格式的本地 URL（如 "![image](/assets-runtime/...)\n"）。
func storeImageAssetsFromResponse(ctx context.Context, g *OpenAIGateway, userID int64, body []byte) (string, error) {
	var resp struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse images response: %w", err)
	}
	if len(resp.Data) == 0 {
		return "", fmt.Errorf("response data array is empty (body length=%d)", len(body))
	}

	logger := sdk.LoggerFromContext(ctx)
	scope := fmt.Sprintf("openai/images/user-%d", userID)
	var sb strings.Builder
	for i, item := range resp.Data {
		var localURL string
		var err error
		switch {
		case item.B64JSON != "":
			data, decErr := base64.StdEncoding.DecodeString(item.B64JSON)
			if decErr != nil {
				logger.Warn("store_image_b64_decode_failed", "index", i, "error", decErr)
				continue
			}
			localURL, err = g.storeAsset(ctx, userID, scope, "image/png", ".png", data)
			if err != nil {
				logger.Warn("store_image_asset_failed", "index", i, "b64_len", len(item.B64JSON), "error", err)
			}
		case item.URL != "" && (strings.HasPrefix(item.URL, "http://") || strings.HasPrefix(item.URL, "https://")):
			localURL, err = g.storeAssetFromURL(ctx, userID, scope, item.URL)
			if err != nil {
				logger.Warn("store_image_from_url_failed", "index", i, "error", err)
			}
		default:
			logger.Warn("store_image_no_data", "index", i,
				"has_b64", item.B64JSON != "", "has_url", item.URL != "")
			continue
		}
		if err != nil || localURL == "" {
			continue
		}
		fmt.Fprintf(&sb, "![image](%s)\n", localURL)
	}
	return strings.TrimSpace(sb.String()), nil
}

func buildImageTaskResponse(task *sdk.HostTask) map[string]any {
	taskID := externalTaskID(task)
	resp := map[string]any{
		"task_id":    taskID,
		"status":     string(task.Status),
		"progress":   task.Progress,
		"created_at": task.CreatedAt,
	}
	if taskID != "" {
		resp["status_url"] = imageTaskLocation("/v1/images/tasks", taskID)
	}
	if task.CompletedAt != nil {
		resp["completed_at"] = *task.CompletedAt
	}
	if task.Output != nil {
		if content, ok := task.Output["content"].(string); ok && content != "" {
			resp["result_content"] = content
		}
		if model, ok := task.Output["model"]; ok {
			resp["model"] = model
		}
		for _, key := range []string{"input_tokens", "output_tokens", "cost"} {
			if v, ok := task.Output[key]; ok {
				resp[key] = v
			}
		}
	}
	if task.Input != nil {
		if prompt, ok := task.Input["prompt"]; ok {
			resp["prompt"] = prompt
		}
		if size, ok := task.Input["size"]; ok {
			resp["image_size"] = size
		}
		if quality, ok := task.Input["quality"]; ok {
			resp["quality"] = quality
		}
	}
	if task.ErrorMessage != "" {
		resp["error"] = task.ErrorMessage
	}
	return resp
}

func buildImageRequestBody(input map[string]any) ([]byte, error) {
	body := map[string]any{
		"prompt": input["prompt"],
	}
	if v, ok := input["model"]; ok && v != "" {
		body["model"] = v
	}
	if v, _ := intFromInput(input, "n"); v > 0 {
		body["n"] = v
	}
	for _, key := range []string{"size", "quality", "background", "output_format", "input_fidelity"} {
		if v, ok := input[key].(string); ok && v != "" {
			body[key] = v
		}
	}
	if images := stringSliceFromInput(input, "images"); len(images) > 0 {
		if len(images) == 1 {
			body["image"] = images[0]
		} else {
			body["image"] = images
		}
	}
	if v, ok := input["mask"].(string); ok && v != "" {
		body["mask"] = v
	}
	return json.Marshal(body)
}

func intFromInput(input map[string]any, key string) (int64, bool) {
	v, ok := input[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func stringSliceFromInput(input map[string]any, key string) []string {
	v, ok := input[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// shrinkTaskInputImages 对任务输入中的图片做压缩，复用直通路径的 shrinkDataImageURL。
// mask 保持 PNG 不压缩（需要透明通道）。
func shrinkTaskInputImages(input map[string]any) {
	images := stringSliceFromInput(input, "images")
	if len(images) == 0 {
		return
	}
	for i, ref := range images {
		if shrunk, err := shrinkDataImageURL(ref, maxEditInputImageBytes); err == nil {
			images[i] = shrunk
		}
	}
	input["images"] = images
}
