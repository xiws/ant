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

func runReadTurn(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	settings := `{
		"schemaVersion": 1,
		"defaultModel": "fake-model",
		"defaultMode": "ask",
		"project": {"autoInit": false},
		"log": {"level": "error"}
	}`
	wd, home := setupWorkspace(t, settings)
	if err := os.WriteFile(filepath.Join(wd, "note.txt"), []byte("hello from tool output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := fake.New().
		QueueToolCalls(model.ToolCall{
			ID:        "call_read",
			Name:      "read",
			Arguments: json.RawMessage(`{"path":"note.txt"}`),
		}).
		QueueText("done")
	return runApp(t, args, wd, home, fp, &captureSink{})
}

func TestVerboseShowsToolResultContent(t *testing.T) {
	code, out, errOut := runReadTurn(t, []string{"new", "-m", "ask", "--verbose", "read note"})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	for _, want := range []string{
		"[node] ask",
		"[req] 请求 #1",
		"[resp] 响应 #1",
		"[tool] read",
		"hello from tool output",
		"[result] read",
		"完整消息：",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, out)
		}
	}
}

func TestNormalOutputHidesToolResultContent(t *testing.T) {
	code, out, errOut := runReadTurn(t, []string{"new", "-m", "ask", "read note"})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if strings.Contains(out, "hello from tool output") {
		t.Fatalf("normal output leaked tool result content:\n%s", out)
	}
	if !strings.Contains(out, "[tool] read") {
		t.Fatalf("normal tool trace missing:\n%s", out)
	}
}

func TestVerboseIgnoredInJSONMode(t *testing.T) {
	code, out, errOut := runReadTurn(t, []string{"new", "-m", "ask", "--json", "--verbose", "read note"})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if strings.Contains(out, "hello from tool output") {
		t.Fatalf("--json stdout must stay NDJSON:\n%s", out)
	}
	if strings.Contains(out, "[node]") || strings.Contains(out, "[req]") {
		t.Fatalf("--json stdout must not contain verbose trace:\n%s", out)
	}
}

func TestVerboseSettingAndNoVerboseFlag(t *testing.T) {
	settings := `{
		"schemaVersion": 1,
		"defaultModel": "fake-model",
		"defaultMode": "ask",
		"ui": {"verbose": true},
		"project": {"autoInit": false},
		"log": {"level": "error"}
	}`
	wd, home := setupWorkspace(t, settings)
	if err := os.WriteFile(filepath.Join(wd, "note.txt"), []byte("hello from tool output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := fake.New().
		QueueToolCalls(model.ToolCall{
			ID: "call_read", Name: "read", Arguments: json.RawMessage(`{"path":"note.txt"}`),
		}).
		QueueText("done")
	code, out, errOut := runApp(t, []string{"new", "-m", "ask", "read note"}, wd, home, fp, &captureSink{})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "hello from tool output") {
		t.Fatalf("ui.verbose should enable detailed output")
	}

	fp = fake.New().
		QueueToolCalls(model.ToolCall{
			ID: "call_read", Name: "read", Arguments: json.RawMessage(`{"path":"note.txt"}`),
		}).
		QueueText("done")
	code, out, errOut = runApp(t, []string{"new", "-m", "ask", "--no-verbose", "read note"}, wd, home, fp, &captureSink{})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if strings.Contains(out, "hello from tool output") {
		t.Fatalf("--no-verbose should suppress detailed output")
	}
}
