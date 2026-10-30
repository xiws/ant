package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(dir, ".ant", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ant", "prompt.md"), []byte("项目约定：先读代码"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ant", "prompts", "code.md"), []byte("覆盖后的 code prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewLoader().Load(context.Background(), config.Options{
		WorkDir: dir, HomeDir: home, Env: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAssembleOrderAndTail(t *testing.T) {
	cfg := testConfig(t)
	asm := NewAssembler(NewFileSource(cfg))
	req := Request{
		Node:    model.NodeCode,
		Mode:    model.ModeCode,
		Project: ProjectDoc{Body: "项目上下文正文", Hash: "sha256:abc"},
		Tools: []model.ToolSpec{
			{Name: "write", Description: "写文件", Mutating: true},
			{Name: "read", Description: "读文件"},
		},
		Summary: "旧会话摘要",
		Recent: []model.Message{
			{Role: model.RoleUser, Content: "上一轮问题"},
			{Role: model.RoleAssistant, Content: "上一轮回答"},
			{Role: model.RoleUser, Content: "本轮问题"},
		},
		Runtime: RuntimeInfo{CWD: "/w", OS: "darwin", Shell: "sh -c", Date: "2026-10-30", Approval: "risk-based"},
	}
	bundle, err := asm.Assemble(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Messages) < 3 {
		t.Fatalf("messages = %d", len(bundle.Messages))
	}
	first := bundle.Messages[0]
	if first.Role != model.RoleSystem {
		t.Fatalf("first role = %s", first.Role)
	}
	for _, want := range []string{"覆盖后的 code prompt", "模式：code", "项目约定：先读代码", "项目上下文正文", "旧会话摘要", "write", "read"} {
		if !strings.Contains(first.Content, want) {
			t.Fatalf("stable prefix missing %q:\n%s", want, first.Content)
		}
	}
	// 工具按名称排序。
	if strings.Index(first.Content, "- read") > strings.Index(first.Content, "- write") {
		t.Fatal("tools must be sorted by name")
	}
	last := bundle.Messages[len(bundle.Messages)-1]
	if last.Role != model.RoleUser || last.Content != "本轮问题" {
		t.Fatalf("last message = %+v", last)
	}
	tail := bundle.Messages[len(bundle.Messages)-2]
	if tail.Role != model.RoleSystem || !strings.Contains(tail.Content, "date: 2026-10-30") {
		t.Fatalf("runtime tail = %+v", tail)
	}
	if bundle.Meta.NodePromptVersion == "" || bundle.Meta.StableHash == "" || bundle.Meta.PromptHash == "" || bundle.Meta.Tokens <= 0 {
		t.Fatalf("meta incomplete: %+v", bundle.Meta)
	}
}

func TestStableHashIgnoresRuntime(t *testing.T) {
	cfg := testConfig(t)
	asm := NewAssembler(NewFileSource(cfg))
	base := Request{
		Node:    model.NodeAsk,
		Mode:    model.ModeAsk,
		Recent:  []model.Message{{Role: model.RoleUser, Content: "你好"}},
		Runtime: RuntimeInfo{CWD: "/w", Date: "2026-10-30"},
	}
	a, err := asm.Assemble(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	base.Runtime.Date = "2026-10-31"
	base.Runtime.CWD = "/other"
	b, err := asm.Assemble(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if a.Meta.StableHash != b.Meta.StableHash {
		t.Fatal("runtime info must not affect stable prefix hash")
	}
	if a.Meta.PromptHash == b.Meta.PromptHash {
		t.Fatal("runtime info should change full prompt hash")
	}
}

func TestModeOverlayCannotBeOverridden(t *testing.T) {
	cfg := testConfig(t)
	// 即使创建 modes 覆盖文件，也不应生效（D13）。
	if err := os.MkdirAll(filepath.Join(cfg.WorkDir(), ".ant", "prompts", "modes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkDir(), ".ant", "prompts", "modes", "code.md"), []byte("恶意覆盖"), 0o600); err != nil {
		t.Fatal(err)
	}
	asm := NewAssembler(NewFileSource(cfg))
	bundle, err := asm.Assemble(context.Background(), Request{Node: model.NodeCode, Mode: model.ModeCode})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bundle.Messages[0].Content, "恶意覆盖") {
		t.Fatal("mode overlay must not be overridable")
	}
	if !strings.Contains(bundle.Messages[0].Content, "状态机流程") {
		t.Fatal("builtin mode overlay missing")
	}
}
