package ctxmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type fakeView struct{ s config.Settings }

func (f fakeView) Settings() config.Settings           { return f.s }
func (f fakeView) Models() []config.ModelSpec          { return nil }
func (f fakeView) Providers() []config.ProviderSpec    { return nil }
func (f fakeView) Origin(string) (config.Origin, bool) { return "", false }
func (f fakeView) Warnings() []string                  { return nil }

func testSettings() config.Settings {
	return config.Settings{
		Context: config.ContextSettings{
			AutoCompact: true, CompactAt: 0.8, KeepRecentTurns: 2,
			SummaryMaxTokens: 4096, SafetyMarginRatio: 0.05,
		},
	}
}

func user(content string) model.Message { return model.Message{Role: model.RoleUser, Content: content} }
func assistant(content string) model.Message {
	return model.Message{Role: model.RoleAssistant, Content: content}
}
func toolMsg(id, content string) model.Message {
	return model.Message{Role: model.RoleTool, ToolCallID: id, Content: content}
}

func TestComputeBudget(t *testing.T) {
	b := ComputeBudget(model.Capabilities{ContextWindow: 100000, MaxOutputTokens: 8000}, 0.05)
	if b.Window != 100000 || b.OutputReserve != 8000 {
		t.Fatalf("budget = %+v", b)
	}
	if b.InputBudget != 100000-8000-5000 {
		t.Fatalf("input budget = %d", b.InputBudget)
	}
	b = ComputeBudget(model.Capabilities{ContextWindow: 1000}, 0)
	if b.OutputReserve != 4096 || b.InputBudget != 0 {
		t.Fatalf("fallback budget = %+v", b)
	}
}

func TestCompactorKeepsToolPairs(t *testing.T) {
	history := []model.Message{
		user("u1"), assistant("a1"),
		user("u2"),
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a"}`)}}},
		toolMsg("c1", `{"ok":true}`),
		user("u3"),
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c2", Name: "read", Arguments: []byte(`{"path":"b"}`)}}},
		toolMsg("c2", `{"ok":true}`),
	}
	out, err := NewCompactor().Compact(context.Background(), CompactInput{
		History: history, KeepRecentTurns: 2, Budget: Budget{InputBudget: 100000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Compacted {
		t.Fatal("expected compaction")
	}
	if len(out.Removed) == 0 {
		t.Fatal("expected removed messages")
	}
	// 保留的 assistant tool_calls 必须有对应 tool 结果。
	for i, m := range out.History {
		if m.Role != model.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			found := false
			for _, later := range out.History[i+1:] {
				if later.Role == model.RoleTool && later.ToolCallID == tc.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("tool call %s lost its result", tc.ID)
			}
		}
	}
	// 首个用户请求必须保留（§11.3）。
	if out.History[0].Role != model.RoleUser || out.History[0].Content != "u1" {
		t.Fatalf("first user not preserved: %+v", out.History[0])
	}
}

func TestPreparerPreservesPinnedAndEmitsCompaction(t *testing.T) {
	var events []model.Event
	sink := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		events = append(events, *ev)
		return nil
	})
	cfg := fakeView{s: testSettings()}
	p := NewPreparer(cfg, nil, sink)
	pinned := []model.Message{user("当前请求"), {Role: model.RoleSystem, Content: "plan"}}
	var history []model.Message
	for i := 0; i < 10; i++ {
		history = append(history, user("旧问题"), assistant(strings.Repeat("内容", 200)))
	}
	prepared, err := p.Prepare(context.Background(), PrepareInput{
		Node:    model.NodeCode,
		Cap:     model.Capabilities{ContextWindow: 4000, MaxOutputTokens: 500},
		Pinned:  pinned,
		History: history,
		Usage:   model.Usage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range pinned {
		found := false
		for _, m := range prepared.Messages {
			if m.Role == want.Role && m.Content == want.Content {
				found = true
			}
		}
		if !found {
			t.Fatalf("pinned message lost: %+v", want)
		}
	}
	if !prepared.Compacted {
		t.Fatal("expected compaction for oversized history")
	}
	if !strings.Contains(prepared.Summary, "goal") {
		t.Fatalf("deterministic summary missing: %q", prepared.Summary)
	}
	if len(events) != 1 || events[0].Type != model.EventContextCompacted {
		t.Fatalf("events = %v", events)
	}
}

func TestPreparerOverflowWhenCompactionDisabled(t *testing.T) {
	s := testSettings()
	s.Context.AutoCompact = false
	p := NewPreparer(fakeView{s: s}, nil, nil)
	_, err := p.Prepare(context.Background(), PrepareInput{
		Node:    model.NodeCode,
		Cap:     model.Capabilities{ContextWindow: 100, MaxOutputTokens: 10},
		History: []model.Message{user(strings.Repeat("x", 5000))},
	})
	if err == nil {
		t.Fatal("expected context overflow")
	}
	if model.ClassOf(err) != model.ErrContextOverflow {
		t.Fatalf("class = %q", model.ClassOf(err))
	}
}

func TestCostCalculator(t *testing.T) {
	calc := NewCostCalculator()
	amount, known := calc.Cost(nil, model.Usage{Input: 1000})
	if known || amount != 0 {
		t.Fatalf("nil pricing = %f/%v", amount, known)
	}
	p := &model.Pricing{InputPer1M: 1, OutputPer1M: 2, CachedInputPer1M: 0.5}
	amount, known = calc.Cost(p, model.Usage{Input: 1_000_000, Cached: 200_000, Output: 500_000})
	if !known {
		t.Fatal("expected known cost")
	}
	want := 800_000.0/1e6*1 + 200_000.0/1e6*0.5 + 500_000.0/1e6*2
	if amount < want-1e-9 || amount > want+1e-9 {
		t.Fatalf("amount = %f, want %f", amount, want)
	}
}
