package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	hostMethodTasksCreate    = "tasks.create"
	hostMethodTasksUpdate    = "tasks.update"
	hostMethodTasksGet       = "tasks.get"
	hostMethodTasksList      = "tasks.list"
	hostMethodGatewayForward = "gateway.forward"
	hostMethodAssetsStore    = "assets.store"
	hostMethodAssetsStoreURL = "assets.store_url"
)

type hostForwardResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Usage      *sdk.Usage
	UsageID    int
}

type hostTaskListResponse struct {
	Tasks []*sdk.HostTask
	Total int
}

func (g *OpenAIGateway) hostInvoke(ctx context.Context, method string, payload map[string]interface{}) (map[string]interface{}, error) {
	if g.host == nil {
		return nil, fmt.Errorf("core host 未启用")
	}
	resp, err := g.host.Invoke(ctx, sdk.HostInvokeRequest{
		Method:  method,
		Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return map[string]interface{}{}, nil
	}
	if strings.EqualFold(resp.Status, "error") {
		if msg, _ := resp.Payload["message"].(string); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("core 方法 %s 返回错误", method)
	}
	return resp.Payload, nil
}

func (g *OpenAIGateway) createHostTask(ctx context.Context, taskType string, userID int64, input map[string]interface{}, attributes map[string]string, priority, maxAttempts int) (*sdk.HostTask, error) {
	payload := map[string]interface{}{
		"plugin_id":    PluginID,
		"task_type":    taskType,
		"user_id":      userID,
		"input":        input,
		"priority":     priority,
		"max_attempts": maxAttempts,
	}
	if len(attributes) > 0 {
		payload["attributes"] = attributes
	}
	payload, err := g.hostInvoke(ctx, hostMethodTasksCreate, payload)
	if err != nil {
		return nil, err
	}
	return hostTaskFromPayload(firstPayloadValue(payload, "task", "data", "result", ""))
}

type updateTaskOptions struct {
	Execution map[string]any
}

type UpdateTaskOption func(*updateTaskOptions)

func WithExecution(exec map[string]any) UpdateTaskOption {
	return func(o *updateTaskOptions) { o.Execution = exec }
}

func (g *OpenAIGateway) updateHostTask(ctx context.Context, taskID int64, status sdk.TaskStatus, progress int, output map[string]interface{}, errorMessage string, opts ...UpdateTaskOption) error {
	var o updateTaskOptions
	for _, fn := range opts {
		fn(&o)
	}
	payload := map[string]interface{}{
		"task_id": taskID,
	}
	if status != "" {
		payload["status"] = status.String()
	}
	if progress > 0 {
		payload["progress"] = progress
	}
	if output != nil {
		payload["output"] = output
	}
	if errorMessage != "" {
		payload["error_message"] = errorMessage
	}
	if o.Execution != nil {
		payload["execution"] = o.Execution
	}
	_, err := g.hostInvoke(ctx, hostMethodTasksUpdate, payload)
	return err
}

func (g *OpenAIGateway) getHostTask(ctx context.Context, userID, taskID int64) (*sdk.HostTask, error) {
	payload, err := g.hostInvoke(ctx, hostMethodTasksGet, map[string]interface{}{"task_id": taskID, "user_id": userID})
	if err != nil {
		return nil, err
	}
	return hostTaskFromPayload(firstPayloadValue(payload, "task", "data", "result", ""))
}

func (g *OpenAIGateway) listHostTasks(ctx context.Context, userID int64, taskType, status string, limit, offset int) (*hostTaskListResponse, error) {
	payload, err := g.hostInvoke(ctx, hostMethodTasksList, map[string]interface{}{
		"user_id":   userID,
		"task_type": taskType,
		"status":    status,
		"limit":     limit,
		"offset":    offset,
	})
	if err != nil {
		return nil, err
	}
	out := &hostTaskListResponse{Total: intFromAny(firstPayloadValue(payload, "total", "count"))}
	if tasks, ok := firstPayloadValue(payload, "tasks", "items", "data").([]interface{}); ok {
		for _, item := range tasks {
			task, err := hostTaskFromPayload(item)
			if err != nil {
				return nil, err
			}
			out.Tasks = append(out.Tasks, task)
		}
	}
	if out.Total == 0 {
		out.Total = len(out.Tasks)
	}
	return out, nil
}

func (g *OpenAIGateway) forwardViaHost(ctx context.Context, userID, groupID, apiKeyID int64, modelID, method, path string, headers http.Header, body []byte, stream bool) (*hostForwardResponse, error) {
	req := map[string]interface{}{
		"user_id":  userID,
		"group_id": groupID,
		"model":    modelID,
		"method":   method,
		"path":     path,
		"headers":  headerPayload(headers),
		"body":     string(body),
		"stream":   stream,
	}
	if apiKeyID > 0 {
		req["api_key_id"] = apiKeyID
	}
	payload, err := g.hostInvoke(ctx, hostMethodGatewayForward, req)
	if err != nil {
		return nil, err
	}
	return &hostForwardResponse{
		StatusCode: intFromAny(firstPayloadValue(payload, "status_code", "status")),
		Headers:    headerFromPayload(firstPayloadValue(payload, "headers")),
		Body:       bytesFromPayload(firstPayloadValue(payload, "body")),
		Usage:      usageFromPayload(firstPayloadValue(payload, "usage")),
		UsageID:    intFromAny(firstPayloadValue(payload, "usage_id")),
	}, nil
}

func (g *OpenAIGateway) storeAsset(ctx context.Context, userID int64, scope, contentType, fileExtension string, data []byte) (string, error) {
	payload, err := g.hostInvoke(ctx, hostMethodAssetsStore, map[string]interface{}{
		"user_id":        userID,
		"scope":          scope,
		"content_type":   contentType,
		"file_extension": fileExtension,
		"data":           data,
	})
	if err != nil {
		return "", err
	}
	return stringFromAny(firstPayloadValue(payload, "public_url")), nil
}

func (g *OpenAIGateway) storeAssetFromURL(ctx context.Context, userID int64, scope, sourceURL string) (string, error) {
	payload, err := g.hostInvoke(ctx, hostMethodAssetsStoreURL, map[string]interface{}{
		"user_id":    userID,
		"scope":      scope,
		"source_url": sourceURL,
	})
	if err != nil {
		return "", err
	}
	return stringFromAny(firstPayloadValue(payload, "public_url")), nil
}

func firstPayloadValue(payload map[string]interface{}, keys ...string) interface{} {
	if payload == nil {
		return nil
	}
	for _, key := range keys {
		if key == "" {
			return payload
		}
		if value, ok := payload[key]; ok {
			return value
		}
	}
	return nil
}

func hostTaskFromPayload(value interface{}) (*sdk.HostTask, error) {
	m, ok := mapFromAny(value)
	if !ok {
		return nil, fmt.Errorf("core 返回的任务结构无效")
	}
	task := &sdk.HostTask{
		ID:           int64FromAny(firstPayloadValue(m, "id", "task_id")),
		PluginID:     stringFromAny(firstPayloadValue(m, "plugin_id")),
		TaskType:     stringFromAny(firstPayloadValue(m, "task_type", "type")),
		Status:       sdk.TaskStatus(stringFromAny(firstPayloadValue(m, "status"))),
		UserID:       int64FromAny(firstPayloadValue(m, "user_id")),
		Input:        mapValueFromAny(firstPayloadValue(m, "input")),
		Output:       mapValueFromAny(firstPayloadValue(m, "output")),
		Execution:    mapValueFromAny(firstPayloadValue(m, "execution")),
		ErrorMessage: stringFromAny(firstPayloadValue(m, "error_message", "error")),
		Progress:     intFromAny(firstPayloadValue(m, "progress")),
		Attempts:     intFromAny(firstPayloadValue(m, "attempts")),
		MaxAttempts:  intFromAny(firstPayloadValue(m, "max_attempts")),
		CreatedAt:    timeFromAny(firstPayloadValue(m, "created_at")),
		UpdatedAt:    timeFromAny(firstPayloadValue(m, "updated_at")),
	}
	task.StartedAt = timePtrFromAny(firstPayloadValue(m, "started_at"))
	task.CompletedAt = timePtrFromAny(firstPayloadValue(m, "completed_at"))
	return task, nil
}

func headerPayload(headers http.Header) map[string]interface{} {
	out := make(map[string]interface{}, len(headers))
	for key, values := range headers {
		copied := append([]string(nil), values...)
		out[key] = copied
	}
	return out
}

func headerFromPayload(value interface{}) http.Header {
	headers := http.Header{}
	m, ok := mapFromAny(value)
	if !ok {
		return headers
	}
	for key, raw := range m {
		switch values := raw.(type) {
		case []interface{}:
			for _, item := range values {
				headers.Add(key, stringFromAny(item))
			}
		case []string:
			for _, item := range values {
				headers.Add(key, item)
			}
		default:
			if s := stringFromAny(values); s != "" {
				headers.Set(key, s)
			}
		}
	}
	return headers
}

func bytesFromPayload(value interface{}) []byte {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return v
	case string:
		if decoded, err := base64.StdEncoding.DecodeString(v); err == nil && looksLikeJSON(decoded) {
			return decoded
		}
		return []byte(v)
	default:
		body, _ := json.Marshal(v)
		return body
	}
}

func looksLikeJSON(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

func usageFromPayload(value interface{}) *sdk.Usage {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var usage sdk.Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil
	}
	return &usage
}

func mapFromAny(value interface{}) (map[string]interface{}, bool) {
	if value == nil {
		return nil, false
	}
	if m, ok := value.(map[string]interface{}); ok {
		return m, true
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false
	}
	return out, true
}

func mapValueFromAny(value interface{}) map[string]interface{} {
	m, _ := mapFromAny(value)
	return m
}

func stringFromAny(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func intFromAny(value interface{}) int {
	return int(int64FromAny(value))
}

func int64FromAny(value interface{}) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func timeFromAny(value interface{}) time.Time {
	if value == nil {
		return time.Time{}
	}
	if t, ok := value.(time.Time); ok {
		return t
	}
	raw := strings.TrimSpace(stringFromAny(value))
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0)
	}
	return time.Time{}
}

func timePtrFromAny(value interface{}) *time.Time {
	ts := timeFromAny(value)
	if ts.IsZero() {
		return nil
	}
	return &ts
}
