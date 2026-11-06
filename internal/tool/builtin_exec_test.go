package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/readcode"
)

func writeWorkspaceFile(t *testing.T, env *testEnv, rel, body string) string {
	t.Helper()
	path := filepath.Join(env.view.WorkDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func execCall(t *testing.T, tk *Toolkit, node model.NodeName, id, name string, args any) toolMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := tk.Runtime.ExecuteBatch(context.Background(), node, []model.ToolCall{{ID: id, Name: name, Arguments: raw}})
	if err != nil {
		t.Fatalf("ExecuteBatch(%s): %v", name, err)
	}
	body := decodeMessage(t, msgs[0])
	return toolMessage{body: body, data: dataOrNil(t, body)}
}

type toolMessage struct {
	body decodedMessage
	data map[string]any
}

func dataOrNil(t *testing.T, body decodedMessage) map[string]any {
	t.Helper()
	if !body.OK || body.Data == nil {
		return nil
	}
	return dataMap(t, body.Data)
}

func TestReadToolLimitsAndBinary(t *testing.T) {
	env := newTestEnv(t, func(s *config.Settings) {
		s.Limits.MaxFileReadLines = 2
		s.Limits.MaxFileReadBytes = 22
	})
	writeWorkspaceFile(t, env, "a.txt", "1\n2\n3\n4\n5\n")
	tk := NewToolkit(env.view, env.deps())

	got := execCall(t, tk, model.NodeCode, "c1", "read", map[string]any{"path": "a.txt"})
	if !got.body.OK {
		t.Fatalf("read failed: %s", got.body.Error)
	}
	if got.data["startLine"] != float64(1) || got.data["endLine"] != float64(2) || got.data["truncated"] != true {
		t.Fatalf("read data = %v", got.data)
	}
	if got.data["nextStartLine"] != float64(3) {
		t.Fatalf("nextStartLine = %v", got.data["nextStartLine"])
	}
	if !strings.Contains(got.data["content"].(string), "1\t1") {
		t.Fatalf("content 缺少行号: %q", got.data["content"])
	}

	writeWorkspaceFile(t, env, "bin.dat", "abc\x00def")
	got = execCall(t, tk, model.NodeCode, "c2", "read", map[string]any{"path": "bin.dat"})
	if got.body.OK || got.body.Error == nil || got.body.Error.Code != "binary_file" {
		t.Fatalf("binary read = %+v", got)
	}

	got = execCall(t, tk, model.NodeCode, "c3", "read", map[string]any{"path": "missing.txt"})
	if got.body.Error == nil || got.body.Error.Code != "not_found" {
		t.Fatalf("missing read = %+v", got)
	}
}

func TestWriteToolRequiresReadAndUpdatesVersion(t *testing.T) {
	env := newTestEnv(t, nil)
	writeWorkspaceFile(t, env, "a.txt", "old\n")
	deps := env.deps()
	tk := NewToolkit(env.view, deps)

	got := execCall(t, tk, model.NodeCode, "w1", "write", map[string]any{"path": "a.txt", "content": "new\n"})
	if got.body.Error == nil || got.body.Error.Code != "hash_mismatch" {
		t.Fatalf("盲写应被拒绝: %+v", got)
	}

	read := execCall(t, tk, model.NodeCode, "r1", "read", map[string]any{"path": "a.txt"})
	if !read.body.OK {
		t.Fatalf("read: %s", read.body.Error)
	}
	sha := read.data["sha256"].(string)
	got = execCall(t, tk, model.NodeCode, "w2", "write", map[string]any{"path": "a.txt", "content": "new\n", "expectedHash": sha})
	if !got.body.OK || got.data["created"] != false {
		t.Fatalf("write = %+v", got)
	}
	body, _ := os.ReadFile(filepath.Join(env.view.WorkDir(), "a.txt"))
	if string(body) != "new\n" {
		t.Fatalf("file = %q", body)
	}

	// 新建文件不需要先读。
	got = execCall(t, tk, model.NodeCode, "w3", "write", map[string]any{"path": "sub/new.txt", "content": "x\n"})
	if !got.body.OK || got.data["created"] != true {
		t.Fatalf("create = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(env.view.WorkDir(), "sub", "new.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestEditToolAppliesAndRefusesPartial(t *testing.T) {
	env := newTestEnv(t, nil)
	path := writeWorkspaceFile(t, env, "a.txt", "line1\nline2\nline3\n")
	deps := env.deps()
	tk := NewToolkit(env.view, deps)

	if read := execCall(t, tk, model.NodeCode, "r1", "read", map[string]any{"path": "a.txt"}); !read.body.OK {
		t.Fatalf("read: %s", read.body.Error)
	}
	diff := "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n+changed\n line3\n"
	got := execCall(t, tk, model.NodeCode, "e1", "edit", map[string]any{"path": "a.txt", "diff": diff})
	if !got.body.OK || got.data["applied"] != float64(1) {
		t.Fatalf("edit: %+v", got.body.Error)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "line1\nchanged\nline3\n" {
		t.Fatalf("file = %q", body)
	}

	bad := "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n line1\n-changed\n+changed2\n@@ -20,2 +20,2 @@\n nope\n-x\n+y\n"
	got = execCall(t, tk, model.NodeCode, "e2", "edit", map[string]any{"path": "a.txt", "diff": bad, "expectedHash": shaOf(body)})
	if got.body.Error == nil || got.body.Error.Code != "partial_hunks" {
		t.Fatalf("partial edit = %+v", got)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "line1\nchanged\nline3\n" {
		t.Fatalf("部分匹配不应落盘: %q", after)
	}

	got = execCall(t, tk, model.NodeCode, "e3", "edit", map[string]any{"path": "a.txt", "diff": "no hunks", "expectedHash": shaOf(body)})
	if got.body.Error == nil || got.body.Error.Code != "no_hunks" {
		t.Fatalf("no_hunks = %+v", got)
	}
}

func TestSearchToolLiteralRegexAndGitignore(t *testing.T) {
	env := newTestEnv(t, nil)
	writeWorkspaceFile(t, env, ".gitignore", "*.log\nbuild/\n")
	writeWorkspaceFile(t, env, "pkg/a.go", "func Alpha() {}\nvar beta = 1\n")
	writeWorkspaceFile(t, env, "pkg/b.go", "func Gamma() {}\n")
	writeWorkspaceFile(t, env, "debug.log", "Alpha\n")
	writeWorkspaceFile(t, env, "build/out.txt", "Alpha\n")
	tk := NewToolkit(env.view, env.deps())

	got := execCall(t, tk, model.NodeCode, "s1", "search", map[string]any{"query": "Alpha"})
	if !got.body.OK || got.data["matchCount"] != float64(1) {
		t.Fatalf("search = %+v", got)
	}
	if strings.Contains(got.data["content"].(string), "debug.log") || strings.Contains(got.data["content"].(string), "build/") {
		t.Fatalf("gitignore 未生效: %q", got.data["content"])
	}
	if !strings.Contains(got.data["content"].(string), "pkg/a.go:1:") {
		t.Fatalf("搜索结果格式: %q", got.data["content"])
	}

	got = execCall(t, tk, model.NodeCode, "s2", "search", map[string]any{"query": "func [A-Z]", "regex": true})
	if !got.body.OK || got.data["matchCount"] != float64(2) {
		t.Fatalf("regex search = %+v", got)
	}
	got = execCall(t, tk, model.NodeCode, "s3", "search", map[string]any{"query": "Alpha", "glob": "*.go"})
	if got.data["matchCount"] != float64(1) {
		t.Fatalf("glob search = %+v", got)
	}
}

func TestBashToolExitCodeTimeoutAndSafety(t *testing.T) {
	env := newTestEnv(t, func(s *config.Settings) {
		s.Limits.BashTimeoutMs = 100
		s.Limits.BashMaxTimeoutMs = 1000
		s.Sandbox.Network = "deny"
	})
	tk := NewToolkit(env.view, env.deps())

	got := execCall(t, tk, model.NodeCode, "b1", "bash", map[string]any{"command": "printf hello; printf err >&2; exit 3"})
	if !got.body.OK || got.data["exitCode"] != float64(3) || got.data["stdout"] != "hello" || got.data["stderr"] != "err" {
		t.Fatalf("bash = %+v", got)
	}

	start := time.Now()
	got = execCall(t, tk, model.NodeCode, "b2", "bash", map[string]any{"command": "sleep 5", "timeoutMs": 40})
	if !got.body.OK || got.data["timedOut"] != true || got.data["exitCode"] != float64(-1) {
		t.Fatalf("timeout bash = %+v", got)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("超时命令未及时终止")
	}

	got = execCall(t, tk, model.NodeCode, "b3", "bash", map[string]any{"command": "rm -rf /"})
	if got.body.Error == nil || got.body.Error.Code != "approval_denied" {
		t.Fatalf("硬拒绝 = %+v", got)
	}
	got = execCall(t, tk, model.NodeCode, "b4", "bash", map[string]any{"command": "curl example.com"})
	if got.body.Error == nil || got.body.Error.Code != "approval_denied" {
		t.Fatalf("网络 deny = %+v", got)
	}
}

func TestRuntimeUnwrapsQuotedArguments(t *testing.T) {
	env := newTestEnv(t, nil)
	writeWorkspaceFile(t, env, "a.txt", "ok\n")
	tk := NewToolkit(env.view, env.deps())
	quoted, _ := json.Marshal(`{"path":"a.txt"}`)
	msgs, err := tk.Runtime.ExecuteBatch(context.Background(), model.NodeCode, []model.ToolCall{
		{ID: "c1", Name: "read", Arguments: quoted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := decodeMessage(t, msgs[0]); !body.OK {
		t.Fatalf("quoted args 未解开: %s", msgs[0].Content)
	}
}

func TestReadCodeToolFormatsClosure(t *testing.T) {
	env := newTestEnv(t, nil)
	writeWorkspaceFile(t, env, "go.mod", "module example.com/demo\n\ngo 1.26\n")
	writeWorkspaceFile(t, env, "demo.go", "package demo\n\nfunc A() { B() }\n\nfunc B() { C() }\n\nfunc C() {}\n")
	deps := env.deps()
	deps.Index = readcode.NewIndex(env.view)
	tk := NewToolkit(env.view, deps)

	got := execCall(t, tk, model.NodeCode, "rc1", "read_code", map[string]any{"target": "A", "depth": 1})
	if !got.body.OK {
		t.Fatalf("read_code = %+v", got)
	}
	text := got.data["content"].(string)
	if !strings.Contains(text, "func A()") || !strings.Contains(text, "func B()") {
		t.Fatalf("缺少依赖切片: %s", text)
	}
	if strings.Contains(text, "func C()") {
		t.Fatalf("depth=1 不应包含 C: %s", text)
	}
}
