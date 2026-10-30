// Package ctxmgr（目录 internal/context）负责 token 预算、压缩与成本
// （docs/09-context.md §11）。导入时统一使用别名：
//
//	ctxmgr "github.com/xiws/ant/internal/context"
package ctxmgr

import (
	"context"

	"github.com/xiws/ant/internal/model"
)

// Preparer 是「模型请求前」的唯一预算/压缩入口。
type Preparer interface {
	Prepare(ctx context.Context, in PrepareInput) (Prepared, error)
}

// PrepareInput 是准备输入。Pinned 永不压缩（§11.4）。
type PrepareInput struct {
	Node    model.NodeName
	Cap     model.Capabilities
	Pricing *model.Pricing
	Pinned  []model.Message
	History []model.Message
	Tools   []model.ToolSpec
	Summary string
	Usage   model.Usage
}

// Prepared 是发送给模型的消息视图；日志与历史不受影响。
type Prepared struct {
	Messages  []model.Message // = 摘要占位 + History' + Pinned
	Budget    Budget
	Compacted bool
	Summary   string
	Ref       model.ArtifactRef
}

// Budget 是一次请求的预算。
type Budget struct {
	Window        int
	InputBudget   int
	OutputReserve int
	Used          int
}

// Estimator 估算消息与工具的 token。
type Estimator interface {
	Estimate(messages []model.Message, tools []model.ToolSpec) int
}

// Compactor 执行「配对安全」的截断；摘要由 Summarizer 负责。
type Compactor interface {
	Compact(ctx context.Context, in CompactInput) (CompactOutput, error)
}

// CompactInput 是压缩输入。
type CompactInput struct {
	History         []model.Message
	Pinned          []model.Message
	Budget          Budget
	KeepRecentTurns int
	Summary         string
}

// CompactOutput 是压缩结果；Summary 为确定性摘要或空。
type CompactOutput struct {
	History      []model.Message
	Removed      []model.Message
	Summary      string
	BeforeTokens int
	AfterTokens  int
	Compacted    bool
}

// Summarizer 把旧消息压成结构化摘要。
type Summarizer interface {
	Summarize(ctx context.Context, in SummaryInput) (string, model.Usage, error)
}

// SummaryInput 是摘要输入。
type SummaryInput struct {
	Model      string
	Messages   []model.Message
	Previous   string
	MaxTokens  int
	TurnsRange string // 来源 turn 范围，写入摘要头部（§11.4）
}

// CostCalculator 按 pricing 估算费用；无 pricing 时 known=false。
type CostCalculator interface {
	Cost(p *model.Pricing, u model.Usage) (amount float64, known bool)
}
