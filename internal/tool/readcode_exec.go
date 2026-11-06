package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/readcode"
)

// readCodeExec 实现 §10.6 read_code：参数映射 + 输出格式化，解析交给 readcode.Index。
func readCodeExec(cfg config.View, deps Deps) func(context.Context, Invocation) (Result, error) {
	return func(ctx context.Context, inv Invocation) (Result, error) {
		target, _ := inv.Args["target"].(string)
		if strings.TrimSpace(target) == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 target",
				"使用 path#Symbol、Symbol、path:line 或 pkg.Symbol", nil)}, nil
		}
		if deps.Index == nil {
			return Result{OK: false, Err: &ToolError{
				Code:    "unsupported_language",
				Message: "read_code 索引未装配",
				Hint:    "使用 read / search；这是内部装配错误",
			}}, nil
		}
		depth := intArg(inv.Args, "depth", 3)
		if depth <= 0 {
			depth = 3
		}
		if depth > 6 {
			depth = 6
		}
		maxSymbols := intArg(inv.Args, "maxSymbols", 50)
		if maxSymbols <= 0 {
			maxSymbols = 50
		}
		if maxSymbols > 100 {
			maxSymbols = 100
		}
		root, err := deps.Index.Resolve(ctx, strings.TrimSpace(target))
		if err != nil {
			return Result{OK: false, Err: toolError(err)}, nil
		}
		includeTest := strings.HasSuffix(target, "_test.go") || strings.HasSuffix(root.File, "_test.go")
		snippets, err := deps.Index.Closure(ctx, root, readcode.Options{
			Depth:       depth,
			MaxSymbols:  maxSymbols,
			IncludeTest: includeTest,
		})
		if err != nil {
			return Result{OK: false, Err: toolError(err)}, nil
		}
		text := formatSnippets(snippets)
		if text == "" {
			return Result{OK: false, Err: &ToolError{
				Code:    "not_found",
				Message: fmt.Sprintf("没有找到 %s 的代码切片", target),
				Hint:    "检查符号名，或先用 search 定位",
			}}, nil
		}
		symbols := make([]map[string]any, 0, len(snippets))
		for _, s := range snippets {
			symbols = append(symbols, map[string]any{
				"file": s.File, "package": s.Package,
			})
		}
		data := map[string]any{
			"target":     target,
			"symbol":     root,
			"depth":      depth,
			"maxSymbols": maxSymbols,
			"count":      len(snippets),
			"symbols":    symbols,
			"content":    text,
			"text":       text,
			"truncated":  len(snippets) >= maxSymbols,
		}
		if o, ok := deps.Index.(interface{ Omitted() []string }); ok {
			if omitted := o.Omitted(); len(omitted) > 0 {
				data["omitted"] = omitted
				data["hint"] = "依赖图超过 maxSymbols，已省略：" + strings.Join(omitted, "、")
			}
		}
		return Result{OK: true, Data: data}, nil
	}
}

// formatSnippets 按 §10.6 的格式输出：文件路径、package、import、函数源码，块间 ---。
func formatSnippets(snippets []readcode.Snippet) string {
	type fileBlock struct {
		file    string
		pkg     string
		imports []string
		codes   []string
	}
	order := make([]string, 0, len(snippets))
	byFile := map[string]*fileBlock{}
	for _, s := range snippets {
		block := byFile[s.File]
		if block == nil {
			block = &fileBlock{file: s.File, pkg: s.Package, imports: s.Imports}
			byFile[s.File] = block
			order = append(order, s.File)
		}
		code := strings.TrimRight(s.Code, "\n")
		if strings.TrimSpace(code) != "" {
			block.codes = append(block.codes, code)
		}
	}
	var out strings.Builder
	for i, file := range order {
		block := byFile[file]
		if i > 0 {
			out.WriteString("\n\n---\n")
		}
		out.WriteString(file)
		out.WriteString("\n")
		if block.pkg != "" {
			out.WriteString("package ")
			out.WriteString(block.pkg)
			out.WriteString("\n")
		}
		if imports := formatImports(block.imports); imports != "" {
			out.WriteString(imports)
			out.WriteString("\n")
		}
		out.WriteString(strings.Join(block.codes, "\n\n"))
	}
	return strings.TrimRight(out.String(), "\n") + "\n"
}

func formatImports(imports []string) string {
	clean := make([]string, 0, len(imports))
	seen := map[string]bool{}
	for _, imp := range imports {
		imp = strings.TrimSpace(imp)
		if imp == "" || seen[imp] {
			continue
		}
		seen[imp] = true
		clean = append(clean, imp)
	}
	if len(clean) == 0 {
		return ""
	}
	if len(clean) == 1 {
		return "import " + clean[0]
	}
	var b strings.Builder
	b.WriteString("import (\n")
	for _, imp := range clean {
		b.WriteString("\t")
		b.WriteString(imp)
		b.WriteString("\n")
	}
	b.WriteString(")")
	return b.String()
}
