package web

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

const (
	// inlineTextLimit 是对话流内联文本的上限；更大的内容只给 artifact 引用。
	inlineTextLimit = 64 << 10
	// defaultTurnLimit 是一次详情请求默认加载的 turn 数（§22.4.4 分页）。
	defaultTurnLimit = 50
)

// ViewOptions 是详情投影的分页参数。
type ViewOptions struct {
	BeforeTurn int
	Limit      int
}

// Projector 把 events.jsonl 投影为前端可直接渲染的视图。
// 它只读 Archive，不写任何文件。
type Projector struct {
	archive session.Archive
	cfg     config.View
	workDir string
	homeDir string
	clock   func() time.Time

	mu         sync.Mutex
	cache      map[string]*SessionView
	cacheOrder []string
}

// NewProjector 构造投影器。
func NewProjector(archive session.Archive, cfg config.View, workDir, homeDir string, clock func() time.Time) *Projector {
	if clock == nil {
		clock = time.Now
	}
	return &Projector{
		archive: archive,
		cfg:     cfg,
		workDir: workDir,
		homeDir: homeDir,
		clock:   clock,
		cache:   map[string]*SessionView{},
	}
}

// List 返回当前 workspace 的 session 列表。
func (p *Projector) List(ctx context.Context, f session.ListFilter, q string) ([]SessionSummary, error) {
	filter := f
	needle := strings.ToLower(strings.TrimSpace(q))
	if needle != "" {
		// 先取全量再按 q 过滤，避免 limit 在过滤前截断匹配项。
		filter.Limit = 0
	}
	list, err := p.archive.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	locks := map[string]session.LockState{}
	if reporter, ok := p.archive.(session.LockReporter); ok {
		if got, lerr := reporter.Locks(ctx, ids); lerr == nil {
			locks = got
		}
	}
	out := make([]SessionSummary, 0, len(list))
	for _, s := range list {
		if needle != "" && !matchesQuery(s, needle) {
			continue
		}
		lock := locks[s.ID]
		summary := SessionSummary{
			ID:        s.ID,
			Title:     s.Title,
			Mode:      s.Mode,
			Status:    s.Status,
			Live:      lock.Live,
			Stale:     lock.Present && !lock.Live,
			Model:     s.Model,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
			TurnCount: s.TurnCount,
			Tokens:    s.Tokens,
			LastEvent: s.LastEvent,
		}
		if summary.Status == session.StatusRunning && !lock.Live {
			// running 但心跳过期：显示为 stale，避免假“运行中”。
			summary.Stale = true
		}
		out = append(out, summary)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func matchesQuery(s session.Summary, needle string) bool {
	for _, field := range []string{s.Title, s.ID, s.Model, string(s.Mode)} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// Version 返回用于 ETag/缓存的版本串（events 大小/mtime + meta mtime）。
func (p *Projector) Version(id string) string {
	dir := p.sessionDir(id)
	evInfo, evErr := statFile(filepath.Join(dir, "events.jsonl"))
	metaInfo, metaErr := statFile(filepath.Join(dir, "meta.json"))
	if evErr != nil && metaErr != nil {
		return ""
	}
	if evErr != nil {
		return fmt.Sprintf("0-0-%d", metaInfo.modUnixNano)
	}
	artifactMod := int64(0)
	if artInfo, artErr := statFile(filepath.Join(dir, "artifacts")); artErr == nil {
		artifactMod = artInfo.modUnixNano
	}
	if metaErr != nil {
		return fmt.Sprintf("%d-%d-0-%d", evInfo.size, evInfo.modUnixNano, artifactMod)
	}
	return fmt.Sprintf("%d-%d-%d-%d", evInfo.size, evInfo.modUnixNano, metaInfo.modUnixNano, artifactMod)
}

type fileStat struct {
	size        int64
	modUnixNano int64
}

func statFile(path string) (fileStat, error) {
	info, err := osStat(path)
	if err != nil {
		return fileStat{}, err
	}
	return fileStat{size: info.Size(), modUnixNano: info.ModTime().UnixNano()}, nil
}

// View 返回一个 session 的完整投影；version 用于 ETag。
func (p *Projector) View(ctx context.Context, id string, opt ViewOptions) (*SessionView, string, error) {
	version := p.Version(id)
	key := id + "|" + version
	if version != "" {
		if cached := p.cacheGet(key); cached != nil {
			out := *cached
			p.applyLimits(&out, opt)
			return &out, version, nil
		}
	}
	snapshot, err := p.archive.Snapshot(ctx, id)
	if err != nil {
		return nil, "", err
	}
	view := p.project(ctx, snapshot)
	p.populateArtifacts(view)
	view.Plan = p.readPlan(id, view)
	view.Readonly = true
	if version != "" {
		p.cachePut(key, view)
	}
	out := *view
	p.applyLimits(&out, opt)
	return &out, version, nil
}

func (p *Projector) cacheGet(key string) *SessionView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache[key]
}

func (p *Projector) cachePut(key string, view *SessionView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.cache[key]; exists {
		return
	}
	p.cache[key] = view
	p.cacheOrder = append(p.cacheOrder, key)
	for len(p.cacheOrder) > 32 {
		oldest := p.cacheOrder[0]
		p.cacheOrder = p.cacheOrder[1:]
		delete(p.cache, oldest)
	}
}

func (p *Projector) sessionDir(id string) string {
	return sessionDir(p.workDir, id)
}

func sessionDir(workDir, id string) string {
	return filepath.Join(workDir, ".ant", "sessions", id)
}

// project 是核心投影状态机（§22.5.5）。
func (p *Projector) project(ctx context.Context, snapshot session.Snapshot) *SessionView {
	b := &builder{
		p:             p,
		id:            snapshot.Meta.ID,
		turns:         map[int]*TurnView{},
		tools:         map[string]*ToolView{},
		lastNode:      map[int]string{},
		computedUsage: map[int]model.Usage{},
		warnSeen:      map[string]struct{}{},
		view: &SessionView{
			ID:       snapshot.Meta.ID,
			Meta:     snapshot.Meta,
			Lock:     snapshot.Lock,
			Warnings: []string{},
			Turns:    []TurnView{},
		},
	}
	for _, warning := range snapshot.Warnings {
		b.warn(warning)
	}
	for _, ev := range snapshot.Events {
		b.event(ctx, ev)
	}
	b.finish()
	return b.view
}

type builder struct {
	p     *Projector
	id    string
	view  *SessionView
	turns map[int]*TurnView
	init  *TurnView
	tools map[string]*ToolView

	active           *TurnView
	activeNum        int
	lastNode         map[int]string
	computedUsage    map[int]model.Usage
	pendingApprovals []*ApprovalView
	unknown          []TrajRow
	warnSeen         map[string]struct{}
	lastTS           time.Time
}

func (b *builder) warn(message string) {
	if message == "" {
		return
	}
	if _, exists := b.warnSeen[message]; exists {
		return
	}
	b.warnSeen[message] = struct{}{}
	b.view.Warnings = append(b.view.Warnings, message)
}

func (b *builder) ensureTurn(turn int) *TurnView {
	if turn <= 0 {
		if b.init == nil {
			b.init = &TurnView{
				Turn:     0,
				Pseudo:   true,
				Status:   "init",
				Input:    "会话初始化",
				Items:    []ItemView{},
				Requests: []RequestView{},
			}
		}
		return b.init
	}
	if t, ok := b.turns[turn]; ok {
		return t
	}
	t := &TurnView{
		Turn:      turn,
		EventTurn: turn,
		Status:    "incomplete",
		Items:     []ItemView{},
		Requests:  []RequestView{},
	}
	b.turns[turn] = t
	return t
}

// currentView 返回事件应归属的 turn：同一 turn 编号内的事件始终跟随当前活动轮次，
// 这样重复出现的 turn_started 不会把后续事件塞回已结束的旧轮次。
func (b *builder) currentView(evTurn int) *TurnView {
	if evTurn <= 0 {
		return b.ensureTurn(0)
	}
	if b.active != nil && b.activeNum == evTurn {
		return b.active
	}
	view := b.ensureTurn(evTurn)
	b.active = view
	b.activeNum = evTurn
	return view
}

// newSyntheticTurn 为复用旧编号的新轮次分配一个独立编号（显示时按时间重排）。
func (b *builder) newSyntheticTurn(eventTurn int) *TurnView {
	next := 1
	for number := range b.turns {
		if number >= next {
			next = number + 1
		}
	}
	view := &TurnView{
		Turn:      next,
		EventTurn: eventTurn,
		Status:    "running",
		Items:     []ItemView{},
		Requests:  []RequestView{},
	}
	b.turns[next] = view
	return view
}

func (b *builder) nodeFor(turn int) string {
	if name := b.lastNode[turn]; name != "" {
		return name
	}
	return ""
}

func (b *builder) appendItem(turn *TurnView, item ItemView) {
	item.TS = item.TS.UTC()
	if item.TS.IsZero() {
		item.TS = b.lastTS
	}
	turn.Items = append(turn.Items, item)
}

func (b *builder) event(ctx context.Context, ev model.Event) {
	if !ev.TS.IsZero() {
		b.lastTS = ev.TS
	}
	turn := b.currentView(ev.Turn)
	turn.LastSeq = ev.Seq
	if turn.StartSeq == 0 {
		turn.StartSeq = ev.Seq
	}
	if ev.Turn > 0 && (turn.StartedAt == nil) && ev.Type != model.EventTurnCompleted {
		ts := ev.TS
		turn.StartedAt = &ts
	}

	switch ev.Type {
	case model.EventSessionCreated:
		// 头部信息来自 meta.json，这里只补事件里可能更新的字段。
		var data struct {
			Mode  model.Mode `json:"mode"`
			Model string     `json:"model"`
		}
		if decodeEvent(ev, &data) == nil {
			if b.view.Meta.Mode == "" && data.Mode.Valid() {
				b.view.Meta.Mode = data.Mode
			}
			if b.view.Meta.Model == "" {
				b.view.Meta.Model = data.Model
			}
		}

	case model.EventModeChanged:
		var data struct {
			From            model.Mode `json:"from"`
			To              model.Mode `json:"to"`
			Reason          string     `json:"reason"`
			ConfirmedByUser bool       `json:"confirmedByUser"`
		}
		if decodeEvent(ev, &data) == nil {
			text := fmt.Sprintf("模式切换 %s → %s", data.From, data.To)
			if data.Reason != "" {
				text += "（" + data.Reason + "）"
			}
			b.appendItem(turn, ItemView{Seq: ev.Seq, Kind: "notice", Notice: text})
			if data.To.Valid() {
				b.view.Meta.Mode = data.To
				turn.Mode = data.To
			}
		}

	case model.EventTurnStarted:
		var data struct {
			Input     string     `json:"input"`
			InputHash string     `json:"inputHash"`
			Mode      model.Mode `json:"mode"`
			Round     int        `json:"round"`
			Turn      int        `json:"turn"`
			Resumed   bool       `json:"resumed"`
		}
		_ = decodeEvent(ev, &data)
		turnNum := ev.Turn
		if turnNum <= 0 {
			turnNum = data.Turn
		}
		target := b.ensureTurn(turnNum)
		if turnNum > 0 && (target.EndedAt != nil || isTerminalTurnStatus(target.Status)) {
			// 真实日志存在同一 turn 编号被后续进程重复使用的情况（如 1,1,2,3,1）。
			// 复用编号会让后续事件写回旧轮次；这里按新轮次渲染。
			b.warn(fmt.Sprintf("turn_started 复用了已结束的第 %d 轮（seq=%d），已作为新轮次展示", turnNum, ev.Seq))
			target = b.newSyntheticTurn(turnNum)
		}
		b.active = target
		b.activeNum = turnNum
		turn = target
		turn.LastSeq = ev.Seq
		if turn.EventTurn == 0 {
			turn.EventTurn = turnNum
		}
		if data.Input != "" {
			if !data.Resumed || turn.Input == "" {
				b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "user", Text: data.Input})
			}
			turn.Input = data.Input
		}
		if data.Mode.Valid() {
			turn.Mode = data.Mode
		}
		turn.Round = data.Round
		turn.Status = "running"
		if turn.StartedAt == nil {
			ts := ev.TS
			turn.StartedAt = &ts
		}

	case model.EventNodeStarted:
		var data struct {
			Node          model.NodeName     `json:"node"`
			Model         string             `json:"model"`
			Provider      string             `json:"provider"`
			PromptVersion string             `json:"promptVersion"`
			PromptHash    string             `json:"promptHash"`
			InputRef      *model.ArtifactRef `json:"inputRef"`
			Stream        bool               `json:"stream"`
		}
		_ = decodeEvent(ev, &data)
		node := data.Node
		if node == "" {
			node = ev.Node
		}
		turn.Nodes = append(turn.Nodes, NodeView{
			Seq:           ev.Seq,
			Node:          node,
			Step:          ev.Step,
			Model:         data.Model,
			Provider:      data.Provider,
			PromptVersion: data.PromptVersion,
			PromptHash:    data.PromptHash,
			InputRef:      normalizeRef(data.InputRef),
			StartedAt:     ev.TS,
		})
		b.lastNode[turn.Turn] = string(node)
		if node == "" {
			b.lastNode[turn.Turn] = string(ev.Node)
		}

	case model.EventModelRequest:
		var data struct {
			Model                string   `json:"model"`
			Provider             string   `json:"provider"`
			MessageCount         int      `json:"messageCount"`
			EstimatedInputTokens int      `json:"estimatedInputTokens"`
			Tools                []string `json:"tools"`
			Stream               bool     `json:"stream"`
			Attempt              int      `json:"attempt"`
		}
		_ = decodeEvent(ev, &data)
		attempt := data.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		node := ev.Node
		if node == "" {
			node = model.NodeName(b.nodeFor(turn.Turn))
		}
		req := RequestView{
			Seq:                  ev.Seq,
			Turn:                 turn.Turn,
			Node:                 node,
			Step:                 ev.Step,
			Model:                data.Model,
			Provider:             data.Provider,
			Attempt:              attempt,
			Status:               "pending",
			StartedAt:            ev.TS,
			MessageCount:         data.MessageCount,
			EstimatedInputTokens: data.EstimatedInputTokens,
			Tools:                data.Tools,
			Stream:               data.Stream,
			RequestData:          decodeMap(ev.Data),
			InputRef:             b.nodeInputRef(turn, node),
		}
		turn.Requests = append(turn.Requests, req)

	case model.EventModelResponse:
		var data struct {
			FinishReason  string           `json:"finishReason"`
			Usage         *model.Usage     `json:"usage"`
			TextHash      string           `json:"textHash"`
			ToolCallCount int              `json:"toolCallCount"`
			DurationMs    int64            `json:"durationMs"`
			Attempt       int              `json:"attempt"`
			Model         string           `json:"model"`
			ErrorClass    model.ErrorClass `json:"errorClass"`
			Code          string           `json:"code"`
			Message       string           `json:"message"`
		}
		_ = decodeEvent(ev, &data)
		node := ev.Node
		pseudo := false
		if node == "" {
			node = "provider"
			pseudo = true
		}
		attempt := data.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		idx := b.findRequestIndex(turn, node, attempt)
		if idx < 0 {
			// 没有对应的 model_request（旧日志/提供方回退）：补一条记录，不丢事件。
			req := RequestView{
				Seq:       ev.Seq,
				Turn:      turn.Turn,
				Node:      node,
				Step:      ev.Step,
				Model:     data.Model,
				Attempt:   attempt,
				Status:    "pending",
				StartedAt: ev.TS,
				Pseudo:    pseudo,
			}
			if prev := b.latestRequest(turn, node); prev != nil && prev.Node == node {
				req.MessageCount = prev.MessageCount
				req.EstimatedInputTokens = prev.EstimatedInputTokens
				req.Tools = append([]string(nil), prev.Tools...)
				req.InputRef = prev.InputRef
			}
			turn.Requests = append(turn.Requests, req)
			idx = len(turn.Requests) - 1
		}
		req := &turn.Requests[idx]
		if data.DurationMs > 0 {
			req.DurationMs = data.DurationMs
		}
		ended := ev.TS
		req.EndedAt = &ended
		if req.DurationMs == 0 && !req.StartedAt.IsZero() {
			req.DurationMs = ended.Sub(req.StartedAt).Milliseconds()
		}
		req.FinishReason = data.FinishReason
		req.TextHash = data.TextHash
		req.ToolCallCount = data.ToolCallCount
		if data.Model != "" {
			req.Model = data.Model
		}
		if data.Usage != nil {
			u := *data.Usage
			req.Usage = &u
			b.addUsage(turn.Turn, u)
		}
		req.ResponseData = decodeMap(ev.Data)
		if data.ErrorClass != "" || data.Code != "" || data.Message != "" {
			req.Status = "failed"
			req.Error = &ErrorView{
				Class:   string(data.ErrorClass),
				Code:    data.Code,
				Message: data.Message,
				Step:    ev.Step,
			}
		} else {
			req.Status = "ok"
		}

	case model.EventModelFallback:
		var data struct {
			From    string `json:"from"`
			To      string `json:"to"`
			Reason  string `json:"reason"`
			Attempt int    `json:"attempt"`
		}
		_ = decodeEvent(ev, &data)
		node := ev.Node
		if node == "" {
			node = model.NodeName(b.nodeFor(turn.Turn))
		}
		if req := b.latestRequest(turn, node); req != nil {
			req.Fallbacks = append(req.Fallbacks, FallbackView{
				From: data.From, To: data.To, Reason: data.Reason, Attempt: data.Attempt,
			})
		}

	case model.EventMessage:
		var data struct {
			Role        model.Role         `json:"role"`
			ContentHash string             `json:"contentHash"`
			ContentRef  *model.ArtifactRef `json:"contentRef"`
			ToolCallID  string             `json:"toolCallID"`
			ToolCalls   []model.ToolCall   `json:"toolCalls"`
		}
		_ = decodeEvent(ev, &data)
		switch data.Role {
		case model.RoleAssistant:
			ref := normalizeRef(data.ContentRef)
			text, ok, cut := "", true, false
			if ref != nil {
				text, ok, cut = b.readText(ctx, *ref)
			}
			if strings.TrimSpace(text) == "" && len(data.ToolCalls) > 0 {
				return
			}
			if strings.TrimSpace(text) == "" && ref == nil {
				return
			}
			b.appendItem(turn, ItemView{
				Seq: ev.Seq, TS: ev.TS, Kind: "assistant", Text: text,
				Ref: ref, RefMissing: ref != nil && !ok, RefCut: cut,
			})
		case model.RoleTool:
			tool := b.tools[data.ToolCallID]
			if tool == nil {
				return
			}
			ref := normalizeRef(data.ContentRef)
			if ref == nil {
				return
			}
			body, ok, _ := b.readText(ctx, *ref)
			if !ok {
				tool.ResultMissing = true
				return
			}
			tool.Result = decodeToolBody(body)
			b.enrichResult(tool)
		}

	case model.EventToolCall:
		var data struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Approval  string          `json:"approval"`
		}
		_ = decodeEvent(ev, &data)
		node := ev.Node
		if node == "" {
			node = model.NodeName(b.nodeFor(turn.Turn))
		}
		tool := &ToolView{
			ID:       data.ID,
			Seq:      ev.Seq,
			CallTS:   ev.TS,
			Name:     data.Name,
			Node:     string(node),
			Step:     ev.Step,
			Approval: data.Approval,
			Args:     decodeArguments(data.Arguments),
			Status:   "pending",
		}
		if data.ID == "" {
			data.ID = fmt.Sprintf("call-%d", ev.Seq)
			tool.ID = data.ID
			b.warn(fmt.Sprintf("tool_call seq=%d 缺少 id，已合成 %s", ev.Seq, data.ID))
		}
		if prev, exists := b.tools[data.ID]; exists {
			b.warn(fmt.Sprintf("tool_call id=%s 重复（seq=%d），以最后一次为准", data.ID, ev.Seq))
			tool = prev
			tool.Seq = ev.Seq
			tool.CallTS = ev.TS
			tool.Args = decodeArguments(data.Arguments)
			tool.Name = data.Name
		}
		b.tools[tool.ID] = tool
		b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "tool", Tool: tool})
		if req := b.latestRequest(turn, node); req != nil && tool.Name != "" {
			req.ToolsUsed = appendUnique(req.ToolsUsed, tool.Name)
		}

	case model.EventToolResult:
		var data struct {
			ID         string          `json:"id"`
			Status     string          `json:"status"`
			OK         *bool           `json:"ok"`
			ExitCode   *int            `json:"exitCode"`
			DurationMs int64           `json:"durationMs"`
			Truncated  bool            `json:"truncated"`
			OutputRef  string          `json:"outputRef"`
			Error      json.RawMessage `json:"error"`
		}
		_ = decodeEvent(ev, &data)
		tool := b.tools[data.ID]
		if tool == nil {
			// 缺 tool_call 的结果：仍保留为工具卡，避免丢事件。
			tool = &ToolView{ID: data.ID, Seq: ev.Seq, CallTS: ev.TS, Name: "unknown", Status: "pending"}
			if data.ID == "" {
				tool.ID = fmt.Sprintf("result-%d", ev.Seq)
				b.warn(fmt.Sprintf("tool_result seq=%d 缺少 id，无法配对", ev.Seq))
			}
			b.tools[tool.ID] = tool
			b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "tool", Tool: tool})
		}
		if tool.ResultSeq != 0 {
			b.warn(fmt.Sprintf("tool_result id=%s 重复（seq=%d），以最后一次为准", tool.ID, ev.Seq))
		}
		tool.ResultSeq = ev.Seq
		tool.DurationMs = data.DurationMs
		tool.Truncated = data.Truncated
		tool.OutputRef = data.OutputRef
		tool.ExitCode = data.ExitCode
		tool.OK = data.OK
		if data.Status == "" {
			if data.OK != nil && *data.OK {
				tool.Status = "ok"
			} else {
				tool.Status = "error"
			}
		} else {
			tool.Status = data.Status
		}
		if len(data.Error) > 0 {
			var wire struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Hint    string         `json:"hint"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal(data.Error, &wire); err == nil {
				tool.Error = &ErrorView{Code: wire.Code, Message: wire.Message, Hint: wire.Hint}
				if wire.Code == "approval_denied" {
					tool.Status = "denied"
				}
			} else {
				tool.Error = &ErrorView{Message: string(data.Error)}
			}
		}
		if tool.Status == "ok" {
			tool.OK = boolPtr(true)
		}
		ended := ev.TS
		tool.EndTS = &ended

	case model.EventApprovalRequested:
		var data struct {
			Class   model.ApprovalClass `json:"class"`
			Node    model.NodeName      `json:"node"`
			Action  string              `json:"action"`
			Command string              `json:"command"`
			Paths   []string            `json:"paths"`
			CWD     string              `json:"cwd"`
			Reason  string              `json:"reason"`
		}
		_ = decodeEvent(ev, &data)
		approval := &ApprovalView{
			Seq:      ev.Seq,
			TS:       ev.TS,
			Class:    data.Class,
			Node:     data.Node,
			Action:   data.Action,
			Command:  data.Command,
			Paths:    data.Paths,
			CWD:      data.CWD,
			Reason:   data.Reason,
			Decision: "pending",
		}
		b.pendingApprovals = append(b.pendingApprovals, approval)
		b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "approval", Approval: approval})

	case model.EventApprovalResolved:
		var data struct {
			Class    model.ApprovalClass `json:"class"`
			Decision string              `json:"decision"`
			Scope    string              `json:"scope"`
			Source   string              `json:"source"`
			Reason   string              `json:"reason"`
		}
		_ = decodeEvent(ev, &data)
		var target *ApprovalView
		for i := len(b.pendingApprovals) - 1; i >= 0; i-- {
			candidate := b.pendingApprovals[i]
			if candidate.Decision != "pending" {
				continue
			}
			if data.Class != "" && candidate.Class != data.Class {
				continue
			}
			if data.Class == "" && ev.Node != "" && candidate.Node != "" && candidate.Node != ev.Node {
				continue
			}
			target = candidate
			break
		}
		if target == nil {
			target = &ApprovalView{Seq: ev.Seq, TS: ev.TS, Class: data.Class, Node: ev.Node, Decision: "pending"}
			b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "approval", Approval: target})
		}
		target.Decision = data.Decision
		if target.Decision == "" {
			target.Decision = "unknown"
		}
		target.Scope = data.Scope
		target.Source = data.Source
		if data.Reason != "" {
			target.Reason = data.Reason
		}
		ts := ev.TS
		target.ResolvedTS = &ts

	case model.EventContextCompacted:
		var data struct {
			BeforeTokens  int                `json:"beforeTokens"`
			AfterTokens   int                `json:"afterTokens"`
			SummaryRef    *model.ArtifactRef `json:"summaryRef"`
			ReplacedRange struct {
				Removed int `json:"removed"`
				Kept    int `json:"kept"`
			} `json:"replacedRange"`
		}
		_ = decodeEvent(ev, &data)
		compaction := &CompactionView{
			BeforeTokens: data.BeforeTokens,
			AfterTokens:  data.AfterTokens,
			Removed:      data.ReplacedRange.Removed,
			Kept:         data.ReplacedRange.Kept,
			SummaryRef:   normalizeRef(data.SummaryRef),
		}
		if compaction.SummaryRef != nil {
			text, ok, _ := b.readText(ctx, *compaction.SummaryRef)
			compaction.Summary = text
			compaction.Missing = !ok
		}
		b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "compaction", Compaction: compaction})

	case model.EventStepCompleted:
		var data struct {
			Node    model.NodeName `json:"node"`
			Status  string         `json:"status"`
			Changed []string       `json:"changed"`
			Checks  []model.Check  `json:"checks"`
			Usage   *model.Usage   `json:"usage"`
		}
		_ = decodeEvent(ev, &data)
		turn.Changed = appendUnique(turn.Changed, data.Changed...)
		if data.Usage != nil {
			b.addUsage(turn.Turn, *data.Usage)
		}

	case model.EventNodeCompleted:
		var data struct {
			Node       model.NodeName `json:"node"`
			Status     string         `json:"status"`
			DurationMs int64          `json:"durationMs"`
			Usage      *model.Usage   `json:"usage"`
		}
		_ = decodeEvent(ev, &data)
		node := data.Node
		if node == "" {
			node = ev.Node
		}
		idx := -1
		for i := len(turn.Nodes) - 1; i >= 0; i-- {
			if turn.Nodes[i].Node == node && turn.Nodes[i].EndedAt == nil {
				idx = i
				break
			}
		}
		if idx < 0 {
			turn.Nodes = append(turn.Nodes, NodeView{
				Seq:       ev.Seq,
				Node:      node,
				Step:      ev.Step,
				StartedAt: ev.TS,
			})
			idx = len(turn.Nodes) - 1
		}
		ended := ev.TS
		turn.Nodes[idx].EndedAt = &ended
		turn.Nodes[idx].Status = data.Status
		if data.DurationMs > 0 {
			turn.Nodes[idx].DurationMs = data.DurationMs
		} else if !turn.Nodes[idx].StartedAt.IsZero() {
			turn.Nodes[idx].DurationMs = ended.Sub(turn.Nodes[idx].StartedAt).Milliseconds()
		}
		if data.Usage != nil {
			u := *data.Usage
			turn.Nodes[idx].Usage = &u
		}

	case model.EventTurnCompleted:
		var data struct {
			Status   string        `json:"status"`
			ExitCode int           `json:"exitCode"`
			Usage    *model.Usage  `json:"usage"`
			Changed  []string      `json:"changed"`
			Checks   []model.Check `json:"checks"`
			PlanPath string        `json:"planPath"`
			Summary  string        `json:"summary"`
		}
		_ = decodeEvent(ev, &data)
		if data.Status != "" {
			turn.Status = data.Status
		} else {
			turn.Status = "ok"
		}
		exitCode := data.ExitCode
		turn.ExitCode = &exitCode
		if data.Usage != nil {
			u := *data.Usage
			turn.Usage = &u
		}
		turn.Changed = appendUnique(turn.Changed, data.Changed...)
		turn.Checks = data.Checks
		turn.PlanPath = data.PlanPath
		turn.Summary = data.Summary
		ended := ev.TS
		turn.EndedAt = &ended
		if turn.StartedAt != nil {
			turn.DurationMs = ended.Sub(*turn.StartedAt).Milliseconds()
		}

	case model.EventSessionInterrupted:
		var data struct {
			Reason      string `json:"reason"`
			CurrentStep string `json:"currentStep"`
			Message     string `json:"message"`
		}
		_ = decodeEvent(ev, &data)
		if turn.Status != "ok" {
			turn.Status = "interrupted"
		}
		message := data.Message
		if message == "" {
			message = data.Reason
		}
		b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "error", Error: &ErrorView{
			Class:   string(model.ErrInterrupted),
			Message: message,
			Step:    data.CurrentStep,
			Reason:  data.Reason,
		}})

	case model.EventSessionFailed:
		var data struct {
			Reason      string           `json:"reason"`
			ErrorClass  model.ErrorClass `json:"errorClass"`
			CurrentStep string           `json:"currentStep"`
			Message     string           `json:"message"`
		}
		_ = decodeEvent(ev, &data)
		turn.Status = "failed"
		b.appendItem(turn, ItemView{Seq: ev.Seq, TS: ev.TS, Kind: "error", Error: &ErrorView{
			Class:   string(data.ErrorClass),
			Message: data.Message,
			Step:    data.CurrentStep,
			Reason:  data.Reason,
		}})

	case model.EventUsage:
		var usage model.Usage
		if decodeEvent(ev, &usage) == nil {
			b.addUsage(turn.Turn, usage)
		}

	default:
		if !model.KnownEventType(ev.Type) {
			b.warn(fmt.Sprintf("未知事件类型 %q（seq=%d），已在轨迹中保留原始 JSON", ev.Type, ev.Seq))
			b.unknown = append(b.unknown, TrajRow{
				ID:     fmt.Sprintf("x%d", ev.Seq),
				Turn:   ev.Turn,
				Kind:   "unknown",
				Seq:    ev.Seq,
				Label:  "未知事件 " + string(ev.Type),
				Status: "unknown",
				Raw:    ev.Data,
			})
		}
	}
}

func (b *builder) addUsage(turn int, usage model.Usage) {
	if usage.IsZero() {
		return
	}
	current := b.computedUsage[turn]
	b.computedUsage[turn] = current.Add(usage)
}

func (b *builder) findRequestIndex(turn *TurnView, node model.NodeName, attempt int) int {
	for i := len(turn.Requests) - 1; i >= 0; i-- {
		req := &turn.Requests[i]
		if req.Node == node && req.Attempt == attempt {
			return i
		}
	}
	return -1
}

func (b *builder) latestRequest(turn *TurnView, node model.NodeName) *RequestView {
	for i := len(turn.Requests) - 1; i >= 0; i-- {
		if turn.Requests[i].Node == node {
			return &turn.Requests[i]
		}
	}
	for i := len(turn.Requests) - 1; i >= 0; i-- {
		return &turn.Requests[i]
	}
	return nil
}

func (b *builder) nodeInputRef(turn *TurnView, node model.NodeName) *model.ArtifactRef {
	for i := len(turn.Nodes) - 1; i >= 0; i-- {
		if turn.Nodes[i].Node == node && turn.Nodes[i].InputRef != nil {
			return turn.Nodes[i].InputRef
		}
	}
	return nil
}

func (b *builder) readText(ctx context.Context, ref model.ArtifactRef) (string, bool, bool) {
	if ref.Bytes > session.MaxArchiveArtifactBytes {
		return "", false, true
	}
	body, err := b.p.archive.Artifact(ctx, b.id, ref)
	if err != nil {
		return "", false, false
	}
	if int64(len(body)) > inlineTextLimit {
		return string(body[:inlineTextLimit]), true, true
	}
	return string(body), true, false
}

// finish 关闭未完成的 turn、汇总 usage、补全工具卡并生成轨迹。
func (b *builder) finish() {
	turns := make([]*TurnView, 0, len(b.turns))
	for _, turn := range b.turns {
		turns = append(turns, turn)
	}
	// 按首次事件时间排序后连续重编号，避免复用编号导致的 UI 顺序错乱。
	sort.SliceStable(turns, func(i, j int) bool {
		ti, tj := turns[i].StartedAt, turns[j].StartedAt
		if ti != nil && tj != nil && !ti.Equal(*tj) {
			return ti.Before(*tj)
		}
		if ti != nil && tj == nil {
			return true
		}
		if ti == nil && tj != nil {
			return false
		}
		return turns[i].EventTurn < turns[j].EventTurn
	})
	for index, turn := range turns {
		turn.EventTurn = firstNonZero(turn.EventTurn, turn.Turn)
		turn.Turn = index + 1
		for reqIndex := range turn.Requests {
			turn.Requests[reqIndex].Turn = turn.Turn
		}
	}

	live := b.view.Lock.Live
	hasInit := b.init != nil && (len(b.init.Requests) > 0 || len(b.init.Nodes) > 0 || len(b.init.Items) > 0)
	if hasInit {
		for idx := range b.init.Items {
			if b.init.Items[idx].Tool != nil {
				b.enrichTool(b.init.Items[idx].Tool, live)
			}
		}
	}
	for i, turn := range turns {
		isLast := i == len(turns)-1
		if turn.Status == "" || turn.Status == "running" || turn.Status == "incomplete" {
			switch {
			case b.view.Meta.Status == session.StatusRunning && live && isLast:
				turn.Status = "running"
			default:
				turn.Status = "interrupted"
			}
		}
		if turn.StartedAt == nil && !turn.Pseudo {
			ts := b.view.Meta.CreatedAt
			turn.StartedAt = &ts
		}
		if turn.EndedAt == nil {
			ts := b.view.Meta.UpdatedAt
			if !b.lastTS.IsZero() {
				ts = b.lastTS
			}
			turn.EndedAt = &ts
		}
		if turn.DurationMs == 0 && turn.StartedAt != nil && turn.EndedAt != nil {
			turn.DurationMs = turn.EndedAt.Sub(*turn.StartedAt).Milliseconds()
		}
		if turn.Usage == nil && !turn.Pseudo {
			if usage, ok := b.computedUsage[turn.Turn]; ok && !usage.IsZero() {
				u := usage
				turn.Usage = &u
			}
		}
		for idx := range turn.Items {
			item := &turn.Items[idx]
			if item.Tool != nil {
				b.enrichTool(item.Tool, live)
			}
		}
		for idx := range turn.Nodes {
			if turn.Nodes[idx].DurationMs == 0 && turn.Nodes[idx].EndedAt != nil {
				turn.Nodes[idx].DurationMs = turn.Nodes[idx].EndedAt.Sub(turn.Nodes[idx].StartedAt).Milliseconds()
			}
		}
	}

	if hasInit {
		b.view.Turns = append(b.view.Turns, *b.init)
	}
	for _, turn := range turns {
		b.view.Turns = append(b.view.Turns, *turn)
	}
	if len(b.view.Turns) == 0 {
		b.view.Turns = []TurnView{}
	}

	// 审批与改动汇总。
	for _, turn := range b.view.Turns {
		for idx := range turn.Items {
			if turn.Items[idx].Approval != nil {
				b.view.Approvals = append(b.view.Approvals, *turn.Items[idx].Approval)
			}
		}
	}
	if b.view.Approvals == nil {
		b.view.Approvals = []ApprovalView{}
	}
	b.view.unknown = b.unknown
	b.view.Changes = b.computeChanges()
	b.view.Stats = b.computeStats()
	b.view.LastSeq = b.view.Meta.LastSeq
	b.view.Trajectory = buildTrajectory(b.view.Turns, b.unknown)
	if b.view.Artifacts == nil {
		b.view.Artifacts = []ArtifactView{}
	}
}

// enrichTool 在事件遍历结束后补全工具卡的 typed 视图（参数 + 结果）。
func (b *builder) enrichTool(tool *ToolView, live bool) {
	if tool.Status == "pending" {
		if live {
			tool.Status = "pending"
		} else {
			tool.Status = "stopped"
		}
	}
	if tool.Name == "" {
		tool.Name = "unknown"
	}
	switch model.ToolName(tool.Name) {
	case model.ToolRead:
		b.enrichRead(tool)
	case model.ToolReadCode:
		b.enrichReadCode(tool)
	case model.ToolSearch:
		b.enrichSearch(tool)
	case model.ToolBash:
		b.enrichBash(tool)
	case model.ToolEdit:
		b.enrichEdit(tool)
	case model.ToolWrite:
		b.enrichWrite(tool)
	case model.ToolAsk:
		b.enrichAsk(tool)
	}
}

// enrichResult 在 tool message 到达时解析结果 body（可能早于 tool_result）。
func (b *builder) enrichResult(tool *ToolView) {
	tool.ResultMissing = false
	if tool.Status == "pending" {
		tool.Status = "ok"
	}
}

func (b *builder) enrichRead(tool *ToolView) {
	view := &ReadView{
		Path:      argString(tool.Args, "path"),
		StartLine: argInt(tool.Args, "startLine"),
		EndLine:   argInt(tool.Args, "endLine"),
	}
	data := resultData(tool)
	view.Path = firstNonEmpty(strOf(data["path"]), view.Path)
	view.StartLine = firstNonZero(intOf(data["startLine"]), view.StartLine)
	view.EndLine = firstNonZero(intOf(data["endLine"]), view.EndLine)
	view.TotalLines = intOf(data["totalLines"])
	view.Content = strOf(data["content"])
	view.Truncated = boolOf(data["truncated"]) || tool.Truncated
	view.Missing = tool.ResultMissing || (tool.Status == "ok" && data == nil)
	tool.Read = view
	tool.Summary = fmt.Sprintf("%s · %d 行", view.Path, view.TotalLines)
	if view.TotalLines == 0 {
		tool.Summary = view.Path
	}
}

func (b *builder) enrichReadCode(tool *ToolView) {
	target := argString(tool.Args, "target")
	data := resultData(tool)
	target = firstNonEmpty(strOf(data["target"]), target)
	symbols := intOf(data["count"])
	content := firstNonEmpty(strOf(data["content"]), strOf(data["text"]))
	tool.Read = &ReadView{Path: target, Content: content, Missing: tool.ResultMissing}
	tool.Summary = fmt.Sprintf("%s · %d 个符号", target, symbols)
}

func (b *builder) enrichSearch(tool *ToolView) {
	view := &SearchView{
		Query: argString(tool.Args, "query"),
		Path:  argString(tool.Args, "path"),
		Regex: argBool(tool.Args, "regex"),
	}
	data := resultData(tool)
	view.Query = firstNonEmpty(strOf(data["query"]), view.Query)
	view.Path = firstNonEmpty(strOf(data["path"]), view.Path)
	view.MatchCount = intOf(data["matchCount"])
	view.ScannedFiles = intOf(data["scannedFiles"])
	view.Truncated = boolOf(data["truncated"])
	view.Results = stringSlice(data["results"])
	if view.MatchCount == 0 && len(view.Results) > 0 {
		view.MatchCount = len(view.Results)
	}
	view.Missing = tool.ResultMissing
	tool.Search = view
	tool.Summary = fmt.Sprintf("%s · %d 命中", view.Query, view.MatchCount)
	if view.Path != "" {
		tool.Summary += " · " + view.Path
	}
}

func (b *builder) enrichBash(tool *ToolView) {
	view := &BashView{
		Command:   argString(tool.Args, "command"),
		Cwd:       argString(tool.Args, "cwd"),
		TimeoutMs: argInt(tool.Args, "timeoutMs"),
	}
	data := resultData(tool)
	view.Command = firstNonEmpty(strOf(data["command"]), view.Command)
	view.Cwd = firstNonEmpty(strOf(data["cwd"]), view.Cwd)
	view.Stdout = strOf(data["stdout"])
	view.Stderr = strOf(data["stderr"])
	view.ExitCode = intOf(data["exitCode"])
	view.TimedOut = boolOf(data["timedOut"])
	view.OutputRef = firstNonEmpty(strOf(data["outputRef"]), tool.OutputRef)
	view.Missing = tool.ResultMissing
	if tool.OutputRef == "" {
		tool.OutputRef = view.OutputRef
	}
	tool.Bash = view
	tool.Summary = view.Command
}

func (b *builder) enrichEdit(tool *ToolView) {
	path := argString(tool.Args, "path")
	diffText := argString(tool.Args, "diff")
	data := resultData(tool)
	path = firstNonEmpty(strOf(data["path"]), path)
	diff := ParseUnifiedDiff(diffText, path)
	if data != nil {
		// 用工具结果的统计交叉校验；不一致时以 diff 解析为准并提示。
		added := intOf(data["added"])
		removed := intOf(data["removed"])
		if added != 0 || removed != 0 {
			if added != diff.Added || removed != diff.Removed {
				tool.Warnings = append(tool.Warnings, fmt.Sprintf(
					"diff 解析为 +%d −%d，工具报告 +%d −%d；以 diff 文本为准",
					diff.Added, diff.Removed, added, removed))
			}
		}
		if hunks := intOf(data["hunks"]); hunks > 0 && hunks != diff.HunkCount && diff.HunkCount > 0 {
			tool.Warnings = append(tool.Warnings, fmt.Sprintf(
				"diff 解析出 %d 个 hunk，工具报告 %d 个", diff.HunkCount, hunks))
		}
	}
	if diff.Warning != "" {
		tool.Warnings = append(tool.Warnings, diff.Warning)
	}
	tool.Diff = diff
	tool.Summary = fmt.Sprintf("%s %s", path, DiffSummary(diff))
}

func (b *builder) enrichWrite(tool *ToolView) {
	path := argString(tool.Args, "path")
	content := argString(tool.Args, "content")
	data := resultData(tool)
	view := &WriteView{Path: path, Content: content, Bytes: len(content), Lines: countLines(content)}
	view.Path = firstNonEmpty(strOf(data["path"]), view.Path)
	if n := intOf(data["bytes"]); n > 0 {
		view.Bytes = n
	}
	view.Created = boolOf(data["created"])
	view.Missing = tool.ResultMissing
	// 默认预览 40 行（§22.5.3）。
	const previewLines = 40
	lines := strings.Split(view.Content, "\n")
	if len(lines) > previewLines {
		view.Content = strings.Join(lines[:previewLines], "\n")
		view.PreviewTruncated = true
	}
	tool.Write = view
	state := "覆盖"
	if view.Created {
		state = "新建"
	}
	tool.Summary = fmt.Sprintf("%s · %s · %d 字节", view.Path, state, view.Bytes)
}

func (b *builder) enrichAsk(tool *ToolView) {
	view := &AskView{Question: argString(tool.Args, "question")}
	view.Options = optionLabels(tool.Args)
	data := resultData(tool)
	view.Selected = strOf(data["selectedId"])
	view.Answer = firstNonEmpty(strOf(data["text"]), strOf(data["answer"]))
	tool.Ask = view
	tool.Summary = view.Question
}

func (b *builder) computeChanges() []ChangedFile {
	index := map[string]int{}
	var out []ChangedFile
	merge := func(change ChangedFile) {
		if change.Path == "" {
			return
		}
		change.Path = strings.TrimPrefix(strings.ReplaceAll(change.Path, "\\", "/"), "./")
		if idx, ok := index[change.Path]; ok {
			existing := &out[idx]
			if existing.Added == 0 && existing.Removed == 0 {
				existing.Added = change.Added
				existing.Removed = change.Removed
			}
			if existing.Status == "" || existing.Status == "unknown" || change.Status == "created" {
				existing.Status = change.Status
			}
			if existing.Source == "turn_completed" {
				existing.Source = change.Source
			}
			return
		}
		index[change.Path] = len(out)
		out = append(out, change)
	}
	for _, turn := range b.view.Turns {
		for _, path := range turn.Changed {
			merge(ChangedFile{Path: path, Status: "modified", Source: "turn_completed", Turn: turn.Turn})
		}
		for idx := range turn.Items {
			tool := turn.Items[idx].Tool
			if tool == nil {
				continue
			}
			switch model.ToolName(tool.Name) {
			case model.ToolEdit:
				if tool.Diff == nil || tool.Status == "error" {
					continue
				}
				merge(ChangedFile{
					Path:   firstNonEmpty(tool.Diff.Path, argString(tool.Args, "path")),
					Status: "modified", Added: tool.Diff.Added, Removed: tool.Diff.Removed,
					Source: "edit", Turn: turn.Turn,
				})
			case model.ToolWrite:
				if tool.Write == nil || tool.Status == "error" {
					continue
				}
				status := "modified"
				if tool.Write.Created {
					status = "created"
				}
				added := tool.Write.Lines
				if added == 0 {
					added = countLines(tool.Write.Content)
				}
				merge(ChangedFile{
					Path:   firstNonEmpty(tool.Write.Path, argString(tool.Args, "path")),
					Status: status, Added: added, Source: "write", Turn: turn.Turn,
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []ChangedFile{}
	}
	return out
}

func (b *builder) computeStats() SessionStats {
	stats := SessionStats{Turns: 0}
	for _, turn := range b.view.Turns {
		if turn.Pseudo {
			continue
		}
		stats.Turns++
		stats.Requests += len(turn.Requests)
		for _, item := range turn.Items {
			if item.Tool != nil {
				stats.ToolCalls++
			}
		}
		if turn.Usage != nil {
			stats.Usage = stats.Usage.Add(*turn.Usage)
		}
		if turn.StartedAt != nil && (stats.StartedAt == nil || turn.StartedAt.Before(*stats.StartedAt)) {
			ts := *turn.StartedAt
			stats.StartedAt = &ts
		}
		if turn.EndedAt != nil && (stats.EndedAt == nil || turn.EndedAt.After(*stats.EndedAt)) {
			ts := *turn.EndedAt
			stats.EndedAt = &ts
		}
		stats.DurationMs += turn.DurationMs
	}
	if stats.Usage.IsZero() && !b.view.Meta.Usage.IsZero() {
		stats.Usage = b.view.Meta.Usage
	}
	return stats
}

// buildTrajectory 生成轨迹表行（§22.4.4）。
func buildTrajectory(turns []TurnView, unknown []TrajRow) []TrajRow {
	rows := make([]TrajRow, 0, 32)
	for _, turn := range turns {
		if turn.Pseudo {
			for i := range turn.Requests {
				req := &turn.Requests[i]
				reqID := "0." + itoa(i+1)
				rows = append(rows, requestRow(turn, req, reqID))
				rows = append(rows, toolRows(turn, req, reqID)...)
			}
			for i := range turn.Nodes {
				node := &turn.Nodes[i]
				if hasRequestForNode(turn, node.Node) {
					continue
				}
				rows = append(rows, nodeRow(turn, node, "0.n"+itoa(i+1)))
			}
			continue
		}
		userSeq := turn.StartSeq
		if userSeq == 0 {
			userSeq = firstSeq(turn.Items, "user")
		}
		rows = append(rows, TrajRow{
			ID:            itoa(turn.Turn),
			Turn:          turn.Turn,
			Kind:          "user",
			Seq:           userSeq,
			Label:         "用户",
			Input:         turn.Input,
			Output:        "—",
			Thinking:      "—",
			DurationLabel: "—",
			Status:        "ok",
		})
		reqIDs := map[int64]string{}
		for i := range turn.Requests {
			req := &turn.Requests[i]
			reqID := itoa(turn.Turn) + "." + itoa(i+1)
			reqIDs[req.Seq] = reqID
			rows = append(rows, requestRow(turn, req, reqID))
			rows = append(rows, toolRows(turn, req, reqID)...)
		}
		for i := range turn.Nodes {
			node := &turn.Nodes[i]
			if hasRequestForNode(turn, node.Node) {
				continue
			}
			rows = append(rows, nodeRow(turn, node, itoa(turn.Turn)+".n"+itoa(i+1)))
		}
		seen := map[string]bool{}
		for idx := range turn.Items {
			item := &turn.Items[idx]
			switch {
			case item.Approval != nil:
				if seen[approvalKey(item.Approval)] {
					continue
				}
				seen[approvalKey(item.Approval)] = true
				rows = append(rows, TrajRow{
					ID:            fmt.Sprintf("%d.a%d", turn.Turn, item.Approval.Seq),
					Turn:          turn.Turn,
					Kind:          "approval",
					Seq:           item.Approval.Seq,
					Label:         "审批 " + string(item.Approval.Class),
					Input:         firstNonEmpty(item.Approval.Command, strings.Join(item.Approval.Paths, ", ")),
					Output:        item.Approval.Decision,
					Thinking:      "—",
					DurationLabel: "—",
					Status:        item.Approval.Decision,
					Approval:      item.Approval,
				})
			case item.Compaction != nil:
				rows = append(rows, TrajRow{
					ID:            fmt.Sprintf("%d.c%d", turn.Turn, item.Seq),
					Turn:          turn.Turn,
					Kind:          "compaction",
					Seq:           item.Seq,
					Label:         "上下文压缩",
					Input:         fmt.Sprintf("%d tok", item.Compaction.BeforeTokens),
					Output:        fmt.Sprintf("%d tok", item.Compaction.AfterTokens),
					Thinking:      "—",
					DurationLabel: "—",
					Status:        "ok",
					Compaction:    item.Compaction,
				})
			case item.Kind == "notice":
				rows = append(rows, TrajRow{
					ID:            fmt.Sprintf("%d.s%d", turn.Turn, item.Seq),
					Turn:          turn.Turn,
					Kind:          "system",
					Seq:           item.Seq,
					Label:         "系统",
					Input:         item.Notice,
					Output:        "—",
					Thinking:      "—",
					DurationLabel: "—",
					Status:        "ok",
				})
			case item.Kind == "error" && item.Error != nil:
				rows = append(rows, TrajRow{
					ID:            fmt.Sprintf("%d.e%d", turn.Turn, item.Seq),
					Turn:          turn.Turn,
					Kind:          "error",
					Seq:           item.Seq,
					Label:         "错误 " + item.Error.Class,
					Input:         item.Error.Step,
					Output:        item.Error.Message,
					Thinking:      "—",
					DurationLabel: "—",
					Status:        "error",
					Error:         item.Error,
				})
			}
		}
		_ = reqIDs
		rows = append(rows, TrajRow{
			ID:            itoa(turn.Turn),
			Turn:          turn.Turn,
			Kind:          "turn",
			Seq:           turn.LastSeq,
			Label:         turnLabel(turn),
			Input:         "—",
			Output:        turnOutput(turn),
			Thinking:      "—",
			DurationLabel: durationLabel(turn.DurationMs),
			DurationMs:    turn.DurationMs,
			Status:        turn.Status,
		})
	}
	rows = append(rows, unknown...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Seq != rows[j].Seq {
			if rows[i].Seq == 0 {
				return false
			}
			if rows[j].Seq == 0 {
				return true
			}
			return rows[i].Seq < rows[j].Seq
		}
		if rows[i].Turn != rows[j].Turn {
			return rows[i].Turn < rows[j].Turn
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

func requestRow(turn TurnView, req *RequestView, id string) TrajRow {
	label := "请求 #" + itoa(requestIndex(turn, req)+1)
	input := "—"
	if req.MessageCount > 0 {
		input = fmt.Sprintf("%d 条消息", req.MessageCount)
		if req.EstimatedInputTokens > 0 {
			input += fmt.Sprintf(" / %s tok", humanTokens(req.EstimatedInputTokens))
		}
	}
	output := "—"
	if req.ToolCallCount > 0 {
		output = fmt.Sprintf("%d tool_calls", req.ToolCallCount)
	}
	if req.FinishReason != "" && req.FinishReason != "tool_calls" {
		if output == "—" {
			output = req.FinishReason
		} else {
			output += " / " + req.FinishReason
		}
	}
	if req.Status == "failed" {
		output = "失败"
	}
	tools := "—"
	if len(req.ToolsUsed) > 0 {
		tools = strings.Join(req.ToolsUsed, ", ")
	}
	return TrajRow{
		ID:            id,
		Turn:          req.Turn,
		Kind:          "request",
		Seq:           req.Seq,
		Label:         label,
		Input:         input,
		Output:        output,
		Thinking:      "未记录",
		DurationLabel: durationLabel(req.DurationMs),
		DurationMs:    req.DurationMs,
		Model:         req.Model,
		Tools:         tools,
		Status:        req.Status,
		Request:       req,
	}
}

func requestIndex(turn TurnView, req *RequestView) int {
	for i := range turn.Requests {
		if &turn.Requests[i] == req {
			return i
		}
	}
	return 0
}

func toolRows(turn TurnView, req *RequestView, reqID string) []TrajRow {
	rows := make([]TrajRow, 0, 2)
	n := 0
	// 工具属于「本 node 上一个请求之后、下一个同 node 请求之前」的调用。
	bound := int64(1 << 62)
	for i := range turn.Requests {
		next := &turn.Requests[i]
		if next.Node == req.Node && next.Seq > req.Seq && next.Seq < bound {
			bound = next.Seq
		}
	}
	for idx := range turn.Items {
		tool := turn.Items[idx].Tool
		if tool == nil {
			continue
		}
		if !nodeMatches(tool.Node, req.Node) {
			continue
		}
		if tool.Seq < req.Seq || tool.Seq >= bound {
			continue
		}
		n++
		input := compactJSON(tool.Args)
		output := tool.OutputSummary()
		rows = append(rows, TrajRow{
			ID:            reqID + "." + itoa(n),
			Turn:          turn.Turn,
			Kind:          "tool",
			Seq:           tool.Seq,
			Label:         "工具 " + tool.Name,
			Input:         input,
			Output:        output,
			Thinking:      "—",
			DurationLabel: durationLabel(tool.DurationMs),
			DurationMs:    tool.DurationMs,
			Status:        tool.Status,
			Tool:          tool,
		})
	}
	return rows
}

func nodeRow(turn TurnView, node *NodeView, id string) TrajRow {
	return TrajRow{
		ID:            id,
		Turn:          turn.Turn,
		Kind:          "node",
		Seq:           node.Seq,
		Label:         "节点 " + string(node.Node),
		Input:         node.Step,
		Output:        node.Status,
		Thinking:      "—",
		DurationLabel: durationLabel(node.DurationMs),
		DurationMs:    node.DurationMs,
		Model:         node.Model,
		Status:        node.Status,
	}
}

func hasRequestForNode(turn TurnView, node model.NodeName) bool {
	for i := range turn.Requests {
		if turn.Requests[i].Node == node {
			return true
		}
	}
	return false
}

func nodeMatches(toolNode string, reqNode model.NodeName) bool {
	return toolNode == "" || reqNode == "" || model.NodeName(toolNode) == reqNode
}

func approvalKey(approval *ApprovalView) string {
	return fmt.Sprintf("%d", approval.Seq)
}

func firstSeq(items []ItemView, kind string) int64 {
	for _, item := range items {
		if item.Kind == kind {
			return item.Seq
		}
	}
	return 0
}

// OutputSummary 返回工具行的一行结果摘要。
func (t *ToolView) OutputSummary() string {
	base := t.Status
	if t.OK != nil && *t.OK {
		base = "ok"
	}
	parts := []string{base}
	if t.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *t.ExitCode))
	}
	if t.DurationMs > 0 {
		parts = append(parts, durationLabel(t.DurationMs))
	}
	if t.Truncated {
		parts = append(parts, "已截断")
	}
	if t.Error != nil && t.Error.Code != "" {
		parts = append(parts, t.Error.Code)
	}
	return strings.Join(parts, " / ")
}

func turnLabel(turn TurnView) string {
	switch turn.Status {
	case "ok":
		return "本轮完成"
	case "failed":
		return "本轮失败"
	case "interrupted":
		return "本轮中断"
	}
	return "本轮未完成"
}

func turnOutput(turn TurnView) string {
	if turn.Usage == nil {
		return turn.Status
	}
	return fmt.Sprintf("%s / in %s out %s", turn.Status,
		humanTokens(turn.Usage.Input), humanTokens(turn.Usage.Output))
}

func durationLabel(ms int64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%.1fm", float64(ms)/60_000)
}

func humanTokens(n int) string {
	if n < 1000 {
		return itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// 以下是 data 解析辅助。

func decodeEvent(ev model.Event, dst any) error {
	if len(ev.Data) == 0 {
		return nil
	}
	return json.Unmarshal(ev.Data, dst)
}

func decodeMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func decodeArguments(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if !json.Valid([]byte(trimmed)) {
		return trimmed
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return trimmed
	}
	return value
}

// decodeToolBody 解析 tool 消息内容 `{"ok":true,"data":...,"truncated":...}`。
func decodeToolBody(body string) any {
	var wire struct {
		OK        bool            `json:"ok"`
		Data      json.RawMessage `json:"data"`
		Truncated bool            `json:"truncated"`
		OutputRef string          `json:"outputRef"`
		ExitCode  int             `json:"exitCode"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return nil
	}
	var out any
	if len(wire.Data) > 0 && string(wire.Data) != "null" {
		var value any
		if json.Unmarshal(wire.Data, &value) == nil {
			out = value
		}
	}
	if out == nil {
		return nil
	}
	return out
}

func resultData(tool *ToolView) map[string]any {
	if tool == nil || tool.Result == nil {
		return nil
	}
	if m, ok := tool.Result.(map[string]any); ok {
		return m
	}
	return nil
}

func argString(args any, key string) string {
	m, ok := args.(map[string]any)
	if !ok {
		return ""
	}
	return strOf(m[key])
}

func argInt(args any, key string) int {
	m, ok := args.(map[string]any)
	if !ok {
		return 0
	}
	return intOf(m[key])
}

func argBool(args any, key string) bool {
	m, ok := args.(map[string]any)
	if !ok {
		return false
	}
	return boolOf(m[key])
}

func optionLabels(args any) []string {
	m, ok := args.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["options"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		switch value := item.(type) {
		case string:
			out = append(out, value)
		case map[string]any:
			label := strOf(value["label"])
			if label == "" {
				label = strOf(value["id"])
			}
			out = append(out, label)
		}
	}
	return out
}

func strOf(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", value)
}

func intOf(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func boolOf(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	return false
}

func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, strOf(item))
	}
	return out
}

func normalizeRef(ref *model.ArtifactRef) *model.ArtifactRef {
	if ref == nil || ref.Name == "" {
		return nil
	}
	cp := *ref
	return &cp
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func boolPtr(v bool) *bool { return &v }

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, existing := range list {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			list = append(list, value)
		}
	}
	return list
}

func compactJSON(value any) string {
	if value == nil {
		return "—"
	}
	if s, ok := value.(string); ok {
		return s
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	text := string(raw)
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return text
}

// applyLimits 按 beforeTurn/limit 裁剪返回的 turn，并重建轨迹行。
// 它只修改传出的浅拷贝，不改变缓存的完整视图。
func (p *Projector) applyLimits(view *SessionView, opt ViewOptions) {
	if opt.Limit <= 0 {
		opt.Limit = defaultTurnLimit
	}
	all := view.Turns
	var init *TurnView
	if len(all) > 0 && all[0].Pseudo {
		first := all[0]
		init = &first
		all = all[1:]
	}
	candidates := all
	if opt.BeforeTurn > 0 {
		filtered := make([]TurnView, 0, len(all))
		for _, turn := range all {
			if turn.Turn < opt.BeforeTurn {
				filtered = append(filtered, turn)
			}
		}
		candidates = filtered
	}
	hasMore := false
	if len(candidates) > opt.Limit {
		candidates = candidates[len(candidates)-opt.Limit:]
		hasMore = true
	}
	if opt.BeforeTurn > 0 && len(candidates) > 0 && len(all) > 0 && candidates[0].Turn > all[0].Turn {
		hasMore = true
	}
	// 无裁剪时保持完整集合（包含 init）。
	out := make([]TurnView, 0, len(candidates)+1)
	if init != nil {
		out = append(out, *init)
	}
	out = append(out, candidates...)
	view.Turns = out
	view.HasMore = hasMore
	view.Truncated = hasMore
	if len(candidates) > 0 {
		view.FirstTurn = candidates[0].Turn
	} else {
		view.FirstTurn = 0
	}
	if len(candidates) > 0 {
		view.LastTurn = candidates[len(candidates)-1].Turn
	}
	view.Trajectory = buildTrajectory(view.Turns, view.unknown)
}

func isTerminalTurnStatus(status string) bool {
	switch status {
	case "ok", "failed", "interrupted":
		return true
	}
	return false
}
