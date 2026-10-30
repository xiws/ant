// Package cli 是 L4 交互层与唯一组合根（D43）。
// Run 是唯一进程入口：解析参数、装配、执行、返回退出码（D5）。
package cli

import (
	"io"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/statemachine"
)

// Stdio 是进程 I/O 边界，便于测试注入。
type Stdio struct {
	In    io.Reader
	Out   io.Writer
	Err   io.Writer
	IsTTY bool
	Env   []string
}

// Overrides 是测试注入点；生产路径传零值。
type Overrides struct {
	Provider  provider.Provider // fake provider
	Prompter  approval.Prompter // 脚本化回答
	Clock     func() time.Time
	EventSink model.EventSink // 捕获事件做断言
	HomeDir   string
	WorkDir   string // 测试用：默认当前目录
	Config    config.View
}

// Renderer 是唯一接触终端输出的接口。
type Renderer interface {
	Delta(text string)     // 流式文本
	Reasoning(text string) // 推理增量（可折叠）
	ToolCall(name string, args []byte)
	ToolResult(name string, ok bool, dur time.Duration)
	Notice(msg string)
	TurnSummary(r statemachine.TurnResult, u logx.UsageAggregator)
}
