package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

// options 是解析后的命令行。
type options struct {
	command string // "" | new | sessions | show | init | models | config | version | help
	prompt  string
	rest    []string // show 的 id、help 的子命令

	model          string
	mode           model.Mode
	sessionID      string
	newSession     bool
	nodeModels     map[string]string
	yes            bool
	nonInteractive bool
	jsonOut        bool
	logLevel       string
	noColor        bool
	help           bool
	showVersion    bool
	force          bool
	showEvents     bool

	limit        int
	filterMode   model.Mode
	filterStatus session.Status
}

var commandNames = map[string]bool{
	"new": true, "sessions": true, "show": true, "init": true,
	"models": true, "config": true, "version": true, "help": true,
}

// parseArgs 解析 `ant [flags] [prompt...]` 与子命令。
// 首个参数精确匹配子命令名时按子命令处理（§3.1）。
func parseArgs(args []string) (options, error) {
	opts := options{limit: 20, nodeModels: map[string]string{}}
	i := 0
	if len(args) > 0 && commandNames[args[0]] {
		opts.command = args[0]
		i = 1
	}

	var promptParts []string
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			promptParts = append(promptParts, args[i+1:]...)
			break
		}
		if arg == "" || arg[0] != '-' || arg == "-" {
			if opts.command == "show" || opts.command == "help" {
				opts.rest = append(opts.rest, arg)
			} else {
				promptParts = append(promptParts, arg)
			}
			i++
			continue
		}

		name, inlineValue, hasInline := splitFlag(arg)
		value := func() (string, error) {
			if hasInline {
				return inlineValue, nil
			}
			if i+1 >= len(args) {
				return "", usageErrorf("参数 %s 缺少取值", name)
			}
			i++
			return args[i], nil
		}

		switch name {
		case "-p", "--model":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.model = v
		case "-m", "--mode":
			v, err := value()
			if err != nil {
				return opts, err
			}
			mode := model.Mode(v)
			if !mode.Valid() {
				return opts, usageErrorf("未知模式 %q（可选 code|plan|ask|council）", v)
			}
			opts.mode = mode
		case "-s", "--session":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.sessionID = v
		case "--new":
			opts.newSession = true
		case "--node-model":
			v, err := value()
			if err != nil {
				return opts, err
			}
			parts := strings.SplitN(v, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return opts, usageErrorf("--node-model 需要 <node>=<alias>，实际 %q", v)
			}
			opts.nodeModels[parts[0]] = parts[1]
		case "-y", "--yes":
			opts.yes = true
		case "--non-interactive":
			opts.nonInteractive = true
		case "--json":
			opts.jsonOut = true
		case "--log-level":
			v, err := value()
			if err != nil {
				return opts, err
			}
			switch v {
			case "error", "warn", "info", "debug":
				opts.logLevel = v
			default:
				return opts, usageErrorf("--log-level 取值必须是 error|warn|info|debug")
			}
		case "--no-color":
			opts.noColor = true
		case "-h", "--help":
			opts.help = true
		case "--version":
			opts.showVersion = true
		case "--force":
			opts.force = true
		case "--events":
			opts.showEvents = true
		case "--limit":
			v, err := value()
			if err != nil {
				return opts, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return opts, usageErrorf("--limit 需要非负整数")
			}
			opts.limit = n
		case "--status":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.filterStatus = session.Status(v)
		default:
			return opts, usageErrorf("未知参数 %s", arg)
		}
		i++
	}

	opts.prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if opts.command == "" && promptParts != nil {
		opts.prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	}
	if opts.command == "sessions" {
		opts.filterMode = opts.mode
	}
	if opts.help && opts.command == "" {
		opts.command = "help"
	}
	if opts.showVersion {
		opts.command = "version"
	}
	if len(opts.rest) > 1 {
		opts.rest = opts.rest[:1]
	}
	return opts, nil
}

func splitFlag(arg string) (name, value string, hasValue bool) {
	if i := strings.IndexByte(arg, '='); i > 0 {
		return arg[:i], arg[i+1:], true
	}
	return arg, "", false
}

// usageError 表示参数/用法错误（退出码 2）。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

func isUsageError(err error) bool {
	_, ok := err.(*usageError)
	return ok
}

const usageText = `ant - AI coding agent

Usage:
  ant [flags] [prompt...]
  ant new [flags] [prompt...]
  ant sessions [flags]
  ant show <session-id>
  ant init [--force]
  ant models
  ant config
  ant version
  ant help

Modes:
  code     修改代码并由 verifier 验证（默认）
  plan     规划 -> 三方评审 -> 聚合 -> 验证，输出 plan.md
  ask      只读问答
  council  修改 -> 三方评审 -> 聚合 -> 验证

Flags:
  -p, --model string       模型别名（models[].name）
  -m, --mode string        模式：code|plan|ask|council
  -s, --session string     指定 session（支持短前缀）
      --new                强制新建 session
      --node-model strings 节点级模型覆盖，如 code=deepseek-v4-flash
  -y, --yes                自动批准非硬拒绝操作（危险）
      --non-interactive    禁止交互；需要审批/提问时退出码 4
      --json               stdout 输出 NDJSON 事件
      --log-level string   error|warn|info|debug
      --no-color           关闭颜色
  -h, --help               帮助
      --version            版本

Exit codes:
  0 成功  1 任务失败  2 用法/配置错误  3 中断  4 需要审批/提问  5 Provider 失败
`

const commandHelp = `ant show <session-id> [--events]   查看 session 摘要/事件
ant sessions [--limit N] [--mode M] [--status S] [--json]
ant init [--force]               生成/刷新 .ant/project.md
ant models                       列出模型别名与能力
ant config                       打印合并后的配置（密钥打码）
ant version                      版本信息`
