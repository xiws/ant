// Package readcode 提供 Go 代码切片（docs/08-tools.md §10.6）。
// 支持两种读取模式：按文件列出函数（不展开依赖），按符号做依赖闭包。
// 解析与格式化分离，便于单测。
package readcode

import (
	"context"
)

// Index 是 read_code 的库面。
type Index interface {
	// Read 是统一入口：target 可以是文件、文件#函数、函数名、path:line 或 pkg.Function。
	// 文件/文件#函数默认只返回本体；只有显式 Expand 才展开依赖闭包。
	Read(ctx context.Context, req Request) (Result, error)
	// Resolve 解析符号 target（path#Symbol / Symbol / path:line / pkg.Symbol）。
	// 歧义返回 *model.Error{Code:"ambiguous_symbol"} 并附候选。
	Resolve(ctx context.Context, target string) (Symbol, error)
	// Closure 以符号为根做 BFS，返回按发现顺序排列的代码切片。
	Closure(ctx context.Context, root Symbol, opt Options) ([]Snippet, error)
	// Invalidate 在文件变化后丢弃缓存（session 内缓存）。
	Invalidate(paths []string)
}

// Mode 是 Read 的读取模式。
type Mode string

const (
	// ModeFile 表示 target 指向文件：返回文件内全部函数/方法，不展开依赖。
	ModeFile Mode = "file"
	// ModeSymbol 表示 target 指向符号：返回函数本体或依赖闭包。
	ModeSymbol Mode = "symbol"
)

// Request 是 read_code 的参数（工具层只做参数映射，不在这里解析 JSON）。
type Request struct {
	Target     string
	Depth      int
	MaxSymbols int
	// Expand 表示显式要求从 file#函数 目标展开依赖闭包。
	// 文件与 file#函数 目标默认只返回本体；裸符号名/path:line 仍按 §10.6 展开。
	Expand bool
}

// Result 是 Read 的解析结果。
type Result struct {
	Mode     Mode
	Target   string
	File     string  // ModeFile 时的 workspace 相对文件路径
	Symbol   *Symbol // ModeSymbol 时的根符号
	Snippets []Snippet
	NoExpand bool     // 本次是否只返回函数本体
	Omitted  []string // 依赖闭包因 maxSymbols 被截断的符号名
}

// Symbol 是一个 Go 符号。
type Symbol struct {
	Name     string `json:"name"`
	Package  string `json:"package"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Receiver string `json:"receiver,omitempty"`
}

// Options 是闭包参数。
type Options struct {
	Depth       int
	MaxSymbols  int
	IncludeTest bool
	ModulePath  string
	// NoExpand 只返回根函数本身，不展开任何依赖（对应 read_code 的本体模式）。
	NoExpand bool
}

// Snippet 是一段代码切片。
type Snippet struct {
	File    string   `json:"file"` // workspace 相对路径
	Package string   `json:"package"`
	Name    string   `json:"name"` // 函数/方法名
	Line    int      `json:"line"` // 函数声明行（含紧邻文档注释时仍是 func 所在行）
	Imports []string `json:"imports"`
	Code    string   `json:"code"` // 含紧邻文档注释
}
