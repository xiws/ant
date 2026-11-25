package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/statemachine"
)

func TestSplitMarkdownBlock(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		ready string
		rest  string
		ok    bool
	}{
		{"paragraph", "第一段\n\n第二段\n\n", "第一段\n\n", "第二段\n\n", true},
		{"incomplete", "第一段\n", "", "第一段\n", false},
		{"fence-internal-blank", "```go\na\n\nb\n```\n\nrest", "```go\na\n\nb\n```\n", "\nrest", true},
		{"unclosed-fence", "```go\na\n", "", "```go\na\n", false},
		{"blank-lines", "\n\ntext", "\n", "\ntext", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ready, rest, ok := splitMarkdownBlock(c.in)
			if ok != c.ok || ready != c.ready || rest != c.rest {
				t.Fatalf("split(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, ready, rest, ok, c.ready, c.rest, c.ok)
			}
		})
	}
}

func TestRichRendererRendersMarkdownAtBlockBoundary(t *testing.T) {
	var out, errOut bytes.Buffer
	r, err := newRichTextRenderer(&out, &errOut, 60, "dark")
	if err != nil {
		t.Fatal(err)
	}
	r.Delta("# 标题")
	if out.Len() != 0 {
		t.Fatalf("incomplete block must not be rendered yet: %q", out.String())
	}
	r.Delta("\n\n正文 **加粗**。\n")
	r.EndMessage()
	got := out.String()
	if !strings.Contains(got, "标题") || strings.Contains(got, "# 标题") {
		t.Fatalf("heading not rendered as markdown: %q", got)
	}
	if !strings.Contains(got, "正文") || !strings.Contains(got, "加粗") {
		t.Fatalf("paragraph missing: %q", got)
	}
	if strings.Contains(got, "**") {
		t.Fatalf("markdown marker leaked: %q", got)
	}
}

func TestRichRendererKeepsFenceUntilClosed(t *testing.T) {
	var out, errOut bytes.Buffer
	r, err := newRichTextRenderer(&out, &errOut, 60, "dark")
	if err != nil {
		t.Fatal(err)
	}
	r.Delta("```go\n")
	r.Delta("fmt.Println(1)\n")
	if out.Len() != 0 {
		t.Fatalf("open fence must wait: %q", out.String())
	}
	r.Delta("```\n")
	if out.Len() == 0 {
		t.Fatal("closed fence was not rendered immediately")
	}
	if !strings.Contains(out.String(), "fmt") {
		t.Fatalf("code missing: %q", out.String())
	}
}

func TestRichRendererProcessBlocks(t *testing.T) {
	var out, errOut bytes.Buffer
	r, err := newRichTextRenderer(&out, &errOut, 80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	r.Reasoning("先看看 README")
	r.Reasoning("\n再决定\n")
	r.ToolCall("read", []byte(`{"path":"README.md"}`))
	r.ToolResult("read", true, 15*time.Millisecond)
	r.ToolCall("bash", []byte(`{"command":"go test ./..."}`))
	r.ToolResult("bash", false, 1500*time.Millisecond)
	r.Delta("最终答案\n")
	r.EndMessage()

	errText := errOut.String()
	if !strings.Contains(errText, "思考") || !strings.Contains(errText, "▌") {
		t.Fatalf("thinking block missing: %q", errText)
	}
	if strings.Contains(out.String(), "思考") {
		t.Fatalf("thinking must stay on stderr: %q", out.String())
	}
	for _, want := range []string{"工具", "完成", "失败", "read", "bash", "go test ./..."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tool block missing %q:\n%s", want, out.String())
		}
	}
	// 思考紫 / 调用蓝 / 成功绿 / 失败红：用背景色区分。
	for _, want := range []string{"\x1b[48;5;60m", "\x1b[48;5;25m", "\x1b[48;5;28m", "\x1b[48;5;124m"} {
		if !strings.Contains(out.String()+errText, want) {
			t.Fatalf("missing color %q", want)
		}
	}
}

func TestRichRendererNeverBlinks(t *testing.T) {
	var out, errOut bytes.Buffer
	r, err := newRichTextRenderer(&out, &errOut, 80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	r.Reasoning("思考\n")
	r.ToolCall("read", []byte(`{"path":"a.go"}`))
	r.ToolResult("read", true, time.Millisecond)
	r.Delta("# 标题\n\n正文\n")
	r.EndMessage()
	r.TurnSummary(statemachine.TurnResult{Status: statemachine.StatusOK}, nil)
	for _, s := range []string{out.String(), errOut.String()} {
		if strings.Contains(s, "\x1b[5m") || strings.Contains(s, "\x1b[6m") {
			t.Fatalf("blinking SGR emitted: %q", s)
		}
	}
}

func TestColorEnabled(t *testing.T) {
	cases := []struct {
		mode string
		tty  bool
		env  []string
		want bool
	}{
		{"auto", true, nil, true},
		{"auto", false, nil, false},
		{"auto", true, []string{"NO_COLOR="}, false},
		{"auto", true, []string{"TERM=dumb"}, false},
		{"never", true, nil, false},
		{"always", false, nil, true},
	}
	for _, c := range cases {
		if got := colorEnabled(c.mode, c.tty, c.env); got != c.want {
			t.Fatalf("colorEnabled(%q,%v,%v) = %v, want %v", c.mode, c.tty, c.env, got, c.want)
		}
	}
}

func TestMarkdownStyle(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{nil, "dark"},
		{[]string{"COLORFGBG=15;0"}, "dark"},
		{[]string{"COLORFGBG=0;15"}, "light"},
		{[]string{"GLAMOUR_STYLE=notty"}, "notty"},
		{[]string{"GLAMOUR_STYLE=light", "COLORFGBG=15;0"}, "light"},
	}
	for _, c := range cases {
		if got := markdownStyle(c.env); got != c.want {
			t.Fatalf("markdownStyle(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestProtectHTMLKeepsTagsOutsideCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HTML 的 <div> 标签", `HTML 的 \<div> 标签`},
		{"泛型 Vec<T> 与 List<String>", `泛型 Vec\<T> 与 List\<String>`},
		{"比较 a < b 且 c > d", "比较 a < b 且 c > d"},
		{"<https://example.com>", "<https://example.com>"},
		{"`<div>` 保持原样", "`<div>` 保持原样"},
		{"```html\n<div>\n```", "```html\n<div>\n```"},
		{"<!-- 注释 -->", `\<!-- 注释 -->`},
		{"[x](<foo>)", "[x](<foo>)"},
		{`\<div> 已转义`, `\<div> 已转义`},
	}
	for _, c := range cases {
		if got := protectHTML(c.in); got != c.want {
			t.Fatalf("protectHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRichRendererPreservesHTMLAndGenerics(t *testing.T) {
	var out, errOut bytes.Buffer
	r, err := newRichTextRenderer(&out, &errOut, 80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	r.Delta("HTML 的 <div> 标签与泛型 Vec<T>，比较 a < b。\n")
	r.EndMessage()
	got := stripANSI(out.String())
	for _, want := range []string{"<div>", "Vec<T>", "a < b"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output lost %q: %q", want, got)
		}
	}
}

func TestTrimBlankRenderedLinesKeepsInternalBlank(t *testing.T) {
	in := "\n  \x1b[0m\n  a\n\n  b\n  \x1b[38;5;252m \x1b[0m\n"
	want := "  a\n\n  b"
	if got := trimBlankRenderedLines(in); got != want {
		t.Fatalf("trim = %q, want %q", got, want)
	}
}
