// Package fake 提供可脚本化的 provider.Provider 测试替身（§17.2）。
//
// 它不访问网络；CI 全部使用 fake 或 httptest，不调用真实 API。
package fake

import (
	"context"
	"sync"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/provider"
)

// Step 是一次预设响应：可包含流式增量、最终响应或错误。
type Step struct {
	Deltas   []provider.Delta
	Response provider.ChatResponse
	Err      error
}

// Provider 是 scripted fake provider。
type Provider struct {
	name string
	mu   sync.Mutex
	caps model.Capabilities
	step []Step
	reqs []provider.ChatRequest
}

// Option 配置 fake。
type Option func(*Provider)

// WithCapabilities 覆盖默认能力。
func WithCapabilities(c model.Capabilities) Option {
	return func(p *Provider) { p.caps = c }
}

// WithName 覆盖 Name()。
func WithName(name string) Option {
	return func(p *Provider) { p.name = name }
}

// New 构造 fake provider；默认能力足以跑通工具循环。
func New(opts ...Option) *Provider {
	p := &Provider{
		name: "fake",
		caps: model.Capabilities{
			ContextWindow:             1_000_000,
			MaxOutputTokens:           8192,
			SupportsTools:             true,
			SupportsStreaming:         true,
			SupportsParallelToolCalls: true,
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Queue 追加一个完整响应。
func (p *Provider) Queue(resp provider.ChatResponse) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step = append(p.step, Step{Response: resp})
	return p
}

// QueueText 追加一个纯文本响应。
func (p *Provider) QueueText(text string) *Provider {
	return p.Queue(provider.ChatResponse{
		Message:      model.Message{Role: model.RoleAssistant, Content: text},
		FinishReason: "stop",
		Usage:        model.Usage{Input: len(text) / 4, Output: len(text) / 4},
	})
}

// QueueToolCalls 追加一个只包含工具调用的响应。
func (p *Provider) QueueToolCalls(calls ...model.ToolCall) *Provider {
	return p.Queue(provider.ChatResponse{
		Message:      model.Message{Role: model.RoleAssistant, ToolCalls: calls},
		FinishReason: "tool_calls",
	})
}

// QueueJSON 追加一个结构化输出响应（verifier/deliberate/aggregation 用）。
func (p *Provider) QueueJSON(text string) *Provider { return p.QueueText(text) }

// QueueError 追加一个错误响应。
func (p *Provider) QueueError(err error) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step = append(p.step, Step{Err: err})
	return p
}

// QueueStream 追加一个流式响应：按顺序回调 Deltas，然后返回 Response。
func (p *Provider) QueueStream(deltas []provider.Delta, resp provider.ChatResponse) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step = append(p.step, Step{Deltas: deltas, Response: resp})
	return p
}

// Name 实现 Provider。
func (p *Provider) Name() string { return p.name }

// Capabilities 实现 Provider。
func (p *Provider) Capabilities(string) (model.Capabilities, error) { return p.caps, nil }

// Chat 实现 Provider：按顺序消费脚本；脚本耗尽时返回空文本 stop 响应。
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest, onDelta func(provider.Delta) error) (provider.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return provider.ChatResponse{}, err
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	if len(p.step) == 0 {
		p.mu.Unlock()
		return provider.ChatResponse{
			Message:      model.Message{Role: model.RoleAssistant, Content: ""},
			FinishReason: "stop",
		}, nil
	}
	step := p.step[0]
	p.step = p.step[1:]
	p.mu.Unlock()

	for _, d := range step.Deltas {
		if onDelta != nil {
			if err := onDelta(d); err != nil {
				return provider.ChatResponse{}, err
			}
		}
	}
	if step.Err != nil {
		return provider.ChatResponse{}, step.Err
	}
	return step.Response, nil
}

// Requests 返回已收到的请求副本。
func (p *Provider) Requests() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.ChatRequest, len(p.reqs))
	copy(out, p.reqs)
	return out
}

// Remaining 返回剩余脚本数。
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.step)
}
