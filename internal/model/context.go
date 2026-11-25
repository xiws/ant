package model

import "context"

type nodeCtxKey struct{}
type runCtxKey struct{}

// WithNode 把当前节点写入 context，供不直接依赖 agent 的装饰器
// （如 provider.Client）在写事件时补齐 node 字段。
func WithNode(ctx context.Context, node NodeName) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nodeCtxKey{}, node)
}

// NodeFromContext 返回 context 中的节点名；不存在时返回空。
func NodeFromContext(ctx context.Context) NodeName {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(nodeCtxKey{}).(NodeName); ok {
		return v
	}
	return ""
}

// WithRun 把一次 node 运行实例的 ID 写入 context。
// 同一次 Run 内的 node_started / model_* / tool_* / message 事件共享该 ID，
// 用于在并发 fanout 中把事件精确归组到同一个节点运行。
func WithRun(ctx context.Context, run string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == "" {
		return ctx
	}
	return context.WithValue(ctx, runCtxKey{}, run)
}

// RunFromContext 返回 context 中的 run ID；不存在时返回空。
func RunFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(runCtxKey{}).(string); ok {
		return v
	}
	return ""
}
