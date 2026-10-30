package projectctx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/model"
)

const (
	defaultScanMaxFiles = 500
	defaultScanMaxBytes = 32768
	defaultScanDepth    = 3

	// maxScannedFileBytes 跳过超大文件：单文件超过 256 KB 不进入目录树、不进入关键文件。
	maxScannedFileBytes int64 = 256 * 1024
	// binarySniffBytes 是二进制探测窗口（前 8 KB 含 NUL 即视为二进制）。
	binarySniffBytes = 8 * 1024

	maxDeps = 20
)

// builtinExcludeNames 是内置排除目录（§14.3）：按 basename 匹配任意层级。
var builtinExcludeNames = map[string]struct{}{
	".git":         {},
	".ant":         {},
	"vendor":       {},
	"node_modules": {},
	"bin":          {},
	"dist":         {},
}

type scanner struct{}

// NewScanner 返回从 workspace 遍历的 Scanner 生产实现。
func NewScanner() Scanner { return scanner{} }

// Scan 遍历 workspace：遵循内置排除 + project.exclude + .gitignore 基础规则，
// 跳过二进制、超大文件与敏感文件，并解析清单得到项目类型/依赖/命令/关键文件。
func (scanner) Scan(ctx context.Context, opts ScanOptions) (ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		return ScanResult{}, model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "扫描需要非空的 workspace 路径")
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		e := model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "workspace 路径不可用")
		e.Cause = err
		return ScanResult{}, e
	}
	info, err := os.Stat(root)
	if err != nil {
		return ScanResult{}, fmt.Errorf("projectctx: 读取 workspace 失败: %w", err)
	}
	if !info.IsDir() {
		return ScanResult{}, model.NewError(model.ErrConfigInvalid, "projectctx.workspace", "workspace 不是目录: "+root)
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultScanMaxFiles
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultScanMaxBytes
	}
	depth := opts.Depth
	if depth <= 0 {
		depth = defaultScanDepth
	}

	ex := newExcluder(opts.Exclude)
	if err := ex.addGitignore(root); err != nil {
		return ScanResult{}, fmt.Errorf("projectctx: 读取 .gitignore 失败: %w", err)
	}

	st := &scanState{
		root:      root,
		ctx:       ctx,
		maxFiles:  maxFiles,
		depth:     depth,
		ex:        ex,
		seen:      make(map[string]int64),
		manifests: make(map[string]string),
	}
	if err := st.walk(); err != nil {
		return ScanResult{}, err
	}
	return st.result(maxBytes), nil
}

type scanState struct {
	root      string
	ctx       context.Context
	maxFiles  int
	depth     int
	ex        *excluder
	tree      []string
	seen      map[string]int64 // rel → size（只记录进入目录树的文件）
	manifests map[string]string
	fileCount int
	bytesRead int
}

func (s *scanState) walk() error {
	return filepath.WalkDir(s.root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		depth := strings.Count(rel, "/") + 1

		if d.IsDir() {
			if s.ex.matchDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			// 目录树深度：只展示 depth 以内的目录，超出的子树直接剪掉。
			if depth >= s.depth {
				return filepath.SkipDir
			}
			s.tree = append(s.tree, rel+"/")
			return nil
		}
		if !d.Type().IsRegular() {
			// 符号链接、设备、管道等一律跳过，避免越出 workspace 或阻塞读取。
			return nil
		}
		if depth > s.depth {
			return nil
		}
		if s.ex.matchFile(rel, d.Name()) {
			return nil
		}
		if s.fileCount >= s.maxFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		if size > maxScannedFileBytes {
			return nil
		}
		head, n, err := readHead(p, binarySniffBytes)
		if err != nil {
			return err
		}
		s.bytesRead += n
		if isBinary(head) {
			return nil
		}
		// 文件条目用 "\t<size>" 携带大小，供 Hasher 使用；生成器展示时会去掉。
		s.tree = append(s.tree, rel+"\t"+strconv.FormatInt(size, 10))
		s.seen[rel] = size
		s.fileCount++
		// 只解析 workspace 根部的清单文件，避免误把子模块/示例当成项目本体。
		if depth == 1 {
			if key, ok := manifestName(rel); ok {
				if _, exists := s.manifests[key]; !exists {
					body, n2, err := readLimited(p, maxScannedFileBytes)
					if err != nil {
						return err
					}
					s.bytesRead += n2
					s.manifests[key] = body
				}
			}
		}
		return nil
	})
}

func (s *scanState) result(maxBytes int) ScanResult {
	res := ScanResult{FileCount: s.fileCount, BytesRead: s.bytesRead}
	res.ProjectType, res.ModulePath, res.Deps = s.projectInfo()
	res.Commands = s.commands()
	res.Entries = s.entries()
	res.KeyFiles = s.keyFiles(maxBytes)
	res.BytesRead = s.bytesRead
	res.Tree = s.tree
	return res
}

func (s *scanState) projectInfo() (projectType, modulePath string, deps []string) {
	switch {
	case s.hasManifest("go.mod"):
		modulePath, deps = parseGoMod(s.manifests["go.mod"])
		return "Go", modulePath, deps
	case s.hasManifest("package.json"):
		if pkg, ok := parsePackageJSON(s.manifests["package.json"]); ok {
			return "Node", pkg.Name, packageJSONDeps(pkg)
		}
		return "Node", "", nil
	case s.hasManifest("pyproject.toml"):
		modulePath, deps = parsePyproject(s.manifests["pyproject.toml"])
		return "Python", modulePath, deps
	case s.hasManifest("Cargo.toml"):
		modulePath, deps = parseCargo(s.manifests["Cargo.toml"])
		return "Rust", modulePath, deps
	default:
		return "", "", nil
	}
}

func (s *scanState) hasManifest(name string) bool {
	_, ok := s.manifests[name]
	return ok
}

// commands 汇总构建/测试/lint：项目类型默认值 + Makefile 目标（Makefile 优先）。
func (s *scanState) commands() Commands {
	var c Commands
	switch s.projectType() {
	case "Go":
		c = Commands{
			Build: []string{"go build ./..."},
			Test:  []string{"go test ./..."},
			Lint:  []string{"go vet ./..."},
		}
	case "Node":
		if pkg, ok := parsePackageJSON(s.manifests["package.json"]); ok {
			if _, exists := pkg.Scripts["build"]; exists {
				c.Build = []string{"npm run build"}
			}
			if _, exists := pkg.Scripts["test"]; exists {
				c.Test = []string{"npm test"}
			}
			if _, exists := pkg.Scripts["lint"]; exists {
				c.Lint = []string{"npm run lint"}
			}
		}
	case "Rust":
		c = Commands{
			Build: []string{"cargo build"},
			Test:  []string{"cargo test"},
			Lint:  []string{"cargo clippy"},
		}
	}
	if targets := s.makeTargets(); len(targets) > 0 {
		if targets["build"] {
			c.Build = []string{"make build"}
		}
		if targets["test"] {
			c.Test = []string{"make test"}
		}
		if targets["lint"] {
			c.Lint = []string{"make lint"}
		}
	}
	return c
}

func (s *scanState) projectType() string {
	t, _, _ := s.projectInfo()
	return t
}

func (s *scanState) makeTargets() map[string]bool {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		if body, ok := s.manifests[name]; ok {
			return parseMakeTargets(body)
		}
	}
	return nil
}

func (s *scanState) entries() []string {
	var out []string
	seen := make(map[string]bool)
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, rel := range s.sortedSeen() {
		base := path.Base(rel)
		switch {
		case rel == "main.go":
			add("main.go")
		case strings.HasPrefix(rel, "cmd/") && base == "main.go":
			parts := strings.Split(rel, "/")
			if len(parts) >= 3 {
				add("cmd/" + parts[1])
			}
		case rel == "src/main.rs":
			add(rel)
		case strings.HasPrefix(rel, "src/bin/") && strings.HasSuffix(rel, ".rs"):
			add(rel)
		case base == "__main__.py" || base == "manage.py":
			add(rel)
		}
	}
	if s.projectType() == "Node" {
		if pkg, ok := parsePackageJSON(s.manifests["package.json"]); ok {
			for _, key := range []string{"start", "dev", "serve"} {
				if _, exists := pkg.Scripts[key]; exists {
					add("npm run " + key)
				}
			}
			if len(out) == 0 {
				keys := sortedMapKeys(pkg.Scripts)
				if len(keys) > 0 {
					add("npm run " + keys[0])
				}
			}
		}
	}
	return out
}

// keyFiles 按优先级挑选关键文件，总大小不超过 project.maxBytes（§14.3 第 3 步）。
func (s *scanState) keyFiles(maxBytes int) []KeyFile {
	candidates := s.keyFileCandidates()
	var out []KeyFile
	total := 0
	used := make(map[string]bool)
	for _, rel := range candidates {
		if used[rel] {
			continue
		}
		size, ok := s.seen[rel]
		if !ok || size > maxScannedFileBytes {
			continue
		}
		if size > int64(maxBytes) || total+int(size) > maxBytes {
			continue
		}
		body, n, err := readLimited(filepath.Join(s.root, filepath.FromSlash(rel)), maxScannedFileBytes)
		if err != nil {
			// 扫描与读取之间文件可能被删改：跳过而不失败。
			continue
		}
		s.bytesRead += n
		if isBinary([]byte(body)) {
			continue
		}
		used[rel] = true
		out = append(out, KeyFile{Path: rel, Body: body})
		total += len(body)
	}
	return out
}

func (s *scanState) keyFileCandidates() []string {
	var out []string
	// 顺序即优先级：README 与清单文件优先，随后是构建入口与 CI 配置。
	fixed := []string{
		"README.md", "README.markdown", "README", "readme.md",
		"go.mod", "package.json", "pyproject.toml", "Cargo.toml",
		"Makefile", "makefile", "GNUmakefile",
		"main.go", "src/main.rs", "Dockerfile", "docker-compose.yml",
		".gitlab-ci.yml", ".travis.yml", "azure-pipelines.yml", "Jenkinsfile",
	}
	for _, name := range fixed {
		if _, ok := s.seen[name]; ok {
			out = append(out, name)
		}
	}
	for _, rel := range s.sortedSeen() {
		switch {
		case strings.HasPrefix(rel, "cmd/") && strings.HasSuffix(rel, "/main.go"):
			out = append(out, rel)
		case strings.HasPrefix(rel, "src/bin/") && strings.HasSuffix(rel, ".rs"):
			out = append(out, rel)
		case strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")):
			out = append(out, rel)
		case strings.HasPrefix(rel, ".circleci/") && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")):
			out = append(out, rel)
		}
	}
	return out
}

func (s *scanState) sortedSeen() []string {
	out := make([]string, 0, len(s.seen))
	for rel := range s.seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// --- 排除规则 ---

type excluder struct {
	patterns []string
}

func newExcluder(extra []string) *excluder {
	e := &excluder{}
	for _, p := range extra {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		e.patterns = append(e.patterns, filepath.ToSlash(p))
	}
	return e
}

// addGitignore 读取 workspace 根部的 .gitignore，只支持基础规则（前缀/后缀/通配），
// 不支持 `!` 反选与 `**` 以外的复杂语义（§14.3 允许的简化实现）。
func (e *excluder) addGitignore(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		e.patterns = append(e.patterns, line)
	}
	return nil
}

func (e *excluder) matchDir(rel, base string) bool {
	if _, ok := builtinExcludeNames[base]; ok {
		return true
	}
	for _, p := range e.patterns {
		if gitignoreMatch(p, rel, base, true) {
			return true
		}
	}
	return false
}

func (e *excluder) matchFile(rel, base string) bool {
	if _, ok := builtinExcludeNames[base]; ok {
		return true
	}
	if isSensitive(rel) {
		return true
	}
	for _, p := range e.patterns {
		if gitignoreMatch(p, rel, base, false) {
			return true
		}
	}
	return false
}

// gitignoreMatch 实现简化的 .gitignore 语义：
//   - 不含 "/" 的模式匹配任意层级的 basename；
//   - 含 "/" 的模式按相对路径做 glob 或目录前缀匹配；
//   - 尾随 "/" 只匹配目录；
//   - 反选（!）在加载时已被忽略。
func gitignoreMatch(pattern, rel, base string, isDir bool) bool {
	p := strings.TrimSpace(pattern)
	if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "!") {
		return false
	}
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return false
	}
	if dirOnly && !isDir {
		// 目录模式只负责剪掉整棵子树，文件条目不单独匹配。
		return false
	}
	if !strings.Contains(p, "/") {
		ok, err := path.Match(p, base)
		return err == nil && ok
	}
	if strings.HasSuffix(p, "/**") {
		prefix := strings.TrimSuffix(p, "/**")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}
	if ok, err := path.Match(p, rel); err == nil && ok {
		return true
	}
	// 忽略整个目录：目录及其内容都命中。
	return strings.HasPrefix(rel, p+"/")
}

// isSensitive 排除 .env、密钥、证书等敏感文件（§14.6）。
func isSensitive(rel string) bool {
	base := strings.ToLower(path.Base(rel))
	if strings.HasPrefix(base, ".env") {
		return true
	}
	for _, prefix := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "secret", "credential"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	switch base {
	case ".netrc", ".npmrc", ".pypirc", "credentials.json", "secrets.json":
		return true
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".pem", ".key", ".crt", ".cer", ".p12", ".pfx", ".jks", ".keystore", ".der", ".kdbx", ".asc", ".gpg":
		return true
	}
	return false
}

// --- 文件读取 ---

func readHead(p string, limit int) ([]byte, int, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return nil, 0, err
	}
	return buf, len(buf), nil
}

func readLimited(p string, limit int64) (string, int, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", 0, err
	}
	if int64(len(buf)) > limit {
		buf = buf[:limit]
	}
	return string(buf), len(buf), nil
}

func isBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}

// manifestName 返回需要解析的清单键名（仅根目录）。
func manifestName(rel string) (string, bool) {
	switch rel {
	case "go.mod", "package.json", "pyproject.toml", "Cargo.toml", "Makefile", "makefile", "GNUmakefile":
		return rel, true
	default:
		return "", false
	}
}

// --- 清单解析 ---

func parseGoMod(body string) (module string, deps []string) {
	inRequire := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "module" && len(fields) >= 2:
			module = strings.Trim(fields[1], "\"`")
		case fields[0] == "require":
			if len(fields) >= 2 && fields[1] != "(" {
				deps = append(deps, strings.Trim(fields[1], "\"`"))
			} else {
				inRequire = true
			}
		case inRequire && fields[0] == ")":
			inRequire = false
		case inRequire:
			deps = append(deps, strings.Trim(fields[0], "\"`"))
		}
	}
	return module, limitStrings(dedupeStrings(deps), maxDeps)
}

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func parsePackageJSON(body string) (packageJSON, bool) {
	var pkg packageJSON
	if err := json.Unmarshal([]byte(body), &pkg); err != nil {
		return packageJSON{}, false
	}
	return pkg, true
}

func packageJSONDeps(pkg packageJSON) []string {
	keys := make([]string, 0, len(pkg.Dependencies)+len(pkg.DevDependencies))
	for name := range pkg.Dependencies {
		keys = append(keys, name)
	}
	for name := range pkg.DevDependencies {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return limitStrings(dedupeStrings(keys), maxDeps)
}

func parsePyproject(body string) (name string, deps []string) {
	section := ""
	inDeps := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			inDeps = false
			continue
		}
		if !strings.HasPrefix(section, "project") && !strings.HasPrefix(section, "tool.poetry") {
			continue
		}
		if strings.HasPrefix(line, "dependencies") && strings.Contains(line, "[") && !strings.Contains(line, "]") {
			inDeps = true
			continue
		}
		if inDeps {
			if strings.HasPrefix(line, "]") {
				inDeps = false
				continue
			}
			deps = append(deps, trimTomlString(strings.TrimSuffix(line, ",")))
			continue
		}
		if name == "" && strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "name" {
				name = trimTomlString(parts[1])
			}
		}
	}
	return name, limitStrings(dedupeStrings(deps), maxDeps)
}

func parseCargo(body string) (name string, deps []string) {
	section := ""
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch {
		case section == "package" && key == "name":
			if name == "" {
				name = trimTomlString(value)
			}
		case section == "dependencies", section == "dev-dependencies", section == "build-dependencies":
			deps = append(deps, strings.Trim(key, "\"'"))
		}
	}
	return name, limitStrings(dedupeStrings(deps), maxDeps)
}

func parseMakeTargets(body string) map[string]bool {
	targets := make(map[string]bool)
	for _, raw := range strings.Split(body, "\n") {
		if strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, " ") {
			continue
		}
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		if idx+1 < len(line) && line[idx+1] == '=' {
			// "VAR := value" 不是目标
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "" || strings.Contains(name, "=") || strings.Contains(name, "%") ||
			strings.HasPrefix(name, ".") || strings.ContainsAny(name, " \t") {
			continue
		}
		targets[name] = true
	}
	return targets
}

func trimTomlString(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"'")
	return v
}

func limitStrings(items []string, limit int) []string {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func dedupeStrings(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
