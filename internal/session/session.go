// Package session 负责事件日志、产物、锁与断点续传（docs/07-session.md §9）。
// 它只依赖 model 词汇表，不 import provider/tool/agent。
package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xiws/ant/internal/model"
)

// Store 打开/列举/查看 session，并负责取锁。
type Store interface {
	Open(ctx context.Context, opts OpenOptions) (Session, Opened, error)
	List(ctx context.Context, f ListFilter) ([]Summary, error)
	Latest(ctx context.Context) (Summary, error)
	Show(ctx context.Context, idPrefix string) (Detail, error)
}

// Opened 告诉调用方「是新建还是续跑」。
type Opened struct {
	ID      string
	Created bool
	Resumed bool
}

// OpenOptions 是打开参数。
type OpenOptions struct {
	ID        string // 空 = 新建；非空 = 续（支持唯一前缀）
	Create    bool
	Mode      model.Mode
	Model     string
	Workspace string
	Lock      bool
}

// Session 是 Recorder + Reader + Meta + Close。
type Session interface {
	Recorder
	Reader
	Meta() Meta
	UpdateMeta(ctx context.Context, mutate func(*Meta) error) error
	Close() error
}

// Meta 对应 meta.json（§9.2）；字段名与 JSON 一致。
type Meta struct {
	SchemaVersion      int               `json:"schemaVersion"`
	ID                 string            `json:"id"`
	Workspace          string            `json:"workspace"`
	CreatedAt          time.Time         `json:"createdAt"`
	UpdatedAt          time.Time         `json:"updatedAt"`
	Status             Status            `json:"status"`
	Mode               model.Mode        `json:"mode"`
	Title              string            `json:"title,omitempty"`
	Model              string            `json:"model,omitempty"`
	TurnCount          int               `json:"turnCount"`
	LastSeq            int64             `json:"lastSeq"`
	Usage              model.Usage       `json:"usage"`
	ProjectContextHash string            `json:"projectContextHash,omitempty"`
	PromptVersions     map[string]string `json:"promptVersions,omitempty"`
}

// Status 是 session 状态；不存在 done（session 永远可以继续追加新 turn）。
type Status string

const (
	StatusIdle        Status = "idle"
	StatusRunning     Status = "running"
	StatusInterrupted Status = "interrupted"
	StatusFailed      Status = "failed"
)

// Summary / Detail 供 `ant sessions`、`ant show` 使用，只读投影。
type Summary struct {
	Meta
	Tokens    int             `json:"tokens"`
	LastEvent model.EventType `json:"lastEvent,omitempty"`
}

// Detail 是单个 session 的详情。
type Detail struct {
	Summary
	Approvals []ApprovalRecord    `json:"approvals,omitempty"`
	Artifacts []model.ArtifactRef `json:"artifacts,omitempty"`
	PlanPath  string              `json:"planPath,omitempty"`
}

// ListFilter 是列举过滤条件。
type ListFilter struct {
	Limit  int
	Mode   model.Mode // 空 = 不过滤
	Status Status     // 空 = 不过滤
}

// ApprovalRecord 是审批历史。
type ApprovalRecord struct {
	Class    model.ApprovalClass `json:"class"`
	Decision string              `json:"decision"`
	Scope    string              `json:"scope"`
	At       time.Time           `json:"at"`
}

// Recorder 是引擎、工具、审批唯一可见的写面。
type Recorder interface {
	model.EventSink
	model.ArtifactStore
}

// Reader 是恢复与展示可见的读面。
type Reader interface {
	Meta() Meta
	Events(ctx context.Context) ([]model.Event, error)
	Artifact(ref model.ArtifactRef) ([]byte, error)
}

// Locker 提供心跳文件锁。
type Locker interface {
	Acquire(ctx context.Context, id string) (Lease, error)
}

// Lease 是持锁凭据。
type Lease interface {
	Heartbeat(ctx context.Context) error
	Release() error
	Stale() bool
}

// Recoverer 折叠事件、重建状态、修复 tool 配对（§9.6）。
type Recoverer interface {
	Recover(ctx context.Context, r Reader, d ReplayDecider) (State, error)
}

// ReplayDecider 决定「上次可能已执行的副作用工具」如何处理（§9.6 第 5 步）。
type ReplayDecider interface {
	Decide(ctx context.Context, call PendingCall) (Replay, error)
}

// Replay 是重放决策。
type Replay int

const (
	ReplaySkip Replay = iota // 合成 interrupted tool 消息
	ReplayRun                // 重跑（仅只读工具默认）
)

// PendingCall 是缺少 tool_result 的调用。
type PendingCall struct {
	CallID   string
	Node     model.NodeName
	Name     model.ToolName
	Args     json.RawMessage
	Mutating bool
}

// State 是折叠事件后的恢复状态。
type State struct {
	Meta     Meta
	LastSeq  int64
	Last     model.EventType
	Turn     int
	Step     model.StepRef
	Round    int
	Messages []model.Message
	Pending  []PendingCall
	Grants   []model.ApprovalClass
	Usage    model.Usage
	Summary  string
	Warnings []string
}
