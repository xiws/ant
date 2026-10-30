package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/provider/fake"
	"github.com/xiws/ant/internal/version"
)

type captureSink struct {
	mu     sync.Mutex
	events []model.Event
}

func (c *captureSink) Emit(ctx context.Context, ev *model.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, *ev)
	return nil
}

func (c *captureSink) snapshot() []model.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.Event(nil), c.events...)
}

func setupWorkspace(t *testing.T, settings string) (wd, home string) {
	t.Helper()
	wd = t.TempDir()
	home = filepath.Join(wd, "home")
	if err := os.MkdirAll(filepath.Join(wd, ".ant"), 0o700); err != nil {
		t.Fatal(err)
	}
	models := `{
		"schemaVersion": 1,
		"providers": {
			"fake": {
				"api": "openai-completions",
				"baseUrl": "http://127.0.0.1:1/v1",
				"models": [
					{"id": "fake-1", "name": "fake-model", "contextWindow": 100000, "maxOutputTokens": 4096, "supportsTools": true}
				]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(wd, ".ant", "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, ".ant", "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return wd, home
}

func runApp(t *testing.T, args []string, wd, home string, fp *fake.Provider, capture *captureSink) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	stdio := Stdio{
		In:    strings.NewReader(""),
		Out:   &out,
		Err:   &errOut,
		IsTTY: false,
		Env:   []string{"HOME=" + home},
	}
	app, err := New(context.Background(), args, stdio, Overrides{
		Provider:  fp,
		Clock:     func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
		EventSink: capture,
		WorkDir:   wd,
		HomeDir:   home,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	code := app.Run(context.Background())
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return code, out.String(), errOut.String()
}

func eventTypes(events []model.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Type))
	}
	return out
}

func hasEvent(events []model.Event, want model.EventType) bool {
	for _, ev := range events {
		if ev.Type == want {
			return true
		}
	}
	return false
}

func TestAppAskModeEndToEnd(t *testing.T) {
	settings := `{
		"schemaVersion": 1,
		"defaultModel": "fake-model",
		"defaultMode": "ask",
		"project": {"autoInit": false},
		"log": {"level": "error"}
	}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New().QueueText("这是 ask 模式的回答，引用 a.go:1。")
	capture := &captureSink{}

	code, out, errOut := runApp(t, []string{"new", "-m", "ask", "解释一下 read_code"}, wd, home, fp, capture)
	if code != 0 {
		t.Fatalf("exit code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "这是 ask 模式的回答") {
		t.Fatalf("stdout = %q", out)
	}
	events := capture.snapshot()
	for _, want := range []model.EventType{
		model.EventSessionCreated, model.EventTurnStarted, model.EventNodeStarted,
		model.EventModelResponse, model.EventStepCompleted, model.EventNodeCompleted, model.EventTurnCompleted,
	} {
		if !hasEvent(events, want) {
			t.Fatalf("missing event %s; got %v", want, eventTypes(events))
		}
	}
	if events[0].Type != model.EventSessionCreated || events[0].Seq != 1 {
		t.Fatalf("first event = %+v (seq %d)", events[0], events[0].Seq)
	}
	for _, ev := range events {
		switch ev.Type {
		case model.EventSessionCreated, model.EventModeChanged:
			continue
		}
		if ev.Turn != 1 {
			t.Fatalf("event %s turn=%d, want 1", ev.Type, ev.Turn)
		}
		if ev.Type == model.EventModelRequest && ev.Node != model.NodeAsk {
			t.Fatalf("model_request node = %q, want ask", ev.Node)
		}
	}
	if len(fp.Requests()) != 1 {
		t.Fatalf("provider requests = %d", len(fp.Requests()))
	}
	files, _ := filepath.Glob(filepath.Join(wd, ".ant", "sessions", "*", "events.jsonl"))
	if len(files) != 1 {
		t.Fatalf("events.jsonl files = %v", files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"session_created"`) || !strings.Contains(string(body), `"turn_completed"`) {
		t.Fatalf("events.jsonl content:\n%s", body)
	}
}

func TestAppCodeModeWithVerifier(t *testing.T) {
	settings := `{
		"schemaVersion": 1,
		"defaultModel": "fake-model",
		"defaultMode": "code",
		"project": {"autoInit": false},
		"log": {"level": "error"}
	}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New()
	fp.QueueText("## 变更\n无\n\n## 已执行\n无\n\n## 未解决\n无")
	fp.QueueText(`{"pass":true,"summary":"无需改动","failures":[],"checks":[{"cmd":"go test ./...","exitCode":0}]}`)
	capture := &captureSink{}

	code, out, errOut := runApp(t, []string{"new", "-m", "code", "给 Test 增加边界检查"}, wd, home, fp, capture)
	if code != 0 {
		t.Fatalf("exit code = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	events := capture.snapshot()
	if hasEvent(events, model.EventModelFallback) {
		t.Fatal("unexpected model_fallback")
	}
	if !strings.Contains(out, "完成") {
		t.Fatalf("summary missing in stdout: %q", out)
	}
	if len(fp.Requests()) < 2 {
		t.Fatalf("expected code + verifier requests, got %d", len(fp.Requests()))
	}
	// 事件顺序硬约束：turn_started 在第一个 node_started 之前，turn_completed 在最后。
	firstNode, turnStarted, turnCompleted := -1, -1, -1
	for i, ev := range events {
		switch ev.Type {
		case model.EventNodeStarted:
			if firstNode < 0 {
				firstNode = i
			}
		case model.EventTurnStarted:
			if turnStarted < 0 {
				turnStarted = i
			}
		case model.EventTurnCompleted:
			turnCompleted = i
		}
	}
	if turnStarted < 0 || firstNode < 0 || turnCompleted < 0 {
		t.Fatalf("missing core events: %v", eventTypes(events))
	}
	if !(turnStarted < firstNode && firstNode < turnCompleted) {
		t.Fatalf("event order violated: %v", eventTypes(events))
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"version"}, Stdio{Out: &out, Err: &errOut, Env: []string{}})
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), version.String()) {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestMissingModelIsConfigError(t *testing.T) {
	wd := t.TempDir()
	home := filepath.Join(wd, "home")
	if err := os.MkdirAll(filepath.Join(wd, ".ant"), 0o700); err != nil {
		t.Fatal(err)
	}
	fp := fake.New()
	var out, errOut bytes.Buffer
	app, err := New(context.Background(), []string{"new", "hello"}, Stdio{
		In: strings.NewReader(""), Out: &out, Err: &errOut, IsTTY: false, Env: []string{},
	}, Overrides{Provider: fp, WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	code := app.Run(context.Background())
	_ = app.Close()
	if code != 2 {
		t.Fatalf("exit = %d, stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
}

func TestProviderOverrideUsed(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New(fake.WithName("injected")).QueueText("ok")
	capture := &captureSink{}
	code, _, errOut := runApp(t, []string{"new", "-m", "ask", "hi"}, wd, home, fp, capture)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if fp.Name() != "injected" {
		t.Fatal("override provider not used")
	}
}

func TestREPLTwoTurnsKeepsHistory(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New().QueueText("回答一").QueueText("回答二")
	capture := &captureSink{}
	var out, errOut bytes.Buffer
	stdio := Stdio{
		In:    strings.NewReader("第一个问题\n第二个问题\n/exit\n"),
		Out:   &out,
		Err:   &errOut,
		IsTTY: true,
		Env:   []string{},
	}
	app, err := New(context.Background(), []string{"new", "-m", "ask"}, stdio, Overrides{
		Provider: fp, WorkDir: wd, HomeDir: home, EventSink: capture,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	code := app.Run(context.Background())
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "回答一") || !strings.Contains(out.String(), "回答二") {
		t.Fatalf("stdout = %q", out.String())
	}
	reqs := fp.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var history string
	for _, m := range reqs[1].Messages {
		history += string(m.Role) + ":" + m.Content + "\n"
	}
	for _, want := range []string{"第一个问题", "回答一"} {
		if !strings.Contains(history, want) {
			t.Fatalf("second request missing %q:\n%s\nstderr: %s", want, history, errOut.String())
		}
	}
}

func TestJSONModeEmitsNDJSON(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New().QueueText("json answer")
	var out, errOut bytes.Buffer
	app, err := New(context.Background(), []string{"new", "-m", "ask", "--json", "hi"}, Stdio{
		In: strings.NewReader(""), Out: &out, Err: &errOut, IsTTY: false, Env: []string{},
	}, Overrides{Provider: fp, WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	code := app.Run(context.Background())
	_ = app.Close()
	if code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		var ev struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout line is not JSON: %q (%v)", line, err)
		}
		if ev.Seq <= 0 {
			t.Fatalf("seq not backfilled in NDJSON: %q", line)
		}
		seen[ev.Type] = true
	}
	for _, want := range []string{"session_created", "turn_started", "turn_completed"} {
		if !seen[want] {
			t.Fatalf("missing %s in NDJSON: %v", want, seen)
		}
	}
	if strings.Contains(out.String(), "json answer") {
		t.Fatal("--json mode must not write human text to stdout")
	}
}

func TestInitSubcommandCreatesProjectContext(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"code","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	if err := os.WriteFile(filepath.Join(wd, "go.mod"), []byte("module example.com/x\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &captureSink{}
	code, out, errOut := runApp(t, []string{"init"}, wd, home, fake.New(), capture)
	if code != 0 {
		t.Fatalf("exit = %d stdout=%s stderr=%s", code, out, errOut)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".ant", "project.md"))
	if err != nil {
		t.Fatalf("project.md not created: %v", err)
	}
	if !strings.Contains(string(body), "ant:generated:start") {
		t.Fatalf("project.md missing markers:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(wd, ".ant", "project.meta.json")); err != nil {
		t.Fatalf("project.meta.json missing: %v", err)
	}
}

func TestNonInteractiveAskAbortsTurn(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"code","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New().Queue(provider.ChatResponse{
		Message: model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{{
				ID:        "c1",
				Name:      "ask",
				Arguments: json.RawMessage(`{"question":"继续吗？","allowFreeText":true}`),
			}},
		},
		FinishReason: "tool_calls",
	})
	capture := &captureSink{}
	code, out, errOut := runApp(t, []string{"new", "-m", "code", "--non-interactive", "问用户"}, wd, home, fp, capture)
	if code != 4 {
		t.Fatalf("exit = %d (want 4)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
}

func TestSessionsAndShowSubcommands(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	if code, out, errOut := runApp(t, []string{"new", "-m", "ask", "记录一次问答"}, wd, home, fake.New().QueueText("answer"), &captureSink{}); code != 0 {
		t.Fatalf("turn exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	dirs, err := filepath.Glob(filepath.Join(wd, ".ant", "sessions", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("session dirs: %v %v", dirs, err)
	}
	id := filepath.Base(dirs[0])

	code, out, errOut := runApp(t, []string{"sessions"}, wd, home, fake.New(), &captureSink{})
	if code != 0 || !strings.Contains(out, id) {
		t.Fatalf("sessions exit=%d stdout=%s stderr=%s", code, out, errOut)
	}

	code, out, errOut = runApp(t, []string{"show", id}, wd, home, fake.New(), &captureSink{})
	if code != 0 || !strings.Contains(out, "Tokens:") {
		t.Fatalf("show exit=%d stdout=%s stderr=%s", code, out, errOut)
	}

	code, out, errOut = runApp(t, []string{"show", id, "--events"}, wd, home, fake.New(), &captureSink{})
	if code != 0 || !strings.Contains(out, `"type":"turn_completed"`) {
		t.Fatalf("show --events exit=%d stdout=%s stderr=%s", code, out, errOut)
	}

	code, out, errOut = runApp(t, []string{"models"}, wd, home, fake.New(), &captureSink{})
	if code != 0 || !strings.Contains(out, "fake-model") {
		t.Fatalf("models exit=%d stdout=%s stderr=%s", code, out, errOut)
	}

	code, out, errOut = runApp(t, []string{"config"}, wd, home, fake.New(), &captureSink{})
	if code != 0 || !strings.Contains(out, "fake-model") {
		t.Fatalf("config exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
}
