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

	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/statemachine"
)

// textRenderer 是默认终端渲染器。
//
// 可读性约定：
//   - 思考增量合并成一个 [思考] 块，而不是每个 chunk 一行；
//   - 一段模型文本结束后补换行（EndMessage），避免与后续日志/摘要粘在一起；
//   - 工具调用只展示关键参数（command/path/target…），不倾倒整段 JSON。
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
}

func newTextRenderer(out, err io.Writer) *textRenderer {
	return &textRenderer{out: out, err: err, lineStart: true, thinkEOL: true}
}

// closeThinking 结束思考块（调用方必须持有锁）。
func (r *textRenderer) closeThinking() {
	if !r.thinking {
		return
	}
	if !r.thinkEOL {
		fmt.Fprintln(r.err)
	}
	r.thinking = false
	r.thinkEOL = true
}

// EndMessage 结束一段流式输出：补换行并收掉未闭合的思考块。
func (r *textRenderer) EndMessage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
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
	fmt.Fprint(r.out, text)
	r.lineStart = strings.HasSuffix(text, "\n")
}

func (r *textRenderer) Reasoning(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *textRenderer) ToolCall(name string, args []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinking()
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
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	fmt.Fprintln(r.out) // 与模型正文之间留一个空行

	label := "完成"
	switch res.Status {
	case statemachine.StatusFailed:
		label = "失败"
	case statemachine.StatusInterrupted:
		label = "已中断"
	}
	fmt.Fprintf(r.out, "[ant] %s · mode=%s · model=%s", label, r.mode, r.modelAlias)
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
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
