package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

const modelsJSON = `{
  "schemaVersion": 1,
  "providers": {
    "p1": {
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:1/v1",
      "models": [
        {"id": "real-alpha", "name": "alpha", "contextWindow": 100000, "supportsTools": true},
        {"id": "real-beta", "name": "beta", "contextWindow": 32000, "supportsTools": false}
      ]
    },
    "p2": {
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:2/v1",
      "models": [
        {"id": "real-alpha", "name": "gamma", "contextWindow": 200000, "supportsTools": true}
      ]
    }
  }
}`

func testConfig(t *testing.T, settings string) config.View {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ant"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ant", "models.json"), []byte(modelsJSON), 0o600); err != nil {
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

func TestRegistryResolve(t *testing.T) {
	cfg := testConfig(t, `{"defaultModel":"alpha"}`)
	reg := NewRegistry(cfg, NewFactory(cfg, logx.Nop()), logx.Nop())

	res, err := reg.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderName != "p1" || res.ModelID != "real-alpha" || res.Alias != "alpha" {
		t.Fatalf("resolved = %+v", res)
	}
	if _, err := reg.Resolve("p2:gamma"); err != nil {
		t.Fatalf("qualified alias: %v", err)
	}
	if _, err := reg.Resolve("real-beta"); err != nil {
		t.Fatalf("id resolution should work: %v", err)
	}
	if _, err := reg.Resolve("missing"); err == nil {
		t.Fatal("unknown alias should fail")
	}
	if _, err := reg.Resolve("real-alpha"); err == nil {
		t.Fatal("duplicate id across providers should be ambiguous")
	}
}

func TestRegistryResolveForNodePriority(t *testing.T) {
	cfg := testConfig(t, `{
		"defaultModel":"alpha",
		"nodeModels":{"ask":"beta","verifier":"gamma"}
	}`)
	reg := NewRegistry(cfg, NewFactory(cfg, logx.Nop()), logx.Nop())

	res, err := reg.ResolveForNode(model.NodeVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if res.Alias != "gamma" {
		t.Fatalf("verifier alias = %q", res.Alias)
	}
	if _, err := reg.ResolveForNode(model.NodeAsk); err == nil {
		t.Fatal("strict policy should reject tool-less model for ask")
	}

	degraded := testConfig(t, `{
		"defaultModel":"alpha",
		"modelPolicy":"degraded",
		"nodeModels":{"ask":"beta"}
	}`)
	reg = NewRegistry(degraded, NewFactory(degraded, logx.Nop()), logx.Nop())
	if _, err := reg.ResolveForNode(model.NodeAsk); err != nil {
		t.Fatalf("degraded ask should be allowed: %v", err)
	}
	if _, err := reg.ResolveForNode(model.NodeCode); err != nil {
		t.Fatalf("code should use default model: %v", err)
	}

	override := testConfig(t, `{"defaultModel":"alpha"}`)
	cliCfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: override.(*config.Config).WorkDir(),
		HomeDir: override.(*config.Config).HomeDir(),
		CLI:     config.Overrides{Model: "gamma", NodeModels: map[string]string{"ask": "gamma"}},
		Env:     []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg = NewRegistry(cliCfg, NewFactory(cliCfg, logx.Nop()), logx.Nop())
	for _, node := range model.AllNodes() {
		res, err := reg.ResolveForNode(node)
		if err != nil {
			t.Fatalf("%s: %v", node, err)
		}
		if res.Alias != "gamma" {
			t.Fatalf("%s alias = %q", node, res.Alias)
		}
	}
}

func TestFallbackNext(t *testing.T) {
	cfg := testConfig(t, `{"defaultModel":"alpha","fallbackModels":["gamma"]}`)
	reg := NewRegistry(cfg, NewFactory(cfg, logx.Nop()), logx.Nop())
	fb := NewFallback(cfg, reg)
	cur, err := reg.Resolve("gamma")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fb.Next(cur, model.ErrProviderRateLimit); ok {
		t.Fatal("no fallback after last alias")
	}
	cur, err = reg.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	next, ok := fb.Next(cur, model.ErrProviderUnavailable)
	if !ok || next.Alias != "gamma" {
		t.Fatalf("next = %+v/%v", next, ok)
	}
	if _, ok := fb.Next(cur, model.ErrProviderAuth); ok {
		t.Fatal("auth errors must not fall back")
	}
}

type flakyProvider struct {
	calls int
	fail  int
	err   error
	resp  ChatResponse
}

func (f *flakyProvider) Name() string { return "flaky" }
func (f *flakyProvider) Capabilities(string) (model.Capabilities, error) {
	return model.Capabilities{ContextWindow: 1000, SupportsTools: true}, nil
}
func (f *flakyProvider) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error) {
	f.calls++
	if f.calls <= f.fail {
		return ChatResponse{}, f.err
	}
	return f.resp, nil
}

func TestClientRetriesAndEmits(t *testing.T) {
	var events []model.Event
	sink := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		events = append(events, *ev)
		return nil
	})
	flaky := &flakyProvider{
		fail: 2,
		err:  &model.Error{Class: model.ErrProviderRateLimit, Code: "429", Message: "slow down", Retryable: true},
		resp: ChatResponse{
			Message:      model.Message{Role: model.RoleAssistant, Content: "hello"},
			Usage:        model.Usage{Input: 3, Output: 2},
			FinishReason: "stop",
		},
	}
	retrier := NewRetrierWithSleep(config.RetrySettings{Attempts: 3, BaseDelayMs: 1, MaxDelayMs: 2}, func(ctx context.Context, d time.Duration) error { return nil })
	client := NewClient(flaky, retrier, nil, sink)
	client.Bind(Resolved{Alias: "alpha", ModelID: "real-alpha", Capabilities: model.Capabilities{ContextWindow: 1000}}, nil)
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "alpha", Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if flaky.calls != 3 || resp.Message.Content != "hello" {
		t.Fatalf("calls=%d resp=%+v", flaky.calls, resp)
	}
	var types []model.EventType
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	joined := eventTypes(types)
	for _, want := range []string{"model_request", "model_response"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, joined)
		}
	}
	if got := flaky.calls; got != 3 {
		t.Fatalf("retry attempts = %d", got)
	}
}

func TestClientDoesNotRetryAuth(t *testing.T) {
	flaky := &flakyProvider{
		fail: 3,
		err:  &model.Error{Class: model.ErrProviderAuth, Code: "401", Message: "bad key"},
	}
	retrier := NewRetrierWithSleep(config.RetrySettings{Attempts: 3}, func(context.Context, time.Duration) error { return nil })
	client := NewClient(flaky, retrier, nil, nil)
	client.Bind(Resolved{Alias: "alpha", ModelID: "x"}, nil)
	_, err := client.Chat(context.Background(), ChatRequest{Model: "alpha"}, nil)
	if err == nil || model.ClassOf(err) != model.ErrProviderAuth {
		t.Fatalf("err = %v", err)
	}
	if flaky.calls != 1 {
		t.Fatalf("auth should not retry, calls=%d", flaky.calls)
	}
}

func TestFactoryReturnsNotImplemented(t *testing.T) {
	cfg := testConfig(t, `{"defaultModel":"alpha"}`)
	f := NewFactory(cfg, logx.Nop())
	_, err := f.New(cfg.Providers()[0])
	if err == nil {
		t.Fatal("abstract-layer factory should be explicit")
	}
	if model.ClassOf(err) != model.ErrAdapterNotImplemented {
		t.Fatalf("class = %q", model.ClassOf(err))
	}
}

func eventTypes(types []model.EventType) string {
	var b strings.Builder
	for _, t := range types {
		b.WriteString(string(t))
		b.WriteByte(' ')
	}
	return b.String()
}
