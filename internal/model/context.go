package model

import "context"

type nodeCtxKey struct{}

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
