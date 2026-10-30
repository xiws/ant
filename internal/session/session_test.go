package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
)

func mustOpen(t *testing.T, store Store, opts OpenOptions) (Session, Opened) {
	t.Helper()
	sess, opened, err := store.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open(%+v): %v", opts, err)
	}
	return sess, opened
}

func mustEmit(t *testing.T, sess Session, ev model.Event) {
	t.Helper()
	if err := sess.Emit(context.Background(), &ev); err != nil {
		t.Fatalf("Emit(%s): %v", ev.Type, err)
	}
}

func emitData(t *testing.T, sess Session, typ model.EventType, turn int, step string, node model.NodeName, data any) {
	t.Helper()
	mustEmit(t, sess, model.Event{
		Type: typ,
		Turn: turn,
		Step: step,
		Node: node,
		Data: model.MustJSON(data),
	})
}

func testSessionDir(workDir, id string) string {
	return filepath.Join(workDir, ".ant", "sessions", id)
}

func TestOpenNewAndResume(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 30, 9, 47, 4, 123_000_000, time.UTC)
	clock := func() time.Time { return now }
	store := NewStoreWithClock(dir, clock)

	sess, opened := mustOpen(t, store, OpenOptions{
		Create: true, Mode: model.ModeCode, Model: "deepseek-v4-flash",
		Workspace: dir, Lock: true,
	})
	if !opened.Created || opened.Resumed {
		t.Fatalf("Opened = %+v, want Created", opened)
	}
	if !strings.HasPrefix(opened.ID, "20261030-094704-") || len(opened.ID) != len("20261030-094704-abcdef") {
		t.Fatalf("unexpected ID %q", opened.ID)
	}
	meta := sess.Meta()
	if meta.ID != opened.ID || meta.SchemaVersion != 1 || meta.Mode != model.ModeCode {
		t.Fatalf("meta = %+v", meta)
	}
	if !meta.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", meta.CreatedAt, now)
	}
	metaPath := filepath.Join(testSessionDir(dir, opened.ID), "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
	// Open 不写事件；cli 才会写 session_created。
	if _, err := os.Stat(filepath.Join(testSessionDir(dir, opened.ID), "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("events.jsonl should not exist before first Emit, err=%v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(testSessionDir(dir, opened.ID))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("session dir perm = %o, want 700", got)
		}
		info, err = os.Stat(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("meta perm = %o, want 600", got)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// 续跑：唯一前缀匹配，保留已有 meta 字段。
	resumed, reopened := mustOpen(t, store, OpenOptions{
		ID: opened.ID[:8], Mode: model.ModeAsk, Lock: true, Workspace: dir,
	})
	if reopened.Created || !reopened.Resumed || reopened.ID != opened.ID {
		t.Fatalf("reopened = %+v", reopened)
	}
	if got := resumed.Meta().Mode; got != model.ModeCode {
		t.Fatalf("resume should keep existing mode, got %q", got)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Open(context.Background(), OpenOptions{ID: "no-such-session"}); err == nil {
		t.Fatal("expected session_not_found")
	} else if model.CodeOf(err) != "session_not_found" {
		t.Fatalf("code = %q, want session_not_found (%v)", model.CodeOf(err), err)
	}
}

func TestEmitSeqMonotonicAndConcurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewStoreWithClock(dir, func() time.Time { return now })
	sess, opened := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})

	for i := 0; i < 3; i++ {
		mustEmit(t, sess, model.Event{Type: model.EventUsage, Data: model.MustJSON(model.Usage{Input: 1})})
	}
	events, err := sess.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("events[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
		if !ev.TS.Equal(now) {
			t.Fatalf("events[%d].TS = %v, want %v", i, ev.TS, now)
		}
	}
	// 未知事件类型仍然写入。
	mustEmit(t, sess, model.Event{Type: model.EventType("future_event"), Data: model.MustJSON(map[string]int{"n": 1})})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	meta, _, err := readMetaFile(filepath.Join(testSessionDir(dir, opened.ID), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastSeq != 4 {
		t.Fatalf("meta.LastSeq = %d, want 4", meta.LastSeq)
	}

	// 续跑后 seq 从最大 seq 继续。
	resumed, _ := mustOpen(t, store, OpenOptions{ID: opened.ID, Lock: true})
	mustEmit(t, resumed, model.Event{Type: model.EventUsage})
	events, err = resumed.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[4].Seq != 5 {
		t.Fatalf("resumed events = %+v", events)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	// 并发 Emit：seq 唯一且连续。
	dir2 := t.TempDir()
	store2 := NewStore(dir2)
	sess2, _ := mustOpen(t, store2, OpenOptions{Create: true, Mode: model.ModeAsk, Lock: true})
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- sess2.Emit(context.Background(), &model.Event{Type: model.EventUsage, Data: model.MustJSON(map[string]int{"i": i})})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Emit: %v", err)
		}
	}
	events, err = sess2.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != n {
		t.Fatalf("len(events) = %d, want %d", len(events), n)
	}
	seen := make(map[int64]bool, n)
	for _, ev := range events {
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
	if sess2.Meta().LastSeq != n {
		t.Fatalf("LastSeq = %d, want %d", sess2.Meta().LastSeq, n)
	}
	if err := sess2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactWriteReadAndEscape(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, opened := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})
	defer sess.Close()

	body := []byte("line1\nline2")
	ref, err := sess.PutArtifact(context.Background(), "notes/readme.txt", body, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if ref.SessionID != opened.ID || ref.Name != "artifacts/notes/readme.txt" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Bytes != int64(len(body)) || ref.SHA256 != hashOf(body) {
		t.Fatalf("ref metadata = %+v", ref)
	}
	got, err := sess.Artifact(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("Artifact = %q, want %q", got, body)
	}
	// 也接受 session 目录相对的原始名称。
	got, err = sess.Artifact(model.ArtifactRef{Name: "artifacts/notes/readme.txt"})
	if err != nil || string(got) != string(body) {
		t.Fatalf("Artifact by name = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(testSessionDir(dir, opened.ID), filepath.FromSlash(ref.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact perm = %o, want 600", info.Mode().Perm())
		}
	}

	if _, err := sess.Artifact(model.ArtifactRef{Name: "artifacts/missing.txt"}); err == nil {
		t.Fatal("expected missing artifact error")
	} else if model.CodeOf(err) != "artifact_not_found" {
		t.Fatalf("code = %q, want artifact_not_found", model.CodeOf(err))
	}
	for _, bad := range []string{"../escape.txt", "artifacts/../../escape.txt", "/tmp/escape.txt"} {
		if _, err := sess.PutArtifact(context.Background(), bad, []byte("x"), "text/plain"); err == nil {
			t.Fatalf("PutArtifact(%q) should fail", bad)
		}
	}
}

func TestListLatestShow(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	store := NewStoreWithClock(dir, clock)

	s1, op1 := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})
	emitData(t, s1, model.EventSessionCreated, 0, "", "", map[string]any{"id": op1.ID, "mode": "code"})
	emitData(t, s1, model.EventTurnStarted, 1, "", "", map[string]string{"input": "add tests"})
	ref, err := s1.PutArtifact(context.Background(), "answer.txt", []byte("done"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	emitData(t, s1, model.EventMessage, 1, "code.code", model.NodeCode, map[string]any{
		"role": "assistant", "contentRef": ref,
	})
	emitData(t, s1, model.EventApprovalResolved, 1, "", model.NodeCode, map[string]string{
		"class": "bash_risk", "decision": "allow", "scope": "session",
	})
	emitData(t, s1, model.EventApprovalResolved, 1, "", model.NodeCode, map[string]string{
		"class": "bash_network", "decision": "deny", "scope": "once",
	})
	if err := s1.UpdateMeta(context.Background(), func(m *Meta) error {
		m.Title = "add tests"
		m.Status = StatusIdle
		m.Usage = model.Usage{Input: 11, Output: 7}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(testSessionDir(dir, op1.ID), "plan.md")
	if err := os.WriteFile(planPath, []byte("# plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	now.Store(base.Add(time.Hour).UnixNano())
	s2, op2 := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeAsk, Lock: true})
	emitData(t, s2, model.EventTurnStarted, 1, "", "", map[string]string{"input": "question"})
	if err := s2.UpdateMeta(context.Background(), func(m *Meta) error {
		m.Status = StatusFailed
		m.Usage = model.Usage{Input: 3, Output: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	if list[0].ID != op2.ID || list[1].ID != op1.ID {
		t.Fatalf("List order = %q,%q, want %q,%q", list[0].ID, list[1].ID, op2.ID, op1.ID)
	}
	if list[0].Tokens != 5 || list[1].Tokens != 18 {
		t.Fatalf("tokens = %d,%d", list[0].Tokens, list[1].Tokens)
	}
	if list[0].LastEvent != model.EventTurnStarted {
		t.Fatalf("LastEvent = %q", list[0].LastEvent)
	}
	if got, err := store.List(context.Background(), ListFilter{Mode: model.ModeCode}); err != nil || len(got) != 1 || got[0].ID != op1.ID {
		t.Fatalf("mode filter = %+v, %v", got, err)
	}
	if got, err := store.List(context.Background(), ListFilter{Status: StatusFailed}); err != nil || len(got) != 1 || got[0].ID != op2.ID {
		t.Fatalf("status filter = %+v, %v", got, err)
	}
	if got, err := store.List(context.Background(), ListFilter{Limit: 1}); err != nil || len(got) != 1 || got[0].ID != op2.ID {
		t.Fatalf("limit = %+v, %v", got, err)
	}
	latest, err := store.Latest(context.Background())
	if err != nil || latest.ID != op2.ID {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}

	detail, err := store.Show(context.Background(), op1.ID[:15])
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title != "add tests" || detail.Tokens != 18 {
		t.Fatalf("detail summary = %+v", detail.Summary)
	}
	if len(detail.Approvals) != 2 {
		t.Fatalf("approvals = %+v", detail.Approvals)
	}
	if detail.Approvals[0].Class != model.ApprovalBashRisk || detail.Approvals[0].Decision != "allow" ||
		detail.Approvals[0].Scope != "session" {
		t.Fatalf("approval[0] = %+v", detail.Approvals[0])
	}
	if detail.PlanPath != planPath {
		t.Fatalf("PlanPath = %q, want %q", detail.PlanPath, planPath)
	}
	found := false
	for _, art := range detail.Artifacts {
		if art.Name == "artifacts/answer.txt" && art.Bytes == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifacts = %+v", detail.Artifacts)
	}

	if _, err := store.Show(context.Background(), "20"); err == nil || model.CodeOf(err) != "session_ambiguous" {
		t.Fatalf("ambiguous Show err = %v, code=%q", err, model.CodeOf(err))
	}

	emptyStore := NewStore(t.TempDir())
	if _, err := emptyStore.Latest(context.Background()); err == nil || model.CodeOf(err) != "no_sessions" {
		t.Fatalf("Latest empty err = %v, code=%q", err, model.CodeOf(err))
	}
}

func TestLockConflictAndStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	var offset atomic.Int64
	clock := func() time.Time { return base.Add(time.Duration(offset.Load()) * time.Second) }

	store1 := NewStoreWithClock(dir, clock)
	sess1, opened := mustOpen(t, store1, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})
	defer sess1.Close()

	store2 := NewStoreWithClock(dir, clock)
	_, _, err := store2.Open(context.Background(), OpenOptions{ID: opened.ID, Lock: true})
	if err == nil || model.CodeOf(err) != "session_locked" {
		t.Fatalf("second open err = %v, code=%q", err, model.CodeOf(err))
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("lock error should mention PID %d: %v", os.Getpid(), err)
	}

	offset.Add(121)
	store3 := NewStoreWithClock(dir, clock)
	sess3, opened3 := mustOpen(t, store3, OpenOptions{ID: opened.ID, Lock: true})
	if !opened3.Resumed {
		t.Fatalf("stale takeover Opened = %+v", opened3)
	}
	state, err := NewRecoverer().Recover(context.Background(), sess3, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(state.Warnings, "\n")
	if !strings.Contains(joined, "stale") || !strings.Contains(joined, "接管") {
		t.Fatalf("warnings = %v", state.Warnings)
	}
	if err := sess3.Close(); err != nil {
		t.Fatal(err)
	}

	// 直接验证 Lease.Stale 与「Release 仅删除自己仍持有的锁」。
	dir2 := t.TempDir()
	offset.Store(0)
	store4 := NewStoreWithClock(dir2, clock)
	sess4, opened4 := mustOpen(t, store4, OpenOptions{Create: true, Mode: model.ModeAsk, Lock: false})
	defer sess4.Close()
	locker := store4.(Locker)
	lease1, err := locker.Acquire(context.Background(), opened4.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease1.Stale() {
		t.Fatal("fresh lease should not be stale")
	}
	offset.Add(121)
	if !lease1.Stale() {
		t.Fatal("lease should be stale after 121s")
	}
	lease2, err := locker.Acquire(context.Background(), opened4.ID)
	if err != nil {
		t.Fatalf("stale takeover: %v", err)
	}
	if err := lease1.Release(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(testSessionDir(dir2, opened4.ID), "lock")
	if _, exists, err := readLockOwner(lockPath); err != nil || !exists {
		t.Fatalf("lease1.Release must not remove lease2's lock (exists=%v err=%v)", exists, err)
	}
	if _, err := locker.Acquire(context.Background(), opened4.ID); err == nil || model.CodeOf(err) != "session_locked" {
		t.Fatalf("third acquire err = %v, code=%q", err, model.CodeOf(err))
	}
	if err := lease2.Release(); err != nil {
		t.Fatal(err)
	}
	if _, exists, _ := readLockOwner(lockPath); exists {
		t.Fatal("lease2.Release should remove lock")
	}
}

func TestRecoverRebuildsStateAndToolPairing(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, opened := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModePlan, Model: "m", Lock: true})
	ctx := context.Background()

	sumRef, err := sess.PutArtifact(ctx, "context/summary.txt", []byte(`{"summary":"earlier work compressed","turns":2}`), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	assistantRef, err := sess.PutArtifact(ctx, "answer.txt", []byte("I'll inspect"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	toolRef, err := sess.PutArtifact(ctx, "tool.out", []byte("file contents"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	emitData(t, sess, model.EventSessionCreated, 0, "", "", map[string]any{
		"id": opened.ID, "workspace": dir, "mode": "plan", "model": "m",
	})
	emitData(t, sess, model.EventTurnStarted, 1, "", "", map[string]string{"input": "do it"})
	// turn_started 与 message(user) 同时存在时不重复。
	emitData(t, sess, model.EventMessage, 1, "", "", map[string]string{"role": "user", "content": "do it"})
	emitData(t, sess, model.EventNodeStarted, 1, "plan.round-1.verifier", model.NodeVerifier,
		map[string]string{"model": "m", "promptVersion": "1"})
	emitData(t, sess, model.EventModelRequest, 1, "plan.round-1.verifier", model.NodeVerifier,
		map[string]int{"messageCount": 1})
	emitData(t, sess, model.EventModelResponse, 1, "plan.round-1.verifier", model.NodeVerifier,
		map[string]any{"usage": model.Usage{Input: 10, Output: 4}})
	emitData(t, sess, model.EventUsage, 1, "plan.round-1.verifier", model.NodeVerifier,
		model.Usage{Input: 10, Output: 4})
	emitData(t, sess, model.EventMessage, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"role":        "assistant",
		"contentRef":  assistantRef,
		"contentHash": "sha256:x",
		"toolCalls": []model.ToolCall{
			{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			{ID: "c2", Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)},
		},
	})
	emitData(t, sess, model.EventToolCall, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"id": "c1", "name": "read", "arguments": json.RawMessage(`{"path":"a.go"}`), "approval": "auto",
	})
	emitData(t, sess, model.EventToolCall, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"id": "c2", "name": "bash", "arguments": json.RawMessage(`{"cmd":"ls"}`), "approval": "ask",
	})
	emitData(t, sess, model.EventToolResult, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"id": "c1", "status": "ok", "outputRef": toolRef, "exitCode": 0,
	})
	emitData(t, sess, model.EventMessage, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"role": "tool", "toolCallID": "c1", "contentRef": toolRef,
	})
	emitData(t, sess, model.EventToolResult, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"id": "c2", "status": "ok", "outputRef": assistantRef, "exitCode": 0,
	})
	emitData(t, sess, model.EventMessage, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"role": "tool", "toolCallID": "c2", "contentRef": assistantRef,
	})
	emitData(t, sess, model.EventApprovalResolved, 1, "", model.NodeVerifier, map[string]string{
		"class": "bash_risk", "decision": "deny", "scope": "once",
	})
	emitData(t, sess, model.EventApprovalResolved, 1, "", model.NodeVerifier, map[string]string{
		"class": "bash_network", "decision": "allow", "scope": "session",
	})
	emitData(t, sess, model.EventApprovalResolved, 1, "", model.NodeVerifier, map[string]string{
		"class": "bash_network", "decision": "allow", "scope": "session",
	})
	emitData(t, sess, model.EventContextCompacted, 1, "plan.round-1.verifier", model.NodeVerifier, map[string]any{
		"summaryRef": sumRef, "beforeTokens": 100, "afterTokens": 20,
	})
	emitData(t, sess, model.EventUsage, 1, "plan.round-1.verifier", model.NodeVerifier,
		model.Usage{Input: 2, Output: 1})
	emitData(t, sess, model.EventTurnCompleted, 1, "", "", map[string]string{"status": "ok"})

	// 第二个 turn：write 工具没有 tool_result，应进入 Pending。
	emitData(t, sess, model.EventTurnStarted, 2, "", "", map[string]string{"input": "second"})
	emitData(t, sess, model.EventMessage, 2, "plan.round-2.verifier", model.NodeVerifier, map[string]any{
		"role":       "assistant",
		"contentRef": assistantRef,
		"toolCalls": []model.ToolCall{
			{ID: "c3", Name: "write", Arguments: json.RawMessage(`{"path":"b.go"}`)},
		},
	})
	emitData(t, sess, model.EventToolCall, 2, "plan.round-2.verifier", model.NodeVerifier, map[string]any{
		"id": "c3", "name": "write", "arguments": json.RawMessage(`{"path":"b.go"}`), "approval": "ask",
	})
	mustEmit(t, sess, model.Event{Type: model.EventType("future_event"), Turn: 2, Data: model.MustJSON(map[string]int{"n": 1})})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	decider := &fakeDecider{decisions: map[string]Replay{"c3": ReplayRun}}
	state, err := NewRecoverer().Recover(ctx, sess, decider)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if state.Turn != 2 || state.Round != 2 {
		t.Fatalf("Turn/Round = %d/%d, want 2/2", state.Turn, state.Round)
	}
	if state.Step.Mode != model.ModePlan || state.Step.Round != 2 || state.Step.Node != model.NodeVerifier {
		t.Fatalf("Step = %+v", state.Step)
	}
	if state.Usage.Input != 12 || state.Usage.Output != 5 {
		t.Fatalf("Usage = %+v", state.Usage)
	}
	if len(state.Grants) != 1 || state.Grants[0] != model.ApprovalBashNetwork {
		t.Fatalf("Grants = %+v", state.Grants)
	}
	if state.Summary != "earlier work compressed" {
		t.Fatalf("Summary = %q", state.Summary)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("Pending = %+v", state.Pending)
	}
	p := state.Pending[0]
	if p.CallID != "c3" || p.Name != model.ToolWrite || !p.Mutating || p.Node != model.NodeVerifier {
		t.Fatalf("pending[0] = %+v", p)
	}
	if state.Last != model.EventType("future_event") {
		t.Fatalf("Last = %q", state.Last)
	}
	if state.Meta.LastSeq != state.LastSeq || state.LastSeq == 0 {
		t.Fatalf("LastSeq mismatch: meta=%d state=%d", state.Meta.LastSeq, state.LastSeq)
	}
	if state.Meta.Status != StatusInterrupted {
		t.Fatalf("status = %q, want interrupted", state.Meta.Status)
	}
	joined := strings.Join(state.Warnings, "\n")
	if !strings.Contains(joined, "unknown event type") || !strings.Contains(joined, "ReplayRun") {
		t.Fatalf("warnings = %v", state.Warnings)
	}

	// 用户消息不重复；每个 tool_call_id 恰好一条 tool 消息。
	userCount := 0
	seenTool := map[string]int{}
	var c1Content, c3Content string
	for _, msg := range state.Messages {
		if msg.Role == model.RoleUser {
			userCount++
			if msg.Content == "do it" && userCount > 2 {
				t.Fatalf("duplicate user message: %+v", state.Messages)
			}
		}
		if msg.Role == model.RoleTool {
			seenTool[msg.ToolCallID]++
			switch msg.ToolCallID {
			case "c1":
				c1Content = msg.Content
			case "c3":
				c3Content = msg.Content
			}
		}
	}
	if userCount != 2 {
		t.Fatalf("user messages = %d, want 2: %+v", userCount, state.Messages)
	}
	if seenTool["c1"] != 1 || seenTool["c3"] != 1 {
		t.Fatalf("tool messages = %+v", seenTool)
	}
	if c1Content != "file contents" {
		t.Fatalf("c1 content = %q", c1Content)
	}
	var interrupted map[string]any
	if err := json.Unmarshal([]byte(c3Content), &interrupted); err != nil {
		t.Fatalf("c3 content is not JSON: %q", c3Content)
	}
	if interrupted["status"] != "interrupted" || interrupted["message"] == "" {
		t.Fatalf("c3 interrupted = %+v", interrupted)
	}
	// 每个 assistant tool_calls 都有对应 tool 消息。
	for _, msg := range state.Messages {
		if msg.Role != model.RoleAssistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			if seenTool[call.ID] != 1 {
				t.Fatalf("tool_call %q has %d tool messages", call.ID, seenTool[call.ID])
			}
		}
	}
}

type fakeDecider struct {
	mu        sync.Mutex
	decisions map[string]Replay
	calls     []PendingCall
	err       error
}

func (f *fakeDecider) Decide(_ context.Context, call PendingCall) (Replay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.err != nil {
		return ReplaySkip, f.err
	}
	if d, ok := f.decisions[call.CallID]; ok {
		return d, nil
	}
	return ReplaySkip, nil
}

func TestRecoverMissingArtifactIncompleteTailAndModelRequest(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, opened := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})
	ctx := context.Background()

	emitData(t, sess, model.EventTurnStarted, 1, "", "", map[string]string{"input": "fix bug"})
	emitData(t, sess, model.EventMessage, 1, "code.code", model.NodeCode, map[string]any{
		"role":        "assistant",
		"contentRef":  model.ArtifactRef{SessionID: opened.ID, Name: "artifacts/gone.txt"},
		"toolCalls":   []model.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"x"}`)}},
		"contentHash": "sha256:x",
	})
	emitData(t, sess, model.EventToolCall, 1, "code.code", model.NodeCode, map[string]any{
		"id": "c1", "name": "read", "arguments": json.RawMessage(`{"path":"x"}`),
	})
	emitData(t, sess, model.EventModelRequest, 1, "code.code", model.NodeCode, map[string]int{"messageCount": 3})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟崩溃留下半行 JSON。
	eventsPath := filepath.Join(testSessionDir(dir, opened.ID), "events.jsonl")
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":999,"type":"usage"`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, _ := mustOpen(t, store, OpenOptions{ID: opened.ID, Lock: true})
	state, err := Recover(ctx, resumed, nil)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	joined := strings.Join(state.Warnings, "\n")
	if !strings.Contains(joined, "incomplete") || !strings.Contains(joined, "不完整") {
		t.Fatalf("warnings = %v", state.Warnings)
	}
	if state.LastSeq == 999 {
		t.Fatalf("incomplete tail should not become LastSeq")
	}
	if state.Last != model.EventModelRequest {
		t.Fatalf("Last = %q, want model_request", state.Last)
	}
	if len(state.Pending) != 1 || state.Pending[0].CallID != "c1" {
		t.Fatalf("Pending = %+v", state.Pending)
	}
	if len(state.Messages) == 0 {
		t.Fatal("messages should not be dropped")
	}
	var assistant *model.Message
	for i := range state.Messages {
		if state.Messages[i].Role == model.RoleAssistant {
			assistant = &state.Messages[i]
		}
	}
	if assistant == nil || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant call was dropped: %+v", state.Messages)
	}
	if !strings.Contains(assistant.Content, "artifact missing") {
		t.Fatalf("assistant placeholder = %q", assistant.Content)
	}
	if !strings.Contains(joined, "contentRef") {
		t.Fatalf("missing artifact warning absent: %v", state.Warnings)
	}
	last := state.Messages[len(state.Messages)-1]
	if last.Role != model.RoleTool || last.ToolCallID != "c1" {
		t.Fatalf("last message = %+v", last)
	}
	var interrupted map[string]any
	if err := json.Unmarshal([]byte(last.Content), &interrupted); err != nil {
		t.Fatalf("tool message content: %q", last.Content)
	}
	if interrupted["status"] != "interrupted" {
		t.Fatalf("tool status = %+v", interrupted)
	}
	// 最后事件是 model_request，恢复不得伪造 assistant 回复。
	for _, msg := range state.Messages {
		if msg.Role == model.RoleAssistant && strings.Contains(msg.Content, "model response") {
			t.Fatalf("unexpected fake assistant message: %+v", msg)
		}
	}

	// 崩溃留下的半行不应阻止后续追加：新事件必须独立成行且 seq 连续。
	mustEmit(t, resumed, model.Event{Type: model.EventUsage, Data: model.MustJSON(model.Usage{Input: 1})})
	events, err := resumed.Events(context.Background())
	if err != nil {
		t.Fatalf("Events after incomplete tail: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events after append")
	}
	lastEv := events[len(events)-1]
	if lastEv.Type != model.EventUsage || lastEv.Seq != state.LastSeq+1 {
		t.Fatalf("appended event = %+v, want seq %d", lastEv, state.LastSeq+1)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParseStepRef(t *testing.T) {
	cases := []struct {
		raw  string
		node model.NodeName
		want model.StepRef
	}{
		{"plan.round-1.verifier", "", model.StepRef{Mode: model.ModePlan, Round: 1, Node: model.NodeVerifier}},
		{"code.code", "", model.StepRef{Mode: model.ModeCode, Node: model.NodeCode}},
		{"round-3.deliberate", "", model.StepRef{Round: 3, Node: model.NodeDeliberate}},
		{"plan.round-2", model.NodeAggregation, model.StepRef{Mode: model.ModePlan, Round: 2, Node: model.NodeAggregation}},
		{"verifier", model.NodeVerifier, model.StepRef{Node: model.NodeVerifier}},
	}
	for _, tc := range cases {
		if got := parseStepRef(tc.raw, tc.node); got != tc.want {
			t.Fatalf("parseStepRef(%q,%q) = %+v, want %+v", tc.raw, tc.node, got, tc.want)
		}
	}
}

func TestEventsMalformedMiddleLine(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess, opened := mustOpen(t, store, OpenOptions{Create: true, Mode: model.ModeCode, Lock: true})
	mustEmit(t, sess, model.Event{Type: model.EventUsage, Data: model.MustJSON(model.Usage{Input: 1})})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(testSessionDir(dir, opened.ID), "events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(
		"not-json\n{\"seq\":2,\"type\":\"usage\",\"data\":{\"input\":2}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, _ := mustOpen(t, store, OpenOptions{ID: opened.ID, Lock: true})
	defer resumed.Close()
	events, err := resumed.Events(context.Background())
	if err != nil {
		t.Fatalf("Events should skip malformed lines: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("events = %+v", events)
	}
	if got := maxEventSeq(events); got != 2 {
		t.Fatalf("maxEventSeq = %d", got)
	}
	state, err := Recover(context.Background(), resumed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Warnings) == 0 {
		t.Fatal("expected malformed line warning")
	}
	if state.LastSeq != 2 {
		t.Fatalf("LastSeq = %d, want 2 (meta may have been reset by rewritten log)", state.LastSeq)
	}
}
