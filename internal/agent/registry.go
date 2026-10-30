package agent

import (
	"context"
	"sync"
	"time"

	"github.com/xiws/ant/internal/model"
)

const defaultMaxToolTurns = 12

type nodeRegistry struct {
	byName map[model.NodeName]Node
	all    []Node
}

// NewRegistry 构造 5 个内置节点：共享 baseNode，仅 NodeSpec 不同（D43）。
func NewRegistry(deps Deps) Registry {
	deps = withDefaults(deps)
	maxTurns := defaultMaxToolTurns
	if deps.Config != nil {
		if n := deps.Config.Settings().Limits.MaxToolTurns; n > 0 {
			maxTurns = n
		}
	}
	readTools := []model.ToolName{
		model.ToolRead, model.ToolReadCode, model.ToolSearch, model.ToolBash, model.ToolAsk,
	}
	writeTools := append(append([]model.ToolName{}, readTools...), model.ToolEdit, model.ToolWrite)

	specs := []NodeSpec{
		{
			Name:         model.NodeCode,
			Tools:        writeTools,
			Contract:     textContract{node: model.NodeCode, required: []string{"## 变更", "## 已执行", "## 未解决"}},
			MaxToolTurns: maxTurns,
		},
		{
			Name:         model.NodeAsk,
			Tools:        readTools,
			Contract:     textContract{node: model.NodeAsk},
			MaxToolTurns: maxTurns,
		},
		{
			Name:         model.NodeVerifier,
			Tools:        readTools,
			Contract:     verifierContract{},
			MaxToolTurns: maxTurns,
		},
		{
			Name:         model.NodeDeliberate,
			Tools:        readTools,
			Contract:     deliberateContract{},
			MaxToolTurns: maxTurns,
		},
		{
			Name:         model.NodeAggregation,
			Tools:        readTools,
			Contract:     aggregationContract{},
			MaxToolTurns: maxTurns,
		},
	}

	reg := &nodeRegistry{byName: map[model.NodeName]Node{}, all: make([]Node, 0, len(specs))}
	for _, spec := range specs {
		n := &baseNode{deps: deps, spec: spec}
		reg.byName[spec.Name] = n
		reg.all = append(reg.all, n)
	}
	return reg
}

func (r *nodeRegistry) Get(name model.NodeName) (Node, bool) {
	n, ok := r.byName[name]
	return n, ok
}

func (r *nodeRegistry) All() []Node { return append([]Node(nil), r.all...) }

func withDefaults(deps Deps) Deps {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Loop == nil {
		deps.Loop = NewLoop(deps.Config)
	}
	if deps.Recorder == nil {
		deps.Recorder = model.EventSinkFunc(func(context.Context, *model.Event) error { return nil })
	}
	return deps
}

// StaticNode 是测试替身：按固定输出实现 Node（§21.18）。
type StaticNode struct {
	Spec   NodeSpec
	Output NodeOutput
	Err    error
	OnRun  func(in NodeInput)

	mu     sync.Mutex
	inputs []NodeInput
}

// Name 实现 Node。
func (n *StaticNode) Name() model.NodeName { return n.Spec.Name }

// Tools 实现 Node。
func (n *StaticNode) Tools() []model.ToolName { return n.Spec.Tools }

// Contract 实现 Node。
func (n *StaticNode) Contract() Contract { return n.Spec.Contract }

// Run 实现 Node。
func (n *StaticNode) Run(ctx context.Context, in NodeInput) (NodeOutput, error) {
	n.mu.Lock()
	n.inputs = append(n.inputs, in)
	n.mu.Unlock()
	if n.OnRun != nil {
		n.OnRun(in)
	}
	if n.Err != nil {
		return NodeOutput{}, n.Err
	}
	return n.Output, nil
}

// Inputs 返回已收到的输入副本。
func (n *StaticNode) Inputs() []NodeInput {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]NodeInput, len(n.inputs))
	copy(out, n.inputs)
	return out
}

// StaticRegistry 用固定节点构造 Registry，便于状态机单测。
func StaticRegistry(nodes ...Node) Registry {
	reg := &nodeRegistry{byName: map[model.NodeName]Node{}, all: make([]Node, 0, len(nodes))}
	for _, n := range nodes {
		reg.byName[n.Name()] = n
		reg.all = append(reg.all, n)
	}
	return reg
}
