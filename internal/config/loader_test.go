package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/model"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, wd, home string, cli Overrides, env ...string) (*Config, error) {
	t.Helper()
	return NewLoader().Load(context.Background(), Options{WorkDir: wd, HomeDir: home, CLI: cli, Env: env})
}

func TestLoaderDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := load(t, dir, filepath.Join(dir, "home"), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Settings()
	if s.Limits.MaxToolTurns != 12 || s.Limits.BashTimeoutMs != 120000 {
		t.Fatalf("unexpected defaults: %+v", s.Limits)
	}
	if s.DefaultMode != model.ModeCode || s.ModelPolicy != "strict" || !s.Context.AutoCompact {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	if len(cfg.Models()) != 0 {
		t.Fatal("no models expected")
	}
	if len(cfg.Warnings()) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings())
	}
}

func TestLoaderDeepMergeAndArrayReplace(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	writeFile(t, filepath.Join(home, "settings.json"), `{
		"limits": {"maxToolTurns": 20},
		"sandbox": {"allowReadPaths": ["/a"]},
		"context": {"keepRecentTurns": 9}
	}`)
	writeFile(t, filepath.Join(dir, ".ant", "settings.json"), `{
		"limits": {"maxToolTurns": 30},
		"sandbox": {"allowReadPaths": ["/b", "/c"]},
		"context": {"keepRecentTurns": null, "summaryMaxTokens": null}
	}`)
	cfg, err := load(t, dir, home, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Settings()
	if s.Limits.MaxToolTurns != 30 {
		t.Fatalf("workspace should override scalar, got %d", s.Limits.MaxToolTurns)
	}
	if got := strings.Join(s.Sandbox.AllowReadPaths, ","); got != "/b,/c" {
		t.Fatalf("arrays must replace, got %q", got)
	}
	if s.Context.KeepRecentTurns != 9 {
		t.Fatalf("null should fall back to home layer (9), got %d", s.Context.KeepRecentTurns)
	}
	if s.Context.SummaryMaxTokens != 4096 {
		t.Fatalf("null with no lower layer should fall back to builtin default, got %d", s.Context.SummaryMaxTokens)
	}
	if o, ok := cfg.Origin("limits.maxToolTurns"); !ok || o != OriginWorkspace {
		t.Fatalf("origin = %v/%v, want workspace", o, ok)
	}
}

func TestLoaderCLIOverridesAndModels(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	writeFile(t, filepath.Join(dir, ".ant", "models.json"), `{
		"schemaVersion": 1,
		"providers": {
			"p1": {
				"api": "openai-completions",
				"baseUrl": "http://127.0.0.1:1/v1",
				"apiKeyEnv": "KEY",
				"models": [
					{"id": "real-1", "name": "alpha", "contextWindow": 100000},
					{"id": "real-2", "name": "beta", "contextWindow": 200000, "supportsTools": true}
				]
			}
		}
	}`)
	writeFile(t, filepath.Join(dir, ".ant", "settings.json"), `{"defaultModel":"alpha"}`)
	cfg, err := load(t, dir, home, Overrides{
		Model:      "beta",
		Mode:       model.ModePlan,
		Yes:        true,
		LogLevel:   "debug",
		NodeModels: map[string]string{"verifier": "alpha"},
	}, "KEY=secret-value")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Settings()
	if s.CLIModel != "beta" || s.CLINodeModels["verifier"] != "alpha" {
		t.Fatalf("CLI overrides not applied: %+v", s)
	}
	if s.DefaultMode != model.ModePlan || !s.Yes || s.Log.Level != "debug" {
		t.Fatalf("CLI runtime overrides not applied: %+v", s)
	}
	models := cfg.Models()
	if len(models) != 2 {
		t.Fatalf("models = %d", len(models))
	}
	if !models[0].SupportsStreaming || models[0].SupportsParallelToolCalls {
		t.Fatalf("optional defaults wrong: %+v", models[0])
	}
	if models[0].MaxOutputTokens != 4096 || models[0].DefaultTemperature != 0.2 {
		t.Fatalf("numeric defaults wrong: %+v", models[0])
	}
	provider := cfg.Providers()[0]
	if provider.APIKey.String() != "<redacted>" || provider.APIKey.Reveal() != "secret-value" {
		t.Fatalf("apiKeyEnv not applied securely")
	}
	blob, _ := json.Marshal(provider)
	if strings.Contains(string(blob), "secret-value") {
		t.Fatal("secret leaked through JSON")
	}
}

func TestLoaderValidationErrors(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	writeFile(t, filepath.Join(dir, ".ant", "models.json"), `{
		"providers": {"p1": {"api": "openai-completions", "baseUrl": "http://x/v1",
			"models": [
				{"id": "1", "name": "dup", "contextWindow": 1000},
				{"id": "2", "name": "dup", "contextWindow": 1000}
			]}}
	}`)
	if _, err := load(t, dir, home, Overrides{}); err == nil {
		t.Fatal("duplicate names should fail")
	} else if model.ClassOf(err) != model.ErrConfigInvalid {
		t.Fatalf("class = %q", model.ClassOf(err))
	}

	writeFile(t, filepath.Join(dir, ".ant", "models.json"), `{
		"providers": {"p1": {"api": "openai-completions", "baseUrl": "http://x/v1",
			"models": [{"id": "1", "name": "m", "contextWindow": 100}]}}
	}`)
	writeFile(t, filepath.Join(dir, ".ant", "settings.json"), `{"defaultModel":"nope"}`)
	if _, err := load(t, dir, home, Overrides{}); err == nil {
		t.Fatal("unknown defaultModel should fail")
	}
}

func TestLoaderStrictJSONAndSize(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	writeFile(t, filepath.Join(dir, ".ant", "settings.json"), `{"defaultMode":"code",}`)
	if _, err := load(t, dir, home, Overrides{}); err == nil {
		t.Fatal("trailing comma should fail")
	}
}
