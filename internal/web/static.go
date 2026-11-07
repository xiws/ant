package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// staticAsset 是嵌入的静态资源与预计算的 ETag。
type staticAsset struct {
	body        []byte
	contentType string
	etag        string
}

// loadStatic 读取嵌入的 static/ 目录，并按相对路径建立索引。
func loadStatic() (map[string]staticAsset, error) {
	assets := make(map[string]staticAsset, 16)
	err := fs.WalkDir(staticFS, "static", func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := staticFS.ReadFile(full)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimPrefix(full, "static/")
		sum := sha256.Sum256(body)
		assets[rel] = staticAsset{
			body:        body,
			contentType: contentTypeForStatic(rel),
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return assets, nil
}

func contentTypeForStatic(rel string) string {
	switch strings.ToLower(path.Ext(rel)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	}
	return "application/octet-stream"
}

// serveIndex 返回 SPA 入口（Cache-Control: no-cache）。
func (s *Server) serveIndex(w http.ResponseWriter) {
	asset, ok := s.assets["index.html"]
	if !ok {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.body)
}

// serveAsset 返回 /assets/* 的静态资源，带 ETag 与 304 协商。
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, rel string) {
	key := path.Clean("assets/" + rel)
	asset, ok := s.assets[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if etagMatches(r.Header.Get("If-None-Match"), asset.etag) {
		w.Header().Set("ETag", asset.etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.body)
}

// etagMatches 做保守的 If-None-Match 匹配（支持逗号分隔与 W/ 前缀）。
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(part)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}
