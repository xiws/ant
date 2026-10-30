package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// ---------- 测试替身 ----------

// fakeView 是只实现 config.View + config.Paths 的测试视图。
type fakeView struct {
	settings config.Settings
	workDir  string
}

func (v *fakeView) Settings() config.Settings           { return v.settings }
func (v *fakeView) Models() []config.ModelSpec          { return nil }
func (v *fakeView) Providers() []config.ProviderSpec    { return nil }
func (v *fakeView) Origin(string) (config.Origin, bool) { return "", false }
func (v *fakeView) Warnings() []string                  { return nil }
func (v *fakeView) WorkDir() string                     { return v.workDir }
func (v *fakeView) HomeDir() string                     { return "" }

func testView(t *testing.T, mutate func(*config.Settings)) *fakeView {
	t.Helper()
	s := config.Settings{
		Limits: config.Limits{
			ToolTimeoutMs:      5000,
			BashTimeoutMs:      120000,
			BashMaxTimeoutMs:   600000,
			MaxToolOutputBytes: 16384,
			MaxFileReadBytes:   262144,
			MaxFileReadLines:   2000,
			MaxWriteBytes:      2097152,
		},
		Sandbox: config.SandboxSettings{
			WorkspaceOnly:   true,
			ProtectedPaths:  []string{".git", ".ant"},
			AllowReadPaths:  []string{},
			AllowWritePaths: []string{},
		},
	}
	if mutate != nil {
		mutate(&s)
	}
	return &fakeView{settings: s, workDir: t.TempDir()}
}

// stubTool 是只依赖 spec + 闭包的 Tool。
type stubTool struct {
	spec model.ToolSpec
	exec func(context.Context, Invocation) (Result, error)
}

func (s *stubTool) Spec() model.ToolSpec { return s.spec }

func (s *stubTool) Execute(ctx context.Context, inv Invocation) (Result, error) {
	if s.exec == nil {
		return Result{OK: true, Data: map[string]any{"args": inv.Args}}, nil
	}
	return s.exec(ctx, inv)
}

func strictReadSpec() model.ToolSpec {
	return model.ToolSpec{
		Name:        string(model.ToolRead),
		Description: "stub read",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"},
				"startLine": {"type": "integer", "minimum": 1}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		Mutating:  false,
		Timeout:   2 * time.Second,
		Approval:  model.ApprovalFSReadOutside,
		MaxOutput: 16384,
	}
}

func looseSpec(name string, mutating bool, class model.ApprovalClass) model.ToolSpec {
	return model.ToolSpec{
		Name:        name,
		Description: "stub " + name,
		Schema:      json.RawMessage(`{"type":"object","additionalProperties":true}`),
		Mutating:    mutating,
		Timeout:     2 * time.Second,
		Approval:    class,
		MaxOutput:   16384,
	}
}

// fakeApprover 按闭包决策，并记录全部请求。
type fakeApprover struct {
	mu       sync.Mutex
	fn       func(approval.Request) (approval.Decision, error)
	requests []approval.Request
}

func (a *fakeApprover) Decide(ctx context.Context, req approval.Request) (approval.Decision, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	fn := a.fn
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return approval.Decision{Allow: true, Scope: approval.ScopeOnce, Reason: "user_allow"}, nil
}

func (a *fakeApprover) snapshot() []approval.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]approval.Request, len(a.requests))
	copy(out, a.requests)
	return out
}

// fakeSink 收集事件；err 非空时模拟写盘失败。
type fakeSink struct {
	mu     sync.Mutex
	events []model.Event
	err    error
}

func (s *fakeSink) Emit(ctx context.Context, ev *model.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, *ev)
	return nil
}

func (s *fakeSink) snapshot() []model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Event, len(s.events))
	copy(out, s.events)
	return out
}

// fakeStore 记录 artifact 全量内容。
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]byte
	err  error
}

func (s *fakeStore) PutArtifact(ctx context.Context, name string, body []byte, mediaType string) (model.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return model.ArtifactRef{}, s.err
	}
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	cp := append([]byte(nil), body...)
	s.data[name] = cp
	sum := sha256.Sum256(body)
	return model.ArtifactRef{Name: name, Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (s *fakeStore) entries() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.data))
	for k, v := range s.data {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// fakePrompter 记录 ask 问题并返回固定答案。
type fakePrompter struct {
	mu        sync.Mutex
	answer    approval.Answer
	err       error
	questions []approval.Question
}

func (p *fakePrompter) Confirm(ctx context.Context, prompt approval.ConfirmPrompt) (approval.Choice, error) {
	return approval.ChoiceOnce, nil
}

func (p *fakePrompter) Ask(ctx context.Context, q approval.Question) (approval.Answer, error) {
	p.mu.Lock()
	p.questions = append(p.questions, q)
	p.mu.Unlock()
	if p.err != nil {
		return approval.Answer{}, p.err
	}
	return p.answer, nil
}

func (p *fakePrompter) snapshot() []approval.Question {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]approval.Question, len(p.questions))
	copy(out, p.questions)
	return out
}

// testEnv 是一次测试的装配环境。
type testEnv struct {
	view     *fakeView
	catalog  *Catalog
	sink     *fakeSink
	store    *fakeStore
	approver *fakeApprover
	prompter *fakePrompter
}

func newTestEnv(t *testing.T, mutate func(*config.Settings)) *testEnv {
	t.Helper()
	return &testEnv{
		view:     testView(t, mutate),
		catalog:  NewCatalog(),
		sink:     &fakeSink{},
		store:    &fakeStore{},
		approver: &fakeApprover{},
		prompter: &fakePrompter{},
	}
}

func (e *testEnv) add(tools ...Tool) {
	for _, t := range tools {
		e.catalog.Register(t)
	}
}

func (e *testEnv) deps() Deps {
	return Deps{
		Guard:    NewPathGuard(e.view, e.view.WorkDir()),
		Versions: NewVersionTable(),
		Approver: e.approver,
		Prompter: e.prompter,
		Sink:     e.sink,
		Store:    e.store,
	}
}

func (e *testEnv) runtime() Runtime {
	return NewRuntime(e.view, e.catalog, e.deps())
}

// ---------- 断言辅助 ----------

type decodedMessage struct {
	OK        bool       `json:"ok"`
	Data      any        `json:"data"`
	Error     *ToolError `json:"error"`
	Truncated bool       `json:"truncated"`
	OutputRef string     `json:"outputRef"`
}

func decodeMessage(t *testing.T, msg model.Message) decodedMessage {
	t.Helper()
	var out decodedMessage
	if err := json.Unmarshal([]byte(msg.Content), &out); err != nil {
		t.Fatalf("tool 消息 content 不是合法 JSON: %v\ncontent=%s", err, msg.Content)
	}
	if out.OK && out.Error != nil {
		t.Fatalf("ok=true 时不应带 error: %s", msg.Content)
	}
	if !out.OK && out.Error == nil {
		t.Fatalf("ok=false 时必须带 error: %s", msg.Content)
	}
	return out
}

func dataMap(t *testing.T, data any) map[string]any {
	t.Helper()
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data 期望对象，收到 %T (%v)", data, data)
	}
	return m
}

func eventData(t *testing.T, ev model.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("事件 data 不是合法 JSON: %v (%s)", err, ev.Data)
	}
	return m
}

func requireCode(t *testing.T, msg model.Message, want string) decodedMessage {
	t.Helper()
	got := decodeMessage(t, msg)
	if got.OK {
		t.Fatalf("期望失败(code=%s)，实际成功: %s", want, msg.Content)
	}
	if got.Error.Code != want {
		t.Fatalf("错误码期望 %q，实际 %q（message=%s）", want, got.Error.Code, got.Error.Message)
	}
	return got
}

func eventTypes(events []model.Event) []model.EventType {
	out := make([]model.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

// ---------- Catalog / 内置 Spec ----------

func TestCatalogBuiltinSpecs(t *testing.T) {
	env := newTestEnv(t, nil)
	tk := NewToolkit(env.view, Deps{Sink: env.sink, Store: env.store})

	codeSpecs := tk.Registry.Specs(model.NodeCode)
	names := make([]string, len(codeSpecs))
	for i, spec := range codeSpecs {
		names[i] = spec.Name
	}
	want := []string{"ask", "bash", "edit", "read", "read_code", "search", "write"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("code 节点工具集合/顺序错误：got %v want %v", names, want)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("Specs 必须按 name 排序：%v", names)
	}

	byName := map[string]model.ToolSpec{}
	for _, spec := range codeSpecs {
		byName[spec.Name] = spec
	}
	checks := []struct {
		name     string
		mutating bool
		timeout  time.Duration
		class    model.ApprovalClass
	}{
		{"read", false, 5 * time.Second, model.ApprovalFSReadOutside},
		{"read_code", false, 5 * time.Second, model.ApprovalFSReadOutside},
		{"search", false, 5 * time.Second, model.ApprovalFSReadOutside},
		{"edit", true, 5 * time.Second, model.ApprovalFSWriteOutside},
		{"write", true, 5 * time.Second, model.ApprovalFSWriteOutside},
		{"bash", true, 120 * time.Second, model.ApprovalBashRisk},
		{"ask", false, 0, ""},
	}
	for _, want := range checks {
		spec, ok := byName[want.name]
		if !ok {
			t.Fatalf("缺少内置工具 %s", want.name)
		}
		if spec.Mutating != want.mutating {
			t.Errorf("%s Mutating=%v want %v", want.name, spec.Mutating, want.mutating)
		}
		if spec.Timeout != want.timeout {
			t.Errorf("%s Timeout=%v want %v", want.name, spec.Timeout, want.timeout)
		}
		if spec.Approval != want.class {
			t.Errorf("%s Approval=%q want %q", want.name, spec.Approval, want.class)
		}
		if spec.MaxOutput != 16384 {
			t.Errorf("%s MaxOutput=%d want 16384", want.name, spec.MaxOutput)
		}
		if strings.TrimSpace(spec.Description) == "" {
			t.Errorf("%s Description 为空", want.name)
		}
		var schema map[string]any
		if err := json.Unmarshal(spec.Schema, &schema); err != nil {
			t.Errorf("%s Schema 不是合法 JSON: %v", want.name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s Schema 根必须是 object: %v", want.name, schema["type"])
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s Schema 必须 additionalProperties=false", want.name)
		}
	}

	askSpecs := tk.Registry.Specs(model.NodeAsk)
	askNames := make([]string, len(askSpecs))
	for i, spec := range askSpecs {
		askNames[i] = spec.Name
	}
	if fmt.Sprint(askNames) != fmt.Sprint([]string{"ask", "bash", "read", "read_code", "search"}) {
		t.Fatalf("ask 节点工具集合错误：%v", askNames)
	}
	if got := tk.Registry.Specs(model.NodeName("unknown")); len(got) != 0 {
		t.Fatalf("未知节点不应有工具：%v", got)
	}
	for _, name := range want {
		if _, ok := tk.Registry.Lookup(name); !ok {
			t.Errorf("Lookup(%q) 失败", name)
		}
	}
	if _, ok := tk.Registry.Lookup("nope"); ok {
		t.Errorf("Lookup(nope) 不应成功")
	}
}

func TestBuiltinPlaceholderExecution(t *testing.T) {
	env := newTestEnv(t, nil)
	tk := NewToolkit(env.view, Deps{Sink: env.sink, Store: env.store})
	msgs, err := tk.Runtime.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch 返回错误: %v", err)
	}
	got := requireCode(t, msgs[0], "internal")
	if !strings.Contains(got.Error.Message, "后续阶段") {
		t.Fatalf("占位错误消息不符：%q", got.Error.Message)
	}
}

// ---------- 运行时流水线 ----------

func TestRuntimeSuccessPipeline(t *testing.T) {
	env := newTestEnv(t, nil)
	var captured Invocation
	env.add(&stubTool{spec: strictReadSpec(), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		captured = inv
		return Result{OK: true, Data: map[string]any{"path": inv.Args["path"], "startLine": inv.Args["startLine"]}}, nil
	}})
	rt := env.runtime()

	raw := json.RawMessage(`{"path":"pkg/a.go","startLine":"12"}`)
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{{ID: "call_1", Name: "read", Arguments: raw}})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("消息数=%d want 1", len(msgs))
	}
	if msgs[0].Role != model.RoleTool || msgs[0].ToolCallID != "call_1" {
		t.Fatalf("tool 消息角色/ID 错误：%+v", msgs[0])
	}
	body := decodeMessage(t, msgs[0])
	if !body.OK {
		t.Fatalf("期望成功: %s", msgs[0].Content)
	}
	data := dataMap(t, body.Data)
	if data["path"] != "pkg/a.go" {
		t.Fatalf("data.path=%v", data["path"])
	}
	if data["startLine"] != float64(12) {
		t.Fatalf("字符串 startLine 未转换为整数: %v (%T)", data["startLine"], data["startLine"])
	}

	// Invocation 契约
	if captured.CallID != "call_1" || captured.Node != model.NodeCode {
		t.Fatalf("Invocation 字段错误：%+v", captured)
	}
	if captured.Args["startLine"] != 12 {
		t.Fatalf("Invocation.Args 类型未转换: %T", captured.Args["startLine"])
	}
	if string(captured.Raw) != string(raw) {
		t.Fatalf("Invocation.Raw 应为参数原文")
	}
	if captured.Workspace != env.view.WorkDir() {
		t.Fatalf("Invocation.Workspace=%q", captured.Workspace)
	}

	// workspace 内读取自动放行，不触发人工审批（§12.3）。
	if reqs := env.approver.snapshot(); len(reqs) != 0 {
		t.Fatalf("workspace 内 read 不应审批，实际 %d 次", len(reqs))
	}

	// 事件
	events := env.sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("事件数=%d want 2：%v", len(events), eventTypes(events))
	}
	if events[0].Type != model.EventToolCall || events[1].Type != model.EventToolResult {
		t.Fatalf("事件类型错误：%v", eventTypes(events))
	}
	callData := eventData(t, events[0])
	if callData["id"] != "call_1" || callData["name"] != "read" || callData["approval"] != "auto" {
		t.Fatalf("tool_call 事件字段错误：%v", callData)
	}
	args, _ := callData["arguments"].(map[string]any)
	if args["path"] != "pkg/a.go" {
		t.Fatalf("tool_call.arguments 错误：%v", callData["arguments"])
	}
	resultData := eventData(t, events[1])
	if resultData["id"] != "call_1" || resultData["status"] != "ok" || resultData["ok"] != true {
		t.Fatalf("tool_result 事件字段错误：%v", resultData)
	}
	if _, hasErr := resultData["error"]; hasErr {
		t.Fatalf("成功事件不应带 error：%v", resultData)
	}
}

func TestRuntimeInvalidArgsAndSchemaMismatch(t *testing.T) {
	env := newTestEnv(t, nil)
	executed := 0
	env.add(&stubTool{spec: strictReadSpec(), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		executed++
		return Result{OK: true, Data: "should not run"}, nil
	}})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "bad_json", Name: "read", Arguments: json.RawMessage(`{"path":`)},
		{ID: "unknown_field", Name: "read", Arguments: json.RawMessage(`{"path":"a.go","extra":1}`)},
		{ID: "missing_required", Name: "read", Arguments: json.RawMessage(`{"startLine":2}`)},
		{ID: "bad_type", Name: "read", Arguments: json.RawMessage(`{"path":["a.go"]}`)},
		{ID: "bad_range", Name: "read", Arguments: json.RawMessage(`{"path":"a.go","startLine":0}`)},
	}
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if executed != 0 {
		t.Fatalf("参数非法时不应执行工具，executed=%d", executed)
	}
	wantCodes := []string{"invalid_args", "schema_mismatch", "invalid_args", "schema_mismatch", "schema_mismatch"}
	if len(msgs) != len(calls) {
		t.Fatalf("消息数=%d want %d", len(msgs), len(calls))
	}
	for i, msg := range msgs {
		if msg.ToolCallID != calls[i].ID {
			t.Fatalf("消息顺序/ID 错误：index %d got %q want %q", i, msg.ToolCallID, calls[i].ID)
		}
		requireCode(t, msg, wantCodes[i])
	}
	if reqs := env.approver.snapshot(); len(reqs) != 0 {
		t.Fatalf("参数非法时不应进入审批，实际 %d 次", len(reqs))
	}
	// 非法 JSON 原文仍写入 tool_call 事件（退化为字符串）。
	events := env.sink.snapshot()
	if len(events) != 2*len(calls) {
		t.Fatalf("事件数=%d want %d", len(events), 2*len(calls))
	}
	first := eventData(t, events[0])
	if _, ok := first["arguments"].(string); !ok {
		t.Fatalf("非法 JSON 参数应以字符串形式保留：%v", first["arguments"])
	}
}

func TestRuntimeRepairsFencedArguments(t *testing.T) {
	env := newTestEnv(t, nil)
	var gotPath string
	env.add(&stubTool{spec: strictReadSpec(), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		gotPath, _ = inv.Args["path"].(string)
		return Result{OK: true, Data: map[string]any{"path": gotPath}}, nil
	}})
	rt := env.runtime()
	raw := json.RawMessage("```json\n{\"path\": \"pkg/a.go\",}\n```")
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{{ID: "c1", Name: "read", Arguments: raw}})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if gotPath != "pkg/a.go" {
		t.Fatalf("修复后的参数未生效：%q", gotPath)
	}
	if body := decodeMessage(t, msgs[0]); !body.OK {
		t.Fatalf("期望成功：%s", msgs[0].Content)
	}
}

func TestRuntimeApprovalDenied(t *testing.T) {
	env := newTestEnv(t, nil)
	var mu sync.Mutex
	var executed []string
	env.add(&stubTool{spec: strictReadSpec(), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		mu.Lock()
		executed = append(executed, inv.Args["path"].(string))
		mu.Unlock()
		return Result{OK: true, Data: map[string]any{"path": inv.Args["path"]}}, nil
	}})
	env.approver.fn = func(req approval.Request) (approval.Decision, error) {
		if len(req.Paths) > 0 && strings.HasSuffix(req.Paths[0], "denied.go") {
			return approval.Decision{Allow: false, Scope: approval.ScopeOnce, Reason: "user_deny"}, nil
		}
		return approval.Decision{Allow: true, Scope: approval.ScopeOnce, Reason: "user_allow"}, nil
	}
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"../denied.go"}`)},
		{ID: "c2", Name: "read", Arguments: json.RawMessage(`{"path":"../allowed.go"}`)},
	}
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	requireCode(t, msgs[0], "approval_denied")
	if body := decodeMessage(t, msgs[1]); !body.OK {
		t.Fatalf("拒绝不影响同批其他调用：%s", msgs[1].Content)
	}
	mu.Lock()
	if len(executed) != 1 || executed[0] != "../allowed.go" {
		t.Fatalf("被拒绝的调用不应执行：%v", executed)
	}
	mu.Unlock()

	events := env.sink.snapshot()
	if len(events) != 4 {
		t.Fatalf("事件数=%d want 4", len(events))
	}
	var callEv, resultEv *model.Event
	for i := range events {
		data := eventData(t, events[i])
		if data["id"] != "c1" {
			continue
		}
		switch events[i].Type {
		case model.EventToolCall:
			callEv = &events[i]
		case model.EventToolResult:
			resultEv = &events[i]
		}
	}
	if callEv == nil || resultEv == nil {
		t.Fatalf("缺少 c1 的事件：%v", eventTypes(events))
	}
	if got := eventData(t, *callEv)["approval"]; got != "user_deny" {
		t.Fatalf("tool_call.approval=%v want user_deny", got)
	}
	res := eventData(t, *resultEv)
	if res["status"] != "error" {
		t.Fatalf("tool_result.status=%v", res["status"])
	}
	errObj, _ := res["error"].(map[string]any)
	if errObj["code"] != "approval_denied" {
		t.Fatalf("tool_result.error.code=%v", errObj["code"])
	}
}

func TestRuntimeToolNotFoundAndNotAllowed(t *testing.T) {
	env := newTestEnv(t, nil)
	env.add(&stubTool{spec: looseSpec("edit", true, model.ApprovalFSWriteOutside)})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "edit", Arguments: json.RawMessage(`{"path":"a.go","diff":"x"}`)},
		{ID: "c2", Name: "nonexistent", Arguments: json.RawMessage(`{}`)},
	}
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeAsk, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	requireCode(t, msgs[0], "tool_not_allowed")
	requireCode(t, msgs[1], "tool_not_found")
	if reqs := env.approver.snapshot(); len(reqs) != 0 {
		t.Fatalf("权限失败时不应进入审批：%d", len(reqs))
	}
}

func TestRuntimeReadConcurrencyAndStableOrder(t *testing.T) {
	env := newTestEnv(t, nil)
	started := make(chan string, 3)
	release := make(chan struct{})
	env.add(&stubTool{spec: looseSpec("read", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		path, _ := inv.Args["path"].(string)
		started <- path
		select {
		case <-release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		return Result{OK: true, Data: map[string]any{"path": path}}, nil
	}})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
		{ID: "c2", Name: "read", Arguments: json.RawMessage(`{"path":"b"}`)},
		{ID: "c3", Name: "read", Arguments: json.RawMessage(`{"path":"c"}`)},
	}

	var msgs []model.Message
	var execErr error
	done := make(chan struct{})
	go func() {
		msgs, execErr = rt.ExecuteBatch(context.Background(), model.NodeCode, calls)
		close(done)
	}()

	seen := make([]string, 0, 3)
	timer := time.After(3 * time.Second)
	for len(seen) < 3 {
		select {
		case p := <-started:
			seen = append(seen, p)
		case <-timer:
			close(release)
			<-done
			t.Fatalf("只读调用没有并发执行，仅启动 %v", seen)
		}
	}
	close(release)
	<-done
	if execErr != nil {
		t.Fatalf("ExecuteBatch: %v", execErr)
	}
	sort.Strings(seen)
	if fmt.Sprint(seen) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("3 个只读调用未全部开始：%v", seen)
	}
	if len(msgs) != 3 {
		t.Fatalf("消息数=%d", len(msgs))
	}
	for i, msg := range msgs {
		if msg.ToolCallID != calls[i].ID {
			t.Fatalf("结果必须按原始 calls 顺序回填：index %d got %q want %q", i, msg.ToolCallID, calls[i].ID)
		}
		data := dataMap(t, decodeMessage(t, msg).Data)
		want := map[int]string{0: "a", 1: "b", 2: "c"}[i]
		if data["path"] != want {
			t.Fatalf("index %d data.path=%v want %v", i, data["path"], want)
		}
	}
}

func TestRuntimeReadConcurrencyLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	var mu sync.Mutex
	inflight, peak := 0, 0
	env.add(&stubTool{spec: looseSpec("search", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return Result{OK: true, Data: "ok"}, nil
	}})
	rt := env.runtime()
	calls := make([]model.ToolCall, 0, 7)
	for i := 0; i < 7; i++ {
		calls = append(calls, model.ToolCall{
			ID:        fmt.Sprintf("c%d", i),
			Name:      "search",
			Arguments: json.RawMessage(`{"query":"x"}`),
		})
	}
	if _, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > maxParallelReads {
		t.Fatalf("只读并发峰值 %d 超过上限 %d", peak, maxParallelReads)
	}
	if peak < 2 {
		t.Fatalf("只读调用没有并发，峰值 %d", peak)
	}
}

func TestRuntimeWritesSerialized(t *testing.T) {
	env := newTestEnv(t, nil)
	var mu sync.Mutex
	inflight, peak := 0, 0
	env.add(&stubTool{spec: looseSpec("bash", true, model.ApprovalBashRisk), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return Result{OK: true, Data: map[string]any{"exitCode": 0}, Meta: ResultMeta{ExitCode: 0}}, nil
	}})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo 1"}`)},
		{ID: "c2", Name: "bash", Arguments: json.RawMessage(`{"command":"echo 2"}`)},
	}
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ToolCallID != "c1" || msgs[1].ToolCallID != "c2" {
		t.Fatalf("写工具结果顺序错误：%+v", msgs)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("写工具必须串行，峰值并发=%d", peak)
	}
}

func TestRuntimeTruncationWritesArtifact(t *testing.T) {
	env := newTestEnv(t, func(s *config.Settings) {
		s.Limits.MaxToolOutputBytes = 200
	})
	long := strings.Repeat("x", 2000)
	env.add(&stubTool{spec: looseSpec("search", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{OK: true, Data: map[string]any{"text": long}}, nil
	}})
	rt := env.runtime()
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "call_9", Name: "search", Arguments: json.RawMessage(`{"query":"x"}`)},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	body := decodeMessage(t, msgs[0])
	if !body.OK {
		t.Fatalf("截断不应改变 ok: %s", msgs[0].Content)
	}
	if !body.Truncated || body.OutputRef == "" {
		t.Fatalf("消息缺少截断标记/引用：%s", msgs[0].Content)
	}
	data := dataMap(t, body.Data)
	preview, _ := data["preview"].(string)
	if !strings.Contains(preview, "已截断") || !strings.Contains(preview, body.OutputRef) {
		t.Fatalf("预览缺少截断标注：%q", preview)
	}
	if len(preview) > 400 {
		t.Fatalf("预览过长：%d", len(preview))
	}

	artifacts := env.store.entries()
	if len(artifacts) != 1 {
		t.Fatalf("artifact 数=%d want 1", len(artifacts))
	}
	var full []byte
	for _, v := range artifacts {
		full = v
	}
	if !strings.Contains(string(full), long) {
		t.Fatalf("artifact 未保存完整内容（len=%d）", len(full))
	}
	if _, ok := artifacts[body.OutputRef]; !ok {
		t.Fatalf("artifact 名与 outputRef 不一致：%v vs %q", keysOf(artifacts), body.OutputRef)
	}

	events := env.sink.snapshot()
	result := eventData(t, events[1])
	if result["truncated"] != true || result["outputRef"] != body.OutputRef {
		t.Fatalf("tool_result 事件截断信息错误：%v", result)
	}

	// 未超限：不写 artifact，也不带截断标记。
	env2 := newTestEnv(t, func(s *config.Settings) { s.Limits.MaxToolOutputBytes = 200 })
	env2.add(&stubTool{spec: looseSpec("search", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{OK: true, Data: map[string]any{"text": "short"}}, nil
	}})
	msgs, err = env2.runtime().ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"query":"x"}`)},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if body := decodeMessage(t, msgs[0]); body.Truncated || body.OutputRef != "" {
		t.Fatalf("未超限不应截断/写 artifact：%s", msgs[0].Content)
	}
	if n := len(env2.store.entries()); n != 0 {
		t.Fatalf("未超限写了 %d 个 artifact", n)
	}
}

func TestRuntimeEventOrder(t *testing.T) {
	env := newTestEnv(t, nil)
	env.add(&stubTool{spec: looseSpec("read", false, model.ApprovalFSReadOutside)})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
		{ID: "c2", Name: "read", Arguments: json.RawMessage(`{"path":"b"}`)},
	}
	if _, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	events := env.sink.snapshot()
	// 事件分两段且确定：先按 calls 顺序写全部 tool_call（写失败即中止，不执行副作用），
	// 再按 calls 顺序写 tool_result。每个 id 的 tool_call 必须先于其 tool_result。
	wantTypes := []model.EventType{
		model.EventToolCall, model.EventToolCall,
		model.EventToolResult, model.EventToolResult,
	}
	if fmt.Sprint(eventTypes(events)) != fmt.Sprint(wantTypes) {
		t.Fatalf("事件顺序错误：%v", eventTypes(events))
	}
	wantIDs := []string{"c1", "c2", "c1", "c2"}
	for i, ev := range events {
		if got := eventData(t, ev)["id"]; got != wantIDs[i] {
			t.Fatalf("index %d id=%v want %v", i, got, wantIDs[i])
		}
	}
	pos := map[string]int{}
	for i, ev := range events {
		data := eventData(t, ev)
		key := fmt.Sprint(data["id"], "/", ev.Type)
		if _, seen := pos[key]; !seen {
			pos[key] = i
		}
	}
	for _, id := range []string{"c1", "c2"} {
		callAt, okCall := pos[id+"/tool_call"]
		resultAt, okResult := pos[id+"/tool_result"]
		if !okCall || !okResult || callAt > resultAt {
			t.Fatalf("%s 的 tool_call 必须先于 tool_result：%v", id, pos)
		}
	}
}

func TestRuntimeSinkFailureIsFatal(t *testing.T) {
	env := newTestEnv(t, nil)
	executed := false
	env.add(&stubTool{spec: looseSpec("read", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
		executed = true
		return Result{OK: true, Data: "ok"}, nil
	}})
	env.sink.err = errors.New("disk full")
	rt := env.runtime()
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
	})
	if err == nil {
		t.Fatalf("Sink 失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("错误未保留底层原因：%v", err)
	}
	if msgs != nil {
		t.Fatalf("致命错误时应返回 nil 消息，got %d", len(msgs))
	}
	if executed {
		t.Fatalf("Sink 失败后不应继续执行副作用工具")
	}
}

func TestRuntimeToolPanicAndTimeout(t *testing.T) {
	t.Run("panic 转结构化错误", func(t *testing.T) {
		env := newTestEnv(t, nil)
		env.add(&stubTool{spec: looseSpec("read", false, model.ApprovalFSReadOutside), exec: func(ctx context.Context, inv Invocation) (Result, error) {
			panic("boom")
		}})
		msgs, err := env.runtime().ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
			{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
		})
		if err != nil {
			t.Fatalf("ExecuteBatch: %v", err)
		}
		got := requireCode(t, msgs[0], "internal")
		if !strings.Contains(got.Error.Message, "panic") {
			t.Fatalf("panic 未转成错误消息：%q", got.Error.Message)
		}
	})

	t.Run("超时", func(t *testing.T) {
		env := newTestEnv(t, nil)
		spec := looseSpec("read", false, model.ApprovalFSReadOutside)
		spec.Timeout = 20 * time.Millisecond
		env.add(&stubTool{spec: spec, exec: func(ctx context.Context, inv Invocation) (Result, error) {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}})
		msgs, err := env.runtime().ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
			{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
		})
		if err != nil {
			t.Fatalf("ExecuteBatch: %v", err)
		}
		requireCode(t, msgs[0], "timeout")
	})
}

func TestRuntimeOneMessagePerCallID(t *testing.T) {
	env := newTestEnv(t, func(s *config.Settings) { s.Limits.MaxToolOutputBytes = 4096 })
	env.add(
		&stubTool{spec: strictReadSpec()},
		&stubTool{spec: looseSpec("ask", false, "")},
	)
	env.approver.fn = func(req approval.Request) (approval.Decision, error) {
		if len(req.Paths) > 0 && strings.HasSuffix(req.Paths[0], "denied.go") {
			return approval.Decision{Allow: false, Reason: "user_deny"}, nil
		}
		return approval.Decision{Allow: true, Reason: "user_allow"}, nil
	}
	calls := []model.ToolCall{
		{ID: "ok", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
		{ID: "bad", Name: "read", Arguments: json.RawMessage(`{"path":`)},
		{ID: "denied", Name: "read", Arguments: json.RawMessage(`{"path":"denied.go"}`)},
		{ID: "forbidden", Name: "write", Arguments: json.RawMessage(`{"path":"a.go","content":"x"}`)},
		{ID: "missing", Name: "nope", Arguments: json.RawMessage(`{}`)},
	}
	msgs, err := env.runtime().ExecuteBatch(context.Background(), model.NodeAsk, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if len(msgs) != len(calls) {
		t.Fatalf("每个 tool_call_id 必须恰好一条消息：got %d want %d", len(msgs), len(calls))
	}
	seen := map[string]int{}
	for i, msg := range msgs {
		if msg.Role != model.RoleTool {
			t.Errorf("index %d role=%q", i, msg.Role)
		}
		if msg.ToolCallID != calls[i].ID {
			t.Errorf("index %d id=%q want %q", i, msg.ToolCallID, calls[i].ID)
		}
		seen[msg.ToolCallID]++
		decodeMessage(t, msg) // 必须是合法 JSON
	}
	for _, id := range []string{"ok", "bad", "denied", "forbidden", "missing"} {
		if seen[id] != 1 {
			t.Errorf("id %q 消息数=%d want 1", id, seen[id])
		}
	}
}

// ---------- ask 工具 ----------

func TestAskToolUsesPrompter(t *testing.T) {
	env := newTestEnv(t, nil)
	env.prompter.answer = approval.Answer{SelectedID: "2", Text: "选第二个"}
	tk := NewToolkit(env.view, Deps{
		Prompter: env.prompter,
		Sink:     env.sink,
		Store:    env.store,
	})
	args := json.RawMessage(`{
		"question": "用哪个方案？",
		"options": [
			{"id": "1", "label": "快"},
			{"id": "2", "label": "稳", "description": "更保守"}
		],
		"allowFreeText": true
	}`)
	msgs, err := tk.Runtime.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "call_ask", Name: "ask", Arguments: args},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	body := decodeMessage(t, msgs[0])
	if !body.OK {
		t.Fatalf("ask 应成功：%s", msgs[0].Content)
	}
	data := dataMap(t, body.Data)
	if data["selectedId"] != "2" || data["text"] != "选第二个" {
		t.Fatalf("ask 返回数据错误：%v", data)
	}
	qs := env.prompter.snapshot()
	if len(qs) != 1 {
		t.Fatalf("Prompter 调用次数=%d", len(qs))
	}
	if qs[0].Question != "用哪个方案？" || len(qs[0].Options) != 2 || qs[0].Options[1].Label != "稳" {
		t.Fatalf("提问内容错误：%+v", qs[0])
	}
	if !qs[0].AllowFreeText {
		t.Fatalf("allowFreeText 未传递")
	}
}

func TestAskToolWithoutPrompter(t *testing.T) {
	env := newTestEnv(t, nil)
	tk := NewToolkit(env.view, Deps{Sink: env.sink, Store: env.store})
	msgs, err := tk.Runtime.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "c1", Name: "ask", Arguments: json.RawMessage(`{"question":"在吗"}`)},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	requireCode(t, msgs[0], "requires_user_input")
}

// ---------- Validator ----------

func TestValidator(t *testing.T) {
	spec := model.ToolSpec{
		Name: "t",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"},
				"count": {"type": "integer", "minimum": 1, "maximum": 10},
				"ratio": {"type": "number"},
				"flag": {"type": "boolean"},
				"mode": {"type": "string", "enum": ["fast", "slow"]},
				"tags": {"type": "array", "items": {"type": "string"}},
				"options": {
					"type": "object",
					"properties": {"label": {"type": "string"}},
					"required": ["label"],
					"additionalProperties": false
				}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
	}
	v := NewValidator()

	args, terr := v.Validate(spec, json.RawMessage(`{
		"path": "a.go", "count": "3", "ratio": "1.5", "flag": "true",
		"mode": "fast", "tags": [1, "x"], "options": {"label": 7}
	}`))
	if terr != nil {
		t.Fatalf("合法参数被拒：%+v", terr)
	}
	if args["path"] != "a.go" || args["count"] != 3 || args["ratio"] != 1.5 || args["flag"] != true {
		t.Fatalf("类型转换错误：%#v", args)
	}
	if mode, _ := args["mode"].(string); mode != "fast" {
		t.Fatalf("enum 值错误：%v", args["mode"])
	}
	tags, _ := args["tags"].([]any)
	if len(tags) != 2 || tags[0] != "1" || tags[1] != "x" {
		t.Fatalf("数组逐项转换错误：%#v", args["tags"])
	}
	opts, _ := args["options"].(map[string]any)
	if opts["label"] != "7" {
		t.Fatalf("嵌套对象转换错误：%#v", args["options"])
	}

	// 数字 → 字符串
	args, terr = v.Validate(spec, json.RawMessage(`{"path": 123}`))
	if terr != nil {
		t.Fatalf("数字转字符串失败：%+v", terr)
	}
	if args["path"] != "123" {
		t.Fatalf("path=%v want \"123\"", args["path"])
	}

	// 整数值的浮点被接受
	args, terr = v.Validate(spec, json.RawMessage(`{"path":"a","count":3.0}`))
	if terr != nil {
		t.Fatalf("整数值浮点应被接受：%+v", terr)
	}
	if args["count"] != 3 {
		t.Fatalf("count=%v want 3", args["count"])
	}

	cases := []struct {
		name string
		raw  string
		code string
	}{
		{"非法 JSON", `{"path":`, "invalid_args"},
		{"根不是对象", `[]`, "invalid_args"},
		{"多余内容", `{"path":"a"} {}`, "invalid_args"},
		{"缺少必填", `{"count":1}`, "invalid_args"},
		{"空参数", ``, "invalid_args"},
		{"未知字段", `{"path":"a","nope":1}`, "schema_mismatch"},
		{"类型错误", `{"path":["a"]}`, "schema_mismatch"},
		{"整数非整", `{"path":"a","count":1.5}`, "schema_mismatch"},
		{"整数下界", `{"path":"a","count":0}`, "schema_mismatch"},
		{"整数上界", `{"path":"a","count":11}`, "schema_mismatch"},
		{"整数溢出", `{"path":"a","count":1e30}`, "schema_mismatch"},
		{"枚举非法", `{"path":"a","mode":"medium"}`, "schema_mismatch"},
		{"数组类型错误", `{"path":"a","tags":{}}`, "schema_mismatch"},
		{"布尔类型错误", `{"path":"a","flag":"yes"}`, "schema_mismatch"},
		{"数字类型错误", `{"path":"a","ratio":"NaN"}`, "schema_mismatch"},
		{"缺嵌套必填", `{"path":"a","options":{}}`, "invalid_args"},
		{"嵌套未知字段", `{"path":"a","options":{"label":"x","z":1}}`, "schema_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := v.Validate(spec, json.RawMessage(tc.raw))
			if got == nil {
				t.Fatalf("期望错误 %s，实际通过", tc.code)
			}
			if got.Code != tc.code {
				t.Fatalf("错误码 got %q want %q（message=%s）", got.Code, tc.code, got.Message)
			}
			if strings.TrimSpace(got.Message) == "" || strings.TrimSpace(got.Hint) == "" {
				t.Fatalf("错误消息/hint 不能为空：%+v", got)
			}
		})
	}

	// 无 schema 的工具接受任意对象
	_, terr = v.Validate(model.ToolSpec{Name: "free"}, json.RawMessage(`{"any":1}`))
	if terr != nil {
		t.Fatalf("无 schema 应放行：%+v", terr)
	}
}

// ---------- Repairer ----------

func TestRepairer(t *testing.T) {
	r := NewRepairer()
	cases := []struct {
		name     string
		in       string
		want     string
		repaired bool
	}{
		{"合法 JSON 原样", `{"path":"a.go"}`, `{"path":"a.go"}`, false},
		{"裸对象", `{"path":"a.go"}`, `{"path":"a.go"}`, false},
		{"围栏", "```json\n{\"path\":\"a.go\"}\n```", `{"path":"a.go"}`, true},
		{"围栏+尾逗号", "```json\n{\"path\":\"a.go\",}\n```", `{"path":"a.go"}`, true},
		{"尾逗号", `{"path":"a.go",}`, `{"path":"a.go"}`, true},
		{"嵌套尾逗号", `{"a":[1,2,],"b":3,}`, `{"a":[1,2],"b":3}`, true},
		{"前后噪声", `说明文字 {"path":"a.go"} 结束`, `{"path":"a.go"}`, true},
		{"字符串内逗号", `{"path":"a,}"}`, `{"path":"a,}"}`, false},
		{"未闭合不修复", `{"path":"a.go"`, `{"path":"a.go"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, repaired := r.Repair([]byte(tc.in))
			if string(out) != tc.want {
				t.Fatalf("Repair(%q)=%q want %q", tc.in, out, tc.want)
			}
			if repaired != tc.repaired {
				t.Fatalf("repaired=%v want %v（out=%q）", repaired, tc.repaired, out)
			}
			if repaired && !json.Valid(out) {
				t.Fatalf("repaired=true 时必须产出合法 JSON：%q", out)
			}
		})
	}

	valid := []byte(`{"a":"b"}`)
	out, repaired := r.Repair(valid)
	if repaired || string(out) != string(valid) {
		t.Fatalf("合法输入不应改动：%q %v", out, repaired)
	}
}

// ---------- Truncator ----------

func TestTruncatorHeadTail(t *testing.T) {
	env := newTestEnv(t, func(s *config.Settings) { s.Limits.MaxToolOutputBytes = 100 })
	store := &fakeStore{}
	tr := NewTruncator(env.view, store)
	body := []byte(strings.Repeat("a", 50) + strings.Repeat("中", 100))
	kept, ref, truncated, err := tr.Clip(context.Background(), "call_1", body)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if !truncated || ref.Name == "" {
		t.Fatalf("期望截断且写 artifact：truncated=%v ref=%+v", truncated, ref)
	}
	if !strings.HasPrefix(string(kept), "aaa") {
		t.Fatalf("缺少 head 内容：%q", kept)
	}
	if !strings.HasSuffix(string(kept), "中") {
		t.Fatalf("缺少 tail 内容")
	}
	if !strings.Contains(string(kept), "已截断") || !strings.Contains(string(kept), ref.Name) {
		t.Fatalf("缺少中间标注：%q", kept)
	}
	if len(kept) > 200 {
		t.Fatalf("截断后长度失控：%d", len(kept))
	}
	artifacts := store.entries()
	if len(artifacts) != 1 || len(artifacts[ref.Name]) != len(body) {
		t.Fatalf("artifact 未保存完整内容：%v", keysOf(artifacts))
	}

	small := []byte("short")
	kept, ref, truncated, err = tr.Clip(context.Background(), "call_2", small)
	if err != nil || truncated || !ref.IsZero() || string(kept) != "short" {
		t.Fatalf("未超限不应截断：kept=%q ref=%+v truncated=%v err=%v", kept, ref, truncated, err)
	}
}

// ---------- PathGuard ----------

func TestPathGuard(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	outside := t.TempDir()
	realWS := resolveExisting(ws)
	realOutside := resolveExisting(outside)

	g := NewPathGuard(&fakeView{workDir: ws, settings: testView(t, nil).settings}, ws)

	t.Run("workspace 内相对路径", func(t *testing.T) {
		got, err := g.Resolve(ctx, "pkg/a.go", AccessRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Outside {
			t.Fatalf("workspace 内路径不应越界")
		}
		if got.Abs != filepath.Join(realWS, "pkg", "a.go") {
			t.Fatalf("Abs=%q", got.Abs)
		}
		if got.Rel != "pkg/a.go" {
			t.Fatalf("Rel=%q", got.Rel)
		}
	})

	t.Run("workspace 内绝对路径", func(t *testing.T) {
		got, err := g.Resolve(ctx, filepath.Join(ws, "b.go"), AccessRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Outside || got.Abs != filepath.Join(realWS, "b.go") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("dotdot 逃逸", func(t *testing.T) {
		for _, p := range []string{"..", "../escape.go", "pkg/../../escape.go"} {
			got, err := g.Resolve(ctx, p, AccessRead)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", p, err)
			}
			if !got.Outside {
				t.Fatalf("%q 应判定为越界：%+v", p, got)
			}
		}
	})

	t.Run("workspace 外绝对路径", func(t *testing.T) {
		got, err := g.Resolve(ctx, filepath.Join(outside, "secret.txt"), AccessRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !got.Outside {
			t.Fatalf("workspace 外路径必须标记越界，不得静默放行：%+v", got)
		}
	})

	t.Run("保护路径写拒绝读放行", func(t *testing.T) {
		for _, p := range []string{".git/config", ".ant/session/events.ndjson", "sub/.git/index"} {
			_, err := g.Resolve(ctx, p, AccessWrite)
			if err == nil {
				t.Fatalf("%q 写操作应被拒绝", p)
			}
			var me *model.Error
			if !errors.As(err, &me) {
				t.Fatalf("%q 错误应为 *model.Error，got %T", p, err)
			}
			if me.Code != "path_protected" {
				t.Fatalf("%q 错误码=%q want path_protected", p, me.Code)
			}
			if _, rerr := g.Resolve(ctx, p, AccessRead); rerr != nil {
				t.Fatalf("%q 读操作不应被拒绝：%v", p, rerr)
			}
		}
	})

	t.Run("allowReadPaths", func(t *testing.T) {
		v := testView(t, func(s *config.Settings) {
			s.Sandbox.AllowReadPaths = []string{outside}
		})
		v.workDir = ws
		guard := NewPathGuard(v, ws)
		target := filepath.Join(outside, "notes.txt")
		got, err := guard.Resolve(ctx, target, AccessRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Outside || got.Abs != filepath.Join(realOutside, "notes.txt") {
			t.Fatalf("allowReadPaths 未生效：%+v", got)
		}
		if wgot, werr := guard.Resolve(ctx, target, AccessWrite); werr != nil || !wgot.Outside {
			t.Fatalf("allowReadPaths 不应放行写：%+v err=%v", wgot, werr)
		}
	})

	t.Run("allowWritePaths", func(t *testing.T) {
		v := testView(t, func(s *config.Settings) {
			s.Sandbox.AllowWritePaths = []string{outside}
		})
		v.workDir = ws
		guard := NewPathGuard(v, ws)
		target := filepath.Join(outside, "out.txt")
		got, err := guard.Resolve(ctx, target, AccessWrite)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Outside {
			t.Fatalf("allowWritePaths 未生效：%+v", got)
		}
		if rgot, rerr := guard.Resolve(ctx, target, AccessRead); rerr != nil || !rgot.Outside {
			t.Fatalf("allowWritePaths 不应放行读：%+v err=%v", rgot, rerr)
		}
	})

	t.Run("符号链接逃逸", func(t *testing.T) {
		link := filepath.Join(ws, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("无法创建符号链接：%v", err)
		}
		got, err := g.Resolve(ctx, "link/secret.txt", AccessRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !got.Outside {
			t.Fatalf("符号链接逃逸必须标记越界：%+v", got)
		}
		if !strings.HasPrefix(got.Abs, realOutside) {
			t.Fatalf("Abs=%q 未解析到真实路径 %q", got.Abs, realOutside)
		}
	})

	t.Run("Created 标记父目录缺失", func(t *testing.T) {
		got, err := g.Resolve(ctx, "newdir/sub/file.txt", AccessWrite)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !got.Created {
			t.Fatalf("父目录不存在时 Created 应为 true：%+v", got)
		}
		if err := os.MkdirAll(filepath.Join(ws, "newdir", "sub"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		got, err = g.Resolve(ctx, "newdir/sub/file.txt", AccessWrite)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Created {
			t.Fatalf("父目录存在时 Created 应为 false：%+v", got)
		}
	})

	t.Run("空路径报错", func(t *testing.T) {
		_, err := g.Resolve(ctx, "  ", AccessRead)
		if err == nil {
			t.Fatalf("空路径必须报错")
		}
		var me *model.Error
		if !errors.As(err, &me) || me.Code != "invalid_args" {
			t.Fatalf("错误应为 *model.Error{Code:invalid_args}，got %v", err)
		}
	})
}

// ---------- VersionTable ----------

func TestVersionTableConcurrent(t *testing.T) {
	vt := NewVersionTable()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("file-%d.go", i)
			vt.RecordRead(path, "r")
			vt.RecordWrite(path, "w")
			if sha, ok := vt.LastRead(path); !ok || sha != "w" {
				t.Errorf("LastRead(%q)=%q,%v", path, sha, ok)
			}
		}(i)
	}
	wg.Wait()
	if _, ok := vt.LastRead("missing"); ok {
		t.Fatalf("未记录的路径不应命中")
	}
}

// ---------- 辅助 ----------

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
