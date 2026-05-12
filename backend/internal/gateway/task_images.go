package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	imageTaskType = "image_generation"

	// taskExecHeader 递归守卫：ProcessTask 通过 host.Forward 回调本网关时携带此头，
	// forwardHTTP 看到后走原始同步转发，不再创建新任务。
	taskExecHeader = "X-Airgate-Task-Execution"
)

// ── TaskProcessor 接口 ──

func (g *OpenAIGateway) TaskTypes() []string {
	return []string{imageTaskType}
}

func (g *OpenAIGateway) ProcessTask(ctx context.Context, task sdk.HostTask) error {
	logger := g.logger.With("task_id", task.ID, "task_type", task.TaskType)

	if task.Attempts > 1 {
		logger.Info("image_task_redispatch", "attempts", task.Attempts)
	} else {
		logger.Info("processing image task")
	}

	if err := g.updateHostTask(ctx, task.ID, sdk.TaskStatusProcessing, 10, nil, ""); err != nil {
		return err
	}

	groupID, _ := intFromInput(task.Input, "group_id")
	model, _ := task.Input["model"].(string)
	requestPath, _ := task.Input["request_path"].(string)
	if requestPath == "" {
		requestPath = "/v1/images/generations"
	}

	reqBody, err := buildTaskRequestBody(task.Input)
	if err != nil {
		g.failTask(ctx, task.ID, logger, "构建请求体失败: "+err.Error())
		return err
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set(taskExecHeader, "true")

	if err := g.updateHostTask(ctx, task.ID, sdk.TaskStatusProcessing, 30, nil, ""); err != nil {
		return err
	}

	resp, err := g.forwardViaHost(ctx, task.UserID, groupID, model, http.MethodPost, requestPath, headers, reqBody, false)
	if err != nil {
		g.failTask(ctx, task.ID, logger, "upstream 转发失败: "+err.Error())
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errMsg := fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(resp.Body, &errResp) == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		}
		g.failTask(ctx, task.ID, logger, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	output := map[string]interface{}{
		"status_code": resp.StatusCode,
		"body":        json.RawMessage(resp.Body),
	}
	if resp.Usage != nil {
		output["model"] = resp.Usage.Model
		if inputTokens := usageMetricInt(resp.Usage, usageMetricInputTokens); inputTokens > 0 {
			output["input_tokens"] = inputTokens
		}
		if outputTokens := usageMetricInt(resp.Usage, usageMetricOutputTokens); outputTokens > 0 {
			output["output_tokens"] = outputTokens
		}
		if resp.Usage.AccountCost > 0 {
			output["cost"] = resp.Usage.AccountCost
		}
	}

	if err := g.updateHostTask(ctx, task.ID, sdk.TaskStatusCompleted, 100, output, ""); err != nil {
		return err
	}

	logger.Info("image task completed")
	return nil
}

// ── Forward 路径：创建任务 + 同步等待 ──

// forwardImagesViaTask 创建 Core Task 后立即返回 task_id，不阻塞 HTTP 连接。
// 客户端通过 GET /v1/images/tasks?task_id=xxx 轮询结果。
func (g *OpenAIGateway) forwardImagesViaTask(ctx context.Context, req *sdk.ForwardRequest, reqPath string) (sdk.ForwardOutcome, error) {
	logger := sdk.LoggerFromContext(ctx)

	task, err := g.createImageTask(ctx, req, reqPath)
	if err != nil {
		logger.Error("create_image_task_failed", sdk.LogFieldError, err)
		body := jsonError("创建图片任务失败: " + err.Error())
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusInternalServerError)
			_, _ = req.Writer.Write(body)
		}
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusInternalServerError, Body: body},
			Reason:   "创建图片任务失败",
		}, nil
	}

	logger.Info("image_task_created", "task_id", task.ID)

	resp := map[string]interface{}{
		"task_id": task.ID,
		"status":  "pending",
	}
	respBody, _ := json.Marshal(resp)

	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusAccepted)
		_, _ = req.Writer.Write(respBody)
	}

	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusAccepted,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       respBody,
		},
	}, nil
}

func (g *OpenAIGateway) createImageTask(ctx context.Context, req *sdk.ForwardRequest, reqPath string) (*sdk.HostTask, error) {
	parsed, err := parseImagesRequest(req.Body, req.Headers.Get("Content-Type"), isImagesEditRequest(reqPath))
	if err != nil {
		return nil, fmt.Errorf("解析图片请求失败: %w", err)
	}

	userID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-User-ID"), 10, 64)
	groupID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-Group-ID"), 10, 64)

	model := parsed.Model
	if model == "" {
		model = req.Model
	}

	inputMap := map[string]interface{}{
		"group_id":     groupID,
		"model":        model,
		"request_path": reqPath,
		"prompt":       parsed.Prompt,
		"n":            parsed.N,
	}
	if parsed.Size != "" {
		inputMap["size"] = parsed.Size
	}
	if parsed.Quality != "" {
		inputMap["quality"] = parsed.Quality
	}
	if parsed.Background != "" {
		inputMap["background"] = parsed.Background
	}
	if parsed.OutputFormat != "" {
		inputMap["output_format"] = parsed.OutputFormat
	}
	if parsed.InputFidelity != "" {
		inputMap["input_fidelity"] = parsed.InputFidelity
	}
	if parsed.IsEdit {
		inputMap["is_edit"] = true
	}
	if len(parsed.Images) > 0 {
		inputMap["images"] = parsed.Images
	}
	if parsed.Mask != "" {
		inputMap["mask"] = parsed.Mask
	}

	return g.createHostTask(ctx, imageTaskType, userID, inputMap, 0, 3)
}

// ── Task 状态查询（GET /v1/images/tasks?task_id=xxx） ──

func (g *OpenAIGateway) handleImageTaskQuery(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	taskIDStr := req.Headers.Get("X-Forwarded-Path")
	if qs := req.Headers.Get("X-Forwarded-Query"); qs != "" {
		for _, pair := range strings.Split(qs, "&") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 && kv[0] == "task_id" {
				taskIDStr = kv[1]
			}
		}
	}
	if taskIDStr == "" || !isNumeric(taskIDStr) {
		var body struct {
			TaskID int64 `json:"task_id"`
		}
		if json.Unmarshal(req.Body, &body) == nil && body.TaskID > 0 {
			taskIDStr = strconv.FormatInt(body.TaskID, 10)
		}
	}

	taskID, err := strconv.ParseInt(taskIDStr, 10, 64)
	if err != nil || taskID <= 0 {
		return writeJSONOutcome(req.Writer, http.StatusBadRequest, sdk.OutcomeClientError, jsonError("缺少有效的 task_id 参数")), nil
	}

	task, err := g.getHostTask(ctx, taskID)
	if err != nil {
		return writeJSONOutcome(req.Writer, http.StatusInternalServerError, sdk.OutcomeUpstreamTransient, jsonError("查询任务失败: "+err.Error())), nil
	}

	respBody, _ := json.Marshal(buildTaskResponse(task))
	return writeJSONOutcome(req.Writer, http.StatusOK, sdk.OutcomeSuccess, respBody), nil
}

// ── Task 历史列表（GET /v1/images/tasks/list） ──

func (g *OpenAIGateway) handleImageTaskList(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	userID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-User-ID"), 10, 64)
	if userID <= 0 {
		return writeJSONOutcome(req.Writer, http.StatusBadRequest, sdk.OutcomeClientError, jsonError("缺少用户信息")), nil
	}

	limit, offset, status := 20, 0, ""
	if qs := req.Headers.Get("X-Forwarded-Query"); qs != "" {
		for _, pair := range strings.Split(qs, "&") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "limit":
				if v, err := strconv.Atoi(kv[1]); err == nil && v > 0 && v <= 100 {
					limit = v
				}
			case "offset":
				if v, err := strconv.Atoi(kv[1]); err == nil && v >= 0 {
					offset = v
				}
			case "status":
				status = kv[1]
			}
		}
	}

	result, err := g.listHostTasks(ctx, userID, imageTaskType, status, limit, offset)
	if err != nil {
		return writeJSONOutcome(req.Writer, http.StatusInternalServerError, sdk.OutcomeUpstreamTransient, jsonError("查询任务列表失败: "+err.Error())), nil
	}

	tasks := make([]map[string]interface{}, 0, len(result.Tasks))
	for _, t := range result.Tasks {
		tasks = append(tasks, buildTaskResponse(t))
	}

	resp := map[string]interface{}{
		"tasks": tasks,
		"total": result.Total,
	}
	respBody, _ := json.Marshal(resp)
	return writeJSONOutcome(req.Writer, http.StatusOK, sdk.OutcomeSuccess, respBody), nil
}

// buildTaskResponse 构造统一的任务响应结构。
func buildTaskResponse(task *sdk.HostTask) map[string]interface{} {
	resp := map[string]interface{}{
		"task_id":    task.ID,
		"status":     string(task.Status),
		"progress":   task.Progress,
		"created_at": task.CreatedAt,
	}
	if task.CompletedAt != nil {
		resp["completed_at"] = *task.CompletedAt
	}
	if task.Output != nil {
		if images, ok := task.Output["images"]; ok {
			resp["images"] = images
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
	}
	if task.ErrorMessage != "" {
		resp["error"] = task.ErrorMessage
	}
	return resp
}

// writeJSONOutcome 写 JSON 响应并返回 ForwardOutcome。
func writeJSONOutcome(w http.ResponseWriter, statusCode int, kind sdk.OutcomeKind, body []byte) sdk.ForwardOutcome {
	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write(body)
	}
	return sdk.ForwardOutcome{
		Kind:     kind,
		Upstream: sdk.UpstreamResponse{StatusCode: statusCode, Body: body},
	}
}

// ── helpers ──

func (g *OpenAIGateway) failTask(ctx context.Context, taskID int64, logger *slog.Logger, errMsg string) {
	logger.Error("image_task_failed", "error", errMsg)
	if err := g.updateHostTask(ctx, taskID, sdk.TaskStatusFailed, 0, nil, errMsg); err != nil {
		logger.Error("task_fail_update_error", sdk.LogFieldError, err)
	}
}

func buildTaskRequestBody(input map[string]interface{}) ([]byte, error) {
	body := map[string]interface{}{
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
	// 图生图字段
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

func intFromInput(input map[string]interface{}, key string) (int64, bool) {
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

func stringSliceFromInput(input map[string]interface{}, key string) []string {
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

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// isTaskExecution 检测当前请求是否由 ProcessTask 通过 host.Forward 发起（递归守卫）。
func isTaskExecution(headers http.Header) bool {
	return headers.Get(taskExecHeader) == "true"
}
