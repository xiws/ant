package tool

import (
	"context"
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
func (r *runtimeImpl) effectiveApproval(ctx context.Context, name string, spec model.ToolSpec, args map[string]any) (model.ApprovalClass, bool) {
	if spec.Approval == "" {
		return "", false
	}
	switch spec.Approval {
	case model.ApprovalFSReadOutside, model.ApprovalFSWriteOutside:
		access := AccessRead
		if spec.Approval == model.ApprovalFSWriteOutside {
			access = AccessWrite
		}
		paths := approvalPathsFromArgs(name, args)
		if len(paths) == 0 {
			return "", false
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			if r.deps.Guard == nil {
				return spec.Approval, true
			}
			res, err := r.deps.Guard.Resolve(ctx, p, access)
			if err != nil {
				// 保护路径等错误由执行阶段给出更精确的 tool 消息；
				// 这里仍要求审批，避免静默放行。
				return spec.Approval, true
			}
			if res.Outside {
				return spec.Approval, true
			}
		}
		return "", false
	case model.ApprovalBashRisk, model.ApprovalBashNetwork, model.ApprovalBashOutside:
		cmd := commandFromArgs(args)
		if cmd == "" {
			return "", false
		}
		if riskCommandPattern.MatchString(cmd) {
			return model.ApprovalBashRisk, true
		}
		if networkCommandPattern.MatchString(cmd) {
			network := "ask"
			if r.cfg != nil {
				network = r.cfg.Settings().Sandbox.Network
			}
			if network == "allow" {
				return "", false
			}
			// ask 与 deny 都需要进入审批；deny 由 approval.policy 决定拒绝。
			return model.ApprovalBashNetwork, true
		}
		if outsidePathPattern.MatchString(cmd) {
			return model.ApprovalBashOutside, true
		}
		return "", false
	default:
		return spec.Approval, true
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
