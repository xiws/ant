// Package projectctx 负责 `.ant/project.md` 的扫描、hash 失效、生成与手改保护
// （docs/12-project-context.md §14）。
package projectctx

import (
	"context"
	"time"

	"github.com/xiws/ant/internal/model"
)

// Builder 判定 hash 失效、生成并保护手改内容。
type Builder interface {
	Ensure(ctx context.Context, opts EnsureOptions) (Doc, error)
}

// EnsureOptions 是 Ensure 参数。
type EnsureOptions struct {
	Force       bool
	SkipIfFresh bool
	// Explicit 表示用户显式执行 `ant init`：即使 project.autoInit=false 也应生成。
	Explicit bool
	Progress func(stage string)
}

// Doc 是 project.md 的投影。
type Doc struct {
	Path        string
	Body        string
	Hash        string // sha256:...
	GeneratedAt time.Time
	FromModel   bool // false 表示退回确定性骨架
}

// Scanner 扫描 workspace。
type Scanner interface {
	Scan(ctx context.Context, opts ScanOptions) (ScanResult, error)
}

// ScanOptions 是扫描参数。
type ScanOptions struct {
	WorkDir  string
	MaxFiles int
	MaxBytes int
	Depth    int
	Exclude  []string
	Progress func(stage string)
}

// ScanResult 是扫描结果。
type ScanResult struct {
	ProjectType string
	ModulePath  string
	Tree        []string
	Entries     []string
	Deps        []string
	Commands    Commands
	KeyFiles    []KeyFile
	FileCount   int
	BytesRead   int
}

// KeyFile 是关键文件内容。
type KeyFile struct {
	Path string
	Body string
}

// Hasher 计算失效 hash。
type Hasher interface {
	Hash(ctx context.Context, r ScanResult) (string, error)
}

// Generator 生成 project.md 正文。
type Generator interface {
	Generate(ctx context.Context, s Skeleton) (md string, usage model.Usage, err error)
}

// Skeleton 是确定性骨架。
type Skeleton struct {
	ProjectType string
	ModulePath  string
	Tree        []string
	Entries     []string
	Deps        []string
	Commands    Commands
	KeyFiles    []KeyFile
}

// Commands 是构建/测试/lint 命令。
type Commands struct {
	Build []string
	Test  []string
	Lint  []string
}
