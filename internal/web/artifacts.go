package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxWorkspacePreview 是 /api/workspace/file 单文件预览上限（§22.10.3）。
	maxWorkspacePreview int64 = 2 << 20
	// maxHomePrompt 是 home prompt 预览上限。
	maxHomePrompt int64 = 256 << 10
	// maxPlanBytes 是计划文件预览上限。
	maxPlanBytes int64 = 1 << 20
	// maxArtifactsListed 是单 session 产物列表的条数上限。
	maxArtifactsListed = 2000
)

// homeWhitelist 是 /api/home 与 /api/home/prompt 允许访问的 home 相对路径。
var homeWhitelist = []string{
	"settings.json",
	"models.json",
	"prompt.md",
	"prompts/code.md",
	"prompts/ask.md",
	"prompts/verifier.md",
	"prompts/deliberate.md",
	"prompts/aggregation.md",
}

// osStat 是 os.Stat 的极薄包装，便于测试替换（当前不使用）。
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

// populateArtifacts 填充 session 的产物列表（只读 artifacts/）。
func (p *Projector) populateArtifacts(view *SessionView) {
	view.Artifacts = listArtifactViews(sessionDir(p.workDir, view.ID))
}

// listArtifactViews 列出 artifacts/ 下的常规文件；不跟随符号链接。
func listArtifactViews(dir string) []ArtifactView {
	root := filepath.Join(dir, "artifacts")
	out := make([]ArtifactView, 0, 16)
	count := 0
	_ = filepath.WalkDir(root, func(full string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if count >= maxArtifactsListed {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, full)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		artifact := ArtifactView{
			Name:      rel,
			Bytes:     info.Size(),
			ModTime:   info.ModTime().UTC(),
			MediaType: mediaTypeFor(rel),
		}
		artifact.Previewable = artifact.MediaType != "" && (strings.HasPrefix(artifact.MediaType, "text/") ||
			artifact.MediaType == "application/json" || artifact.MediaType == "application/x-ndjson")
		if artifact.Previewable && info.Size() <= 1<<20 {
			if body, readErr := os.ReadFile(full); readErr == nil {
				sum := sha256.Sum256(body)
				artifact.SHA256 = "sha256:" + hex.EncodeToString(sum[:])
				if !isText(body) {
					artifact.Previewable = false
				}
			}
		}
		out = append(out, artifact)
		count++
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readPlan 读取计划文件，按 §22.5.6 的路径回退顺序：
// artifacts/plan.md → artifacts/plan/round-N.md → <session>/plan.md。
func (p *Projector) readPlan(id string, view *SessionView) *PlanView {
	dir := sessionDir(p.workDir, id)
	type candidate struct {
		rel  string
		full string
	}
	var candidates []candidate
	var referenced []string
	add := func(rel string) {
		rel = filepath.ToSlash(rel)
		candidates = append(candidates, candidate{rel: rel, full: filepath.Join(dir, filepath.FromSlash(rel))})
	}
	if view != nil {
		for _, turn := range view.Turns {
			if strings.TrimSpace(turn.PlanPath) == "" {
				continue
			}
			full := turn.PlanPath
			if !filepath.IsAbs(full) {
				full = filepath.Join(p.workDir, full)
			}
			if !withinRoot(dir, full) {
				continue
			}
			referenced = append(referenced, full)
		}
	}
	add("artifacts/plan.md")
	if rounds, err := filepath.Glob(filepath.Join(dir, "artifacts", "plan", "round-*.md")); err == nil && len(rounds) > 0 {
		sort.Strings(rounds)
		for i := len(rounds) - 1; i >= 0; i-- {
			rel, _ := filepath.Rel(dir, rounds[i])
			add(rel)
		}
	}
	add("plan.md")

	for _, item := range candidates {
		info, err := os.Lstat(item.full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		body, err := readCapped(item.full, maxPlanBytes)
		if err != nil {
			continue
		}
		return &PlanView{
			Found:  true,
			Source: item.rel,
			Name:   filepath.Base(item.rel),
			Path:   item.full,
			Body:   string(body),
		}
	}
	for _, full := range referenced {
		rel, _ := filepath.Rel(p.workDir, full)
		return &PlanView{
			Found:   false,
			Missing: true,
			Source:  filepath.ToSlash(rel),
			Name:    filepath.Base(full),
			Path:    full,
		}
	}
	return nil
}

// WorkspaceFile 读取 workspace 内的文件预览；所有路径都经过双重校验。
func (p *Projector) WorkspaceFile(rel string) (*WorkspaceFileResponse, error) {
	cleaned, err := cleanQueryPath(rel)
	if err != nil {
		return nil, err
	}
	if deniedWorkspacePath(cleaned) {
		return nil, &apiErr{
			class:   "bad_request",
			code:    "path_forbidden",
			message: fmt.Sprintf("禁止通过 Web 读取 %s", cleaned),
			hint:    "配置请使用 /api/config，session 与日志请使用对应接口",
		}
	}
	full, err := safePathWithin(p.workDir, cleaned)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &apiErr{class: "not_found", code: "file_not_found", message: "文件不存在：" + cleaned}
		}
		return nil, &apiErr{class: "internal", code: "file_read_failed", message: "无法读取文件：" + cleaned}
	}
	if info.IsDir() {
		return nil, &apiErr{class: "bad_request", code: "file_is_dir", message: cleaned + " 是目录"}
	}
	body, err := readCapped(full, maxWorkspacePreview)
	if err != nil {
		return nil, &apiErr{class: "too_large", code: "file_too_large", message: fmt.Sprintf("文件超过 %d 字节，无法预览", maxWorkspacePreview)}
	}
	response := &WorkspaceFileResponse{
		Path:      cleaned,
		Bytes:     info.Size(),
		ModTime:   info.ModTime().UTC(),
		Truncated: info.Size() > int64(len(body)),
	}
	if !isText(body) {
		response.Binary = true
		return response, nil
	}
	response.Content = string(body)
	sum := sha256.Sum256(body)
	response.SHA256 = "sha256:" + hex.EncodeToString(sum[:])
	return response, nil
}

// HomeFiles 返回 home 白名单文件元数据；不返回内容。
func (p *Projector) HomeFiles() *HomeResponse {
	if strings.TrimSpace(p.homeDir) == "" {
		return &HomeResponse{Files: []HomeFileView{}}
	}
	files := make([]HomeFileView, 0, len(homeWhitelist))
	for _, rel := range homeWhitelist {
		if !isAllowedHomeFile(rel) {
			continue
		}
		full := filepath.Join(p.homeDir, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, HomeFileView{
			Name:    rel,
			Bytes:   info.Size(),
			ModTime: info.ModTime().UTC(),
			Exists:  true,
		})
	}
	return &HomeResponse{Path: p.homeDir, Files: files}
}

// HomePrompt 只返回 prompt.md 或 prompts/<known>.md。
func (p *Projector) HomePrompt(name string) (*HomePromptResponse, error) {
	if strings.TrimSpace(p.homeDir) == "" {
		return nil, &apiErr{class: "not_found", code: "home_unavailable", message: "无法定位 $ANT_HOME"}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "prompt.md"
	}
	name = filepath.ToSlash(name)
	if !isAllowedHomeFile(name) || name == "settings.json" || name == "models.json" {
		return nil, &apiErr{
			class:   "bad_request",
			code:    "home_path_forbidden",
			message: fmt.Sprintf("不允许读取 home 文件 %q", name),
			hint:    "只允许 prompt.md 与 prompts/<node>.md",
		}
	}
	full := filepath.Join(p.homeDir, filepath.FromSlash(name))
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil, &apiErr{class: "not_found", code: "file_not_found", message: "home 文件不存在：" + name}
	}
	if info.Size() > maxHomePrompt {
		return nil, &apiErr{class: "too_large", code: "file_too_large", message: fmt.Sprintf("文件超过 %d 字节", maxHomePrompt)}
	}
	body, err := readCapped(full, maxHomePrompt)
	if err != nil {
		return nil, &apiErr{class: "internal", code: "file_read_failed", message: "无法读取 home 文件：" + name}
	}
	return &HomePromptResponse{Name: name, Path: full, Content: string(body), Bytes: int64(len(body))}, nil
}

// ProjectContext 读取 .ant/project.md 与 .ant/project.meta.json。
func (p *Projector) ProjectContext() *ProjectResponse {
	rel := filepath.Join(".ant", "project.md")
	full := filepath.Join(p.workDir, rel)
	response := &ProjectResponse{Path: rel}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return response
	}
	response.Exists = true
	response.Bytes = info.Size()
	response.ModTime = info.ModTime().UTC()
	if body, readErr := readCapped(full, maxWorkspacePreview); readErr == nil && isText(body) {
		response.Body = string(body)
	}
	if raw, readErr := os.ReadFile(filepath.Join(p.workDir, ".ant", "project.meta.json")); readErr == nil {
		var meta struct {
			Hash        string `json:"hash"`
			GeneratedAt string `json:"generatedAt"`
		}
		if json.Unmarshal(raw, &meta) == nil {
			response.Hash = meta.Hash
			response.GeneratedAt = meta.GeneratedAt
		}
	} else if sum := sha256.Sum256([]byte(response.Body)); len(response.Body) > 0 {
		response.Hash = "sha256:" + hex.EncodeToString(sum[:])
	}
	return response
}

// isAllowedHomeFile 报告 home 相对路径是否在白名单内。
func isAllowedHomeFile(rel string) bool {
	cleaned := path.Clean(filepath.ToSlash(rel))
	for _, allowed := range homeWhitelist {
		if cleaned == allowed {
			return true
		}
	}
	return false
}

// deniedWorkspacePath 拒绝 .git、sessions、logs 与配置文件原文件。
func deniedWorkspacePath(rel string) bool {
	cleaned := path.Clean(filepath.ToSlash(rel))
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return true
	}
	if cleaned == ".ant/sessions" || strings.HasPrefix(cleaned, ".ant/sessions/") {
		return true
	}
	if cleaned == ".ant/logs" || strings.HasPrefix(cleaned, ".ant/logs/") {
		return true
	}
	switch cleaned {
	case ".ant/settings.json", ".ant/models.json":
		return true
	}
	return false
}

// cleanQueryPath 校验 `path` 查询参数：长度、控制字符、相对路径、..。
func cleanQueryPath(rel string) (string, error) {
	if len(rel) > 1024 {
		return "", &apiErr{class: "bad_request", code: "path_invalid", message: "path 超过 1024 字节"}
	}
	if rel == "" {
		return "", &apiErr{class: "bad_request", code: "path_required", message: "缺少 path 参数"}
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return "", &apiErr{class: "bad_request", code: "path_invalid", message: "path 含控制字符"}
		}
	}
	slashed := filepath.ToSlash(strings.TrimSpace(rel))
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", &apiErr{class: "bad_request", code: "path_invalid", message: "path 必须是 workspace 相对路径"}
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", &apiErr{class: "bad_request", code: "path_invalid", message: "path 不能包含 .."}
		}
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "../") {
		return "", &apiErr{class: "bad_request", code: "path_invalid", message: "path 无效"}
	}
	return cleaned, nil
}

// safePathWithin 解析符号链接后确认目标仍在 root 内。
func safePathWithin(root, rel string) (string, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", &apiErr{class: "internal", code: "workspace_unavailable", message: "无法解析 workspace 目录"}
	}
	full := filepath.Join(rootReal, filepath.FromSlash(rel))
	if !withinRoot(rootReal, full) {
		return "", &apiErr{class: "bad_request", code: "path_escape", message: "path 逃逸 workspace"}
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", &apiErr{class: "not_found", code: "file_not_found", message: "文件不存在：" + rel}
		}
		return "", &apiErr{class: "internal", code: "file_read_failed", message: "无法解析文件路径：" + rel}
	}
	if !withinRoot(rootReal, real) {
		return "", &apiErr{class: "bad_request", code: "path_escape", message: "符号链接逃逸 workspace"}
	}
	return real, nil
}

func withinRoot(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// readCapped 读取最多 limit 字节；超过时返回完整读取的前 limit 字节。
func readCapped(full string, limit int64) ([]byte, error) {
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// isText 用 NUL 字节与 UTF-8 有效性做保守判断。
func isText(body []byte) bool {
	if len(body) == 0 {
		return true
	}
	sample := body
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	for _, b := range sample {
		if b == 0 {
			return false
		}
	}
	return utf8.Valid(sample) || len(sample) < len(body)
}

func mediaTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json"
	case ".jsonl", ".ndjson":
		return "application/x-ndjson"
	case ".txt", ".log", ".out", ".diff", ".patch":
		return "text/plain; charset=utf-8"
	case ".go", ".js", ".ts", ".css", ".html", ".xml", ".yaml", ".yml", ".toml", ".sh":
		return "text/plain; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// artifactMediaType 用于 artifact 响应；未知类型按二进制下载。
func artifactMediaType(name string) string {
	if mt := mediaTypeFor(name); mt != "" {
		return mt
	}
	return "application/octet-stream"
}

// artifactRelativeName 把 URL 里的相对 artifacts/ 名字还原为 ref.Name。
func artifactRelativeName(rel string) (string, error) {
	cleaned, err := cleanQueryPath(rel)
	if err != nil {
		return "", err
	}
	if cleaned == "artifacts" || strings.HasPrefix(cleaned, "artifacts/") {
		return cleaned, nil
	}
	return "artifacts/" + cleaned, nil
}

// artifactPreviewable 报告 artifact 是否按文本返回。
func artifactPreviewable(body []byte) bool { return isText(body) }

// nowUTC 统一时间来源，便于测试。
func nowUTC(clock func() time.Time) time.Time {
	if clock == nil {
		return time.Now().UTC()
	}
	return clock().UTC()
}

// readDirNames 只读取目录项名称；目录不存在时返回错误。
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// fileStamp 返回 meta.json 的 size/mtime；缺失时返回 "0"。
func fileStamp(dir string) string {
	parts := make([]string, 0, 2)
	for _, name := range []string{"meta.json", "events.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			parts = append(parts, "0")
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano()))
	}
	return strings.Join(parts, "-")
}

// hashShort 返回字符串的短哈希，用于 ETag。
func hashShort(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
