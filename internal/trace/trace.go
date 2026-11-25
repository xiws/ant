// Package trace 把 session 事件与 artifact 重建为可读的调试链路，
// 并提供 Markdown / HTML 两种离线渲染（docs/13-observability.md §15.5）。
package trace

import (
	"encoding/json"
	"time"

	"github.com/xiws/ant/internal/model"
)

// Options 是 Build 的可选输入。
type Options struct {
	PlanPath   string
	Plan       string
	Artifacts  []model.ArtifactRef
	IncludeRaw bool
	AntVersion string
	ExportedAt time.Time
}

// Document 是一个 session 的完整调试链路。
type Document struct {
	SessionID  string
	Workspace  string
	Title      string
	Mode       model.Mode
	Model      string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExportedAt time.Time
	AntVersion string
	Usage      model.Usage

	Turns     []*Turn
	Plan      string
	PlanPath  string
	Artifacts []model.ArtifactRef
	Warnings  []string
	RawEvents []model.Event
}

// Turn 是一个用户 turn。
type Turn struct {
	Index       int
	Input       string
	StartedAt   time.Time
	CompletedAt time.Time
	Status      string
	ExitCode    int
	Usage       model.Usage
	Nodes       []*Node
	Approvals   []*Approval
	Compactions []*Compaction
	ModeChanges []*ModeChange
	Error       string
	Warnings    []string
}

// Node 是一次节点运行（同一 node 的 fanout 分支各自一个）。
type Node struct {
	RunID         string
	Name          model.NodeName
	Step          string
	Model         string
	Provider      string
	PromptVersion string
	PromptHash    string
	StableHash    string
	ToolsHash     string
	StartedAt     time.Time
	Input         *NodeInput
	InputRef      string
	Rounds        []*Round
	Output        *NodeOutput
	OutputRef     string
	Completed     *NodeCompleted
	Messages      []Message
	Compactions   []*Compaction
	Warnings      []string
}

// NodeInput 对应 node_started.inputRef 的 artifact。
type NodeInput struct {
	Messages  []model.Message
	Tools     []model.ToolSpec
	Meta      map[string]any
	Request   string
	Plan      string
	Dimension string
	Feedback  json.RawMessage
}

// Round 是一次模型请求 + 响应。
type Round struct {
	Index         int
	Request       *ModelRequest
	Response      *ModelResponse
	Assistant     *Message
	ToolCalls     []*ToolCall
	Messages      []model.Message // 本轮请求消息（首轮原文，后续重建）
	Reconstructed bool
	Warnings      []string
}

// ModelRequest 是 model_request 事件的可读投影。
type ModelRequest struct {
	At                   time.Time
	Attempt              int
	MessageCount         int
	EstimatedInputTokens int
	Tools                []string
	Stream               bool
	Model                string
	Provider             string
}

// ModelResponse 是 model_response 事件的可读投影。
type ModelResponse struct {
	At            time.Time
	Attempt       int
	FinishReason  string
	Usage         model.Usage
	Duration      time.Duration
	TextHash      string
	ToolCallCount int
	Model         string
	ErrorClass    string
	Code          string
	Message       string
}

// Message 是 assistant/tool 消息的可读投影。
type Message struct {
	At         time.Time
	Role       model.Role
	Content    string
	ContentRef string
	ToolCallID string
	ToolCalls  []model.ToolCall
}

// ToolCall 是一次工具调用及其结果。
type ToolCall struct {
	ID            string
	Name          string
	Arguments     json.RawMessage
	Approval      string
	Status        string
	OK            bool
	Duration      time.Duration
	ExitCode      int
	Truncated     bool
	OutputRef     string
	Error         *ToolError
	Content       string
	ContentRef    string
	FullOutput    string
	FullOutputRef string
}

// ToolError 是工具错误的可读投影。
type ToolError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Hint    string          `json:"hint"`
	Details json.RawMessage `json:"details"`
}

// NodeOutput 对应 step_completed.outputRef 的 artifact。
type NodeOutput struct {
	Status     string
	Text       string
	Structured json.RawMessage
	Usage      model.Usage
	ToolCalls  int
	Changed    []string
	Checks     []model.Check
	Ref        string
}

// NodeCompleted 对应 node_completed 事件。
type NodeCompleted struct {
	At        time.Time
	Status    string
	Duration  time.Duration
	Usage     model.Usage
	Verdict   json.RawMessage
	Checks    []model.Check
	Changed   []string
	Dimension string
	Error     string
}

// Compaction 对应 context_compacted 事件。
type Compaction struct {
	At           time.Time
	BeforeTokens int
	AfterTokens  int
	Removed      int
	Kept         int
	Summary      string
	SummaryRef   string
}

// Approval 对应 approval_requested / approval_resolved。
type Approval struct {
	At       time.Time
	Class    string
	Decision string
	Scope    string
	Node     string
	Action   string
	Command  string
	Reason   string
}

// ModeChange 对应 mode_changed。
type ModeChange struct {
	At        time.Time
	From      string
	To        string
	Reason    string
	Confirmed bool
}
