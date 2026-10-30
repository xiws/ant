> 模块 9 / 19 · [返回索引](../README.md) · [上一模块](08-tools.md) · [下一模块](10-security.md)

# 11. 上下文与 Token 管理

## 11.1 预算公式

```text
inputBudget = contextWindow - maxOutputTokens - safetyMargin
safetyMargin = contextWindow * context.safetyMarginRatio   // 默认 5%
```

**固定部分上限**（超出时先压缩/降级，仍超出则报错）：

| 部分 | 上限 |
| --- | --- |
| 节点 prompt + 模式 overlay | 8K tokens |
| 用户 prompt（home + workspace） | 4K tokens |
| 项目上下文 | 8 KB（约 2–4K tokens，由 `project.maxOutputBytes` 约束） |
| 工具定义 | 2–5K tokens（按工具数量） |
| 压缩摘要 | 4K tokens |
| 最近消息 | 剩余预算 |

- 若固定部分已超过 `inputBudget` 的 60%，启动时 warning；超过 100% 则 `context_overflow` 并指出哪一部分过大。
- 每次请求前计算；不使用“等到报错再处理”。

## 11.2 Token 计数

- 优先使用 provider 返回的 usage：`prompt_tokens`、`completion_tokens`、`cached_tokens`、`reasoning_tokens`。
- 无 usage 时估算：
  - ASCII：约 4 字符/token；
  - CJK：约 1 字符/token（保守）；
  - 混合：分段估算后相加。
- 事件与摘要中标记 `estimated:true`，避免把估算当精确值。
- 上下文窗口以配置为准；若 provider 报 `context_length_exceeded`，按实际错误触发压缩并记录。

## 11.3 压缩算法

**触发**：请求前估算输入 > `inputBudget * compactAt`（默认 0.8），且 `context.autoCompact=true`。`autoCompact=false` 时直接 `context_overflow` 失败，保证确定性。

**阶段（按顺序，逐步升级）**：

1. **截断旧工具结果**：只处理当前 turn 之前的结果；保留 `assistant(tool_calls)` 消息与对应 `tool` 消息的配对，仅把 `tool` 消息内容替换为
   `{"truncated":true,"outputRef":"artifacts/...","note":"完整内容见 artifact"}`。
2. **摘要旧轮次**：用 `summaryModel` 把“保留窗口之外”的旧消息压成结构化摘要：

```json
{
  "goal": "用户最初/当前目标",
  "constraints": ["..."],
  "decisions": ["..."],
  "files_touched": ["..."],
  "commands_run": ["..."],
  "open_issues": ["..."],
  "next_steps": ["..."]
}
```

   摘要上限 `summaryMaxTokens`（4K）；摘要调用本身也受预算约束。
3. **丢弃更旧轮次**：保留系统提示、首个用户请求、project、当前 plan、摘要、最近 `keepRecentTurns` 轮。
4. **硬截断**：对仍然超大的单条工具输出按字节截断。
5. **失败**：仍超过 `inputBudget` → `context_overflow`，错误信息给出“缩小任务/开启新 session/调大 contextWindow”的建议。

**摘要失败兜底**：summary 模型调用失败时，用确定性摘要（用户请求列表 + 改动文件 + 执行命令 + 未解决标记）替代，不阻塞主流程。

**压缩事件**：

- 写 `context_compacted`，包含 `beforeTokens`、`afterTokens`、`summaryRef`、被替换的消息范围。
- 事件日志本身不删除；压缩只影响发送给模型的消息，不篡改历史。

## 11.4 不可破坏的不变量

1. `assistant` 的 `tool_calls` 消息与其全部 `tool` 结果必须作为整体保留或整体替换；不得只删其中一半。
2. 当前 turn 的用户请求、当前 plan、未解决的 verifier failures 必须保留（pinned）。
3. 压缩不能改变已完成 step 的语义；恢复时以事件日志为准，不以压缩后的消息为准。
4. 摘要必须标注来源 turn 范围，便于追溯。
5. 任何截断都要在消息中留下引用或说明，不能静默丢内容。

## 11.5 跨节点上下文

不把完整 transcript 传给每个节点，而是传“节点输入包”：

| 节点 | 输入包 |
| --- | --- |
| `code` | 用户请求、session 摘要、plan、上一轮 failures、相关 artifact |
| `ask`（plan） | 用户请求、摘要、项目上下文、可读代码 |
| `deliberate` | 计划/改动、用户请求、项目上下文、维度指令（**不含**其他 deliberate 输出） |
| `aggregation` | 三份 deliberate 输出 + 计划/改动 + 用户请求 |
| `verifier` | 计划/改动、aggregation 结果、用户验收规则、项目上下文 |

> **设计理由**：多节点模式天然放大上下文；跨节点传“蒸馏后的输入包”比传全量 transcript 更省 token，也避免一个节点的错误推理污染另一个节点。deliberate 之间隔离还能保留观点独立性。
>
> **备选方案**：所有节点共享同一份完整历史（未采用：token 爆炸、相互 Anchoring、缓存前缀频繁变化）。

## 11.6 Prompt 缓存与用量

- 稳定前缀顺序见 [§6.1](04-prompt.md#61-组装顺序稳定-易变)；动态信息放尾部。
- 工具定义按名称排序，JSON 字段顺序稳定；不因 map 遍历随机化。
- 项目上下文按 hash 复用；压缩摘要变化时才更新。
- Ollama：`keep_alive` 默认 `30m`；避免每次冷加载。
- DeepSeek/Qwen：隐式前缀缓存；usage 有 `cached_tokens` 时报告命中量，便于评估 prompt 结构。
- 不在 system prompt 注入时间戳/随机数/进度；这些放最新 user 消息或末尾片段。

## 11.7 成本与预算

- 按 `models[].pricing` 估算：
  - `inputCost = (input - cached)/1e6 * inputPer1M + cached/1e6 * cachedInputPer1M`；
  - `cachedInputPer1M` 未提供时，缓存部分按 `inputPer1M` 计价；
  - `outputCost = output/1e6 * outputPer1M`。
- 无 pricing 时只报告 token，不报告金额。
- turn 结束打印：per-node、per-model 的 input/output/cached/reasoning、费用、耗时。
- `limits.maxSessionTokens`：
  - 0 = 不限；
  - 达到 80% warning；
  - 达到 100% 优雅中止当前 turn，保留 session 与 checkpoint，退出码 1。
- 压缩、重试、fan-out 都会增加成本；turn 摘要中分别列出，便于定位“钱花在哪”。

> **设计理由**：原始需求只问了“是否需要自动压缩、如何利用缓存”。答案是：需要，但压缩必须有不可破坏的协议不变量；缓存靠稳定前缀而不是特殊 API。成本可视化和预算上限让长任务可控。
>
> **备选方案**：超限直接报错（未采用：体验差；保留 `autoCompact=false` 给需要确定性的用户）；只在客户端缓存完整 prompt（未采用：服务端前缀缓存才是主要收益来源）。

---

[返回索引](../README.md) · [上一模块](08-tools.md) · [下一模块](10-security.md)
