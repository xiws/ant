package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/projectctx"
	"github.com/xiws/ant/internal/prompt"
	"github.com/xiws/ant/internal/session"
)

func (a *App) runModels(ctx context.Context) int {
	models := a.cfg.Models()
	if a.opts.jsonOut {
		b, _ := json.MarshalIndent(models, "", "  ")
		fmt.Fprintln(a.stdio.Out, string(b))
		return 0
	}
	if len(models) == 0 {
		fmt.Fprintln(a.stdio.Out, "未配置模型；请在 .ant/models.json 或 $ANT_HOME/models.json 中配置 providers[].models[]")
		return 0
	}
	def := initialAlias(a.cfg, a.opts)
	fmt.Fprintf(a.stdio.Out, "%-28s %-14s %-28s %8s %6s %6s\n", "NAME", "PROVIDER", "ID", "CONTEXT", "TOOLS", "STREAM")
	for _, m := range models {
		name := m.Name
		if name == def {
			name = "*" + name
		}
		fmt.Fprintf(a.stdio.Out, "%-28s %-14s %-28s %8d %6v %6v\n",
			name, m.ProviderKey, m.ID, m.ContextWindow, m.SupportsTools, m.SupportsStreaming)
	}
	for _, w := range a.cfg.Warnings() {
		a.render.Notice(w)
	}
	return 0
}

func (a *App) runConfig(ctx context.Context) int {
	payload := map[string]any{
		"settings": a.cfg.Settings(),
		"models":   a.cfg.Models(),
		"warnings": a.cfg.Warnings(),
	}
	if c, ok := a.cfg.(interface {
		Origins() map[string]config.Origin
	}); ok {
		payload["origins"] = c.Origins()
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.stdio.Out, string(b))
	return 0
}

func (a *App) runSessions(ctx context.Context) int {
	if a.store == nil {
		return a.fail(usageErrorf("内部错误：store 未装配"))
	}
	list, err := a.store.List(ctx, session.ListFilter{
		Limit:  a.opts.limit,
		Mode:   a.opts.filterMode,
		Status: a.opts.filterStatus,
	})
	if err != nil {
		return a.fail(err)
	}
	if a.opts.jsonOut {
		b, _ := json.MarshalIndent(list, "", "  ")
		fmt.Fprintln(a.stdio.Out, string(b))
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintln(a.stdio.Out, "当前 workspace 没有 session")
		return 0
	}
	fmt.Fprintf(a.stdio.Out, "%-28s %-9s %-6s %-22s %6s %8s %-12s %s\n",
		"ID", "UPDATED", "MODE", "MODEL", "TURNS", "TOKENS", "STATUS", "TITLE")
	for _, s := range list {
		fmt.Fprintf(a.stdio.Out, "%-28s %-9s %-6s %-22s %6d %8d %-12s %s\n",
			s.ID,
			s.UpdatedAt.Local().Format("15:04"),
			s.Mode,
			s.Model,
			s.TurnCount,
			s.Tokens,
			s.Status,
			truncateLine(s.Title, 40),
		)
	}
	return 0
}

func (a *App) runShow(ctx context.Context) int {
	if a.store == nil {
		return a.fail(usageErrorf("内部错误：store 未装配"))
	}
	if len(a.opts.rest) == 0 {
		return a.fail(usageErrorf("用法：ant show <session-id>"))
	}
	detail, err := a.store.Show(ctx, a.opts.rest[0])
	if err != nil {
		return a.fail(err)
	}
	if a.opts.jsonOut {
		b, _ := json.MarshalIndent(detail, "", "  ")
		fmt.Fprintln(a.stdio.Out, string(b))
		return 0
	}
	fmt.Fprintf(a.stdio.Out, "ID:        %s\n", detail.ID)
	fmt.Fprintf(a.stdio.Out, "Workspace: %s\n", detail.Workspace)
	fmt.Fprintf(a.stdio.Out, "Status:    %s · mode=%s · model=%s\n", detail.Status, detail.Mode, detail.Model)
	fmt.Fprintf(a.stdio.Out, "Turns:     %d\n", detail.TurnCount)
	fmt.Fprintf(a.stdio.Out, "Tokens:    in %d / out %d / cached %d / reasoning %d\n",
		detail.Usage.Input, detail.Usage.Output, detail.Usage.Cached, detail.Usage.Reasoning)
	fmt.Fprintf(a.stdio.Out, "Title:     %s\n", detail.Title)
	if detail.PlanPath != "" {
		fmt.Fprintf(a.stdio.Out, "Plan:      %s\n", detail.PlanPath)
	}
	for _, ap := range detail.Approvals {
		fmt.Fprintf(a.stdio.Out, "Approval:  %s %s/%s at %s\n",
			ap.Class, ap.Decision, ap.Scope, ap.At.Local().Format("2006-01-02 15:04:05"))
	}
	for _, art := range detail.Artifacts {
		fmt.Fprintf(a.stdio.Out, "Artifact:  %s (%d bytes)\n", art.Name, art.Bytes)
	}
	if a.opts.showEvents {
		a.printEvents(ctx, detail.ID)
	}
	return 0
}

func (a *App) printEvents(ctx context.Context, id string) {
	sess, _, err := a.store.Open(ctx, session.OpenOptions{ID: id, Lock: false})
	if err != nil {
		a.render.Notice("读取事件失败：" + formatError(err))
		return
	}
	defer sess.Close()
	events, err := sess.Events(ctx)
	if err != nil {
		a.render.Notice("读取事件失败：" + formatError(err))
		return
	}
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		fmt.Fprintln(a.stdio.Out, string(b))
	}
}

func (a *App) runInit(ctx context.Context) int {
	if a.builder == nil {
		return a.fail(usageErrorf("内部错误：projectctx builder 未装配"))
	}
	doc, err := a.builder.Ensure(ctx, projectctx.EnsureOptions{Force: a.opts.force, Explicit: true})
	if err != nil {
		return a.fail(err)
	}
	if doc.Path == "" {
		fmt.Fprintln(a.stdio.Out, "项目上下文未生成（project.autoInit=false？）")
		return 0
	}
	a.projectMu.Lock()
	a.projectDoc = prompt.ProjectDoc{Body: doc.Body, Hash: doc.Hash}
	a.projectMu.Unlock()
	fmt.Fprintf(a.stdio.Out, "%s（%s，%d bytes）\n", doc.Path, doc.Hash, len(doc.Body))
	return 0
}

func truncateLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
