package provider

import (
	"context"

	"github.com/xiws/ant/internal/model"
)

// Router 实现 Provider：agent 只依赖单一 Provider，节点不必知道别名属于哪个适配器。
// 它是唯一把「别名」改写为「真实 model id」的地方（§5.4）。
type Router struct {
	reg      Registry
	retrier  Retrier
	fallback Fallback
	sink     model.EventSink
}

// NewRouter 构造 Router；retrier/fallback 可为零值实现。
func NewRouter(reg Registry, retrier Retrier, fb Fallback, sink model.EventSink) *Router {
	return &Router{reg: reg, retrier: retrier, fallback: fb, sink: sink}
}

// Name 实现 Provider。
func (r *Router) Name() string { return "router" }

// Capabilities 实现 Provider。
func (r *Router) Capabilities(alias string) (model.Capabilities, error) {
	return r.reg.Capabilities(alias)
}

// Chat 实现 Provider：解析别名 → 取适配器 → 重试/回退/事件。
func (r *Router) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error) {
	res, err := r.reg.Resolve(req.Model)
	if err != nil {
		return ChatResponse{}, err
	}
	adapter, err := r.reg.Adapter(res)
	if err != nil {
		return ChatResponse{}, err
	}
	// 能力驱动的请求归一化（§5.2）。
	if req.Stream && !res.Capabilities.SupportsStreaming {
		req.Stream = false
	}
	if !res.Capabilities.SupportsParallelToolCalls {
		f := false
		req.ParallelToolCalls = &f
	}
	if req.MaxTokens <= 0 && res.Capabilities.MaxOutputTokens > 0 {
		req.MaxTokens = res.Capabilities.MaxOutputTokens
	}
	if res.Capabilities.MaxOutputTokens > 0 && req.MaxTokens > res.Capabilities.MaxOutputTokens {
		req.MaxTokens = res.Capabilities.MaxOutputTokens
	}
	req.Model = res.Alias

	client := NewClient(adapter, r.retrier, r.fallback, r.sink)
	client.Bind(res, r.reg.Adapter)
	return client.Chat(ctx, req, onDelta)
}
