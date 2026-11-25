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
		target = strings.TrimSpace(target)
		if target == "" {
			return Result{OK: false, Err: invalidArgs("invalid_args", "缺少 target",
				"使用文件路径、文件#函数、函数名、path:line 或 pkg.Function", nil)}, nil
		}
		if deps.Index == nil {
			return Result{OK: false, Err: &ToolError{
				Code:    "unsupported_language",
				Message: "read_code 索引未装配",
				Hint:    "使用 read / search；这是内部装配错误",
			}}, nil
		}
		// depth 是“展开依赖”的开关：只在显式给出时对 file#函数 生效，
		// 因此用 args 中是否存在来决定 Expand，而不是用默认值。
		depth, expand := 0, false
		if _, ok := inv.Args["depth"]; ok {
			expand = true
			depth = intArg(inv.Args, "depth", 3)
			if depth <= 0 {
				depth = 1
			}
			if depth > 6 {
				depth = 6
			}
		}
		maxSymbols := intArg(inv.Args, "maxSymbols", 50)
		if maxSymbols <= 0 {
			maxSymbols = 50
		}
		if maxSymbols > 100 {
			maxSymbols = 100
		}

		res, err := deps.Index.Read(ctx, readcode.Request{
			Target:     target,
			Depth:      depth,
			MaxSymbols: maxSymbols,
			Expand:     expand,
		})
		if err != nil {
			return Result{OK: false, Err: toolError(err)}, nil
		}

		withLine := res.Mode == readcode.ModeFile || res.NoExpand
		budget := maxOutputBytesOf(cfg) - 1024
		if budget < 512 {
			budget = 512
		}
		kept, dropped := fitSnippets(res.Snippets, budget, withLine)
		text := formatSnippets(kept, withLine)
		if strings.TrimSpace(text) == "" {
			return Result{OK: false, Err: &ToolError{
				Code:    "not_found",
				Message: fmt.Sprintf("没有找到 %s 的代码切片", target),
				Hint:    "检查路径与符号名，或先用 search 定位",
			}}, nil
		}

		symbols := make([]map[string]any, 0, len(kept))
		for _, s := range kept {
			symbols = append(symbols, map[string]any{
				"name": s.Name, "file": s.File, "package": s.Package, "line": s.Line,
			})
		}
		data := map[string]any{
			"mode":      string(res.Mode),
			"target":    target,
			"count":     len(kept),
			"symbols":   symbols,
			"content":   text,
			"truncated": len(dropped) > 0 || len(res.Omitted) > 0,
		}
		if res.File != "" {
			data["file"] = res.File
		}
		if res.Symbol != nil {
			data["symbol"] = *res.Symbol
		}
		if expand {
			data["depth"] = depth
			data["maxSymbols"] = maxSymbols
		}
		if res.NoExpand {
			data["noExpand"] = true
		}
		if len(res.Omitted) > 0 {
			data["omitted"] = res.Omitted
		}
		if hint := buildReadCodeHint(res, dropped); hint != "" {
			data["hint"] = hint
		}
		return Result{OK: true, Data: data}, nil
	}
}

// buildReadCodeHint 汇总依赖截断与输出截断的续读提示。
func buildReadCodeHint(res readcode.Result, dropped []readcode.Snippet) string {
	hints := make([]string, 0, 2)
	if len(res.Omitted) > 0 {
		hints = append(hints, "依赖图超过 maxSymbols，已省略："+strings.Join(res.Omitted, "、"))
	}
	if len(dropped) > 0 {
		names := make([]string, 0, len(dropped))
		for _, s := range dropped {
			if s.Name != "" {
				names = append(names, s.Name)
			}
		}
		shown, suffix := names, ""
		if len(shown) > 10 {
			shown, suffix = shown[:10], "、…"
		}
		hint := fmt.Sprintf("输出超过上限，已省略 %d 个函数（%s%s）", len(dropped), strings.Join(shown, "、"), suffix)
		if res.Mode == readcode.ModeFile && res.File != "" {
			hint += fmt.Sprintf("；用 target=\"%s#函数名\" 单独读取", res.File)
		}
		hints = append(hints, hint)
	}
	return strings.Join(hints, "；")
}

// fitSnippets 按输出预算裁剪切片，返回保留与省略的部分。
func fitSnippets(snippets []readcode.Snippet, budget int, withLine bool) (kept, dropped []readcode.Snippet) {
	used := 0
	for i, s := range snippets {
		size := len(s.Code) + len(s.File) + 64
		if withLine {
			size += len(s.File) + 16
		}
		if used+size > budget {
			return kept, snippets[i:]
		}
		used += size
		kept = append(kept, s)
	}
	return kept, nil
}

// formatSnippets 按 §10.6 的格式输出：文件路径、package、import、函数源码，块间 ---。
// withLine 为真时每个函数前加 file:line 标题（文件/本体模式）。
func formatSnippets(snippets []readcode.Snippet, withLine bool) string {
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
		if strings.TrimSpace(code) == "" {
			continue
		}
		if withLine {
			code = fmt.Sprintf("%s:%d\n%s", s.File, s.Line, code)
		}
		block.codes = append(block.codes, code)
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
		if len(block.codes) > 0 {
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
