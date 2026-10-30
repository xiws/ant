package model

import "context"

// EventSink 写入一条事件。实现方负责分配 Seq/TS 并做持久化；
// 多路扇出时 seq 由第一路（session）回填，后续 sink 看到最终值。
type EventSink interface {
	Emit(ctx context.Context, ev *Event) error
}

// ArtifactStore 把大内容落盘并返回引用（§9.5：事件只存 hash/引用）。
type ArtifactStore interface {
	PutArtifact(ctx context.Context, name string, data []byte, mediaType string) (ArtifactRef, error)
}

// EventSinkFunc 把函数适配为 EventSink，便于测试与组合。
type EventSinkFunc func(ctx context.Context, ev *Event) error

// Emit 实现 EventSink。
func (f EventSinkFunc) Emit(ctx context.Context, ev *Event) error {
	if f == nil {
		return nil
	}
	return f(ctx, ev)
}
