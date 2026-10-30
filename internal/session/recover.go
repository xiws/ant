package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// recoverer 是 Recoverer 的无状态实现。
type recoverer struct{}

// NewRecoverer 返回 Recoverer 实现。
func NewRecoverer() Recoverer { return recoverer{} }

func (recoverer) Recover(ctx context.Context, r Reader, d ReplayDecider) (State, error) {
	return Recover(ctx, r, d)
}

// Recover 折叠 events.jsonl，重建状态并修复 tool 配对（docs/07-session.md §9.6）。
func Recover(ctx context.Context, r Reader, d ReplayDecider) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, contextErr(ctx)
	}
	if r == nil {
		return State{}, sessionErr(model.ErrInternal, "recover_nil_reader",
			"Recover 需要非 nil Reader", "组合根应在打开 session 后再调用 Recover", nil)
	}
	meta := r.Meta()
	events, err := r.Events(ctx)
	if err != nil {
		return State{}, err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	state := State{
		Meta:    cloneMeta(meta),
		LastSeq: meta.LastSeq,
		Turn:    meta.TurnCount,
		Usage:   meta.Usage,
	}
	if wr, ok := r.(interface{ Warnings() []string }); ok {
		state.Warnings = append(state.Warnings, wr.Warnings()...)
	}

	b := &recoveryBuilder{
		reader:    r,
		decider:   d,
		calls:     make(map[string]*callState),
		callOwner: make(map[string]int),
		synthetic: make(map[int]bool),
		toolMsg:   make(map[string]int),
		grantSeen: make(map[model.ApprovalClass]bool),
	}
	if err := b.fold(ctx, events, &state); err != nil {
		return State{}, err
	}
	if err := b.finish(ctx); err != nil {
		return State{}, err
	}

	state.LastSeq = meta.LastSeq
	if len(events) > 0 {
		state.LastSeq = maxEventSeq(events)
		state.Last = events[len(events)-1].Type
	}
	state.Messages = b.messages
	state.Pending = b.pendingCalls()
	state.Grants = b.grants
	state.Summary = b.summary
	state.Warnings = append(state.Warnings, b.warnings...)

	switch {
	case b.usageFromEvents:
		state.Usage = b.usage
	case b.responseUsageSeen:
		state.Usage = b.responseUsage
	case b.stepUsageSeen:
		state.Usage = b.stepUsage
	}

	if state.Turn < b.maxTurn {
		state.Turn = b.maxTurn
	}
	if state.Turn > state.Meta.TurnCount {
		state.Meta.TurnCount = state.Turn
	}
	if b.mode != "" {
		state.Meta.Mode = b.mode
	}
	state.Meta.LastSeq = state.LastSeq
	state.Meta.Usage = state.Usage
	applyRecoveredStatus(&state)
	return state, nil
}

type callState struct {
	pending    PendingCall
	resolved   bool
	flushed    bool
	status     string
	resultRaw  []byte
	outputRef  model.ArtifactRef
	hasOutRef  bool
	resultText string
}

type recoveryBuilder struct {
	reader  Reader
	decider ReplayDecider

	messages  []model.Message
	callOwner map[string]int // tool_call_id -> assistant message index
	synthetic map[int]bool   // 由 tool_call 事件临时合成的 assistant 消息
	toolMsg   map[string]int // tool_call_id -> tool 消息 index

	calls map[string]*callState
	order []string

	grants    []model.ApprovalClass
	grantSeen map[model.ApprovalClass]bool

	usage             model.Usage
	usageFromEvents   bool
	responseUsage     model.Usage
	responseUsageSeen bool
	stepUsage         model.Usage
	stepUsageSeen     bool

	summary  string
	mode     model.Mode
	maxTurn  int
	warnings []string
}

func (b *recoveryBuilder) warn(msg string) {
	if msg == "" {
		return
	}
	b.warnings = append(b.warnings, msg)
}

// fold 按 seq 顺序折叠事件。
func (b *recoveryBuilder) fold(ctx context.Context, events []model.Event, state *State) error {
	b.mode = state.Meta.Mode
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return contextErr(ctx)
		}
		if ev.Turn > b.maxTurn {
			b.maxTurn = ev.Turn
		}
		b.updateStep(state, ev)
		switch ev.Type {
		case model.EventSessionCreated:
			var data struct {
				Mode model.Mode `json:"mode"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && data.Mode != "" {
				b.mode = data.Mode
			}
		case model.EventModeChanged:
			var data struct {
				To model.Mode `json:"to"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && data.To != "" {
				b.mode = data.To
			}
		case model.EventTurnStarted:
			if ev.Turn > 0 {
				if ev.Turn > state.Turn {
					state.Turn = ev.Turn
				}
			} else {
				state.Turn++
			}
			var data struct {
				Input   json.RawMessage `json:"input"`
				Request string          `json:"request"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				b.warn(fmt.Sprintf("turn_started seq=%d data 无法解析: %v", ev.Seq, err))
				continue
			}
			input := decodeInput(data.Input)
			if input == "" {
				input = data.Request
			}
			if err := b.flushPending(ctx, nil); err != nil {
				return err
			}
			b.appendUser(input)
		case model.EventMessage:
			if err := b.handleMessage(ctx, ev); err != nil {
				return err
			}
		case model.EventToolCall:
			b.handleToolCall(ev)
		case model.EventToolResult:
			b.handleToolResult(ev)
		case model.EventUsage:
			var usage model.Usage
			if err := json.Unmarshal(ev.Data, &usage); err != nil {
				b.warn(fmt.Sprintf("usage seq=%d data 无法解析: %v", ev.Seq, err))
				continue
			}
			b.usage = b.usage.Add(usage)
			b.usageFromEvents = true
		case model.EventModelResponse:
			var data struct {
				Usage model.Usage `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && !data.Usage.IsZero() {
				b.responseUsage = b.responseUsage.Add(data.Usage)
				b.responseUsageSeen = true
			}
		case model.EventStepCompleted, model.EventNodeCompleted:
			var data struct {
				Usage model.Usage `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && !data.Usage.IsZero() {
				b.stepUsage = b.stepUsage.Add(data.Usage)
				b.stepUsageSeen = true
			}
		case model.EventApprovalResolved:
			b.handleApprovalResolved(ev)
		case model.EventContextCompacted:
			b.handleContextCompacted(ev)
		default:
			if !model.KnownEventType(ev.Type) {
				b.warn(fmt.Sprintf("unknown event type %q at seq=%d ignored", ev.Type, ev.Seq))
			}
		}
	}
	return nil
}

// updateStep 解析 step 字符串（如 plan.round-1.verifier）为 model.StepRef。
func (b *recoveryBuilder) updateStep(state *State, ev model.Event) {
	if ev.Step != "" {
		ref := parseStepRef(ev.Step, ev.Node)
		if ref.Mode == "" {
			ref.Mode = b.mode
		}
		if ref.Mode != "" {
			b.mode = ref.Mode
		}
		if ref.Round == 0 {
			ref.Round = state.Round
		}
		state.Step = ref
		if ref.Round > 0 {
			state.Round = ref.Round
		}
		return
	}
	switch ev.Type {
	case model.EventNodeStarted, model.EventStepCompleted, model.EventNodeCompleted:
		if ev.Node == "" {
			return
		}
		state.Step = model.StepRef{Mode: b.mode, Round: state.Round, Node: ev.Node}
	}
}

// parseStepRef 解析 "code"、"plan.round-1.verifier" 等 step 标识。
func parseStepRef(raw string, node model.NodeName) model.StepRef {
	ref := model.StepRef{}
	for _, part := range strings.Split(strings.TrimSpace(raw), ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if ref.Mode == "" && model.Mode(part).Valid() {
			ref.Mode = model.Mode(part)
			continue
		}
		if strings.HasPrefix(part, "round-") {
			if n, err := strconv.Atoi(strings.TrimPrefix(part, "round-")); err == nil && n > 0 {
				ref.Round = n
				continue
			}
		}
		if isNodeName(part) {
			ref.Node = model.NodeName(part)
			continue
		}
		if ref.Phase == "" {
			ref.Phase = part
		}
	}
	if ref.Node == "" {
		ref.Node = node
	}
	return ref
}

func isNodeName(name string) bool {
	for _, node := range model.AllNodes() {
		if string(node) == name {
			return true
		}
	}
	return false
}

// handleMessage 重建 message 事件；contentRef 缺失时用占位内容并 warning。
func (b *recoveryBuilder) handleMessage(ctx context.Context, ev model.Event) error {
	var data struct {
		Role        string          `json:"role"`
		Content     string          `json:"content"`
		ContentRef  json.RawMessage `json:"contentRef"`
		ContentHash string          `json:"contentHash"`
		Name        string          `json:"name"`
		ToolCallID  string          `json:"toolCallID"`
		ToolCallID2 string          `json:"tool_call_id"`
		ToolCalls   json.RawMessage `json:"toolCalls"`
		ToolCalls2  json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		b.warn(fmt.Sprintf("message seq=%d data 无法解析: %v", ev.Seq, err))
		return nil
	}
	_ = data.ContentHash

	content := data.Content
	if ref, ok := decodeArtifactRef(data.ContentRef); ok {
		body, err := b.reader.Artifact(ref)
		if err != nil {
			b.warn(fmt.Sprintf("message seq=%d 的 contentRef %q 缺失，已用占位内容: %v",
				ev.Seq, ref.Name, err))
			content = missingArtifactPlaceholder(ref)
		} else {
			content = string(body)
		}
	}

	toolCallID := data.ToolCallID
	if toolCallID == "" {
		toolCallID = data.ToolCallID2
	}

	rawCalls := data.ToolCalls
	if len(rawCalls) == 0 {
		rawCalls = data.ToolCalls2
	}
	var calls []model.ToolCall
	if len(rawCalls) > 0 && string(rawCalls) != "null" {
		if err := json.Unmarshal(rawCalls, &calls); err != nil {
			b.warn(fmt.Sprintf("message seq=%d 的 toolCalls 无法解析: %v", ev.Seq, err))
			calls = nil
		}
	}

	role := model.Role(data.Role)
	if role == "" {
		switch {
		case toolCallID != "":
			role = model.RoleTool
		case len(calls) > 0:
			role = model.RoleAssistant
		default:
			role = model.RoleAssistant
		}
	}
	msg := model.Message{
		Role:       role,
		Content:    content,
		Name:       data.Name,
		ToolCalls:  calls,
		ToolCallID: toolCallID,
	}

	switch role {
	case model.RoleTool:
		if toolCallID != "" {
			b.markResolved(toolCallID)
			b.appendToolMessage(toolCallID, content)
			return nil
		}
		if err := b.flushPending(ctx, nil); err != nil {
			return err
		}
		b.messages = append(b.messages, msg)
	case model.RoleUser:
		if err := b.flushPending(ctx, nil); err != nil {
			return err
		}
		b.appendUser(content)
	case model.RoleAssistant:
		if len(calls) > 0 {
			return b.appendAssistantWithCalls(ctx, msg, calls, ev.Node)
		}
		if err := b.flushPending(ctx, nil); err != nil {
			return err
		}
		b.messages = append(b.messages, msg)
	default:
		if err := b.flushPending(ctx, nil); err != nil {
			return err
		}
		b.messages = append(b.messages, msg)
	}
	return nil
}

func (b *recoveryBuilder) appendUser(content string) {
	if content == "" {
		return
	}
	if n := len(b.messages); n > 0 {
		last := b.messages[n-1]
		// turn_started 与 message(user) 可能同时存在，避免重复。
		if last.Role == model.RoleUser && last.Content == content {
			return
		}
	}
	b.messages = append(b.messages, model.Message{Role: model.RoleUser, Content: content})
}

// appendAssistantWithCalls 合并/追加 assistant tool_calls，并登记调用状态。
func (b *recoveryBuilder) appendAssistantWithCalls(ctx context.Context, msg model.Message, calls []model.ToolCall, node model.NodeName) error {
	except := make(map[string]bool, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			except[call.ID] = true
		}
	}

	// tool_call 事件可能先于 message 事件到达；优先把消息内容合并进临时 assistant。
	mergeIdx := -1
	if n := len(b.messages); n > 0 && b.messages[n-1].Role == model.RoleAssistant && b.synthetic[n-1] {
		mergeIdx = n - 1
	}
	if mergeIdx < 0 {
		for _, call := range calls {
			if idx, ok := b.callOwner[call.ID]; ok && b.synthetic[idx] {
				mergeIdx = idx
				break
			}
		}
	}
	if mergeIdx >= 0 {
		for _, call := range b.messages[mergeIdx].ToolCalls {
			except[call.ID] = true
		}
	}
	if err := b.flushPending(ctx, except); err != nil {
		return err
	}

	for _, call := range calls {
		b.recordCall(call, node)
	}

	if mergeIdx >= 0 {
		last := &b.messages[mergeIdx]
		last.Content = msg.Content
		if msg.Name != "" {
			last.Name = msg.Name
		}
		for _, call := range calls {
			if owner, ok := b.callOwner[call.ID]; ok {
				if owner != mergeIdx {
					b.warn(fmt.Sprintf("tool_call_id %q 在多个 assistant 消息中重复，已保留首个", call.ID))
				}
				continue
			}
			if call.ID == "" {
				continue
			}
			last.ToolCalls = append(last.ToolCalls, call)
			b.callOwner[call.ID] = mergeIdx
		}
		delete(b.synthetic, mergeIdx)
		return nil
	}

	msg.ToolCalls = calls
	idx := len(b.messages)
	b.messages = append(b.messages, msg)
	for _, call := range calls {
		if call.ID == "" {
			continue
		}
		if _, exists := b.callOwner[call.ID]; exists {
			b.warn(fmt.Sprintf("tool_call_id %q 在多个 assistant 消息中重复，已保留首个", call.ID))
			continue
		}
		b.callOwner[call.ID] = idx
	}
	return nil
}

// handleToolCall 记录调用；若日志中缺失 assistant 消息，则合成一条以保持协议合法。
func (b *recoveryBuilder) handleToolCall(ev model.Event) {
	var data struct {
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		CallID2   string          `json:"callId"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Node      string          `json:"node"`
		Approval  string          `json:"approval"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		b.warn(fmt.Sprintf("tool_call seq=%d data 无法解析: %v", ev.Seq, err))
		return
	}
	id := data.ID
	if id == "" {
		id = data.CallID
	}
	if id == "" {
		id = data.CallID2
	}
	if id == "" {
		b.warn(fmt.Sprintf("tool_call seq=%d 缺少 id，已忽略", ev.Seq))
		return
	}
	node := ev.Node
	if node == "" && data.Node != "" {
		node = model.NodeName(data.Node)
	}
	call := model.ToolCall{ID: id, Name: data.Name, Arguments: data.Arguments}
	b.recordCall(call, node)
	b.ensureAssistantCall(call, node)
}

func (b *recoveryBuilder) ensureAssistantCall(call model.ToolCall, node model.NodeName) {
	if call.ID == "" {
		return
	}
	if _, exists := b.callOwner[call.ID]; exists {
		return
	}
	if n := len(b.messages); n > 0 {
		last := &b.messages[n-1]
		if last.Role == model.RoleAssistant && len(last.ToolCalls) > 0 {
			last.ToolCalls = append(last.ToolCalls, call)
			b.callOwner[call.ID] = n - 1
			return
		}
	}
	idx := len(b.messages)
	b.messages = append(b.messages, model.Message{
		Role:      model.RoleAssistant,
		ToolCalls: []model.ToolCall{call},
	})
	b.callOwner[call.ID] = idx
	b.synthetic[idx] = true
	_ = node
}

// recordCall 登记一次调用；同一 ID 只保留一份状态。
func (b *recoveryBuilder) recordCall(call model.ToolCall, node model.NodeName) {
	if call.ID == "" {
		return
	}
	cs, exists := b.calls[call.ID]
	if !exists {
		cs = &callState{
			pending: PendingCall{
				CallID:   call.ID,
				Node:     node,
				Name:     model.ToolName(call.Name),
				Args:     cloneRaw(call.Arguments),
				Mutating: isMutatingTool(model.ToolName(call.Name)),
			},
		}
		b.calls[call.ID] = cs
		b.order = append(b.order, call.ID)
		return
	}
	if cs.pending.Name == "" && call.Name != "" {
		cs.pending.Name = model.ToolName(call.Name)
		cs.pending.Mutating = isMutatingTool(cs.pending.Name)
	}
	if len(cs.pending.Args) == 0 && len(call.Arguments) > 0 {
		cs.pending.Args = cloneRaw(call.Arguments)
	}
	if cs.pending.Node == "" && node != "" {
		cs.pending.Node = node
	}
}

func (b *recoveryBuilder) handleToolResult(ev model.Event) {
	var data struct {
		ID         string          `json:"id"`
		CallID     string          `json:"call_id"`
		CallID2    string          `json:"callId"`
		Status     string          `json:"status"`
		OutputRef  json.RawMessage `json:"outputRef"`
		Content    string          `json:"content"`
		Error      string          `json:"error"`
		Truncated  bool            `json:"truncated"`
		ExitCode   int             `json:"exitCode"`
		DurationMS int64           `json:"durationMs"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		b.warn(fmt.Sprintf("tool_result seq=%d data 无法解析: %v", ev.Seq, err))
		return
	}
	id := data.ID
	if id == "" {
		id = data.CallID
	}
	if id == "" {
		id = data.CallID2
	}
	if id == "" {
		b.warn(fmt.Sprintf("tool_result seq=%d 缺少 id，已忽略", ev.Seq))
		return
	}
	cs, exists := b.calls[id]
	if !exists {
		b.warn(fmt.Sprintf("tool_result seq=%d 的 id=%q 没有对应 tool_call", ev.Seq, id))
		return
	}
	cs.resolved = true
	cs.status = data.Status
	cs.resultText = data.Content
	cs.resultRaw = append([]byte(nil), ev.Data...)
	if ref, ok := decodeArtifactRef(data.OutputRef); ok {
		cs.outputRef = ref
		cs.hasOutRef = true
	}
}

func (b *recoveryBuilder) handleApprovalResolved(ev model.Event) {
	var data struct {
		Class    string `json:"class"`
		Decision string `json:"decision"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		b.warn(fmt.Sprintf("approval_resolved seq=%d data 无法解析: %v", ev.Seq, err))
		return
	}
	if !strings.EqualFold(data.Decision, "allow") || data.Scope != "session" {
		return
	}
	class := model.ApprovalClass(data.Class)
	if class == "" || b.grantSeen[class] {
		return
	}
	b.grantSeen[class] = true
	b.grants = append(b.grants, class)
}

func (b *recoveryBuilder) handleContextCompacted(ev model.Event) {
	var data struct {
		Summary    string          `json:"summary"`
		SummaryRef json.RawMessage `json:"summaryRef"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		b.warn(fmt.Sprintf("context_compacted seq=%d data 无法解析: %v", ev.Seq, err))
		return
	}
	if data.Summary != "" {
		b.summary = data.Summary
		return
	}
	ref, ok := decodeArtifactRef(data.SummaryRef)
	if !ok {
		return
	}
	body, err := b.reader.Artifact(ref)
	if err != nil {
		b.warn(fmt.Sprintf("context_compacted seq=%d 的 summaryRef %q 缺失: %v",
			ev.Seq, ref.Name, err))
		return
	}
	b.summary = summaryFromArtifact(body)
}

// summaryFromArtifact 兼容 preparer 写入的 {"summary":...} 与纯文本摘要。
func summaryFromArtifact(body []byte) string {
	var obj struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.Summary != "" {
		return obj.Summary
	}
	return string(body)
}

func (b *recoveryBuilder) markResolved(id string) {
	if cs, ok := b.calls[id]; ok {
		cs.resolved = true
	}
}

// appendToolMessage 保证每个 tool_call_id 最多一条 tool 消息。
func (b *recoveryBuilder) appendToolMessage(id, content string) {
	if id == "" {
		b.messages = append(b.messages, model.Message{Role: model.RoleTool, Content: content})
		return
	}
	if _, exists := b.toolMsg[id]; exists {
		return
	}
	idx := len(b.messages)
	b.messages = append(b.messages, model.Message{
		Role:       model.RoleTool,
		ToolCallID: id,
		Content:    content,
	})
	b.toolMsg[id] = idx
}

// flushPending 为仍未完成的调用合成 interrupted tool 消息（doc §9.6 第 5 步）。
// except 中的调用属于当前正在拼接的 assistant 消息，先不处理。
func (b *recoveryBuilder) flushPending(ctx context.Context, except map[string]bool) error {
	for _, id := range b.order {
		cs := b.calls[id]
		if cs == nil || cs.resolved || cs.flushed || except[id] {
			continue
		}
		content, err := b.decide(ctx, cs)
		if err != nil {
			return err
		}
		b.appendToolMessage(id, content)
		cs.flushed = true
	}
	return nil
}

func (b *recoveryBuilder) decide(ctx context.Context, cs *callState) (string, error) {
	replay := ReplaySkip
	if b.decider != nil {
		decision, err := b.decider.Decide(ctx, cs.pending)
		if err != nil {
			return "", &model.Error{
				Class:   model.ErrInternal,
				Code:    "replay_decide_failed",
				Message: fmt.Sprintf("恢复时无法决定工具调用 %q 的重放策略", cs.pending.CallID),
				Hint:    "ReplayDecider 返回错误；中止恢复以避免静默重放副作用工具",
				Cause:   err,
			}
		}
		replay = decision
	}
	message := fmt.Sprintf("上次会话中断，工具调用 %s (%s) 未完成，已跳过；如仍需执行请重新发起。",
		cs.pending.CallID, cs.pending.Name)
	if replay == ReplayRun {
		b.warn(fmt.Sprintf(
			"恢复决策 ReplayRun for tool call %s (%s)：组合根不会自动重放，已合成 interrupted tool 消息",
			cs.pending.CallID, cs.pending.Name))
	}
	return string(model.MustJSON(interruptedToolResult{
		Status:  "interrupted",
		Message: message,
	})), nil
}

type interruptedToolResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// finish 补齐缺失的 tool 消息：已完成的用 result 兜底，未完成的合成 interrupted。
func (b *recoveryBuilder) finish(ctx context.Context) error {
	for _, id := range b.order {
		cs := b.calls[id]
		if cs == nil {
			continue
		}
		if _, exists := b.toolMsg[id]; exists {
			continue
		}
		if cs.resolved {
			b.appendToolMessage(id, b.resultContent(cs))
			continue
		}
		if cs.flushed {
			continue
		}
		content, err := b.decide(ctx, cs)
		if err != nil {
			return err
		}
		b.appendToolMessage(id, content)
		cs.flushed = true
	}
	return nil
}

func (b *recoveryBuilder) resultContent(cs *callState) string {
	if cs.resultText != "" {
		return cs.resultText
	}
	if cs.hasOutRef {
		body, err := b.reader.Artifact(cs.outputRef)
		if err == nil {
			return string(body)
		}
		b.warn(fmt.Sprintf("tool_result 的 outputRef %q 缺失，使用事件摘要兜底: %v",
			cs.outputRef.Name, err))
	}
	if len(cs.resultRaw) > 0 {
		return string(cs.resultRaw)
	}
	status := cs.status
	if status == "" {
		status = "unknown"
	}
	return string(model.MustJSON(map[string]any{"status": status}))
}

func (b *recoveryBuilder) pendingCalls() []PendingCall {
	out := make([]PendingCall, 0, len(b.order))
	for _, id := range b.order {
		cs := b.calls[id]
		if cs == nil || cs.resolved {
			continue
		}
		p := cs.pending
		p.Args = cloneRaw(p.Args)
		out = append(out, p)
	}
	return out
}

func applyRecoveredStatus(state *State) {
	switch state.Last {
	case model.EventTurnCompleted:
		if len(state.Pending) == 0 {
			state.Meta.Status = StatusIdle
		}
	case model.EventSessionInterrupted:
		state.Meta.Status = StatusInterrupted
	case model.EventSessionFailed:
		state.Meta.Status = StatusFailed
	default:
		if len(state.Pending) > 0 || midTurnEvent(state.Last) {
			state.Meta.Status = StatusInterrupted
		}
	}
}

func midTurnEvent(t model.EventType) bool {
	switch t {
	case model.EventTurnStarted, model.EventNodeStarted, model.EventModelRequest,
		model.EventModelResponse, model.EventModelFallback, model.EventMessage,
		model.EventToolCall, model.EventToolResult, model.EventApprovalRequested,
		model.EventApprovalResolved, model.EventContextCompacted, model.EventStepCompleted,
		model.EventNodeCompleted, model.EventUsage:
		return true
	}
	return false
}

func isMutatingTool(name model.ToolName) bool {
	switch name {
	case model.ToolEdit, model.ToolWrite, model.ToolBash:
		return true
	}
	return false
}

func decodeArtifactRef(raw json.RawMessage) (model.ArtifactRef, bool) {
	if len(raw) == 0 {
		return model.ArtifactRef{}, false
	}
	var ref model.ArtifactRef
	if err := json.Unmarshal(raw, &ref); err == nil && ref.Name != "" {
		return ref, true
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil && strings.TrimSpace(name) != "" {
		return model.ArtifactRef{Name: name}, true
	}
	return model.ArtifactRef{}, false
}

func missingArtifactPlaceholder(ref model.ArtifactRef) string {
	name := ref.Name
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("[artifact missing: %s]", name)
}

// decodeInput 兼容 input 为字符串或 {"text":...} 对象。
func decodeInput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Text != "" {
			return obj.Text
		}
		return obj.Content
	}
	return ""
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return json.RawMessage(cp)
}

var _ Recoverer = recoverer{}
