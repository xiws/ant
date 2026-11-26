package web

import (
	"context"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/model"
)

// RunRequest 是 Web 发起的一次真实 turn。
type RunRequest struct {
	SessionID string     `json:"-"`
	Input     string     `json:"input"`
	Mode      model.Mode `json:"mode,omitempty"`
	Model     string     `json:"model,omitempty"`
}

// RunHooks 把执行器的实时输出接到 Web 的 SSE 与审批桥。
type RunHooks struct {
	Prompter    approval.Prompter
	OnSessionID func(id string)
	OnEvent     func(*model.Event)
	OnDelta     func(text, reasoning string)
}

// RunOutcome 是一轮执行的结论。错误事件已经由执行器持久化；ExitCode
// 用于前端显示终态并触发权威投影刷新。
type RunOutcome struct {
	ExitCode int
}

// Executor 由组合根注入。Web 包只定义接口，不直接依赖 cli/agent/tool 实现。
type Executor interface {
	Run(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error)
}
