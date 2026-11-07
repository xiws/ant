package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

// registerRoutes 注册全部只读路由；一期没有任何 POST/PUT/PATCH/DELETE。
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/sessions", s.handleSessions)
	s.mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	s.mux.HandleFunc("GET /api/sessions/{id}/events", s.handleSessionEvents)
	s.mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleStream)
	s.mux.HandleFunc("GET /api/sessions/{id}/artifacts/{path...}", s.handleArtifact)
	s.mux.HandleFunc("GET /api/sessions/{id}/plan", s.handlePlan)
	s.mux.HandleFunc("GET /api/workspace/file", s.handleWorkspaceFile)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/home", s.handleHome)
	s.mux.HandleFunc("GET /api/home/prompt", s.handleHomePrompt)
	s.mux.HandleFunc("GET /api/project", s.handleProject)
	s.mux.HandleFunc("GET /assets/{path...}", s.handleAsset)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.serveIndex(w) })
	s.mux.HandleFunc("/", s.handleFallback)
}

// handleFallback 处理 SPA 路由与未知 API（只读，不提供写方法）。
func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	isAPI := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeAPIError(w, &apiErr{
			class:   "bad_request",
			code:    "method_not_allowed",
			message: "ant serve 一期只读，只接受 GET/HEAD",
			status:  http.StatusMethodNotAllowed,
		})
		return
	}
	if isAPI {
		writeAPIError(w, notFound("not_found", fmt.Sprintf("未知 API 路径 %s", r.URL.Path)))
		return
	}
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.serveIndex(w)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		OK:        true,
		Version:   s.deps.Version,
		Workspace: s.deps.WorkDir,
		Home:      s.deps.HomeDir,
		Readonly:  true,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := parseIntQuery(query.Get("limit"), 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	var mode model.Mode
	if raw := strings.TrimSpace(query.Get("mode")); raw != "" {
		mode = model.Mode(raw)
		if !mode.Valid() {
			writeAPIError(w, badRequest("mode_invalid", "未知模式 "+raw))
			return
		}
	}
	var status session.Status
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		status = session.Status(raw)
	}
	q := query.Get("q")

	etag := `"list-` + s.projector.ListVersion() + `"`
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	list, err := s.projector.List(r.Context(), session.ListFilter{Limit: limit, Mode: mode, Status: status}, q)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, SessionsResponse{
		Workspace: s.deps.WorkDir,
		Home:      s.deps.HomeDir,
		Sessions:  list,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 必须形如 20261030-094704-3f9a2c"))
		return
	}
	query := r.URL.Query()
	beforeTurn := parseIntQuery(query.Get("beforeTurn"), 0)
	limit := parseIntQuery(query.Get("limit"), defaultTurnLimit)
	if limit <= 0 {
		limit = defaultTurnLimit
	}
	if limit > 500 {
		limit = 500
	}

	etag := fmt.Sprintf("%q", fmt.Sprintf("%s-%s-%d-%d", id, s.projector.Version(id), beforeTurn, limit))
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	view, version, err := s.projector.View(r.Context(), id, ViewOptions{BeforeTurn: beforeTurn, Limit: limit})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if version != "" {
		etag = fmt.Sprintf("%q", fmt.Sprintf("%s-%s-%d-%d", id, version, beforeTurn, limit))
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	after := int64(parseIntQuery(r.URL.Query().Get("after"), 0))
	snapshot, err := s.deps.Archive.Snapshot(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	events := make([]model.Event, 0, 64)
	for _, ev := range snapshot.Events {
		if ev.Seq > after {
			events = append(events, ev)
			if len(events) >= 2000 {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, EventsResponse{
		ID:      id,
		LastSeq: snapshot.Meta.LastSeq,
		Events:  events,
	})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	refName, err := artifactRelativeName(r.PathValue("path"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body, err := s.deps.Archive.Artifact(r.Context(), id, model.ArtifactRef{SessionID: id, Name: refName})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if artifactPreviewable(body) {
		w.Header().Set("Content-Type", artifactMediaType(refName))
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(refName)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	if s.projector.Version(id) == "" {
		// events.jsonl 不存在时仍可能只有 meta；直接看目录是否存在。
		if _, err := s.deps.Archive.Snapshot(r.Context(), id); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	view, _, err := s.projector.View(r.Context(), id, ViewOptions{Limit: 500})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	plan := s.projector.readPlan(id, view)
	if plan == nil {
		writeAPIError(w, notFound("plan_not_found",
			"该 session 没有 plan.md；plan 模式完成规划后才会生成"))
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	response, err := s.projector.WorkspaceFile(r.URL.Query().Get("path"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	response := buildConfigView(s.deps.Config)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.projector.HomeFiles())
}

func (s *Server) handleHomePrompt(w http.ResponseWriter, r *http.Request) {
	response, err := s.projector.HomePrompt(r.URL.Query().Get("name"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.projector.ProjectContext())
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, r.PathValue("path"))
}

// buildConfigView 把 config.View 投影为脱敏 JSON：不含 apiKey/headers/env。
func buildConfigView(cfg config.View) ConfigResponse {
	if cfg == nil {
		return ConfigResponse{
			Available: false,
			Error:     "配置无法加载；历史浏览不受影响",
			Warnings:  []string{},
		}
	}
	settings := cfg.Settings()
	view := ConfigResponse{
		Available: true,
		Settings: &SettingsView{
			SchemaVersion:  settings.SchemaVersion,
			DefaultModel:   settings.DefaultModel,
			DefaultMode:    settings.DefaultMode,
			ModelPolicy:    settings.ModelPolicy,
			FallbackModels: settings.FallbackModels,
			Stream:         settings.Stream,
			NodeModels:     settings.NodeModels,
			LogLevel:       settings.Log.Level,
			ApprovalPolicy: settings.Approval.Policy,
			ProjectAuto:    settings.Project.AutoInit,
		},
		Warnings:     cfg.Warnings(),
		DefaultModel: settings.DefaultModel,
		DefaultMode:  settings.DefaultMode,
	}
	if view.Warnings == nil {
		view.Warnings = []string{}
	}
	for _, spec := range cfg.Models() {
		view.Models = append(view.Models, ModelView{
			Name:                      spec.Name,
			ID:                        spec.ID,
			Provider:                  spec.ProviderKey,
			ContextWindow:             spec.ContextWindow,
			MaxOutputTokens:           spec.MaxOutputTokens,
			SupportsTools:             spec.SupportsTools,
			SupportsStreaming:         spec.SupportsStreaming,
			SupportsParallelToolCalls: spec.SupportsParallelToolCalls,
			Reasoning:                 spec.Reasoning,
			DefaultTemperature:        spec.DefaultTemperature,
			Pricing:                   spec.Pricing,
			Notes:                     spec.Notes,
		})
	}
	for _, provider := range cfg.Providers() {
		// 只暴露能力元数据；不返回 APIKey/Headers/APIKeyEnv。
		view.Providers = append(view.Providers, ProviderView{
			Name:       provider.DisplayName(),
			API:        provider.API,
			BaseURL:    provider.BaseURL,
			TimeoutMs:  provider.TimeoutMS,
			ModelCount: len(provider.Models),
		})
	}
	if source, ok := cfg.(interface {
		Origins() map[string]config.Origin
	}); ok {
		view.Origins = make(map[string]string, len(source.Origins()))
		for key, origin := range source.Origins() {
			// 不把 provider 密钥/头部相关键名下发（§22.10.6）。
			if sensitiveConfigKey(key) {
				continue
			}
			view.Origins[key] = string(origin)
		}
	}
	for i, warning := range view.Warnings {
		view.Warnings[i] = scrubConfigText(warning)
	}
	if view.DefaultModel == "" {
		switch {
		case settings.CLIModel != "":
			view.DefaultModel = settings.CLIModel
		case settings.DefaultProvider != "":
			view.DefaultModel = settings.DefaultProvider
		case len(view.Models) == 1:
			view.DefaultModel = view.Models[0].Name
		}
	}
	return view
}

// ListVersion 返回 sessions 目录的轻量版本串（目录与 meta 的 mtime）。
func (p *Projector) ListVersion() string {
	return listVersion(p.workDir)
}

func listVersion(workDir string) string {
	root := filepath.Join(workDir, ".ant", "sessions")
	entries, err := readDirNames(root)
	if err != nil {
		return "0"
	}
	sort.Strings(entries)
	parts := make([]string, 0, len(entries))
	for _, name := range entries {
		parts = append(parts, name+":"+fileStamp(filepath.Join(root, name)))
	}
	return hashShort(strings.Join(parts, ";"))
}

func parseIntQuery(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// sensitiveConfigKey 拒绝把密钥/头部相关配置键名下发给浏览器。
func sensitiveConfigKey(key string) bool {
	lower := strings.ToLower(key)
	for _, word := range []string{"apikey", "api_key", "authorization", "header", "secret", "token"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// scrubConfigText 只保留配置告警的语义，去掉密钥字段名。
func scrubConfigText(text string) string {
	replacer := strings.NewReplacer(
		"apiKeyEnv", "密钥环境变量",
		"apiKey", "密钥",
		"APIKeyEnv", "密钥环境变量",
		"APIKey", "密钥",
	)
	return replacer.Replace(text)
}
