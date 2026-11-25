package web

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

// uiSessionItem 是 htmx 片段使用的会话摘要。它只包含展示所需字段，
// 与 /api/sessions 的 JSON 投影保持同一数据源，避免前端维护第二套状态机。
type uiSessionItem struct {
	ID          string
	Title       string
	Mode        string
	ModeClass   string
	StatusClass string
	StatusTitle string
	TurnCount   int
	Tokens      string
	Updated     string
}

type uiSessionGroup struct {
	Label string
	Items []uiSessionItem
}

// handleUISessions 返回左栏会话列表 HTML 片段，供 htmx 直接 swap。
// 过滤参数与 /api/sessions 完全一致；所有动态文本均经 html.EscapeString。
func (s *Server) handleUISessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := parseIntQuery(query.Get("limit"), 200)
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	var mode model.Mode
	if raw := strings.TrimSpace(query.Get("mode")); raw != "" {
		mode = model.Mode(raw)
		if !mode.Valid() {
			http.Error(w, "未知模式", http.StatusBadRequest)
			return
		}
	}
	var status session.Status
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		status = session.Status(raw)
	}
	list, err := s.projector.List(r.Context(), session.ListFilter{Limit: limit, Mode: mode, Status: status}, query.Get("q"))
	if err != nil {
		writeAPIError(w, err)
		return
	}

	now := nowUTC(s.deps.Clock)
	groups := make(map[string][]uiSessionItem, 3)
	for _, item := range list {
		statusClass := string(item.Status)
		statusTitle := string(item.Status)
		if item.Live {
			statusClass = "running"
			statusTitle = "运行中"
		} else if item.Stale {
			statusClass = "stale"
			statusTitle = "上次运行中断（锁心跳过期）"
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "（无标题会话）"
		}
		modeName := string(item.Mode)
		if modeName == "" {
			modeName = "—"
		}
		label := uiDayGroup(item.UpdatedAt, now)
		groups[label] = append(groups[label], uiSessionItem{
			ID:          item.ID,
			Title:       title,
			Mode:        modeName,
			ModeClass:   "mode-" + modeName,
			StatusClass: statusClass,
			StatusTitle: statusTitle,
			TurnCount:   item.TurnCount,
			Tokens:      uiTokens(item.Tokens),
			Updated:     uiRelativeTime(item.UpdatedAt, now),
		})
	}

	var b strings.Builder
	hasAny := false
	for _, label := range []string{"今天", "昨天", "更早"} {
		items := groups[label]
		if len(items) == 0 {
			continue
		}
		hasAny = true
		fmt.Fprintf(&b, `<section class="session-group" aria-label="%s"><h2 class="session-group-title">%s</h2>`, html.EscapeString(label), html.EscapeString(label))
		for _, item := range items {
			id := html.EscapeString(item.ID)
			title := html.EscapeString(item.Title)
			fmt.Fprintf(&b, `<a class="session-item" href="#/sessions/%s" data-session-id="%s" data-title="%s" data-mode="%s" data-status="%s" data-updated="%s" data-turns="%d" title="%s · %s">`, id, id, title, html.EscapeString(item.Mode), html.EscapeString(strings.ToLower(item.StatusClass)), html.EscapeString(item.Updated), item.TurnCount, title, id)
			fmt.Fprintf(&b, `<span class="status-dot %s" title="%s" aria-label="%s"></span>`, html.EscapeString(item.StatusClass), html.EscapeString(item.StatusTitle), html.EscapeString(item.StatusTitle))
			b.WriteString(`<span class="session-copy"><span class="session-title">`)
			b.WriteString(title)
			b.WriteString(`</span><span class="session-meta">`)
			fmt.Fprintf(&b, `<span class="badge badge-sm %s">%s</span>`, html.EscapeString(item.ModeClass), html.EscapeString(item.Mode))
			fmt.Fprintf(&b, `<span>%d 轮</span><span>%s tok</span><span>%s</span>`, item.TurnCount, html.EscapeString(item.Tokens), html.EscapeString(item.Updated))
			b.WriteString(`</span><span class="session-id">`)
			b.WriteString(id)
			b.WriteString(`</span></span></a>`)
		}
		b.WriteString(`</section>`)
	}
	if !hasAny {
		if query.Get("q") != "" || mode != "" || status != "" {
			b.WriteString(`<div class="empty-state"><p>没有匹配的会话</p><p class="small muted">调整搜索条件，或清空筛选。</p></div>`)
		} else {
			b.WriteString(`<div class="empty-state"><p>当前 workspace 还没有会话</p><p class="small muted">在终端运行 <code>ant "你的任务"</code> 创建第一个会话。</p></div>`)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

// handleUIStatus 返回左栏底部“本地模式”卡片，由 htmx 在首屏和刷新时加载。
func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(s.deps.Version)
	if version == "" {
		version = "vdev"
	} else if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	workspace := html.EscapeString(s.deps.WorkDir)
	host := html.EscapeString(r.Host)
	var b strings.Builder
	b.WriteString(`<div class="local-card"><div class="local-status-line"><span class="status-dot running" aria-hidden="true"></span><strong>本地模式 · 只读</strong></div>`)
	fmt.Fprintf(&b, `<div class="path" title="%s">%s</div>`, workspace, workspace)
	fmt.Fprintf(&b, `<div class="local-meta"><span>ant %s</span><span>·</span><span>%s</span></div>`, html.EscapeString(version), host)
	b.WriteString(`<div class="local-actions"><button class="btn btn-xs" type="button" data-action="local-config">本机配置</button><button class="btn btn-xs" type="button" data-action="settings">设置</button></div></div>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func uiDayGroup(value, now time.Time) string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	target := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, now.Location())
	diff := int(today.Sub(target).Hours() / 24)
	switch {
	case diff <= 0:
		return "今天"
	case diff == 1:
		return "昨天"
	default:
		return "更早"
	}
}

func uiRelativeTime(value, now time.Time) string {
	diff := now.Sub(value)
	switch {
	case diff < time.Minute:
		return "刚刚"
	case diff < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(diff/time.Minute))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(diff/time.Hour))
	case diff < 7*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(diff/(24*time.Hour)))
	default:
		return value.Format("01-02")
	}
}

func uiTokens(value int) string {
	switch {
	case value < 1000:
		return fmt.Sprintf("%d", value)
	case value < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(value)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(value)/1_000_000)
	}
}
