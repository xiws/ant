package approval

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type grants struct {
	mu   sync.RWMutex
	seen map[model.ApprovalClass]bool
}

// NewGrants 返回本 session 的类别放行表。
func NewGrants() Grants { return &grants{seen: map[model.ApprovalClass]bool{}} }

func (g *grants) Granted(class model.ApprovalClass) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.seen[class]
}

func (g *grants) Grant(class model.ApprovalClass) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen[class] = true
}

// Restore 从恢复出的历史事件灌入放行类别（§9.6）。
func (g *grants) Restore(classes []model.ApprovalClass) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range classes {
		g.seen[c] = true
	}
}

type classifier struct{ cfg config.View }

// NewClassifier 按 §12.3 的类别表判断是否需要审批。
func NewClassifier(cfg config.View) Classifier { return &classifier{cfg: cfg} }

func (c *classifier) Classify(ctx context.Context, req Request) (model.ApprovalClass, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	switch req.Class {
	case model.ApprovalFSReadOutside, model.ApprovalFSWriteOutside,
		model.ApprovalBashRisk, model.ApprovalBashNetwork, model.ApprovalBashOutside,
		model.ApprovalPlanToCode:
		return req.Class, true, nil
	}
	return req.Class, false, nil
}

type hardDeny struct {
	extra []string
}

var builtinDeny = []string{
	"rm -rf /", "rm -rf --no-preserve-root", "mkfs", "dd of=/dev/",
	"shutdown", "reboot", "halt", "chmod -r 777 /",
	":(){:|:&};:", "format ", "del /s /q c:\\", "bcdedit", "diskpart",
}

// NewHardDeny 构造硬拒绝匹配器；extra 追加但不移除内置项（§12.4）。
func NewHardDeny(cfg config.View) HardDeny {
	var extra []string
	if cfg != nil {
		extra = append(extra, cfg.Settings().Sandbox.HardDenyExtra...)
	}
	return &hardDeny{extra: extra}
}

func (h *hardDeny) Match(command string) (string, bool) {
	norm := normalizeCommand(command)
	if norm == "" {
		return "", false
	}
	for _, pat := range builtinDeny {
		if strings.Contains(norm, normalizeCommand(pat)) {
			return pat, true
		}
	}
	for _, pat := range h.extra {
		if pat = normalizeCommand(pat); pat != "" && strings.Contains(norm, pat) {
			return pat, true
		}
	}
	return "", false
}

func normalizeCommand(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

type approver struct {
	cfg        config.View
	prompter   Prompter
	grants     Grants
	sink       model.EventSink
	deny       HardDeny
	classifier Classifier
}

// New 组装出唯一审批入口。Prompter 由 cli 提供，Grants 由组合根在恢复后灌入。
func New(cfg config.View, p Prompter, g Grants, sink model.EventSink) Approver {
	if g == nil {
		g = NewGrants()
	}
	return &approver{
		cfg:        cfg,
		prompter:   p,
		grants:     g,
		sink:       sink,
		deny:       NewHardDeny(cfg),
		classifier: NewClassifier(cfg),
	}
}

func (a *approver) Decide(ctx context.Context, req Request) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	s := a.cfg.Settings()

	// 1. 硬拒绝：不可被 y/a/--yes 覆盖。
	if rule, denied := a.deny.Match(req.Command); denied {
		dec := Decision{Allow: false, Scope: ScopeOnce, Reason: "hard_deny:" + rule}
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		if err := a.resolved(ctx, req, dec, "hard_deny"); err != nil {
			return Decision{}, err
		}
		return dec, nil
	}

	// 1.5 配置中的 deny/allow glob（deny 优先，§4.5）。
	if pat, ok := matchGlob(s.Approval.Deny, req); ok {
		dec := Decision{Allow: false, Scope: ScopeOnce, Reason: "deny_glob:" + pat}
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		if err := a.resolved(ctx, req, dec, "deny_glob"); err != nil {
			return Decision{}, err
		}
		return dec, nil
	}
	if pat, ok := matchGlob(s.Approval.Allow, req); ok {
		return Decision{Allow: true, Scope: ScopeOnce, Reason: "allow_glob:" + pat}, nil
	}

	// 2. 本 session 已放行该类。
	if a.grants.Granted(req.Class) {
		return Decision{Allow: true, Scope: ScopeSession, Reason: "granted_session"}, nil
	}

	// 3. 分级判定：不需要审批直接放行。
	class, needs, err := a.classifier.Classify(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	if class != "" {
		req.Class = class
	}
	if !needs {
		return Decision{Allow: true, Reason: "auto"}, nil
	}

	// 4. --yes / approval.policy=always 放行；never 拒绝（§12.3）。
	if s.Yes || s.Approval.Policy == "always" {
		dec := Decision{Allow: true, Scope: ScopeOnce, Reason: "yes"}
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		if err := a.resolved(ctx, req, dec, "yes"); err != nil {
			return Decision{}, err
		}
		return dec, nil
	}
	if s.Approval.Policy == "never" {
		dec := Decision{Allow: false, Scope: ScopeOnce, Reason: "policy_never"}
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		if err := a.resolved(ctx, req, dec, "policy_never"); err != nil {
			return Decision{}, err
		}
		return dec, nil
	}

	// 5. 非交互：返回 ErrNonInteractive，不落假批准。
	if s.NonInteractive {
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		return Decision{}, ErrNonInteractive
	}
	if a.prompter == nil {
		if err := a.requested(ctx, req); err != nil {
			return Decision{}, err
		}
		return Decision{}, ErrNonInteractive
	}

	// 6. 交互确认。
	if err := a.requested(ctx, req); err != nil {
		return Decision{}, err
	}
	choice, err := a.prompter.Confirm(ctx, ConfirmPrompt{
		Class:  req.Class,
		Title:  titleFor(req),
		Detail: detailFor(req),
		CWD:    req.CWD,
		Reason: req.Reason,
	})
	if err != nil {
		return Decision{}, err
	}
	dec := Decision{Scope: ScopeOnce}
	switch choice {
	case ChoiceAll:
		a.grants.Grant(req.Class)
		dec.Allow = true
		dec.Scope = ScopeSession
	case ChoiceOnce:
		dec.Allow = true
	default:
		dec.Allow = false
	}
	if dec.Allow {
		dec.Reason = "user_allow"
	} else {
		dec.Reason = "user_deny"
	}
	if err := a.resolved(ctx, req, dec, "user"); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

func (a *approver) requested(ctx context.Context, req Request) error {
	if a.sink == nil {
		return nil
	}
	return a.sink.Emit(ctx, &model.Event{
		Type: model.EventApprovalRequested,
		Node: req.Node,
		Data: model.MustJSON(map[string]any{
			"class":   req.Class,
			"node":    req.Node,
			"action":  req.Action,
			"command": req.Command,
			"paths":   req.Paths,
			"cwd":     req.CWD,
			"reason":  req.Reason,
		}),
	})
}

func (a *approver) resolved(ctx context.Context, req Request, dec Decision, source string) error {
	if a.sink == nil {
		return nil
	}
	decision := "deny"
	if dec.Allow {
		decision = "allow"
	}
	return a.sink.Emit(ctx, &model.Event{
		Type: model.EventApprovalResolved,
		Node: req.Node,
		Data: model.MustJSON(map[string]any{
			"class":    req.Class,
			"decision": decision,
			"scope":    string(dec.Scope),
			"source":   source,
			"reason":   dec.Reason,
		}),
	})
}

func titleFor(req Request) string {
	switch req.Class {
	case model.ApprovalBashRisk:
		return "bash 执行风险命令"
	case model.ApprovalBashNetwork:
		return "bash 访问网络"
	case model.ApprovalBashOutside:
		return "bash 访问 workspace 之外的路径"
	case model.ApprovalFSReadOutside:
		return "读取 workspace 之外的文件"
	case model.ApprovalFSWriteOutside:
		return "写入 workspace 之外的文件"
	case model.ApprovalPlanToCode:
		return "计划通过，切换 code 模式执行"
	default:
		return "需要审批：" + string(req.Class)
	}
}

func detailFor(req Request) string {
	if req.Command != "" {
		return "命令：" + req.Command
	}
	if len(req.Paths) > 0 {
		return "路径：" + strings.Join(req.Paths, ", ")
	}
	return req.Reason
}

func matchGlob(patterns []string, req Request) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}
	values := []string{}
	if req.Command != "" {
		values = append(values, req.Command)
	}
	values = append(values, req.Paths...)
	for _, pat := range patterns {
		for _, v := range values {
			if v == "" {
				continue
			}
			if wildcardMatch(pat, v) {
				return pat, true
			}
			if !strings.ContainsAny(pat, "*?[") && strings.Contains(v, pat) {
				return pat, true
			}
		}
	}
	return "", false
}

// wildcardMatch 是面向命令/路径 glob 的简单匹配：* 匹配任意字符（含 /）。
func wildcardMatch(pattern, value string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(value)
}
