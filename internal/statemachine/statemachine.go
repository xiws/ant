// Package statemachine 用「图数据 + 统一求值器」实现四种模式
// （docs/06-state-machine.md §8、docs/19-interfaces.md §21.12）。
package statemachine

import (
	"context"
	"time"

	"github.com/xiws/ant/internal/agent"
	"github.com/xiws/ant/internal/model"
)

// Engine 负责 turn 生命周期、模式切换与恢复。
type Engine interface {
	RunTurn(ctx context.Context, in TurnInput) (TurnResult, error)
	ResumeTurn(ctx context.Context, st ResumeState) (TurnResult, error)
	SwitchMode(ctx context.Context, to model.Mode, reason string, confirmed bool) error
	Mode() model.Mode
}

// TurnInput 是一次用户输入。
type TurnInput struct {
	UserInput string
	History   []model.Message
	Round     int // 续跑时的当前轮次；新 turn 为 0
	// Summary 是 session 折叠出的压缩摘要；引擎转交 NodeInput.SessionSummary。
	Summary string
}

// TurnResult 是 turn 结果。
type TurnResult struct {
	Status   Status
	Usage    model.Usage
	Changed  []string
	Checks   []model.Check
	PlanPath string
	ExitCode int // 0/1/3/4/5（§8.9）
}

// Status 是 turn 状态。
type Status string

const (
	StatusOK          Status = "ok"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// ResumeState 是 session.State 的编排视图。
type ResumeState struct {
	Turn    int
	Step    model.StepRef
	Round   int
	Done    []string // 已完成 step ID
	History []model.Message
	Summary string
}

// Plan 是 Plan gate 的输入。
type Plan struct {
	Path  string
	Body  string
	Round int
}

// Graph 是数据，不是代码分支：四张内置图共用同一求值器。
type Graph struct {
	Mode  model.Mode
	Entry string
	Steps map[string]Step
	Edges []Edge
}

// Step 是一次节点执行，或一个并发组。
type Step struct {
	ID     string
	Node   model.NodeName
	Fanout []FanoutBranch
}

// FanoutBranch 描述并发组中的一个分支。
type FanoutBranch struct {
	Node      model.NodeName
	Dimension model.Dimension
}

// Edge 是转移。
type Edge struct {
	From string
	When Condition
	To   string
}

// Condition 是转移条件。
type Condition string

const (
	CondAlways         Condition = "always"
	CondPass           Condition = "pass"
	CondFail           Condition = "fail"
	CondFailUnderRound Condition = "fail_under_round"
	CondNoProgress     Condition = "no_progress"
)

// StepRunner 执行单个 step。
type StepRunner interface {
	RunStep(ctx context.Context, st Step, in agent.NodeInput) (StepOutcome, error)
}

// StepOutcome 是 step 结果。
type StepOutcome struct {
	Node     model.NodeName
	Output   agent.NodeOutput
	Parsed   agent.Parsed
	Duration time.Duration
}

// PlanGate 在 plan 通过后询问用户。
type PlanGate interface {
	Confirm(ctx context.Context, plan Plan) (bool, error)
}

// Budget 是 turn 内轮次、工具轮数、token 与超时的唯一计数来源。
type Budget interface {
	TakeToolTurn() bool
	Round() int
	Remaining() time.Duration
	SpentTokens() int
}
