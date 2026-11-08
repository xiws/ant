package trace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

type fakeReader struct {
	meta   session.Meta
	events []model.Event
	arts   map[string][]byte
}

func (r *fakeReader) Meta() session.Meta { return r.meta }
func (r *fakeReader) Events(ctx context.Context) ([]model.Event, error) {
	return append([]model.Event(nil), r.events...), nil
}
func (r *fakeReader) Artifact(ref model.ArtifactRef) ([]byte, error) {
	if body, ok := r.arts[ref.Name]; ok {
		return body, nil
	}
	return nil, errors.New("missing artifact")
}

func testEvent(seq int64, typ model.EventType, turn int, node model.NodeName, run string, ts time.Time, data any) model.Event {
	return model.Event{Seq: seq, TS: ts, Type: typ, Turn: turn, Node: node, Run: run, Data: model.MustJSON(data)}
}

func traceReader(t *testing.T) *fakeReader {
	t.Helper()
	base := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	nodeInput := map[string]any{
		"messages": []model.Message{
			{Role: model.RoleSystem, Content: "## 节点：ask\n你是 ask\n\n## 模式：ask\n只读"},
			{Role: model.RoleUser, Content: "hi"},
		},
		"tools": []model.ToolSpec{},
		"meta":  map[string]any{"NodePromptVersion": "1"},
	}
	inputBody, _ := json.Marshal(nodeInput)
	outputBody, _ := json.Marshal(map[string]any{
		"text": "final answer", "usage": model.Usage{Input: 10, Output: 5},
		"toolCalls": 1, "changed": []string{"x.txt"},
	})
	r := &fakeReader{
		meta: session.Meta{
			ID: "s1", Workspace: "/w", Title: "demo", Mode: model.ModeAsk,
			Model: "m", Status: session.StatusIdle,
			CreatedAt: base, UpdatedAt: base, Usage: model.Usage{Input: 10, Output: 5},
		},
		arts: map[string][]byte{
			"artifacts/node-input.json":  inputBody,
			"artifacts/tool.txt":         []byte("{\"ok\":true,\"data\":{\"content\":\"hello ``` <script>x</script>\"}}"),
			"artifacts/answer.txt":       []byte("final answer"),
			"artifacts/node-output.json": outputBody,
		},
	}
	r.events = []model.Event{
		testEvent(1, model.EventSessionCreated, 0, "", "", base, map[string]any{"id": "s1"}),
		testEvent(2, model.EventTurnStarted, 1, "", "", base, map[string]any{"input": "hi"}),
		testEvent(3, model.EventNodeStarted, 1, model.NodeAsk, "r1", base, map[string]any{
			"node": "ask", "model": "m", "provider": "p", "promptVersion": "1",
			"promptHash": "sha256:abcdef1234567890", "inputRef": map[string]any{"name": "artifacts/node-input.json"},
		}),
		testEvent(4, model.EventModelRequest, 1, model.NodeAsk, "r1", base, map[string]any{
			"messageCount": 2, "estimatedInputTokens": 10, "tools": []string{"read"}, "stream": true,
		}),
		testEvent(5, model.EventModelResponse, 1, model.NodeAsk, "r1", base, map[string]any{
			"finishReason": "tool_calls", "toolCallCount": 1, "durationMs": 12,
		}),
		testEvent(6, model.EventMessage, 1, model.NodeAsk, "r1", base, map[string]any{
			"role": "assistant", "toolCalls": []model.ToolCall{{
				ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"x.txt"}`),
			}},
		}),
		testEvent(7, model.EventToolCall, 1, model.NodeAsk, "r1", base, map[string]any{
			"id": "c1", "name": "read", "arguments": map[string]any{"path": "x.txt"}, "approval": "auto",
		}),
		testEvent(8, model.EventToolResult, 1, model.NodeAsk, "r1", base, map[string]any{
			"id": "c1", "name": "read", "status": "ok", "ok": true, "durationMs": 3,
		}),
		testEvent(9, model.EventMessage, 1, model.NodeAsk, "r1", base, map[string]any{
			"role": "tool", "toolCallID": "c1", "contentRef": map[string]any{"name": "artifacts/tool.txt"},
		}),
		testEvent(10, model.EventModelRequest, 1, model.NodeAsk, "r1", base, map[string]any{
			"messageCount": 4, "estimatedInputTokens": 20, "tools": []string{"read"},
		}),
		testEvent(11, model.EventModelResponse, 1, model.NodeAsk, "r1", base, map[string]any{
			"finishReason": "stop", "toolCallCount": 0, "durationMs": 20,
		}),
		testEvent(12, model.EventMessage, 1, model.NodeAsk, "r1", base, map[string]any{
			"role": "assistant", "contentRef": map[string]any{"name": "artifacts/answer.txt"},
		}),
		testEvent(13, model.EventStepCompleted, 1, model.NodeAsk, "r1", base, map[string]any{
			"status": "ok", "outputRef": map[string]any{"name": "artifacts/node-output.json"},
			"usage": model.Usage{Input: 10, Output: 5}, "toolCalls": 1,
		}),
		testEvent(14, model.EventNodeCompleted, 1, model.NodeAsk, "", base, map[string]any{
			"status": "ok", "durationMs": 40, "node": "ask",
		}),
		testEvent(15, model.EventTurnCompleted, 1, "", "", base, map[string]any{
			"status": "ok", "exitCode": 0, "usage": model.Usage{Input: 10, Output: 5},
		}),
	}
	return r
}

func TestBuildGroupsRoundsAndToolContent(t *testing.T) {
	doc, err := Build(context.Background(), traceReader(t), Options{AntVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Turns) != 1 || len(doc.Turns[0].Nodes) != 1 {
		t.Fatalf("turns=%d nodes=%d", len(doc.Turns), len(doc.Turns[0].Nodes))
	}
	n := doc.Turns[0].Nodes[0]
	if n.RunID != "r1" || n.Name != model.NodeAsk {
		t.Fatalf("node = %+v", n)
	}
	if len(n.Rounds) != 2 {
		t.Fatalf("rounds = %d", len(n.Rounds))
	}
	if len(n.Rounds[0].Messages) != 2 {
		t.Fatalf("round1 messages = %d", len(n.Rounds[0].Messages))
	}
	if len(n.Rounds[1].Messages) != 4 {
		t.Fatalf("round2 messages = %d", len(n.Rounds[1].Messages))
	}
	if !n.Rounds[1].Reconstructed {
		t.Fatal("round2 should be marked reconstructed")
	}
	if len(n.Rounds[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(n.Rounds[0].ToolCalls))
	}
	tc := n.Rounds[0].ToolCalls[0]
	if tc.Name != "read" || !strings.Contains(tc.Content, "hello") || !strings.Contains(tc.Content, "<script>") {
		t.Fatalf("tool call = %+v", tc)
	}
	if n.Output == nil || n.Output.Text != "final answer" {
		t.Fatalf("output = %+v", n.Output)
	}
	if n.Completed == nil || n.Completed.Status != "ok" {
		t.Fatalf("completed = %+v", n.Completed)
	}
}

func TestRenderMarkdownUsesSafeFence(t *testing.T) {
	doc, err := Build(context.Background(), traceReader(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := string(RenderMarkdown(doc))
	if !strings.Contains(md, "````") {
		t.Fatalf("expected 4-backtick fence for content containing ```:\n%s", md)
	}
	if !strings.Contains(md, "# ant session trace · s1") {
		t.Fatalf("missing title")
	}
}

func TestRenderHTMLEscapes(t *testing.T) {
	doc, err := Build(context.Background(), traceReader(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := string(RenderHTML(doc))
	if !strings.Contains(out, "&lt;script&gt;x&lt;/script&gt;") {
		t.Fatalf("html content not escaped")
	}
	if strings.Contains(out, "<script>x</script>") {
		t.Fatalf("raw script leaked")
	}
}

func TestBuildMissingArtifactWarns(t *testing.T) {
	r := traceReader(t)
	delete(r.arts, "artifacts/node-input.json")
	doc, err := Build(context.Background(), r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) == 0 && len(doc.Turns[0].Nodes[0].Warnings) == 0 {
		t.Fatal("expected warning for missing artifact")
	}
}

func TestBuildOldSessionWithoutRun(t *testing.T) {
	r := traceReader(t)
	for i := range r.events {
		r.events[i].Run = ""
	}
	doc, err := Build(context.Background(), r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Turns) != 1 || len(doc.Turns[0].Nodes) != 1 {
		t.Fatalf("fallback grouping failed: %+v", doc.Turns)
	}
	if len(doc.Warnings) == 0 {
		t.Fatal("expected legacy run warning")
	}
}
