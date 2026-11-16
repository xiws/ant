package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/model"
)

// TestCommandTouchesOutside 锁定 §12.3「bash 越界路径」的判定：只有真正逃出
// workspace 的路径才触发 bash_outside，workspace 内的写法（含 `../` 归一化、
// `2>/dev/null` 重定向）必须放行。
func TestCommandTouchesOutside(t *testing.T) {
	ws := t.TempDir()
	elsewhere := t.TempDir()

	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// ---- workspace 内：不应触发 bash_outside ----
		{"相对路径", "go test ./internal/tool/", false},
		{"workspace 绝对路径", "cd " + ws + " && go build ./...", false},
		{"workspace 绝对路径带尾斜杠", "ls -la " + ws + "/", false},
		{"规范化后仍在 workspace 内", "cd internal/../pkg && ls", false},
		{"引号里的 ..", `echo "a/../b"`, false},
		{"引号内的 workspace 路径", `cd "` + ws + `" && go build ./...`, false},
		{"管道组合", "go test ./... 2>&1 | grep -v FAIL", false},
		{"重定向到 /dev/null", "go test ./... 2>/dev/null", false},
		{"stdout 重定向到 /dev/null", "echo hi 1>/dev/null", false},
		{"workspace 内文件 + /dev/null", "cat " + filepath.Join(ws, "README.md") + " 2>/dev/null", false},
		{"重定向到 /dev/fd", "echo hi >/dev/fd/2", false},
		{"转义的正则", `grep -rn "\.\./" docs`, false},
		{"token 尾部句点", "echo " + ws + ".", false},
		{"glob 参数", "go vet ./internal/[a-z]*/...", false},
		{"纯符号参数", "go build -v --all", false},
		{"选项取值在 workspace 内", "go build --out=" + filepath.Join(ws, "ant") + " ./cmd/ant", false},

		// ---- 真越界：必须触发 ----
		{"相对逃逸", "ls ../other-project", true},
		{"上级目录", "cd .. && ls", true},
		{"越界绝对路径", "cat " + filepath.Join(elsewhere, "x.txt"), true},
		{"系统路径", "cat /etc/hosts", true},
		{"家目录", "cat ~/.ssh/id_rsa", true},
		{"临时目录", "cp a.go /tmp/x.go", true},
		{"选项取值越界", "go build --out=/tmp/x ./...", true},
		{"短选项带越界路径", "gcc -I../include -c x.c", true},
		{"短选项带系统路径", "gcc -I/usr/include -c x.c", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := commandTouchesOutside(c.cmd, ws); got != c.want {
				t.Fatalf("commandTouchesOutside(%q, %q) = %v, want %v", c.cmd, ws, got, c.want)
			}
		})
	}
}

// TestCommandTouchesOutsideSymlinkedWorkspace 覆盖 workspace 被 EvalSymlinks 解析
// 后与命令里的拼法不一致的情况：两边指向同一个目录时不应触发审批。
func TestCommandTouchesOutsideSymlinkedWorkspace(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-project")
	if err := os.MkdirAll(filepath.Join(real, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "project-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("当前文件系统不支持符号链接：%v", err)
	}

	// workspace 是解析后的真实路径，命令写软链拼法。
	if commandTouchesOutside("cd "+link+" && go build ./...", real) {
		t.Fatalf("软链拼法的 workspace 内路径应放行")
	}
	// workspace 是软链拼法，命令写真实路径。
	if commandTouchesOutside("cd "+real+" && go build ./...", link) {
		t.Fatalf("真实路径相对软链 workspace 应放行")
	}
	// 指向同一目录的越界写仍然要触发。
	if !commandTouchesOutside("cp a.go "+filepath.Join(base, "outside.go"), real) {
		t.Fatalf("workspace 之外的路径必须触发审批")
	}
}

// TestCommandTouchesOutsideWithoutWorkDir 覆盖配置视图没有 workspace 时的降级启发式。
func TestCommandTouchesOutsideWithoutWorkDir(t *testing.T) {
	if !commandTouchesOutside("cat /etc/hosts", "") {
		t.Fatalf("系统路径应触发审批")
	}
	if commandTouchesOutside("ls -la /Users/someone/project", "") {
		t.Fatalf("普通用户目录不应在无 workspace 时硬判越界")
	}
	if commandTouchesOutside("echo hi 2>/dev/null", "") {
		t.Fatalf("/dev/null 重定向不应触发审批")
	}
	if !commandTouchesOutside("cd .. && ls", "") {
		t.Fatalf("没有 workspace 时相对逃逸仍应触发审批")
	}
}

// TestEffectiveApprovalBashOutside 从运行时入口验证：workspace 内的命令不产生审批
// 请求，越界命令才产生 bash_outside。
func TestEffectiveApprovalBashOutside(t *testing.T) {
	env := newTestEnv(t, nil)
	ws := env.view.WorkDir()
	executed := 0
	env.add(&stubTool{spec: looseSpec("bash", true, model.ApprovalBashOutside), exec: func(context.Context, Invocation) (Result, error) {
		executed++
		return Result{OK: true, Data: map[string]any{"ok": true}}, nil
	}})
	rt := env.runtime()
	calls := []model.ToolCall{
		{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./... 2>/dev/null"}`)},
		{ID: "c2", Name: "bash", Arguments: json.RawMessage(`{"command":"cd ` + ws + ` && go build ./..."}`)},
		{ID: "c3", Name: "bash", Arguments: json.RawMessage(`{"command":"cat /etc/hosts"}`)},
	}
	msgs, err := rt.ExecuteBatch(context.Background(), model.NodeCode, calls)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if len(msgs) != len(calls) {
		t.Fatalf("消息数=%d want %d", len(msgs), len(calls))
	}
	for i, msg := range msgs {
		if body := decodeMessage(t, msg); !body.OK {
			t.Fatalf("c%d 应直接执行：%s", i+1, msg.Content)
		}
	}

	reqs := env.approver.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("只有越界命令需要审批，实际请求 %d 次：%+v", len(reqs), reqs)
	}
	if reqs[0].Class != model.ApprovalBashOutside {
		t.Fatalf("审批类别=%q want %q", reqs[0].Class, model.ApprovalBashOutside)
	}
	if !strings.Contains(reqs[0].Command, "/etc/hosts") {
		t.Fatalf("审批命令=%q 应指向越界路径", reqs[0].Command)
	}
	if executed != len(calls) {
		t.Fatalf("执行次数=%d want %d", executed, len(calls))
	}
}
