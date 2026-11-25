package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiws/ant/internal/session"
	"github.com/xiws/ant/internal/trace"
	"github.com/xiws/ant/internal/version"
)

// runExport 实现 `ant export`：把 session 的事件与 artifact 导出为
// 自包含 HTML 或 Markdown（docs/13-observability.md §15.5）。
func (a *App) runExport(ctx context.Context) int {
	if a.store == nil {
		return a.fail(usageErrorf("内部错误：store 未装配"))
	}
	if a.opts.jsonOut {
		return a.fail(usageErrorf("ant export 不支持 --json；请使用 --format md|html 或 -o -"))
	}
	if len(a.opts.rest) > 0 && a.opts.sessionID != "" {
		return a.fail(usageErrorf("ant export 不能同时使用位置参数与 -s/--session"))
	}

	id := ""
	if len(a.opts.rest) > 0 {
		id = a.opts.rest[0]
	} else {
		id = a.opts.sessionID
	}
	if id == "" {
		latest, err := a.store.Latest(ctx)
		if err != nil {
			return a.fail(err)
		}
		id = latest.ID
	}

	format, err := exportFormat(a.opts.format, a.opts.output)
	if err != nil {
		return a.fail(err)
	}
	detail, err := a.store.Show(ctx, id)
	if err != nil {
		return a.fail(err)
	}
	fullID := detail.ID
	if fullID == "" {
		fullID = id
	}

	sess, _, err := a.store.Open(ctx, session.OpenOptions{ID: fullID, ReadOnly: true})
	if err != nil {
		return a.fail(err)
	}
	defer sess.Close()

	plan := ""
	if detail.PlanPath != "" {
		body, readErr := os.ReadFile(detail.PlanPath)
		if readErr != nil {
			a.render.Notice("读取 plan.md 失败：" + formatError(readErr))
		} else {
			plan = string(body)
		}
	}

	doc, err := trace.Build(ctx, sess, trace.Options{
		PlanPath:   detail.PlanPath,
		Plan:       plan,
		Artifacts:  detail.Artifacts,
		IncludeRaw: a.opts.showEvents,
		AntVersion: version.String(),
		ExportedAt: a.clock(),
	})
	if err != nil {
		return a.fail(err)
	}

	var data []byte
	if format == "md" {
		data = trace.RenderMarkdown(doc)
	} else {
		data = trace.RenderHTML(doc)
	}

	outPath := strings.TrimSpace(a.opts.output)
	if outPath == "" {
		outPath = filepath.Join(a.workDir(), ".ant", "sessions", fullID, "trace."+exportExt(format))
	}
	if outPath == "-" {
		if _, err := a.stdio.Out.Write(data); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if info, statErr := os.Stat(outPath); statErr == nil && info.IsDir() {
		outPath = filepath.Join(outPath, "trace."+exportExt(format))
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return a.fail(err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return a.fail(err)
	}
	_ = os.Chmod(outPath, 0o600)
	display := outPath
	if abs, absErr := filepath.Abs(outPath); absErr == nil {
		display = abs
	}
	fmt.Fprintf(a.stdio.Out, "已导出：%s（%s，%d bytes）\n", display, format, len(data))
	a.render.Notice("导出内容包含 prompt、代码与工具输出，请勿提交到公开仓库")
	if string(detail.Status) == string(session.StatusRunning) {
		a.render.Notice("session 正在运行，导出的是当前快照")
	}
	return 0
}

func exportFormat(explicit, output string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(explicit))
	if f != "" {
		switch f {
		case "md", "markdown":
			return "md", nil
		case "html", "htm":
			return "html", nil
		default:
			return "", usageErrorf("--format 取值必须是 md|html，实际 %q", explicit)
		}
	}
	if strings.TrimSpace(output) == "-" {
		return "md", nil
	}
	switch strings.ToLower(filepath.Ext(output)) {
	case ".md", ".markdown":
		return "md", nil
	case ".html", ".htm":
		return "html", nil
	}
	return "html", nil
}

func exportExt(format string) string {
	if format == "md" {
		return "md"
	}
	return "html"
}
