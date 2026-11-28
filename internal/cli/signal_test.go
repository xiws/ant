package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
	"github.com/xiws/ant/internal/provider/fake"
)

type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{})}
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Capabilities(string) (model.Capabilities, error) {
	return model.Capabilities{
		ContextWindow:     1_000_000,
		MaxOutputTokens:   4096,
		SupportsTools:     true,
		SupportsStreaming: true,
	}, nil
}

func (p *blockingProvider) Chat(ctx context.Context, _ provider.ChatRequest, _ func(provider.Delta) error) (provider.ChatResponse, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return provider.ChatResponse{}, ctx.Err()
}

func TestREPLCtrlCAtPromptExits(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	pr, pw := io.Pipe()
	defer pw.Close()
	var out, errOut bytes.Buffer
	app, err := New(context.Background(), []string{"new", "-m", "ask"}, Stdio{
		In: pr, Out: &out, Err: &errOut, IsTTY: true, Env: []string{},
	}, Overrides{Provider: fake.New(), WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	app.sigCh = make(chan os.Signal, 1)

	done := make(chan int, 1)
	go func() { done <- app.Run(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	app.sigCh <- os.Interrupt

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstderr=%s", code, errOut.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C at prompt did not exit")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestREPLKeepsRunningAfterTurnInterrupt(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)
	pr, pw := io.Pipe()
	defer pw.Close()
	fp := newBlockingProvider()
	var out, errOut bytes.Buffer
	app, err := New(context.Background(), []string{"new", "-m", "ask"}, Stdio{
		In: pr, Out: &out, Err: &errOut, IsTTY: true, Env: []string{},
	}, Overrides{Provider: fp, WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	app.sigCh = make(chan os.Signal, 2)

	done := make(chan int, 1)
	go func() { done <- app.Run(context.Background()) }()
	if _, err := pw.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	select {
	case <-fp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	app.sigCh <- os.Interrupt
	// 第一下 Ctrl-C 只应中断当前 turn；随后 REPL 应仍可读取下一条命令。
	time.Sleep(100 * time.Millisecond)
	if _, err := pw.Write([]byte("/exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", code, out.String(), errOut.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("REPL did not continue after turn interrupt")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBareTTYEnterREPLWithoutAutoResumingFailedTurn(t *testing.T) {
	settings := `{"schemaVersion":1,"defaultModel":"fake-model","defaultMode":"ask","project":{"autoInit":false},"log":{"level":"error"}}`
	wd, home := setupWorkspace(t, settings)

	// 先制造一个 failed 的最近 session；旧实现会在裸 TTY 启动时自动续跑它。
	code, _, errOut := runApp(t, []string{"new", "-m", "ask", "hi"}, wd, home,
		fake.New().QueueError(context.DeadlineExceeded), &captureSink{})
	if code == 0 {
		t.Fatalf("setup turn unexpectedly succeeded\nstderr=%s", errOut)
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	fp := newBlockingProvider()
	var out, errOut2 bytes.Buffer
	app, err := New(context.Background(), nil, Stdio{
		In: pr, Out: &out, Err: &errOut2, IsTTY: true, Env: []string{},
	}, Overrides{Provider: fp, WorkDir: wd, HomeDir: home})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	app.sigCh = make(chan os.Signal, 1)

	done := make(chan int, 1)
	go func() { done <- app.Run(context.Background()) }()
	if _, err := pw.Write([]byte("/exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	select {
	case <-fp.started:
		t.Fatal("bare TTY resumed the old failed turn before entering REPL")
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", code, out.String(), errOut2.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bare TTY did not reach REPL")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
