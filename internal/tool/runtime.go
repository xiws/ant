package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

const (
	approvalAuto         = "auto"
	approvalNotRequested = "not_requested"
	statusOK             = "ok"
	statusError          = "error"
)

// runtimeImpl 是 Runtime 的生产实现：一批 tool_calls → 一批 tool 消息。
type runtimeImpl struct {
	cfg       config.View
	reg       Registry
	deps      Deps
	validator Validator
	repairer  Repairer
	truncator Truncator
	workDir   string
}

// NewRuntime 构造执行面。校验、修复、截断在运行期内装配，调用方只提供 Deps。
func NewRuntime(cfg config.View, reg Registry, deps Deps) Runtime {
	return &runtimeImpl{
		cfg:       cfg,
		reg:       reg,
		deps:      deps,
		validator: NewValidator(),
		repairer:  NewRepairer(),
		truncator: NewTruncator(cfg, deps.Store),
		workDir:   workDirOf(cfg),
	}
}

// callState 是一次调用在流水线各阶段的状态。
type callState struct {
	call     model.ToolCall
	node     model.NodeName
	tool     Tool
	spec     model.ToolSpec
	args     map[string]any
	approval string
	fail     *ToolError
}

// finishedResult 是流水线结束后的结果摘要（用于写 tool_result 事件）。
type finishedResult struct {
	ok   bool
	err  *ToolError
	meta ResultMeta
}

// messageBody 是 tool 消息 content 的 JSON 结构（§10.1）。
type messageBody struct {
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     *ToolError      `json:"error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	OutputRef string          `json:"outputRef,omitempty"`
}

// ExecuteBatch 实现 Runtime。
//
// 顺序：逐个调用做 查找→权限→修复→校验→审批（批量执行前完成），
// 然后统一写 tool_call 事件、按 Mutating 分并发/串行执行，
// 最后按原始 calls 顺序截断、写 tool_result 事件并回填消息。
func (r *runtimeImpl) ExecuteBatch(ctx context.Context, node model.NodeName, calls []model.ToolCall) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return []model.Message{}, nil
	}

	allowed := map[string]model.ToolSpec{}
	if r.reg != nil {
		for _, spec := range r.reg.Specs(node) {
			allowed[spec.Name] = spec
		}
	}

	states := make([]*callState, len(calls))
	for i := range calls {
		states[i] = &callState{call: calls[i], node: node, approval: approvalNotRequested}
	}
	// 1. 预处理（审批在批量执行前逐个处理）。
	for _, st := range states {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.prepare(ctx, node, st, allowed)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	// 2. 记录模型发起的调用（按原始顺序，确定性）。
	for _, st := range states {
		if err := r.emit(ctx, node, model.EventToolCall, toolCallEventData(st)); err != nil {
			return nil, err
		}
	}

	// 3. 执行：只读并发（上限 4），写操作串行且等待在途只读完成。
	results := make([]Result, len(states))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelReads)
	for i, st := range states {
		if st.fail != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			wg.Wait()
			return nil, err
		}
		if st.spec.Mutating {
			wg.Wait()
			results[i] = r.executeOne(ctx, st)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, st *callState) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.executeOne(ctx, st)
		}(i, st)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 4. 按原始 calls 顺序回填消息与 tool_result 事件。
	out := make([]model.Message, len(states))
	for i, st := range states {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg, fin := r.finish(ctx, st, results[i])
		out[i] = msg
		if err := r.emit(ctx, node, model.EventToolResult, toolResultEventData(st, fin)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prepare 执行查找 → 权限 → 修复 → 校验 → 审批。
func (r *runtimeImpl) prepare(ctx context.Context, node model.NodeName, st *callState, allowed map[string]model.ToolSpec) {
	// 协议上 id 由 provider 提供；缺失时仍保留调用，绝不吞掉消息。
	name := st.call.Name
	if r.reg == nil {
		st.fail = &ToolError{
			Code:    "tool_not_found",
			Message: fmt.Sprintf("未知工具 %q", name),
			Hint:    "只能调用工具列表中的工具",
			Details: map[string]any{"tool": name},
		}
		return
	}
	tool, ok := r.reg.Lookup(name)
	if !ok {
		st.fail = &ToolError{
			Code:    "tool_not_found",
			Message: fmt.Sprintf("未知工具 %q", name),
			Hint:    "只能调用工具列表中的工具；未注册的工具不可用",
			Details: map[string]any{"tool": name},
		}
		return
	}
	spec, ok := allowed[name]
	if !ok {
		st.fail = &ToolError{
			Code:    "tool_not_allowed",
			Message: fmt.Sprintf("节点 %s 无权调用工具 %q", node, name),
			Hint:    "该节点只能调用其工具列表中的工具",
			Details: map[string]any{"tool": name, "node": string(node)},
		}
		return
	}
	st.tool = tool
	st.spec = spec

	raw := bytes.TrimSpace(st.call.Arguments)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	repaired, _ := r.repairer.Repair(raw)
	args, verr := r.validator.Validate(spec, repaired)
	if verr != nil {
		st.fail = verr
		return
	}
	if args == nil {
		args = map[string]any{}
	}
	st.args = args

	class, needApproval := r.effectiveApproval(ctx, name, spec, args)
	if !needApproval || r.deps.Approver == nil {
		st.approval = approvalAuto
		return
	}
	dec, err := r.deps.Approver.Decide(ctx, approval.Request{
		Class:   class,
		Node:    node,
		Action:  actionFor(name, spec),
		Command: commandFromArgs(args),
		Paths:   pathsFromArgs(args),
		CWD:     r.workDir,
		Reason:  fmt.Sprintf("工具 %s 需要审批（类别 %s）", name, class),
	})
	if err != nil {
		st.approval = statusError
		st.fail = toolErrorFrom(err, nil)
		return
	}
	st.approval = dec.Reason
	if st.approval == "" {
		if dec.Allow {
			st.approval = "allow"
		} else {
			st.approval = "deny"
		}
	}
	if !dec.Allow {
		st.fail = &ToolError{
			Code:    "approval_denied",
			Message: fmt.Sprintf("用户拒绝了工具 %s 的调用", name),
			Hint:    "换一种不需要该权限的做法，或征得用户同意后重试",
			Details: map[string]any{
				"tool":   name,
				"class":  string(class),
				"reason": dec.Reason,
			},
		}
	}
}

// executeOne 在单次调用的超时上下文中执行工具，并把 panic/错误转成结构化结果。
func (r *runtimeImpl) executeOne(parent context.Context, st *callState) Result {
	execCtx := parent
	var cancel context.CancelFunc
	if st.spec.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(parent, st.spec.Timeout)
	} else {
		execCtx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	inv := Invocation{
		CallID:    st.call.ID,
		Node:      st.node,
		Args:      st.args,
		Raw:       st.call.Arguments,
		Workspace: r.workDir,
	}
	start := time.Now()
	res, err := invokeTool(execCtx, st.tool, st.spec.Name, inv)
	elapsed := time.Since(start)

	if err != nil {
		return Result{OK: false, Err: toolErrorFrom(err, execCtx), Meta: ResultMeta{Duration: elapsed}}
	}
	if res.Meta.Duration == 0 {
		res.Meta.Duration = elapsed
	}
	if res.Err != nil {
		return Result{OK: false, Err: res.Err, Meta: res.Meta}
	}
	if !res.OK {
		return Result{OK: false, Err: &ToolError{
			Code:    "internal",
			Message: "工具返回失败但没有提供错误信息",
			Details: map[string]any{"tool": st.spec.Name},
		}, Meta: res.Meta}
	}
	return res
}

// invokeTool 隔离工具执行体，任何 panic 都转成错误而不是进程崩溃。
func invokeTool(ctx context.Context, t Tool, name string, inv Invocation) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("工具 %s 执行时 panic: %v", name, p)
			res = Result{}
		}
	}()
	return t.Execute(ctx, inv)
}

// finish 把结果转成 tool 消息：成功 {ok:true,data}，失败 {ok:false,error}。
func (r *runtimeImpl) finish(ctx context.Context, st *callState, res Result) (model.Message, finishedResult) {
	msg := model.Message{Role: model.RoleTool, ToolCallID: st.call.ID}

	fail := st.fail
	if fail == nil && res.Err != nil {
		fail = res.Err
	}
	if fail == nil && !res.OK {
		fail = &ToolError{Code: "internal", Message: "工具执行失败"}
	}
	if fail != nil {
		msg.Content = encodeToolError(fail)
		return msg, finishedResult{ok: false, err: fail, meta: ResultMeta{Duration: res.Meta.Duration, ExitCode: res.Meta.ExitCode}}
	}

	data, err := marshalData(res.Data)
	if err != nil {
		te := &ToolError{Code: "internal", Message: "工具结果无法序列化为 JSON：" + err.Error()}
		msg.Content = encodeToolError(te)
		return msg, finishedResult{ok: false, err: te, meta: res.Meta}
	}
	kept, ref, truncated, cerr := r.truncator.Clip(ctx, st.call.ID, data)
	if cerr != nil {
		te := &ToolError{Code: "internal", Message: "工具输出截断失败：" + cerr.Error()}
		msg.Content = encodeToolError(te)
		return msg, finishedResult{ok: false, err: te, meta: res.Meta}
	}
	meta := res.Meta
	if !truncated {
		msg.Content = `{"ok":true,"data":` + string(kept) + `}`
		return msg, finishedResult{ok: true, meta: meta}
	}

	preview, perr := json.Marshal(map[string]any{"preview": string(kept)})
	if perr != nil {
		te := &ToolError{Code: "internal", Message: "截断预览无法序列化：" + perr.Error()}
		msg.Content = encodeToolError(te)
		return msg, finishedResult{ok: false, err: te, meta: res.Meta}
	}
	body := messageBody{OK: true, Data: preview, Truncated: true, OutputRef: ref.Name}
	encoded, merr := json.Marshal(body)
	if merr != nil {
		te := &ToolError{Code: "internal", Message: "截断结果无法序列化：" + merr.Error()}
		msg.Content = encodeToolError(te)
		return msg, finishedResult{ok: false, err: te, meta: res.Meta}
	}
	meta.Truncated = true
	meta.OutputRef = ref
	msg.Content = string(encoded)
	return msg, finishedResult{ok: true, meta: meta}
}

// emit 写一条事件；Sink 失败视为致命（§9.7）。
func (r *runtimeImpl) emit(ctx context.Context, node model.NodeName, typ model.EventType, data map[string]any) error {
	if r.deps.Sink == nil {
		return nil
	}
	ev := &model.Event{Type: typ, Node: node, Data: model.MustJSON(data)}
	if err := r.deps.Sink.Emit(ctx, ev); err != nil {
		return fmt.Errorf("tool: 写 %s 事件失败: %w", typ, err)
	}
	return nil
}

// toolErrorFrom 把任意错误转成结构化工具错误。
func toolErrorFrom(err error, execCtx context.Context) *ToolError {
	if err == nil {
		return &ToolError{Code: "internal", Message: "未知错误"}
	}
	var te *ToolError
	if errors.As(err, &te) && te != nil {
		return te
	}
	var me *model.Error
	if errors.As(err, &me) && me != nil {
		code := me.Code
		if code == "" {
			code = string(me.Class)
		}
		if code == "" {
			code = "internal"
		}
		return &ToolError{Code: code, Message: me.Message, Hint: me.Hint, Details: me.Details}
	}
	if execCtx != nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return &ToolError{
			Code:    "timeout",
			Message: "工具执行超时",
			Hint:    "缩小操作范围，或调大 limits.toolTimeoutMs / limits.bashTimeoutMs",
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ToolError{Code: "timeout", Message: "工具执行超时", Hint: "缩小操作范围或调大超时配置"}
	}
	if errors.Is(err, context.Canceled) {
		return &ToolError{Code: "interrupted", Message: "工具执行被取消"}
	}
	msg := err.Error()
	if strings.TrimSpace(msg) == "" {
		msg = "工具执行失败"
	}
	return &ToolError{Code: "internal", Message: msg}
}

// encodeToolError 生成失败消息 content。
func encodeToolError(te *ToolError) string {
	cp := &ToolError{Code: "internal", Message: "工具执行失败"}
	if te != nil {
		*cp = *te
	}
	if cp.Code == "" {
		cp.Code = "internal"
	}
	if cp.Message == "" {
		cp.Message = "工具执行失败"
	}
	body := messageBody{OK: false, Error: cp}
	encoded, err := json.Marshal(body)
	if err == nil {
		return string(encoded)
	}
	// details 里可能有不可序列化的值：退化为最小错误。
	body.Error = &ToolError{Code: cp.Code, Message: cp.Message}
	encoded, err = json.Marshal(body)
	if err == nil {
		return string(encoded)
	}
	return `{"ok":false,"error":{"code":"internal","message":"错误结果无法序列化"}}`
}

// marshalData 把工具数据编码为 JSON；nil 编码为 null。
func marshalData(data any) (json.RawMessage, error) {
	if data == nil {
		return json.RawMessage("null"), nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if !json.Valid(encoded) {
		return nil, errors.New("结果不是合法 JSON")
	}
	return encoded, nil
}

// toolCallEventData 按 §9.3 生成 tool_call 事件 data。
func toolCallEventData(st *callState) map[string]any {
	return map[string]any{
		"id":        st.call.ID,
		"name":      st.call.Name,
		"arguments": argumentsValue(st.call.Arguments),
		"approval":  st.approval,
	}
}

// toolResultEventData 按 §9.3 生成 tool_result 事件 data。
func toolResultEventData(st *callState, fin finishedResult) map[string]any {
	status := statusOK
	if !fin.ok {
		status = statusError
	}
	data := map[string]any{
		"id":         st.call.ID,
		"status":     status,
		"ok":         fin.ok,
		"exitCode":   fin.meta.ExitCode,
		"durationMs": fin.meta.Duration.Milliseconds(),
		"truncated":  fin.meta.Truncated,
	}
	if !fin.meta.OutputRef.IsZero() {
		data["outputRef"] = fin.meta.OutputRef.Name
	}
	if fin.err != nil {
		data["error"] = fin.err
	}
	return data
}

// argumentsValue 保留参数原文：合法 JSON 原样嵌入，非法 JSON 退化为字符串。
func argumentsValue(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(trimmed) {
		return json.RawMessage(trimmed)
	}
	return string(trimmed)
}

// actionFor 把工具名映射为审批动作。
func actionFor(name string, spec model.ToolSpec) string {
	switch model.ToolName(name) {
	case model.ToolBash:
		return "bash"
	case model.ToolEdit, model.ToolWrite:
		return "write"
	case model.ToolRead, model.ToolReadCode, model.ToolSearch:
		return "read"
	}
	if spec.Mutating {
		return "write"
	}
	return "read"
}

// pathsFromArgs 抽取审批需要的路径参数。
func pathsFromArgs(args map[string]any) []string {
	var out []string
	for _, key := range []string{"path", "cwd"} {
		if s, ok := args[key].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// commandFromArgs 抽取审批需要的命令参数。
func commandFromArgs(args map[string]any) string {
	s, _ := args["command"].(string)
	return s
}
