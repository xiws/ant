package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/xiws/ant/internal/config"
)

// readExec 实现 §10.5 read。
func readExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	maxBytes := maxFileReadBytesOf(cfg)
	maxLines := maxFileReadLinesOf(cfg)
	budget := maxBytes
	if m := maxOutputBytesOf(cfg); m > 0 && m-512 < budget {
		budget = m - 512
		if budget < 256 {
			budget = m
		}
	}
	return func(ctx context.Context, inv Invocation) (Result, error) {
		path, _ := inv.Args["path"].(string)
		if strings.TrimSpace(path) == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 path", "提供 workspace 内的文件路径", nil)}, nil
		}
		res, err := deps.Guard.Resolve(ctx, path, AccessRead)
		if err != nil {
			return Result{OK: false, Err: pathError(err)}, nil
		}
		info, err := os.Stat(res.Abs)
		if err != nil {
			if os.IsNotExist(err) {
				return Result{OK: false, Err: notFoundError(path, similarPaths(res.Abs, workDirOf(cfg)))}, nil
			}
			return Result{OK: false, Err: toolError(err)}, nil
		}
		if info.IsDir() {
			return Result{OK: false, Err: &ToolError{
				Code:    "not_found",
				Message: fmt.Sprintf("%s 是目录，不能作为文件读取", path),
				Hint:    "使用 search 查找目录内文件，或 read_code 定位 Go 符号",
				Details: map[string]any{"path": path, "isDir": true},
			}}, nil
		}
		if !info.Mode().IsRegular() {
			return Result{OK: false, Err: &ToolError{
				Code:    "not_found",
				Message: fmt.Sprintf("%s 不是常规文件", path),
				Hint:    "只能读取 workspace 内的常规文件",
			}}, nil
		}

		start := intArg(inv.Args, "startLine", 1)
		if start < 1 {
			start = 1
		}
		endArg := intArg(inv.Args, "endLine", 0)
		if endArg > 0 && endArg < start {
			return Result{OK: false, Err: invalidArgs("invalid_args",
				fmt.Sprintf("startLine=%d 大于 endLine=%d", start, endArg),
				"startLine 必须 <= endLine", map[string]any{"startLine": start, "endLine": endArg})}, nil
		}
		end := start + maxLines - 1
		if endArg > 0 && endArg < end {
			end = endArg
		}

		head, err := readHead(res.Abs, binarySniffBytes)
		if err != nil {
			return Result{OK: false, Err: toolError(err)}, nil
		}
		if isBinary(head) {
			return Result{OK: false, Err: &ToolError{
				Code:    "binary_file",
				Message: fmt.Sprintf("%s 看起来是二进制文件", path),
				Hint:    "read 只返回文本；二进制内容请用 bash 的专用工具处理",
				Details: map[string]any{"path": path},
			}}, nil
		}

		f, err := os.Open(res.Abs)
		if err != nil {
			return Result{OK: false, Err: toolError(err)}, nil
		}
		var b strings.Builder
		total := 0
		last := 0
		used := 0
		byteLimited := false
		scanErr := scanLines(f, func(line string, n int) bool {
			if err := ctx.Err(); err != nil {
				return false
			}
			total = n
			if n < start {
				return true
			}
			if n > end {
				return true
			}
			// 行号对齐，便于模型精确引用。
			formatted := fmt.Sprintf("%6d\t%s\n", n, line)
			if used+len(formatted) > budget {
				if used == 0 && len(formatted) > 0 {
					// 单行就超预算：保留截断后的首行，避免续读永远停在同一起点。
					cut := budget
					if cut > len(formatted) {
						cut = len(formatted)
					}
					for cut > 0 && cut < len(formatted) && (formatted[cut]&0xC0) == 0x80 {
						cut--
					}
					b.WriteString(formatted[:cut])
					last = n
				}
				byteLimited = true
				return false
			}
			b.WriteString(formatted)
			used += len(formatted)
			last = n
			return true
		})
		_ = f.Close()
		if scanErr != nil {
			return Result{OK: false, Err: toolError(scanErr)}, nil
		}
		// 上面的回调在 n>end 时停止，total 可能尚未统计完整；再补一次总行数。
		totalLines, err := countLines(res.Abs)
		if err != nil {
			totalLines = total
		}
		if total > totalLines {
			totalLines = total
		}
		if start > totalLines && totalLines > 0 {
			sha, herr := fileSHA256(res.Abs)
			if herr != nil {
				return Result{OK: false, Err: toolError(herr)}, nil
			}
			if deps.Versions != nil {
				deps.Versions.RecordRead(res.Abs, sha)
				if res.Rel != "" {
					deps.Versions.RecordRead(res.Rel, sha)
				}
			}
			return Result{OK: true, Data: map[string]any{
				"path":          formatPath(res.Rel, res.Abs),
				"absolutePath":  res.Abs,
				"content":       "",
				"startLine":     start,
				"endLine":       0,
				"totalLines":    totalLines,
				"truncated":     false,
				"nextStartLine": 0,
				"sha256":        sha,
				"size":          info.Size(),
				"hint":          fmt.Sprintf("文件只有 %d 行，startLine=%d 已超出文件末尾", totalLines, start),
			}}, nil
		}
		if last == 0 && totalLines == 0 {
			last = 0
		}

		truncated := last < totalLines || byteLimited
		next := 0
		if truncated {
			next = last + 1
		}
		sha, herr := fileSHA256(res.Abs)
		if herr != nil {
			return Result{OK: false, Err: toolError(herr)}, nil
		}
		if deps.Versions != nil {
			deps.Versions.RecordRead(res.Abs, sha)
			if res.Rel != "" {
				deps.Versions.RecordRead(res.Rel, sha)
			}
		}
		content := b.String()
		hint := ""
		if truncated && next > 0 {
			hint = fmt.Sprintf("内容已截断：下一次从 startLine=%d 继续读取", next)
			content += "\n... [已截断；下一次从 startLine=" + fmt.Sprint(next) + " 继续] ...\n"
		}
		return Result{OK: true, Data: map[string]any{
			"path":          formatPath(res.Rel, res.Abs),
			"absolutePath":  res.Abs,
			"content":       content,
			"startLine":     start,
			"endLine":       last,
			"totalLines":    totalLines,
			"truncated":     truncated,
			"nextStartLine": next,
			"sha256":        sha,
			"size":          info.Size(),
			"hint":          hint,
		}}, nil
	}
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	err = scanLines(f, func(string, int) bool {
		n++
		return true
	})
	return n, err
}

// searchExec 实现 §10.7 search。
func searchExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	maxFileBytes := maxFileReadBytesOf(cfg)
	workDir := workspaceDir(cfg)
	ign := newIgnoreMatcher(workDir)
	return func(ctx context.Context, inv Invocation) (Result, error) {
		query, _ := inv.Args["query"].(string)
		if query == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 query", "提供要搜索的字面量或正则", nil)}, nil
		}
		useRegex := boolArg(inv.Args, "regex", false)
		ignoreCase := boolArg(inv.Args, "ignoreCase", false)
		glob, _ := inv.Args["glob"].(string)
		maxResults := intArg(inv.Args, "maxResults", 100)
		if maxResults <= 0 {
			maxResults = 100
		}
		if maxResults > 500 {
			maxResults = 500
		}

		pattern := query
		if useRegex {
			if ignoreCase {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return Result{OK: false, Err: invalidArgs("invalid_args",
					"正则表达式非法："+err.Error(),
					"使用 Go RE2 语法，或设置 regex=false 做字面量搜索",
					map[string]any{"query": query})}, nil
			}
			return searchWithMatcher(ctx, inv, deps, re.MatchString, glob, maxResults, maxFileBytes, ign, workDir)
		}
		needle := query
		if ignoreCase {
			needle = strings.ToLower(needle)
		}
		matcher := func(line string) bool {
			hay := line
			if ignoreCase {
				hay = strings.ToLower(hay)
			}
			return strings.Contains(hay, needle)
		}
		return searchWithMatcher(ctx, inv, deps, matcher, glob, maxResults, maxFileBytes, ign, workDir)
	}
}

type lineMatcher func(string) bool

func searchWithMatcher(ctx context.Context, inv Invocation, deps Deps, match lineMatcher, glob string, maxResults, maxFileBytes int, ign *ignoreMatcher, workDir string) (Result, error) {
	rootArg, _ := inv.Args["path"].(string)
	root := workDir
	rootRel := ""
	if strings.TrimSpace(rootArg) != "" {
		res, err := deps.Guard.Resolve(ctx, rootArg, AccessRead)
		if err != nil {
			return Result{OK: false, Err: pathError(err)}, nil
		}
		root = res.Abs
		rootRel = res.Rel
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{OK: false, Err: notFoundError(rootArg, similarPaths(root, workDir))}, nil
		}
		return Result{OK: false, Err: toolError(err)}, nil
	}

	var matches []string
	scanned := 0
	skipped := 0
	truncated := false

	visitFile := func(path string, d os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if glob != "" && !matchGlobPath(glob, path, workDir) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > int64(maxFileBytes) {
			skipped++
			return nil
		}
		head, err := readHead(path, binarySniffBytes)
		if err != nil {
			return nil
		}
		if isBinary(head) {
			skipped++
			return nil
		}
		scanned++
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel := relPath(root, path)
		err = scanLines(f, func(line string, n int) bool {
			if err := ctx.Err(); err != nil {
				return false
			}
			if !match(line) {
				return true
			}
			if len(matches) >= maxResults {
				truncated = true
				return false
			}
			matches = append(matches, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), n, line))
			return true
		})
		if err != nil {
			return err
		}
		return nil
	}

	if !info.IsDir() {
		entry := &singleDirEntry{info: info, name: filepath.Base(root)}
		if err := visitFile(root, entry); err != nil {
			return Result{}, err
		}
	} else {
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := relPath(workDir, path)
			if d.IsDir() {
				if path != root && (excludedDir(d.Name()) || ign.ignored(rel, true)) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
				return nil
			}
			if ignoredFile(d.Name()) || ign.ignored(rel, false) {
				return nil
			}
			return visitFile(path, d)
		})
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{OK: false, Err: toolError(err)}, nil
		}
	}
	content := strings.Join(matches, "\n")
	if content != "" {
		content += "\n"
	}
	hint := ""
	if truncated {
		hint = fmt.Sprintf("结果超过 maxResults=%d，已截断；请缩小路径、增加 glob 或使用更精确的 query", maxResults)
	}
	return Result{OK: true, Data: map[string]any{
		"query":        inv.Args["query"],
		"path":         rootRel,
		"content":      content,
		"results":      matches,
		"matchCount":   len(matches),
		"truncated":    truncated,
		"scannedFiles": scanned,
		"skippedFiles": skipped,
		"hint":         hint,
	}}, nil
}

type singleDirEntry struct {
	info os.FileInfo
	name string
}

func (e *singleDirEntry) Name() string               { return e.name }
func (e *singleDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e *singleDirEntry) Type() os.FileMode          { return e.info.Mode().Type() }
func (e *singleDirEntry) Info() (os.FileInfo, error) { return e.info, nil }

var builtinExcludedDirs = map[string]bool{
	".git": true, ".ant": true, "vendor": true, "node_modules": true,
	"bin": true, "dist": true, "__pycache__": true, ".idea": true,
}

func excludedDir(name string) bool { return builtinExcludedDirs[name] }

func ignoredFile(name string) bool {
	// 常见二进制/大文件；文本过滤仍由 isBinary 兜底。
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".dll", ".so", ".dylib", ".a", ".o", ".obj", ".class", ".jar",
		".zip", ".gz", ".tar", ".png", ".jpg", ".jpeg", ".gif", ".pdf", ".ico", ".woff", ".woff2":
		return true
	}
	return false
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func matchGlobPath(glob, path, root string) bool {
	rel := relPath(root, path)
	if strings.Contains(glob, "/") {
		ok, _ := filepath.Match(glob, rel)
		return ok
	}
	ok, _ := filepath.Match(glob, filepath.Base(path))
	return ok
}

// ignoreMatcher 实现 .gitignore 的常用子集：注释、取反、目录、通配符。
type ignoreMatcher struct {
	root  string
	mu    sync.Mutex
	rules []ignoreRule
	load  bool
}

type ignoreRule struct {
	pattern string
	negate  bool
	dirOnly bool
	base    bool // 不含 / 时匹配任意层级的 basename
}

func newIgnoreMatcher(root string) *ignoreMatcher {
	return &ignoreMatcher{root: root}
}

func (m *ignoreMatcher) ignored(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	if !m.load {
		m.load = true
		m.rules = parseGitignore(filepath.Join(m.root, ".gitignore"))
	}
	rules := m.rules
	m.mu.Unlock()
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	matched := false
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.matches(rel) {
			matched = !r.negate
		}
	}
	return matched
}

func (r ignoreRule) matches(rel string) bool {
	if r.base {
		for _, part := range strings.Split(rel, "/") {
			if globMatch(r.pattern, part) {
				return true
			}
		}
		// 目录规则也可能写成 build/，按前缀匹配。
		return globMatch(strings.TrimSuffix(r.pattern, "/"), rel)
	}
	return globMatch(strings.TrimPrefix(r.pattern, "/"), rel)
}

func parseGitignore(path string) []ignoreRule {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rules []ignoreRule
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negate = true
			line = strings.TrimPrefix(line, "!")
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if line == "" {
			continue
		}
		rule.pattern = strings.TrimPrefix(line, "/")
		rule.base = !strings.Contains(rule.pattern, "/")
		rules = append(rules, rule)
	}
	return rules
}

func globMatch(pattern, value string) bool {
	// filepath.Match 不跨分隔符；gitignore 的 ** 需要特殊处理。
	if strings.Contains(pattern, "**") {
		pattern = strings.ReplaceAll(pattern, "**", "*")
	}
	ok, err := filepath.Match(pattern, value)
	if err == nil && ok {
		return true
	}
	if !strings.Contains(pattern, "/") {
		for _, part := range strings.Split(value, "/") {
			if ok, _ := filepath.Match(pattern, part); ok {
				return true
			}
		}
	}
	return false
}
