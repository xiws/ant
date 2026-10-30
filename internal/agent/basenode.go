package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiws/ant/internal/config"
	ctxmgr "github.com/xiws/ant/internal/context"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/prompt"
	"github.com/xiws/ant/internal/provider"
)

// baseNode 实现 Node.Run：组装 → 模型 → 工具循环 → 契约解析 → 事件（§21.11）。
type baseNode struct {
	deps Deps
	spec NodeSpec
	seq  int64
}

func (n *baseNode) Name() model.NodeName    { return n.spec.Name }
func (n *baseNode) Tools() []model.ToolName { return append([]model.ToolName(nil), n.spec.Tools...) }
func (n *baseNode) Contract() Contract      { return n.spec.Contract }

func (n *baseNode) Run(ctx context.Context, in NodeInput) (NodeOutput, error) {
	if n.deps.Provider == nil || n.deps.Prompt == nil || n.deps.Context == nil || n.deps.Registry == nil {
		return NodeOutput{}, &model.Error{
			Class:   model.ErrInternal,
			Code:    "agent_deps",
			Message: fmt.Sprintf("节点 %s 缺少必需依赖（provider/prompt/context/registry）", n.spec.Name),
		}
	}
	res, err := n.deps.Registry.ResolveForNode(n.spec.Name)
	if err != nil {
		return NodeOutput{}, err
	}

	nodeCtx := ctx
	if n.deps.Config != nil {
		if ms := n.deps.Config.Settings().Limits.NodeTimeoutMs; ms > 0 {
			var cancel context.CancelFunc
			nodeCtx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
	}

	tools := []model.ToolSpec(nil)
	if n.deps.ToolRegistry != nil && res.Capabilities.SupportsTools {
		tools = n.deps.ToolRegistry.Specs(n.spec.Name)
	}

	conversation := append([]model.Message(nil), in.History...)
	userMsg := model.Message{Role: model.RoleUser, Content: in.Request}
	// 引擎可能已把本轮 user 放进 History（§8.1）；避免重复 pin 导致双份用户消息。
	if n := len(conversation); n > 0 && conversation[n-1].Role == model.RoleUser && conversation[n-1].Content == in.Request {
		userMsg = model.Message{}
	}
	extraPinned := buildPinned(in)
	maxTurns := n.spec.MaxToolTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxToolTurns
		if n.deps.Config != nil {
			if v := n.deps.Config.Settings().Limits.MaxToolTurns; v > 0 {
				maxTurns = v
			}
		}
	}

	var (
		usage      model.Usage
		toolRounds int
		toolCalls  int
		changed    []string
		finalText  string
		structured json.RawMessage
		started    bool
		promptHash string
		stream     = n.streamEnabled(res)
		summary    = in.SessionSummary
	)

	for {
		pinned := append([]model.Message(nil), extraPinned...)
		if userMsg.Content != "" {
			pinned = append(pinned, userMsg)
		}
		prepared, perr := n.deps.Context.Prepare(nodeCtx, ctxmgr.PrepareInput{
			Node:    n.spec.Name,
			Cap:     res.Capabilities,
			Pricing: res.Pricing,
			Pinned:  pinned,
			History: conversation,
			Tools:   tools,
			Summary: summary,
			Usage:   usage,
		})
		if perr != nil {
			return n.partial(usage, toolCalls, changed), perr
		}
		summary = prepared.Summary
		recent := stripSummaryPlaceholder(prepared.Messages)
		bundle, berr := n.deps.Prompt.Assemble(nodeCtx, prompt.Request{
			Node:    n.spec.Name,
			Mode:    in.Mode,
			Project: n.projectDoc(),
			Tools:   tools,
			Summary: prepared.Summary,
			Recent:  recent,
			Runtime: n.runtimeInfo(),
		})
		if berr != nil {
			return n.partial(usage, toolCalls, changed), berr
		}
		promptHash = bundle.Meta.PromptHash

		if !started {
			if err := n.emitNodeStarted(nodeCtx, in, res, bundle, tools); err != nil {
				return n.partial(usage, toolCalls, changed), err
			}
			started = true
		}

		req := provider.ChatRequest{
			Model:      res.Alias,
			Messages:   bundle.Messages,
			Tools:      tools,
			ToolChoice: "auto",
			MaxTokens:  res.Capabilities.MaxOutputTokens,
			Stream:     stream,
		}
		if len(tools) == 0 {
			req.ToolChoice = "none"
		}
		streamed := false
		chatCtx := model.WithNode(nodeCtx, n.spec.Name)
		resp, cerr := n.deps.Provider.Chat(chatCtx, req, func(d provider.Delta) error {
			if d.Text != "" {
				streamed = true
			}
			return n.onDelta()(d)
		})
		if cerr != nil {
			return n.partial(usage, toolCalls, changed), cerr
		}
		usage = usage.Add(resp.Usage)
		if err := n.emitUsage(nodeCtx, resp.Usage); err != nil {
			return n.partial(usage, toolCalls, changed), err
		}

		assistant := resp.Message
		assistant.Role = model.RoleAssistant
		if !streamed && assistant.Content != "" && n.deps.OnText != nil {
			n.deps.OnText(assistant.Content)
		}
		if len(assistant.ToolCalls) == 0 {
			if err := n.emitMessage(nodeCtx, assistant); err != nil {
				return n.partial(usage, toolCalls, changed), err
			}
			finalText = assistant.Content
			break
		}

		if toolRounds >= maxTurns {
			return n.partial(usage, toolCalls, changed), &model.Error{
				Class:   model.ErrLoopDetected,
				Code:    "too_many_tool_turns",
				Message: fmt.Sprintf("节点 %s 工具轮数达到上限 %d", n.spec.Name, maxTurns),
				Hint:    "缩小任务范围，或调大 limits.maxToolTurns",
			}
		}
		warned := false
		for _, call := range assistant.ToolCalls {
			verdict, lerr := n.deps.Loop.Observe(n.spec.Name, call)
			if lerr != nil {
				return n.partial(usage, toolCalls, changed), lerr
			}
			if verdict == LoopAbort {
				return n.partial(usage, toolCalls, changed), &model.Error{
					Class:   model.ErrLoopDetected,
					Code:    "identical_tool_calls",
					Message: fmt.Sprintf("节点 %s 重复调用同一工具参数，已中止", n.spec.Name),
					Details: map[string]any{"tool": call.Name},
				}
			}
			if verdict == LoopWarn {
				warned = true
			}
		}

		if userMsg.Content != "" {
			conversation = append(conversation, userMsg)
		}
		conversation = append(conversation, assistant)
		if err := n.emitMessage(nodeCtx, assistant); err != nil {
			return n.partial(usage, toolCalls, changed), err
		}

		var toolMsgs []model.Message
		if n.deps.Runtime != nil {
			toolMsgs, err = n.deps.Runtime.ExecuteBatch(nodeCtx, n.spec.Name, assistant.ToolCalls)
			if err != nil {
				return n.partial(usage, toolCalls, changed), err
			}
		} else {
			for _, call := range assistant.ToolCalls {
				toolMsgs = append(toolMsgs, model.Message{
					Role:       model.RoleTool,
					ToolCallID: call.ID,
					Content:    `{"ok":false,"error":{"code":"internal","message":"工具运行时未装配"}}`,
				})
			}
		}
		toolCalls += len(assistant.ToolCalls)
		toolRounds++
		okIDs := make(map[string]bool, len(toolMsgs))
		for _, m := range toolMsgs {
			var body struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal([]byte(m.Content), &body) == nil && body.OK {
				okIDs[m.ToolCallID] = true
			}
			if strings.Contains(m.Content, `"code":"requires_user_input"`) {
				// §10.11：非交互模式下 ask 无法回答时立即中止当前 turn（退出码 4）。
				return n.partial(usage, toolCalls, changed), &model.Error{
					Class:   model.ErrApprovalRequired,
					Code:    "requires_user_input",
					Message: fmt.Sprintf("节点 %s 调用 ask 需要用户输入，但当前是非交互模式", n.spec.Name),
					Hint:    "在交互模式运行，或把需要的信息直接写进任务描述",
				}
			}
			conversation = append(conversation, m)
			if err := n.emitMessage(nodeCtx, m); err != nil {
				return n.partial(usage, toolCalls, changed), err
			}
		}
		for _, call := range assistant.ToolCalls {
			if call.Name != string(model.ToolEdit) && call.Name != string(model.ToolWrite) {
				continue
			}
			if !okIDs[call.ID] {
				continue // 失败/被拒绝的写操作不视为进度（§8.2 无进展检测）
			}
			if p := toolCallPath(call.Arguments); p != "" {
				changed = appendUnique(changed, p)
			}
		}
		if warned {
			conversation = append(conversation, model.Message{
				Role:    model.RoleSystem,
				Content: "检测到重复的工具调用参数；请换一种方案，不要重复同一调用。",
			})
		}
		// 下一轮不再重复 pin 用户请求：它已进入 conversation。
		userMsg = model.Message{}
	}

	parsed, perr := n.spec.Contract.Parse(nodeCtx, finalText)
	if perr != nil {
		if n.spec.Name == model.NodeVerifier {
			// 修复请求仍失败时按 pass=false 处理并保留原文（§7.4.1）。
			parsed = Parsed{
				Raw: model.MustJSON(map[string]string{"text": finalText}),
				Verdict: &model.Verdict{
					Pass:    false,
					Summary: "verifier 输出契约解析失败：" + perr.Error(),
					Failures: []model.Failure{{
						Severity:   model.SeverityBlocker,
						Evidence:   "verifier 输出不是合法契约 JSON",
						Suggestion: "检查 verifier prompt 覆盖是否删除了输出契约",
					}},
				},
			}
		} else {
			return n.partial(usage, toolCalls, changed), perr
		}
	}
	if n.isStructured() && len(parsed.Raw) > 0 {
		structured = parsed.Raw
	}
	var checks []model.Check
	if parsed.Verdict != nil {
		checks = parsed.Verdict.Checks
	}

	out := NodeOutput{
		Text:       finalText,
		Structured: structured,
		Parsed:     parsed,
		Usage:      usage,
		ToolCalls:  toolCalls,
		Changed:    changed,
		Checks:     checks,
	}
	if err := n.emitStepCompleted(nodeCtx, in, out, promptHash); err != nil {
		return out, err
	}
	return out, nil
}

func (n *baseNode) partial(usage model.Usage, calls int, changed []string) NodeOutput {
	return NodeOutput{Usage: usage, ToolCalls: calls, Changed: changed}
}

func (n *baseNode) isStructured() bool {
	if n.spec.Contract == nil {
		return false
	}
	switch n.spec.Name {
	case model.NodeVerifier, model.NodeDeliberate, model.NodeAggregation:
		return true
	}
	return false
}

func (n *baseNode) streamEnabled(res provider.Resolved) bool {
	if !res.Capabilities.SupportsStreaming {
		return false
	}
	if n.deps.Config == nil {
		return true
	}
	return n.deps.Config.Settings().Stream
}

func (n *baseNode) onDelta() func(provider.Delta) error {
	return func(d provider.Delta) error {
		if n.deps.OnDelta == nil {
			return nil
		}
		return n.deps.OnDelta(d)
	}
}

func (n *baseNode) projectDoc() prompt.ProjectDoc {
	if n.deps.Project == nil {
		return prompt.ProjectDoc{}
	}
	return n.deps.Project()
}

func (n *baseNode) runtimeInfo() prompt.RuntimeInfo {
	info := prompt.RuntimeInfo{Approval: "risk-based", OS: runtime.GOOS}
	if n.deps.Config != nil {
		s := n.deps.Config.Settings()
		info.Approval = s.Approval.Policy
		if s.Yes {
			info.Approval = "--yes"
		}
		info.Shell = s.Sandbox.Shell
	}
	if p, ok := n.deps.Config.(config.Paths); ok {
		info.CWD = p.WorkDir()
	}
	info.Date = n.deps.Clock().UTC().Format("2006-01-02")
	if info.Shell == "" {
		if runtime.GOOS == "windows" {
			info.Shell = "cmd.exe /C"
		} else {
			info.Shell = "sh -c"
		}
	}
	return info
}

func buildPinned(in NodeInput) []model.Message {
	var pinned []model.Message
	if in.Dimension != "" {
		pinned = append(pinned, model.Message{
			Role:    model.RoleSystem,
			Content: "## 本次评审维度\n" + string(in.Dimension) + "\n只从这个维度审查，不要替其他维度下结论。",
		})
	}
	if strings.TrimSpace(in.Plan) != "" {
		pinned = append(pinned, model.Message{Role: model.RoleSystem, Content: "## 当前计划\n" + in.Plan})
	}
	if len(in.Prior) > 0 {
		var b strings.Builder
		b.WriteString("## 上游节点输出")
		for _, p := range in.Prior {
			b.WriteString("\n\n### ")
			b.WriteString(string(p.Node))
			b.WriteString("\n")
			b.WriteString(strings.TrimSpace(p.Text))
		}
		pinned = append(pinned, model.Message{Role: model.RoleSystem, Content: b.String()})
	}
	if len(in.Artifacts) > 0 {
		var b strings.Builder
		b.WriteString("## 相关产物引用")
		for _, a := range in.Artifacts {
			b.WriteString("\n- ")
			b.WriteString(a.Name)
		}
		pinned = append(pinned, model.Message{Role: model.RoleSystem, Content: b.String()})
	}
	if len(in.Feedback) > 0 {
		var b strings.Builder
		b.WriteString("## 上轮验证未通过（必须修复）")
		for _, f := range in.Feedback {
			b.WriteString("\n- [")
			b.WriteString(string(f.Severity))
			b.WriteString("] ")
			if f.File != "" {
				b.WriteString(f.File)
				b.WriteString(": ")
			}
			b.WriteString(f.Evidence)
			if f.Suggestion != "" {
				b.WriteString("（建议：")
				b.WriteString(f.Suggestion)
				b.WriteString("）")
			}
		}
		pinned = append(pinned, model.Message{Role: model.RoleSystem, Content: b.String()})
	}
	return pinned
}

func stripSummaryPlaceholder(msgs []model.Message) []model.Message {
	if len(msgs) > 0 && msgs[0].Role == model.RoleSystem && strings.HasPrefix(msgs[0].Content, "## 会话摘要") {
		return msgs[1:]
	}
	return msgs
}

func toolCallPath(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func appendUnique(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}

func (n *baseNode) nextArtifact(kind, ext string) string {
	id := atomic.AddInt64(&n.seq, 1)
	ts := n.deps.Clock().UTC().Format("20060102-150405")
	return fmt.Sprintf("artifacts/%s-%s-%d%s", kind, ts, id, ext)
}

func (n *baseNode) emitNodeStarted(ctx context.Context, in NodeInput, res provider.Resolved, bundle prompt.Bundle, tools []model.ToolSpec) error {
	inputRef := model.ArtifactRef{}
	if n.deps.Store != nil {
		body, _ := json.Marshal(map[string]any{
			"node": n.spec.Name, "mode": in.Mode, "messages": bundle.Messages,
			"meta": bundle.Meta, "tools": tools, "request": in.Request,
			"plan": in.Plan, "feedback": in.Feedback, "dimension": in.Dimension,
		})
		ref, err := n.deps.Store.PutArtifact(ctx, n.nextArtifact("node-input", ".json"), body, "application/json")
		if err != nil {
			return err
		}
		inputRef = ref
	}
	return n.deps.Recorder.Emit(ctx, &model.Event{
		Type: model.EventNodeStarted,
		Node: n.spec.Name,
		Step: n.stepName(in.Mode),
		Data: model.MustJSON(map[string]any{
			"node": n.spec.Name, "model": res.Alias, "provider": res.ProviderName,
			"promptVersion": bundle.Meta.NodePromptVersion, "promptHash": bundle.Meta.PromptHash,
			"stableHash": bundle.Meta.StableHash, "toolsHash": bundle.Meta.ToolsHash,
			"inputRef": inputRef, "stream": n.streamEnabled(res),
		}),
	})
}

func (n *baseNode) emitUsage(ctx context.Context, u model.Usage) error {
	return n.deps.Recorder.Emit(ctx, &model.Event{
		Type: model.EventUsage,
		Node: n.spec.Name,
		Data: model.MustJSON(u),
	})
}

func (n *baseNode) emitMessage(ctx context.Context, msg model.Message) error {
	data := map[string]any{
		"role":        msg.Role,
		"contentHash": sha256String(msg.Content),
	}
	if msg.Content != "" && n.deps.Store != nil {
		ref, err := n.deps.Store.PutArtifact(ctx, n.nextArtifact("message", ".txt"), []byte(msg.Content), "text/plain")
		if err != nil {
			return err
		}
		data["contentRef"] = ref
	}
	if len(msg.ToolCalls) > 0 {
		data["toolCalls"] = msg.ToolCalls
	}
	if msg.ToolCallID != "" {
		data["toolCallID"] = msg.ToolCallID
	}
	return n.deps.Recorder.Emit(ctx, &model.Event{Type: model.EventMessage, Node: n.spec.Name, Data: model.MustJSON(data)})
}

func (n *baseNode) emitStepCompleted(ctx context.Context, in NodeInput, out NodeOutput, promptHash string) error {
	outputRef := model.ArtifactRef{}
	if n.deps.Store != nil {
		body, _ := json.Marshal(map[string]any{
			"text": out.Text, "structured": out.Structured, "usage": out.Usage,
			"toolCalls": out.ToolCalls, "changed": out.Changed, "checks": out.Checks,
		})
		ref, err := n.deps.Store.PutArtifact(ctx, n.nextArtifact("node-output", ".json"), body, "application/json")
		if err != nil {
			return err
		}
		outputRef = ref
	}
	status := "ok"
	if out.Parsed.Verdict != nil && !out.Parsed.Verdict.Pass {
		status = "failed"
	}
	return n.deps.Recorder.Emit(ctx, &model.Event{
		Type: model.EventStepCompleted,
		Node: n.spec.Name,
		Step: n.stepName(in.Mode),
		Data: model.MustJSON(map[string]any{
			"node": n.spec.Name, "status": status, "outputHash": sha256String(out.Text),
			"outputRef": outputRef, "usage": out.Usage, "promptHash": promptHash,
			"toolCalls": out.ToolCalls, "changed": out.Changed,
		}),
	})
}

func (n *baseNode) stepName(mode model.Mode) string {
	if mode == "" {
		return string(n.spec.Name)
	}
	return string(mode) + "." + string(n.spec.Name)
}

func sha256String(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
