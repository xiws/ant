package tool

import (
	"context"
	"errors"
	"strings"

	"github.com/xiws/ant/internal/approval"
	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

// NewToolkit 构造工具层装配结果：注册内置工具，并装配 Runtime。
// 未提供的 Deps（Guard/Versions）在此补齐，避免组合根遗漏。
func NewToolkit(cfg config.View, deps Deps) *Toolkit {
	deps = normalizeDeps(cfg, deps)
	catalog := NewCatalog()
	for _, t := range builtinTools(cfg, deps) {
		catalog.Register(t)
	}
	return &Toolkit{Registry: catalog, Runtime: NewRuntime(cfg, catalog, deps)}
}

// normalizeDeps 为缺省协作者填上生产实现。
func normalizeDeps(cfg config.View, deps Deps) Deps {
	if deps.Guard == nil {
		deps.Guard = NewPathGuard(cfg, workDirOf(cfg))
	}
	if deps.Versions == nil {
		deps.Versions = NewVersionTable()
	}
	return deps
}

// builtinTool 是 Spec + 执行体的通用内置工具。
type builtinTool struct {
	spec model.ToolSpec
	exec func(context.Context, Invocation) (Result, error)
}

// Spec 实现 Tool。
func (t *builtinTool) Spec() model.ToolSpec { return t.spec }

// Execute 实现 Tool。
func (t *builtinTool) Execute(ctx context.Context, inv Invocation) (Result, error) {
	if t.exec == nil {
		return placeholderResult(t.spec.Name), nil
	}
	return t.exec(ctx, inv)
}

// placeholderResult 是本轮文件类工具的占位结果（Spec 契约已完整）。
func placeholderResult(name string) Result {
	return Result{OK: false, Err: &ToolError{
		Code:    "internal",
		Message: "工具实现将在后续阶段提供",
		Hint:    "该工具的执行体尚未实现；本轮只能查看其 Spec 或使用 ask",
		Details: map[string]any{"tool": name, "stage": "placeholder"},
	}}
}

// builtinTools 按 §7.3/§10 构造 7 个内置工具。
func builtinTools(cfg config.View, deps Deps) []Tool {
	limits := settingsOf(cfg).Limits
	toolTimeout := msDuration(limits.ToolTimeoutMs, defaultToolTimeoutMs)
	bashTimeout := msDuration(limits.BashTimeoutMs, defaultBashTimeoutMs)
	maxOutput := maxOutputBytesOf(cfg)

	return []Tool{
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolRead),
				Description: "读取 workspace 内文件内容，返回带行号的文本；超过上限时给出下一次 startLine 续读提示。",
				Schema:      []byte(readSchema),
				Mutating:    false,
				Timeout:     toolTimeout,
				Approval:    model.ApprovalFSReadOutside,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolRead)),
		},
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolReadCode),
				Description: "从 Go 函数/方法出发，按调用关系收集项目内依赖方法的精简代码切片，排除标准库与第三方依赖。",
				Schema:      []byte(readCodeSchema),
				Mutating:    false,
				Timeout:     toolTimeout,
				Approval:    model.ApprovalFSReadOutside,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolReadCode)),
		},
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolSearch),
				Description: "在 workspace 内按字面量或正则搜索，返回 file:line: 内容；遵循 .gitignore 与内置排除。",
				Schema:      []byte(searchSchema),
				Mutating:    false,
				Timeout:     toolTimeout,
				Approval:    model.ApprovalFSReadOutside,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolSearch)),
		},
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolEdit),
				Description: "用 unified diff 精确修改已存在的文件；先 read 再 edit，部分 hunk 不匹配时不落盘。",
				Schema:      []byte(editSchema),
				Mutating:    true,
				Timeout:     toolTimeout,
				Approval:    model.ApprovalFSWriteOutside,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolEdit)),
		},
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolWrite),
				Description: "新建文件或整文件重写；已有文件必须先 read 或提供 expectedHash。",
				Schema:      []byte(writeSchema),
				Mutating:    true,
				Timeout:     toolTimeout,
				Approval:    model.ApprovalFSWriteOutside,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolWrite)),
		},
		&builtinTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolBash),
				Description: "执行终端命令（POSIX 用 sh -c，Windows 用 cmd.exe /C）；非零退出码是数据而不是工具错误。",
				Schema:      []byte(bashSchema),
				Mutating:    true,
				Timeout:     bashTimeout,
				Approval:    model.ApprovalBashRisk,
				MaxOutput:   maxOutput,
			},
			exec: placeholderExec(string(model.ToolBash)),
		},
		&askTool{
			spec: model.ToolSpec{
				Name:        string(model.ToolAsk),
				Description: "信息不足或需要用户选择时向用户提问，可给出编号选项并允许自由文本。",
				Schema:      []byte(askSchema),
				Mutating:    false,
				Timeout:     0, // ask 不设置超时（§10.11）
				Approval:    "",
				MaxOutput:   maxOutput,
			},
			prompter: deps.Prompter,
		},
	}
}

func placeholderExec(name string) func(context.Context, Invocation) (Result, error) {
	return func(context.Context, Invocation) (Result, error) {
		return placeholderResult(name), nil
	}
}

// askTool 是唯一在本轮真正可执行的工具：把问题交给 cli 的 Prompter。
type askTool struct {
	spec     model.ToolSpec
	prompter approval.Prompter
}

// Spec 实现 Tool。
func (t *askTool) Spec() model.ToolSpec { return t.spec }

// Execute 实现 Tool。
func (t *askTool) Execute(ctx context.Context, inv Invocation) (Result, error) {
	question, _ := inv.Args["question"].(string)
	if strings.TrimSpace(question) == "" {
		return Result{OK: false, Err: &ToolError{
			Code:    "invalid_args",
			Message: "缺少提问内容 question",
			Hint:    "提供要问用户的完整问题",
		}}, nil
	}
	allowFreeText := true
	if v, ok := inv.Args["allowFreeText"].(bool); ok {
		allowFreeText = v
	}
	var options []approval.Option
	if raw, ok := inv.Args["options"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			opt := approval.Option{}
			opt.ID, _ = m["id"].(string)
			opt.Label, _ = m["label"].(string)
			opt.Description, _ = m["description"].(string)
			if opt.Label == "" {
				continue
			}
			if opt.ID == "" {
				opt.ID = opt.Label
			}
			options = append(options, opt)
		}
	}
	if t.prompter == nil {
		return Result{OK: false, Err: &ToolError{
			Code:    "requires_user_input",
			Message: "当前没有可用的交互界面，无法向用户提问",
			Hint:    "在交互模式运行，或把需要的信息写进任务描述后重试",
			Details: map[string]any{"question": question},
		}}, nil
	}
	answer, err := t.prompter.Ask(ctx, approval.Question{
		Question:      question,
		Options:       options,
		AllowFreeText: allowFreeText,
	})
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) && (me.Code == "non_interactive" || me.Class == model.ErrApprovalRequired) {
			return Result{OK: false, Err: &ToolError{
				Code:    "requires_user_input",
				Message: me.Message,
				Hint:    me.Hint,
			}}, nil
		}
		return Result{}, err
	}
	return Result{OK: true, Data: map[string]any{
		"selectedId": answer.SelectedID,
		"text":       answer.Text,
	}}, nil
}

// 以下 schema 与 docs/08-tools.md §10.5–§10.11 一一对应。

const readSchema = `{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "workspace 相对路径或绝对路径" },
    "startLine": { "type": "integer", "minimum": 1 },
    "endLine": { "type": "integer", "minimum": 1 }
  },
  "required": ["path"],
  "additionalProperties": false
}`

const readCodeSchema = `{
  "type": "object",
  "properties": {
    "target": {
      "type": "string",
      "description": "path#Symbol、Symbol、path:line 或 pkg.Symbol"
    },
    "depth": { "type": "integer", "minimum": 1, "maximum": 6 },
    "maxSymbols": { "type": "integer", "minimum": 1, "maximum": 100 }
  },
  "required": ["target"],
  "additionalProperties": false
}`

const searchSchema = `{
  "type": "object",
  "properties": {
    "query": { "type": "string" },
    "regex": { "type": "boolean", "default": false },
    "ignoreCase": { "type": "boolean", "default": false },
    "glob": { "type": "string" },
    "path": { "type": "string" },
    "maxResults": { "type": "integer", "minimum": 1, "maximum": 500 }
  },
  "required": ["query"],
  "additionalProperties": false
}`

const editSchema = `{
  "type": "object",
  "properties": {
    "path": { "type": "string" },
    "diff": { "type": "string", "description": "模型生成的 unified diff，可含围栏/文件头" },
    "expectedHash": { "type": "string", "description": "可选，读取时返回的 sha256" }
  },
  "required": ["path", "diff"],
  "additionalProperties": false
}`

const writeSchema = `{
  "type": "object",
  "properties": {
    "path": { "type": "string" },
    "content": { "type": "string" },
    "expectedHash": { "type": "string" }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`

const bashSchema = `{
  "type": "object",
  "properties": {
    "command": { "type": "string" },
    "cwd": { "type": "string", "description": "workspace 相对路径；默认 workspace 根" },
    "timeoutMs": { "type": "integer", "minimum": 1 },
    "description": { "type": "string", "description": "给用户看的用途说明" }
  },
  "required": ["command"],
  "additionalProperties": false
}`

const askSchema = `{
  "type": "object",
  "properties": {
    "question": { "type": "string" },
    "options": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "label": { "type": "string" },
          "description": { "type": "string" }
        },
        "required": ["label"],
        "additionalProperties": false
      }
    },
    "allowFreeText": { "type": "boolean", "default": true }
  },
  "required": ["question"],
  "additionalProperties": false
}`
