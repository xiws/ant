package trace

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// RenderMarkdown 生成对 GitHub/编辑器友好的 Markdown 报告。
func RenderMarkdown(doc *Document) []byte {
	if doc == nil {
		return []byte("# ant session trace\n\n（空文档）\n")
	}
	var b strings.Builder
	mdTitle(&b, doc)
	mdWarnings(&b, doc)
	mdPlan(&b, doc)

	for _, turn := range doc.Turns {
		mdTurn(&b, turn)
	}
	mdArtifacts(&b, doc)
	mdRawEvents(&b, doc)
	return []byte(b.String())
}

func mdTitle(b *strings.Builder, doc *Document) {
	fmt.Fprintf(b, "# ant session trace · %s\n\n", doc.SessionID)
	fmt.Fprintf(b, "- workspace：`%s`\n", doc.Workspace)
	fmt.Fprintf(b, "- title：%s\n", OneLine(doc.Title, 200))
	fmt.Fprintf(b, "- mode：%s · model：%s · status：%s\n", doc.Mode, doc.Model, StatusLabel(doc.Status))
	fmt.Fprintf(b, "- created：%s · updated：%s\n", fmtTime(doc.CreatedAt), fmtTime(doc.UpdatedAt))
	fmt.Fprintf(b, "- exported：%s · ant：%s\n", fmtTime(doc.ExportedAt), doc.AntVersion)
	fmt.Fprintf(b, "- usage：%s\n\n", FormatUsage(doc.Usage))
}

func mdWarnings(b *strings.Builder, doc *Document) {
	if len(doc.Warnings) == 0 {
		return
	}
	b.WriteString("## Warnings\n\n")
	for _, w := range doc.Warnings {
		fmt.Fprintf(b, "- %s\n", OneLine(w, 500))
	}
	b.WriteString("\n")
}

func mdPlan(b *strings.Builder, doc *Document) {
	if strings.TrimSpace(doc.Plan) == "" {
		return
	}
	b.WriteString("## Plan\n\n")
	if doc.PlanPath != "" {
		fmt.Fprintf(b, "来源：`%s`\n\n", doc.PlanPath)
	}
	codeBlock(b, "markdown", doc.Plan)
}

func mdTurn(b *strings.Builder, turn *Turn) {
	fmt.Fprintf(b, "## Turn %d · %s\n\n", turn.Index, OneLine(turn.Input, 160))
	fmt.Fprintf(b, "- status：%s · exit：%d · usage：%s\n", StatusLabel(turn.Status), turn.ExitCode, FormatUsage(turn.Usage))
	if !turn.StartedAt.IsZero() {
		fmt.Fprintf(b, "- started：%s", fmtTime(turn.StartedAt))
		if !turn.CompletedAt.IsZero() {
			fmt.Fprintf(b, " · completed：%s", fmtTime(turn.CompletedAt))
		}
		b.WriteString("\n")
	}
	if turn.Error != "" {
		fmt.Fprintf(b, "- error：%s\n", OneLine(turn.Error, 300))
	}
	b.WriteString("\n")

	if strings.TrimSpace(turn.Input) != "" {
		b.WriteString("### 输入\n\n")
		codeBlock(b, "text", turn.Input)
	}
	for _, mc := range turn.ModeChanges {
		fmt.Fprintf(b, "- mode changed：%s → %s（%s，confirmed=%v）\n", mc.From, mc.To, mc.Reason, mc.Confirmed)
	}
	if len(turn.ModeChanges) > 0 {
		b.WriteString("\n")
	}
	if len(turn.Approvals) > 0 {
		b.WriteString("### 审批\n\n")
		for _, a := range turn.Approvals {
			parts := []string{"class=" + a.Class}
			if a.Node != "" {
				parts = append(parts, "node="+a.Node)
			}
			if a.Action != "" {
				parts = append(parts, "action="+a.Action)
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
			fmt.Fprintf(b, "- %s · %s\n", fmtTime(a.At), strings.Join(parts, " · "))
		}
		b.WriteString("\n")
	}
	for _, c := range turn.Compactions {
		mdCompaction(b, c)
	}
	for _, n := range turn.Nodes {
		mdNode(b, n)
	}
}

func mdNode(b *strings.Builder, n *Node) {
	fmt.Fprintf(b, "### 节点 %s（run=%s）\n\n", n.Name, n.RunID)
	fmt.Fprintf(b, "- step：`%s` · model：%s · provider：%s\n", n.Step, n.Model, n.Provider)
	fmt.Fprintf(b, "- prompt：version=%s · hash=%s · stable=%s · tools=%s\n",
		orDash(n.PromptVersion), ShortHash(n.PromptHash), ShortHash(n.StableHash), ShortHash(n.ToolsHash))
	if n.InputRef != "" {
		fmt.Fprintf(b, "- inputRef：`%s`\n", n.InputRef)
	}
	if n.Completed != nil {
		fmt.Fprintf(b, "- status：%s · duration：%s · usage：%s\n",
			StatusLabel(n.Completed.Status), FormatDuration(n.Completed.Duration.Milliseconds()), FormatUsage(n.Completed.Usage))
		if n.Completed.Dimension != "" {
			fmt.Fprintf(b, "- dimension：%s\n", n.Completed.Dimension)
		}
		if n.Completed.Error != "" {
			fmt.Fprintf(b, "- error：%s\n", OneLine(n.Completed.Error, 300))
		}
	}
	for _, w := range n.Warnings {
		fmt.Fprintf(b, "- warning：%s\n", OneLine(w, 300))
	}
	b.WriteString("\n")

	if n.Input != nil {
		if n.Input.Dimension != "" {
			fmt.Fprintf(b, "- dimension：%s\n", n.Input.Dimension)
		}
		if n.Input.Request != "" {
			b.WriteString("\n#### 节点输入 request\n\n")
			codeBlock(b, "text", n.Input.Request)
		}
		if n.Input.Plan != "" {
			b.WriteString("\n#### 节点输入 plan\n\n")
			codeBlock(b, "markdown", n.Input.Plan)
		}
		if fb := strings.TrimSpace(string(n.Input.Feedback)); fb != "" && fb != "null" && fb != "[]" {
			b.WriteString("\n#### 节点输入 feedback\n\n")
			codeBlock(b, "json", PrettyJSON(n.Input.Feedback))
		}
		b.WriteString("\n#### 有效 Prompt / 消息（node-input 原文）\n\n")
		mdMessages(b, n.Input.Messages)
		if len(n.Input.Tools) > 0 {
			b.WriteString("#### 工具定义\n\n")
			for _, t := range n.Input.Tools {
				fmt.Fprintf(b, "##### tool %s\n\n", t.Name)
				if t.Description != "" {
					fmt.Fprintf(b, "%s\n\n", t.Description)
				}
				fmt.Fprintf(b, "- mutating=%v · approval=%s · timeout=%s\n\n", t.Mutating, t.Approval, t.Timeout)
				if len(t.Schema) > 0 {
					codeBlock(b, "json", PrettyJSON(t.Schema))
				}
			}
		}
	} else if n.InputRef != "" {
		b.WriteString("> node-input artifact 缺失，无法展示原始 prompt。\n\n")
	}
	for _, msg := range n.Messages {
		mdMessage(b, msg.Role, msg.Content, msg.ContentRef, msg.ToolCalls)
	}
	for _, r := range n.Rounds {
		mdRound(b, r)
	}
	if n.Output != nil {
		b.WriteString("#### 节点输出\n\n")
		if n.Output.Ref != "" {
			fmt.Fprintf(b, "- outputRef：`%s`\n", n.Output.Ref)
		}
		if n.Output.Text != "" {
			codeBlock(b, "markdown", n.Output.Text)
		}
		if len(n.Output.Structured) > 0 && string(n.Output.Structured) != "null" {
			b.WriteString("structured：\n\n")
			codeBlock(b, "json", PrettyJSON(n.Output.Structured))
		}
		if len(n.Output.Changed) > 0 {
			fmt.Fprintf(b, "- changed：%s\n", strings.Join(n.Output.Changed, ", "))
		}
		for _, c := range n.Output.Checks {
			fmt.Fprintf(b, "- check：%s (exit %d)\n", c.Cmd, c.ExitCode)
		}
		b.WriteString("\n")
	}
	if n.Completed != nil && len(n.Completed.Verdict) > 0 && string(n.Completed.Verdict) != "null" {
		b.WriteString("#### Verdict\n\n")
		codeBlock(b, "json", PrettyJSON(n.Completed.Verdict))
	}
}

func mdRound(b *strings.Builder, r *Round) {
	if r.Request != nil {
		source := "原文"
		if r.Reconstructed {
			source = "重建"
		}
		fmt.Fprintf(b, "#### 请求 #%d（%s）\n\n", roundIndex(r), source)
		fmt.Fprintf(b, "- messages=%d · est=%s · tools=%d · stream=%v · attempt=%d\n\n",
			r.Request.MessageCount, FormatCount(r.Request.EstimatedInputTokens),
			len(r.Request.Tools), r.Request.Stream, r.Request.Attempt)
		for _, w := range r.Warnings {
			fmt.Fprintf(b, "> warning：%s\n\n", w)
		}
		if len(r.Messages) > 0 {
			mdMessages(b, r.Messages)
		}
	}
	if r.Response != nil {
		fmt.Fprintf(b, "#### 响应 #%d\n\n", roundIndex(r))
		if r.Response.ErrorClass != "" || r.Response.Code != "" || r.Response.Message != "" {
			fmt.Fprintf(b, "- error：%s/%s %s\n\n", r.Response.ErrorClass, r.Response.Code, OneLine(r.Response.Message, 300))
		} else {
			fmt.Fprintf(b, "- finish=%s · duration=%s · usage=%s · calls=%d\n\n",
				orDash(r.Response.FinishReason), FormatDuration(r.Response.Duration.Milliseconds()),
				FormatUsage(r.Response.Usage), r.Response.ToolCallCount)
		}
	}
	if r.Assistant != nil {
		mdMessage(b, r.Assistant.Role, r.Assistant.Content, r.Assistant.ContentRef, r.Assistant.ToolCalls)
	}
	for _, tc := range r.ToolCalls {
		mdToolCall(b, tc)
	}
}

func roundIndex(r *Round) int {
	if r == nil || r.Index <= 0 {
		return 1
	}
	return r.Index
}

func mdToolCall(b *strings.Builder, tc *ToolCall) {
	fmt.Fprintf(b, "##### 工具调用 %s（id=%s）\n\n", orDash(tc.Name), tc.ID)
	if tc.Approval != "" {
		fmt.Fprintf(b, "- approval：%s\n", tc.Approval)
	}
	fmt.Fprintf(b, "- status：%s · ok=%v · duration=%s · exit=%d · truncated=%v\n",
		orDash(tc.Status), tc.OK, FormatDuration(tc.Duration.Milliseconds()), tc.ExitCode, tc.Truncated)
	if tc.OutputRef != "" {
		fmt.Fprintf(b, "- outputRef：`%s`\n", tc.OutputRef)
	}
	if tc.ContentRef != "" {
		fmt.Fprintf(b, "- contentRef：`%s`\n", tc.ContentRef)
	}
	b.WriteString("\n参数：\n\n")
	codeBlock(b, "json", PrettyJSON(tc.Arguments))
	if tc.Error != nil {
		fmt.Fprintf(b, "错误：`%s` %s", tc.Error.Code, OneLine(tc.Error.Message, 300))
		if tc.Error.Hint != "" {
			fmt.Fprintf(b, "（提示：%s）", OneLine(tc.Error.Hint, 300))
		}
		b.WriteString("\n\n")
	}
	if tc.Content != "" {
		b.WriteString("结果：\n\n")
		codeBlock(b, "json", PrettyJSON([]byte(tc.Content)))
	}
	if tc.FullOutput != "" {
		b.WriteString("完整输出：\n\n")
		codeBlock(b, "text", tc.FullOutput)
	}
}

func mdMessages(b *strings.Builder, msgs []model.Message) {
	for _, m := range msgs {
		mdMessage(b, m.Role, m.Content, "", m.ToolCalls)
	}
}

func mdMessage(b *strings.Builder, role model.Role, content, contentRef string, calls []model.ToolCall) {
	fmt.Fprintf(b, "##### %s\n\n", role)
	if contentRef != "" {
		fmt.Fprintf(b, "ref：`%s`\n\n", contentRef)
	}
	if role == model.RoleSystem {
		for _, sec := range splitPromptSections(content) {
			if sec.Title != "" {
				fmt.Fprintf(b, "###### %s\n\n", sec.Title)
			}
			codeBlock(b, "text", sec.Body)
		}
	} else if content != "" {
		codeBlock(b, "text", content)
	}
	if len(calls) > 0 {
		b.WriteString("tool calls：\n\n")
		for _, c := range calls {
			fmt.Fprintf(b, "- `%s` %s\n", c.ID, c.Name)
			codeBlock(b, "json", PrettyJSON(c.Arguments))
		}
	}
}

func mdCompaction(b *strings.Builder, c *Compaction) {
	fmt.Fprintf(b, "#### 上下文压缩 · %s\n\n", fmtTime(c.At))
	fmt.Fprintf(b, "- tokens：%d → %d · removed=%d · kept=%d\n", c.BeforeTokens, c.AfterTokens, c.Removed, c.Kept)
	if c.SummaryRef != "" {
		fmt.Fprintf(b, "- summaryRef：`%s`\n", c.SummaryRef)
	}
	if c.Summary != "" {
		b.WriteString("\n")
		codeBlock(b, "text", c.Summary)
	} else {
		b.WriteString("\n")
	}
}

func mdArtifacts(b *strings.Builder, doc *Document) {
	if len(doc.Artifacts) == 0 {
		return
	}
	b.WriteString("## 附录：artifacts\n\n")
	for _, a := range doc.Artifacts {
		fmt.Fprintf(b, "- `%s`（%d bytes）\n", a.Name, a.Bytes)
	}
	b.WriteString("\n")
}

func mdRawEvents(b *strings.Builder, doc *Document) {
	if len(doc.RawEvents) == 0 {
		return
	}
	b.WriteString("## 原始事件\n\n")
	var lines strings.Builder
	for _, ev := range doc.RawEvents {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	codeBlock(b, "jsonl", lines.String())
}

type promptSection struct {
	Title string
	Body  string
}

func splitPromptSections(content string) []promptSection {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "\n## ")
	out := make([]promptSection, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if i == 0 && strings.HasPrefix(p, "## ") {
			p = strings.TrimPrefix(p, "## ")
		}
		if p == "" {
			continue
		}
		title, body := p, p
		if idx := strings.IndexByte(p, '\n'); idx >= 0 {
			title = strings.TrimSpace(p[:idx])
			body = strings.TrimSpace(p[idx+1:])
		}
		out = append(out, promptSection{Title: title, Body: body})
	}
	return out
}

func codeBlock(b *strings.Builder, lang, content string) {
	if strings.TrimSpace(content) == "" {
		b.WriteString("（空）\n\n")
		return
	}
	b.WriteString(Fenced(lang, content))
	b.WriteString("\n\n")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
