package model

import (
	"encoding/json"
	"time"
)

// EventType 是事件类型；与 docs/07-session.md §9.3 一一对应。
// 新增类型只能在末尾追加（§9.9）。
type EventType string

const (
	EventSessionCreated     EventType = "session_created"
	EventModeChanged        EventType = "mode_changed"
	EventTurnStarted        EventType = "turn_started"
	EventNodeStarted        EventType = "node_started"
	EventModelRequest       EventType = "model_request"
	EventModelResponse      EventType = "model_response"
	EventModelFallback      EventType = "model_fallback"
	EventMessage            EventType = "message"
	EventToolCall           EventType = "tool_call"
	EventToolResult         EventType = "tool_result"
	EventApprovalRequested  EventType = "approval_requested"
	EventApprovalResolved   EventType = "approval_resolved"
	EventContextCompacted   EventType = "context_compacted"
	EventStepCompleted      EventType = "step_completed"
	EventNodeCompleted      EventType = "node_completed"
	EventTurnCompleted      EventType = "turn_completed"
	EventSessionInterrupted EventType = "session_interrupted"
	EventSessionFailed      EventType = "session_failed"
	EventUsage              EventType = "usage"
)

// EventTSLayout 是事件时间戳的持久化格式：UTC RFC3339 毫秒（§9.3）。
const EventTSLayout = "2006-01-02T15:04:05.000Z"

// Event 是持久化契约：Data 用 json.RawMessage，写入前必须确定字段（§9.9）。
type Event struct {
	Seq  int64           `json:"seq"`
	TS   time.Time       `json:"ts"`
	Type EventType       `json:"type"`
	Turn int             `json:"turn,omitempty"`
	Step string          `json:"step,omitempty"`
	Node NodeName        `json:"node,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// eventWire 是 Event 的序列化形态：ts 固定为 UTC RFC3339 毫秒。
type eventWire struct {
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Type EventType       `json:"type"`
	Turn int             `json:"turn,omitempty"`
	Step string          `json:"step,omitempty"`
	Node NodeName        `json:"node,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// MarshalJSON 按 §9.3 输出 UTC RFC3339 毫秒时间戳。
func (e Event) MarshalJSON() ([]byte, error) {
	ts := ""
	if !e.TS.IsZero() {
		ts = e.TS.UTC().Format(EventTSLayout)
	}
	return json.Marshal(eventWire{
		Seq: e.Seq, TS: ts, Type: e.Type, Turn: e.Turn, Step: e.Step, Node: e.Node, Data: e.Data,
	})
}

// UnmarshalJSON 接受 RFC3339（含毫秒）时间戳；缺失或非法时保留零值。
func (e *Event) UnmarshalJSON(data []byte) error {
	var w eventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.Seq, e.Type, e.Turn, e.Step, e.Node, e.Data = w.Seq, w.Type, w.Turn, w.Step, w.Node, w.Data
	e.TS = time.Time{}
	if w.TS != "" {
		if t, err := time.Parse(time.RFC3339Nano, w.TS); err == nil {
			e.TS = t.UTC()
		} else if t, err := time.Parse(EventTSLayout, w.TS); err == nil {
			e.TS = t.UTC()
		}
	}
	return nil
}

// KnownEventType 报告事件类型是否属于当前 schema。
func KnownEventType(t EventType) bool {
	switch t {
	case EventSessionCreated, EventModeChanged, EventTurnStarted, EventNodeStarted,
		EventModelRequest, EventModelResponse, EventModelFallback, EventMessage,
		EventToolCall, EventToolResult, EventApprovalRequested, EventApprovalResolved,
		EventContextCompacted, EventStepCompleted, EventNodeCompleted, EventTurnCompleted,
		EventSessionInterrupted, EventSessionFailed, EventUsage:
		return true
	}
	return false
}
