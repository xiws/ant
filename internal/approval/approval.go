// Package approval 是审批与硬拒绝的唯一入口（docs/10-security.md §12）。
// 审批由引擎侧发起，模型无法通过工具参数伪造「已批准」。
package approval

import (
	"context"

	"github.com/xiws/ant/internal/model"
)

// Approver 按类别决策，返回 allow/deny/scope。
type Approver interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Request 是一次审批请求。
type Request struct {
	Class   model.ApprovalClass
	Node    model.NodeName
	Action  string   // "read" / "write" / "bash" / "plan_to_code"
	Command string   // bash 用
	Paths   []string // 文件工具用
	CWD     string
	Reason  string
}

// Decision 是审批结果。
type Decision struct {
	Allow  bool
	Scope  Scope
	Reason string
}

// Scope 是放行作用域；all 只限本 session（D26）。
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session"
)

// Classifier 返回类别与该操作是否默认需要审批（§12.3）。
type Classifier interface {
	Classify(ctx context.Context, req Request) (model.ApprovalClass, bool, error)
}

// HardDeny 命中内置/追加的硬拒绝模式（§12.4）。
type HardDeny interface {
	Match(command string) (rule string, denied bool)
}

// Prompter 是 cli 实现的交互面；approval 包不接触 os.Stdin（R4）。
type Prompter interface {
	Confirm(ctx context.Context, p ConfirmPrompt) (Choice, error)
	Ask(ctx context.Context, q Question) (Answer, error)
}

// Choice 是审批选项。
type Choice int

const (
	ChoiceDeny Choice = iota
	ChoiceOnce
	ChoiceAll // 本 session 该类别
)

// ConfirmPrompt 是审批提示内容。
type ConfirmPrompt struct {
	Class  model.ApprovalClass
	Title  string
	Detail string
	CWD    string
	Reason string
}

// Question 是 ask 工具的提问。
type Question struct {
	Question      string
	Options       []Option
	AllowFreeText bool
}

// Option 是提问选项。
type Option struct {
	ID          string
	Label       string
	Description string
}

// Answer 是提问结果。
type Answer struct {
	SelectedID string
	Text       string
}

// Grants 是本 session 的类别放行记录。
type Grants interface {
	Granted(class model.ApprovalClass) bool
	Grant(class model.ApprovalClass)
	Restore(classes []model.ApprovalClass)
}

// ErrNonInteractive 在需要用户输入但处于 --non-interactive 时返回（退出码 4）。
var ErrNonInteractive = &model.Error{
	Class:   model.ErrApprovalRequired,
	Code:    "non_interactive",
	Message: "当前为非交互模式，需要审批或用户回答",
	Hint:    "使用 --yes 或交互模式",
}
