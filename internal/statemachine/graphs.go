package statemachine

import (
	"fmt"
	"sort"

	"github.com/xiws/ant/internal/model"
)

// 图出口：不是可执行 step，只表示 turn 结束的位置（§21.12）。
//   - terminalEnd：成功出口；
//   - terminalEndFail：轮次耗尽后的失败出口；
//   - terminalGate：plan 模式通过验证后的 Plan gate（由引擎执行）。
const (
	terminalEnd     = "end"
	terminalEndFail = "end_fail"
	terminalGate    = "gate"
)

// isTerminal 报告 id 是否为图出口。
func isTerminal(id string) bool {
	return id == terminalEnd || id == terminalEndFail || id == terminalGate
}

// ValidateGraph 校验图的入口、边引用与可达性。
// 启动时（NewEngine）对四张内置图各做一次，避免配置错误在运行时才暴露。
func ValidateGraph(g Graph) error {
	if !g.Mode.Valid() {
		return fmt.Errorf("statemachine: 图模式 %q 非法", g.Mode)
	}
	if g.Entry == "" {
		return fmt.Errorf("statemachine: 图 %s 缺少 Entry", g.Mode)
	}
	if _, ok := g.Steps[g.Entry]; !ok {
		return fmt.Errorf("statemachine: 图 %s 的 Entry %q 不是已声明的 step", g.Mode, g.Entry)
	}

	ids := make([]string, 0, len(g.Steps))
	for id := range g.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := g.Steps[id]
		if st.ID != "" && st.ID != id {
			return fmt.Errorf("statemachine: 图 %s 的 step %q 声明 ID 为 %q", g.Mode, id, st.ID)
		}
		if st.Node == "" && len(st.Fanout) == 0 {
			return fmt.Errorf("statemachine: 图 %s 的 step %q 既没有 Node 也没有 Fanout", g.Mode, id)
		}
		if len(st.Fanout) > 0 {
			for _, br := range st.Fanout {
				if br.Node == "" {
					return fmt.Errorf("statemachine: 图 %s 的 step %q 有分支缺少节点名", g.Mode, id)
				}
			}
		}
	}

	for _, ed := range g.Edges {
		if _, ok := g.Steps[ed.From]; !ok {
			return fmt.Errorf("statemachine: 图 %s 的边起点 %q 不存在", g.Mode, ed.From)
		}
		if _, ok := g.Steps[ed.To]; !ok && !isTerminal(ed.To) {
			return fmt.Errorf("statemachine: 图 %s 的边终点 %q 不存在", g.Mode, ed.To)
		}
	}

	// 入口可达性：每个 step 都必须能从 Entry 沿边到达。
	seen := map[string]bool{g.Entry: true}
	queue := []string{g.Entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ed := range g.Edges {
			if ed.From != cur || seen[ed.To] || isTerminal(ed.To) {
				continue
			}
			seen[ed.To] = true
			queue = append(queue, ed.To)
		}
	}
	unreachable := make([]string, 0)
	for _, id := range ids {
		if !seen[id] {
			unreachable = append(unreachable, id)
		}
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		return fmt.Errorf("statemachine: 图 %s 中不可达的 step：%v", g.Mode, unreachable)
	}
	return nil
}

// BuiltinGraphs 返回四张内置模式图（§21.12 的表）。
// 调用方拿到的是新副本，可以安全修改而不影响其他调用。
func BuiltinGraphs() map[model.Mode]Graph {
	return map[model.Mode]Graph{
		model.ModeCode:    codeGraph(),
		model.ModePlan:    planGraph(),
		model.ModeAsk:     askGraph(),
		model.ModeCouncil: councilGraph(),
	}
}

// code：code → verifier；pass 结束，fail 回到 code，达上限失败。
func codeGraph() Graph {
	return Graph{
		Mode:  model.ModeCode,
		Entry: "code",
		Steps: map[string]Step{
			"code":     {ID: "code", Node: model.NodeCode},
			"verifier": {ID: "verifier", Node: model.NodeVerifier},
		},
		Edges: []Edge{
			{From: "code", When: CondAlways, To: "verifier"},
			{From: "verifier", When: CondPass, To: terminalEnd},
			{From: "verifier", When: CondFailUnderRound, To: "code"},
			{From: "verifier", When: CondFail, To: terminalEndFail},
		},
	}
}

// plan：ask → fanout(deliberate×3) → aggregation → verifier；pass 后进入 Plan gate。
func planGraph() Graph {
	return Graph{
		Mode:  model.ModePlan,
		Entry: "ask",
		Steps: map[string]Step{
			"ask": {ID: "ask", Node: model.NodeAsk},
			"fanout": {
				ID: "fanout",
				Fanout: []FanoutBranch{
					{Node: model.NodeDeliberate, Dimension: model.DimCorrectness},
					{Node: model.NodeDeliberate, Dimension: model.DimRisk},
					{Node: model.NodeDeliberate, Dimension: model.DimExecution},
				},
			},
			"aggregation": {ID: "aggregation", Node: model.NodeAggregation},
			"verifier":    {ID: "verifier", Node: model.NodeVerifier},
		},
		Edges: []Edge{
			{From: "ask", When: CondAlways, To: "fanout"},
			{From: "fanout", When: CondAlways, To: "aggregation"},
			{From: "aggregation", When: CondAlways, To: "verifier"},
			{From: "verifier", When: CondPass, To: terminalGate},
			{From: "verifier", When: CondFailUnderRound, To: "ask"},
			{From: "verifier", When: CondFail, To: terminalEndFail},
		},
	}
}

// ask：单节点回答，直接结束。
func askGraph() Graph {
	return Graph{
		Mode:  model.ModeAsk,
		Entry: "ask",
		Steps: map[string]Step{
			"ask": {ID: "ask", Node: model.NodeAsk},
		},
		Edges: []Edge{
			{From: "ask", When: CondAlways, To: terminalEnd},
		},
	}
}

// council：code → fanout(deliberate×3) → aggregation → verifier；fail 回到 code。
func councilGraph() Graph {
	return Graph{
		Mode:  model.ModeCouncil,
		Entry: "code",
		Steps: map[string]Step{
			"code": {ID: "code", Node: model.NodeCode},
			"fanout": {
				ID: "fanout",
				Fanout: []FanoutBranch{
					{Node: model.NodeDeliberate, Dimension: model.DimCorrectness},
					{Node: model.NodeDeliberate, Dimension: model.DimRisk},
					{Node: model.NodeDeliberate, Dimension: model.DimExecution},
				},
			},
			"aggregation": {ID: "aggregation", Node: model.NodeAggregation},
			"verifier":    {ID: "verifier", Node: model.NodeVerifier},
		},
		Edges: []Edge{
			{From: "code", When: CondAlways, To: "fanout"},
			{From: "fanout", When: CondAlways, To: "aggregation"},
			{From: "aggregation", When: CondAlways, To: "verifier"},
			{From: "verifier", When: CondPass, To: terminalEnd},
			{From: "verifier", When: CondFailUnderRound, To: "code"},
		},
	}
}
