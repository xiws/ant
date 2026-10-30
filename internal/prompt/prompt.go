// Package prompt 按「稳定 → 易变」组装节点消息与 hash（docs/04-prompt.md §6）。
package prompt

import (
	"context"

	"github.com/xiws/ant/internal/model"
)

// Assembler 按固定顺序拼装消息与 hash。
type Assembler interface {
	Assemble(ctx context.Context, req Request) (Bundle, error)
}

// Request 是组装输入；Recent 的最后一条应为本次 user 消息。
type Request struct {
	Node    model.NodeName
	Mode    model.Mode
	Project ProjectDoc       // 由调用方从 projectctx.Doc 转换
	Tools   []model.ToolSpec // 已按权限过滤并排序
	Summary string           // 压缩摘要（§11.3）
	Recent  []model.Message  // 最近 N 轮（已压缩）
	Runtime RuntimeInfo      // 易变尾部
}

// ProjectDoc 是本包自带的最小投影，避免 prompt ↔ projectctx 互相依赖。
type ProjectDoc struct {
	Body string
	Hash string
}

// RuntimeInfo 是易变尾部；不得插入稳定前缀（D12）。
type RuntimeInfo struct {
	CWD      string
	OS       string
	Shell    string
	Date     string // UTC，仅日期，不含时间戳
	Approval string // risk-based / always / never / --yes
}

// Bundle 是组装结果：Messages 顺序即 §6.1，最后一条为本次 user。
type Bundle struct {
	Messages []model.Message
	Meta     Meta
}

// Meta 记录版本与 hash，写入 node_started。
type Meta struct {
	NodePromptVersion string
	ModeOverlayHash   string
	StableHash        string // 1..6 稳定的前缀 hash
	ToolsHash         string
	PromptHash        string // 全量 hash
	Tokens            int    // 估算，供 context 复核
}

// Source 提供稳定前缀素材；覆盖优先级在实现内处理（§6.2）。
type Source interface {
	NodePrompt(node model.NodeName) (Doc, error)
	ModeOverlay(mode model.Mode) (Doc, error) // 不可覆盖（D13）
	UserPrompt(scope Scope) (Doc, error)
}

// Scope 是用户 prompt 的作用域。
type Scope string

const (
	ScopeHome      Scope = "home"
	ScopeWorkspace Scope = "workspace"
)

// Doc 是一段 prompt 文本及其来源。
type Doc struct {
	Path    string
	Text    string
	Version string
	Origin  string
}
