package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider/fake"
)

func latestSessionID(t *testing.T, wd string) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(wd, ".ant", "sessions", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("session dirs: %v %v", dirs, err)
	}
	return filepath.Base(dirs[0])
}

func setupReadSession(t *testing.T, name, content string) (wd, home, id string, out, errOut string) {
	t.Helper()
	settings := `{
		"schemaVersion": 1,
		"defaultModel": "fake-model",
		"defaultMode": "ask",
		"project": {"autoInit": false},
		"log": {"level": "error"}
	}`
	wd, home = setupWorkspace(t, settings)
	if err := os.WriteFile(filepath.Join(wd, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := fake.New().
		QueueToolCalls(model.ToolCall{
			ID:        "call_read",
			Name:      "read",
			Arguments: json.RawMessage(`{"path":"` + name + `"}`),
		}).
		QueueText("最终回答")
	code, out, errOut := runApp(t, []string{"new", "-m", "ask", "read " + name}, wd, home, fp, &captureSink{})
	if code != 0 {
		t.Fatalf("turn exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	return wd, home, latestSessionID(t, wd), out, errOut
}

func TestExportMarkdownContainsFullTrace(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "note.txt", "hello from tool output\n")

	code, out, errOut := runApp(t, []string{"export", id, "--format", "md", "-o", "-"}, wd, home, nil, &captureSink{})
	if code != 0 {
		t.Fatalf("export exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	for _, want := range []string{
		"# ant session trace",
		"## Turn 1",
		"### 节点 ask",
		"有效 Prompt / 消息",
		"节点：ask",
		"工具调用 read",
		"hello from tool output",
		"最终回答",
		"usage：",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown export missing %q:\n%s", want, out)
		}
	}
}

func TestExportHTMLIsSelfContainedAndEscaped(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "x.txt", "<script>alert(1)</script>\n")

	code, out, errOut := runApp(t, []string{"export", id, "--format", "html", "-o", "-"}, wd, home, nil, &captureSink{})
	if code != 0 {
		t.Fatalf("export exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "<!doctype html>") || !strings.Contains(out, "<details>") {
		t.Fatalf("html export missing structure")
	}
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Fatalf("tool output must not appear as raw HTML")
	}
	if !strings.Contains(out, "&lt;!-- version: 1 --&gt;") {
		t.Fatalf("prompt content should be HTML-escaped")
	}
}

func TestExportDefaultPath(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "note.txt", "hello from tool output\n")

	code, out, errOut := runApp(t, []string{"export", id}, wd, home, nil, &captureSink{})
	if code != 0 {
		t.Fatalf("export exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	path := filepath.Join(wd, ".ant", "sessions", id, "trace.html")
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("default export missing: %v", err)
	}
	if !strings.Contains(out, "已导出：") {
		t.Fatalf("export path not printed: %s", out)
	}
}

func TestExportRejectsJSON(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "note.txt", "hello\n")

	code, _, errOut := runApp(t, []string{"export", id, "--json"}, wd, home, nil, &captureSink{})
	if code != 2 {
		t.Fatalf("export --json exit=%d stderr=%s", code, errOut)
	}
}

func TestExportInfersFormatFromOutputExtension(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "note.txt", "hello\n")
	outPath := filepath.Join(wd, "reports", "trace.md")
	code, _, errOut := runApp(t, []string{"export", id, "-o", outPath}, wd, home, nil, &captureSink{})
	if code != 0 {
		t.Fatalf("export exit=%d stderr=%s", code, errOut)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "# ant session trace") {
		t.Fatalf("expected markdown based on .md extension, got:\n%s", string(body[:min(80, len(body))]))
	}
}

func TestExportIncludesRawEvents(t *testing.T) {
	wd, home, id, _, _ := setupReadSession(t, "note.txt", "hello\n")
	code, out, errOut := runApp(t, []string{"export", id, "--format", "md", "--events", "-o", "-"}, wd, home, nil, &captureSink{})
	if code != 0 {
		t.Fatalf("export exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "## 原始事件") || !strings.Contains(out, `"type":"turn_completed"`) {
		t.Fatalf("raw events missing")
	}
}

func TestExportUnknownSession(t *testing.T) {
	wd, home, _, _, _ := setupReadSession(t, "note.txt", "hello\n")
	code, _, errOut := runApp(t, []string{"export", "no-such-session"}, wd, home, nil, &captureSink{})
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
}
