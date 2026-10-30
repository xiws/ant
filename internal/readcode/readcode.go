// Package readcode 提供 Go 符号索引与依赖闭包切片（docs/08-tools.md §10.6）。
// 解析与格式化分离，便于单测。
package readcode

import (
	"context"
)

// Index 是 read_code 的库面。
type Index interface {
	// Resolve 解析 target（path#Symbol / Symbol / path:line / pkg.Symbol）。
	// 歧义返回 *model.Error{Code:"ambiguous_symbol"} 并附候选。
	Resolve(ctx context.Context, target string) (Symbol, error)
	// Closure 以符号为根做 BFS，返回按发现顺序排列的代码切片。
	Closure(ctx context.Context, root Symbol, opt Options) ([]Snippet, error)
	// Invalidate 在文件变化后丢弃缓存（session 内缓存）。
	Invalidate(paths []string)
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
}

// Snippet 是一段代码切片。
type Snippet struct {
	File    string   `json:"file"` // workspace 相对路径
	Package string   `json:"package"`
	Imports []string `json:"imports"`
	Code    string   `json:"code"` // 含紧邻文档注释
}
