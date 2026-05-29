package gateway

import (
	"context"
	"sort"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// TaskHandler 任务类型处理器。插件为每个任务类型实现此接口，注册到 TaskRegistry。
// 状态机由 Core 维护，插件只负责：构造 input → 执行业务 → 构造响应。
type TaskHandler interface {
	// Type 返回领域.动作 格式的任务类型，如 "image.generate"。
	Type() string

	// BuildInput 从转发请求中提取纯业务参数和展示属性。
	// input 只含业务字段（prompt/size/quality 等），不含路由元数据。
	// attributes 是少量字符串化展示/筛选维度。
	BuildInput(req *sdk.ForwardRequest, reqPath string) (input map[string]any, attributes map[string]string, err error)

	// Execute 执行任务业务逻辑。通过 rt 报告进度和结果，不直接操作状态机。
	Execute(ctx context.Context, g *OpenAIGateway, task sdk.HostTask, rt *TaskRuntime) error

	// BuildResponse 把 Core 返回的 HostTask 投影为当前协议的查询响应。
	BuildResponse(task *sdk.HostTask) map[string]any
}

// TaskRegistry 按任务类型注册和查找 handler。
type TaskRegistry struct {
	handlers map[string]TaskHandler
}

func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{handlers: make(map[string]TaskHandler)}
}

func (r *TaskRegistry) Register(h TaskHandler) {
	r.handlers[h.Type()] = h
}

func (r *TaskRegistry) Get(taskType string) TaskHandler {
	if r == nil {
		return nil
	}
	return r.handlers[taskType]
}

func (r *TaskRegistry) Types() []string {
	if r == nil {
		return nil
	}
	types := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}
