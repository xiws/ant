package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	ctxmgr "github.com/xiws/ant/internal/context"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/prompt"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/provider/fake"
	"github.com/xiws/ant/internal/tool"
)

type fixedRegistry struct{ res provider.Resolved }

func (r fixedRegistry) Resolve(string) (provider.Resolved, error)                { return r.res, nil }
func (r fixedRegistry) ResolveForNode(model.NodeName) (provider.Resolved, error) { return r.res, nil }
func (r fixedRegistry) Capabilities(string) (model.Capabilities, error) {
	return r.res.Capabilities, nil
}
func (r fixedRegistry) Adapter(provider.Resolved) (provider.Provider, error) {
	return nil, nil
}

type fixedTools struct{ specs []model.ToolSpec }

func (f fixedTools) Lookup(string) (tool.Tool, bool) { return nil, false }
func (f fixedTools) Specs(model.NodeName) []model.ToolSpec {
	return append([]model.ToolSpec(nil), f.specs...)
}

type testRuntime struct {
	mu    sync.Mutex
	calls [][]model.ToolCall
}

func (r *testRuntime) ExecuteBatch(ctx context.Context, node model.NodeName, calls []model.ToolCall) ([]model.Message, error) {
	r.mu.Lock()
	r.calls = append(r.calls, calls)
	r.mu.Unlock()
	out := make([]model.Message, 0, len(calls))
	for _, c := range calls {
		out = append(out, model.Message{
			Role:       model.RoleTool,
			ToolCallID: c.ID,
			Content:    `{"ok":true,"data":"file content"}`,
		})
	}
	return out, nil
}

func agentConfig(t *testing.T) config.View {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ant"), 0o700); err != nil {
		t.Fatal(err)
	}
	models := `{"providers":{"p":{"api":"openai-completions","baseUrl":"http://127.0.0.1:1/v1","models":[{"id":"real","name":"alpha","contextWindow":100000,"maxOutputTokens":4096,"supportsTools":true}]}}}`
	settings := `{"defaultModel":"alpha","log":{"level":"error"},"project":{"autoInit":false},"limits":{"maxToolTurns":3}}`
	if err := os.WriteFile(filepath.Join(dir, ".ant", "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ant", "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: dir, HomeDir: filepath.Join(dir, "home"), Env: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBaseNodeToolLoop(t *testing.T) {
	cfg := agentConfig(t)
	fp := fake.New()
	fp.Queue(provider.ChatResponse{
		Message: model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			},
		},
		Usage:        model.Usage{Input: 10, Output: 2},
		FinishReason: "tool_calls",
	})
	fp.Queue(provider.ChatResponse{
		Message: model.Message{
			Role:    model.RoleAssistant,
			Content: "## 变更\n无\n\n## 已执行\n无\n\n## 未解决\n无",
		},
		Usage:        model.Usage{Input: 20, Output: 5},
		FinishReason: "stop",
	})

	var (
		mu     sync.Mutex
		events []model.Event
	)
	sink := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		mu.Lock()
		events = append(events, *ev)
		mu.Unlock()
		return nil
	})
	rt := &testRuntime{}
	reg := NewRegistry(Deps{
		Provider: fp,
		Registry: fixedRegistry{res: provider.Resolved{
			Alias: "alpha", ProviderName: "p", ModelID: "real",
			Capabilities: model.Capabilities{
				ContextWindow: 100000, MaxOutputTokens: 4096,
				SupportsTools: true, SupportsStreaming: false, SupportsParallelToolCalls: true,
			},
		}},
		Prompt:  prompt.NewAssembler(prompt.NewFileSource(cfg)),
		Context: ctxmgr.NewPreparer(cfg, nil, sink),
		Runtime: rt,
		ToolRegistry: fixedTools{specs: []model.ToolSpec{{
			Name: "read", Description: "读取文件", Schema: json.RawMessage(`{"type":"object"}`),
			Timeout: time.Second,
		}}},
		Recorder: sink,
		Loop:     NewLoop(cfg),
		Clock:    func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
		Config:   cfg,
	})
	node, ok := reg.Get(model.NodeCode)
	if !ok {
		t.Fatal("code node missing")
	}
	out, err := node.Run(context.Background(), NodeInput{
		Request: "给 Test 增加边界检查",
		Mode:    model.ModeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolCalls != 1 {
		t.Fatalf("tool calls = %d", out.ToolCalls)
	}
	if !strings.Contains(out.Text, "## 变更") {
		t.Fatalf("text = %q", out.Text)
	}
	if out.Usage.Input != 30 || out.Usage.Output != 7 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	rt.mu.Lock()
	if len(rt.calls) != 1 || rt.calls[0][0].Name != "read" {
		t.Fatalf("runtime calls = %+v", rt.calls)
	}
	rt.mu.Unlock()
	if len(fp.Requests()) != 2 {
		t.Fatalf("provider requests = %d", len(fp.Requests()))
	}
	second := fp.Requests()[1]
	foundTool := false
	for _, m := range second.Messages {
		if m.Role == model.RoleTool && m.ToolCallID == "call_1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatal("second request must include the tool result")
	}

	var types []string
	for _, ev := range events {
		types = append(types, string(ev.Type))
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"node_started", "usage", "message", "step_completed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
}

func TestContracts(t *testing.T) {
	ctx := context.Background()
	verifier := verifierContract{}
	parsed, err := verifier.Parse(ctx, "```json\n{\"pass\":true,\"summary\":\"ok\",\"failures\":[]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Verdict == nil || !parsed.Verdict.Pass {
		t.Fatalf("verdict = %+v", parsed.Verdict)
	}
	if _, err := verifier.Parse(ctx, `{"pass":true,"summary":"ok","failures":[{"severity":"blocker","evidence":"x"}]}`); err == nil {
		t.Fatal("pass with failures must fail")
	}
	if _, err := verifier.Parse(ctx, `{"pass":false,"summary":"bad","failures":[]}`); err == nil {
		t.Fatal("fail without failures must fail")
	}

	deliberate := deliberateContract{}
	parsed, err = deliberate.Parse(ctx, `{"dimension":"risk","issues":[{"severity":"major","file":"x.go","evidence":"race","suggestion":"lock"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Issues) != 1 || parsed.Issues[0].Description != "race" {
		t.Fatalf("issues = %+v", parsed.Issues)
	}

	aggregation := aggregationContract{}
	parsed, err = aggregation.Parse(ctx, `{"plan":"do it","issues":[{"id":"I1","severity":"major","description":"d"}],"missing_dimensions":[],"provenance":{"I1":["risk"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Plan == nil || parsed.Plan.Plan != "do it" {
		t.Fatalf("aggregation = %+v", parsed.Plan)
	}

	text := textContract{node: model.NodeCode, required: []string{"## 变更"}}
	if _, err := text.Parse(ctx, "只有正文"); err == nil {
		t.Fatal("missing section should fail")
	}
}

func TestLoopDetectorThresholds(t *testing.T) {
	loop := NewLoop(nil)
	call := model.ToolCall{ID: "1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}
	call2 := model.ToolCall{ID: "2", Name: "read", Arguments: json.RawMessage(`{ "path" : "a" }`)}
	verdicts := []LoopVerdict{}
	for i := 0; i < 5; i++ {
		c := call
		if i%2 == 1 {
			c = call2
		}
		v, err := loop.Observe(model.NodeCode, c)
		if err != nil {
			t.Fatal(err)
		}
		verdicts = append(verdicts, v)
	}
	if verdicts[2] != LoopWarn {
		t.Fatalf("third = %v, want warn", verdicts[2])
	}
	if verdicts[4] != LoopAbort {
		t.Fatalf("fifth = %v, want abort", verdicts[4])
	}
}
