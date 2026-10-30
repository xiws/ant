package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/statemachine"
)

// textRenderer 是默认终端渲染器。
type textRenderer struct {
	mu          sync.Mutex
	out         io.Writer
	err         io.Writer
	mode        model.Mode
	modelAlias  string
	sessionPath string
	lineStart   bool
}

func newTextRenderer(out, err io.Writer) *textRenderer {
	return &textRenderer{out: out, err: err, lineStart: true}
}

func (r *textRenderer) Delta(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprint(r.out, text)
	r.lineStart = strings.HasSuffix(text, "\n")
}

func (r *textRenderer) Reasoning(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	fmt.Fprintf(r.err, "[reasoning] %s\n", strings.TrimRight(text, "\n"))
	r.lineStart = true
}

func (r *textRenderer) ToolCall(name string, args []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	sum := string(args)
	if len(sum) > 240 {
		sum = sum[:240] + "…"
	}
	fmt.Fprintf(r.out, "[tool] %s %s\n", name, sum)
	r.lineStart = true
}

func (r *textRenderer) ToolResult(name string, ok bool, dur time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := "ok"
	if !ok {
		status = "failed"
	}
	fmt.Fprintf(r.out, "[tool] %s %s (%s)\n", name, status, dur.Round(time.Millisecond))
	r.lineStart = true
}

func (r *textRenderer) Notice(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lineStart {
		fmt.Fprintln(r.out)
	}
	fmt.Fprintf(r.err, "[ant] %s\n", msg)
	r.lineStart = true
}

func (r *textRenderer) TurnSummary(res statemachine.TurnResult, u logx.UsageAggregator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lineStart {
		fmt.Fprintln(r.out)
		r.lineStart = true
	}
	label := "完成"
	switch res.Status {
	case statemachine.StatusFailed:
		label = "失败"
	case statemachine.StatusInterrupted:
		label = "已中断"
	}
	fmt.Fprintf(r.out, "[ant] %s · mode=%s · model=%s", label, r.mode, r.modelAlias)
	if res.ExitCode != 0 {
		fmt.Fprintf(r.out, " · exit=%d", res.ExitCode)
	}
	fmt.Fprintln(r.out)

	total := res.Usage
	if u != nil {
		if agg := u.Total(); total.IsZero() {
			total = agg
		}
	}
	fmt.Fprintf(r.out, "      tokens：in %d / out %d / cached %d / reasoning %d\n",
		total.Input, total.Output, total.Cached, total.Reasoning)
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
func (r *jsonRenderer) TurnSummary(statemachine.TurnResult, logx.UsageAggregator) {}

// renderSink 把工具事件映射到 Renderer，保证工具轨迹可见而不改事件契约。
func renderSink(r Renderer) model.EventSink {
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev == nil || r == nil {
			return nil
		}
		switch ev.Type {
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
		parts = append(parts, fmt.Sprintf("%s(%d in / %d out)", n, usage.Input, usage.Output))
	}
	return strings.Join(parts, " → ")
}
