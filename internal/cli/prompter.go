package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/session"
)

// terminalPrompter 是 approval.Prompter 的终端实现；approval 包不接触 stdin（R4）。
type terminalPrompter struct {
	mu             sync.Mutex
	reader         *bufio.Reader
	out            io.Writer
	isTTY          bool
	nonInteractive bool
}

func newTerminalPrompter(reader *bufio.Reader, out io.Writer, isTTY, nonInteractive bool) *terminalPrompter {
	return &terminalPrompter{reader: reader, out: out, isTTY: isTTY, nonInteractive: nonInteractive}
}

func (p *terminalPrompter) Confirm(ctx context.Context, cp approval.ConfirmPrompt) (approval.Choice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonInteractive || !p.isTTY {
		return approval.ChoiceDeny, approval.ErrNonInteractive
	}
	if err := ctx.Err(); err != nil {
		return approval.ChoiceDeny, err
	}
	fmt.Fprintf(p.out, "\n[ant] 需要审批：%s\n", cp.Title)
	if cp.Detail != "" {
		fmt.Fprintln(p.out, cp.Detail)
	}
	if cp.CWD != "" {
		fmt.Fprintf(p.out, "cwd：%s\n", cp.CWD)
	}
	if cp.Reason != "" {
		fmt.Fprintf(p.out, "原因：%s\n", cp.Reason)
	}
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(p.out, "允许？ [y] 一次 / [n] 拒绝 / [a] 本 session 同类全部允许：")
		line, err := p.readLine()
		if err != nil {
			return approval.ChoiceDeny, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes", "是":
			return approval.ChoiceOnce, nil
		case "n", "no", "否":
			return approval.ChoiceDeny, nil
		case "a", "all", "全部":
			return approval.ChoiceAll, nil
		}
		fmt.Fprintln(p.out, "请输入 y / n / a。")
	}
	return approval.ChoiceDeny, nil
}

func (p *terminalPrompter) Ask(ctx context.Context, q approval.Question) (approval.Answer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonInteractive || !p.isTTY {
		return approval.Answer{}, approval.ErrNonInteractive
	}
	if err := ctx.Err(); err != nil {
		return approval.Answer{}, err
	}
	fmt.Fprintf(p.out, "\n[ant] 需要你确认：%s\n", q.Question)
	for i, opt := range q.Options {
		fmt.Fprintf(p.out, "  %d) %s", i+1, opt.Label)
		if opt.Description != "" {
			fmt.Fprintf(p.out, " — %s", opt.Description)
		}
		fmt.Fprintln(p.out)
	}
	if q.AllowFreeText {
		fmt.Fprintf(p.out, "  %d) 其他（请输入）\n", len(q.Options)+1)
	}
	fmt.Fprint(p.out, "> ")
	line, err := p.readLine()
	if err != nil {
		return approval.Answer{}, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return approval.Answer{}, nil
	}
	if n, convErr := atoi(line); convErr == nil && n >= 1 && n <= len(q.Options) {
		return approval.Answer{SelectedID: q.Options[n-1].ID, Text: q.Options[n-1].Label}, nil
	}
	if q.AllowFreeText && !(q.Options != nil && line == fmt.Sprint(len(q.Options)+1)) {
		return approval.Answer{Text: line}, nil
	}
	// 非编号输入：作为自由文本兜底。
	return approval.Answer{Text: line}, nil
}

func (p *terminalPrompter) readLine() (string, error) {
	line, err := p.reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func atoi(s string) (int, error) {
	var n int
	if len(s) == 0 {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// replayDecider 用同一套交互决定「可能已执行的副作用工具」如何处理（§9.6）。
type replayDecider struct {
	prompter       approval.Prompter
	nonInteractive bool
}

func (d replayDecider) Decide(ctx context.Context, call session.PendingCall) (session.Replay, error) {
	if !call.Mutating {
		return session.ReplayRun, nil
	}
	if d.nonInteractive || d.prompter == nil {
		return session.ReplaySkip, nil
	}
	ans, err := d.prompter.Ask(ctx, approval.Question{
		Question: fmt.Sprintf("上次可能已执行工具 %s（call_id=%s），如何处理？", call.Name, call.CallID),
		Options: []approval.Option{
			{ID: "skip", Label: "跳过（合成 interrupted 结果）", Description: "推荐：避免重复副作用"},
			{ID: "run", Label: "重跑（可能重复副作用）"},
		},
		AllowFreeText: false,
	})
	if err != nil {
		return session.ReplaySkip, nil
	}
	if ans.SelectedID == "run" {
		return session.ReplayRun, nil
	}
	return session.ReplaySkip, nil
}
