// Package model 是 ant 的共享词汇表（L0）。
//
// 它不 import 任何 internal 包、不做 I/O、不含策略：所有跨模块传递的数据
// 以及两个最小写接口（EventSink / ArtifactStore）都在这里。
// 见 docs/19-interfaces.md §21.3。
package model

import "encoding/json"

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是唯一消息形态。provider 适配器只做「协议 ⇄ Message」映射，
// 不得定义第二套消息类型。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
	// ToolCalls 仅 assistant 消息使用；必须原样保存（§5.7.1 不变量）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 tool 消息使用；与请求严格一一对应。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall 是模型返回的一次工具调用。
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments 保留原始字符串；解析、修复与校验由 tool.Runtime 负责。
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCallDelta 是流式响应中的工具调用增量；按 Index 累加。
type ToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// HasToolCalls 报告消息是否携带工具调用。
func (m Message) HasToolCalls() bool { return len(m.ToolCalls) > 0 }
