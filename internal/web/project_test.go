package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

const projectTestID = "20261106-234456-df479f"

func writeWebSession(t *testing.T, work, id string, meta session.Meta, events []model.Event, artifacts map[string]string) string {
	t.Helper()
	dir := filepath.Join(work, ".ant", "sessions", id)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(events) > 0 {
		var lines []string
		for _, ev := range events {
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(raw))
		}
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range artifacts {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ev(seq int64, ts time.Time, typ model.EventType, turn int, node model.NodeName, data map[string]any) model.Event {
	return model.Event{Seq: seq, TS: ts.UTC(), Type: typ, Turn: turn, Node: node, Data: model.MustJSON(data)}
}

func projectorFixture(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	base := time.Date(2026, 11, 6, 23, 44, 56, 0, time.UTC)
	step := func(n int) time.Time { return base.Add(time.Duration(n) * time.Second) }
	id := projectTestID
	meta := session.Meta{
		SchemaVersion: 1,
		ID:            id,
		Workspace:     work,
		CreatedAt:     base,
		UpdatedAt:     step(120),
		Status:        session.StatusIdle,
		Mode:          model.ModeCode,
		Title:         "projector fixture",
		Model:         "alpha",
		TurnCount:     2,
		Usage:         model.Usage{Input: 100, Output: 50},
	}
	dir := filepath.Join(work, ".ant", "sessions", id)

	diffText := "@@ -1,3 +1,4 @@\n a\n-b\n+c\n+d\n"
	toolBody := func(data map[string]any) string {
		raw, _ := json.Marshal(map[string]any{"ok": true, "data": data})
		return string(raw)
	}
	events := []model.Event{
		ev(1, step(0), model.EventSessionCreated, 0, "", map[string]any{"id": id, "mode": "code", "model": "alpha"}),
		ev(2, step(1), model.EventTurnStarted, 1, "", map[string]any{"input": "fix the bug", "mode": "code", "round": 0}),
		ev(3, step(2), model.EventNodeStarted, 1, model.NodeCode, map[string]any{
			"node": "code", "model": "alpha", "provider": "p", "step": "code",
			"inputRef": map[string]any{"name": "artifacts/node-input-1.json", "bytes": 12},
		}),
		ev(4, step(3), model.EventModelRequest, 1, model.NodeCode, map[string]any{
			"attempt": 1, "messageCount": 3, "estimatedInputTokens": 100, "tools": []string{"read"}, "stream": true,
		}),
		ev(5, step(4), model.EventModelResponse, 1, model.NodeCode, map[string]any{
			"attempt": 1, "durationMs": 1000, "finishReason": "tool_calls",
			"usage": map[string]any{"input": 12, "output": 3}, "toolCallCount": 1, "model": "alpha",
		}),
		ev(6, step(5), model.EventMessage, 1, model.NodeCode, map[string]any{
			"role": "assistant", "contentRef": map[string]any{"name": "artifacts/message-1.txt", "bytes": 30},
			"toolCalls": []any{map[string]any{"id": "call-read", "name": "read"}},
		}),
		ev(7, step(6), model.EventToolCall, 1, model.NodeCode, map[string]any{
			"id": "call-read", "name": "read", "arguments": map[string]any{"path": "README.md"}, "approval": "auto",
		}),
		ev(8, step(7), model.EventMessage, 1, model.NodeCode, map[string]any{
			"role": "tool", "toolCallID": "call-read",
			"contentRef": map[string]any{"name": "artifacts/tool-read.json", "bytes": 100},
		}),
		ev(9, step(8), model.EventToolResult, 1, model.NodeCode, map[string]any{
			"id": "call-read", "status": "ok", "ok": true, "exitCode": 0, "durationMs": 0,
		}),
		ev(10, step(9), model.EventModelRequest, 1, model.NodeCode, map[string]any{
			"attempt": 1, "messageCount": 5, "estimatedInputTokens": 150, "tools": []string{"read", "edit"}, "stream": true,
		}),
		ev(11, step(10), model.EventModelResponse, 1, model.NodeCode, map[string]any{
			"attempt": 1, "durationMs": 2000, "finishReason": "tool_calls",
			"usage": map[string]any{"input": 20, "output": 5}, "toolCallCount": 2, "model": "alpha",
		}),
		ev(12, step(11), model.EventApprovalRequested, 1, model.NodeCode, map[string]any{
			"class": "bash_risk", "action": "bash", "command": "git push", "reason": "network",
		}),
		ev(13, step(12), model.EventApprovalResolved, 1, model.NodeCode, map[string]any{
			"class": "bash_risk", "decision": "allow", "scope": "session", "source": "user",
		}),
		ev(14, step(13), model.EventToolCall, 1, model.NodeCode, map[string]any{
			"id": "call-edit", "name": "edit",
			"arguments": map[string]any{"path": "internal/x.go", "diff": diffText}, "approval": "required",
		}),
		ev(15, step(14), model.EventMessage, 1, model.NodeCode, map[string]any{
			"role": "tool", "toolCallID": "call-edit",
			"contentRef": map[string]any{"name": "artifacts/tool-edit.json", "bytes": 100},
		}),
		ev(16, step(15), model.EventToolResult, 1, model.NodeCode, map[string]any{
			"id": "call-edit", "status": "ok", "ok": true, "exitCode": 0, "durationMs": 31,
		}),
		ev(17, step(16), model.EventToolCall, 1, model.NodeCode, map[string]any{
			"id": "call-write", "name": "write",
			"arguments": map[string]any{"path": "docs/new.md", "content": "line1\nline2\nline3"},
			"approval":  "auto",
		}),
		ev(18, step(17), model.EventMessage, 1, model.NodeCode, map[string]any{
			"role": "tool", "toolCallID": "call-write",
			"contentRef": map[string]any{"name": "artifacts/tool-write.json", "bytes": 100},
		}),
		ev(19, step(18), model.EventToolResult, 1, model.NodeCode, map[string]any{
			"id": "call-write", "status": "ok", "ok": true, "created": true, "durationMs": 2,
		}),
		ev(20, step(19), model.EventContextCompacted, 1, "", map[string]any{
			"beforeTokens": 4000, "afterTokens": 1000,
			"summaryRef":    map[string]any{"name": "artifacts/summary-1.txt", "bytes": 10},
			"replacedRange": map[string]any{"removed": 4, "kept": 1},
		}),
		ev(21, step(20), model.EventMessage, 1, model.NodeCode, map[string]any{
			"role": "assistant", "contentRef": map[string]any{"name": "artifacts/message-final.txt", "bytes": 20},
		}),
		ev(22, step(21), model.EventStepCompleted, 1, model.NodeCode, map[string]any{
			"status": "ok", "changed": []string{"internal/x.go"},
		}),
		ev(23, step(22), model.EventNodeCompleted, 1, model.NodeCode, map[string]any{
			"node": "code", "status": "ok", "durationMs": 19000, "usage": map[string]any{"input": 32, "output": 8},
		}),
		ev(24, step(23), model.EventTurnCompleted, 1, "", map[string]any{
			"status": "ok", "exitCode": 0, "usage": map[string]any{"input": 32, "output": 8},
			"changed": []string{"internal/x.go", "docs/new.md"},
		}),
		ev(25, step(30), model.EventTurnStarted, 2, "", map[string]any{"input": "broken task", "mode": "code", "round": 0}),
		ev(26, step(31), model.EventTurnCompleted, 2, "", map[string]any{
			"status": "failed", "exitCode": 1, "usage": map[string]any{"input": 5, "output": 1},
		}),
		{Seq: 27, TS: step(32), Type: model.EventType("future_event"), Data: json.RawMessage(`{"foo":"bar"}`)},
		ev(28, step(33), model.EventMessage, 2, "", map[string]any{
			"role": "assistant", "contentRef": map[string]any{"name": "artifacts/missing.txt", "bytes": 10},
		}),
	}
	artifacts := map[string]string{
		"artifacts/node-input-1.json": `{"messages":[]}`,
		"artifacts/message-1.txt":     "assistant body",
		"artifacts/message-final.txt": "final answer",
		"artifacts/summary-1.txt":     "compacted summary",
		"artifacts/plan.md":           "# Plan\n\n- step 1",
		"artifacts/tool-read.json":    toolBody(map[string]any{"path": "README.md", "content": "1\t# ant", "startLine": 1, "endLine": 1, "totalLines": 1}),
		"artifacts/tool-edit.json":    toolBody(map[string]any{"path": "internal/x.go", "hunks": 1, "applied": 1, "added": 2, "removed": 1, "bytes": 10}),
		"artifacts/tool-write.json":   toolBody(map[string]any{"path": "docs/new.md", "bytes": 18, "created": true}),
	}
	writeWebSession(t, work, id, meta, events, artifacts)
	return work, dir
}

func newTestProjector(work string) *Projector {
	return NewProjector(session.NewArchive(work), nil, work, filepath.Join(work, "home"), time.Now)
}

func TestProjectorView(t *testing.T) {
	work, _ := projectorFixture(t)
	p := newTestProjector(work)
	view, version, err := p.View(context.Background(), projectTestID, ViewOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("version should not be empty")
	}
	if len(view.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(view.Turns))
	}
	turn1 := view.Turns[0]
	if turn1.Turn != 1 || turn1.Status != "ok" || turn1.Input != "fix the bug" {
		t.Fatalf("turn1 = %+v", turn1)
	}
	if turn1.DurationMs <= 0 {
		t.Fatalf("turn1 duration = %d", turn1.DurationMs)
	}
	kinds := make([]string, 0, len(turn1.Items))
	for _, item := range turn1.Items {
		kinds = append(kinds, item.Kind)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"user", "assistant", "tool", "approval", "compaction"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("turn1 items %v missing %s", kinds, want)
		}
	}
	if len(turn1.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(turn1.Requests))
	}
	if turn1.Requests[0].Status != "ok" || turn1.Requests[0].DurationMs != 1000 {
		t.Fatalf("request0 = %+v", turn1.Requests[0])
	}
	if turn1.Requests[1].ToolsUsed == nil || !strings.Contains(strings.Join(turn1.Requests[1].ToolsUsed, ","), "edit") {
		t.Fatalf("request1 toolsUsed = %v", turn1.Requests[1].ToolsUsed)
	}

	// 工具卡：read / edit / write。
	tools := map[string]*ToolView{}
	for _, item := range turn1.Items {
		if item.Tool != nil {
			tools[item.Tool.Name] = item.Tool
		}
	}
	read := tools["read"]
	if read == nil || read.Status != "ok" || read.Read == nil || !strings.Contains(read.Read.Content, "# ant") {
		t.Fatalf("read tool = %+v", read)
	}
	edit := tools["edit"]
	if edit == nil || edit.Diff == nil {
		t.Fatalf("edit tool = %+v", edit)
	}
	if edit.Diff.Added != 2 || edit.Diff.Removed != 1 || edit.Diff.HunkCount != 1 {
		t.Fatalf("edit diff = +%d -%d hunks=%d", edit.Diff.Added, edit.Diff.Removed, edit.Diff.HunkCount)
	}
	write := tools["write"]
	if write == nil || write.Write == nil || !write.Write.Created || write.Write.Path != "docs/new.md" {
		t.Fatalf("write tool = %+v", write)
	}
	if !strings.Contains(write.Write.Content, "line1") {
		t.Fatalf("write content = %q", write.Write.Content)
	}

	// 审批合成。
	if len(view.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(view.Approvals))
	}
	if view.Approvals[0].Decision != "allow" || view.Approvals[0].Scope != "session" {
		t.Fatalf("approval = %+v", view.Approvals[0])
	}

	// 压缩摘要。
	var compaction *CompactionView
	for _, item := range turn1.Items {
		if item.Compaction != nil {
			compaction = item.Compaction
		}
	}
	if compaction == nil || compaction.BeforeTokens != 4000 || compaction.AfterTokens != 1000 {
		t.Fatalf("compaction = %+v", compaction)
	}
	if !strings.Contains(compaction.Summary, "compacted summary") {
		t.Fatalf("compaction summary = %q", compaction.Summary)
	}

	// 改动：turn_completed + edit + write 合并。
	changes := map[string]ChangedFile{}
	for _, change := range view.Changes {
		changes[change.Path] = change
	}
	if change, ok := changes["internal/x.go"]; !ok || change.Added != 2 || change.Removed != 1 {
		t.Fatalf("change internal/x.go = %+v (all %+v)", change, view.Changes)
	}
	if change, ok := changes["docs/new.md"]; !ok || change.Status != "created" {
		t.Fatalf("change docs/new.md = %+v", change)
	}

	// plan 与 artifacts。
	if view.Plan == nil || !view.Plan.Found || !strings.Contains(view.Plan.Body, "# Plan") {
		t.Fatalf("plan = %+v", view.Plan)
	}
	if len(view.Artifacts) == 0 {
		t.Fatal("artifacts should be listed")
	}
	if view.Readonly != true {
		t.Fatal("view must be readonly")
	}

	// 轨迹：请求、工具、本轮行都存在。
	var requestRows, toolRows, turnRows int
	for _, row := range view.Trajectory {
		switch row.Kind {
		case "request":
			requestRows++
		case "tool":
			toolRows++
		case "turn":
			turnRows++
		}
	}
	if requestRows != 2 || toolRows != 3 {
		t.Fatalf("trajectory requests=%d tools=%d (rows %d)", requestRows, toolRows, len(view.Trajectory))
	}
	if turnRows != 2 {
		t.Fatalf("trajectory turn rows = %d, want 2", turnRows)
	}

	// 未知事件进入 warning，不中断渲染。
	if !strings.Contains(strings.Join(view.Warnings, "\n"), "unknown") &&
		!strings.Contains(strings.Join(view.Warnings, "\n"), "未知事件") {
		t.Fatalf("warnings = %v", view.Warnings)
	}

	// 缺失 artifact：turn 2 的 assistant 消息降级为 warning/占位。
	if len(view.Turns) < 2 || view.Turns[1].Status != "failed" {
		t.Fatalf("turn2 = %+v", view.Turns[len(view.Turns)-1])
	}
}

func TestProjectorPagination(t *testing.T) {
	work, _ := projectorFixture(t)
	p := newTestProjector(work)
	view, _, err := p.View(context.Background(), projectTestID, ViewOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Turns) != 1 || view.Turns[0].Turn != 2 {
		t.Fatalf("limited turns = %+v", view.Turns)
	}
	if !view.HasMore || !view.Truncated {
		t.Fatalf("hasMore=%v truncated=%v, want true", view.HasMore, view.Truncated)
	}
	if view.FirstTurn != 2 {
		t.Fatalf("firstTurn = %d, want 2", view.FirstTurn)
	}

	earlier, _, err := p.View(context.Background(), projectTestID, ViewOptions{BeforeTurn: 2, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(earlier.Turns) != 1 || earlier.Turns[0].Turn != 1 {
		t.Fatalf("beforeTurn turns = %+v", earlier.Turns)
	}
	if earlier.HasMore {
		t.Fatal("beforeTurn=2 should not claim more turns")
	}
}

func TestProjectorMissingArtifact(t *testing.T) {
	work, _ := projectorFixture(t)
	p := newTestProjector(work)
	view, _, err := p.View(context.Background(), projectTestID, ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var missing bool
	for _, item := range view.Turns[len(view.Turns)-1].Items {
		if item.Kind == "assistant" && item.RefMissing {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("missing artifact should set RefMissing; items = %+v", view.Turns[len(view.Turns)-1].Items)
	}
}

func TestProjectorListFilters(t *testing.T) {
	work, _ := projectorFixture(t)
	p := newTestProjector(work)
	all, err := p.List(context.Background(), session.ListFilter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("list = %d", len(all))
	}
	if all[0].Title != "projector fixture" || all[0].Live {
		t.Fatalf("summary = %+v", all[0])
	}
	none, err := p.List(context.Background(), session.ListFilter{}, "no-such-title")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("query filter = %d", len(none))
	}
	mode, err := p.List(context.Background(), session.ListFilter{Mode: model.ModeAsk}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mode) != 0 {
		t.Fatalf("mode filter = %d", len(mode))
	}
}

func TestProjectorRepeatedTurnNumbers(t *testing.T) {
	work := t.TempDir()
	base := time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)
	step := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
	id := "20261107-070000-abcdef"
	meta := session.Meta{
		SchemaVersion: 1, ID: id, Workspace: work,
		CreatedAt: step(0), UpdatedAt: step(40), Status: session.StatusIdle,
		Mode: model.ModeAsk, TurnCount: 3,
	}
	// 真实日志里出现过 1,1,2,3,1 的 turn 编号：相同编号的已结束轮次必须被拆开。
	events := []model.Event{
		ev(1, step(0), model.EventTurnStarted, 1, "", map[string]any{"input": "first", "mode": "ask"}),
		ev(2, step(1), model.EventTurnCompleted, 1, "", map[string]any{"status": "ok", "exitCode": 0}),
		ev(3, step(2), model.EventTurnStarted, 1, "", map[string]any{"input": "second", "mode": "ask"}),
		ev(4, step(3), model.EventTurnCompleted, 1, "", map[string]any{"status": "ok", "exitCode": 0}),
		ev(5, step(4), model.EventTurnStarted, 2, "", map[string]any{"input": "third", "mode": "ask"}),
		ev(6, step(5), model.EventTurnCompleted, 2, "", map[string]any{"status": "ok", "exitCode": 0}),
		ev(7, step(6), model.EventTurnStarted, 1, "", map[string]any{"input": "fourth", "mode": "ask"}),
		ev(8, step(7), model.EventTurnCompleted, 1, "", map[string]any{"status": "failed", "exitCode": 1}),
	}
	writeWebSession(t, work, id, meta, events, nil)
	view, _, err := newTestProjector(work).View(context.Background(), id, ViewOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Turns) != 4 {
		t.Fatalf("turns = %d, want 4 (%+v)", len(view.Turns), view.Turns)
	}
	want := []string{"first", "second", "third", "fourth"}
	for i, input := range want {
		if view.Turns[i].Input != input {
			t.Fatalf("turn %d input = %q, want %q", i+1, view.Turns[i].Input, input)
		}
		if view.Turns[i].Turn != i+1 {
			t.Fatalf("turn numbering = %d, want %d", view.Turns[i].Turn, i+1)
		}
	}
	if view.Turns[1].EventTurn != 1 || view.Turns[0].EventTurn != 1 {
		t.Fatalf("event turns = %d/%d", view.Turns[0].EventTurn, view.Turns[1].EventTurn)
	}
	// 轨迹按事件顺序，而不是按重编号后的 turn 排序。
	var userInputs []string
	for _, row := range view.Trajectory {
		if row.Kind == "user" {
			userInputs = append(userInputs, row.Input)
		}
	}
	if strings.Join(userInputs, ",") != strings.Join(want, ",") {
		t.Fatalf("trajectory user order = %v", userInputs)
	}
	if !strings.Contains(strings.Join(view.Warnings, "\n"), "复用了已结束的第 1 轮") {
		t.Fatalf("warnings = %v", view.Warnings)
	}
}
