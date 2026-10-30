// Package agent 定义节点抽象、输出契约与共享工具循环
// （docs/05-nodes.md §7、docs/19-interfaces.md §21.11）。
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xiws/ant/internal/config"
	ctxmgr "github.com/xiws/ant/internal/context"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/prompt"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/tool"
)

// Node 是「独立 prompt + 工具权限 + 输出契约」的执行单元。
type Node interface {
	Name() model.NodeName
	Tools() []model.ToolName
	Contract() Contract
	Run(ctx context.Context, in NodeInput) (NodeOutput, error)
}

// NodeInput 由引擎按图填写；节点不得自己去 session 取数据。
type NodeInput struct {
	Request        string
	SessionSummary string
	Plan           string
	History        []model.Message
	Artifacts      []model.ArtifactRef
	Feedback       []model.Failure
	Prior          []PriorOutput
	Dimension      model.Dimension // 仅 deliberate
	Mode           model.Mode
}

// PriorOutput 是「上一个 step 的结果」；聚合节点靠它拿到三份 deliberate。
type PriorOutput struct {
	Node   model.NodeName
	Text   string
	Parsed Parsed
	Usage  model.Usage
}

// NodeOutput 是节点结果。
type NodeOutput struct {
	Text       string
	Structured json.RawMessage // §7.1 原字段：契约原文
	Parsed     Parsed          // 解析视图；verifier/deliberate/aggregation 必填
	Usage      model.Usage
	ToolCalls  int
	Changed    []string // write/edit 触及的文件（相对路径）
	Checks     []model.Check
}

// Contract 解析/校验结构化输出。
type Contract interface {
	Name() string
	Parse(ctx context.Context, text string) (Parsed, error)
}

// Parsed 同时保留原文与已识别的结构化视图。
type Parsed struct {
	Raw     json.RawMessage
	Verdict *model.Verdict     // verifier
	Issues  []model.Issue      // deliberate / aggregation
	Plan    *model.Aggregation // aggregation
}

// Registry 是节点名 → 节点。
type Registry interface {
	Get(name model.NodeName) (Node, bool)
	All() []Node
}

// LoopDetector 做重复调用与无进展判定（§13.5）。
type LoopDetector interface {
	Observe(node model.NodeName, call model.ToolCall) (LoopVerdict, error)
}

// LoopVerdict 是循环判定。
type LoopVerdict int

const (
	LoopOK LoopVerdict = iota
	LoopWarn
	LoopAbort
)

// Deps 是 baseNode 的协作者；由组合根注入。
type Deps struct {
	Provider     provider.Provider // Router，已含重试/回退
	Registry     provider.Registry // 解析本节点模型与能力
	Prompt       prompt.Assembler
	Context      ctxmgr.Preparer
	Runtime      tool.Runtime
	ToolRegistry tool.Registry
	Recorder     model.EventSink
	Store        model.ArtifactStore
	Loop         LoopDetector
	Clock        func() time.Time
	Config       config.View
	// Project 返回当前 project.md 投影；可为 nil。
	Project func() prompt.ProjectDoc
	// OnDelta 接收流式增量（cli.Renderer）；可为 nil。
	OnDelta func(provider.Delta) error
	// OnText 在非流式响应（或流未产生文本增量）时交出最终文本；可为 nil。
	OnText func(text string)
}

// NodeSpec 声明一个节点的静态属性。
type NodeSpec struct {
	Name         model.NodeName
	Tools        []model.ToolName
	Contract     Contract
	Dimension    model.Dimension // deliberate 用
	MaxToolTurns int             // 默认 limits.maxToolTurns
}
