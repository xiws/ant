package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/logx"
	"github.com/xiws/ant/internal/model"
)

const (
	// runStartTimeout 限制 POST 等待 session 打开/取锁的时间；turn 本身不在此超时内。
	runStartTimeout = 30 * time.Second
	// promptTimeout 防止浏览器关闭或断线后审批/提问永久挂起。
	promptTimeout = 5 * time.Minute
)

var errPromptTimeout = errors.New("等待用户响应超时")

// RunStartResult 是 POST 启动 turn 后同步返回给浏览器的身份信息。
type RunStartResult struct {
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	Status    string `json:"status"`
}

type sseMessage struct {
	Event string
	Data  any
}

type activeRun struct {
	id        string
	sessionID string
	request   RunRequest
	cancel    context.CancelFunc
	started   chan struct{}
	startOnce sync.Once
	startErr  error
	done      chan struct{}
	startedAt time.Time
}

type promptReply struct {
	choice approval.Choice
	answer approval.Answer
	err    error
}

type pendingPrompt struct {
	id        string
	sessionID string
	runID     string
	kind      string
	payload   any
	reply     chan promptReply
}

// runManager 管理活动 turn、SSE 订阅和审批/ask 队列。
type runManager struct {
	backend Executor
	clock   func() time.Time
	logger  logx.Logger

	rootCtx context.Context
	cancel  context.CancelFunc

	mu            sync.Mutex
	closed        bool
	runWG         sync.WaitGroup
	runsByID      map[string]*activeRun
	runsBySession map[string]*activeRun
	prompts       map[string]*pendingPrompt
	subs          map[string]map[chan sseMessage]struct{}
}

func newRunManager(backend Executor, clock func() time.Time, logger logx.Logger) *runManager {
	if clock == nil {
		clock = time.Now
	}
	if logger == nil {
		logger = logx.Nop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &runManager{
		backend:       backend,
		clock:         clock,
		logger:        logger,
		rootCtx:       ctx,
		cancel:        cancel,
		runsByID:      map[string]*activeRun{},
		runsBySession: map[string]*activeRun{},
		prompts:       map[string]*pendingPrompt{},
		subs:          map[string]map[chan sseMessage]struct{}{},
	}
}

// Start 启动一次执行。新建 session 时会等待执行器创建 session 并返回 ID；
// 续跑时会等待 session 锁成功获取，因此锁冲突可以直接作为 HTTP 错误返回。
func (m *runManager) Start(ctx context.Context, req RunRequest) (*RunStartResult, error) {
	runCtx, cancel := context.WithCancel(m.rootCtx)
	run := &activeRun{
		id:        newRunID("run"),
		sessionID: req.SessionID,
		request:   req,
		cancel:    cancel,
		started:   make(chan struct{}),
		done:      make(chan struct{}),
		startedAt: m.clock().UTC(),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, unavailable("server_shutting_down", "本地服务正在关闭，无法开始新的执行")
	}
	if req.SessionID != "" {
		if active := m.runsBySession[req.SessionID]; active != nil {
			m.mu.Unlock()
			cancel()
			return nil, conflict("run_active", "该 session 已有正在执行的 turn，请等待完成或先停止")
		}
		m.runsBySession[req.SessionID] = run
	}
	m.runsByID[run.id] = run
	m.runWG.Add(1)
	m.mu.Unlock()

	go m.execute(runCtx, run)

	select {
	case <-run.started:
		if run.startErr != nil {
			cancel()
			return nil, run.startErr
		}
		return &RunStartResult{SessionID: run.sessionID, RunID: run.id, Status: "running"}, nil
	case <-time.After(runStartTimeout):
		cancel()
		return nil, unavailable("run_start_timeout", "启动执行超时；请检查配置与 session 锁")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func (m *runManager) execute(ctx context.Context, run *activeRun) {
	defer m.runWG.Done()
	defer m.finish(run)
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("执行器 panic：%v", recovered)
			run.startOnce.Do(func() {
				run.startErr = err
				close(run.started)
			})
			m.logger.Error("web executor panic", "run", run.id, "panic", recovered)
			m.broadcast(run.sessionID, "run_error", map[string]any{
				"runId": run.id, "sessionId": run.sessionID, "message": err.Error(),
			})
		}
	}()

	hooks := RunHooks{
		Prompter: &webPrompter{manager: m, run: run},
		OnSessionID: func(id string) {
			m.bindSession(run, id)
		},
		OnEvent: func(ev *model.Event) {
			m.onEvent(run, ev)
		},
		OnDelta: func(text, reasoning string) {
			m.onDelta(run, text, reasoning)
		},
	}

	outcome, err := m.backend.Run(ctx, run.request, hooks)
	if err != nil {
		run.startOnce.Do(func() {
			run.startErr = err
			close(run.started)
		})
		m.broadcast(run.sessionID, "run_error", map[string]any{
			"runId":     run.id,
			"sessionId": run.sessionID,
			"message":   err.Error(),
			"code":      model.CodeOf(err),
			"class":     string(model.ClassOf(err)),
		})
		return
	}

	run.startOnce.Do(func() { close(run.started) })
	status := "ok"
	if ctx.Err() != nil {
		status = "interrupted"
	} else if outcome.ExitCode != 0 {
		status = "failed"
	}
	m.broadcast(run.sessionID, "run_done", map[string]any{
		"runId":     run.id,
		"sessionId": run.sessionID,
		"status":    status,
		"exitCode":  outcome.ExitCode,
	})
}

// bindSession 把新建 session 的 ID 绑定到 run；若同 session 已有别的活动 run，
// 则拒绝并取消当前执行，避免两路写同一 events.jsonl。
func (m *runManager) bindSession(run *activeRun, id string) {
	if id == "" {
		return
	}
	var conflictErr error
	m.mu.Lock()
	if m.closed {
		conflictErr = unavailable("server_shutting_down", "本地服务正在关闭")
	} else if existing := m.runsBySession[id]; existing != nil && existing != run {
		conflictErr = conflict("run_active", "该 session 已有正在执行的 turn")
	} else {
		run.sessionID = id
		m.runsBySession[id] = run
	}
	m.mu.Unlock()

	if conflictErr != nil {
		run.startOnce.Do(func() {
			run.startErr = conflictErr
			close(run.started)
		})
		run.cancel()
		return
	}
	run.startOnce.Do(func() { close(run.started) })
	m.broadcast(id, "status", m.statusPayload(run, true))
}

func (m *runManager) finish(run *activeRun) {
	run.cancel()
	m.mu.Lock()
	if m.runsByID[run.id] == run {
		delete(m.runsByID, run.id)
	}
	if run.sessionID != "" && m.runsBySession[run.sessionID] == run {
		delete(m.runsBySession, run.sessionID)
	}
	m.mu.Unlock()
	m.broadcast(run.sessionID, "status", map[string]any{
		"runId":     run.id,
		"sessionId": run.sessionID,
		"running":   false,
		"status":    "idle",
	})
	close(run.done)
}

func (m *runManager) statusPayload(run *activeRun, running bool) map[string]any {
	return map[string]any{
		"runId":     run.id,
		"sessionId": run.sessionID,
		"running":   running,
		"startedAt": run.startedAt.Format(time.RFC3339),
	}
}

func (m *runManager) onEvent(run *activeRun, ev *model.Event) {
	if ev == nil {
		return
	}
	m.broadcast(run.sessionID, "event", map[string]any{
		"runId": run.id,
		"seq":   ev.Seq,
		"type":  ev.Type,
		"turn":  ev.Turn,
		"node":  ev.Node,
		"ts":    ev.TS,
	})
}

func (m *runManager) onDelta(run *activeRun, text, reasoning string) {
	if text == "" && reasoning == "" {
		return
	}
	m.broadcast(run.sessionID, "delta", map[string]any{
		"runId":     run.id,
		"sessionId": run.sessionID,
		"text":      text,
		"reasoning": reasoning,
	})
}

// Cancel 取消 session 当前活动 turn；返回是否找到活动 run。
func (m *runManager) Cancel(sessionID string) bool {
	m.mu.Lock()
	run := m.runsBySession[sessionID]
	if run != nil {
		run.cancel()
	}
	m.mu.Unlock()
	if run == nil {
		return false
	}
	m.broadcast(sessionID, "status", map[string]any{
		"runId":     run.id,
		"sessionId": sessionID,
		"running":   true,
		"stopping":  true,
	})
	return true
}

// ActiveStatus 返回 session 当前是否正在执行，供 SSE 重连时恢复状态。
func (m *runManager) ActiveStatus(sessionID string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run := m.runsBySession[sessionID]; run != nil {
		return m.statusPayload(run, true)
	}
	return map[string]any{"sessionId": sessionID, "running": false}
}

// Subscribe 注册一个 SSE 连接，并在注册瞬间补发当前运行状态与待响应 prompt。
func (m *runManager) Subscribe(sessionID string) (<-chan sseMessage, func()) {
	ch := make(chan sseMessage, 256)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	if m.subs[sessionID] == nil {
		m.subs[sessionID] = map[chan sseMessage]struct{}{}
	}
	m.subs[sessionID][ch] = struct{}{}
	if run := m.runsBySession[sessionID]; run != nil {
		ch <- sseMessage{Event: "status", Data: m.statusPayload(run, true)}
	} else {
		ch <- sseMessage{Event: "status", Data: map[string]any{"sessionId": sessionID, "running": false}}
	}
	for _, prompt := range m.prompts {
		if prompt.sessionID == sessionID {
			ch <- sseMessage{Event: prompt.kind, Data: prompt.payload}
		}
	}
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		if set := m.subs[sessionID]; set != nil {
			if _, ok := set[ch]; ok {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(m.subs, sessionID)
			}
		}
		m.mu.Unlock()
	}
	return ch, cancel
}

func (m *runManager) broadcast(sessionID, event string, data any) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs[sessionID] {
		select {
		case ch <- sseMessage{Event: event, Data: data}:
		default:
			// 慢客户端不能阻塞执行；最终状态仍可通过 changed + 权威投影补齐。
			m.logger.Warn("SSE 客户端过慢，丢弃一条实时消息", "session", sessionID, "event", event)
		}
	}
}

func (m *runManager) requestPrompt(ctx context.Context, run *activeRun, kind string, payload map[string]any) (promptReply, error) {
	id := newRunID("prompt")
	envelope := make(map[string]any, len(payload)+4)
	for key, value := range payload {
		envelope[key] = value
	}
	envelope["id"] = id
	envelope["kind"] = kind
	envelope["runId"] = run.id
	envelope["sessionId"] = run.sessionID

	prompt := &pendingPrompt{
		id:        id,
		sessionID: run.sessionID,
		runID:     run.id,
		kind:      kind,
		payload:   envelope,
		reply:     make(chan promptReply, 1),
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return promptReply{}, unavailable("server_shutting_down", "本地服务正在关闭")
	}
	m.prompts[id] = prompt
	m.mu.Unlock()
	defer m.finishPrompt(id)
	m.broadcast(run.sessionID, kind, envelope)

	timer := time.NewTimer(promptTimeout)
	defer timer.Stop()
	select {
	case reply := <-prompt.reply:
		return reply, reply.err
	case <-ctx.Done():
		return promptReply{}, ctx.Err()
	case <-timer.C:
		return promptReply{}, errPromptTimeout
	}
}

func (m *runManager) finishPrompt(promptID string) {
	m.mu.Lock()
	delete(m.prompts, promptID)
	m.mu.Unlock()
}

func (m *runManager) Approve(promptID, decision string) error {
	var choice approval.Choice
	switch decision {
	case "allow_once", "once", "allow":
		choice = approval.ChoiceOnce
	case "allow_session", "session":
		choice = approval.ChoiceAll
	case "deny":
		choice = approval.ChoiceDeny
	default:
		return badRequest("approval_decision_invalid", "审批决定必须是 allow_once、allow_session 或 deny")
	}

	m.mu.Lock()
	prompt := m.prompts[promptID]
	if prompt == nil || prompt.kind != "approval" {
		m.mu.Unlock()
		return notFound("approval_not_found", "审批请求不存在、已处理或已过期")
	}
	delete(m.prompts, promptID)
	m.mu.Unlock()

	select {
	case prompt.reply <- promptReply{choice: choice}:
		return nil
	default:
		return conflict("approval_already_resolved", "审批请求已被处理")
	}
}

func (m *runManager) Answer(promptID, selectedID, text string) error {
	m.mu.Lock()
	prompt := m.prompts[promptID]
	if prompt == nil || prompt.kind != "ask" {
		m.mu.Unlock()
		return notFound("ask_not_found", "提问不存在、已处理或已过期")
	}
	delete(m.prompts, promptID)
	m.mu.Unlock()

	select {
	case prompt.reply <- promptReply{answer: approval.Answer{SelectedID: selectedID, Text: text}}:
		return nil
	default:
		return conflict("ask_already_resolved", "提问已被处理")
	}
}

func (m *runManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	runs := make([]*activeRun, 0, len(m.runsByID))
	for _, run := range m.runsByID {
		runs = append(runs, run)
	}
	m.mu.Unlock()
	m.cancel()
	for _, run := range runs {
		run.cancel()
	}
	done := make(chan struct{})
	go func() {
		m.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		m.logger.Warn("等待活动 turn 退出超时")
	}
}

// webPrompter 把引擎的审批/提问转成 SSE 消息并等待浏览器 POST 回答。
type webPrompter struct {
	manager *runManager
	run     *activeRun
}

func (p *webPrompter) Confirm(ctx context.Context, cp approval.ConfirmPrompt) (approval.Choice, error) {
	reply, err := p.manager.requestPrompt(ctx, p.run, "approval", map[string]any{
		"class":  cp.Class,
		"title":  cp.Title,
		"detail": cp.Detail,
		"cwd":    cp.CWD,
		"reason": cp.Reason,
	})
	if errors.Is(err, errPromptTimeout) {
		return approval.ChoiceDeny, nil
	}
	if err != nil {
		return approval.ChoiceDeny, err
	}
	return reply.choice, nil
}

func (p *webPrompter) Ask(ctx context.Context, q approval.Question) (approval.Answer, error) {
	options := make([]map[string]string, 0, len(q.Options))
	for _, option := range q.Options {
		options = append(options, map[string]string{
			"id":          option.ID,
			"label":       option.Label,
			"description": option.Description,
		})
	}
	reply, err := p.manager.requestPrompt(ctx, p.run, "ask", map[string]any{
		"question":      q.Question,
		"options":       options,
		"allowFreeText": q.AllowFreeText,
	})
	if err != nil {
		return approval.Answer{}, err
	}
	return reply.answer, nil
}

func newRunID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
