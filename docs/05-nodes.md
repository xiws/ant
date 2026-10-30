> 模块 5 / 19 · [返回索引](../README.md) · [上一模块](04-prompt.md) · [下一模块](06-state-machine.md)

# 7. Agent Node

## 7.1 抽象

节点是“独立 prompt + 工具权限 + 输出契约”的执行单元，通过 `name` 区分。原始需求中的“抽象出一个节点，节点通过 name 来命名”落地为：

```go
type Node interface {
    Name() string
    Tools() []string
    Run(ctx context.Context, in NodeInput) (NodeOutput, error)
}

type NodeInput struct {
    Request        string        // 用户本轮请求
    SessionSummary string        // 压缩摘要（如有）
    Plan           string        // plan 模式产物（如有）
    Artifacts      []ArtifactRef // 代码片段、验证失败、diff 等
    Feedback       []Failure     // 上一轮失败点
    Dimension      string        // 仅 deliberate 使用
}

type NodeOutput struct {
    Text       string          // 面向用户/下一节点的文本
    Structured json.RawMessage // verifier/deliberate/aggregation 必须提供
    Usage      Usage
}
```

引擎负责：组装 prompt、执行工具循环、解析结构化输出、写事件。节点本身不直接读写 session。

## 7.2 节点清单

| 节点 | 工具 | 输出 |
| --- | --- | --- |
| `code` | read, read_code, search, edit, write, bash, ask | 变更摘要 + 已执行命令 + 未解决问题（文本尾部有固定小节） |
| `ask` | read, read_code, search, bash, ask | 回答或 plan 草稿 + `file:line` 引用 |
| `verifier` | read, read_code, search, bash, ask | 结构化 verdict（[§7.4.1](#741-verifier)） |
| `deliberate` | read, read_code, search, bash, ask | 结构化 issues（[§7.4.2](#742-deliberate)） |
| `aggregation` | read, read_code, search, bash, ask | 合并计划 + 去重 issues + provenance（[§7.4.3](#743-aggregation)） |

**内部模型调用**（不是 agent 节点，不暴露工具）：

- `summary`：上下文压缩（[§11.3](09-context.md#113-压缩算法)）。
- `project_init`：项目上下文生成（[§14.3](12-project-context.md#143-生成流程)）。

## 7.3 权限矩阵

| 工具 | code | ask | verifier | deliberate | aggregation |
| --- | --- | --- | --- | --- | --- |
| `read` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `read_code` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `search` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `edit` | ✅ | ❌ | ❌ | ❌ | ❌ |
| `write` | ✅ | ❌ | ❌ | ❌ | ❌ |
| `bash` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `ask` | ✅ | ✅ | ✅ | ✅ | ✅ |

- 只有 `code` 节点能改文件；其他节点即使模型请求 `write`/`edit`，工具运行时也返回 `tool_not_allowed`。
- `bash` 虽然对所有节点开放，但风险命令仍走审批（[§12.3](10-security.md#123-审批分级)）；验证节点执行构建/测试是预期用途。
- 非 `code` 节点的 bash 采用**只读策略**：重定向写文件、`rm`/`mv`/`cp` 修改源码、`sed -i`、`git` 写操作等需要审批；审批通过后执行，但事件标注节点，且不视为计划/验证的一部分。
- 权限矩阵由引擎注入，不依赖 prompt 自律。

## 7.4 输出契约

状态机能否自动转移，取决于输出是否可解析。文本节点（code/ask）允许自由文本，但必须包含固定小节；其余节点必须输出 JSON。

### 7.4.1 verifier

```json
{
  "pass": false,
  "summary": "编译失败，Test 缺少边界检查",
  "failures": [
    {
      "severity": "blocker",
      "file": "pkg/test.go",
      "evidence": "go test ./... 退出码 1：pkg/test.go:12: undefined: x",
      "suggestion": "补上 x 的定义或修正引用"
    }
  ],
  "checks": [
    {
      "cmd": "go test ./...",
      "exitCode": 1,
      "outputRef": "artifacts/step-3-go-test.out"
    }
  ]
}
```

规则：

- `pass=true` 时 `failures` 必须为空；`pass=false` 时至少一条 failure。
- `severity` 取 `blocker`/`major`/`minor`。
- 必须给出 `evidence`（命令、退出码、file:line 或代码引用），不能只写“看起来不对”。
- `checks` 记录实际执行过的命令与退出码；未执行的检查不得写入。
- JSON 解析失败：一次修复请求 → 仍失败则以 `pass=false` 处理，并把原文放入 `summary`。

### 7.4.2 deliberate

```json
{
  "dimension": "risk",
  "issues": [
    {
      "severity": "major",
      "file": "pkg/session/store.go",
      "evidence": "恢复时未校验 tool_call_id 配对，可能触发 400",
      "suggestion": "启动时合成 interrupted tool 消息"
    }
  ],
  "questions": ["并发写同一 session 时锁策略是什么？"],
  "confidence": "medium"
}
```

### 7.4.3 aggregation

```json
{
  "plan": "合并后的计划正文（Markdown）",
  "issues": [
    {
      "id": "I1",
      "severity": "major",
      "description": "恢复流程缺少协议修复",
      "suggestion": "在恢复算法第 5 步补 interrupted 结果",
      "from": ["correctness", "risk"]
    }
  ],
  "missing_dimensions": [],
  "provenance": {
    "I1": ["correctness", "risk"]
  }
}
```

规则：去重、按严重度排序、保留来源；不得丢弃任一维度的 blocker；缺维度时写入 `missing_dimensions` 并说明。

### 7.4.4 code / ask

- `code` 最终文本必须包含：`## 变更`、`## 已执行`、`## 未解决`（无内容写“无”）。
- `ask` 在 plan 模式下必须包含计划小节：目标、范围（含不做）、现状与约束、实施步骤、风险与缓解、验证与验收、未决问题；在 ask 模式下必须给 `file:line` 引用（无法引用时明确说明“未读取代码”）。
- 文本输出不参与状态机判定；判定由 verifier 完成。

## 7.5 deliberate 的三个维度

| 维度 | 关注点 | 典型问题 |
| --- | --- | --- |
| `correctness` | 需求符合、技术正确、依赖与前置条件 | 是否漏掉用户明确要求？方案是否可行？ |
| `risk` | 边界、失败模式、安全、回归、兼容性 | 并发/崩溃/越权/旧行为是否被破坏？ |
| `execution` | 步骤可执行、可测试、验收、回滚、工作量 | 每一步能否落地？怎么证明完成？ |

- 三个实例并发执行，各自独立上下文，**互不可见**，避免相互 Anchoring。
- 每个实例拿到同一份 plan/请求/项目上下文，外加自己的维度指令。
- 输出必须带 `dimension` 字段；引擎校验与请求维度一致。

## 7.6 fan-out 与 join

- fan-out 上限 3；受 provider `maxConcurrency` 约束时排队而非失败。
- join 后按固定维度顺序 `correctness → risk → execution` 传给 aggregation/verifier，保证 prompt 确定性。
- 单个 deliberate 失败：重试 1 次；仍失败则记录缺失维度，aggregation 继续。
- 两个及以上失败：plan/council 模式中止当前轮，session 标记 `failed`，保留已有产物。
- 所有 deliberate 都不得调用 `edit`/`write`；只允许读取与执行只读命令。

## 7.7 预算与重试

| 项 | 默认 | 说明 |
| --- | --- | --- |
| 节点总超时 | 15 min | 含模型与工具 |
| 单节点工具轮数 | 12 | `limits.maxToolTurns` |
| 模型错误重试 | 1 次（节点内） | 仅网络/限流/5xx；provider 层已有 3 次退避 |
| 工具重试 | 只读 1 次 | write/edit/bash 不自动重试 |
| token 预算 | 由上下文预算推导 | 超限走压缩，仍超则失败 |

> 原始需求建议 `max_turns=5`；实际“读→改→测”常超过 5。默认 12，并同时受 token 预算、循环检测和模式轮次约束，可按项目调低。

## 7.8 节点模型选择

- 按 [§5.1](03-models.md#51-模型解析) 解析；`--node-model` > `--model` > `settings.nodeModels` > `defaultModel`。
- 启动时对每个将运行的节点做能力校验；不满足则在**开始执行前**报错，避免跑到一半失败。
- `nodeModels` 中引用不存在的节点名：warning；引用不存在的模型：fatal。

## 7.9 失败处理

- 节点失败必须产生 `node_completed`（带 `status=failed`）或 `session_failed` 事件，不能静默退出。
- 失败信息包含：节点、错误分类、最后一次模型输出摘要、失败工具与退出码、session id、续跑命令。
- 已完成步骤不重跑；恢复从失败步骤开始（[§9.6](07-session.md#96-断点续传算法)）。

> **设计理由**：状态机的自动流转必须依赖机器可判定的结果；如果 verifier 输出散文，引擎只能再让一个模型去“理解”，成本和不确定性都会翻倍。代码/问答节点保留自然语言以提升可读性，但验证与评审节点使用 JSON。
>
> **备选方案**：所有节点统一 JSON（未采用：code/ask 的可读性差，且模型容易为了格式牺牲内容）；依赖 provider 的 JSON mode（未采用：并非所有 OpenAI 兼容实现支持）。

---

[返回索引](../README.md) · [上一模块](04-prompt.md) · [下一模块](06-state-machine.md)
