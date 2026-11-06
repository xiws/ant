package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/config"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*(?:\x07|\x1b\\)`)

const (
	bashCaptureLimit      = 1 << 20
	bashKillGrace         = 2 * time.Second
	defaultBashTimeout    = 120 * time.Second
	defaultBashMaxTimeout = 600 * time.Second
)

// bashExec 实现 §10.10 bash。
func bashExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	settings := settingsOf(cfg)
	workDir := workspaceDir(cfg)
	defaultTimeout := msDuration(settings.Limits.BashTimeoutMs, int(defaultBashTimeout/time.Millisecond))
	if defaultTimeout <= 0 {
		defaultTimeout = defaultBashTimeout
	}
	maxTimeout := msDuration(settings.Limits.BashMaxTimeoutMs, int(defaultBashMaxTimeout/time.Millisecond))
	if maxTimeout <= 0 {
		maxTimeout = defaultBashMaxTimeout
	}
	secretEnv := secretEnvNames(cfg)
	return func(ctx context.Context, inv Invocation) (Result, error) {
		command, _ := inv.Args["command"].(string)
		if strings.TrimSpace(command) == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 command", "提供要执行的命令", nil)}, nil
		}
		dir := workDir
		if cwd, _ := inv.Args["cwd"].(string); strings.TrimSpace(cwd) != "" {
			res, err := deps.Guard.Resolve(ctx, cwd, AccessRead)
			if err != nil {
				return Result{OK: false, Err: pathError(err)}, nil
			}
			rel, relErr := filepath.Rel(workDir, res.Abs)
			if res.Outside || relErr != nil || isOutsideRel(rel) {
				return Result{OK: false, Err: &ToolError{
					Code:    "path_outside_workspace",
					Message: fmt.Sprintf("cwd %s 不在 workspace 内", cwd),
					Hint:    "bash 的 cwd 必须位于 workspace 内",
					Details: map[string]any{"cwd": cwd, "workspace": workDir},
				}}, nil
			}
			info, err := os.Stat(res.Abs)
			if err != nil || !info.IsDir() {
				return Result{OK: false, Err: &ToolError{
					Code:    "not_found",
					Message: fmt.Sprintf("cwd %s 不存在或不是目录", cwd),
					Hint:    "使用 workspace 内已存在的目录",
					Details: map[string]any{"cwd": cwd},
				}}, nil
			}
			dir = res.Abs
		}

		timeout := defaultTimeout
		if n := intArg(inv.Args, "timeoutMs", 0); n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
		parent := ctx
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		name, args := shellCommand(command, settings.Sandbox.Shell)
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = sanitizeEnv(os.Environ(), secretEnv)
		cmd.Stdin = nil
		setProcessGroup(cmd)

		outBuf := newLimitedBuffer(bashCaptureLimit)
		errBuf := newLimitedBuffer(bashCaptureLimit)
		cmd.Stdout = outBuf
		cmd.Stderr = errBuf

		start := time.Now()
		if err := cmd.Start(); err != nil {
			return Result{OK: false, Err: &ToolError{
				Code:    "command_failed",
				Message: fmt.Sprintf("命令无法启动：%v", err),
				Hint:    "检查 sandbox.shell 是否存在、命令参数是否合法",
				Details: map[string]any{"command": command, "shell": name},
			}}, nil
		}

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		var (
			waitErr  error
			timedOut bool
		)
		select {
		case waitErr = <-done:
		case <-runCtx.Done():
			terminateProcess(cmd)
			select {
			case waitErr = <-done:
			case <-time.After(bashKillGrace):
				killProcessTree(cmd)
				waitErr = <-done
			}
			if errors.Is(parent.Err(), context.Canceled) {
				return Result{}, parent.Err()
			}
			// 单次工具超时（父 ctx  deadline）与命令 timeoutMs 都按“已捕获输出 + timedOut”返回；
			// 只有用户取消才算 interrupted（§10.10）。
			timedOut = errors.Is(parent.Err(), context.DeadlineExceeded) ||
				errors.Is(runCtx.Err(), context.DeadlineExceeded)
		}
		duration := time.Since(start)

		exitCode := 0
		if waitErr != nil {
			var exit *exec.ExitError
			if errors.As(waitErr, &exit) {
				exitCode = exit.ExitCode()
			} else if !timedOut {
				return Result{OK: false, Err: &ToolError{
					Code:    "command_failed",
					Message: fmt.Sprintf("命令执行失败：%v", waitErr),
					Hint:    "检查命令是否存在、是否有执行权限",
					Details: map[string]any{"command": command},
				}}, nil
			}
		}
		if timedOut {
			exitCode = -1
		}
		stdout := sanitizeOutput(outBuf.String())
		stderr := sanitizeOutput(errBuf.String())
		outputRef := ""
		if deps.Store != nil {
			artifactCtx := ctx
			if artifactCtx.Err() != nil {
				artifactCtx = context.WithoutCancel(ctx)
			}
			var log strings.Builder
			if stdout != "" {
				log.WriteString(stdout)
			}
			if stderr != "" {
				if log.Len() > 0 && !strings.HasSuffix(log.String(), "\n") {
					log.WriteString("\n")
				}
				log.WriteString(stderr)
			}
			ref, err := deps.Store.PutArtifact(artifactCtx, "artifacts/bash-"+sanitizeArtifactID(inv.CallID)+".log",
				[]byte(log.String()), "text/plain; charset=utf-8")
			if err != nil {
				return Result{OK: false, Err: &ToolError{
					Code:    "internal",
					Message: "写入 bash 输出 artifact 失败：" + err.Error(),
					Hint:    "检查 session artifact 目录权限与磁盘空间",
				}}, nil
			}
			outputRef = ref.Name
		}
		hint := ""
		if timedOut {
			hint = fmt.Sprintf("命令在 %s 后超时并被终止；可缩小任务范围或调大 limits.bashTimeoutMs", timeout)
			stderr = strings.TrimRight(stderr, "\n")
			if stderr != "" {
				stderr += "\n"
			}
			stderr += "\n[ant] 命令超时，已终止进程组"
		}
		if outBuf.truncated || errBuf.truncated {
			hint = strings.TrimSpace(hint + " 输出超过捕获上限，已截断")
		}
		return Result{
			OK: true,
			Data: map[string]any{
				"command":    command,
				"cwd":        dir,
				"exitCode":   exitCode,
				"stdout":     stdout,
				"stderr":     stderr,
				"durationMs": duration.Milliseconds(),
				"timedOut":   timedOut,
				"truncated":  outBuf.truncated || errBuf.truncated,
				"outputRef":  outputRef,
				"hint":       hint,
			},
			Meta: ResultMeta{ExitCode: exitCode, Duration: duration},
		}, nil
	}
}

func shellCommand(command, override string) (string, []string) {
	override = strings.TrimSpace(override)
	if override != "" {
		fields := strings.Fields(override)
		if len(fields) == 1 {
			if runtime.GOOS == "windows" {
				return fields[0], []string{"/C", command}
			}
			return fields[0], []string{"-c", command}
		}
		if len(fields) > 1 {
			return fields[0], append(fields[1:], command)
		}
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

func secretEnvNames(cfg config.View) map[string]bool {
	names := map[string]bool{}
	if cfg == nil {
		return names
	}
	for _, p := range cfg.Providers() {
		if p.APIKeyEnv != "" {
			names[p.APIKeyEnv] = true
		}
	}
	return names
}

// sanitizeEnv 继承父进程环境，但移除 provider 密钥与 ANT_* 内部变量（§12.7）。
func sanitizeEnv(env []string, secret map[string]bool) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key := item
		if i := strings.IndexByte(item, '='); i >= 0 {
			key = item[:i]
		}
		if strings.HasPrefix(key, "ANT_") || secret[key] {
			continue
		}
		// 常见 provider key 变量也统一剥离，避免模型通过环境读取。
		upper := strings.ToUpper(key)
		if strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_APIKEY") ||
			upper == "OPENAI_API_KEY" || upper == "ANTHROPIC_API_KEY" ||
			upper == "DEEPSEEK_API_KEY" || upper == "GEMINI_API_KEY" {
			continue
		}
		out = append(out, item)
	}
	return out
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{limit: limit} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return n, nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return n, nil
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func sanitizeCWD(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
