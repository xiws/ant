package web

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/session"
)

// Deps 是 Web 服务的显式依赖，全部由组合根注入。
type Deps struct {
	WorkDir string
	HomeDir string
	Config  config.View // 可为 nil：配置非法时仍可浏览历史
	Archive session.Archive
	Clock   func() time.Time
	Logger  logx.Logger
	Version string
}

// Server 是本地只读 Web 服务。
type Server struct {
	deps      Deps
	projector *Projector
	mux       *http.ServeMux
	assets    map[string]staticAsset
	streams   atomic.Int32
}

// 只读服务的边界上限。
const (
	// maxStreams 是同时打开的 SSE 连接上限。
	maxStreams = 32
	// shutdownTimeout 是优雅关闭的最长等待时间。
	shutdownTimeout = 5 * time.Second
)

// New 构造 Server；WorkDir 与 Archive 必填。
func New(deps Deps) (*Server, error) {
	if strings.TrimSpace(deps.WorkDir) == "" {
		return nil, &apiErr{class: "internal", code: "workspace_required", message: "web: WorkDir 不能为空"}
	}
	if deps.Archive == nil {
		return nil, &apiErr{class: "internal", code: "archive_required", message: "web: Archive 不能为空"}
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = logx.Nop()
	}
	assets, err := loadStatic()
	if err != nil {
		return nil, err
	}
	server := &Server{
		deps:      deps,
		projector: NewProjector(deps.Archive, deps.Config, deps.WorkDir, deps.HomeDir, deps.Clock),
		mux:       http.NewServeMux(),
		assets:    assets,
	}
	server.registerRoutes()
	return server, nil
}

// Handler 返回带完整中间件的 http.Handler。
//
// 中间件顺序：recover → request log → security headers → host check → mux。
func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux
	handler = s.hostCheck(handler)
	handler = s.securityHeaders(handler)
	handler = s.requestLog(handler)
	handler = s.recover(handler)
	return handler
}

// Serve 在给定 listener 上提供服务，并在 ctx 取消时优雅关闭。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(nopWriter{}, "", 0),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		serveErr := <-errCh
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// recover 捕获 panic，返回 500 JSON；stack 只写日志，不返回给浏览器。
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.deps.Logger.Error("http panic", "path", r.URL.Path, "panic", recovered, "stack", string(debug.Stack()))
				writeAPIError(w, &apiErr{class: "internal", code: "internal", message: "服务内部错误", status: http.StatusInternalServerError})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLog 只记录方法、路径、状态与耗时，不记录正文。
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.deps.Logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"durationMs", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

// Flush 让 SSE 通过 statusRecorder 仍然可刷新。
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// securityHeaders 统一附加安全响应头；不设置任何 CORS 或 Cookie。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		header.Set("Cache-Control", "no-store")
		header.Set("Server", "ant")
		next.ServeHTTP(w, r)
	})
}

// hostCheck 只允许 localhost/127.0.0.1/[::1]（含端口），防 DNS rebinding。
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedHost(r.Host) {
			writeAPIError(w, &apiErr{
				class:   "bad_request",
				code:    "host_not_allowed",
				message: "只允许通过 localhost/127.0.0.1/[::1] 访问本地服务",
				status:  http.StatusForbidden,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAllowedHost 判断 Host 头是否为回环地址。
func isAllowedHost(hostport string) bool {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return false
	}
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
