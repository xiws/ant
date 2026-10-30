> 模块 4 / 18 · [返回索引](../README.md) · [上一模块](03-models.md) · [下一模块](05-nodes.md)

# 6. Prompt 体系

## 6.1 组装顺序（稳定 → 易变）

| 顺序 | 内容 | 来源 | 是否每轮变化 |
| --- | --- | --- | --- |
| 1 | 节点基础 prompt | 内置 `prompts/<node>.md` | 否 |
| 2 | 模式 overlay（状态机指令、输出契约） | 内置 `prompts/modes/<mode>.md` | 否 |
| 3 | 用户全局 prompt | home `.ant/prompt.md` | 否 |
| 4 | 用户项目 prompt | workspace `.ant/prompt.md` | 否 |
| 5 | 项目上下文 | `.ant/project.md`（≤ 8KB） | hash 变化时 |
| 6 | 工具定义 | 运行时生成，按名称排序 | 否 |
| 7 | Session 压缩摘要 | 事件日志中的 `context_compacted` | 压缩时 |
| 8 | 最近 N 轮消息 | session | 是 |
| 9 | 运行时信息 | cwd、OS、shell、UTC 日期、审批状态 | 是 |

**运行时信息放置**：放在最新一条 user 消息之前或作为独立的尾部 system 片段，**不要**放在稳定前缀开头。

## 6.2 文件与覆盖

| 内容 | 位置 | 覆盖关系 |
| --- | --- | --- |
| 节点 prompt | 内置 `internal/agent/prompts/<node>.md` | workspace `.ant/prompts/<node>.md` > home `.ant/prompts/<node>.md` > 内置 |
| 模式 overlay | 内置 `internal/agent/prompts/modes/<mode>.md` | **不允许**覆盖（输出契约与状态机解析绑定） |
| 用户全局 prompt | home `.ant/prompt.md` | 追加到内置之后 |
| 用户项目 prompt | workspace `.ant/prompt.md` | 追加在 home 之后（项目更具体） |
| 项目上下文 | `.ant/project.md` | 自动生成，受 [§14](12-project-context.md#14-项目级上下文自动初始化) 手改保护 |

- 节点覆盖文件缺失时静默回落，不报错。
- 覆盖文件为空时视为“显式清空该节点 prompt”（warning）。
- 模式 overlay 若允许覆盖，用户可能删掉 verifier 的 JSON 契约，导致状态机无法解析；因此不可覆盖，需要调整时通过配置或升级。

## 6.3 缓存友好的结构

- 稳定前缀只包含不随轮次变化的内容；工具定义按名称排序，JSON 字段顺序稳定。
- 不在 system prompt 中注入当前时间、随机 ID、进度百分比。
- 项目上下文按 hash 复用，不每轮重新生成。
- 压缩摘要替换旧消息时，保持其后的最近消息不变，减少前缀抖动。
- Ollama：请求中设置 `keep_alive`（默认 `30m`），避免模型反复加载。
- DeepSeek/Qwen：利用其隐式前缀缓存，不需要额外字段；usage 中若返回 `cached_tokens`，写入事件并展示。
- 未来若接入显式缓存协议（如 Anthropic prompt caching），在 Provider 层增加 cache breakpoint，不改变本文档的分层顺序。

## 6.4 版本与可复现

- 每个内置 prompt 文件带版本号（文件头注释或 manifest）。
- 每次 `model_request` 事件记录：节点名、prompt 版本、组装后 prompt 的 hash、工具定义 hash。
- 调试时可用 `ant show <id> --events` 查看某次请求使用了哪个版本（不默认打印全文）。

## 6.5 语言

- 面向用户文案、默认 prompt：中文。
- 工具名、JSON 字段、schema：英文。
- 用户覆盖文件可以使用任意语言；不强制翻译。

## 6.6 组装示意

```text
[system]
<node base prompt>                       # 稳定
<mode overlay>                           # 稳定
<home prompt.md>                         # 稳定
<workspace prompt.md>                    # 稳定
<project.md>                             # 稳定（按 hash）
<tool definitions>                       # 稳定（按名称排序）
<session summary>                        # 压缩后更新
<recent messages>                        # 变化
<runtime info: cwd/os/shell/date/approval>  # 变化，放尾部

[user]
<本次输入>
```

> **设计理由**：把稳定内容放前面、易变内容放后面，才能命中 DeepSeek/Qwen 的隐式前缀缓存与 Ollama 的 prompt cache；时间戳若放在系统提示开头会每轮击穿缓存。模式 overlay 不允许覆盖，是为了保证状态机能解析节点输出。
>
> **备选方案**：每轮重新拼一份完整 prompt（未采用：缓存全失效、成本高）；允许覆盖模式 prompt（未采用：破坏输出契约）。

## 6.7 示例

`.ant/prompt.md`：

```markdown
# 项目约定
- 修改 Go 代码后必须运行 `go test ./...`。
- 不要新增第三方依赖，除非先征得同意。
```

`.ant/prompts/verifier.md`（覆盖内置 verifier）：

```markdown
在原有验证规则基础上，额外检查 pkg/ 下是否有未使用的导出符号。
必须保持 §7.4.1 的 JSON 输出契约。
```

> 覆盖 `verifier`/`deliberate`/`aggregation` 时，必须保留对应 JSON 输出契约；否则状态机无法解析。

---

[返回索引](../README.md) · [上一模块](03-models.md) · [下一模块](05-nodes.md)
