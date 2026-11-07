package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/statemachine"
)

// textRenderer 是默认终端渲染器。
//
// 可读性约定：
//   - TTY 下用 Glamour 按 Markdown 块渲染模型正文（保留流式：完整块到达即渲染）；
//   - 思考/工具/结果使用不同颜色、带左色块的独立块，不使用闪烁 SGR；
//   - 过程块与结果块之间留空行，长过程里也能一眼定位最终答案；
//   - 非 TTY 或 --no-color 时退回纯文本：正文原样输出，过程用 [思考]/[tool] 前缀。
type textRenderer struct {
	mu          sync.Mutex
	out         io.Writer
	err         io.Writer
	mode        model.Mode
	modelAlias  string
	sessionPath string
	lineStart   bool // stdout 是否位于行首
	thinking    bool // 正在输出思考块
	thinkEOL    bool // 思考块（stderr）是否位于行首

	// rich 模式（TTY + color）：Glamour Markdown + ANSI 过程块。
	rich    bool
	width   int
	md      *glamour.TermRenderer
	mdBuf   string
	outKind blockKind
	outAny  bool
	errAny  bool
}

func newTextRenderer(out, err io.Writer) *textRenderer {
	return &textRenderer{out: out, err: err, lineStart: true, thinkEOL: true}
}

// closeThinking 结束思考块（调用方必须持有锁）。
func (r *textRenderer) closeThinking() {
	if !r.thinking {
		return
	}
	if r.rich {
		if !r.thinkEOL {
			fmt.Fprint(r.err, ansiReset+"\n")
		}
		r.thinking = false
		r.thinkEOL = true
		r.errAny = true
		return
	}
	if !r.thinkEOL {
		fmt.Fprintln(r.err)
	}
	r.thinking = false
	r.thinkEOL = true
}

// EndMessage 结束一段流式输出：渲染未完成的 Markdown 块并收掉思考块。
func (r *textRenderer) EndMessage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	if r.rich {
		r.flushMarkdown()
		if !r.lineStart {
			fmt.Fprintln(r.out)
			r.lineStart = true
		}
		return
	}
	if !r.lineStart {
		fmt.Fprintln(r.out)
		r.lineStart = true
	}
}

func (r *textRenderer) Delta(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	if r.rich {
		r.appendMarkdown(text)
		return
	}
	fmt.Fprint(r.out, text)
	r.lineStart = strings.HasSuffix(text, "\n")
}

func (r *textRenderer) Reasoning(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rich {
		if !r.thinking {
			r.beforeErrBlock()
			fmt.Fprintf(r.err, "%s %s\n", colorBar(colorThinkingBar), colorChip("思考", colorThinkingChip, colorOnChip))
			r.thinking = true
			r.thinkEOL = true
			r.errAny = true
		}
		r.writeThinkingText(text)
		return
	}
	if !r.thinking {
		if !r.lineStart {
			fmt.Fprintln(r.out)
			r.lineStart = true
		}
		fmt.Fprint(r.err, "[思考] ")
		r.thinking = true
		r.thinkEOL = false
	}
	fmt.Fprint(r.err, text)
	r.thinkEOL = strings.HasSuffix(text, "\n")
}

// writeThinkingText 按行加左色块，空行保持为空行，避免整屏“▌”。
func (r *textRenderer) writeThinkingText(text string) {
	prefix := colorBar(colorThinkingBar) + " " + ansiSGR("3", "38;5;"+strconv.Itoa(colorMuted))
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i == 0 {
			fmt.Fprint(r.err, "\n")
			text = text[1:]
			r.thinkEOL = true
			continue
		}
		if r.thinkEOL {
			fmt.Fprint(r.err, prefix)
			r.thinkEOL = false
		}
		if i < 0 {
			fmt.Fprint(r.err, text)
			return
		}
		fmt.Fprint(r.err, text[:i]+ansiReset+"\n")
		text = text[i+1:]
		r.thinkEOL = true
	}
}

func (r *textRenderer) ToolCall(name string, args []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	if r.rich {
		r.flushMarkdown()
		r.beforeTool()
		label := truncateRunes(name, 18)
		limit := r.width - 30 - len([]rune(label))
		if limit < 24 {
			limit = 24
		}
		line := fmt.Sprintf("%s %s %s", colorBar(colorToolBar), colorChip("工具", colorToolChip, colorOnChip), paint(label, "1", "38;5;252"))
		if arg := summarizeToolArgs(args); arg != "" {
			line += " " + paint(truncateRunes(arg, limit), "38;5;"+strconv.Itoa(colorMuted))
		}
		fmt.Fprintln(r.out, line)
		r.outKind = blockTool
		r.outAny = true
		r.lineStart = true
		return
	}
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	fmt.Fprintf(r.out, "[tool] %s %s\n", name, summarizeToolArgs(args))
	r.lineStart = true
}

func (r *textRenderer) ToolResult(name string, ok bool, dur time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	status := "ok"
	if !ok {
		status = "failed"
	}
	if r.rich {
		r.flushMarkdown()
		r.beforeTool()
		label, bar, chip := "完成", colorOKBar, colorOKChip
		if !ok {
			label, bar, chip = "失败", colorFailBar, colorFailChip
		}
		line := fmt.Sprintf("%s %s %s", colorBar(bar), colorChip(label, chip, colorOnChip), paint(name, "38;5;252"))
		if d := formatDuration(dur); d != "" {
			line += " " + paint("· "+d, "38;5;"+strconv.Itoa(colorMuted))
		}
		fmt.Fprintln(r.out, line)
		r.outKind = blockTool
		r.outAny = true
		r.lineStart = true
		return
	}
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	fmt.Fprintf(r.out, "[tool] %s %s (%s)\n", name, status, formatDuration(dur))
	r.lineStart = true
}

func (r *textRenderer) Notice(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	if r.rich {
		r.beforeErrBlock()
		fmt.Fprintf(r.err, "%s %s %s\n", colorBar(colorNoticeBar), colorChip("提示", colorNoticeChip, 252), paint(msg, "38;5;250"))
		r.errAny = true
		return
	}
	if !r.lineStart {
		fmt.Fprintln(r.out)
		r.lineStart = true
	}
	fmt.Fprintf(r.err, "[ant] %s\n", msg)
}

func (r *textRenderer) TurnSummary(res statemachine.TurnResult, u logx.UsageAggregator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
	if r.rich {
		r.flushMarkdown()
	}
	if !r.lineStart {
		fmt.Fprintln(r.out)
		r.lineStart = true
	}
	fmt.Fprintln(r.out) // 与模型正文之间留一个空行

	label := "完成"
	bar, chip := colorOKBar, colorOKChip
	switch res.Status {
	case statemachine.StatusFailed:
		label, bar, chip = "失败", colorFailBar, colorFailChip
	case statemachine.StatusInterrupted:
		label, bar, chip = "已中断", 136, 214
	}
	if r.rich {
		fmt.Fprintf(r.out, "%s %s · mode=%s · model=%s", colorBar(bar), colorChip(label, chip, colorOnChip), r.mode, r.modelAlias)
	} else {
		fmt.Fprintf(r.out, "[ant] %s · mode=%s · model=%s", label, r.mode, r.modelAlias)
	}
	if d := formatDuration(res.Duration); d != "" {
		fmt.Fprintf(r.out, " · %s", d)
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(r.out, " · exit=%d", res.ExitCode)
	}
	fmt.Fprintln(r.out)
	r.lineStart = true

	total := res.Usage
	if u != nil {
		if agg := u.Total(); total.IsZero() {
			total = agg
		}
	}
	fmt.Fprintf(r.out, "      tokens：in %s / out %s / cached %s / reasoning %s\n",
		formatCount(total.Input), formatCount(total.Output), formatCount(total.Cached), formatCount(total.Reasoning))
	if u != nil {
		if amount, known := u.Cost(); known {
			fmt.Fprintf(r.out, "      费用：$%.4f\n", amount)
		} else {
			fmt.Fprintln(r.out, "      费用：未配置 pricing（仅统计 token）")
		}
		if nodes := printNodeUsage(u); nodes != "" {
			fmt.Fprintf(r.out, "      节点：%s\n", nodes)
		}
	}
	if len(res.Changed) > 0 {
		fmt.Fprintf(r.out, "      改动：%s\n", strings.Join(res.Changed, ", "))
	}
	for _, c := range res.Checks {
		fmt.Fprintf(r.out, "      验证：%s (exit %d)\n", c.Cmd, c.ExitCode)
	}
	if res.PlanPath != "" {
		fmt.Fprintf(r.out, "      计划：%s\n", res.PlanPath)
	}
	if r.sessionPath != "" {
		fmt.Fprintf(r.out, "      session：%s\n", r.sessionPath)
	}
	r.outKind = blockAnswer
}

// jsonRenderer 在 --json 模式下不写 stdout（事件由 NDJSON sink 输出）。
type jsonRenderer struct{ err io.Writer }

func (r *jsonRenderer) Delta(string)                                              {}
func (r *jsonRenderer) Reasoning(string)                                          {}
func (r *jsonRenderer) ToolCall(string, []byte)                                   {}
func (r *jsonRenderer) ToolResult(string, bool, time.Duration)                    {}
func (r *jsonRenderer) Notice(msg string)                                         { fmt.Fprintf(r.err, "[ant] %s\n", msg) }
func (r *jsonRenderer) EndMessage()                                               {}
func (r *jsonRenderer) TurnSummary(statemachine.TurnResult, logx.UsageAggregator) {}

// renderSink 把工具事件映射到 Renderer，保证工具轨迹可见而不改事件契约。
func renderSink(r Renderer) model.EventSink {
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev == nil || r == nil {
			return nil
		}
		switch ev.Type {
		case model.EventModelResponse, model.EventNodeCompleted, model.EventTurnCompleted:
			// 文本流结束：先补换行，避免后续日志/摘要粘在最后一行。
			r.EndMessage()
		case model.EventToolCall:
			var d struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				r.ToolCall(d.Name, d.Arguments)
			}
		case model.EventToolResult:
			var d struct {
				Name       string `json:"name"`
				OK         *bool  `json:"ok"`
				Status     string `json:"status"`
				DurationMs int64  `json:"durationMs"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				ok := d.Status == "ok"
				if d.OK != nil {
					ok = *d.OK
				}
				name := d.Name
				if name == "" {
					name = string(ev.Node)
				}
				r.ToolResult(name, ok, time.Duration(d.DurationMs)*time.Millisecond)
			}
		}
		return nil
	})
}

// usageSink 把 usage 事件累加到 turn 摘要聚合器。
func usageSink(agg logx.UsageAggregator) model.EventSink {
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev == nil || ev.Type != model.EventUsage || agg == nil {
			return nil
		}
		var u model.Usage
		if err := json.Unmarshal(ev.Data, &u); err != nil {
			return nil
		}
		providerName := "router"
		var extra struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(ev.Data, &extra); err == nil && extra.Provider != "" {
			providerName = extra.Provider
		}
		agg.Add(ev.Node, providerName, u)
		return nil
	})
}

func printNodeUsage(u logx.UsageAggregator) string {
	if u == nil {
		return ""
	}
	byNode := u.ByNode()
	if len(byNode) == 0 {
		return ""
	}
	names := make([]string, 0, len(byNode))
	for n := range byNode {
		names = append(names, string(n))
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		usage := byNode[model.NodeName(n)]
		parts = append(parts, fmt.Sprintf("%s(%s in / %s out)", n, formatCount(usage.Input), formatCount(usage.Output)))
	}
	return strings.Join(parts, " → ")
}

// formatCount 把 token 数格式化为人类易读的 1.2k / 3.4M。
func formatCount(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	default:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	}
}

func trimZero(s string) string {
	return strings.TrimSuffix(s, ".0")
}

// formatDuration 输出紧凑耗时：850ms / 8.8s / 2m03s。
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return trimZero(fmt.Sprintf("%.1f", d.Seconds())) + "s"
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// summarizeToolArgs 提取工具调用的关键参数，便于一眼看出在做什么。
func summarizeToolArgs(args []byte) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err == nil {
		for _, key := range []string{"command", "cmd", "path", "target", "pattern", "query", "symbol", "name"} {
			if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
				return oneLine(v, 160)
			}
		}
	}
	return oneLine(string(args), 240)
}

// oneLine 压成单行并按 rune 截断，避免多行命令把工具轨迹撑乱。
func oneLine(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncateRunes(s, limit)
}
