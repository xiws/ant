package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

const runtimeSystemPrefix = "## 运行时信息"

// Build 从 session 的 meta/events/artifacts 重建完整链路。
// artifact 缺失只记 warning，不让整个导出失败。
func Build(ctx context.Context, r session.Reader, opts Options) (*Document, error) {
	if r == nil {
		return nil, fmt.Errorf("trace: 缺少 session reader")
	}
	events, err := r.Events(ctx)
	if err != nil {
		return nil, err
	}
	meta := r.Meta()
	doc := &Document{
		SessionID:  meta.ID,
		Workspace:  meta.Workspace,
		Title:      meta.Title,
		Mode:       meta.Mode,
		Model:      meta.Model,
		Status:     string(meta.Status),
		CreatedAt:  meta.CreatedAt,
		UpdatedAt:  meta.UpdatedAt,
		ExportedAt: opts.ExportedAt,
		AntVersion: opts.AntVersion,
		Usage:      meta.Usage,
		Plan:       opts.Plan,
		PlanPath:   opts.PlanPath,
		Artifacts:  append([]model.ArtifactRef(nil), opts.Artifacts...),
	}
	if doc.ExportedAt.IsZero() {
		doc.ExportedAt = time.Now()
	}
	if doc.AntVersion == "" {
		doc.AntVersion = "dev"
	}
	if opts.IncludeRaw {
		doc.RawEvents = append([]model.Event(nil), events...)
	}
	if w, ok := r.(interface{ Warnings() []string }); ok {
		doc.Warnings = append(doc.Warnings, w.Warnings()...)
	}

	b := &builder{
		doc:          doc,
		r:            r,
		byRun:        map[string]*Node{},
		byNode:       map[string][]*Node{},
		byTool:       map[string]*ToolCall{},
		conversation: map[*Node][]model.Message{},
	}
	for i := range events {
		b.handle(ctx, &events[i])
	}
	b.finish()
	return doc, nil
}

type builder struct {
	doc *Document
	r   session.Reader

	turn          *Turn
	byRun         map[string]*Node
	byNode        map[string][]*Node
	byTool        map[string]*ToolCall
	conversation  map[*Node][]model.Message
	legacyRunWarn bool
}

func (b *builder) warn(format string, args ...any) {
	b.doc.Warnings = append(b.doc.Warnings, fmt.Sprintf(format, args...))
}

func (b *builder) nodeWarn(n *Node, format string, args ...any) {
	if n == nil {
		b.warn(format, args...)
		return
	}
	n.Warnings = append(n.Warnings, fmt.Sprintf(format, args...))
}

func (b *builder) ensureTurn(ev *model.Event) *Turn {
	if b.turn != nil && (ev.Turn <= 0 || b.turn.Index == ev.Turn) {
		return b.turn
	}
	idx := ev.Turn
	if idx <= 0 {
		idx = len(b.doc.Turns) + 1
	}
	t := &Turn{Index: idx, StartedAt: ev.TS}
	b.doc.Turns = append(b.doc.Turns, t)
	b.turn = t
	return t
}

func (b *builder) findNode(ev *model.Event) *Node {
	if ev.Run != "" {
		if n := b.byRun[ev.Run]; n != nil {
			return n
		}
	}
	if ev.Node == "" {
		return nil
	}
	list := b.byNode[string(ev.Node)]
	if len(list) == 0 {
		return nil
	}
	// 旧 session 无 run：取当前 turn 内最近一次同名节点。
	if b.turn != nil {
		for i := len(b.turn.Nodes) - 1; i >= 0; i-- {
			if b.turn.Nodes[i].Name == ev.Node {
				return b.turn.Nodes[i]
			}
		}
	}
	return list[len(list)-1]
}

func (b *builder) handle(ctx context.Context, ev *model.Event) {
	if ev == nil {
		return
	}
	if ev.Type == model.EventTurnStarted {
		b.handleTurnStarted(ev)
		return
	}
	if ev.Type == model.EventSessionCreated {
		return
	}
	turn := b.ensureTurn(ev)

	switch ev.Type {
	case model.EventNodeStarted:
		b.handleNodeStarted(ctx, ev, turn)
	case model.EventModelRequest:
		b.handleModelRequest(ev)
	case model.EventModelResponse:
		b.handleModelResponse(ev)
	case model.EventMessage:
		b.handleMessage(ctx, ev)
	case model.EventToolCall:
		b.handleToolCall(ev)
	case model.EventToolResult:
		b.handleToolResult(ev)
	case model.EventStepCompleted:
		b.handleStepCompleted(ctx, ev)
	case model.EventNodeCompleted:
		b.handleNodeCompleted(ev)
	case model.EventContextCompacted:
		b.handleCompacted(ctx, ev, turn)
	case model.EventApprovalRequested:
		b.handleApprovalRequested(ev, turn)
	case model.EventApprovalResolved:
		b.handleApprovalResolved(ev, turn)
	case model.EventModeChanged:
		b.handleModeChanged(ev, turn)
	case model.EventTurnCompleted:
		b.handleTurnCompleted(ev, turn)
	case model.EventSessionFailed:
		b.handleSessionEvent(ev, turn, "failed")
	case model.EventSessionInterrupted:
		b.handleSessionEvent(ev, turn, "interrupted")
	case model.EventUsage:
		// usage 已合并到 model_response/node/turn。
	default:
		if !model.KnownEventType(ev.Type) {
			b.warn("seq=%d 未知事件类型 %q，已忽略", ev.Seq, ev.Type)
		}
	}
}

func (b *builder) handleTurnStarted(ev *model.Event) {
	turn := &Turn{Index: ev.Turn, StartedAt: ev.TS}
	if turn.Index <= 0 {
		turn.Index = len(b.doc.Turns) + 1
	}
	var d struct {
		Input string `json:"input"`
	}
	if json.Unmarshal(ev.Data, &d) == nil {
		turn.Input = d.Input
	}
	b.doc.Turns = append(b.doc.Turns, turn)
	b.turn = turn
}

func (b *builder) handleNodeStarted(ctx context.Context, ev *model.Event, turn *Turn) {
	var d struct {
		Node          model.NodeName  `json:"node"`
		Model         string          `json:"model"`
		Provider      string          `json:"provider"`
		PromptVersion string          `json:"promptVersion"`
		PromptHash    string          `json:"promptHash"`
		StableHash    string          `json:"stableHash"`
		ToolsHash     string          `json:"toolsHash"`
		InputRef      json.RawMessage `json:"inputRef"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		b.warn("seq=%d node_started data 解析失败: %v", ev.Seq, err)
		return
	}
	nodeName := d.Node
	if nodeName == "" {
		nodeName = ev.Node
	}
	runID := ev.Run
	if runID == "" {
		runID = fmt.Sprintf("%s-%d", nodeName, ev.Seq)
	}
	n := &Node{
		RunID: runID, Name: nodeName, Step: ev.Step,
		Model: d.Model, Provider: d.Provider,
		PromptVersion: d.PromptVersion, PromptHash: d.PromptHash,
		StableHash: d.StableHash, ToolsHash: d.ToolsHash,
		StartedAt: ev.TS,
	}
	if ref, ok := decodeRef(d.InputRef); ok {
		n.InputRef = ref.Name
		if body := b.readArtifact(ref, "node-input", n); body != nil {
			var in struct {
				Messages  []model.Message  `json:"messages"`
				Tools     []model.ToolSpec `json:"tools"`
				Meta      map[string]any   `json:"meta"`
				Request   string           `json:"request"`
				Plan      string           `json:"plan"`
				Dimension string           `json:"dimension"`
				Feedback  json.RawMessage  `json:"feedback"`
			}
			if err := json.Unmarshal(body, &in); err == nil {
				n.Input = &NodeInput{
					Messages: in.Messages, Tools: in.Tools, Meta: in.Meta,
					Request: in.Request, Plan: in.Plan,
					Dimension: in.Dimension, Feedback: in.Feedback,
				}
				b.conversation[n] = append([]model.Message(nil), in.Messages...)
			} else {
				b.nodeWarn(n, "node-input 解析失败：%v", err)
			}
		}
	}
	if ev.Run != "" {
		b.byRun[ev.Run] = n
	} else if !b.legacyRunWarn {
		b.legacyRunWarn = true
		b.warn("旧 session 的 node_started 缺少 run 标识，事件按 node/step/dimension 回退归组；并发分支可能不精确")
	}
	if nodeName != "" {
		b.byNode[string(nodeName)] = append(b.byNode[string(nodeName)], n)
	}
	turn.Nodes = append(turn.Nodes, n)
}

func (b *builder) handleModelRequest(ev *model.Event) {
	n := b.findNode(ev)
	if n == nil {
		b.warn("seq=%d model_request 找不到所属节点", ev.Seq)
		return
	}
	var d struct {
		Model                string   `json:"model"`
		Provider             string   `json:"provider"`
		MessageCount         int      `json:"messageCount"`
		EstimatedInputTokens int      `json:"estimatedInputTokens"`
		Tools                []string `json:"tools"`
		Stream               bool     `json:"stream"`
		Attempt              int      `json:"attempt"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		b.nodeWarn(n, "seq=%d model_request data 解析失败: %v", ev.Seq, err)
		return
	}
	first := len(n.Rounds) == 0
	msgs := b.requestMessages(n, first)
	round := &Round{
		Index: len(n.Rounds) + 1,
		Request: &ModelRequest{
			At: ev.TS, Attempt: d.Attempt, MessageCount: d.MessageCount,
			EstimatedInputTokens: d.EstimatedInputTokens,
			Tools:                d.Tools, Stream: d.Stream, Model: d.Model, Provider: d.Provider,
		},
		Messages:      msgs,
		Reconstructed: !first,
	}
	if d.MessageCount > 0 && len(msgs) > 0 && len(msgs) != d.MessageCount {
		round.Warnings = append(round.Warnings,
			fmt.Sprintf("重建请求消息数 %d 与事件记录 %d 不一致（可能因上下文压缩）", len(msgs), d.MessageCount))
	}
	n.Rounds = append(n.Rounds, round)
}

func (b *builder) requestMessages(n *Node, first bool) []model.Message {
	conv := b.conversation[n]
	if len(conv) == 0 && n.Input != nil {
		conv = append([]model.Message(nil), n.Input.Messages...)
	}
	if first || n.Input == nil {
		return append([]model.Message(nil), conv...)
	}
	return reconstructRequest(n.Input.Messages, conv)
}

// reconstructRequest 按 baseNode 的组装规则，把初始消息 + 后续 assistant/tool 消息
// 重建为下一轮请求。运行时 system 块在最后一轮不是 user 时位于末尾。
func reconstructRequest(initial, conversation []model.Message) []model.Message {
	var base, runtime []model.Message
	for _, m := range initial {
		if isRuntimeMessage(m) {
			runtime = append(runtime, m)
			continue
		}
		base = append(base, m)
	}
	appended := conversation
	if len(conversation) >= len(initial) {
		appended = conversation[len(initial):]
	}
	out := make([]model.Message, 0, len(base)+len(appended)+len(runtime))
	out = append(out, base...)
	out = append(out, appended...)
	if len(runtime) == 0 {
		return out
	}
	if len(out) > 0 && out[len(out)-1].Role == model.RoleUser {
		last := out[len(out)-1]
		out = append(out[:len(out)-1], runtime...)
		out = append(out, last)
		return out
	}
	return append(out, runtime...)
}

func isRuntimeMessage(m model.Message) bool {
	return m.Role == model.RoleSystem && strings.HasPrefix(strings.TrimSpace(m.Content), runtimeSystemPrefix)
}

func (b *builder) handleModelResponse(ev *model.Event) {
	n := b.findNode(ev)
	if n == nil {
		b.warn("seq=%d model_response 找不到所属节点", ev.Seq)
		return
	}
	var d struct {
		FinishReason  string      `json:"finishReason"`
		Usage         model.Usage `json:"usage"`
		TextHash      string      `json:"textHash"`
		ToolCallCount int         `json:"toolCallCount"`
		DurationMs    int64       `json:"durationMs"`
		Attempt       int         `json:"attempt"`
		Model         string      `json:"model"`
		ErrorClass    string      `json:"errorClass"`
		Code          string      `json:"code"`
		Message       string      `json:"message"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		b.nodeWarn(n, "seq=%d model_response data 解析失败: %v", ev.Seq, err)
		return
	}
	resp := &ModelResponse{
		At: ev.TS, Attempt: d.Attempt, FinishReason: d.FinishReason,
		Usage: d.Usage, TextHash: d.TextHash, ToolCallCount: d.ToolCallCount,
		Duration: time.Duration(d.DurationMs) * time.Millisecond, Model: d.Model,
		ErrorClass: d.ErrorClass, Code: d.Code, Message: d.Message,
	}
	round := b.currentRound(n)
	if round == nil {
		round = &Round{}
		n.Rounds = append(n.Rounds, round)
	}
	round.Response = resp
}

func (b *builder) currentRound(n *Node) *Round {
	if n == nil || len(n.Rounds) == 0 {
		return nil
	}
	return n.Rounds[len(n.Rounds)-1]
}

func (b *builder) handleMessage(ctx context.Context, ev *model.Event) {
	n := b.findNode(ev)
	if n == nil {
		return
	}
	var d struct {
		Role        model.Role       `json:"role"`
		Content     string           `json:"content"`
		ContentRef  json.RawMessage  `json:"contentRef"`
		ContentHash string           `json:"contentHash"`
		ToolCallID  string           `json:"toolCallID"`
		ToolCalls   []model.ToolCall `json:"toolCalls"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		b.nodeWarn(n, "seq=%d message data 解析失败: %v", ev.Seq, err)
		return
	}
	content := d.Content
	contentRef := ""
	if ref, ok := decodeRef(d.ContentRef); ok {
		contentRef = ref.Name
		if body := b.readArtifact(ref, "message", n); body != nil {
			content = string(body)
		}
	}
	msg := Message{
		At: ev.TS, Role: d.Role, Content: content, ContentRef: contentRef,
		ToolCallID: d.ToolCallID, ToolCalls: d.ToolCalls,
	}
	switch d.Role {
	case model.RoleTool:
		b.attachToolContent(d.ToolCallID, content, contentRef, n)
		b.conversation[n] = append(b.conversation[n], model.Message{
			Role: model.RoleTool, Content: content, ToolCallID: d.ToolCallID,
		})
	case model.RoleAssistant:
		round := b.currentRound(n)
		if round == nil {
			round = &Round{}
			n.Rounds = append(n.Rounds, round)
		}
		round.Assistant = &msg
		for _, call := range d.ToolCalls {
			tc := b.ensureToolCall(call.ID, call.Name)
			tc.Arguments = call.Arguments
			tc.Name = call.Name
			if !containsTool(round.ToolCalls, tc) {
				round.ToolCalls = append(round.ToolCalls, tc)
			}
		}
		b.conversation[n] = append(b.conversation[n], model.Message{
			Role: model.RoleAssistant, Content: content, ToolCalls: d.ToolCalls,
		})
	default:
		n.Messages = append(n.Messages, msg)
		b.conversation[n] = append(b.conversation[n], model.Message{
			Role: d.Role, Content: content,
		})
	}
}

func containsTool(list []*ToolCall, target *ToolCall) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func (b *builder) ensureToolCall(id, name string) *ToolCall {
	if tc := b.byTool[id]; tc != nil {
		if tc.Name == "" {
			tc.Name = name
		}
		return tc
	}
	tc := &ToolCall{ID: id, Name: name}
	b.byTool[id] = tc
	return tc
}

func (b *builder) handleToolCall(ev *model.Event) {
	var d struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Approval  string          `json:"approval"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	tc := b.ensureToolCall(d.ID, d.Name)
	tc.Name = d.Name
	tc.Arguments = d.Arguments
	tc.Approval = d.Approval
	if n := b.findNode(ev); n != nil {
		if round := b.currentRound(n); round != nil && !containsTool(round.ToolCalls, tc) {
			round.ToolCalls = append(round.ToolCalls, tc)
		}
	}
}

func (b *builder) handleToolResult(ev *model.Event) {
	var d struct {
		ID         string     `json:"id"`
		Name       string     `json:"name"`
		Status     string     `json:"status"`
		OK         *bool      `json:"ok"`
		ExitCode   int        `json:"exitCode"`
		DurationMs int64      `json:"durationMs"`
		Truncated  bool       `json:"truncated"`
		OutputRef  string     `json:"outputRef"`
		Error      *ToolError `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	tc := b.ensureToolCall(d.ID, d.Name)
	if d.Name != "" {
		tc.Name = d.Name
	}
	tc.Status = d.Status
	tc.OK = d.Status == "ok"
	if d.OK != nil {
		tc.OK = *d.OK
	}
	tc.ExitCode = d.ExitCode
	tc.Duration = time.Duration(d.DurationMs) * time.Millisecond
	tc.Truncated = d.Truncated
	tc.OutputRef = d.OutputRef
	tc.Error = d.Error
	if n := b.findNode(ev); n != nil {
		if round := b.currentRound(n); round != nil && !containsTool(round.ToolCalls, tc) {
			round.ToolCalls = append(round.ToolCalls, tc)
		}
	}
}

func (b *builder) attachToolContent(callID, content, contentRef string, n *Node) {
	if callID == "" {
		return
	}
	tc := b.ensureToolCall(callID, "")
	tc.Content = content
	tc.ContentRef = contentRef
	var env struct {
		OK        bool       `json:"ok"`
		Truncated bool       `json:"truncated"`
		OutputRef string     `json:"outputRef"`
		Error     *ToolError `json:"error"`
	}
	if json.Unmarshal([]byte(content), &env) == nil {
		if env.Truncated {
			tc.Truncated = true
		}
		if env.OutputRef != "" {
			tc.OutputRef = env.OutputRef
			ref := model.ArtifactRef{Name: env.OutputRef}
			if body := b.readArtifact(ref, "tool-output", n); body != nil {
				tc.FullOutput = string(body)
				tc.FullOutputRef = env.OutputRef
			}
		}
		if env.Error != nil {
			tc.Error = env.Error
		}
	}
}

func (b *builder) handleStepCompleted(ctx context.Context, ev *model.Event) {
	n := b.findNode(ev)
	if n == nil {
		return
	}
	var d struct {
		Status    string          `json:"status"`
		OutputRef json.RawMessage `json:"outputRef"`
		Usage     model.Usage     `json:"usage"`
		ToolCalls int             `json:"toolCalls"`
		Changed   []string        `json:"changed"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	out := &NodeOutput{Status: d.Status, Usage: d.Usage, ToolCalls: d.ToolCalls, Changed: d.Changed}
	if ref, ok := decodeRef(d.OutputRef); ok {
		out.Ref = ref.Name
		if body := b.readArtifact(ref, "node-output", n); body != nil {
			var payload struct {
				Text       string          `json:"text"`
				Structured json.RawMessage `json:"structured"`
				Usage      model.Usage     `json:"usage"`
				ToolCalls  int             `json:"toolCalls"`
				Changed    []string        `json:"changed"`
				Checks     []model.Check   `json:"checks"`
			}
			if json.Unmarshal(body, &payload) == nil {
				out.Text = payload.Text
				out.Structured = payload.Structured
				out.Usage = payload.Usage
				out.ToolCalls = payload.ToolCalls
				out.Changed = payload.Changed
				out.Checks = payload.Checks
			}
		}
	}
	n.Output = out
}

func (b *builder) handleNodeCompleted(ev *model.Event) {
	n := b.findNode(ev)
	var d struct {
		Status     string          `json:"status"`
		DurationMs int64           `json:"durationMs"`
		Usage      model.Usage     `json:"usage"`
		Verdict    json.RawMessage `json:"verdict"`
		Checks     []model.Check   `json:"checks"`
		Changed    []string        `json:"changed"`
		Dimension  string          `json:"dimension"`
		Error      string          `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	if ev.Run == "" && ev.Node != "" && d.Dimension != "" {
		n = b.nodeByDimension(ev.Node, d.Dimension)
	} else if n == nil && ev.Node != "" {
		n = b.nodeByDimension(ev.Node, d.Dimension)
	}
	if n == nil {
		b.warn("seq=%d node_completed 找不到所属节点", ev.Seq)
		return
	}
	n.Completed = &NodeCompleted{
		At: ev.TS, Status: d.Status, Duration: time.Duration(d.DurationMs) * time.Millisecond,
		Usage: d.Usage, Verdict: d.Verdict, Checks: d.Checks,
		Changed: d.Changed, Dimension: d.Dimension, Error: d.Error,
	}
}

func (b *builder) nodeByDimension(node model.NodeName, dimension string) *Node {
	if b.turn == nil {
		return nil
	}
	for i := len(b.turn.Nodes) - 1; i >= 0; i-- {
		n := b.turn.Nodes[i]
		if n.Name != node {
			continue
		}
		if dimension == "" || (n.Input != nil && n.Input.Dimension == dimension) {
			return n
		}
	}
	list := b.byNode[string(node)]
	if len(list) > 0 {
		return list[len(list)-1]
	}
	return nil
}

func (b *builder) handleCompacted(ctx context.Context, ev *model.Event, turn *Turn) {
	var d struct {
		BeforeTokens  int             `json:"beforeTokens"`
		AfterTokens   int             `json:"afterTokens"`
		SummaryRef    json.RawMessage `json:"summaryRef"`
		ReplacedRange struct {
			Removed int `json:"removed"`
			Kept    int `json:"kept"`
		} `json:"replacedRange"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	c := &Compaction{
		At: ev.TS, BeforeTokens: d.BeforeTokens, AfterTokens: d.AfterTokens,
		Removed: d.ReplacedRange.Removed, Kept: d.ReplacedRange.Kept,
	}
	if ref, ok := decodeRef(d.SummaryRef); ok {
		c.SummaryRef = ref.Name
		if body := b.readArtifact(ref, "context-summary", nil); body != nil {
			var payload struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal(body, &payload) == nil {
				c.Summary = payload.Summary
			}
		}
	}
	turn.Compactions = append(turn.Compactions, c)
	if n := b.findNode(ev); n != nil {
		n.Compactions = append(n.Compactions, c)
	}
}

func (b *builder) handleApprovalRequested(ev *model.Event, turn *Turn) {
	var d struct {
		Class   string   `json:"class"`
		Node    string   `json:"node"`
		Action  string   `json:"action"`
		Command string   `json:"command"`
		Reason  string   `json:"reason"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	command := d.Command
	if command == "" && len(d.Paths) > 0 {
		command = strings.Join(d.Paths, ", ")
	}
	turn.Approvals = append(turn.Approvals, &Approval{
		At: ev.TS, Class: d.Class, Node: d.Node, Action: d.Action,
		Command: command, Reason: d.Reason,
	})
}

func (b *builder) handleApprovalResolved(ev *model.Event, turn *Turn) {
	var d struct {
		Class    string `json:"class"`
		Decision string `json:"decision"`
		Scope    string `json:"scope"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	turn.Approvals = append(turn.Approvals, &Approval{
		At: ev.TS, Class: d.Class, Decision: d.Decision, Scope: d.Scope, Reason: d.Reason,
	})
}

func (b *builder) handleModeChanged(ev *model.Event, turn *Turn) {
	var d struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Reason    string `json:"reason"`
		Confirmed bool   `json:"confirmedByUser"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	turn.ModeChanges = append(turn.ModeChanges, &ModeChange{
		At: ev.TS, From: d.From, To: d.To, Reason: d.Reason, Confirmed: d.Confirmed,
	})
}

func (b *builder) handleTurnCompleted(ev *model.Event, turn *Turn) {
	var d struct {
		Status   string      `json:"status"`
		ExitCode int         `json:"exitCode"`
		Usage    model.Usage `json:"usage"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	turn.CompletedAt = ev.TS
	turn.Status = d.Status
	turn.ExitCode = d.ExitCode
	if !d.Usage.IsZero() {
		turn.Usage = d.Usage
	}
}

func (b *builder) handleSessionEvent(ev *model.Event, turn *Turn, status string) {
	var d struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	if turn.Status == "" {
		turn.Status = status
	}
	if d.Reason != "" {
		turn.Error = d.Reason
	}
}

func (b *builder) finish() {
	for _, t := range b.doc.Turns {
		if t.Status == "" {
			if t.CompletedAt.IsZero() {
				t.Status = "incomplete"
			} else {
				t.Status = "ok"
			}
		}
		for _, n := range t.Nodes {
			if n.Completed == nil && n.Output != nil {
				status := n.Output.Status
				if status == "" {
					status = "ok"
				}
				n.Completed = &NodeCompleted{Status: status, Usage: n.Output.Usage, Checks: n.Output.Checks, Changed: n.Output.Changed}
			}
		}
	}
}

func (b *builder) readArtifact(ref model.ArtifactRef, what string, n *Node) []byte {
	body, err := b.r.Artifact(ref)
	if err != nil {
		msg := fmt.Sprintf("读取 %s artifact %q 失败: %v", what, ref.Name, err)
		if n != nil {
			b.nodeWarn(n, "%s", msg)
		} else {
			b.warn("%s", msg)
		}
		return nil
	}
	return body
}

func decodeRef(raw json.RawMessage) (model.ArtifactRef, bool) {
	if len(raw) == 0 || string(raw) == "null" {
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
