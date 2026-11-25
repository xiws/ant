package readcode

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// defaultDepth / defaultMaxSymbols 与 §10.6 一致。
const (
	defaultDepth      = 3
	defaultMaxSymbols = 50
	maxFileBytes      = 4 << 20
)

var generatedPattern = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

type fileInfo struct {
	rel       string
	abs       string
	pkg       string
	imports   []string
	importMap map[string]string // 本地名/包名 -> import path
	src       []byte
	funcs     []*funcInfo
	generated bool
}

type funcInfo struct {
	sym     Symbol
	file    *fileInfo
	decl    *ast.FuncDecl
	doc     string
	start   int
	end     int
	endLine int
}

type symbolIndex struct {
	cfg    config.View
	work   string
	module string

	mu       sync.RWMutex
	loaded   bool
	files    map[string]*fileInfo
	packages map[string][]*fileInfo // 包名 -> 文件
	byName   map[string][]*funcInfo
	methods  map[string]map[string]*funcInfo // 接收者类型 -> 方法名
	funcs    map[string]*funcInfo            // 稳定 key -> 函数
	parseErr map[string]error
	omitted  []string
	// generated 记录因生成文件被跳过的路径，便于给出准确错误。
	generated map[string]bool
}

// NewIndex 构造会话内 Go 符号索引；索引在首次使用时惰性构建。
func NewIndex(cfg config.View) Index {
	work := ""
	if p, ok := cfg.(config.Paths); ok {
		work = p.WorkDir()
	}
	if work == "" {
		work, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(work); err == nil {
		work = abs
	}
	return &symbolIndex{cfg: cfg, work: filepath.Clean(work)}
}

// Invalidate 丢弃缓存：edit/write 后必须重建（§21.9.1）。
func (s *symbolIndex) Invalidate(paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(paths) == 0 {
		s.loaded = false
		s.files = nil
		return
	}
	// 只保留仍在索引中的其它文件；保守地整体失效，避免目录包信息过期。
	s.loaded = false
	s.files = nil
}

func (s *symbolIndex) ensure(ctx context.Context) error {
	s.mu.RLock()
	if s.loaded {
		s.mu.RUnlock()
		return nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	if err := s.buildLocked(ctx); err != nil {
		return err
	}
	s.loaded = true
	return nil
}

func (s *symbolIndex) buildLocked(ctx context.Context) error {
	s.files = map[string]*fileInfo{}
	s.packages = map[string][]*fileInfo{}
	s.byName = map[string][]*funcInfo{}
	s.methods = map[string]map[string]*funcInfo{}
	s.funcs = map[string]*funcInfo{}
	s.parseErr = map[string]error{}
	s.omitted = nil
	s.generated = map[string]bool{}
	s.module = findModulePath(s.work)

	err := filepath.WalkDir(s.work, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != s.work && excludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".go") {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileBytes {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(s.work, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if generatedPattern.Match(body) {
			s.generated[rel] = true
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, body, parser.ParseComments)
		if err != nil {
			s.parseErr[rel] = err
			return nil
		}
		fi := &fileInfo{rel: rel, abs: path, pkg: parsed.Name.Name, src: body, importMap: map[string]string{}}
		for _, imp := range parsed.Imports {
			unquoted, _ := strconv.Unquote(imp.Path.Value)
			local := ""
			if imp.Name != nil {
				local = imp.Name.Name
			}
			spec := imp.Path.Value
			if local != "" {
				spec = local + " " + spec
				if local != "_" && local != "." {
					fi.importMap[local] = unquoted
				}
			} else {
				base := unquoted
				if i := strings.LastIndexByte(base, '/'); i >= 0 {
					base = base[i+1:]
				}
				if base != "" {
					fi.importMap[base] = unquoted
				}
			}
			fi.imports = append(fi.imports, spec)
		}
		s.files[rel] = fi
		s.packages[fi.pkg] = append(s.packages[fi.pkg], fi)

		tf := fset.File(parsed.Pos())
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil {
				continue
			}
			recv := receiverType(fd.Recv)
			pos := fset.Position(fd.Pos())
			fn := &funcInfo{
				sym: Symbol{
					Name: fd.Name.Name, Package: fi.pkg, File: rel,
					Line: pos.Line, Receiver: recv,
				},
				file: fi, decl: fd, doc: docText(fd.Doc),
			}
			if fd.Doc != nil {
				fn.start = fset.Position(fd.Doc.Pos()).Offset
			} else {
				fn.start = fset.Position(fd.Pos()).Offset
			}
			fn.end = fset.Position(fd.End()).Offset
			fn.endLine = fset.Position(fd.End()).Line
			if tf != nil && fn.end > len(fi.src) {
				fn.end = len(fi.src)
			}
			if fn.start < 0 {
				fn.start = 0
			}
			fi.funcs = append(fi.funcs, fn)
			s.byName[fn.sym.Name] = append(s.byName[fn.sym.Name], fn)
			s.funcs[fn.key()] = fn
			if recv != "" {
				if s.methods[recv] == nil {
					s.methods[recv] = map[string]*funcInfo{}
				}
				s.methods[recv][fn.sym.Name] = fn
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (f *funcInfo) key() string {
	return f.file.rel + "#" + f.sym.Receiver + "." + f.sym.Name + "@" + strconv.Itoa(f.sym.Line)
}

// snippet 把函数切成代码片段（含紧邻文档注释）。
func (f *funcInfo) snippet() Snippet {
	return Snippet{
		File:    f.file.rel,
		Package: f.file.pkg,
		Name:    f.sym.Name,
		Line:    f.sym.Line,
		Imports: append([]string(nil), f.file.imports...),
		Code:    string(f.file.src[f.start:f.end]),
	}
}

func receiverType(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	return typeName(fl.List[0].Type)
}

func typeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return typeName(e.X)
	case *ast.IndexListExpr:
		return typeName(e.X)
	case *ast.ParenExpr:
		return typeName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func docText(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	return doc.Text()
}

func findModulePath(work string) string {
	cur := work
	for {
		data, err := os.ReadFile(filepath.Join(cur, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "module "))
				}
			}
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func excludedDir(name string) bool {
	switch name {
	case ".git", ".ant", "vendor", "node_modules", "testdata", "bin", "dist", "__pycache__", ".idea":
		return true
	}
	return false
}

// Resolve 实现 Index。
func (s *symbolIndex) Resolve(ctx context.Context, target string) (Symbol, error) {
	if err := s.ensure(ctx); err != nil {
		return Symbol{}, err
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return Symbol{}, invalidArgs("target 不能为空", "使用 path#Symbol、Symbol、path:line 或 pkg.Symbol")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if i := strings.IndexByte(target, '#'); i > 0 {
		if !isGoTarget(target[:i]) {
			return Symbol{}, unsupportedLanguage(target)
		}
		return s.resolveInFile(target[:i], target[i+1:])
	}
	if path, line, ok := splitLineTarget(target); ok {
		if !isGoTarget(path) {
			return Symbol{}, unsupportedLanguage(target)
		}
		return s.resolveLine(path, line)
	}
	if len(s.files) == 0 {
		return Symbol{}, unsupportedLanguage(target)
	}
	if pkg, name, ok := splitPkgTarget(target); ok {
		return s.resolveQualified(pkg, name)
	}
	return s.resolveBare(target)
}

func (s *symbolIndex) resolveInFile(path, name string) (Symbol, error) {
	rel := s.relOf(path)
	fi := s.files[rel]
	if fi == nil {
		if err := s.parseErr[rel]; err != nil {
			return Symbol{}, &model.Error{
				Class: model.ErrToolFailed, Code: "parse_error",
				Message: fmt.Sprintf("%s 解析失败：%v", rel, err),
				Hint:    "修复语法错误后重试",
			}
		}
		if s.generated[rel] {
			return Symbol{}, generatedFile(rel)
		}
		return Symbol{}, notFoundFile(path, s.similarFilesLocked(rel))
	}
	candidates := make([]*funcInfo, 0, 1)
	for _, fn := range fi.funcs {
		if fn.sym.Name == name {
			candidates = append(candidates, fn)
		}
	}
	if len(candidates) == 0 {
		return Symbol{}, notFoundInFile(rel, name, funcNamesOf(fi, 8))
	}
	if len(candidates) > 1 {
		return Symbol{}, ambiguous(name, candidates)
	}
	return candidates[0].sym, nil
}

func (s *symbolIndex) resolveLine(path string, line int) (Symbol, error) {
	rel := s.relOf(path)
	fi := s.files[rel]
	if fi == nil {
		if err := s.parseErr[rel]; err != nil {
			return Symbol{}, &model.Error{
				Class: model.ErrToolFailed, Code: "parse_error",
				Message: fmt.Sprintf("%s 解析失败：%v", rel, err),
			}
		}
		return Symbol{}, notFound(path, nil)
	}
	var best *funcInfo
	// token.Pos 的行号在构建时已记录；选择覆盖目标行的函数。
	for _, fn := range fi.funcs {
		if fn.sym.Line <= line && (fn.endLine == 0 || line <= fn.endLine) {
			if best == nil || fn.sym.Line > best.sym.Line {
				best = fn
			}
		}
	}
	if best == nil {
		return Symbol{}, notFound(fmt.Sprintf("%s:%d", path, line), nil)
	}
	return best.sym, nil
}

func (s *symbolIndex) resolveQualified(pkg, name string) (Symbol, error) {
	candidates := make([]*funcInfo, 0)
	for _, fn := range s.byName[name] {
		if fn.file.pkg == pkg {
			candidates = append(candidates, fn)
		}
	}
	if len(candidates) == 0 {
		return Symbol{}, notFound(pkg+"."+name, s.similarSymbols(name))
	}
	// 包级函数与方法同名时，pkg.Name 优先指向包级函数。
	if len(candidates) > 1 {
		plain := make([]*funcInfo, 0, 1)
		for _, fn := range candidates {
			if fn.sym.Receiver == "" {
				plain = append(plain, fn)
			}
		}
		if len(plain) == 1 {
			candidates = plain
		}
	}
	if len(candidates) > 1 {
		return Symbol{}, ambiguous(pkg+"."+name, candidates)
	}
	return candidates[0].sym, nil
}

func (s *symbolIndex) resolveBare(name string) (Symbol, error) {
	candidates := s.byName[name]
	if len(candidates) == 0 {
		return Symbol{}, notFound(name, s.similarSymbols(name))
	}
	if len(candidates) > 1 {
		return Symbol{}, ambiguous(name, candidates)
	}
	return candidates[0].sym, nil
}

func (s *symbolIndex) relOf(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.work, abs)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(s.work, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func (s *symbolIndex) similarSymbols(name string) []string {
	type candidate struct {
		name  string
		score int
	}
	var out []candidate
	lower := strings.ToLower(name)
	for n := range s.byName {
		ln := strings.ToLower(n)
		score := 0
		switch {
		case ln == lower:
			score = 100
		case strings.HasPrefix(ln, lower) || strings.HasPrefix(lower, ln):
			score = 70
		case strings.Contains(ln, lower) || strings.Contains(lower, ln):
			score = 50
		}
		if score > 0 {
			out = append(out, candidate{n, score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].name < out[j].name
	})
	names := make([]string, 0, 3)
	for _, c := range out {
		if len(names) >= 3 {
			break
		}
		names = append(names, c.name)
	}
	return names
}

func splitLineTarget(target string) (string, int, bool) {
	i := strings.LastIndexByte(target, ':')
	if i <= 0 || i == len(target)-1 {
		return "", 0, false
	}
	line, err := strconv.Atoi(target[i+1:])
	if err != nil || line < 1 {
		return "", 0, false
	}
	// Windows 盘符里的冒号不是行号分隔符；LastIndex 已避开。
	return target[:i], line, true
}

func splitPkgTarget(target string) (pkg, name string, ok bool) {
	i := strings.LastIndexByte(target, '.')
	if i <= 0 || i == len(target)-1 {
		return "", "", false
	}
	pkg, name = target[:i], target[i+1:]
	if strings.ContainsAny(pkg, `/\\#:`) {
		return "", "", false
	}
	if strings.Contains(name, ".") {
		return "", "", false
	}
	return pkg, name, true
}

func isGoTarget(path string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	return ext == "" || ext == ".go"
}

func unsupportedLanguage(target string) error {
	return &model.Error{
		Class: model.ErrToolFailed, Code: "unsupported_language",
		Message: fmt.Sprintf("read_code 目前只支持 Go，无法解析 %q", target),
		Hint:    "非 Go 文件请使用 read / search",
		Details: map[string]any{"target": target},
	}
}

func notFound(target string, similar []string) error {
	hint := "检查符号名与文件路径；可用 search 定位"
	if len(similar) > 0 {
		hint = "相似符号：" + strings.Join(similar, "、")
	}
	return &model.Error{
		Class: model.ErrToolFailed, Code: "not_found",
		Message: fmt.Sprintf("找不到符号 %q", target),
		Hint:    hint,
		Details: map[string]any{"target": target, "candidates": similar},
	}
}

func ambiguous(target string, candidates []*funcInfo) error {
	shown := make([]map[string]any, 0, len(candidates))
	for _, fn := range candidates {
		shown = append(shown, map[string]any{
			"name": fn.sym.Name, "file": fn.file.rel, "line": fn.sym.Line,
			"package": fn.file.pkg, "receiver": fn.sym.Receiver,
		})
	}
	return &model.Error{
		Class: model.ErrToolFailed, Code: "ambiguous_symbol",
		Message: fmt.Sprintf("符号 %q 有 %d 个候选", target, len(candidates)),
		Hint:    "使用 path#Symbol、pkg.Symbol 或 path:line 明确目标",
		Details: map[string]any{"target": target, "candidates": shown},
	}
}

func invalidArgs(message, hint string) error {
	return &model.Error{Class: model.ErrToolInvalidArgs, Code: "invalid_args", Message: message, Hint: hint}
}

// notFoundFile 是“target 指向的文件不在索引里”的错误。
func notFoundFile(path string, similar []string) error {
	hint := "路径相对 workspace；确认文件存在且后缀为 .go"
	if len(similar) > 0 {
		hint = "是否想找：" + strings.Join(similar, "、")
	}
	return &model.Error{
		Class: model.ErrToolFailed, Code: "not_found",
		Message: fmt.Sprintf("找不到文件 %q", path),
		Hint:    hint,
		Details: map[string]any{"target": path, "candidates": similar},
	}
}

// notFoundInFile 在文件内找不到指定函数时给出该文件可用函数名。
func notFoundInFile(rel, name string, available []string) error {
	hint := fmt.Sprintf("%s 内没有函数 %q", rel, name)
	if len(available) > 0 {
		hint = fmt.Sprintf("%s 内没有函数 %q；该文件有：%s", rel, name, strings.Join(available, "、"))
	}
	return &model.Error{
		Class: model.ErrToolFailed, Code: "not_found",
		Message: fmt.Sprintf("找不到符号 %q", rel+"#"+name),
		Hint:    hint,
		Details: map[string]any{"target": rel + "#" + name, "file": rel, "functions": available},
	}
}

// dirTarget 在 target 是目录时给出目录内的 Go 文件候选。
func dirTarget(path string, children []string) error {
	hint := "read_code 需要文件路径；用 search 列出目录内文件"
	if len(children) > 0 {
		hint = "该目录是 Go 包，请指定其中一个文件：" + strings.Join(children, "、")
	}
	return &model.Error{
		Class: model.ErrToolInvalidArgs, Code: "invalid_args",
		Message: fmt.Sprintf("%s 是目录，不能按文件读取", path),
		Hint:    hint,
		Details: map[string]any{"target": path, "isDir": true, "candidates": children},
	}
}

// generatedFile 是生成文件被索引跳过时的错误。
func generatedFile(rel string) error {
	return &model.Error{
		Class: model.ErrToolFailed, Code: "not_found",
		Message: fmt.Sprintf("%s 是生成文件，已被 read_code 索引跳过", rel),
		Hint:    "生成文件请用 read 直接读取",
		Details: map[string]any{"target": rel, "generated": true},
	}
}

// noFunctions 表示文件存在但没有任何函数/方法声明。
func noFunctions(rel string) error {
	return &model.Error{
		Class: model.ErrToolFailed, Code: "not_found",
		Message: fmt.Sprintf("%s 中没有函数或方法声明", rel),
		Hint:    "该文件可能只有类型/变量/常量；用 read 查看完整内容",
		Details: map[string]any{"target": rel},
	}
}

// Closure 实现 Index：BFS 收集依赖函数，按发现顺序返回代码切片。
func (s *symbolIndex) Closure(ctx context.Context, root Symbol, opt Options) ([]Snippet, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	depth := opt.Depth
	switch {
	case opt.NoExpand:
		depth = 0
	case depth <= 0:
		depth = defaultDepth
	}
	maxSymbols := opt.MaxSymbols
	if maxSymbols <= 0 {
		maxSymbols = defaultMaxSymbols
	}
	if !opt.IncludeTest {
		opt.IncludeTest = strings.HasSuffix(root.File, "_test.go")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if opt.ModulePath != "" {
		prev := s.module
		s.module = opt.ModulePath
		defer func() { s.module = prev }()
	}

	rootFn := s.lookup(root)
	if rootFn == nil {
		return nil, notFound(root.Name, s.similarSymbols(root.Name))
	}
	type item struct {
		fn    *funcInfo
		depth int
	}
	queue := []item{{fn: rootFn, depth: 0}}
	seen := map[string]bool{rootFn.key(): true}
	snippets := make([]Snippet, 0, 8)
	s.omitted = nil
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := queue[0]
		queue = queue[1:]
		if !opt.IncludeTest && strings.HasSuffix(cur.fn.file.rel, "_test.go") {
			continue
		}
		snippets = append(snippets, cur.fn.snippet())
		if cur.depth >= depth {
			continue
		}
		for _, ref := range s.references(cur.fn) {
			if ref == nil || seen[ref.key()] {
				continue
			}
			if len(snippets)+len(queue) >= maxSymbols {
				s.omitted = append(s.omitted, ref.sym.Name)
				continue
			}
			seen[ref.key()] = true
			queue = append(queue, item{fn: ref, depth: cur.depth + 1})
		}
	}
	return snippets, nil
}

func (s *symbolIndex) lookup(sym Symbol) *funcInfo {
	key := sym.File + "#" + sym.Receiver + "." + sym.Name + "@" + strconv.Itoa(sym.Line)
	if fn := s.funcs[key]; fn != nil {
		return fn
	}
	for _, fn := range s.byName[sym.Name] {
		if fn.file.rel == sym.File && fn.sym.Receiver == sym.Receiver {
			return fn
		}
	}
	return nil
}

// Omitted 返回上次 Closure 因 maxSymbols 截断的符号名。
func (s *symbolIndex) Omitted() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.omitted...)
}

// Read 实现 Index：按 target 形态选择“文件函数清单”或“符号依赖闭包”。
//
//	target = path/to/file.go        → 文件内全部函数/方法（不展开依赖）
//	target = path/to/file.go#Func   → 只返回该函数本体；显式 Expand 时才展开
//	target = Func / pkg.Func / path:line → §10.6 的依赖闭包
func (s *symbolIndex) Read(ctx context.Context, req Request) (Result, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return Result{}, invalidArgs("target 不能为空",
			"使用文件路径、文件#函数、函数名、path:line 或 pkg.Function")
	}
	if err := s.ensure(ctx); err != nil {
		return Result{}, err
	}
	if rel, ok := s.fileTarget(target); ok {
		snippets, err := s.File(ctx, rel)
		if err != nil {
			return Result{}, err
		}
		return Result{Mode: ModeFile, Target: target, File: rel, Snippets: snippets}, nil
	}
	root, err := s.Resolve(ctx, target)
	if err != nil {
		return Result{}, err
	}
	noExpand := strings.ContainsRune(target, '#') && !req.Expand
	snippets, err := s.Closure(ctx, root, Options{
		Depth:       req.Depth,
		MaxSymbols:  req.MaxSymbols,
		IncludeTest: strings.HasSuffix(root.File, "_test.go"),
		NoExpand:    noExpand,
	})
	if err != nil {
		return Result{}, err
	}
	sym := root
	return Result{
		Mode: ModeSymbol, Target: target, Symbol: &sym,
		Snippets: snippets, NoExpand: noExpand, Omitted: s.Omitted(),
	}, nil
}

// File 返回某个 Go 文件内的全部函数/方法（声明顺序），不做依赖展开。
func (s *symbolIndex) File(ctx context.Context, path string) ([]Snippet, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rel := s.relOf(path)
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, notFoundFile(path, nil)
	}
	abs := filepath.Join(s.work, filepath.FromSlash(rel))
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		s.mu.RLock()
		children := s.indexedInDirLocked(rel)
		s.mu.RUnlock()
		return nil, dirTarget(path, children)
	}

	s.mu.RLock()
	fi := s.files[rel]
	parseErr := s.parseErr[rel]
	generated := s.generated[rel]
	var snippets []Snippet
	if fi != nil {
		snippets = make([]Snippet, 0, len(fi.funcs))
		for _, fn := range fi.funcs {
			snippets = append(snippets, fn.snippet())
		}
	}
	candidates := s.similarFilesLocked(rel)
	s.mu.RUnlock()

	switch {
	case parseErr != nil:
		return nil, &model.Error{
			Class: model.ErrToolFailed, Code: "parse_error",
			Message: fmt.Sprintf("%s 解析失败：%v", rel, parseErr),
			Hint:    "修复语法错误后重试",
		}
	case generated:
		return nil, generatedFile(rel)
	case fi == nil:
		if !strings.EqualFold(filepath.Ext(rel), ".go") {
			return nil, unsupportedLanguage(rel)
		}
		return nil, notFoundFile(rel, candidates)
	case len(snippets) == 0:
		return nil, noFunctions(rel)
	}
	return snippets, nil
}

// fileTarget 判断 target 是否指文件；返回 workspace 相对路径。
// 路径形态（含 / 或 \）与 .go 结尾的 target 一律按文件处理，不回退到符号解析；
// 裸名字默认按符号处理，只有磁盘上确实是目录时才按目录给出专用错误。
func (s *symbolIndex) fileTarget(target string) (string, bool) {
	if strings.ContainsRune(target, '#') {
		return "", false
	}
	if _, _, ok := splitLineTarget(target); ok {
		return "", false
	}
	rel := s.relOf(target)
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	if isFileLike(target) {
		return rel, true
	}
	if info, err := os.Stat(filepath.Join(s.work, filepath.FromSlash(rel))); err == nil && info.IsDir() {
		return rel, true
	}
	return "", false
}

func isFileLike(target string) bool {
	if strings.ContainsAny(target, `/\`) {
		return true
	}
	return strings.EqualFold(filepath.Ext(target), ".go")
}

// funcNamesOf 返回文件内函数/方法名（最多 n 个），用于 not_found 提示。
func funcNamesOf(fi *fileInfo, n int) []string {
	names := make([]string, 0, len(fi.funcs))
	for _, fn := range fi.funcs {
		if len(names) >= n {
			break
		}
		names = append(names, fn.sym.Name)
	}
	return names
}

// indexedInDirLocked 返回目录下已索引的 Go 文件（调用方需持有读锁）。
func (s *symbolIndex) indexedInDirLocked(rel string) []string {
	out := make([]string, 0, 4)
	for p := range s.files {
		if filepath.ToSlash(filepath.Dir(p)) != rel {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// similarFilesLocked 按文件名为未索引的 target 找候选（调用方需持有读锁）。
func (s *symbolIndex) similarFilesLocked(rel string) []string {
	base := strings.ToLower(filepath.Base(rel))
	want := strings.TrimSuffix(base, ".go")
	type candidate struct {
		path  string
		score int
	}
	var out []candidate
	for p := range s.files {
		name := strings.ToLower(filepath.Base(p))
		score := 0
		switch {
		case name == base:
			score = 100
		case strings.TrimSuffix(name, ".go") == want:
			score = 80
		case want != "" && (strings.Contains(name, want) || strings.Contains(want, name)):
			score = 50
		}
		if score > 0 {
			out = append(out, candidate{path: p, score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].path < out[j].path
	})
	paths := make([]string, 0, 3)
	for _, c := range out {
		if len(paths) >= 3 {
			break
		}
		paths = append(paths, c.path)
	}
	return paths
}

func (s *symbolIndex) references(fn *funcInfo) []*funcInfo {
	vars := localVarTypes(fn)
	var refs []*funcInfo
	add := func(candidate *funcInfo) {
		if candidate == nil {
			return
		}
		for _, existing := range refs {
			if existing.key() == candidate.key() {
				return
			}
		}
		refs = append(refs, candidate)
	}

	ast.Inspect(fn.decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			add(s.samePackageFunc(fn, fun.Name))
		case *ast.SelectorExpr:
			if ident, ok := fun.X.(*ast.Ident); ok {
				if typ := vars[ident.Name]; typ != "" {
					add(s.methods[typ][fun.Sel.Name])
					return true
				}
				if path, ok := fn.file.importMap[ident.Name]; ok {
					add(s.importedFunc(fn, path, fun.Sel.Name))
					return true
				}
			}
			// 无法静态解析接收者类型：跳过（§10.6 第 5 步）。
		}
		return true
	})
	// 函数值引用：裸标识符若命中同包函数则纳入（不展开到类型/变量）。
	ast.Inspect(fn.decl, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name == "_" {
			return true
		}
		if fn.decl.Name != nil && id == fn.decl.Name {
			return true
		}
		add(s.samePackageFunc(fn, id.Name))
		return true
	})
	return refs
}

func (s *symbolIndex) samePackageFunc(from *funcInfo, name string) *funcInfo {
	for _, fn := range s.byName[name] {
		if fn.file.pkg == from.file.pkg {
			// 同一包内同名方法可能属于不同接收者；接收者不同则不猜。
			if fn.sym.Receiver != "" && fn.sym.Receiver != from.sym.Receiver {
				continue
			}
			return fn
		}
	}
	return nil
}

func (s *symbolIndex) importedFunc(from *funcInfo, importPath, name string) *funcInfo {
	if !s.isInternal(importPath) {
		return nil
	}
	dir := s.importDir(importPath)
	for _, fi := range s.files {
		if fi.pkg == "" || fi.rel == "" {
			continue
		}
		if filepath.ToSlash(filepath.Dir(fi.rel)) != dir {
			continue
		}
		for _, fn := range fi.funcs {
			if fn.sym.Name == name && fn.sym.Receiver == "" {
				return fn
			}
		}
	}
	// 回退：按包名（import 基名）匹配。
	base := importPath
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	for _, fi := range s.files {
		if fi.pkg != base {
			continue
		}
		for _, fn := range fi.funcs {
			if fn.sym.Name == name && fn.sym.Receiver == "" {
				return fn
			}
		}
	}
	return nil
}

func (s *symbolIndex) isInternal(importPath string) bool {
	if s.module != "" {
		return importPath == s.module || strings.HasPrefix(importPath, s.module+"/")
	}
	// 无 go.mod 时的启发式：首段不含点（不是域名）视为内部。
	first := importPath
	if i := strings.IndexByte(first, '/'); i >= 0 {
		first = first[:i]
	}
	return !strings.Contains(first, ".")
}

func (s *symbolIndex) importDir(importPath string) string {
	if s.module != "" {
		rel := strings.TrimPrefix(importPath, s.module)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return "."
		}
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Dir(importPath))
}

// localVarTypes 收集函数内可静态确定类型的变量名（参数、接收者、var、:=）。
func localVarTypes(fn *funcInfo) map[string]string {
	vars := map[string]string{}
	addFieldList := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			typ := typeName(field.Type)
			if typ == "" {
				continue
			}
			for _, name := range field.Names {
				vars[name.Name] = typ
			}
		}
	}
	if fn.decl.Recv != nil && len(fn.decl.Recv.List) > 0 {
		field := fn.decl.Recv.List[0]
		if typ := typeName(field.Type); typ != "" {
			for _, name := range field.Names {
				vars[name.Name] = typ
			}
		}
	}
	addFieldList(fn.decl.Type.Params)
	addFieldList(fn.decl.Type.Results)
	if fn.decl.Body == nil {
		return vars
	}
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeclStmt:
			gen, ok := stmt.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typ := typeName(vs.Type)
				if typ == "" && len(vs.Values) == 1 {
					typ = exprType(vs.Values[0])
				}
				if typ == "" {
					continue
				}
				for _, name := range vs.Names {
					vars[name.Name] = typ
				}
			}
		case *ast.AssignStmt:
			if stmt.Tok != token.DEFINE || len(stmt.Lhs) != len(stmt.Rhs) {
				return true
			}
			for i, lhs := range stmt.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if typ := exprType(stmt.Rhs[i]); typ != "" {
					vars[id.Name] = typ
				}
			}
		}
		return true
	})
	return vars
}

func exprType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return typeName(e.X)
		}
	case *ast.CompositeLit:
		return typeName(e.Type)
	case *ast.CallExpr:
		name := ""
		switch fun := e.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if strings.HasPrefix(name, "New") {
			return strings.TrimPrefix(name, "New")
		}
		return ""
	}
	return typeName(expr)
}
