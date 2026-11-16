package tool

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/xiws/ant/internal/model"
)

// 风险/网络命令启发式（§12.3）。只用于决定「是否需要审批」，
// 硬拒绝由 approval.HardDeny 单独负责。
var riskCommandPattern = regexp.MustCompile(`(?i)(^|[\s;&|])(sudo|su|chown|chmod|rm\s+-[a-z]*r[a-z]*|git\s+reset\s+--hard|git\s+clean\s+-[a-z]*[fd][a-z]*|git\s+push\s+--force|go\s+install|npm\s+install\s+-g|pip3?\s+install|brew\s+install|apt(-get)?\s+install|kill\s+-9|pkill|launchctl|systemctl|eval)([\s;&|]|$)`)

var networkCommandPattern = regexp.MustCompile(`(?i)(^|[\s;&|])(curl|wget|ssh|scp|nc|telnet|git\s+clone|go\s+get|npm\s+install|pip3?\s+install|brew\s+install)([\s;&|]|$)`)

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

// systemPathPrefixes 是 workspace 未知时使用的越界启发式（配置视图未提供 workDir）。
var systemPathPrefixes = []string{
	"/etc", "/usr", "/var", "/root", "/home", "/opt", "/tmp",
	"/bin", "/sbin", "/private", "/System", "/Library",
}

// bashPseudoPathPrefixes 是重定向常用的设备/伪路径。它们不是 workspace 之外的
// 普通文件，`2>/dev/null` 这类写法不应触发 bash_outside 审批（§12.3）。
var bashPseudoPathPrefixes = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/tty", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/fd",
}

// commandTouchesOutside 检查命令中出现的路径是否逃出 workspace（§12.3）。
//
// 判定口径与 pathGuard 一致：workspace 在启动时已 EvalSymlinks，而命令里写的可能
// 是未解析的拼法（macOS 的 /var→/private/var、指向项目目录的软链），所以必须先把
// token 归一到真实路径再比较——只做 filepath.Rel 会把 workspace 内的路径误判成越界。
// 本函数只决定「是否需要审批」，硬拒绝由 approval.HardDeny 负责。
func commandTouchesOutside(command, workDir string) bool {
	tokens := commandPathTokens(command)
	if len(tokens) == 0 {
		return false
	}
	if workDir == "" {
		for _, tok := range tokens {
			p := expandHome(tok)
			if !filepath.IsAbs(p) {
				// 没有 workspace 时 cwd 就是 bash 的工作目录，相对逃逸仍按越界处理。
				if isOutsideRel(filepath.Clean(p)) {
					return true
				}
				continue
			}
			p = filepath.Clean(p)
			if isBashPseudoPath(p) {
				continue
			}
			for _, prefix := range systemPathPrefixes {
				if hasPathPrefix(p, prefix) {
					return true
				}
			}
		}
		return false
	}
	root := realAbs(workDir)
	for _, tok := range tokens {
		if tokenTouchesOutside(tok, root) {
			return true
		}
	}
	return false
}

// commandPathTokens 把命令拆成「像路径」的参数。
//
// 只做词法切分而不解析 shell：以空白、引号、重定向/管道/分隔符、括号和 `=` 为边界，
// 因此 `--out=/x`、`2>/dev/null`、`a && cd b` 里的路径都能被单独取出；点与冒号
// 不作为边界，否则 `./internal` 这类路径会被切开。
func commandPathTokens(command string) []string {
	fields := strings.FieldsFunc(command, isCommandTokenDelimiter)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		tok := trimCommandTokenPunct(field)
		if strings.HasPrefix(tok, "-") {
			// 短选项可以把路径直接接在选项名后（`-I/usr/include`、`-o../out`）。
			if p := attachedFlagPath(tok); p != "" {
				out = append(out, p)
			}
			continue
		}
		if !pathishToken(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// attachedFlagPath 取出「选项名 + 紧接的路径」里的路径部分；`-v`、`--all` 这类
// 纯选项返回空串。长选项的取值一律走 `=`（已在切分阶段拆开），故不在此处理。
func attachedFlagPath(tok string) string {
	if strings.HasPrefix(tok, "--") {
		return ""
	}
	rest := strings.TrimLeft(tok, "-")
	if rest == "" {
		return ""
	}
	i := 0
	for i < len(rest) && isOptionNameByte(rest[i]) {
		i++
	}
	if i == 0 || i == len(rest) {
		return ""
	}
	switch rest[i] {
	case '/', '~', '.':
		return rest[i:]
	}
	return ""
}

func isOptionNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func isCommandTokenDelimiter(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', '\u3000',
		'"', '\'', '`',
		'=', '<', '>', '|', ';', '&',
		'(', ')', ',':
		return true
	}
	return false
}

// trimCommandTokenPunct 剥掉参数尾部的标点（句末、列表分隔、全角标点），
// 但保留以 `..` 结尾的 token（`../..`、`/x/..`），否则 `..` 逃逸会被抹掉。
func trimCommandTokenPunct(tok string) string {
	for len(tok) > 0 && !strings.HasSuffix(tok, "..") {
		r, size := utf8.DecodeLastRuneInString(tok)
		switch r {
		case '.', ':', '!', '?', '，', '。', '；', '：', '！', '？', '、', '）', '】', '》', '」', '』':
			trimmed := tok[:len(tok)-size]
			if trimmed == "" || isWindowsDriveToken(trimmed) {
				return tok
			}
			tok = trimmed
		default:
			return tok
		}
	}
	return tok
}

func isWindowsDriveToken(s string) bool {
	if len(s) != 2 || s[1] != ':' {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// pathishToken 判断参数是否可能是路径：`~` 展开、绝对路径、含分隔符的相对路径，
// 以及单独成词的 `..`。纯符号参数（`-v`、`--all`、`a..b`）不参与越界判定。
func pathishToken(tok string) bool {
	if tok == "" {
		return false
	}
	if tok == ".." || tok == "." {
		return true
	}
	if strings.HasPrefix(tok, "~") {
		return true
	}
	return strings.ContainsAny(tok, `/\`)
}

// tokenTouchesOutside 把单个路径 token 归一到真实路径，再与 workspace 比较。
func tokenTouchesOutside(tok, workDir string) bool {
	p := expandHome(tok)
	if !filepath.IsAbs(p) {
		p = filepath.Join(workDir, p)
	}
	p = filepath.Clean(p)
	if isBashPseudoPath(p) {
		return false
	}
	rel, err := filepath.Rel(workDir, resolveExisting(p))
	return err != nil || isOutsideRel(rel)
}

func isBashPseudoPath(p string) bool {
	for _, prefix := range bashPseudoPathPrefixes {
		if hasPathPrefix(p, prefix) {
			return true
		}
	}
	return false
}
