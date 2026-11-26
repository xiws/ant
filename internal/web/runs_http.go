package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

const maxWriteBody = 1 << 20 // 1 MiB

type runRequestBody struct {
	Input string     `json:"input"`
	Mode  model.Mode `json:"mode,omitempty"`
	Model string     `json:"model,omitempty"`
}

type approvalBody struct {
	Decision string `json:"decision"`
}

type askBody struct {
	SelectedID string `json:"selectedId,omitempty"`
	Text       string `json:"text,omitempty"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	s.handleStartRun(w, r, "")
}

func (s *Server) handleSessionTurn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	s.handleStartRun(w, r, id)
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.runs == nil {
		writeAPIError(w, unavailable("executor_unavailable", "当前 serve 未启用执行器，只能浏览历史"))
		return
	}
	if !s.authorizeWrite(w, r) {
		return
	}
	var body runRequestBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	body.Input = strings.TrimSpace(body.Input)
	if body.Input == "" {
		writeAPIError(w, badRequest("input_required", "输入不能为空"))
		return
	}
	if body.Mode != "" && !body.Mode.Valid() {
		writeAPIError(w, badRequest("mode_invalid", "未知模式 "+string(body.Mode)))
		return
	}
	body.Model = strings.TrimSpace(body.Model)

	result, err := s.runs.Start(r.Context(), RunRequest{
		SessionID: sessionID,
		Input:     body.Input,
		Mode:      body.Mode,
		Model:     body.Model,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Location", "/api/sessions/"+result.SessionID)
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		writeAPIError(w, unavailable("executor_unavailable", "当前 serve 未启用执行器"))
		return
	}
	if !s.authorizeWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	cancelled := s.runs.Cancel(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": id,
		"cancelled": cancelled,
	})
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		writeAPIError(w, unavailable("executor_unavailable", "当前 serve 未启用执行器"))
		return
	}
	if !s.authorizeWrite(w, r) {
		return
	}
	var body approvalBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if err := s.runs.Approve(r.PathValue("id"), strings.TrimSpace(body.Decision)); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		writeAPIError(w, unavailable("executor_unavailable", "当前 serve 未启用执行器"))
		return
	}
	if !s.authorizeWrite(w, r) {
		return
	}
	var body askBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	body.SelectedID = strings.TrimSpace(body.SelectedID)
	body.Text = strings.TrimSpace(body.Text)
	if body.SelectedID == "" && body.Text == "" {
		writeAPIError(w, badRequest("ask_answer_required", "请选择选项或输入回答"))
		return
	}
	if err := s.runs.Answer(r.PathValue("id"), body.SelectedID, body.Text); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authorizeWrite 为本地写接口提供无账号场景下的 CSRF/跨站防护。
func (s *Server) authorizeWrite(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeAPIError(w, unsupportedMediaType("content_type_required", "写接口只接受 application/json"))
		return false
	}
	if r.Header.Get("X-ANT-Request") != "1" {
		writeAPIError(w, forbidden("request_header_required", "缺少 X-ANT-Request: 1"))
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeAPIError(w, forbidden("origin_not_allowed", "只接受同源请求"))
			return false
		}
	}
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" &&
		site != "same-origin" && site != "none" {
		writeAPIError(w, forbidden("cross_site_request", "拒绝跨站写请求"))
		return false
	}
	return true
}

// decodeJSONBody 限制请求体大小并拒绝多 JSON 值，避免请求走私式歧义。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeAPIError(w, badRequest("json_body_required", "请求体不能为空"))
			return false
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, tooLarge("request_too_large", "请求体超过 1 MiB"))
			return false
		}
		writeAPIError(w, badRequest("json_invalid", "请求体不是合法 JSON"))
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeAPIError(w, badRequest("json_multiple_values", "请求体只能包含一个 JSON 对象"))
		return false
	}
	return true
}
