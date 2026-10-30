// Package tool 是工具运行时：注册、校验、审批、执行、截断与事件
// （docs/08-tools.md §10）。节点只依赖 Registry 与 Runtime 两张窄接口。
package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/readcode"
)

// Tool 是单个工具的执行体；Execute 拿到的是已解析、已校验的参数。
type Tool interface {
	Spec() model.ToolSpec
	Execute(ctx context.Context, inv Invocation) (Result, error)
}

// Invocation 是一次已校验的调用。
type Invocation struct {
	CallID    string
	Node      model.NodeName
	Args      map[string]any
	Raw       json.RawMessage
	Workspace string
}

// Result 是工具返回；ToolError 表示工具本身没执行成功。
type Result struct {
	OK   bool
	Data any
	Err  *ToolError
	Meta ResultMeta
}

// ResultMeta 是执行附加信息。
type ResultMeta struct {
	Duration  time.Duration
	Truncated bool
	OutputRef model.ArtifactRef
	ExitCode  int // bash/verifier 类工具
}

// ToolError 是结构化工具错误（§10.4）。
type ToolError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Error 实现 error，便于日志。
func (e *ToolError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// Registry 负责名称查找与按节点权限出 ToolSpec。
type Registry interface {
	Lookup(name string) (Tool, bool)
	Specs(node model.NodeName) []model.ToolSpec // 按权限矩阵过滤（§7.3）并排序
}

// Runtime 是节点唯一可见的执行面：一批调用 → 一批 tool 消息。
type Runtime interface {
	ExecuteBatch(ctx context.Context, node model.NodeName, calls []model.ToolCall) ([]model.Message, error)
}

// Validator 做 Schema 校验与类型转换。
type Validator interface {
	Validate(spec model.ToolSpec, raw json.RawMessage) (map[string]any, *ToolError)
}

// Repairer 做有限 JSON 修复（§13.4）。
type Repairer interface {
	Repair(raw []byte) (out []byte, repaired bool)
}

// Truncator 截断输出并落 artifact。
type Truncator interface {
	Clip(ctx context.Context, callID string, body []byte) (kept []byte, ref model.ArtifactRef, truncated bool, err error)
}

// PathGuard 做 workspace 约束、符号链接与保护路径校验（§12.2）。
type PathGuard interface {
	Resolve(ctx context.Context, path string, access Access) (Resolved, error)
}

// Access 是访问类型。
type Access int

const (
	AccessRead Access = iota
	AccessWrite
)

// Resolved 是路径解析结果。
type Resolved struct {
	Abs     string
	Rel     string
	Outside bool // 是否越界（需审批）
	Created bool // 父目录是否需要创建
}

// VersionTable 记录本 session 最后读取/写入的 hash。
type VersionTable interface {
	LastRead(path string) (sha string, ok bool)
	RecordRead(path, sha string)
	RecordWrite(path, sha string)
}

// Toolkit 是工具层的装配结果。
type Toolkit struct {
	Registry Registry
	Runtime  Runtime
}

// Deps 是 Toolkit 的外部协作者。
type Deps struct {
	Guard    PathGuard
	Versions VersionTable
	Approver approval.Approver // tool → approval 单向依赖
	Prompter approval.Prompter // ask 工具使用
	Index    readcode.Index    // read_code 工具使用
	Sink     model.EventSink
	Store    model.ArtifactStore
}
