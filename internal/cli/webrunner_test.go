package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/provider/fake"
	"github.com/xiws/ant/internal/session"
	"github.com/xiws/ant/internal/web"
)

func TestWebExecutorCreatesAndContinuesSession(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: wd, HomeDir: home, Env: []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := fake.New().QueueText("first answer").QueueText("second answer")
	ex := &webExecutor{
		workDir: wd, homeDir: home, env: []string{"HOME=" + home}, cfg: cfg, provider: fp,
		clock: func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
	var sessionID string
	var deltas strings.Builder
	events := []model.Event{}
	hooks := web.RunHooks{
		OnSessionID: func(id string) { sessionID = id },
		OnEvent:     func(ev *model.Event) { events = append(events, *ev) },
		OnDelta:     func(text, reasoning string) { deltas.WriteString(text) },
	}
	outcome, err := ex.Run(context.Background(), web.RunRequest{Input: "first", Mode: model.ModeAsk, Model: "fake-model"}, hooks)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if outcome.ExitCode != 0 || sessionID == "" {
		t.Fatalf("first outcome=%+v session=%q events=%s", outcome, sessionID, eventDump(events))
	}
	if deltas.String() != "first answer" {
		t.Fatalf("first deltas = %q", deltas.String())
	}
	if _, err := os.Stat(filepath.Join(wd, ".ant", "sessions", sessionID, "events.jsonl")); err != nil {
		t.Fatalf("events.jsonl: %v", err)
	}
	if !hasEvent(events, model.EventTurnCompleted) {
		t.Fatalf("first events missing turn_completed: %v", eventTypes(events))
	}

	// 续跑同一 session：Web 每次提交都复用 CLI 组合根，但不复用旧 App 状态。
	deltas.Reset()
	events = nil
	outcome, err = ex.Run(context.Background(), web.RunRequest{
		SessionID: sessionID, Input: "second", Mode: model.ModeAsk, Model: "fake-model",
	}, hooks)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if outcome.ExitCode != 0 || deltas.String() != "second answer" {
		t.Fatalf("second outcome=%+v deltas=%q", outcome, deltas.String())
	}
	if !hasEvent(events, model.EventTurnCompleted) {
		t.Fatalf("second events missing turn_completed: %v", eventTypes(events))
	}
	reqs := fp.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d", len(reqs))
	}
	var history strings.Builder
	for _, m := range reqs[1].Messages {
		history.WriteString(string(m.Role) + ":" + m.Content + "\n")
	}
	for _, want := range []string{"first", "first answer", "second"} {
		if !strings.Contains(history.String(), want) {
			t.Fatalf("second request missing %q:\n%s", want, history.String())
		}
	}
}

func eventDump(events []model.Event) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString(string(ev.Type))
		b.WriteString(" ")
		b.Write(ev.Data)
		b.WriteString("\n")
	}
	return b.String()
}

type recordingPrompter struct {
	asked chan approval.Question
}

func (p *recordingPrompter) Confirm(ctx context.Context, cp approval.ConfirmPrompt) (approval.Choice, error) {
	return approval.ChoiceDeny, nil
}

func (p *recordingPrompter) Ask(ctx context.Context, q approval.Question) (approval.Answer, error) {
	p.asked <- q
	return approval.Answer{Text: "继续"}, nil
}

func TestWebExecutorBridgesAskPrompter(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: wd, HomeDir: home, Env: []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := fake.New().Queue(provider.ChatResponse{
		Message: model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{{
				ID: "call-1", Name: "ask", Arguments: json.RawMessage(`{"question":"继续吗？","allowFreeText":true}`),
			}},
		},
		FinishReason: "tool_calls",
	}).QueueText("继续完成")
	ex := &webExecutor{
		workDir: wd, homeDir: home, env: []string{"HOME=" + home}, cfg: cfg, provider: fp,
		clock: func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
	prompter := &recordingPrompter{asked: make(chan approval.Question, 1)}
	outcome, err := ex.Run(context.Background(), web.RunRequest{
		Input: "需要确认", Mode: model.ModeAsk, Model: "fake-model",
	}, web.RunHooks{Prompter: prompter})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome.ExitCode != 0 {
		t.Fatalf("exit = %d", outcome.ExitCode)
	}
	select {
	case q := <-prompter.asked:
		if q.Question != "继续吗？" || !q.AllowFreeText {
			t.Fatalf("question = %+v", q)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompter.Ask was not called")
	}
}

func TestWebExecutorHTTPTwoTurnsKeepsNewInput(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: wd, HomeDir: home, Env: []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := fake.New().QueueText("first answer").QueueText("second answer")
	ex := &webExecutor{
		workDir: wd, homeDir: home, env: []string{"HOME=" + home}, cfg: cfg, provider: fp,
		clock: time.Now,
	}
	srv, err := web.New(web.Deps{
		WorkDir: wd, HomeDir: home, Config: cfg, Archive: session.NewArchive(wd),
		Executor: ex, Clock: time.Now, Logger: logx.Nop(), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path string, payload any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-ANT-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST %s status=%d body=%s", path, resp.StatusCode, body)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := post("/api/sessions", map[string]any{"input": "first", "mode": "ask", "model": "fake-model"})
	sessionID, _ := first["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("first response = %v", first)
	}
	waitForTurnCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(ts.URL + "/api/sessions/" + sessionID)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var view struct {
				Meta struct {
					TurnCount int    `json:"turnCount"`
					Status    string `json:"status"`
				} `json:"meta"`
			}
			_ = json.Unmarshal(body, &view)
			if view.Meta.TurnCount >= want && view.Meta.Status == "idle" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("turnCount did not reach %d", want)
	}
	waitForTurnCount(1)
	second := post("/api/sessions/"+sessionID+"/turns", map[string]any{"input": "second", "mode": "ask", "model": "fake-model"})
	if second["sessionId"] != sessionID {
		t.Fatalf("second response = %v", second)
	}
	waitForTurnCount(2)

	resp, err := http.Get(ts.URL + "/api/sessions/" + sessionID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	if !strings.Contains(text, `"input":"first"`) || !strings.Contains(text, `"input":"second"`) {
		t.Fatalf("events missing distinct inputs:\n%s", text)
	}
	reqs := fp.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d", len(reqs))
	}
	for i, m := range reqs[1].Messages {
		t.Logf("second request message[%d] role=%s content=%q", i, m.Role, m.Content)
	}
}

func TestWebExecutorDoesNotReplayFailedTurnOnNewInput(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: wd, HomeDir: home, Env: []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := fake.New().QueueError(errors.New("first turn failed")).QueueText("second answer")
	ex := &webExecutor{
		workDir: wd, homeDir: home, env: []string{"HOME=" + home}, cfg: cfg, provider: fp,
		clock: time.Now,
	}
	var sessionID string
	events := []model.Event{}
	hooks := web.RunHooks{
		OnSessionID: func(id string) { sessionID = id },
		OnEvent:     func(ev *model.Event) { events = append(events, *ev) },
	}
	outcome, err := ex.Run(context.Background(), web.RunRequest{
		Input: "first", Mode: model.ModeAsk, Model: "fake-model",
	}, hooks)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if outcome.ExitCode == 0 || sessionID == "" {
		t.Fatalf("first outcome=%+v session=%q", outcome, sessionID)
	}

	events = nil
	outcome, err = ex.Run(context.Background(), web.RunRequest{
		SessionID: sessionID, Input: "second", Mode: model.ModeAsk, Model: "fake-model",
	}, hooks)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if outcome.ExitCode != 0 {
		t.Fatalf("second exit = %d", outcome.ExitCode)
	}
	var turn2Input string
	for _, ev := range events {
		if ev.Type == model.EventTurnStarted && ev.Turn == 2 {
			var data struct {
				Input   string `json:"input"`
				Resumed bool   `json:"resumed"`
			}
			_ = json.Unmarshal(ev.Data, &data)
			turn2Input = data.Input
			if data.Resumed {
				t.Fatal("new Web input must not reuse a failed turn as resumed turn")
			}
		}
	}
	if turn2Input != "second" {
		t.Fatalf("turn 2 input = %q, want second; events=%s", turn2Input, eventDump(events))
	}
}
