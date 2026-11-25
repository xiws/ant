package trace

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// RenderHTML 生成单文件、自包含的 HTML 报告。
// 所有外部内容都经 html.EscapeString；不使用 template.HTML 注入。
func RenderHTML(doc *Document) []byte {
	if doc == nil {
		doc = &Document{}
	}
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>ant session trace · %s</title>\n", esc(doc.SessionID))
	b.WriteString(htmlCSS)
	b.WriteString("</head>\n<body>\n")
	htmlHeader(&b, doc)
	htmlWarnings(&b, doc)
	htmlPlan(&b, doc)
	htmlTOC(&b, doc)
	for _, turn := range doc.Turns {
		htmlTurn(&b, turn)
	}
	htmlArtifacts(&b, doc)
	htmlRawEvents(&b, doc)
	b.WriteString("</div>\n</body>\n</html>\n")
	return []byte(b.String())
}

const htmlCSS = `<style>
:root{color-scheme:light dark;--bg:#0f1115;--fg:#e6e6e6;--muted:#9aa4b2;--line:#2a2f3a;--card:#171b22;--ok:#2ea043;--bad:#d73a49;--warn:#d29922;--accent:#58a6ff}
*{box-sizing:border-box}
body{margin:0;padding:24px;background:var(--bg);color:var(--fg);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"Noto Sans SC",sans-serif}
h1{font-size:22px;margin:0 0 8px}h2{font-size:18px;margin:28px 0 10px;border-bottom:1px solid var(--line);padding-bottom:6px}h3{font-size:16px;margin:20px 0 8px}h4{font-size:14px;margin:16px 0 6px}h5{font-size:13px;margin:12px 0 4px}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.wrap{max-width:1100px;margin:0 auto}
.meta,.muted{color:var(--muted)}
.meta{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:6px 18px;margin:8px 0 0}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px 14px;margin:10px 0}
.node{border-left:3px solid var(--accent);padding-left:12px;margin:14px 0}
.round{border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin:10px 0;background:var(--card)}
.tool{border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin:8px 0;background:#12161d}
pre{white-space:pre-wrap;word-break:break-word;background:#0b0e13;border:1px solid var(--line);border-radius:6px;padding:10px;overflow:auto;margin:6px 0}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace;font-size:12.5px}
.badge{display:inline-block;border-radius:999px;padding:1px 8px;font-size:12px;border:1px solid var(--line);color:var(--muted);margin-right:6px}
.badge.ok{color:var(--ok);border-color:var(--ok)}.badge.bad{color:var(--bad);border-color:var(--bad)}.badge.warn{color:var(--warn);border-color:var(--warn)}
details{margin:6px 0}summary{cursor:pointer;color:var(--accent)}
ul{margin:6px 0 6px 20px;padding:0}li{margin:2px 0}
.warnbox{border-left:3px solid var(--warn);background:#1c1a12;padding:8px 12px;margin:10px 0;border-radius:6px}
@media (prefers-color-scheme:light){:root{--bg:#ffffff;--fg:#1f2328;--muted:#59636e;--line:#d0d7de;--card:#f6f8fa;--ok:#1a7f37;--bad:#cf222e;--warn:#9a6700;--accent:#0969da}pre{background:#f6f8fa}}
</style>
`

func htmlHeader(b *strings.Builder, doc *Document) {
	b.WriteString("<div class=\"wrap\">\n")
	fmt.Fprintf(b, "<h1>ant session trace · <code>%s</code></h1>\n", esc(doc.SessionID))
	b.WriteString("<div class=\"meta\">\n")
	meta := [][2]string{
		{"workspace", doc.Workspace},
		{"title", OneLine(doc.Title, 200)},
		{"mode", string(doc.Mode)},
		{"model", doc.Model},
		{"status", StatusLabel(doc.Status)},
		{"created", fmtTime(doc.CreatedAt)},
		{"updated", fmtTime(doc.UpdatedAt)},
		{"exported", fmtTime(doc.ExportedAt)},
		{"ant", doc.AntVersion},
		{"usage", FormatUsage(doc.Usage)},
	}
	for _, kv := range meta {
		fmt.Fprintf(b, "<div><span class=\"muted\">%s：</span>%s</div>\n", esc(kv[0]), esc(kv[1]))
	}
	b.WriteString("</div>\n")
}

func htmlWarnings(b *strings.Builder, doc *Document) {
	if len(doc.Warnings) == 0 {
		return
	}
	b.WriteString("<div class=\"warnbox\"><strong>Warnings</strong><ul>\n")
	for _, w := range doc.Warnings {
		fmt.Fprintf(b, "<li>%s</li>\n", esc(OneLine(w, 500)))
	}
	b.WriteString("</ul></div>\n")
}

func htmlPlan(b *strings.Builder, doc *Document) {
	if strings.TrimSpace(doc.Plan) == "" {
		return
	}
	b.WriteString("<h2>Plan</h2>\n")
	if doc.PlanPath != "" {
		fmt.Fprintf(b, "<p class=\"muted\">来源：<code>%s</code></p>\n", esc(doc.PlanPath))
	}
	writeHTMLCode(b, "markdown", doc.Plan)
}

func htmlTOC(b *strings.Builder, doc *Document) {
	if len(doc.Turns) == 0 {
		return
	}
	b.WriteString("<h2>目录</h2><ul>\n")
	for _, t := range doc.Turns {
		fmt.Fprintf(b, "<li><a href=\"#turn-%d\">Turn %d · %s</a>", t.Index, t.Index, esc(OneLine(t.Input, 80)))
		if len(t.Nodes) > 0 {
			b.WriteString("<ul>")
			for _, n := range t.Nodes {
				fmt.Fprintf(b, "<li><a href=\"#node-%s\">%s</a></li>", esc(anchor(n.RunID)), esc(string(n.Name)))
			}
			b.WriteString("</ul>")
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
}

func htmlTurn(b *strings.Builder, turn *Turn) {
	fmt.Fprintf(b, "<section id=\"turn-%d\"><h2>Turn %d · %s</h2>\n", turn.Index, turn.Index, esc(OneLine(turn.Input, 120)))
	fmt.Fprintf(b, "<div class=\"card\"><span class=\"badge %s\">%s</span>", statusClass(turn.Status), esc(StatusLabel(turn.Status)))
	if turn.ExitCode != 0 {
		fmt.Fprintf(b, "<span class=\"badge\">exit %d</span>", turn.ExitCode)
	}
	fmt.Fprintf(b, "<span class=\"muted\">%s</span>", esc(FormatUsage(turn.Usage)))
	if !turn.StartedAt.IsZero() {
		fmt.Fprintf(b, "<div class=\"muted\">started %s", esc(fmtTime(turn.StartedAt)))
		if !turn.CompletedAt.IsZero() {
			fmt.Fprintf(b, " · completed %s", esc(fmtTime(turn.CompletedAt)))
		}
		b.WriteString("</div>")
	}
	if turn.Error != "" {
		fmt.Fprintf(b, "<div class=\"badge bad\">%s</div>", esc(OneLine(turn.Error, 300)))
	}
	b.WriteString("</div>\n")

	if strings.TrimSpace(turn.Input) != "" {
		b.WriteString("<details><summary>输入</summary>\n")
		writeHTMLCode(b, "text", turn.Input)
		b.WriteString("</details>\n")
	}
	if len(turn.ModeChanges) > 0 {
		b.WriteString("<div class=\"card\"><strong>模式变更</strong><ul>\n")
		for _, mc := range turn.ModeChanges {
			fmt.Fprintf(b, "<li>%s → %s（%s，confirmed=%v）</li>\n", esc(mc.From), esc(mc.To), esc(mc.Reason), mc.Confirmed)
		}
		b.WriteString("</ul></div>\n")
	}
	if len(turn.Approvals) > 0 {
		b.WriteString("<details><summary>审批（")
		fmt.Fprintf(b, "%d）</summary><ul>\n", len(turn.Approvals))
		for _, a := range turn.Approvals {
			parts := []string{"class=" + a.Class}
			if a.Node != "" {
				parts = append(parts, "node="+a.Node)
			}
			if a.Decision != "" {
				parts = append(parts, "decision="+a.Decision)
			}
			if a.Scope != "" {
				parts = append(parts, "scope="+a.Scope)
			}
			if a.Command != "" {
				parts = append(parts, "command="+OneLine(a.Command, 160))
			}
			fmt.Fprintf(b, "<li>%s · %s</li>\n", esc(fmtTime(a.At)), esc(strings.Join(parts, " · ")))
		}
		b.WriteString("</ul></details>\n")
	}
	for _, c := range turn.Compactions {
		htmlCompaction(b, c)
	}
	for _, n := range turn.Nodes {
		htmlNode(b, n)
	}
	b.WriteString("</section>\n")
}

func htmlNode(b *strings.Builder, n *Node) {
	fmt.Fprintf(b, "<article class=\"node\" id=\"node-%s\"><h3>节点 %s <code>%s</code></h3>\n", esc(anchor(n.RunID)), esc(string(n.Name)), esc(n.RunID))
	fmt.Fprintf(b, "<div class=\"muted\">step <code>%s</code> · model %s · provider %s</div>\n", esc(n.Step), esc(n.Model), esc(n.Provider))
	fmt.Fprintf(b, "<div class=\"muted\">prompt version=%s · hash=%s · stable=%s · tools=%s</div>\n",
		esc(orDash(n.PromptVersion)), esc(ShortHash(n.PromptHash)), esc(ShortHash(n.StableHash)), esc(ShortHash(n.ToolsHash)))
	if n.InputRef != "" {
		fmt.Fprintf(b, "<div class=\"muted\">inputRef <code>%s</code></div>\n", esc(n.InputRef))
	}
	if n.Completed != nil {
		fmt.Fprintf(b, "<div class=\"card\"><span class=\"badge %s\">%s</span><span class=\"muted\">duration %s · %s</span>",
			statusClass(n.Completed.Status), esc(StatusLabel(n.Completed.Status)),
			esc(FormatDuration(n.Completed.Duration.Milliseconds())), esc(FormatUsage(n.Completed.Usage)))
		if n.Completed.Dimension != "" {
			fmt.Fprintf(b, "<div class=\"muted\">dimension：%s</div>", esc(n.Completed.Dimension))
		}
		if n.Completed.Error != "" {
			fmt.Fprintf(b, "<div class=\"badge bad\">%s</div>", esc(OneLine(n.Completed.Error, 300)))
		}
		b.WriteString("</div>\n")
	}
	for _, w := range n.Warnings {
		fmt.Fprintf(b, "<div class=\"warnbox\">%s</div>\n", esc(OneLine(w, 300)))
	}

	if n.Input != nil {
		if n.Input.Dimension != "" {
			fmt.Fprintf(b, "<div class=\"muted\">dimension：%s</div>\n", esc(n.Input.Dimension))
		}
		if n.Input.Request != "" {
			fmt.Fprintf(b, "<details><summary>节点输入 request</summary>\n")
			writeHTMLCode(b, "text", n.Input.Request)
			b.WriteString("</details>\n")
		}
		if n.Input.Plan != "" {
			fmt.Fprintf(b, "<details><summary>节点输入 plan</summary>\n")
			writeHTMLCode(b, "markdown", n.Input.Plan)
			b.WriteString("</details>\n")
		}
		if fb := strings.TrimSpace(string(n.Input.Feedback)); fb != "" && fb != "null" && fb != "[]" {
			fmt.Fprintf(b, "<details><summary>节点输入 feedback</summary>\n")
			writeHTMLCode(b, "json", PrettyJSON(n.Input.Feedback))
			b.WriteString("</details>\n")
		}
		b.WriteString("<details><summary>有效 Prompt / 消息（node-input 原文）</summary>\n")
		htmlMessages(b, n.Input.Messages)
		b.WriteString("</details>\n")
		if len(n.Input.Tools) > 0 {
			fmt.Fprintf(b, "<details><summary>工具定义（%d）</summary>\n", len(n.Input.Tools))
			for _, t := range n.Input.Tools {
				fmt.Fprintf(b, "<h5>tool %s</h5>", esc(t.Name))
				if t.Description != "" {
					fmt.Fprintf(b, "<p>%s</p>", esc(t.Description))
				}
				fmt.Fprintf(b, "<div class=\"muted\">mutating=%v · approval=%s · timeout=%s</div>\n", t.Mutating, esc(string(t.Approval)), esc(t.Timeout.String()))
				if len(t.Schema) > 0 {
					writeHTMLCode(b, "json", PrettyJSON(t.Schema))
				}
			}
			b.WriteString("</details>\n")
		}
	} else if n.InputRef != "" {
		b.WriteString("<div class=\"warnbox\">node-input artifact 缺失，无法展示原始 prompt。</div>\n")
	}
	for _, msg := range n.Messages {
		htmlMessage(b, msg.Role, msg.Content, msg.ContentRef, msg.ToolCalls)
	}
	for _, r := range n.Rounds {
		htmlRound(b, r)
	}
	if n.Output != nil {
		b.WriteString("<details><summary>节点输出</summary>\n")
		if n.Output.Ref != "" {
			fmt.Fprintf(b, "<div class=\"muted\">outputRef <code>%s</code></div>\n", esc(n.Output.Ref))
		}
		if n.Output.Text != "" {
			writeHTMLCode(b, "markdown", n.Output.Text)
		}
		if len(n.Output.Structured) > 0 && string(n.Output.Structured) != "null" {
			writeHTMLCode(b, "json", PrettyJSON(n.Output.Structured))
		}
		if len(n.Output.Changed) > 0 {
			fmt.Fprintf(b, "<div class=\"muted\">changed：%s</div>\n", esc(strings.Join(n.Output.Changed, ", ")))
		}
		b.WriteString("</details>\n")
	}
	if n.Completed != nil && len(n.Completed.Verdict) > 0 && string(n.Completed.Verdict) != "null" {
		b.WriteString("<details><summary>Verdict</summary>\n")
		writeHTMLCode(b, "json", PrettyJSON(n.Completed.Verdict))
		b.WriteString("</details>\n")
	}
	b.WriteString("</article>\n")
}

func htmlRound(b *strings.Builder, r *Round) {
	b.WriteString("<div class=\"round\">\n")
	if r.Request != nil {
		source := "原文"
		if r.Reconstructed {
			source = "重建"
		}
		fmt.Fprintf(b, "<h4>请求 #%d（%s）</h4>\n", roundIndex(r), source)
		fmt.Fprintf(b, "<div class=\"muted\">messages=%d · est=%s · tools=%d · stream=%v · attempt=%d</div>\n",
			r.Request.MessageCount, esc(FormatCount(r.Request.EstimatedInputTokens)), len(r.Request.Tools), r.Request.Stream, r.Request.Attempt)
		for _, w := range r.Warnings {
			fmt.Fprintf(b, "<div class=\"warnbox\">%s</div>\n", esc(w))
		}
		if len(r.Messages) > 0 {
			fmt.Fprintf(b, "<details><summary>请求消息（%d）</summary>\n", len(r.Messages))
			htmlMessages(b, r.Messages)
			b.WriteString("</details>\n")
		}
	}
	if r.Response != nil {
		fmt.Fprintf(b, "<h4>响应 #%d</h4>\n", roundIndex(r))
		if r.Response.ErrorClass != "" || r.Response.Code != "" || r.Response.Message != "" {
			fmt.Fprintf(b, "<div class=\"badge bad\">%s/%s %s</div>\n", esc(r.Response.ErrorClass), esc(r.Response.Code), esc(OneLine(r.Response.Message, 300)))
		} else {
			fmt.Fprintf(b, "<div class=\"muted\">finish=%s · duration=%s · %s · calls=%d</div>\n",
				esc(orDash(r.Response.FinishReason)), esc(FormatDuration(r.Response.Duration.Milliseconds())),
				esc(FormatUsage(r.Response.Usage)), r.Response.ToolCallCount)
		}
	}
	if r.Assistant != nil {
		htmlMessage(b, r.Assistant.Role, r.Assistant.Content, r.Assistant.ContentRef, r.Assistant.ToolCalls)
	}
	for _, tc := range r.ToolCalls {
		htmlToolCall(b, tc)
	}
	b.WriteString("</div>\n")
}

func htmlToolCall(b *strings.Builder, tc *ToolCall) {
	b.WriteString("<div class=\"tool\">\n")
	fmt.Fprintf(b, "<h5>工具调用 %s <code>%s</code></h5>\n", esc(orDash(tc.Name)), esc(tc.ID))
	fmt.Fprintf(b, "<div>")
	fmt.Fprintf(b, "<span class=\"badge %s\">%s</span>", statusClass(tc.Status), esc(StatusLabel(tc.Status)))
	if tc.Approval != "" {
		fmt.Fprintf(b, "<span class=\"badge\">approval %s</span>", esc(tc.Approval))
	}
	if tc.Duration > 0 {
		fmt.Fprintf(b, "<span class=\"badge\">%s</span>", esc(FormatDuration(tc.Duration.Milliseconds())))
	}
	if tc.ExitCode != 0 {
		fmt.Fprintf(b, "<span class=\"badge bad\">exit %d</span>", tc.ExitCode)
	}
	if tc.Truncated {
		b.WriteString("<span class=\"badge warn\">truncated</span>")
	}
	b.WriteString("</div>\n")
	if tc.OutputRef != "" {
		fmt.Fprintf(b, "<div class=\"muted\">outputRef <code>%s</code></div>\n", esc(tc.OutputRef))
	}
	if tc.ContentRef != "" {
		fmt.Fprintf(b, "<div class=\"muted\">contentRef <code>%s</code></div>\n", esc(tc.ContentRef))
	}
	fmt.Fprintf(b, "<details><summary>参数</summary>\n")
	writeHTMLCode(b, "json", PrettyJSON(tc.Arguments))
	b.WriteString("</details>\n")
	if tc.Error != nil {
		fmt.Fprintf(b, "<div class=\"badge bad\">%s %s</div>\n", esc(tc.Error.Code), esc(tc.Error.Message))
		if tc.Error.Hint != "" {
			fmt.Fprintf(b, "<div class=\"muted\">%s</div>\n", esc(tc.Error.Hint))
		}
	}
	if tc.Content != "" {
		fmt.Fprintf(b, "<details><summary>结果</summary>\n")
		writeHTMLCode(b, "json", PrettyJSON([]byte(tc.Content)))
		b.WriteString("</details>\n")
	}
	if tc.FullOutput != "" {
		fmt.Fprintf(b, "<details><summary>完整输出</summary>\n")
		writeHTMLCode(b, "text", tc.FullOutput)
		b.WriteString("</details>\n")
	}
	b.WriteString("</div>\n")
}

func htmlMessages(b *strings.Builder, msgs []model.Message) {
	for _, m := range msgs {
		htmlMessage(b, m.Role, m.Content, "", m.ToolCalls)
	}
}

func htmlMessage(b *strings.Builder, role model.Role, content, contentRef string, calls []model.ToolCall) {
	fmt.Fprintf(b, "<div class=\"card\"><h5>%s</h5>\n", esc(string(role)))
	if contentRef != "" {
		fmt.Fprintf(b, "<div class=\"muted\">ref <code>%s</code></div>\n", esc(contentRef))
	}
	if role == model.RoleSystem {
		for _, sec := range splitPromptSections(content) {
			if sec.Title != "" {
				fmt.Fprintf(b, "<h5>%s</h5>", esc(sec.Title))
			}
			writeHTMLCode(b, "text", sec.Body)
		}
	} else if content != "" {
		writeHTMLCode(b, "text", content)
	}
	if len(calls) > 0 {
		b.WriteString("<div class=\"muted\">tool calls</div><ul>")
		for _, c := range calls {
			fmt.Fprintf(b, "<li><code>%s</code> %s</li>", esc(c.ID), esc(c.Name))
			writeHTMLCode(b, "json", PrettyJSON(c.Arguments))
		}
		b.WriteString("</ul>")
	}
	b.WriteString("</div>\n")
}

func htmlCompaction(b *strings.Builder, c *Compaction) {
	fmt.Fprintf(b, "<details><summary>上下文压缩 · %s</summary>\n", esc(fmtTime(c.At)))
	fmt.Fprintf(b, "<div class=\"muted\">tokens %d → %d · removed=%d · kept=%d</div>\n", c.BeforeTokens, c.AfterTokens, c.Removed, c.Kept)
	if c.SummaryRef != "" {
		fmt.Fprintf(b, "<div class=\"muted\">summaryRef <code>%s</code></div>\n", esc(c.SummaryRef))
	}
	if c.Summary != "" {
		writeHTMLCode(b, "text", c.Summary)
	}
	b.WriteString("</details>\n")
}

func htmlArtifacts(b *strings.Builder, doc *Document) {
	if len(doc.Artifacts) == 0 {
		return
	}
	fmt.Fprintf(b, "<h2>附录：artifacts（%d）</h2><ul>\n", len(doc.Artifacts))
	for _, a := range doc.Artifacts {
		fmt.Fprintf(b, "<li><code>%s</code>（%d bytes）</li>\n", esc(a.Name), a.Bytes)
	}
	b.WriteString("</ul>\n")
}

func htmlRawEvents(b *strings.Builder, doc *Document) {
	if len(doc.RawEvents) == 0 {
		return
	}
	b.WriteString("<details><summary>原始事件</summary>\n")
	var lines strings.Builder
	for _, ev := range doc.RawEvents {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	writeHTMLCode(b, "jsonl", lines.String())
	b.WriteString("</details>\n")
}

func writeHTMLCode(b *strings.Builder, lang, content string) {
	if strings.TrimSpace(content) == "" {
		b.WriteString("<p class=\"muted\">（空）</p>\n")
		return
	}
	fmt.Fprintf(b, "<pre class=\"code %s\"><code>%s</code></pre>\n", esc(lang), esc(content))
}

func esc(s string) string { return html.EscapeString(s) }

func anchor(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func statusClass(status string) string {
	switch status {
	case "ok", "success":
		return "ok"
	case "failed", "error":
		return "bad"
	case "interrupted", "incomplete":
		return "warn"
	default:
		return ""
	}
}

func fmtTime(t interface{ IsZero() bool }) string {
	type stringer interface{ Format(string) string }
	if t == nil || t.IsZero() {
		return "-"
	}
	if s, ok := t.(stringer); ok {
		return s.Format("2006-01-02 15:04:05")
	}
	return "-"
}
