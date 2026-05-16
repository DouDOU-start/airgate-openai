package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	// taskExecHeader 递归守卫：ProcessTask 通过 host.Forward 回调本网关时携带此头，
	// forwardHTTP 看到后走原始同步转发，不再创建新任务。
	taskExecHeader = "X-Airgate-Task-Execution"
	// taskIDHeader 携带 core task ID，forward 层用它保存上游异步 task_id 到 execution。
	taskIDHeader = "X-Airgate-Task-ID"
	// upstreamTaskIDHeader 携带上次保存的上游异步 task_id，forward 层看到后直接 poll 恢复。
	upstreamTaskIDHeader = "X-Airgate-Upstream-Task-ID"
)

// TaskError 结构化任务错误。Core 可按 Type 决定是否重试。
type TaskError struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *TaskError) Error() string { return e.Message }

// TaskRuntime 供 handler 安全地更新任务状态。
// 状态机由 Core 校验，runtime 只负责发送状态更新请求。
type TaskRuntime struct {
	g      *OpenAIGateway
	taskID int64
	logger *slog.Logger
}

func (rt *TaskRuntime) SetProgress(ctx context.Context, progress int) error {
	return rt.g.updateHostTask(ctx, rt.taskID, "", progress, nil, "")
}

func (rt *TaskRuntime) SaveExecution(ctx context.Context, execution map[string]any) error {
	return rt.g.updateHostTask(ctx, rt.taskID, "", 0, nil, "", WithExecution(execution))
}

func (rt *TaskRuntime) Complete(ctx context.Context, output map[string]any) error {
	return rt.g.updateHostTask(ctx, rt.taskID, sdk.TaskStatusCompleted, 100, output, "")
}

func (rt *TaskRuntime) Fail(ctx context.Context, taskErr *TaskError) error {
	msg := taskErr.Message
	if taskErr.Type != "" {
		msg = fmt.Sprintf("[%s] %s", taskErr.Type, msg)
	}
	rt.logger.Warn("task_failed", "task_id", rt.taskID, "error_type", taskErr.Type, "error", msg)
	userMsg := sanitizeTaskMessage(taskErr)
	return rt.g.updateHostTask(ctx, rt.taskID, sdk.TaskStatusFailed, 0, nil, userMsg)
}

func sanitizeTaskMessage(taskErr *TaskError) string {
	msg := taskErr.Message
	if strings.Contains(msg, "rpc error:") {
		if desc := extractGRPCDesc(msg); desc != "" {
			return desc
		}
		return userFriendlyFallback(taskErr.Type)
	}
	if strings.Contains(msg, "upstream 转发失败") {
		return userFriendlyFallback(taskErr.Type)
	}
	return msg
}

func userFriendlyFallback(errType string) string {
	switch errType {
	case "rate_limited":
		return "当前请求过多，请稍后重试"
	case "auth_error":
		return "账号认证失败，请联系管理员"
	default:
		return "请求暂时无法完成，请稍后重试"
	}
}

func extractGRPCDesc(s string) string {
	const prefix = "desc = "
	if i := strings.Index(s, prefix); i >= 0 {
		return strings.TrimSpace(s[i+len(prefix):])
	}
	return ""
}

// ── ProcessTask 分发入口 ──

func (g *OpenAIGateway) TaskTypes() []string {
	return g.tasks.Types()
}

func (g *OpenAIGateway) ProcessTask(ctx context.Context, task sdk.HostTask) error {
	handler := g.tasks.Get(task.TaskType)
	if handler == nil {
		return fmt.Errorf("不支持的任务类型: %s", task.TaskType)
	}

	logger := g.logger.With("task_id", task.ID, "task_type", task.TaskType)

	// Fetch full task from core to get Execution and Attempts (gRPC only sends minimal fields).
	full, err := g.getHostTask(ctx, 0, task.ID)
	if err == nil && full != nil {
		task.Attempts = full.Attempts
		task.Execution = full.Execution
	}

	if task.Attempts > 1 {
		logger.Info("task_redispatch", "attempts", task.Attempts)
	} else {
		logger.Info("task_start")
	}

	if err := g.updateHostTask(ctx, task.ID, sdk.TaskStatusProcessing, 10, nil, ""); err != nil {
		return err
	}

	rt := &TaskRuntime{g: g, taskID: task.ID, logger: logger}
	if err := handler.Execute(ctx, g, task, rt); err != nil {
		logger.Error("task_execute_failed", sdk.LogFieldError, err)
		return err
	}

	logger.Info("task_completed")
	return nil
}

// ── Forward 路径：创建任务 ──

func (g *OpenAIGateway) forwardTask(ctx context.Context, req *sdk.ForwardRequest, reqPath string, handler TaskHandler) (sdk.ForwardOutcome, error) {
	logger := sdk.LoggerFromContext(ctx)

	input, attributes, err := handler.BuildInput(req, reqPath)
	if err != nil {
		logger.Error("task_build_input_failed", sdk.LogFieldError, err)
		body := jsonError("创建任务失败: " + err.Error())
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadRequest)
			_, _ = req.Writer.Write(body)
		}
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadRequest, Body: body},
			Reason:   err.Error(),
		}, nil
	}

	userID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-User-ID"), 10, 64)
	task, err := g.createHostTask(ctx, handler.Type(), userID, input, attributes, 0, 3)
	if err != nil {
		logger.Error("task_create_failed", sdk.LogFieldError, err)
		body := jsonError("创建任务失败: " + err.Error())
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusInternalServerError)
			_, _ = req.Writer.Write(body)
		}
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusInternalServerError, Body: body},
			Reason:   "创建任务失败",
		}, nil
	}

	logger.Info("task_created", "task_id", task.ID, "task_type", handler.Type())

	publicTaskID := externalTaskID(task)
	location := imageTaskLocation(reqPath, publicTaskID)
	resp := map[string]any{
		"object":     "image.task",
		"task_id":    publicTaskID,
		"status":     "pending",
		"status_url": location,
	}
	respBody, _ := json.Marshal(resp)
	headers := http.Header{"Content-Type": []string{"application/json"}}
	headers.Set("Preference-Applied", "respond-async")
	headers.Set("Location", location)

	if req.Writer != nil {
		for key, values := range headers {
			for _, value := range values {
				req.Writer.Header().Add(key, value)
			}
		}
		req.Writer.WriteHeader(http.StatusAccepted)
		_, _ = req.Writer.Write(respBody)
	}
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusAccepted,
			Headers:    headers,
			Body:       respBody,
		},
	}, nil
}

func imageTaskLocation(reqPath string, taskID string) string {
	if strings.HasPrefix(reqPath, "/images/") {
		return "/images/tasks?task_id=" + url.QueryEscape(taskID)
	}
	return "/v1/images/tasks?task_id=" + url.QueryEscape(taskID)
}

// isTaskExecution 检测当前请求是否由 ProcessTask 通过 host.Forward 发起（递归守卫）。
func isTaskExecution(headers http.Header) bool {
	return headers.Get(taskExecHeader) == "true"
}

// ── Task HTTP 查询 ──

func (g *OpenAIGateway) handleTaskQuery(ctx context.Context, req *sdk.ForwardRequest, handler TaskHandler) (sdk.ForwardOutcome, error) {
	taskIDStr := imageTaskIDFromRequest(req)
	if taskIDStr == "" {
		return writeJSONOutcome(req.Writer, http.StatusBadRequest, sdk.OutcomeClientError, jsonError("缺少有效的 task_id 参数")), nil
	}

	userID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-User-ID"), 10, 64)
	var (
		task *sdk.HostTask
		err  error
	)
	if isNumeric(taskIDStr) {
		taskID, parseErr := strconv.ParseInt(taskIDStr, 10, 64)
		if parseErr != nil || taskID <= 0 {
			return writeJSONOutcome(req.Writer, http.StatusBadRequest, sdk.OutcomeClientError, jsonError("缺少有效的 task_id 参数")), nil
		}
		task, err = g.getHostTask(ctx, userID, taskID)
	} else {
		task, err = g.getHostTaskByPublicTaskID(ctx, userID, taskIDStr)
	}
	if err != nil {
		return writeJSONOutcome(req.Writer, http.StatusInternalServerError, sdk.OutcomeUpstreamTransient, jsonError("查询任务失败: "+err.Error())), nil
	}

	respBody, _ := json.Marshal(handler.BuildResponse(task))
	return writeJSONOutcome(req.Writer, http.StatusOK, sdk.OutcomeSuccess, respBody), nil
}

func imageTaskIDFromRequest(req *sdk.ForwardRequest) string {
	if req == nil {
		return ""
	}
	if qs := req.Headers.Get("X-Forwarded-Query"); qs != "" {
		if values, err := url.ParseQuery(qs); err == nil {
			if taskID := strings.TrimSpace(values.Get("task_id")); taskID != "" {
				return taskID
			}
		}
	}
	if len(req.Body) == 0 {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return ""
	}
	return stringIDFromAny(body["task_id"])
}

func (g *OpenAIGateway) handleTaskList(ctx context.Context, req *sdk.ForwardRequest, handler TaskHandler) (sdk.ForwardOutcome, error) {
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

	result, err := g.listHostTasks(ctx, userID, handler.Type(), status, limit, offset)
	if err != nil {
		return writeJSONOutcome(req.Writer, http.StatusInternalServerError, sdk.OutcomeUpstreamTransient, jsonError("查询任务列表失败: "+err.Error())), nil
	}

	tasks := make([]map[string]any, 0, len(result.Tasks))
	for _, t := range result.Tasks {
		tasks = append(tasks, handler.BuildResponse(t))
	}

	resp := map[string]any{"tasks": tasks, "total": result.Total}
	respBody, _ := json.Marshal(resp)
	return writeJSONOutcome(req.Writer, http.StatusOK, sdk.OutcomeSuccess, respBody), nil
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

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func externalTaskID(task *sdk.HostTask) string {
	if task == nil {
		return ""
	}
	if task.PublicTaskID != "" {
		return task.PublicTaskID
	}
	return strconv.FormatInt(task.ID, 10)
}

func stringIDFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
	case json.Number:
		return strings.TrimSpace(v.String())
	}
	return ""
}
