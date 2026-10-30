// Package logx 提供日志、打码与事件扇出（docs/13-observability.md §15）。
package logx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// Level 是日志级别。
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

// ParseLevel 解析级别字符串，未知值回落到 info。
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError
	case "warn", "warning":
		return LevelWarn
	case "debug":
		return LevelDebug
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

// Logger 是分级日志与打码的唯一入口。
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
	With(kv ...any) Logger
}

// Redactor 把敏感值替换为 ***。
type Redactor interface {
	Redact(s string) string
}

// Sink 是事件扇出的一路。
type Sink interface {
	model.EventSink
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
func (nopLogger) With(...any) Logger   { return nopLogger{} }

// Nop 返回丢弃全部日志的 Logger，便于测试与可选注入。
func Nop() Logger { return nopLogger{} }

type textRedactor struct{}

var (
	reSecretToken = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`)
	reSecretKV    = regexp.MustCompile(`(?i)(api[_-]?key|authorization|access[_-]?token|secret)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`)
)

func (textRedactor) Redact(s string) string {
	if s == "" {
		return s
	}
	s = reSecretToken.ReplaceAllString(s, "***")
	s = reSecretKV.ReplaceAllString(s, "$1$2***")
	return s
}

// NewRedactor 返回默认打码器。
func NewRedactor() Redactor { return textRedactor{} }

type logger struct {
	mu       *sync.Mutex
	out      io.Writer
	level    Level
	format   string // text | json
	redactor Redactor
	kvs      []any
}

// New 按 log 配置构造 Logger；w 通常为 stderr。空 writer 时丢弃输出。
func New(s config.LogSettings, w io.Writer) Logger {
	if w == nil {
		w = io.Discard
	}
	return &logger{
		mu:       &sync.Mutex{},
		out:      w,
		level:    ParseLevel(s.Level),
		format:   s.Format,
		redactor: NewRedactor(),
	}
}

// NewWithWriter 便于测试直接指定级别。
func NewWithWriter(level Level, w io.Writer) Logger {
	return &logger{mu: &sync.Mutex{}, out: w, level: level, format: "text", redactor: NewRedactor()}
}

func (l *logger) With(kv ...any) Logger {
	merged := make([]any, 0, len(l.kvs)+len(kv))
	merged = append(merged, l.kvs...)
	merged = append(merged, kv...)
	return &logger{mu: l.mu, out: l.out, level: l.level, format: l.format, redactor: l.redactor, kvs: merged}
}

func (l *logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv) }
func (l *logger) Info(msg string, kv ...any)  { l.log(LevelInfo, msg, kv) }
func (l *logger) Warn(msg string, kv ...any)  { l.log(LevelWarn, msg, kv) }
func (l *logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv) }

func (l *logger) log(level Level, msg string, kv []any) {
	if level > l.level {
		return
	}
	pairs := make([]any, 0, len(l.kvs)+len(kv))
	pairs = append(pairs, l.kvs...)
	pairs = append(pairs, kv...)
	ts := time.Now().UTC().Format(model.EventTSLayout)

	var line string
	if l.format == "json" {
		fields := map[string]any{"ts": ts, "level": level.String(), "msg": msg}
		for i := 0; i+1 < len(pairs); i += 2 {
			key, ok := pairs[i].(string)
			if !ok {
				key = fmt.Sprint(pairs[i])
			}
			fields[key] = l.redactValue(pairs[i+1])
		}
		b, err := json.Marshal(fields)
		if err != nil {
			b, _ = json.Marshal(map[string]any{"ts": ts, "level": level.String(), "msg": msg})
		}
		line = string(b)
	} else {
		var b strings.Builder
		b.WriteString(ts)
		b.WriteString(" ")
		b.WriteString(strings.ToUpper(level.String()))
		b.WriteString(" ")
		b.WriteString(l.redactor.Redact(msg))
		for i := 0; i+1 < len(pairs); i += 2 {
			b.WriteString(" ")
			b.WriteString(fmt.Sprint(pairs[i]))
			b.WriteString("=")
			b.WriteString(l.redactor.Redact(fmt.Sprint(pairs[i+1])))
		}
		line = b.String()
	}
	l.mu.Lock()
	fmt.Fprintln(l.out, line)
	l.mu.Unlock()
}

func (l *logger) redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return l.redactor.Redact(t)
	case error:
		return l.redactor.Redact(t.Error())
	default:
		return v
	}
}

// NewLoggerSink 把事件摘要（打码后）写到日志。大字段只记录引用或占位。
func NewLoggerSink(l Logger, level string) model.EventSink {
	if l == nil {
		return model.EventSinkFunc(func(context.Context, *model.Event) error { return nil })
	}
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev == nil {
			return nil
		}
		msg := "event " + string(ev.Type)
		lv := ParseLevel(level)
		kv := []any{"seq", ev.Seq, "turn", ev.Turn}
		if ev.Node != "" {
			kv = append(kv, "node", string(ev.Node))
		}
		if ev.Step != "" {
			kv = append(kv, "step", ev.Step)
		}
		if len(ev.Data) > 0 {
			kv = append(kv, "data", summarizeEventData(ev.Data))
		}
		switch {
		case ev.Type == model.EventSessionFailed:
			l.Error(msg, kv...)
		case lv == LevelDebug:
			l.Debug(msg, kv...)
		default:
			l.Info(msg, kv...)
		}
		return nil
	})
}

// summarizeEventData 去掉大字段，只保留可读摘要。
func summarizeEventData(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "<data>"
	}
	for _, key := range []string{"content", "arguments", "text", "stdout", "stderr", "summary", "plan"} {
		if _, ok := m[key]; ok {
			m[key] = "<omitted>"
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "<data>"
	}
	if len(b) > 512 {
		return string(b[:512]) + "..."
	}
	return string(b)
}

// NewNDJSON 构造 --json 模式的 stdout sink（§9.4）。
func NewNDJSON(w io.Writer) model.EventSink {
	if w == nil {
		return model.EventSinkFunc(func(context.Context, *model.Event) error { return nil })
	}
	var mu sync.Mutex
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if ev == nil {
			return nil
		}
		cp := *ev
		cp.Data = trimEventDataForNDJSON(ev.Data)
		b, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintln(w, string(b)); err != nil {
			return err
		}
		return nil
	})
}

func trimEventDataForNDJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"content", "arguments", "text", "stdout", "stderr", "summary", "plan"} {
		if v, ok := m[key]; ok {
			cp := make(map[string]any, len(m))
			for k, vv := range m {
				cp[k] = vv
			}
			cp[key+"Ref"] = map[string]any{"omitted": true, "bytes": len(fmt.Sprint(v))}
			delete(cp, key)
			m = cp
			changed = true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// Multi 按顺序扇出：第一个必须是 session（它分配 seq），后续 sink 只读。
// 第一个失败视为致命；后续失败只 warning（不影响 turn）。
func Multi(sinks ...model.EventSink) model.EventSink {
	clean := make([]model.EventSink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			clean = append(clean, s)
		}
	}
	return model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		if len(clean) == 0 {
			return nil
		}
		if err := clean[0].Emit(ctx, ev); err != nil {
			return err
		}
		for _, s := range clean[1:] {
			_ = s.Emit(ctx, ev)
		}
		return nil
	})
}
