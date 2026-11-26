package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

type fakeExecutor struct {
	mu      sync.Mutex
	run     func(context.Context, RunRequest, RunHooks) (RunOutcome, error)
	started chan struct{}
}

func (f *fakeExecutor) Run(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
	}
	if f.run != nil {
		return f.run(ctx, req, hooks)
	}
	if hooks.OnSessionID != nil {
		hooks.OnSessionID(req.SessionID)
	}
	return RunOutcome{}, nil
}

func runTestServer(t *testing.T, ex Executor) (*httptest.Server, string, string) {
	t.Helper()
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "20261106-234456-abcdef"
	writeWebSession(t, work, id, session.Meta{
		SchemaVersion: 1, ID: id, Workspace: work,
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
		Status: session.StatusIdle, Mode: model.ModeAsk, Title: "run test",
	}, []model.Event{
		{Seq: 1, TS: time.Now().Add(-time.Minute), Type: model.EventSessionCreated,
			Data: model.MustJSON(map[string]any{"id": id, "mode": "ask"})},
	}, nil)
	srv, err := New(Deps{
		WorkDir: work, HomeDir: home, Archive: session.NewArchive(work),
		Executor: ex, Clock: time.Now, Logger: logx.Nop(), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, work, id
}

func writeJSONReq(t *testing.T, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ANT-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestRunAPIWriteGuards(t *testing.T) {
	ex := &fakeExecutor{}
	ts, _, id := runTestServer(t, ex)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing X-ANT-Request status = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/"+id+"/turns", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-ANT-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content-type status = %d, want 415", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/sessions", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ANT-Request", "1")
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}
}

func TestRunNewSessionReturnsID(t *testing.T) {
	ex := &fakeExecutor{run: func(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
		hooks.OnSessionID("20261106-234457-abcdef")
		return RunOutcome{ExitCode: 0}, nil
	}}
	ts, _, _ := runTestServer(t, ex)
	resp, body := writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions", map[string]any{
		"input": "new task", "mode": "ask", "model": "alpha",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d body=%s", resp.StatusCode, body)
	}
	var got RunStartResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "20261106-234457-abcdef" || got.RunID == "" || got.Status != "running" {
		t.Fatalf("start response = %+v", got)
	}
}

func TestRunLockConflictReturns409(t *testing.T) {
	ex := &fakeExecutor{run: func(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
		return RunOutcome{}, &model.Error{Class: model.ErrConfigInvalid, Code: "session_locked", Message: "locked"}
	}}
	ts, _, id := runTestServer(t, ex)
	resp, body := writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions/"+id+"/turns", map[string]any{"input": "hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("lock conflict status = %d body=%s", resp.StatusCode, body)
	}
}

func TestRunSSEDeltaAndDone(t *testing.T) {
	release := make(chan struct{})
	ex := &fakeExecutor{run: func(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
		hooks.OnSessionID(req.SessionID)
		<-release
		hooks.OnEvent(&model.Event{Seq: 2, TS: time.Now(), Type: model.EventModelResponse, Turn: 1})
		hooks.OnDelta("hello", "think")
		return RunOutcome{ExitCode: 0}, nil
	}}
	ts, _, id := runTestServer(t, ex)
	resp, body := writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions/"+id+"/turns", map[string]any{"input": "hi"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d body=%s", resp.StatusCode, body)
	}

	streamResp, err := http.Get(ts.URL + "/api/sessions/" + id + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	reader := bufio.NewReader(streamResp.Body)
	close(release)
	if ev := readSSEEvent(t, reader); ev != "status" {
		t.Fatalf("first event = %q, want status", ev)
	}
	seenDelta, seenDone := false, false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(seenDelta && seenDone) {
		event, data := readSSEEventWithData(t, reader)
		switch event {
		case "delta":
			if data["text"] == "hello" && data["reasoning"] == "think" {
				seenDelta = true
			}
		case "run_done":
			seenDone = true
		}
	}
	if !seenDelta || !seenDone {
		t.Fatalf("SSE delta=%v done=%v", seenDelta, seenDone)
	}
}

func TestApprovalRoundtripOverSSE(t *testing.T) {
	choiceCh := make(chan approval.Choice, 1)
	ex := &fakeExecutor{run: func(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
		hooks.OnSessionID(req.SessionID)
		choice, err := hooks.Prompter.Confirm(ctx, approval.ConfirmPrompt{
			Class: model.ApprovalBashRisk, Title: "bash 风险", Detail: "rm file", CWD: "/tmp",
		})
		if err != nil {
			return RunOutcome{ExitCode: 1}, err
		}
		choiceCh <- choice
		return RunOutcome{ExitCode: 0}, nil
	}}
	ts, _, id := runTestServer(t, ex)
	resp, body := writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions/"+id+"/turns", map[string]any{"input": "hi"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d body=%s", resp.StatusCode, body)
	}

	streamResp, err := http.Get(ts.URL + "/api/sessions/" + id + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	reader := bufio.NewReader(streamResp.Body)
	var promptID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && promptID == "" {
		event, data := readSSEEventWithData(t, reader)
		if event == "approval" {
			promptID, _ = data["id"].(string)
		}
	}
	if promptID == "" {
		t.Fatal("did not receive approval SSE event")
	}
	resp, body = writeJSONReq(t, http.MethodPost, ts.URL+"/api/approvals/"+promptID, map[string]any{"decision": "allow_once"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approval status = %d body=%s", resp.StatusCode, body)
	}
	select {
	case choice := <-choiceCh:
		if choice != approval.ChoiceOnce {
			t.Fatalf("choice = %v, want once", choice)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not receive approval")
	}
}

func TestCancelRun(t *testing.T) {
	ex := &fakeExecutor{run: func(ctx context.Context, req RunRequest, hooks RunHooks) (RunOutcome, error) {
		hooks.OnSessionID(req.SessionID)
		<-ctx.Done()
		return RunOutcome{ExitCode: 3}, nil
	}}
	ts, _, id := runTestServer(t, ex)
	resp, body := writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions/"+id+"/turns", map[string]any{"input": "hi"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = writeJSONReq(t, http.MethodPost, ts.URL+"/api/sessions/"+id+"/cancel", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["cancelled"] != true {
		t.Fatalf("cancel response = %v", out)
	}
}

func readSSEEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	event, _ := readSSEEventWithData(t, r)
	return event
}

func readSSEEventWithData(t *testing.T, r *bufio.Reader) (string, map[string]any) {
	t.Helper()
	event := ""
	data := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event == "" && data == "" {
				continue
			}
			payload := map[string]any{}
			if data != "" {
				_ = json.Unmarshal([]byte(data), &payload)
			}
			return event, payload
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}
