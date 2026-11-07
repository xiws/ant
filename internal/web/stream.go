package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xiws/ant/internal/session"
)

// handleStream 是 SSE 通知端点：只推 changed/ping，投影逻辑仍由前端按需重取。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !session.ValidID(id) {
		writeAPIError(w, badRequest("session_id_invalid", "session ID 非法"))
		return
	}
	if s.projector.Version(id) == "" {
		if _, err := s.deps.Archive.Snapshot(r.Context(), id); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if s.streams.Add(1) > maxStreams {
		s.streams.Add(-1)
		writeAPIError(w, unavailable("too_many_streams", "同时打开的实时连接过多，请关闭一些标签页"))
		return
	}
	defer s.streams.Add(-1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, &apiErr{class: "internal", code: "stream_unsupported", message: "当前连接不支持流式响应"})
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	last := s.projector.Version(id)
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			version := s.projector.Version(id)
			if version == last {
				continue
			}
			last = version
			payload := map[string]any{
				"lastSeq":   s.metaLastSeq(id),
				"updatedAt": nowUTC(s.deps.Clock).Format(time.RFC3339),
				"version":   version,
			}
			if version == "" {
				payload["deleted"] = true
			}
			if !writeSSE(w, "changed", payload) {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if !writeSSE(w, "ping", map[string]any{}) {
				return
			}
			flusher.Flush()
		}
	}
}

// metaLastSeq 只读 meta.json 的 lastSeq，不解析事件正文。
func (s *Server) metaLastSeq(id string) int64 {
	body, err := os.ReadFile(filepath.Join(s.deps.WorkDir, ".ant", "sessions", id, "meta.json"))
	if err != nil {
		return 0
	}
	var meta struct {
		LastSeq int64 `json:"lastSeq"`
	}
	if json.Unmarshal(body, &meta) != nil {
		return 0
	}
	return meta.LastSeq
}

// writeSSE 写一条 SSE 事件；编码失败时返回 false。
func writeSSE(w http.ResponseWriter, event string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return false
	}
	return true
}
