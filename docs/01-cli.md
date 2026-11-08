> 模块 1 / 19 · [返回索引](../README.md) · [上一模块](00-overview.md) · [下一模块](02-config.md)

# 3. CLI 规格

## 3.1 总览

```text
ant [flags] [prompt...]
ant new [flags] [prompt...]
ant sessions [flags]
ant show <session-id>
ant export [session-id] [--format html|md] [-o path|-] [--events]
ant init [--force]
ant models
ant config
ant version
ant help
```

- 首个参数精确匹配子命令名时按子命令处理；否则作为 prompt。
- 需要以子命令名作为 prompt 时使用 `--`：`ant -- sessions`。
- 没有参数时：TTY 进入 REPL；stdin 是管道则读取整段 stdin 作为 prompt；两者都不是则打印帮助并以退出码 2 结束。

## 3.2 命令

| 命令 | 行为 | 关键规则 |
| --- | --- | --- |
| `ant "prompt"` | 继续最近 session；没有则新建 `code` session | 最近 = 当前 workspace 中 `updatedAt` 最大者，不论状态 |
| `ant new [prompt]` | 强制新建 session | 有 prompt 立即执行；无 prompt 且 TTY 进入 REPL；非 TTY 报错 |
| `ant new -m plan [-p model]` | 新建 plan session | 无 prompt 时进入 REPL，由用户输入第一个任务 |
| `ant sessions` | 列出当前 workspace 的 session | 支持 `--json`、`--limit`、`--mode`、`--status` |
| `ant show <id>` | 查看单个 session 摘要 | 接受 ID 短前缀；显示 turns、usage、审批记录、产物路径；`--events` 查看事件流 |
| `ant export [id]` | 导出完整调试链路 | 省略 id 时导出最近 session；`--format html\|md`；`-o -` 写 stdout；`--events` 附原始事件；默认写 session 目录的 `trace.html` |
| `ant init [--force]` | 生成/重新生成项目上下文 | `--force` 忽略 hash |
| `ant models` | 列出模型别名与能力 | 不显示密钥；标记默认模型与不可用模型 |
| `ant config` | 打印合并后的配置 | 每个键标注来源（home/workspace/cli）；密钥打码 |
| `ant version` / `ant help` | 版本/帮助 | `help <command>` 显示子命令帮助 |

**session 选择规则**：

- `ant`（无子命令、无 `-s`、无 `--new`）→ 续当前 workspace 最近 session。
- `ant -s <id>` → 续指定 session；短前缀必须在当前 workspace 唯一。
- `ant --new` 等价 `ant new`。
- 最近 session 即使状态为 `interrupted`、`failed` 也允许续；`done` 概念不存在（session 始终可续）。
- session 只属于创建它的 workspace；不支持跨 workspace 读取。

`ant sessions` 输出示例：

```text
ID                       UPDATED   MODE   MODEL              TURNS  TOKENS  STATUS  TITLE
20261030-094704-3f9a2c   10:02     code   deepseek-v4-flash  1      1.8k    idle    给 Test 增加边界检查
20261030-101500-8b1e04   10:20     plan   qwen3.7-flash_snap 3      12.4k   idle    设计 session 存储方案
```

**模式规则**：

- 新 session 使用 `-m/--mode`，缺省取 `settings.defaultMode`（默认 `code`）。
- 续 session 时 `-m` 与当前模式相同则直接继续；不同则交互确认后切换（保留历史，重置当前状态机轮次），非交互直接以退出码 2 报错。
- `plan` 验证通过后的 Plan gate 可以显式切换到 `code`（[§8.3](06-state-machine.md#83-plan-模式)）。

**模型规则**：

- `-p/--model <alias>` 选择模型；解析顺序见 [§5.1](03-models.md#51-模型解析)。
- `--node-model code=alias`（可重复）按节点覆盖；优先级高于 `settings.nodeModels`。
- 未指定且没有 `defaultModel`/`defaultProvider` 时，若全局恰好只有一个模型则使用它并警告；否则报错列出可用别名（[§5.1](03-models.md#51-模型解析)）。

## 3.3 参数

| 参数 | 取值 | 说明 |
| --- | --- | --- |
| `-p, --model` | 模型别名 | 保留原始 `-p`；长名为 `--model`，因为值是模型而非 provider |
| `-m, --mode` | `code` \| `plan` \| `ask` \| `council` | 同时支持 `-m` 与 `--mode`，修正原文不一致 |
| `-s, --session` | session ID 或短前缀 | 显式续指定 session |
| `--new` | 布尔 | 等价 `new` 子命令 |
| `--node-model` | `node=alias`，可重复 | 节点级模型覆盖 |
| `-y, --yes` | 布尔 | 自动批准非硬拒绝操作；启动时打印警告 |
| `--non-interactive` | 布尔 | 禁止任何提示；需要 ask/审批时退出码 4 |
| `--json` | 布尔 | stdout 输出 NDJSON 事件；人类日志走 stderr |
| `--log-level` | `error`\|`warn`\|`info`\|`debug` | 覆盖 `settings.log.level` |
| `-v, --verbose` | 布尔 | 实时展示节点、请求/响应摘要、工具完整参数与结果预览 |
| `--no-verbose` | 布尔 | 显式关闭详细输出，覆盖 `settings.ui.verbose` |
| `-o, --output` | 路径或 `-` | `export` 输出文件；`-` 表示 stdout |
| `--format` | `html`\|`md` | `export` 格式；缺省按 `-o` 扩展名推断 |
| `--no-color` | 布尔 | 关闭颜色；同时尊重 `NO_COLOR` |
| `-h, --help` | 布尔 | 显示帮助 |
| `--version` | 布尔 | 等价 `ant version` |
| `--` | 分隔符 | 其后全部视为 prompt |

## 3.4 退出码

| 退出码 | 含义 |
| --- | --- |
| `0` | 成功（含 verification 通过、plan 已输出） |
| `1` | 任务失败（verification 不通过且达到轮次上限、节点失败、无进展中止） |
| `2` | 用法错误或配置错误（未知参数、模型无法解析、模式与 session 冲突） |
| `3` | 用户中断（SIGINT / Ctrl-C） |
| `4` | 非交互模式下需要审批或需要用户回答 `ask` |
| `5` | Provider 重试后仍失败（鉴权、限流、不可用） |

## 3.5 交互与 REPL

- TTY 下的普通对话：用户输入一行，agent 执行一个 turn；turn 结束后回到提示符。
- REPL 命令：
  - `/exit`：退出。
  - `/new`：新建 session。
  - `/mode <mode>`：切换模式（需要确认）。
  - `/help`：帮助。
  - 其他 `/xxx`：报错并提示可用命令，不当作 prompt。
- 审批提示格式：

```text
[ant] 需要审批：bash 访问 workspace 之外的路径
命令：cat /etc/hosts
cwd：/Users/me/project
允许？ [y] 一次 / [n] 拒绝 / [a] 本 session 同类全部允许：
```

- `y` 只批准本次；`n` 返回结构化错误给模型；`a` 在本 session 内对该审批类别全部放行（硬拒绝清单仍拦截）。
- `ask` 工具提示使用编号选项 + 自由输入：

```text
[ant] 需要你确认：是否保留现有 API？
  1) 保留（推荐）
  2) 替换
  3) 其他（请输入）
>
```

- 非交互模式：任何审批/`ask` 都返回退出码 4，并在 stderr 说明缺少哪个授权。

## 3.6 输出与日志

- 默认：模型文本、工具调用轨迹、最终摘要输出到 stdout；进度、警告、日志输出到 stderr。
- TTY 且颜色开启时：
  - 模型正文交给 Glamour 渲染 Markdown（标题、列表、表格、代码高亮），并**按完整 Markdown 块流式输出**：段落遇到空行、代码围栏闭合即渲染，不等待整段回答。
  - 思考、工具调用、工具结果、提示分别使用紫/蓝/绿/红/灰的带左色块（`▌`）样式；连续工具行不插空行，过程块与结果块之间留空行。只使用前景/背景色，不使用闪烁 SGR。
  - 正文有 2 列缩进与 Markdown 排版，和贴左的过程块形成明显分界，长过程里也能快速定位答案。
  - 代码围栏与行内代码原样保留；代码外的 `<div>`、`Vec<T>` 等尖括号内容按字面量显示（Glamour 默认会清洗 HTML，直接渲染会丢失）。

```text
▌ 思考
▌ 先看 README 和 go.mod。

▌ 工具 read README.md
▌ 完成 read · 15ms

  # 这是一个什么项目
  `ant` 是一个 Go 语言实现的 AI Agent 框架。

▌ 完成 · mode=ask · model=ornith-1.5:9b · 1m34s
      节点：ask(9.3k in / 831 out)
      tokens：in 9.3k / out 831 / cached 2.1k / reasoning 0
      session：.ant/sessions/20261107-131015-5175f5
```

- 纯文本回退（非 TTY、`NO_COLOR`、`TERM=dumb` 或 `--no-color`）：正文原样输出 Markdown 源码，思考/工具退回 `[思考]`、`[tool]` 前缀，便于管道与日志消费。
- 推理模型：`reasoning` 增量合并为一个思考块输出到 stderr，不逐 chunk 换行；模型正文结束或工具调用前自动收尾并补换行。
- 工具轨迹：只展示关键参数（`command`/`path`/`target` 等，超长截断），不倾倒整段 JSON。
- `--json`：stdout 只输出 NDJSON 事件（[§9.4](07-session.md#94-ndjson-输出)），人类可读信息全部转到 stderr；`--verbose` 在 `--json` 下被忽略。
- `--verbose`：在默认工具轨迹之外，额外展示节点 prompt 版本/hash/输入 artifact、每轮请求/响应摘要、工具完整参数和结果预览；完整结果仍以 artifact 路径与 `ant export` 为准。
- `ant export`：离线查看完整链路，包含有效 prompt、请求消息、模型响应、工具调用/结果、节点输出与 usage；HTML 为单文件自包含报告。
- 颜色：`ui.color` 取 `auto`/`always`/`never`；`auto` 要求 TTY、未设置 `NO_COLOR`、`TERM != dumb`，`--no-color` 等价 `never`。Markdown 主题默认 `dark`，浅色终端可设 `GLAMOUR_STYLE=light`。
- 结束摘要至少包含：模式、模型、tokens（输入/输出/缓存/推理）、费用估算（如有定价）、耗时、改动文件、执行过的验证命令、session 路径。

## 3.7 信号与中断

- 第一次 SIGINT：取消当前模型请求或工具执行；把 `session_interrupted` 写入事件；REPL 保留在提示符；非交互退出码 3。
- 第二次 SIGINT（在提示符处）：退出进程（退出码 0 或 3，取决于是否有未完成 turn）。
- 工具子进程必须随上下文取消而终止（[§10.10](08-tools.md#1010-bash)）。
- 中断后再次运行 `ant` 会走恢复算法（[§9.6](07-session.md#96-断点续传算法)）。

## 3.8 示例

```bash
# 续最近 session（没有则新建）
ant "给 pkg/test.go 的 Test 增加边界检查"

# 指定模型继续
ant -p deepseek-v4-flash "继续，先跑 go test ./..."

# 新建 plan session 并进入 REPL
ant new -m plan

# 新建 plan session 并直接给出任务
ant new -m plan -p qwen3.7-flash_snap "设计 session 存储的断点续传方案"

# 只读问答
ant new -m ask "解释 read_code 的符号解析流程"

# 脚本/CI：非交互 + NDJSON
ant --non-interactive --json "运行 go test ./..."

# 查看历史
ant sessions --limit 20
ant show 20261030-094704

# 调试：实时查看工具结果，然后导出完整链路
ant new -m ask --verbose "解释 read_code 的符号解析流程"
ant export 20261030-094704 --format html
ant export -s 20261030-094704 --format md -o - > trace.md
```

> **设计理由**：原始 CLI 只给了 5 条命令，没有回答“续哪个 session、`new` 无 prompt 怎么办、如何脚本化、退出码是什么”。补全后，人工使用保持极简，CI 使用有确定行为。
>
> **备选方案**：引入 `ant run`/`ant resume` 显式子命令（未采用：破坏原始 `ant "prompt"` 的入口习惯）；用交互式 TUI（未采用：依赖重、跨平台成本高）。

---

[返回索引](../README.md) · [上一模块](00-overview.md) · [下一模块](02-config.md)
