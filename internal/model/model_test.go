package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUsageAdd(t *testing.T) {
	a := Usage{Input: 10, Output: 2, Cached: 3}
	b := Usage{Input: 1, Output: 4, Reasoning: 5, Estimated: true}
	got := a.Add(b)
	want := Usage{Input: 11, Output: 6, Cached: 3, Reasoning: 5, Estimated: true}
	if got != want {
		t.Fatalf("Add() = %+v, want %+v", got, want)
	}
	if !got.Estimated {
		t.Fatal("Estimated should be sticky")
	}
}

func TestErrorClassification(t *testing.T) {
	e := NewError(ErrProviderRateLimit, "rate", "too many")
	if !IsRetryable(e) {
		t.Fatal("rate limit should be retryable")
	}
	if got := ClassOf(e); got != ErrProviderRateLimit {
		t.Fatalf("ClassOf = %q", got)
	}
	if got := ClassOf(errors.New("plain")); got != ErrInternal {
		t.Fatalf("unclassified = %q", got)
	}
	wrapped := &Error{Class: ErrToolFailed, Code: "x", Cause: e}
	if !errors.Is(wrapped, e) {
		t.Fatal("Unwrap should expose cause")
	}
	if !IsRetryable(wrapped) {
		t.Fatal("retryable cause should propagate through chain")
	}
	if !strings.Contains(e.WithHint("h").Error(), "hint") {
		t.Fatal("hint should appear in Error()")
	}
}

func TestEventJSONUsesMillisecondUTC(t *testing.T) {
	ev := Event{
		Seq:  7,
		TS:   time.Date(2026, 10, 30, 9, 47, 4, 123456789, time.FixedZone("UTC+8", 8*3600)),
		Type: EventTurnStarted,
		Turn: 1,
		Data: MustJSON(map[string]string{"input": "hi"}),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ts":"2026-10-30T01:47:04.123Z"`) {
		t.Fatalf("unexpected ts: %s", b)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.TS.Equal(ev.TS.Truncate(time.Millisecond)) {
		t.Fatalf("roundtrip ts = %v, want %v", back.TS, ev.TS.Truncate(time.Millisecond))
	}
	if !KnownEventType(back.Type) {
		t.Fatal("turn_started should be known")
	}
}

func TestEventSinkFunc(t *testing.T) {
	var got *Event
	sink := EventSinkFunc(func(ctx context.Context, ev *Event) error { got = ev; return nil })
	if err := sink.Emit(context.Background(), &Event{Type: EventUsage}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Type != EventUsage {
		t.Fatal("sink did not receive event")
	}
}
