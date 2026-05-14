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
	// taskExecHeader 递归守卫：ProcessTask 通过 host.Forward 回调本网关时携带此头，
	// forwardHTTP 看到后走原始同步转发，不再创建新任务。
	taskExecHeader = "X-Airgate-Task-Execution"
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

func (rt *TaskRuntime) Complete(ctx context.Context, output map[string]any) error {
	return rt.g.updateHostTask(ctx, rt.taskID, sdk.TaskStatusCompleted, 100, output, "")
}

func (rt *TaskRuntime) Fail(ctx context.Context, taskErr *TaskError) error {
	msg := taskErr.Message
	if taskErr.Type != "" {
		msg = fmt.Sprintf("[%s] %s", taskErr.Type, msg)
	}
	rt.logger.Warn("task_failed", "task_id", rt.taskID, "error_type", taskErr.Type, "error", msg)
	return rt.g.updateHostTask(ctx, rt.taskID, sdk.TaskStatusFailed, 0, nil, msg)
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

	resp := map[string]any{"task_id": task.ID, "status": "pending"}
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

// isTaskExecution 检测当前请求是否由 ProcessTask 通过 host.Forward 发起（递归守卫）。
func isTaskExecution(headers http.Header) bool {
	return headers.Get(taskExecHeader) == "true"
}

// ── Task HTTP 查询 ──

func (g *OpenAIGateway) handleTaskQuery(ctx context.Context, req *sdk.ForwardRequest, handler TaskHandler) (sdk.ForwardOutcome, error) {
	taskIDStr := ""
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

	userID, _ := strconv.ParseInt(req.Headers.Get("X-Airgate-User-ID"), 10, 64)
	task, err := g.getHostTask(ctx, userID, taskID)
	if err != nil {
		return writeJSONOutcome(req.Writer, http.StatusInternalServerError, sdk.OutcomeUpstreamTransient, jsonError("查询任务失败: "+err.Error())), nil
	}

	respBody, _ := json.Marshal(handler.BuildResponse(task))
	return writeJSONOutcome(req.Writer, http.StatusOK, sdk.OutcomeSuccess, respBody), nil
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
