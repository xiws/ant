package agent

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type loopDetector struct {
	mu    sync.Mutex
	warn  int
	abort int
	seen  map[string]int
}

// NewLoop 按 limits.maxIdenticalToolCalls 构造循环检测器：达到阈值 warning，
// 达到阈值 + 2 时中止（§13.5：默认 3 警告 / 5 中止）。
func NewLoop(cfg config.View) LoopDetector {
	warn, abort := 3, 5
	if cfg != nil {
		if n := cfg.Settings().Limits.MaxIdenticalToolCalls; n > 0 {
			warn = n
			abort = n + 2
		}
	}
	return &loopDetector{warn: warn, abort: abort, seen: map[string]int{}}
}

func (l *loopDetector) Observe(node model.NodeName, call model.ToolCall) (LoopVerdict, error) {
	key := string(node) + "|" + call.Name + "|" + canonicalArgs(call.Arguments)
	l.mu.Lock()
	l.seen[key]++
	n := l.seen[key]
	l.mu.Unlock()
	switch {
	case n >= l.abort:
		return LoopAbort, nil
	case n >= l.warn:
		return LoopWarn, nil
	default:
		return LoopOK, nil
	}
}

// canonicalArgs 归一化参数（key 排序、去空白），用于指纹（§13.5）。
func canonicalArgs(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v) // encoding/json 对 map key 排序
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func sortedToolNames(names []model.ToolName) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}
