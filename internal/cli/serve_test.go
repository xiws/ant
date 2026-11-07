package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestParseServeOptions(t *testing.T) {
	opts, err := parseArgs([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "serve" || opts.listen != "127.0.0.1:7788" || opts.open {
		t.Fatalf("defaults = %+v", opts)
	}

	opts, err = parseArgs([]string{"serve", "--listen", "127.0.0.1:0", "--open"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.listen != "127.0.0.1:0" || !opts.open {
		t.Fatalf("flags = %+v", opts)
	}

	for _, args := range [][]string{
		{"serve", "do something"},
		{"serve", "--json"},
		{"serve", "-s", "20261106-234456-df479f"},
		{"serve", "--new"},
		{"serve", "-p", "alpha"},
		{"serve", "-m", "ask"},
		{"serve", "--listen", "0.0.0.0:7788"},
		{"serve", "--listen", "::1:7788"},
		{"serve", "--listen", "example.com:7788"},
		{"serve", "--listen", "127.0.0.1:99999"},
		{"serve", "--listen", "127.0.0.1"},
		{"serve", "--listen", ":7788"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("parseArgs(%v) should fail", args)
		} else if !isUsageError(err) {
			t.Fatalf("parseArgs(%v) error = %v, want usage error", args, err)
		}
	}

	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:7788", "localhost:8080", "[::1]:8080"} {
		if err := validateListenAddr(addr); err != nil {
			t.Fatalf("validateListenAddr(%q) = %v", addr, err)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST.", "127.0.0.1", "127.0.0.2", "::1", "[::1]"} {
		if !isLoopbackHost(host) {
			t.Fatalf("isLoopbackHost(%q) = false", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "example.com", "192.168.1.1", "10.0.0.1"} {
		if isLoopbackHost(host) {
			t.Fatalf("isLoopbackHost(%q) = true", host)
		}
	}
}

func newServeTestApp(t *testing.T, work, home string) (*App, *safeBuffer, *safeBuffer) {
	t.Helper()
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	app, err := New(context.Background(), []string{"serve", "--listen", "127.0.0.1:0"}, Stdio{
		In:    strings.NewReader(""),
		Out:   out,
		Err:   errOut,
		IsTTY: false,
		Env:   []string{"ANT_HOME=" + home},
	}, Overrides{WorkDir: work, HomeDir: home, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return app, out, errOut
}

func waitForServeOutput(t *testing.T, out *safeBuffer, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text := out.String()
		if strings.Contains(text, "serve: http://") {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for serve output; got %q", out.String())
	return ""
}

func TestServeStartsAndStopsGracefully(t *testing.T) {
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	app, out, errOut := newServeTestApp(t, work, home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- app.Run(ctx) }()

	text := waitForServeOutput(t, out, 5*time.Second)
	if !strings.Contains(text, "workspace: "+work) {
		t.Fatalf("output missing workspace: %q", text)
	}
	if !strings.Contains(text, "只读模式") || !strings.Contains(text, "无需登录") {
		t.Fatalf("output missing readonly hint: %q", text)
	}
	if !strings.Contains(text, "home:      "+home) {
		t.Fatalf("output missing home: %q", text)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("graceful stop code = %d, want 0; stderr=%q", code, errOut.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serve did not stop after context cancel")
	}
	if _, err := os.Stat(filepath.Join(work, ".ant", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("ant serve must not create .ant/sessions (stat err = %v)", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestServeListensOnLoopbackOnly(t *testing.T) {
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	app, out, _ := newServeTestApp(t, work, home)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- app.Run(ctx) }()

	text := waitForServeOutput(t, out, 5*time.Second)
	line := ""
	for _, candidate := range strings.Split(text, "\n") {
		if strings.Contains(candidate, "serve: http://") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "[ant] serve: "))
			break
		}
	}
	if !strings.HasPrefix(line, "http://127.0.0.1:") {
		t.Fatalf("URL = %q, want loopback", line)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(line, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port == "0" {
		t.Fatalf("listener = %s:%s", host, port)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("listener ip = %v", ip)
	}
	cancel()
	<-done
}

func TestServeStartsWithBadConfig(t *testing.T) {
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(filepath.Join(work, ".ant"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".ant", "settings.json"), []byte(`{"limits": "not-an-object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	app, out, errOut := newServeTestApp(t, work, home)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- app.Run(ctx) }()

	waitForServeOutput(t, out, 5*time.Second)
	if !strings.Contains(errOut.String(), "配置告警") {
		t.Fatalf("expected config warning on stderr, got %q", errOut.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestRunServeExitCodeZeroOnCancel(t *testing.T) {
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"serve", "--listen", "127.0.0.1:0"}, Stdio{
			In:    strings.NewReader(""),
			Out:   out,
			Err:   errOut,
			IsTTY: false,
			Env:   append(os.Environ(), "ANT_HOME="+home),
		})
	}()
	waitForServeOutput(t, out, 5*time.Second)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Run(serve) exit code = %d, want 0 (stderr=%q)", code, errOut.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run(serve) did not stop")
	}
	if _, err := os.Stat(filepath.Join(work, ".ant", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("serve must not create .ant/sessions (stat err = %v)", err)
	}
}
