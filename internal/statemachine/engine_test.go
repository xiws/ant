package statemachine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiws/ant/internal/agent"
	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

// ---- 测试替身：config.View / PlanGate / EventSink ----

type fakeView struct{ settings config.Settings }

func (f fakeView) Settings() config.Settings           { return f.settings }
func (f fakeView) Models() []config.ModelSpec          { return nil }
func (f fakeView) Providers() []config.ProviderSpec    { return nil }
func (f fakeView) Origin(string) (config.Origin, bool) { return "", false }
func (f fakeView) Warnings() []string                  { return nil }

func testView(mode model.Mode) config.View {
	return fakeView{settings: config.Settings{
		DefaultMode: mode,
		Limits: config.Limits{
			MaxToolTurns:  12,
			MaxRounds:     map[string]int{"code": 3, "plan": 3, "council": 3},
			NodeTimeoutMs: 5000,
		},
	}}
}

type fixedGate struct {
	accept bool
	err    error

	mu    sync.Mutex
	plans []Plan
}

func (g *fixedGate) Confirm(_ context.Context, p Plan) (bool, error) {
	g.mu.Lock()
	g.plans = append(g.plans, p)
	g.mu.Unlock()
	if g.err != nil {
		return false, g.err
	}
	return g.accept, nil
}

func (g *fixedGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.plans)
}

type captureSink struct {
	mu     sync.Mutex
	events []*model.Event
}

func newCapture() (*captureSink, model.EventSink) {
	c := &captureSink{}
	return c, model.EventSinkFunc(func(_ context.Context, ev *model.Event) error {
		cp := *ev
		c.mu.Lock()
		c.events = append(c.events, &cp)
		c.mu.Unlock()
		return nil
	})
}

func (c *captureSink) all() []*model.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*model.Event(nil), c.events...)
}

func (c *captureSink) types() []model.EventType {
	evs := c.all()
	out := make([]model.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func (c *captureSink) first(t model.EventType) *model.Event {
	for _, ev := range c.all() {
		if ev.Type == t {
			return ev
		}
	}
	return nil
}

func (c *captureSink) count(t model.EventType) int {
	n := 0
	for _, ev := range c.all() {
		if ev.Type == t {
			n++
		}
	}
	return n
}

func (c *captureSink) last() *model.Event {
	evs := c.all()
	if len(evs) == 0 {
		return nil
	}
	return evs[len(evs)-1]
}

func eventData(t *testing.T, ev *model.Event) map[string]any {
	t.Helper()
	if ev == nil {
		t.Fatal("event 为 nil")
	}
	var m map[string]any
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatalf("事件 %s 的 data 不是合法 JSON: %v", ev.Type, err)
		}
	}
	return m
}

// ---- 节点替身 ----

func codeNode(changed []string) *agent.StaticNode {
	return &agent.StaticNode{
		Spec: agent.NodeSpec{Name: model.NodeCode},
		Output: agent.NodeOutput{
			Text:      "## 变更\n- 修改\n## 已执行\n- go test\n## 未解决\n无",
			Changed:   changed,
			Usage:     model.Usage{Input: 10, Output: 5},
			ToolCalls: 1,
		},
	}
}

func verdictNode(pass bool, checks []model.Check) *agent.StaticNode {
	v := &model.Verdict{Pass: pass, Checks: checks}
	if pass {
		v.Summary = "全部通过"
	} else {
		v.Summary = "失败"
		v.Failures = []model.Failure{{
			Severity:   model.SeverityBlocker,
			File:       "a.go",
			Evidence:   "go test ./... 退出码 1",
			Suggestion: "修正",
		}}
	}
	raw := model.MustJSON(v)
	return &agent.StaticNode{
		Spec: agent.NodeSpec{Name: model.NodeVerifier},
		Output: agent.NodeOutput{
			Text:   string(raw),
			Parsed: agent.Parsed{Raw: raw, Verdict: v},
		},
	}
}

func askNode(text string) *agent.StaticNode {
	return &agent.StaticNode{
		Spec:   agent.NodeSpec{Name: model.NodeAsk},
		Output: agent.NodeOutput{Text: text, Usage: model.Usage{Input: 2, Output: 3}},
	}
}

func aggregationNode(text, plan string) *agent.StaticNode {
	return &agent.StaticNode{
		Spec: agent.NodeSpec{Name: model.NodeAggregation},
		Output: agent.NodeOutput{
			Text:   text,
			Parsed: agent.Parsed{Plan: &model.Aggregation{Plan: plan}},
		},
	}
}

// dimNode 按维度返回不同文本，并记录并发度与输入，用于 fan-out 断言。
type dimNode struct {
	name model.NodeName
	errs map[model.Dimension]error

	mu         sync.Mutex
	inputs     []agent.NodeInput
	running    int32
	maxRunning int32
	started    chan struct{}
	release    chan struct{}
}

func newDimNode(name model.NodeName) *dimNode {
	return &dimNode{name: name, started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (n *dimNode) Name() model.NodeName     { return n.name }
func (n *dimNode) Tools() []model.ToolName  { return nil }
func (n *dimNode) Contract() agent.Contract { return nil }

func (n *dimNode) Run(ctx context.Context, in agent.NodeInput) (agent.NodeOutput, error) {
	n.mu.Lock()
	n.inputs = append(n.inputs, in)
	n.mu.Unlock()

	cur := atomic.AddInt32(&n.running, 1)
	for {
		max := atomic.LoadInt32(&n.maxRunning)
		if cur <= max || atomic.CompareAndSwapInt32(&n.maxRunning, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&n.running, -1)

	select {
	case n.started <- struct{}{}:
	default:
	}
	select {
	case <-n.release:
	case <-time.After(2 * time.Second):
	}
	if err, ok := n.errs[in.Dimension]; ok {
		return agent.NodeOutput{}, err
	}
	return agent.NodeOutput{Text: string(in.Dimension), Usage: model.Usage{Input: 1, Output: 1}}, nil
}

func (n *dimNode) snapshot() []agent.NodeInput {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]agent.NodeInput(nil), n.inputs...)
}

func (n *dimNode) dimensions() []model.Dimension {
	var out []model.Dimension
	for _, in := range n.snapshot() {
		out = append(out, in.Dimension)
	}
	return out
}

func (n *dimNode) maxConcurrent() int32 { return atomic.LoadInt32(&n.maxRunning) }

// releaseAfter 等到 count 个分支都进入 Run 后放行，用来证明 fan-out 是并发的。
func releaseAfter(n *dimNode, count int) {
	go func() {
		for i := 0; i < count; i++ {
			select {
			case <-n.started:
			case <-time.After(3 * time.Second):
				return
			}
		}
		close(n.release)
	}()
}

// ---- 测试 ----

func TestCodeModePass(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, []model.Check{{Cmd: "go test ./...", ExitCode: 0}})
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{
		UserInput: "fix it",
		History:   []model.Message{{Role: model.RoleUser, Content: "old"}},
	})
	if err != nil {
		t.Fatalf("RunTurn 返回错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v, want ok/0", res)
	}
	if !reflect.DeepEqual(res.Changed, []string{"a.go"}) {
		t.Fatalf("Changed = %v", res.Changed)
	}
	if len(res.Checks) != 1 || res.Checks[0].Cmd != "go test ./..." {
		t.Fatalf("Checks = %+v", res.Checks)
	}
	if res.Usage.Input != 10 || res.Usage.Output != 5 {
		t.Fatalf("Usage = %+v", res.Usage)
	}
	if res.PlanPath != "" {
		t.Fatalf("code 模式不应有 PlanPath: %q", res.PlanPath)
	}

	want := []model.EventType{
		model.EventTurnStarted, model.EventNodeCompleted, model.EventNodeCompleted, model.EventTurnCompleted,
	}
	if got := sink.types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}

	// 节点只拿到本 turn 的输入包：user 本轮消息 + 已完成节点文本，不含旧 transcript。
	inputs := code.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("code 执行次数 = %d", len(inputs))
	}
	if inputs[0].Request != "fix it" {
		t.Fatalf("Request = %q", inputs[0].Request)
	}
	if len(inputs[0].History) != 2 ||
		inputs[0].History[0].Content != "old" || inputs[0].History[1].Content != "fix it" {
		t.Fatalf("code History = %+v, 应为 旧历史 + 本轮 user", inputs[0].History)
	}
	if inputs[0].Mode != model.ModeCode {
		t.Fatalf("Mode = %q", inputs[0].Mode)
	}

	vi := verifier.Inputs()
	if len(vi) != 1 {
		t.Fatalf("verifier 执行次数 = %d", len(vi))
	}
	if len(vi[0].Prior) != 1 || vi[0].Prior[0].Node != model.NodeCode {
		t.Fatalf("verifier Prior = %+v", vi[0].Prior)
	}
	if !strings.Contains(vi[0].History[len(vi[0].History)-1].Content, "## 变更") {
		t.Fatalf("verifier 应看到 code 的文本: %+v", vi[0].History)
	}
}

func TestCodeModeFailThenPass(t *testing.T) {
	code := codeNode([]string{"a.go"})
	fail := verdictNode(false, nil).Output
	pass := verdictNode(true, nil).Output
	verifier := verdictNode(false, nil)
	var calls int
	verifier.OnRun = func(agent.NodeInput) {
		calls++
		if calls == 2 {
			verifier.Output = pass
		}
	}
	verifier.Output = fail

	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	if calls != 2 {
		t.Fatalf("verifier 调用次数 = %d, want 2", calls)
	}
	codeInputs := code.Inputs()
	if len(codeInputs) != 2 {
		t.Fatalf("code 执行次数 = %d, want 2（fail 且未达上限应回到 code）", len(codeInputs))
	}
	if len(codeInputs[1].Feedback) != 1 || codeInputs[1].Feedback[0].Evidence == "" {
		t.Fatalf("第二轮 code Feedback = %+v", codeInputs[1].Feedback)
	}
	if sink.count(model.EventSessionFailed) != 0 {
		t.Fatalf("不应写 session_failed: %v", sink.types())
	}
}

func TestCodeModeRoundsExhausted(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(false, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("结果 = %+v, want failed/1", res)
	}
	if n := len(code.Inputs()); n != 3 {
		t.Fatalf("code 执行次数 = %d, want 3（轮次上限）", n)
	}
	failed := sink.first(model.EventSessionFailed)
	if failed == nil {
		t.Fatal("缺少 session_failed")
	}
	data := eventData(t, failed)
	if data["reason"] != "verification_failed" {
		t.Fatalf("reason = %v", data["reason"])
	}
	if data["errorClass"] != string(model.ErrVerificationFailed) {
		t.Fatalf("errorClass = %v", data["errorClass"])
	}
	if data["currentStep"] == "" || data["currentStep"] == nil {
		t.Fatalf("currentStep 缺失: %v", data)
	}
	if last := sink.last(); last == nil || last.Type != model.EventTurnCompleted {
		t.Fatalf("最后事件 = %+v", last)
	}
	if evs := sink.types(); evs[0] != model.EventTurnStarted {
		t.Fatalf("首事件 = %v", evs[0])
	}
}

func TestCodeModeNoProgress(t *testing.T) {
	code := codeNode(nil) // 没有 write/edit 改动
	verifier := verdictNode(false, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("结果 = %+v, want failed/1", res)
	}
	if n := len(code.Inputs()); n != 1 {
		t.Fatalf("code 执行次数 = %d, want 1（无进展立即中止）", n)
	}
	failed := sink.first(model.EventSessionFailed)
	data := eventData(t, failed)
	if data["reason"] != "no_progress" {
		t.Fatalf("reason = %v, want no_progress", data["reason"])
	}
}

func TestAskModeSingleNode(t *testing.T) {
	ask := askNode("答案见 a.go:12")
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeAsk), agent.StaticRegistry(ask), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "what?"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	want := []model.EventType{model.EventTurnStarted, model.EventNodeCompleted, model.EventTurnCompleted}
	if got := sink.types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("事件 = %v, want %v", got, want)
	}
	if len(ask.Inputs()) != 1 || ask.Inputs()[0].Request != "what?" {
		t.Fatalf("ask 输入 = %+v", ask.Inputs())
	}
}

func TestPlanModeFanoutGateAndSwitch(t *testing.T) {
	ask := askNode("draft plan")
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("aggregation text", "merged plan")
	verifier := verdictNode(true, nil)
	code := codeNode([]string{"a.go"})
	gate := &fixedGate{accept: true}
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModePlan),
		agent.StaticRegistry(ask, deliberate, agg, verifier, code), sinkFn, gate, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "design it"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	if res.PlanPath == "" {
		t.Fatal("plan 模式应返回 PlanPath")
	}

	// fan-out：三个维度并发执行，各自固定 Dimension，互不可见。
	if got := deliberate.maxConcurrent(); got != 3 {
		t.Fatalf("fan-out 最大并发 = %d, want 3", got)
	}
	dims := deliberate.dimensions()
	if len(dims) != 3 {
		t.Fatalf("deliberate 执行次数 = %d", len(dims))
	}
	wantDims := []model.Dimension{model.DimCorrectness, model.DimRisk, model.DimExecution}
	for _, want := range wantDims {
		found := false
		for _, d := range dims {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺少维度 %q（实际 %v）", want, dims)
		}
	}
	for _, in := range deliberate.snapshot() {
		if in.Request != "design it" || in.Plan != "draft plan" {
			t.Fatalf("deliberate 输入 = %+v", in)
		}
		if len(in.Prior) != 1 || in.Prior[0].Text != "draft plan" {
			t.Fatalf("deliberate 只应看到规划输出: %+v", in.Prior)
		}
	}

	// join 顺序固定 correctness → risk → execution，交给 aggregation。
	aggIn := agg.Inputs()
	if len(aggIn) != 1 {
		t.Fatalf("aggregation 执行次数 = %d", len(aggIn))
	}
	if aggIn[0].Plan != "draft plan" {
		t.Fatalf("aggregation Plan = %q", aggIn[0].Plan)
	}
	var priors []string
	for _, p := range aggIn[0].Prior {
		priors = append(priors, p.Text)
	}
	if !reflect.DeepEqual(priors, []string{"correctness", "risk", "execution"}) {
		t.Fatalf("aggregation Prior 顺序 = %v", priors)
	}
	if vi := verifier.Inputs(); len(vi) != 1 || vi[0].Plan != "merged plan" {
		t.Fatalf("verifier 输入 = %+v", vi)
	}

	// Plan gate：拿到最终计划，确认后切到 code。
	if gate.count() != 1 {
		t.Fatalf("gate 调用次数 = %d", gate.count())
	}
	if gate.plans[0].Body != "merged plan" || gate.plans[0].Path == "" || gate.plans[0].Round != 1 {
		t.Fatalf("gate Plan = %+v", gate.plans[0])
	}
	if eng.Mode() != model.ModeCode {
		t.Fatalf("确认后模式 = %q, want code", eng.Mode())
	}
	mc := sink.first(model.EventModeChanged)
	if mc == nil {
		t.Fatal("缺少 mode_changed")
	}
	mcData := eventData(t, mc)
	if mcData["from"] != "plan" || mcData["to"] != "code" || mcData["confirmedByUser"] != true {
		t.Fatalf("mode_changed data = %v", mcData)
	}

	// 切换后的 code turn 应拿到 plan 文本。
	res2, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "execute plan"})
	if err != nil {
		t.Fatalf("第二个 turn 错误: %v", err)
	}
	if res2.Status != StatusOK || res2.ExitCode != 0 {
		t.Fatalf("第二个 turn 结果 = %+v", res2)
	}
	codeIn := code.Inputs()
	if len(codeIn) != 1 {
		t.Fatalf("code 执行次数 = %d", len(codeIn))
	}
	if codeIn[0].Plan != "merged plan" {
		t.Fatalf("code 切换后 Plan = %q, want merged plan", codeIn[0].Plan)
	}
}

func TestCouncilModeFanoutOrder(t *testing.T) {
	code := codeNode([]string{"b.go"})
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "merged")
	verifier := verdictNode(true, nil)
	gate := &fixedGate{accept: true}
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCouncil),
		agent.StaticRegistry(code, deliberate, agg, verifier), sinkFn, gate, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "refactor"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	if eng.Mode() != model.ModeCouncil {
		t.Fatalf("council 不应切模式: %q", eng.Mode())
	}
	if gate.count() != 0 {
		t.Fatalf("council 不应调用 Plan gate: %d", gate.count())
	}
	if got := deliberate.maxConcurrent(); got != 3 {
		t.Fatalf("fan-out 最大并发 = %d", got)
	}
	aggIn := agg.Inputs()
	if len(aggIn) != 1 {
		t.Fatalf("aggregation 执行次数 = %d", len(aggIn))
	}
	var priors []string
	for _, p := range aggIn[0].Prior {
		priors = append(priors, p.Text)
	}
	if !reflect.DeepEqual(priors, []string{"correctness", "risk", "execution"}) {
		t.Fatalf("aggregation Prior 顺序 = %v", priors)
	}
	// code 的输出应作为 deliberate 的 Prior。
	for _, in := range deliberate.snapshot() {
		if len(in.Prior) != 1 || in.Prior[0].Node != model.NodeCode {
			t.Fatalf("deliberate Prior = %+v", in.Prior)
		}
	}
	// join 顺序在事件层面也固定。
	var dims []string
	for _, ev := range sink.all() {
		if ev.Type == model.EventNodeCompleted && ev.Node == model.NodeDeliberate {
			dims = append(dims, eventData(t, ev)["dimension"].(string))
		}
	}
	if !reflect.DeepEqual(dims, []string{"correctness", "risk", "execution"}) {
		t.Fatalf("node_completed 维度顺序 = %v", dims)
	}
}

func TestEventOrderAndTurnStartedPayload(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	if _, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "hello"}); err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	evs := sink.all()
	if len(evs) < 3 {
		t.Fatalf("事件过少: %v", sink.types())
	}
	if evs[0].Type != model.EventTurnStarted {
		t.Fatalf("首事件 = %s", evs[0].Type)
	}
	if evs[len(evs)-1].Type != model.EventTurnCompleted {
		t.Fatalf("末事件 = %s", evs[len(evs)-1].Type)
	}
	started := eventData(t, evs[0])
	if started["input"] != "hello" {
		t.Fatalf("turn_started input = %v", started["input"])
	}
	if h, _ := started["inputHash"].(string); len(h) != 64 {
		t.Fatalf("inputHash = %v", started["inputHash"])
	}
	if sink.count(model.EventNodeCompleted) != 2 {
		t.Fatalf("node_completed 次数 = %d", sink.count(model.EventNodeCompleted))
	}
	done := eventData(t, evs[len(evs)-1])
	if done["status"] != string(StatusOK) || done["exitCode"].(float64) != 0 {
		t.Fatalf("turn_completed data = %v", done)
	}
}

func TestSwitchModeValidation(t *testing.T) {
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(), sinkFn, nil, logx.Nop())

	err := eng.SwitchMode(context.Background(), model.ModePlan, "user", false)
	var me *model.Error
	if !errors.As(err, &me) || me.Class != model.ErrApprovalRequired {
		t.Fatalf("未确认切换错误 = %v, want approval_required", err)
	}
	if eng.Mode() != model.ModeCode {
		t.Fatalf("未确认不应切换: %q", eng.Mode())
	}
	if err := eng.SwitchMode(context.Background(), model.Mode("bogus"), "x", true); err == nil {
		t.Fatal("非法模式应报错")
	}
	if err := eng.SwitchMode(context.Background(), model.ModePlan, "user", true); err != nil {
		t.Fatalf("合法切换失败: %v", err)
	}
	if eng.Mode() != model.ModePlan {
		t.Fatalf("Mode = %q", eng.Mode())
	}
	mc := sink.first(model.EventModeChanged)
	if mc == nil {
		t.Fatal("缺少 mode_changed")
	}
	if d := eventData(t, mc); d["from"] != "code" || d["to"] != "plan" || d["reason"] != "user" {
		t.Fatalf("mode_changed data = %v", d)
	}
}

func TestResumeSkipsDoneSteps(t *testing.T) {
	cases := []struct {
		name string
		st   ResumeState
	}{
		{
			name: "从当前节点继续",
			st: ResumeState{
				Turn: 1, Round: 1,
				Step:    model.StepRef{Mode: model.ModeCode, Round: 1, Phase: "verifier", Node: model.NodeVerifier},
				Done:    []string{"code"},
				History: []model.Message{{Role: model.RoleUser, Content: "resume"}},
			},
		},
		{
			name: "从入口继续但跳过 Done",
			st: ResumeState{
				Turn:    1,
				Round:   1,
				Done:    []string{"code"},
				History: []model.Message{{Role: model.RoleUser, Content: "resume"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := codeNode([]string{"a.go"})
			verifier := verdictNode(true, nil)
			sink, sinkFn := newCapture()
			eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

			res, err := eng.ResumeTurn(context.Background(), tc.st)
			if err != nil {
				t.Fatalf("ResumeTurn 错误: %v", err)
			}
			if res.Status != StatusOK || res.ExitCode != 0 {
				t.Fatalf("结果 = %+v", res)
			}
			if n := len(code.Inputs()); n != 0 {
				t.Fatalf("已完成 step 被重跑 %d 次", n)
			}
			if n := len(verifier.Inputs()); n != 1 {
				t.Fatalf("verifier 执行次数 = %d, want 1", n)
			}
			started := eventData(t, sink.first(model.EventTurnStarted))
			if started["resumed"] != true {
				t.Fatalf("turn_started 应标记 resumed: %v", started)
			}
			if last := sink.last(); last.Type != model.EventTurnCompleted {
				t.Fatalf("末事件 = %s", last.Type)
			}
		})
	}
}

func TestPlanGateNonInteractive(t *testing.T) {
	ask := askNode("draft")
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "plan body")
	verifier := verdictNode(true, nil)
	gate := &fixedGate{err: approval.ErrNonInteractive}
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModePlan),
		agent.StaticRegistry(ask, deliberate, agg, verifier), sinkFn, gate, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "plan"})
	if !errors.Is(err, approval.ErrNonInteractive) {
		t.Fatalf("错误 = %v, want ErrNonInteractive", err)
	}
	if res.Status != StatusFailed || res.ExitCode != 4 {
		t.Fatalf("结果 = %+v, want failed/4", res)
	}
	if eng.Mode() != model.ModePlan {
		t.Fatalf("非交互不应切模式: %q", eng.Mode())
	}
	failed := eventData(t, sink.first(model.EventSessionFailed))
	if failed["errorClass"] != string(model.ErrApprovalRequired) {
		t.Fatalf("errorClass = %v", failed["errorClass"])
	}
}

func TestFanoutSingleFailureContinues(t *testing.T) {
	code := codeNode([]string{"a.go"})
	deliberate := newDimNode(model.NodeDeliberate)
	deliberate.errs = map[model.Dimension]error{model.DimRisk: errors.New("risk 分支失败")}
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "merged")
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCouncil),
		agent.StaticRegistry(code, deliberate, agg, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "go"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("单分支失败应继续: %+v", res)
	}
	aggIn := agg.Inputs()
	if len(aggIn) != 1 {
		t.Fatalf("aggregation 执行次数 = %d", len(aggIn))
	}
	var priors []string
	for _, p := range aggIn[0].Prior {
		priors = append(priors, p.Text)
	}
	if !reflect.DeepEqual(priors, []string{"correctness", "execution"}) {
		t.Fatalf("缺失维度后 Prior = %v", priors)
	}
	if !strings.Contains(aggIn[0].Plan, "risk") {
		t.Fatalf("aggregation Plan 应提示缺失维度: %q", aggIn[0].Plan)
	}
	if sink.count(model.EventSessionFailed) != 0 {
		t.Fatalf("单分支失败不应 session_failed: %v", sink.types())
	}
}

func TestFanoutTwoFailuresAbort(t *testing.T) {
	code := codeNode([]string{"a.go"})
	deliberate := newDimNode(model.NodeDeliberate)
	deliberate.errs = map[model.Dimension]error{
		model.DimRisk:      errors.New("risk 失败"),
		model.DimExecution: errors.New("execution 失败"),
	}
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "merged")
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCouncil),
		agent.StaticRegistry(code, deliberate, agg, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "go"})
	if err == nil {
		t.Fatal("两个分支失败应返回错误")
	}
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("结果 = %+v, want failed/1", res)
	}
	if len(agg.Inputs()) != 0 {
		t.Fatalf("两个分支失败后不应执行 aggregation")
	}
	failed := eventData(t, sink.first(model.EventSessionFailed))
	if failed["reason"] != "fanout_failed" {
		t.Fatalf("reason = %v", failed["reason"])
	}
}

func TestPlanGateDeclinedStillSucceeds(t *testing.T) {
	ask := askNode("draft")
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "plan body")
	verifier := verdictNode(true, nil)
	gate := &fixedGate{accept: false}
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModePlan),
		agent.StaticRegistry(ask, deliberate, agg, verifier), sinkFn, gate, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "plan"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("未确认仍应成功（计划已输出）: %+v", res)
	}
	if res.PlanPath == "" {
		t.Fatal("未确认时也应返回 PlanPath")
	}
	if gate.count() != 1 {
		t.Fatalf("gate 调用次数 = %d", gate.count())
	}
	if eng.Mode() != model.ModePlan {
		t.Fatalf("未确认不应切模式: %q", eng.Mode())
	}
	if sink.count(model.EventModeChanged) != 0 {
		t.Fatalf("未确认不应写 mode_changed: %v", sink.types())
	}
}

func TestNodeProviderErrorExitCode(t *testing.T) {
	code := &agent.StaticNode{
		Spec: agent.NodeSpec{Name: model.NodeCode},
		Err: &model.Error{
			Class:   model.ErrProviderRateLimit,
			Code:    "429",
			Message: "rate limited",
		},
	}
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err == nil {
		t.Fatal("节点失败应返回错误")
	}
	if res.Status != StatusFailed || res.ExitCode != 5 {
		t.Fatalf("结果 = %+v, want failed/5", res)
	}
	failed := eventData(t, sink.first(model.EventSessionFailed))
	if failed["errorClass"] != string(model.ErrProviderRateLimit) {
		t.Fatalf("errorClass = %v", failed["errorClass"])
	}
	if sink.count(model.EventNodeCompleted) != 1 {
		t.Fatalf("node_completed 次数 = %d, want 1", sink.count(model.EventNodeCompleted))
	}
	if len(verifier.Inputs()) != 0 {
		t.Fatal("节点失败后不应继续执行 verifier")
	}
}

func TestEventSinkFailureReturnsError(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, nil)
	sinkFn := model.EventSinkFunc(func(context.Context, *model.Event) error {
		return errors.New("disk full")
	})
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("错误 = %v, want 事件写入错误", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("结果 = %+v", res)
	}
	if len(code.Inputs()) != 0 {
		t.Fatal("turn_started 写失败后不应执行节点")
	}
}

func TestCanceledContextInterrupts(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := eng.RunTurn(ctx, TurnInput{UserInput: "fix"})
	if err != nil {
		t.Fatalf("取消不应返回普通错误: %v", err)
	}
	if res.Status != StatusInterrupted || res.ExitCode != 3 {
		t.Fatalf("结果 = %+v, want interrupted/3", res)
	}
	if sink.count(model.EventSessionInterrupted) != 1 {
		t.Fatalf("session_interrupted 次数 = %d", sink.count(model.EventSessionInterrupted))
	}
	if sink.count(model.EventSessionFailed) != 0 {
		t.Fatal("取消不应写 session_failed")
	}
	if last := sink.last(); last.Type != model.EventTurnCompleted {
		t.Fatalf("末事件 = %s", last.Type)
	}
	if len(code.Inputs()) != 0 {
		t.Fatal("取消后不应执行节点")
	}
}

func TestValidateGraph(t *testing.T) {
	if err := ValidateGraph(codeGraph()); err != nil {
		t.Fatalf("内置 code 图应合法: %v", err)
	}
	bad := codeGraph()
	bad.Entry = "nope"
	if err := ValidateGraph(bad); err == nil {
		t.Fatal("缺失 Entry 应报错")
	}
	orphan := codeGraph()
	orphan.Steps["orphan"] = Step{ID: "orphan", Node: model.NodeAsk}
	if err := ValidateGraph(orphan); err == nil {
		t.Fatal("不可达 step 应报错")
	}
	for mode, g := range BuiltinGraphs() {
		if g.Mode != mode {
			t.Fatalf("图 %s 的 Mode = %s", mode, g.Mode)
		}
		if err := ValidateGraph(g); err != nil {
			t.Fatalf("内置图 %s 不合法: %v", mode, err)
		}
	}
}

func TestPlanModeRetryFeedsFailuresBackToAsk(t *testing.T) {
	ask := askNode("draft v1")
	deliberate := newDimNode(model.NodeDeliberate)
	releaseAfter(deliberate, 3)
	agg := aggregationNode("agg", "merged plan")
	passOut := verdictNode(true, nil).Output
	verifier := verdictNode(false, nil)
	failOut := verifier.Output
	var calls int
	verifier.OnRun = func(agent.NodeInput) {
		calls++
		if calls == 2 {
			verifier.Output = passOut
		}
	}
	verifier.Output = failOut
	gate := &fixedGate{accept: true}
	sink, sinkFn := newCapture()
	eng := NewEngine(testView(model.ModePlan),
		agent.StaticRegistry(ask, deliberate, agg, verifier), sinkFn, gate, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "plan it"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	askIn := ask.Inputs()
	if len(askIn) != 2 {
		t.Fatalf("ask 执行次数 = %d, want 2", len(askIn))
	}
	if len(askIn[1].Feedback) == 0 {
		t.Fatal("第二轮 ask 应拿到 verifier failures")
	}
	var priorNodes []model.NodeName
	for _, p := range askIn[1].Prior {
		priorNodes = append(priorNodes, p.Node)
	}
	if !reflect.DeepEqual(priorNodes, []model.NodeName{model.NodeAggregation, model.NodeVerifier}) {
		t.Fatalf("第二轮 ask Prior = %v", priorNodes)
	}
	if len(deliberate.snapshot()) != 6 {
		t.Fatalf("deliberate 执行次数 = %d, want 6（两轮各 3）", len(deliberate.snapshot()))
	}
	if gate.count() != 1 || gate.plans[0].Round != 2 {
		t.Fatalf("gate 调用 = %d, round = %v", gate.count(), gate.plans)
	}
	if sink.count(model.EventSessionFailed) != 0 {
		t.Fatalf("第二轮通过不应失败: %v", sink.types())
	}
}

func TestTurnBudget(t *testing.T) {
	b := newTurnBudget(testView(model.ModeCode), 2)
	if b.Round() != 0 {
		t.Fatalf("初始 Round = %d", b.Round())
	}
	if !b.TakeToolTurn() || !b.TakeToolTurn() {
		t.Fatal("预算内 TakeToolTurn 应成功")
	}
	if b.TakeToolTurn() {
		t.Fatal("工具轮耗尽后 TakeToolTurn 应失败")
	}
	b.addUsage(model.Usage{Input: 3, Output: 4, Cached: 1})
	if got := b.SpentTokens(); got != 8 {
		t.Fatalf("SpentTokens = %d, want 8", got)
	}
	b.setRound(2)
	if b.Round() != 2 {
		t.Fatalf("Round = %d, want 2", b.Round())
	}
	if b.Remaining() <= 0 {
		t.Fatalf("Remaining = %v, want > 0", b.Remaining())
	}
	b.reset(0)
	if b.Round() != 0 || b.SpentTokens() != 0 {
		t.Fatalf("reset 后 round=%d tokens=%d", b.Round(), b.SpentTokens())
	}
	if empty := newTurnBudget(nil, 0); empty.Remaining() != 0 {
		t.Fatalf("无配置时 Remaining = %v, want 0", empty.Remaining())
	}
}

func TestNodeMissingFailsTurn(t *testing.T) {
	code := codeNode([]string{"a.go"})
	sink, sinkFn := newCapture()
	// 故意不注册 verifier。
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err == nil {
		t.Fatal("缺少节点应返回错误")
	}
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("结果 = %+v, want failed/1", res)
	}
	failed := eventData(t, sink.first(model.EventSessionFailed))
	if failed["reason"] != "node_missing" {
		t.Fatalf("reason = %v", failed["reason"])
	}
	if failed["currentStep"] == "" {
		t.Fatalf("currentStep 缺失: %v", failed)
	}
}

type slowNode struct{ name model.NodeName }

func (n *slowNode) Name() model.NodeName     { return n.name }
func (n *slowNode) Tools() []model.ToolName  { return nil }
func (n *slowNode) Contract() agent.Contract { return nil }

func (n *slowNode) Run(ctx context.Context, _ agent.NodeInput) (agent.NodeOutput, error) {
	<-ctx.Done()
	return agent.NodeOutput{}, ctx.Err()
}

func TestNodeTimeout(t *testing.T) {
	view := fakeView{settings: config.Settings{
		DefaultMode: model.ModeCode,
		Limits: config.Limits{
			MaxToolTurns:  12,
			MaxRounds:     map[string]int{"code": 3},
			NodeTimeoutMs: 20,
		},
	}}
	code := &slowNode{name: model.NodeCode}
	verifier := verdictNode(true, nil)
	sink, sinkFn := newCapture()
	eng := NewEngine(view, agent.StaticRegistry(code, verifier), sinkFn, nil, logx.Nop())

	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err == nil {
		t.Fatal("节点超时应返回错误")
	}
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("结果 = %+v, want failed/1", res)
	}
	failed := eventData(t, sink.first(model.EventSessionFailed))
	if failed["reason"] != "node_timeout" {
		t.Fatalf("reason = %v, want node_timeout", failed["reason"])
	}
	if len(verifier.Inputs()) != 0 {
		t.Fatal("超时后不应继续执行 verifier")
	}
}

type fakeApprover struct {
	dec  approval.Decision
	err  error
	mu   sync.Mutex
	reqs []approval.Request
}

func (a *fakeApprover) Decide(_ context.Context, req approval.Request) (approval.Decision, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	return a.dec, a.err
}

func TestNewPlanGate(t *testing.T) {
	fa := &fakeApprover{dec: approval.Decision{Allow: true, Scope: approval.ScopeOnce, Reason: "user_allow"}}
	gate := NewPlanGate(fa)
	ok, err := gate.Confirm(context.Background(), Plan{Path: ".ant/sessions/plan.md", Body: "plan", Round: 2})
	if err != nil || !ok {
		t.Fatalf("Confirm = %v, %v", ok, err)
	}
	if len(fa.reqs) != 1 {
		t.Fatalf("审批次数 = %d", len(fa.reqs))
	}
	req := fa.reqs[0]
	if req.Class != model.ApprovalPlanToCode || req.Action != "plan_to_code" {
		t.Fatalf("审批请求 = %+v", req)
	}
	if len(req.Paths) != 1 || req.Paths[0] != ".ant/sessions/plan.md" {
		t.Fatalf("审批路径 = %v", req.Paths)
	}

	declined := NewPlanGate(&fakeApprover{dec: approval.Decision{Allow: false}})
	if ok, err := declined.Confirm(context.Background(), Plan{}); err != nil || ok {
		t.Fatalf("拒绝时 Confirm = %v, %v", ok, err)
	}

	if _, err := NewPlanGate(nil).Confirm(context.Background(), Plan{}); !errors.Is(err, approval.ErrNonInteractive) {
		t.Fatalf("nil approver 错误 = %v", err)
	}

	boom := errors.New("boom")
	if _, err := NewPlanGate(&fakeApprover{err: boom}).Confirm(context.Background(), Plan{}); !errors.Is(err, boom) {
		t.Fatalf("审批错误 = %v", err)
	}
}

func TestEngineWithNilDependencies(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, nil)
	// nil cfg / sink / gate / logger：引擎应回落到默认值而不是 panic。
	eng := NewEngine(nil, agent.StaticRegistry(code, verifier), nil, nil, nil)
	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "fix"})
	if err != nil {
		t.Fatalf("RunTurn 错误: %v", err)
	}
	if res.Status != StatusOK || res.ExitCode != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	if eng.Mode() != model.ModeCode {
		t.Fatalf("默认模式 = %q", eng.Mode())
	}
	if err := eng.SwitchMode(context.Background(), model.ModeAsk, "nil test", true); err != nil {
		t.Fatalf("SwitchMode 错误: %v", err)
	}
	if eng.Mode() != model.ModeAsk {
		t.Fatalf("Mode = %q", eng.Mode())
	}
}

func TestRunTurnHistoryPropagates(t *testing.T) {
	code := codeNode([]string{"a.go"})
	verifier := verdictNode(true, nil)
	eng := NewEngine(testView(model.ModeCode), agent.StaticRegistry(code, verifier), nil, nil, logx.Nop())

	history := []model.Message{
		{Role: model.RoleUser, Content: "旧问题"},
		{Role: model.RoleAssistant, Content: "旧回答"},
	}
	res, err := eng.RunTurn(context.Background(), TurnInput{UserInput: "新问题", History: history})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("RunTurn = %+v, %v", res, err)
	}

	in := code.Inputs()
	if len(in) != 1 {
		t.Fatalf("code 执行次数 = %d", len(in))
	}
	want := []model.Message{
		{Role: model.RoleUser, Content: "旧问题"},
		{Role: model.RoleAssistant, Content: "旧回答"},
		{Role: model.RoleUser, Content: "新问题"},
	}
	if !reflect.DeepEqual(in[0].History, want) {
		t.Fatalf("NodeInput.History = %+v, want %+v", in[0].History, want)
	}

	// 后续节点同样能看到旧历史 + 本轮 user + 已完成节点文本。
	vi := verifier.Inputs()
	if len(vi) != 1 {
		t.Fatalf("verifier 执行次数 = %d", len(vi))
	}
	if len(vi[0].History) != 4 {
		t.Fatalf("verifier History 长度 = %d, want 4", len(vi[0].History))
	}
	if vi[0].History[0].Content != "旧问题" || vi[0].History[1].Content != "旧回答" {
		t.Fatalf("verifier 丢失旧历史: %+v", vi[0].History)
	}
	if vi[0].History[2].Content != "新问题" {
		t.Fatalf("verifier 当前 user = %q", vi[0].History[2].Content)
	}
	if !strings.Contains(vi[0].History[3].Content, "## 变更") {
		t.Fatalf("verifier 应看到 code 输出: %+v", vi[0].History)
	}

	// 调用方传入的 History 不能被原地修改。
	if !reflect.DeepEqual(history, []model.Message{
		{Role: model.RoleUser, Content: "旧问题"},
		{Role: model.RoleAssistant, Content: "旧回答"},
	}) {
		t.Fatalf("TurnInput.History 被修改: %+v", history)
	}
}
