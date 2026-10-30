// Package provider 定义模型协议适配面：Provider / Registry / Router / Client。
// 适配器只做协议映射，不重试；重试、回退与事件由 Client/Router 负责
// （docs/19-interfaces.md §21.5）。
package provider

import (
	"context"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// Provider 是一次对话请求的适配器（流式/非流式归一化）。
type Provider interface {
	Name() string
	Capabilities(modelAlias string) (model.Capabilities, error)
	Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error)
}

// ChatRequest 是统一请求。Router 解析后把 Model 改写为真实 id。
type ChatRequest struct {
	Model             string           `json:"model"` // 别名（Router 解析后改写为真实 id）
	Messages          []model.Message  `json:"messages"`
	Tools             []model.ToolSpec `json:"tools,omitempty"`
	ToolChoice        string           `json:"toolChoice,omitempty"` // "auto" / "none"
	Temperature       *float64         `json:"temperature,omitempty"`
	MaxTokens         int              `json:"maxTokens,omitempty"`
	Stream            bool             `json:"stream"`
	ParallelToolCalls *bool            `json:"parallelToolCalls,omitempty"`
}

// Delta 是流式增量。
type Delta struct {
	Text          string
	ReasoningText string
	ToolCalls     []model.ToolCallDelta // index/id/name/arguments 增量（§5.5）
}

// ChatResponse 是归一化后的响应。
type ChatResponse struct {
	Message      model.Message
	Usage        model.Usage
	FinishReason string // stop / tool_calls / length / content_filter
}

// Resolved 是 §5.1 解析后的结果：别名、provider 名、真实模型 id。
type Resolved struct {
	Alias        string
	ProviderName string
	ModelID      string
	Capabilities model.Capabilities
	Pricing      *model.Pricing
}

// Registry 负责别名解析、能力查询与节点模型选择。
type Registry interface {
	Resolve(alias string) (Resolved, error)
	ResolveForNode(node model.NodeName) (Resolved, error)
	Capabilities(alias string) (model.Capabilities, error)
	Adapter(res Resolved) (Provider, error) // 仅适配器，不含重试/回退
}

// Factory 按 provider 配置造适配器。
type Factory interface {
	New(spec config.ProviderSpec) (Provider, error)
}

// Retrier 执行退避重试（§13.2）。
type Retrier interface {
	Do(ctx context.Context, attempt func(ctx context.Context, n int) error) error
}

// Fallback 按设置顺序切换模型（§13.6）；默认空链 = 永不回退。
type Fallback interface {
	Next(cur Resolved, class model.ErrorClass) (Resolved, bool)
}
