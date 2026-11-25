package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/provider/fake"
)

func runTTYTurn(t *testing.T, args []string, text string) (string, string) {
	t.Helper()
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	fp := fake.New().QueueText(text)
	var out, errOut bytes.Buffer
	app, err := New(context.Background(), args, Stdio{
		In: strings.NewReader(""), Out: &out, Err: &errOut, IsTTY: true, Env: []string{},
	}, Overrides{Provider: fp, WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if code := app.Run(context.Background()); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.String(), errOut.String()
}

func TestTTYTurnRendersMarkdownAndBlocks(t *testing.T) {
	out, _ := runTTYTurn(t, []string{"new", "-m", "ask", "hi"}, "# 标题\n\n正文\n")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("TTY output should contain ANSI styles: %q", out)
	}
	if !strings.Contains(out, "▌") || !strings.Contains(out, "完成") {
		t.Fatalf("summary block missing: %q", out)
	}
	if strings.Contains(out, "# 标题") {
		t.Fatalf("markdown source leaked into rich output: %q", out)
	}
}

func TestNoColorFallsBackToRawMarkdown(t *testing.T) {
	out, _ := runTTYTurn(t, []string{"new", "-m", "ask", "--no-color", "hi"}, "# 标题\n\n正文\n")
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("--no-color must not emit ANSI: %q", out)
	}
	if !strings.Contains(out, "# 标题") || !strings.Contains(out, "[ant] 完成") {
		t.Fatalf("plain fallback missing raw markdown/[ant]: %q", out)
	}
}
