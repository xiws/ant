package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor()
	in := "Authorization: Bearer sk-abcdef1234567890 apiKey=super-secret"
	out := r.Redact(in)
	if strings.Contains(out, "sk-abcdef") || strings.Contains(out, "super-secret") {
		t.Fatalf("secret leaked: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Fatalf("expected redaction marker: %q", out)
	}
}

func TestMultiOrderAndSeqBackfill(t *testing.T) {
	var order []string
	first := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		order = append(order, "session")
		ev.Seq = 42
		return nil
	})
	second := model.EventSinkFunc(func(ctx context.Context, ev *model.Event) error {
		order = append(order, "ndjson")
		if ev.Seq != 42 {
			t.Fatalf("later sink saw seq %d", ev.Seq)
		}
		return nil
	})
	sink := Multi(first, nil, second)
	if err := sink.Emit(context.Background(), &model.Event{Type: model.EventTurnStarted}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "session,ndjson" {
		t.Fatalf("order = %v", order)
	}
}

func TestMultiFirstFailureIsFatal(t *testing.T) {
	fail := model.EventSinkFunc(func(context.Context, *model.Event) error { return context.Canceled })
	called := false
	second := model.EventSinkFunc(func(context.Context, *model.Event) error { called = true; return nil })
	if err := Multi(fail, second).Emit(context.Background(), &model.Event{}); err == nil {
		t.Fatal("first sink failure should be fatal")
	}
	if called {
		t.Fatal("later sinks must not run after session failure")
	}
}

func TestNDJSONTrimsLargeFields(t *testing.T) {
	var buf bytes.Buffer
	sink := NewNDJSON(&buf)
	ev := &model.Event{
		Seq:  1,
		TS:   time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC),
		Type: model.EventToolCall,
		Data: model.MustJSON(map[string]any{"id": "c1", "arguments": "very long arguments", "name": "read"}),
	}
	if err := sink.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatalf("invalid NDJSON: %v\n%s", err, buf.String())
	}
	data := decoded["data"].(map[string]any)
	if _, ok := data["arguments"]; ok {
		t.Fatal("arguments should be trimmed")
	}
	if _, ok := data["argumentsRef"]; !ok {
		t.Fatalf("expected argumentsRef, got %v", data)
	}
}

func TestUsageAggregator(t *testing.T) {
	pricing := map[model.NodeName]*model.Pricing{
		model.NodeCode: {InputPer1M: 1, OutputPer1M: 2, CachedInputPer1M: 0.5},
	}
	agg := NewUsageAggregator(func(n model.NodeName) *model.Pricing { return pricing[n] })
	agg.Add(model.NodeCode, "p", model.Usage{Input: 1_000_000, Output: 500_000, Cached: 200_000})
	agg.Add(model.NodeAsk, "p", model.Usage{Input: 10, Output: 10})
	total := agg.Total()
	if total.Input != 1_000_010 || total.Output != 500_010 {
		t.Fatalf("total = %+v", total)
	}
	if len(agg.ByNode()) != 2 || len(agg.ByProvider()) != 1 {
		t.Fatalf("byNode=%v byProvider=%v", agg.ByNode(), agg.ByProvider())
	}
	amount, known := agg.Cost()
	if known {
		t.Fatal("ask has no pricing, so the aggregate must be reported unknown")
	}
	// 仍应计入已知 pricing 的 code 部分。
	if amount <= 0 {
		t.Fatalf("amount = %f", amount)
	}

	unknown := NewUsageAggregator(nil)
	unknown.Add(model.NodeCode, "", model.Usage{Input: 1})
	if _, known := unknown.Cost(); known {
		t.Fatal("nil pricing must be unknown")
	}
}

func TestLoggerLevelsAndRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithWriter(LevelWarn, &buf)
	logger.Info("hidden")
	logger.Warn("visible", "key", "sk-abcdef123456")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatal("info should be filtered at warn level")
	}
	if !strings.Contains(out, "visible") || strings.Contains(out, "sk-abcdef") {
		t.Fatalf("log output = %q", out)
	}
	if ParseLevel("debug") != LevelDebug || ParseLevel("nope") != LevelInfo {
		t.Fatal("ParseLevel mismatch")
	}
}

func TestLoggerSinkDoesNotLogContent(t *testing.T) {
	var buf bytes.Buffer
	sink := NewLoggerSink(NewWithWriter(LevelDebug, &buf), "debug")
	ev := &model.Event{
		Seq:  1,
		Type: model.EventToolCall,
		Data: model.MustJSON(map[string]string{"arguments": "SECRET-ARGS", "name": "read"}),
	}
	if err := sink.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECRET-ARGS") {
		t.Fatalf("content leaked to log: %s", buf.String())
	}
}

func TestLoggerSinkNilAndLogFormat(t *testing.T) {
	if err := NewLoggerSink(nil, "info").Emit(context.Background(), &model.Event{Type: model.EventUsage}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := New(config.LogSettings{Level: "info", Format: "json"}, &buf)
	logger.Info("hello", "n", 1)
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("json log invalid: %v", err)
	}
	if m["msg"] != "hello" || m["level"] != "info" {
		t.Fatalf("log = %v", m)
	}
}
