package tool

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// 风险/网络命令启发式（§12.3）。只用于决定「是否需要审批」，
// 硬拒绝由 approval.HardDeny 单独负责。
var riskCommandPattern = regexp.MustCompile(`(?i)(^|[\s;&|])(sudo|su|chown|chmod|rm\s+-[a-z]*r[a-z]*|git\s+reset\s+--hard|git\s+clean\s+-[a-z]*[fd][a-z]*|git\s+push\s+--force|go\s+install|npm\s+install\s+-g|pip3?\s+install|brew\s+install|apt(-get)?\s+install|kill\s+-9|pkill|launchctl|systemctl|eval)([\s;&|]|$)`)

var networkCommandPattern = regexp.MustCompile(`(?i)(^|[\s;&|])(curl|wget|ssh|scp|nc|telnet|git\s+clone|go\s+get|npm\s+install|pip3?\s+install|brew\s+install)([\s;&|]|$)`)

var outsidePathPattern = regexp.MustCompile(`(^|[\s"'=])(/(etc|usr|var|root|home|opt|tmp|bin|sbin|private|System|Library)/|\.\./)`)

// effectiveApproval 把 ToolSpec 的「默认审批类别」细化为「本次调用是否需要审批」。
// 这一步避免 workspace 内的 read/write 每次都触发人工确认（§12.3）。
func (r *runtimeImpl) effectiveApproval(ctx context.Context, name string, spec model.ToolSpec, args map[string]any) (model.ApprovalClass, bool, *ToolError) {
	if spec.Approval == "" {
		return "", false, nil
	}
	switch spec.Approval {
	case model.ApprovalFSReadOutside, model.ApprovalFSWriteOutside:
		access := AccessRead
		if spec.Approval == model.ApprovalFSWriteOutside {
			access = AccessWrite
		}
		paths := approvalPathsFromArgs(name, args)
		if len(paths) == 0 {
			return "", false, nil
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			if r.deps.Guard == nil {
				return spec.Approval, true, nil
			}
			res, err := r.deps.Guard.Resolve(ctx, p, access)
			if err != nil {
				// 保护路径是硬拒绝，不进入审批；其它解析错误交给执行阶段给出精确消息。
				if model.CodeOf(err) == "path_protected" {
					return "", false, pathError(err)
				}
				return spec.Approval, true, nil
			}
			if res.Outside {
				return spec.Approval, true, nil
			}
		}
		return "", false, nil
	case model.ApprovalBashRisk, model.ApprovalBashNetwork, model.ApprovalBashOutside:
		cmd := commandFromArgs(args)
		if cmd == "" {
			return "", false, nil
		}
		if r.deny != nil {
			if rule, denied := r.deny.Match(cmd); denied {
				return "", false, &ToolError{
					Code:    "approval_denied",
					Message: "命令命中硬拒绝规则，已拒绝执行",
					Hint:    "硬拒绝不可通过审批或 --yes 覆盖；请换一种安全的实现方式",
					Details: map[string]any{"rule": rule, "command": cmd},
				}
			}
		}
		if networkCommandPattern.MatchString(cmd) {
			network := "ask"
			if r.cfg != nil {
				network = r.cfg.Settings().Sandbox.Network
			}
			switch network {
			case "allow":
				// 网络放行不等于风险放行：继续检查风险命令/越界路径。
			case "deny":
				return "", false, &ToolError{
					Code:    "approval_denied",
					Message: "sandbox.network=deny：网络命令被拒绝",
					Hint:    "改用本地数据，或由用户把 sandbox.network 改为 ask/allow",
					Details: map[string]any{"command": cmd, "network": network},
				}
			default:
				return model.ApprovalBashNetwork, true, nil
			}
		}
		if riskCommandPattern.MatchString(cmd) {
			return model.ApprovalBashRisk, true, nil
		}
		if commandTouchesOutside(cmd, r.workDir) {
			return model.ApprovalBashOutside, true, nil
		}
		return "", false, nil
	default:
		return spec.Approval, true, nil
	}
}

func approvalPathsFromArgs(name string, args map[string]any) []string {
	var out []string
	for _, key := range []string{"path", "file"} {
		if v, ok := args[key].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	if name == string(model.ToolReadCode) {
		if target, ok := args["target"].(string); ok && target != "" {
			// path#Symbol / path:line 才按路径判断；纯符号名不触发越界审批。
			if i := strings.IndexByte(target, '#'); i > 0 {
				out = append(out, target[:i])
			} else if !strings.Contains(target, ".") {
				if i := strings.IndexByte(target, ':'); i > 0 {
					out = append(out, target[:i])
				}
			}
		}
	}
	return out
}

var absoluteTokenPattern = regexp.MustCompile(`(?:^|[\s"'=<>|;&(])((?:[A-Za-z]:[\\/]|/)[^\s"'|;&<>()]*)`)

// commandTouchesOutside 检查命令中的绝对路径是否逃出 workspace（§12.3）。
func commandTouchesOutside(command, workDir string) bool {
	if strings.Contains(command, "../") || strings.Contains(command, `..\`) {
		return true
	}
	if workDir == "" {
		return outsidePathPattern.MatchString(command)
	}
	for _, m := range absoluteTokenPattern.FindAllStringSubmatch(command, -1) {
		if len(m) < 2 {
			continue
		}
		p := strings.TrimRight(m[1], `,;:)`)
		if !filepath.IsAbs(p) {
			continue
		}
		rel, err := filepath.Rel(workDir, filepath.Clean(p))
		if err != nil || isOutsideRel(rel) {
			return true
		}
	}
	return false
}
