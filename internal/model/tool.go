package model

import (
	"encoding/json"
	"time"
)

// ToolName 是工具的稳定标识（snake_case）。
type ToolName string

const (
	ToolRead     ToolName = "read"
	ToolReadCode ToolName = "read_code"
	ToolSearch   ToolName = "search"
	ToolEdit     ToolName = "edit"
	ToolWrite    ToolName = "write"
	ToolBash     ToolName = "bash"
	ToolAsk      ToolName = "ask"
)

// ToolSpec 描述一个工具；字段与 docs/08-tools.md §10.1 一一对应。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Mutating    bool            `json:"mutating"`
	Timeout     time.Duration   `json:"timeout"`
	Approval    ApprovalClass   `json:"approval"`
	MaxOutput   int             `json:"maxOutput"`
}

// Capabilities 描述一个模型的能力；由配置推导，不在 model 里读配置。
type Capabilities struct {
	ContextWindow             int  `json:"contextWindow"`
	MaxOutputTokens           int  `json:"maxOutputTokens"`
	SupportsTools             bool `json:"supportsTools"`
	SupportsStreaming         bool `json:"supportsStreaming"`
	SupportsParallelToolCalls bool `json:"supportsParallelToolCalls"`
	Reasoning                 bool `json:"reasoning"`
}
