package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/statemachine"
)

func TestTextRendererReasoningIsOneBlock(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newTextRenderer(&out, &errOut)

	r.Reasoning("先分析")
	r.Reasoning("一下\n")
	if got := errOut.String(); got != "[思考] 先分析一下\n" {
		t.Fatalf("reasoning block = %q", got)
	}
	if out.Len() != 0 {
		t.Fatalf("reasoning must not write stdout: %q", out.String())
	}

	r.Delta("答案")
	r.EndMessage()
	if got := out.String(); got != "答案\n" {
		t.Fatalf("stdout = %q", got)
	}
	// 幂等：重复 EndMessage 不应再多出空行。
	r.EndMessage()
	if got := out.String(); got != "答案\n" {
		t.Fatalf("stdout after second EndMessage = %q", got)
	}
}

func TestTextRendererReasoningInterruptsPartialText(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newTextRenderer(&out, &errOut)

	r.Delta("半句")
	r.Reasoning("再想想")
	r.EndMessage()
	if got := out.String(); got != "半句\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := errOut.String(); got != "[思考] 再想想\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestTextRendererToolCallUsesKeyArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newTextRenderer(&out, &errOut)

	r.ToolCall("bash", []byte(`{"command":"go test ./...\nexport FOO=1","timeoutMs":120000}`))
	r.ToolResult("bash", true, 1500*time.Millisecond)
	want := "[tool] bash go test ./... …\n[tool] bash ok (1.5s)\n"
	if got := out.String(); got != want {
		t.Fatalf("tool trace = %q, want %q", got, want)
	}
}

func TestTextRendererTurnSummary(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newTextRenderer(&out, &errOut)
	r.mode = model.ModeCode
	r.modelAlias = "fake-model"

	r.Delta("正文")
	r.TurnSummary(statemachine.TurnResult{
		Status:   statemachine.StatusOK,
		Duration: 1234 * time.Millisecond,
		Usage:    model.Usage{Input: 1234, Output: 567, Cached: 12},
	}, nil)

	got := out.String()
	for _, want := range []string{
		"正文\n\n",
		"[ant] 完成 · mode=code · model=fake-model · 1.2s",
		"tokens：in 1.2k / out 567 / cached 12 / reasoning 0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestFormatDurationAndCount(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{850 * time.Millisecond, "850ms"},
		{8 * time.Second, "8s"},
		{2*time.Minute + 3*time.Second, "2m03s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Fatalf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if formatCount(999) != "999" || formatCount(1234) != "1.2k" || formatCount(1_500_000) != "1.5M" {
		t.Fatalf("formatCount mismatch: %s/%s/%s", formatCount(999), formatCount(1234), formatCount(1_500_000))
	}
}
