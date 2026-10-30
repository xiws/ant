package statemachine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/agent"
	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

// fanoutLimit 是并发组上限（§7.6）。
const fanoutLimit = 3

type engine struct {
	cfg     config.View
	nodes   agent.Registry
	sink    model.EventSink
	gate    PlanGate
	log     logx.Logger
	graphs  map[model.Mode]Graph
	initErr error

	mu       sync.Mutex
	mode     model.Mode
	turn     int
	planText string
	history  []model.Message
	summary  string
	budget   *turnBudget // 当前 turn 预算；每个 turn 开始时重置（§8.1）
}

// NewEngine 构造 turn 生命周期引擎；启动时校验四张内置图。
func NewEngine(cfg config.View, nodes agent.Registry, sink model.EventSink, gate PlanGate, log logx.Logger) Engine {
	if log == nil {
		log = logx.Nop()
	}
	graphs := BuiltinGraphs()
	e := &engine{
		cfg:    cfg,
		nodes:  nodes,
		sink:   sink,
		gate:   gate,
		log:    log,
		graphs: graphs,
		mode:   model.ModeCode,
	}
	var errs []string
	for _, m := range []model.Mode{model.ModeCode, model.ModePlan, model.ModeAsk, model.ModeCouncil} {
		if err := ValidateGraph(graphs[m]); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		e.initErr = model.NewError(model.ErrConfigInvalid, "graph_invalid", strings.Join(errs, "；")).
			WithHint("检查 builtin 模式图的 Entry/Steps/Edges")
		log.Error("内置模式图校验失败", "error", e.initErr)
	}
	if cfg != nil {
		if dm := cfg.Settings().DefaultMode; dm.Valid() {
			e.mode = dm
		}
	}
	e.budget = newTurnBudget(cfg, e.maxToolTurns())
	return e
}

func (e *engine) ready() error {
	if e.initErr != nil {
		return e.initErr
	}
	if len(e.graphs) == 0 {
		return model.NewError(model.ErrInternal, "graphs_missing", "内置模式图缺失")
	}
	return nil
}

// RunTurn 执行一个完整 turn。
func (e *engine) RunTurn(ctx context.Context, in TurnInput) (TurnResult, error) {
	if err := e.ready(); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 2}, err
	}
	tc := e.beginRunTurn(in)
	defer e.endTurn(tc)
	return e.execute(ctx, tc)
}

// ResumeTurn 从断点继续：跳过 Done 中的 step，不重跑已完成节点。
func (e *engine) ResumeTurn(ctx context.Context, st ResumeState) (TurnResult, error) {
	if err := e.ready(); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 2}, err
	}
	tc := e.beginResumeTurn(st)
	g, ok := e.graphs[tc.mode]
	if !ok {
		return e.failTurn(ctx, tc, "", "graph_missing", model.ErrConfigInvalid,
			fmt.Sprintf("没有模式 %s 的内置图", tc.mode), 2)
	}
	tc.start = resumeStart(g, st.Step, tc.done)
	tc.current = tc.start
	defer e.endTurn(tc)
	return e.execute(ctx, tc)
}

// SwitchMode 切换模式；未确认时返回 ErrApprovalRequired 类错误。
func (e *engine) SwitchMode(ctx context.Context, to model.Mode, reason string, confirmed bool) error {
	if !to.Valid() {
		return model.Errorf(model.ErrConfigInvalid, "invalid_mode", "未知模式 %q", to).
			WithHint("可用模式：code/plan/ask/council")
	}
	if !confirmed {
		return &model.Error{
			Class:   model.ErrApprovalRequired,
			Code:    "mode_switch_unconfirmed",
			Message: fmt.Sprintf("切换到 %s 模式需要用户确认", to),
			Hint:    "在交互模式确认，或显式传入 --yes",
			Details: map[string]any{"to": string(to), "reason": reason},
		}
	}

	e.mu.Lock()
	from := e.mode
	e.mode = to
	e.budget = newTurnBudget(e.cfg, e.maxToolTurns())
	e.mu.Unlock()

	if from == to {
		return nil
	}
	return e.emit(ctx, &model.Event{
		Type: model.EventModeChanged,
		Data: model.MustJSON(map[string]any{
			"from":            string(from),
			"to":              string(to),
			"reason":          reason,
			"confirmedByUser": confirmed,
		}),
	})
}

// Mode 返回当前模式。
func (e *engine) Mode() model.Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// beginRunTurn 初始化新 turn：history 深拷贝、UserInput 作为本轮 user、预算初始化。
func (e *engine) beginRunTurn(in TurnInput) *turnCtx {
	e.mu.Lock()
	e.turn++
	turn := e.turn
	mode := e.mode
	plan := e.planText
	e.mu.Unlock()

	base := cloneMessages(in.History)
	msgs := []model.Message{{Role: model.RoleUser, Content: in.UserInput}}
	tc := &turnCtx{
		turn:     turn,
		mode:     mode,
		user:     in.UserInput,
		base:     base,
		msgs:     msgs,
		planText: plan,
		summary:  in.Summary,
		round:    in.Round,
		done:     map[string]bool{},
		byNode:   map[model.NodeName]StepOutcome{},
	}
	tc.budget = e.takeBudget(tc.round)
	return tc
}

// beginResumeTurn 从 ResumeState 恢复 turn/round/history。
func (e *engine) beginResumeTurn(st ResumeState) *turnCtx {
	e.mu.Lock()
	turn := st.Turn
	if turn <= 0 {
		e.turn++
		turn = e.turn
	} else {
		e.turn = turn
	}
	mode := e.mode
	plan := e.planText
	e.mu.Unlock()

	history := cloneMessages(st.History)
	if len(history) == 0 {
		// ResumeState 未带历史时回落到引擎保存的上一个 turn 历史。
		e.mu.Lock()
		history = cloneMessages(e.history)
		e.mu.Unlock()
	}
	done := make(map[string]bool, len(st.Done))
	for _, id := range st.Done {
		if id != "" {
			done[id] = true
		}
	}
	tc := &turnCtx{
		turn:     turn,
		mode:     mode,
		user:     lastUserContent(history),
		msgs:     history,
		planText: plan,
		summary:  st.Summary,
		round:    st.Round,
		done:     done,
		resumed:  true,
		byNode:   map[model.NodeName]StepOutcome{},
	}
	tc.budget = e.takeBudget(tc.round)
	return tc
}

// takeBudget 返回本 turn 的预算实例（重置计数，保证 Budget 是唯一计数来源）。
func (e *engine) takeBudget(round int) *turnBudget {
	e.mu.Lock()
	b := e.budget
	if b == nil {
		b = newTurnBudget(e.cfg, e.maxToolTurns())
		e.budget = b
	}
	e.mu.Unlock()
	b.reset(round)
	return b
}

// endTurn 合并本 turn 消息到引擎历史，并保留 plan 文本供后续 code turn 使用。
func (e *engine) endTurn(tc *turnCtx) {
	e.mu.Lock()
	defer e.mu.Unlock()
	merged := cloneMessages(tc.base)
	merged = append(merged, cloneMessages(tc.msgs)...)
	e.history = merged
	if tc.mode == model.ModePlan && strings.TrimSpace(tc.planText) != "" {
		e.planText = tc.planText
	}
}

func (e *engine) execute(ctx context.Context, tc *turnCtx) (TurnResult, error) {
	g, ok := e.graphs[tc.mode]
	if !ok {
		return e.failTurn(ctx, tc, "", "graph_missing", model.ErrConfigInvalid,
			fmt.Sprintf("没有模式 %s 的内置图", tc.mode), 2)
	}
	if err := e.emit(auditCtx(ctx), &model.Event{
		Type: model.EventTurnStarted,
		Turn: tc.turn,
		Data: e.turnStartedData(tc),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	cur := tc.start
	if cur == "" {
		cur = g.Entry
	}
	return e.walk(ctx, tc, g, cur)
}

// walk 是统一求值器：图只是数据，四种模式共用这段循环。
func (e *engine) walk(ctx context.Context, tc *turnCtx, g Graph, cur string) (TurnResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return e.interruptTurn(ctx, tc, cur, err)
		}
		if isTerminal(cur) {
			switch cur {
			case terminalEnd:
				return e.succeedTurn(ctx, tc, cur)
			case terminalEndFail:
				return e.failTurn(ctx, tc, cur, "verification_failed", model.ErrVerificationFailed,
					"达到轮次上限仍未通过验证", 1)
			case terminalGate:
				return e.planGate(ctx, tc, g)
			}
		}

		step, ok := g.Steps[cur]
		if !ok {
			return e.failTurn(ctx, tc, cur, "graph_invalid", model.ErrInternal,
				fmt.Sprintf("图 %s 的 step %q 不存在", g.Mode, cur), 1)
		}
		if step.ID == "" {
			step.ID = cur
		}
		tc.current = step.ID

		// 恢复：Done 中的 step 不重跑，按图推进。
		if tc.done[step.ID] {
			next, ok := nextAfterDone(g, step.ID, tc.done)
			if !ok {
				return e.succeedTurn(ctx, tc, step.ID)
			}
			cur = next
			continue
		}

		if step.ID == g.Entry {
			tc.enterRound()
			if limit := e.maxRounds(tc.mode); limit > 0 && tc.round > limit {
				return e.failTurn(ctx, tc, step.ID, "round_limit", model.ErrVerificationFailed,
					fmt.Sprintf("模式 %s 的轮次上限 %d 已用尽", tc.mode, limit), 1)
			}
		}

		outcomes, err := e.runStep(ctx, tc, step)
		if err != nil {
			return e.stepFailed(ctx, tc, step.ID, err)
		}
		e.applyOutcomes(tc, step, outcomes)

		// 无进展：本轮 code 没有 write/edit 改动且 verifier 失败 → 立即中止。
		if step.Node == model.NodeVerifier && tc.verdict != nil && !tc.verdict.Pass &&
			tc.roundHasCode && !tc.roundWrote {
			return e.failTurn(ctx, tc, step.ID, "no_progress", model.ErrVerificationFailed,
				"本轮未产生 write/edit 改动且验证失败，继续循环无意义", 1)
		}

		next, matched := e.nextStep(g, step, tc)
		if !matched {
			if tc.verdict != nil && !tc.verdict.Pass {
				return e.failTurn(ctx, tc, step.ID, "verification_failed", model.ErrVerificationFailed,
					"验证失败且轮次已用尽", 1)
			}
			return e.failTurn(ctx, tc, step.ID, "no_transition", model.ErrInternal,
				fmt.Sprintf("step %q 没有匹配的转移边", step.ID), 1)
		}
		cur = next
	}
}

// runStep 执行一个 step：单节点或并发组。
func (e *engine) runStep(ctx context.Context, tc *turnCtx, step Step) ([]StepOutcome, error) {
	if len(step.Fanout) > 0 {
		return e.runFanout(ctx, tc, step)
	}
	n, ok := e.node(step.Node)
	if !ok {
		return nil, &missingNodeError{node: step.Node, step: step.ID}
	}
	in := e.buildInput(tc, step, step.Node, "")
	out, dur, err := e.runNode(ctx, tc, n, in)
	if emitErr := e.emitNodeCompleted(ctx, tc, step.ID, step.Node, "", out, dur, err); emitErr != nil {
		return nil, emitError{err: emitErr}
	}
	if err != nil {
		return nil, err
	}
	return []StepOutcome{{Node: step.Node, Output: out, Parsed: out.Parsed, Duration: dur}}, nil
}

type branchResult struct {
	out agent.NodeOutput
	dur time.Duration
	err error
}

// runFanout 并发执行全部分支（上限 fanoutLimit），按图声明顺序 join。
func (e *engine) runFanout(ctx context.Context, tc *turnCtx, step Step) ([]StepOutcome, error) {
	results := make([]branchResult, len(step.Fanout))
	sem := make(chan struct{}, fanoutLimit)
	var wg sync.WaitGroup

	for i, br := range step.Fanout {
		n, ok := e.node(br.Node)
		if !ok {
			results[i] = branchResult{err: &missingNodeError{node: br.Node, step: step.ID}}
			continue
		}
		in := e.buildInput(tc, step, br.Node, br.Dimension)
		wg.Add(1)
		go func(i int, br FanoutBranch, n agent.Node, in agent.NodeInput) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, dur, err := e.runNode(ctx, tc, n, in)
			if err != nil && ctx.Err() == nil {
				// 单个 deliberate 失败重试一次（§7.6）。
				o2, d2, e2 := e.runNode(ctx, tc, n, in)
				dur += d2
				out, err = o2, e2
			}
			results[i] = branchResult{out: out, dur: dur, err: err}
		}(i, br, n, in)
	}
	wg.Wait()

	// join 顺序固定：correctness → risk → execution（§7.6）。
	order := branchOrder(step.Fanout)
	for _, i := range order {
		br := step.Fanout[i]
		r := results[i]
		if emitErr := e.emitNodeCompleted(ctx, tc, step.ID, br.Node, br.Dimension, r.out, r.dur, r.err); emitErr != nil {
			return nil, emitError{err: emitErr}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	outcomes := make([]StepOutcome, 0, len(step.Fanout))
	failed := 0
	for _, i := range order {
		br := step.Fanout[i]
		if results[i].err != nil {
			failed++
			tc.missingDims = append(tc.missingDims, br.Dimension)
			e.log.Warn("并发分支失败", "step", step.ID, "dimension", string(br.Dimension), "error", results[i].err)
			continue
		}
		outcomes = append(outcomes, StepOutcome{
			Node: br.Node, Output: results[i].out, Parsed: results[i].out.Parsed, Duration: results[i].dur,
		})
	}
	if failed >= 2 {
		return nil, &fanoutFailure{failed: failed, total: len(step.Fanout)}
	}
	return outcomes, nil
}

// branchOrder 返回固定的 join 顺序：correctness → risk → execution。
// 没有维度声明的并发组保持图声明顺序。
func branchOrder(branches []FanoutBranch) []int {
	order := make([]int, len(branches))
	for i := range order {
		order[i] = i
	}
	hasDim := false
	for _, br := range branches {
		if br.Dimension != "" {
			hasDim = true
			break
		}
	}
	if !hasDim {
		return order
	}
	rank := make(map[model.Dimension]int, len(model.AllDimensions()))
	for i, d := range model.AllDimensions() {
		rank[d] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, oka := rank[branches[order[a]].Dimension]
		rb, okb := rank[branches[order[b]].Dimension]
		if !oka {
			ra = len(rank)
		}
		if !okb {
			rb = len(rank)
		}
		return ra < rb
	})
	return order
}

// runNode 在预算内执行一个节点；未配置超时时不包装 ctx。
func (e *engine) runNode(ctx context.Context, tc *turnCtx, n agent.Node, in agent.NodeInput) (agent.NodeOutput, time.Duration, error) {
	nctx := ctx
	if tc.budget.timed() {
		var cancel context.CancelFunc
		nctx, cancel = context.WithTimeout(ctx, tc.budget.Remaining())
		defer cancel()
	}
	start := time.Now()
	out, err := n.Run(nctx, in)
	return out, time.Since(start), err
}

// applyOutcomes 把节点结果并入 turn 状态（用量、改动、校验、verdict、消息）。
func (e *engine) applyOutcomes(tc *turnCtx, step Step, outcomes []StepOutcome) {
	tc.lastOutcomes = outcomes
	if len(step.Fanout) > 0 {
		tc.fanoutOutcomes = outcomes
	}
	for _, o := range outcomes {
		tc.byNode[o.Node] = o
		tc.usage = tc.usage.Add(o.Output.Usage)
		tc.budget.addUsage(o.Output.Usage)
		tc.budget.chargeToolTurns(o.Output.ToolCalls)
		tc.changed = appendUnique(tc.changed, o.Output.Changed...)
		if len(o.Output.Changed) > 0 {
			tc.roundWrote = true
		}
		tc.checks = appendChecks(tc.checks, o.Output.Checks)
		if v := verdictOf(o.Output); v != nil {
			tc.verdict = v
			tc.checks = appendChecks(tc.checks, v.Checks)
			tc.feedback = cloneFailures(v.Failures)
		}
		if o.Node == model.NodeCode {
			tc.roundHasCode = true
		}
		if o.Node == model.NodeAsk || o.Node == model.NodeAggregation {
			plan := o.Output.Text
			if o.Output.Parsed.Plan != nil && strings.TrimSpace(o.Output.Parsed.Plan.Plan) != "" {
				plan = o.Output.Parsed.Plan.Plan
			}
			if strings.TrimSpace(plan) != "" {
				tc.planText = plan
			}
		}
		if o.Output.Text != "" {
			tc.msgs = append(tc.msgs, model.Message{
				Role: model.RoleAssistant, Name: string(o.Node), Content: o.Output.Text,
			})
		}
	}
}

// nextStep 按声明顺序找第一条满足条件的边。
func (e *engine) nextStep(g Graph, step Step, tc *turnCtx) (string, bool) {
	for _, ed := range g.Edges {
		if ed.From != step.ID {
			continue
		}
		if e.match(ed.When, tc) {
			return ed.To, true
		}
	}
	return "", false
}

func (e *engine) match(c Condition, tc *turnCtx) bool {
	switch c {
	case CondAlways:
		return true
	case CondPass:
		return tc.verdict != nil && tc.verdict.Pass
	case CondFail:
		return tc.verdict != nil && !tc.verdict.Pass
	case CondFailUnderRound:
		return tc.verdict != nil && !tc.verdict.Pass && tc.round < e.maxRounds(tc.mode)
	case CondNoProgress:
		return tc.roundHasCode && !tc.roundWrote && (tc.verdict == nil || !tc.verdict.Pass)
	}
	return false
}

// nextAfterDone 在恢复时跳过已完成 step：优先沿 always 边前进。
func nextAfterDone(g Graph, id string, done map[string]bool) (string, bool) {
	fallback := ""
	for _, ed := range g.Edges {
		if ed.From != id {
			continue
		}
		if ed.When == CondAlways {
			return ed.To, true
		}
		if fallback == "" && !done[ed.To] {
			fallback = ed.To
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

// resumeStart 把 ResumeState.Step 映射回图里的 step ID。
func resumeStart(g Graph, ref model.StepRef, done map[string]bool) string {
	if ref.Phase != "" {
		if _, ok := g.Steps[ref.Phase]; ok {
			return ref.Phase
		}
	}
	if ref.Node != "" {
		ids := make([]string, 0, len(g.Steps))
		for id, st := range g.Steps {
			if st.Node == ref.Node {
				ids = append(ids, id)
				continue
			}
			for _, br := range st.Fanout {
				if br.Node == ref.Node {
					ids = append(ids, id)
					break
				}
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			return ids[0]
		}
	}
	if done[g.Entry] {
		if next, ok := nextAfterDone(g, g.Entry, done); ok {
			return next
		}
	}
	return g.Entry
}

// buildInput 按节点契约填充 NodeInput（§11.5 / §21.12 表）。
func (e *engine) buildInput(tc *turnCtx, step Step, node model.NodeName, dim model.Dimension) agent.NodeInput {
	in := agent.NodeInput{
		Request:        tc.user,
		SessionSummary: tc.summary,
		History:        tc.historyFor(),
		Mode:           tc.mode,
		Dimension:      dim,
	}
	switch node {
	case model.NodeCode:
		in.Plan = tc.planText
		in.Feedback = cloneFailures(tc.feedback)
		if o, ok := tc.lastOutcomeOf(model.NodeVerifier); ok {
			in.Prior = []agent.PriorOutput{priorOf(o)}
		}
	case model.NodeAsk:
		in.Feedback = cloneFailures(tc.feedback)
		in.Prior = tc.priorsForAsk()
	case model.NodeDeliberate:
		in.Plan = tc.planText
		in.Prior = priorsOf(tc.lastOutcomes)
	case model.NodeAggregation:
		in.Plan = tc.aggregationPlan()
		in.Prior = priorsOf(tc.fanoutOutcomes)
		in.Feedback = cloneFailures(tc.feedback)
	case model.NodeVerifier:
		in.Plan = tc.planText
		in.Feedback = cloneFailures(tc.feedback)
		in.Prior = priorsOf(tc.lastOutcomes)
		in.Artifacts = artifactRefs(tc.checks)
	}
	return in
}

// aggregationPlan 在已知缺失维度时把提示并入 Plan 文本，保证 aggregation 能报告 missing_dimensions。
func (tc *turnCtx) aggregationPlan() string {
	plan := tc.planText
	if len(tc.missingDims) == 0 {
		return plan
	}
	names := make([]string, len(tc.missingDims))
	for i, d := range tc.missingDims {
		names[i] = string(d)
	}
	if strings.TrimSpace(plan) != "" {
		plan = strings.TrimRight(plan, "\n") + "\n\n"
	}
	return plan + "缺失维度：" + strings.Join(names, "、")
}

// emitNodeCompleted 写 node_completed（node_started/step_completed 由节点自己写）。
func (e *engine) emitNodeCompleted(ctx context.Context, tc *turnCtx, stepID string, node model.NodeName, dim model.Dimension, out agent.NodeOutput, dur time.Duration, runErr error) error {
	status := "ok"
	if runErr != nil {
		status = "failed"
	}
	data := map[string]any{
		"step":       stepID,
		"node":       string(node),
		"status":     status,
		"durationMs": dur.Milliseconds(),
	}
	if dim != "" {
		data["dimension"] = string(dim)
	}
	if !out.Usage.IsZero() {
		data["usage"] = out.Usage
	}
	if len(out.Changed) > 0 {
		data["changed"] = out.Changed
	}
	if len(out.Checks) > 0 {
		data["checks"] = out.Checks
	}
	if v := verdictOf(out); v != nil {
		data["verdict"] = v
	}
	if runErr != nil {
		data["error"] = runErr.Error()
		data["errorClass"] = string(model.ClassOf(runErr))
	}
	return e.emit(auditCtx(ctx), &model.Event{
		Type: model.EventNodeCompleted,
		Turn: tc.turn,
		Step: tc.stepLabel(stepID, node),
		Node: node,
		Data: model.MustJSON(data),
	})
}

// stepFailed 把节点/并发组错误映射为状态、事件与退出码。
// 预期内的状态机失败（轮次耗尽等）由 walk 直接处理并返回 nil error；
// 节点/图/超时等真实失败会把原错误一并返回，便于上层分类与提示。
func (e *engine) stepFailed(ctx context.Context, tc *turnCtx, stepID string, err error) (TurnResult, error) {
	var ee emitError
	if errors.As(err, &ee) {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, ee.err
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return e.interruptTurn(ctx, tc, stepID, err)
	}
	var fe *fanoutFailure
	if errors.As(err, &fe) {
		res, emitErr := e.failTurn(ctx, tc, stepID, "fanout_failed", model.ErrInternal, fe.Error(), 1)
		if emitErr != nil {
			return res, emitErr
		}
		return res, err
	}
	var ne *missingNodeError
	if errors.As(err, &ne) {
		res, emitErr := e.failTurn(ctx, tc, stepID, "node_missing", model.ErrInternal, ne.Error(), 1)
		if emitErr != nil {
			return res, emitErr
		}
		return res, err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// 包装为分类错误：否则上层会把节点超时误判为用户中断（退出码 3）。
		timeoutErr := model.NewError(model.ErrToolTimeout, "node_timeout", err.Error())
		res, emitErr := e.failTurn(ctx, tc, stepID, "node_timeout", model.ErrToolTimeout, timeoutErr.Error(), 1)
		if emitErr != nil {
			return res, emitErr
		}
		return res, timeoutErr
	}
	class := model.ClassOf(err)
	res, emitErr := e.failTurn(ctx, tc, stepID, reasonForClass(class), class, err.Error(), exitCodeForClass(class))
	if emitErr != nil {
		return res, emitErr
	}
	return res, err
}

// succeedTurn 写 turn_completed 并返回成功结果。
func (e *engine) succeedTurn(ctx context.Context, tc *turnCtx, stepID string) (TurnResult, error) {
	res := TurnResult{
		Status:   StatusOK,
		Usage:    tc.usage,
		Changed:  tc.changed,
		Checks:   tc.checks,
		PlanPath: tc.planPath,
		ExitCode: 0,
	}
	if err := e.emit(ctx, &model.Event{
		Type: model.EventTurnCompleted,
		Turn: tc.turn,
		Step: tc.stepLabel(stepID, ""),
		Data: model.MustJSON(turnCompletedData(res)),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	return res, nil
}

// failTurn 写 session_failed + turn_completed（不静默退出）。
func (e *engine) failTurn(ctx context.Context, tc *turnCtx, stepID, reason string, class model.ErrorClass, message string, exitCode int) (TurnResult, error) {
	if exitCode <= 0 {
		exitCode = 1
	}
	res := TurnResult{
		Status:   StatusFailed,
		Usage:    tc.usage,
		Changed:  tc.changed,
		Checks:   tc.checks,
		PlanPath: tc.planPath,
		ExitCode: exitCode,
	}
	evCtx := context.WithoutCancel(ctx)
	ref := tc.failureRef(stepID)
	data := map[string]any{
		"reason":      reason,
		"errorClass":  string(class),
		"currentStep": ref,
		"step":        stepID,
		"turn":        tc.turn,
		"round":       tc.round,
	}
	if message != "" {
		data["message"] = message
	}
	if err := e.emit(evCtx, &model.Event{
		Type: model.EventSessionFailed,
		Turn: tc.turn,
		Step: ref,
		Data: model.MustJSON(data),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	if err := e.emit(evCtx, &model.Event{
		Type: model.EventTurnCompleted,
		Turn: tc.turn,
		Step: ref,
		Data: model.MustJSON(turnCompletedData(res)),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	return res, nil
}

// interruptTurn 写 session_interrupted + turn_completed，绝不把取消写成成功。
func (e *engine) interruptTurn(ctx context.Context, tc *turnCtx, stepID string, cause error) (TurnResult, error) {
	res := TurnResult{
		Status:   StatusInterrupted,
		Usage:    tc.usage,
		Changed:  tc.changed,
		Checks:   tc.checks,
		PlanPath: tc.planPath,
		ExitCode: 3,
	}
	evCtx := context.WithoutCancel(ctx)
	ref := tc.failureRef(stepID)
	data := map[string]any{
		"reason":      "interrupted",
		"errorClass":  string(model.ErrInterrupted),
		"currentStep": ref,
		"step":        stepID,
		"turn":        tc.turn,
		"round":       tc.round,
	}
	if cause != nil {
		data["message"] = cause.Error()
	}
	if err := e.emit(evCtx, &model.Event{
		Type: model.EventSessionInterrupted,
		Turn: tc.turn,
		Step: ref,
		Data: model.MustJSON(data),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	if err := e.emit(evCtx, &model.Event{
		Type: model.EventTurnCompleted,
		Turn: tc.turn,
		Step: ref,
		Data: model.MustJSON(turnCompletedData(res)),
	}); err != nil {
		return TurnResult{Status: StatusFailed, ExitCode: 1}, err
	}
	return res, nil
}

// planGate 是 plan 通过验证后的人机回路（§8.3）。
func (e *engine) planGate(ctx context.Context, tc *turnCtx, g Graph) (TurnResult, error) {
	planPath := e.planPath()
	tc.planPath = planPath
	plan := Plan{Path: planPath, Body: tc.planText, Round: tc.round}

	var (
		confirmed bool
		gateErr   error
	)
	if e.gate != nil {
		confirmed, gateErr = e.gate.Confirm(ctx, plan)
	}
	if gateErr != nil {
		if ctx.Err() != nil || errors.Is(gateErr, context.Canceled) {
			return e.interruptTurn(ctx, tc, terminalGate, gateErr)
		}
		class := model.ClassOf(gateErr)
		if errors.Is(gateErr, approval.ErrNonInteractive) {
			class = model.ErrApprovalRequired
		}
		res, emitErr := e.failTurn(ctx, tc, terminalGate, reasonForClass(class), class, gateErr.Error(), exitCodeForClass(class))
		if emitErr != nil {
			return res, emitErr
		}
		return res, gateErr
	}
	if confirmed {
		if err := e.SwitchMode(ctx, model.ModeCode, "plan gate confirmed", true); err != nil {
			res, _ := e.failTurn(ctx, tc, terminalGate, "mode_switch_failed", model.ClassOf(err), err.Error(), 1)
			return res, err
		}
	}
	return e.succeedTurn(ctx, tc, terminalGate)
}

// planPath 返回 plan.md 的默认位置；真正的落盘由 session 层完成。
func (e *engine) planPath() string {
	if p, ok := e.cfg.(config.Paths); ok {
		if wd := p.WorkDir(); wd != "" {
			return filepath.Join(wd, ".ant", "sessions", "plan.md")
		}
	}
	return filepath.Join(".ant", "sessions", "plan.md")
}

func (e *engine) maxRounds(mode model.Mode) int {
	if e.cfg == nil {
		return defaultMaxRounds
	}
	if n, ok := e.cfg.Settings().Limits.MaxRounds[string(mode)]; ok && n > 0 {
		return n
	}
	return defaultMaxRounds
}

func (e *engine) maxToolTurns() int {
	if e.cfg == nil {
		return 0
	}
	return e.cfg.Settings().Limits.MaxToolTurns
}

func (e *engine) node(name model.NodeName) (agent.Node, bool) {
	if e.nodes == nil {
		return nil, false
	}
	return e.nodes.Get(name)
}

// auditCtx 在 ctx 已取消时仍允许写审计事件（node_completed/turn_started），
// 但保留 ctx 上的值；取消本身由 session_interrupted 表达。
func auditCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	if ctx.Err() != nil {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

func (e *engine) emit(ctx context.Context, ev *model.Event) error {
	if e.sink == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return e.sink.Emit(ctx, ev)
}

func (e *engine) turnStartedData(tc *turnCtx) json.RawMessage {
	data := map[string]any{
		"input":     tc.user,
		"inputHash": hashString(tc.user),
		"mode":      string(tc.mode),
		"turn":      tc.turn,
		"round":     tc.round,
	}
	if tc.resumed {
		data["resumed"] = true
		data["currentStep"] = tc.start
		data["done"] = tc.doneIDs()
	}
	return model.MustJSON(data)
}

func turnCompletedData(res TurnResult) map[string]any {
	data := map[string]any{
		"status":   string(res.Status),
		"exitCode": res.ExitCode,
		"usage":    res.Usage,
	}
	if len(res.Changed) > 0 {
		data["changed"] = res.Changed
	}
	if len(res.Checks) > 0 {
		data["checks"] = res.Checks
	}
	if res.PlanPath != "" {
		data["planPath"] = res.PlanPath
	}
	return data
}

func exitCodeForClass(class model.ErrorClass) int {
	switch class {
	case model.ErrInterrupted:
		return 3
	case model.ErrApprovalRequired:
		return 4
	case model.ErrProviderAuth, model.ErrProviderRateLimit, model.ErrProviderUnavailable,
		model.ErrProviderInvalidReq, model.ErrAdapterNotImplemented:
		return 5
	case model.ErrConfigInvalid:
		return 2
	}
	return 1
}

func reasonForClass(class model.ErrorClass) string {
	if class == "" {
		return "node_failed"
	}
	return string(class)
}

// ---- turnCtx ----

type turnCtx struct {
	turn    int
	mode    model.Mode
	user    string
	summary string
	base    []model.Message // 本 turn 之前的历史（新 turn 用）
	msgs    []model.Message // 本 turn 累积：user + 已完成节点文本
	budget  *turnBudget

	start   string
	current string
	resumed bool
	done    map[string]bool

	round     int
	roundSeen bool

	roundWrote   bool
	roundHasCode bool

	changed  []string
	checks   []model.Check
	usage    model.Usage
	feedback []model.Failure
	verdict  *model.Verdict

	planText    string
	planPath    string
	missingDims []model.Dimension

	byNode         map[model.NodeName]StepOutcome
	lastOutcomes   []StepOutcome
	fanoutOutcomes []StepOutcome
}

// historyFor 返回节点可见的 History：本 turn 之前的历史 + 本 turn 累积消息。
// resume 时 base 为空、msgs 已是从 ResumeState 恢复的历史，不会重复。
func (tc *turnCtx) historyFor() []model.Message {
	if len(tc.base) == 0 {
		return cloneMessages(tc.msgs)
	}
	out := make([]model.Message, 0, len(tc.base)+len(tc.msgs))
	out = append(out, cloneMessages(tc.base)...)
	out = append(out, cloneMessages(tc.msgs)...)
	return out
}

// enterRound 按「回到入口节点」计数并重置本轮标记。
func (tc *turnCtx) enterRound() {
	if !tc.roundSeen {
		tc.roundSeen = true
		if tc.round < 1 {
			tc.round = 1
		}
	} else {
		tc.round++
	}
	tc.budget.setRound(tc.round)
	tc.roundWrote = false
	tc.roundHasCode = false
	tc.missingDims = nil
}

// stepLabel 生成事件与 checkpoint 的 step 标识。
func (tc *turnCtx) stepLabel(stepID string, node model.NodeName) string {
	out := string(tc.mode)
	if tc.round > 0 {
		out += fmt.Sprintf(".round-%d", tc.round)
	}
	if stepID != "" {
		out += "." + stepID
	}
	if node != "" && string(node) != stepID {
		out += "." + string(node)
	}
	return out
}

// failureRef 为失败事件生成 currentStep：出口不是真实 step 时回退到最近执行的 step。
func (tc *turnCtx) failureRef(stepID string) string {
	label := stepID
	if label == "" || isTerminal(label) {
		label = tc.current
	}
	return tc.stepLabel(label, "")
}

// lastOutcomeOf 返回最近一次执行的指定节点结果。
func (tc *turnCtx) lastOutcomeOf(node model.NodeName) (StepOutcome, bool) {
	o, ok := tc.byNode[node]
	return o, ok
}

// priorsForAsk 把聚合问题与 verifier 结论一起带回规划者（§8.3 第 6 步）。
func (tc *turnCtx) priorsForAsk() []agent.PriorOutput {
	var out []agent.PriorOutput
	if o, ok := tc.byNode[model.NodeAggregation]; ok {
		out = append(out, priorOf(o))
	}
	if o, ok := tc.byNode[model.NodeVerifier]; ok {
		out = append(out, priorOf(o))
	}
	if len(out) == 0 {
		return priorsOf(tc.lastOutcomes)
	}
	return out
}

// doneIDs 返回已完成 step 的稳定顺序列表。
func (tc *turnCtx) doneIDs() []string {
	ids := make([]string, 0, len(tc.done))
	for id := range tc.done {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ---- 错误类型 ----

// emitError 表示事件写入失败（不是节点失败）。
type emitError struct{ err error }

func (e emitError) Error() string { return "事件写入失败: " + e.err.Error() }
func (e emitError) Unwrap() error { return e.err }

// fanoutFailure 表示并发组中两个及以上分支失败。
type fanoutFailure struct {
	failed int
	total  int
}

func (f *fanoutFailure) Error() string {
	return fmt.Sprintf("%d/%d 个并发分支失败，本轮中止", f.failed, f.total)
}

// missingNodeError 表示节点未注册。
type missingNodeError struct {
	node model.NodeName
	step string
}

func (m *missingNodeError) Error() string {
	return fmt.Sprintf("step %q 的节点 %q 未注册", m.step, m.node)
}

// ---- 小工具 ----

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func cloneMessages(in []model.Message) []model.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.Message, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.ToolCalls) > 0 {
			calls := make([]model.ToolCall, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				calls[j] = c
				if len(c.Arguments) > 0 {
					calls[j].Arguments = append(json.RawMessage(nil), c.Arguments...)
				}
			}
			out[i].ToolCalls = calls
		}
	}
	return out
}

func cloneFailures(in []model.Failure) []model.Failure {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.Failure, len(in))
	copy(out, in)
	return out
}

func lastUserContent(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

func priorOf(o StepOutcome) agent.PriorOutput {
	return agent.PriorOutput{
		Node:   o.Node,
		Text:   o.Output.Text,
		Parsed: o.Parsed,
		Usage:  o.Output.Usage,
	}
}

func priorsOf(outcomes []StepOutcome) []agent.PriorOutput {
	if len(outcomes) == 0 {
		return nil
	}
	out := make([]agent.PriorOutput, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, priorOf(o))
	}
	return out
}

func verdictOf(out agent.NodeOutput) *model.Verdict {
	if out.Parsed.Verdict != nil {
		return out.Parsed.Verdict
	}
	raw := out.Parsed.Raw
	if len(raw) == 0 {
		raw = out.Structured
	}
	if len(raw) == 0 {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	if _, ok := probe["pass"]; !ok {
		return nil
	}
	var v model.Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &v
}

func appendUnique(dst []string, add ...string) []string {
	for _, s := range add {
		if s == "" {
			continue
		}
		found := false
		for _, have := range dst {
			if have == s {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, s)
		}
	}
	return dst
}

func appendChecks(dst []model.Check, add []model.Check) []model.Check {
	for _, c := range add {
		dup := false
		for _, have := range dst {
			if have.Cmd == c.Cmd && have.ExitCode == c.ExitCode && have.OutputRef == c.OutputRef {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, c)
		}
	}
	return dst
}

func artifactRefs(checks []model.Check) []model.ArtifactRef {
	var out []model.ArtifactRef
	for _, c := range checks {
		if !c.OutputRef.IsZero() {
			out = append(out, c.OutputRef)
		}
	}
	return out
}
