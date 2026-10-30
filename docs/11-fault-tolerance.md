> 模块 11 / 18 · [返回索引](../README.md) · [上一模块](10-security.md) · [下一模块](12-project-context.md)

# 13. 容错、重试与幻觉控制

## 13.1 错误分类

| 类别 | 典型原因 | 是否重试 | 退出码/行为 |
| --- | --- | --- | --- |
| `config_invalid` | 配置类型/引用错误 | 否 | 2，指出文件与字段 |
| `provider_auth` | 401/403 | 否 | 5，提示检查 key/权限 |
| `provider_rate_limit` | 429 | 是（退避） | 5（最终失败） |
| `provider_unavailable` | 5xx/网络/超时 | 是（退避） | 5（最终失败） |
| `provider_invalid_request` | 400 | 否 | 5，打印请求摘要（不含密钥） |
| `context_overflow` | 输入超窗口 | 压缩后再试 | 仍失败则 1 |
| `tool_invalid_args` | 参数 JSON/Schema | 模型自愈 | 工具错误回传 |
| `tool_timeout` | 工具超时 | 只读 1 次 | 工具错误回传 |
| `tool_failed` | 工具内部错误 | 否 | 工具错误回传 |
| `approval_denied` | 用户拒绝 | 否 | 工具错误回传 |
| `verification_failed` | verifier 不通过 | 按轮次 | 1（达上限） |
| `loop_detected` | 重复调用/无进展 | 否 | 1，保留 checkpoint |
| `interrupted` | SIGINT | 用户决定 | 3 |
| `internal` | 未分类 | 否 | 1，带 session id |

## 13.2 模型请求重试

| 参数 | 默认 |
| --- | --- |
| 总尝试次数 | 3 |
| 退避 | 1s、2s、4s |
| 抖动 | 20% |
| 单次最大等待 | 30s |
| 可重试 | `408`、`429`、`500`、`502`、`503`、`504`、网络错误、流在 `finish_reason` 前中断 |
| 不重试 | `400`、`401`、`403`、`404`、内容过滤、配置错误 |

**例外**：部分 provider 用 400 返回 `context_length_exceeded`。适配器必须优先识别这类错误并归类为 `context_overflow`（压缩后重试），不要当作 `provider_invalid_request` 直接失败。

- 尊重 `Retry-After`（若存在且不超过上限）。
- 每次重试写事件（`attempt`、`delayMs`、`errorClass`）。
- 只在**没有执行任何工具**时重试模型调用；工具副作用不重放。
- 超过次数 → 归类为 provider 错误，按 `fallbackModels`（若启用）或退出码 5。

## 13.3 流式异常

- 流在 tool_calls 累积中途断开：丢弃该批调用，按 [§13.2](#132-模型请求重试) 重试；绝不执行半截参数。
- 缺少 `id` 或 `name`：整批丢弃并重试；连续两次则报 `provider_invalid_request`。
- `finish_reason=length`：输出被截断；记录事件，提示模型继续或缩小范围；不把截断内容当完整结果。
- Provider 不支持流式：自动走非流式，行为一致。
- 客户端取消：立即中断 HTTP 请求并写 `session_interrupted`。

## 13.4 JSON 修复与类型转换

**修复顺序**（仅对工具参数与结构化节点输出）：

1. 去掉 Markdown 代码围栏（```json ... ```）。
2. 提取首个平衡的 `{...}` 对象。
3. 删除尾随逗号（有限、逐字符扫描，不改变字符串内部）。
4. 尝试 `json.Unmarshal`。
5. Schema 校验 + 类型转换（[§10.4](08-tools.md#104-参数校验与类型转换)）。
6. 仍失败：把错误、原始文本、schema 摘要作为工具结果回传模型，要求重试。

**限制**：

- 不做“语义猜测”（例如把自然语言改写成 JSON）。
- 不做危险修复（不补全缺失的括号/引号以外的大段内容）。
- 同一工具连续 2 次参数错误 → 中止该节点并报告，避免无限自愈。
- 结构化节点输出（verifier/deliberate/aggregation）修复失败 → 按 `fail` 处理并保留原文。

## 13.5 循环与无进展检测

| 检测 | 阈值 | 行为 |
| --- | --- | --- |
| 相同工具调用指纹 | 3 次 | warning，提示模型换方案 |
| 相同工具调用指纹 | 5 次 | `loop_detected`，中止节点 |
| 单节点工具轮数 | 12 | 中止节点 |
| 模式轮次 | 3 | 按失败处理 |
| code 无 write/edit 且 verifier 失败 | 1 轮 | 中止并说明“无进展” |
| 单 turn token 上限 | 配置 | 中止并保留 session |

**指纹**：`tool name + 规范化 JSON 参数`（key 排序、去除空白）。只读工具重复查询同一内容也算循环。

## 13.6 降级与回退

- `fallbackModels` 默认关闭；启用后仅在 `provider_unavailable`/`provider_rate_limit` 且能力满足时切换。
- 切换前检查 `supportsTools`/上下文窗口；不满足则跳过。
- 记录 `model_fallback` 事件（[§9.3](07-session.md#93-事件模型)）与原因。
- 不因“回答质量差”自动换模型；质量由 verifier/用户决定，避免费用不可预测。

## 13.7 崩溃、中断与恢复

- 每个 step 完成即写事件；SIGINT 优雅落盘并释放锁。
- 恢复算法见 [§9.6](07-session.md#96-断点续传算法)；副作用工具不静默重放。
- 恢复后先修复 `tool_call_id` 配对，再发下一次模型请求。
- 最坏情况丢失最后一行事件；不会把半行 JSON 当有效状态。
- 磁盘写失败：中止 turn，保留已有事件，提示检查磁盘/权限。

## 13.8 幻觉控制

- `verifier` 必须给出证据（命令、退出码、file:line、代码引用），否则视为无效 verdict。
- `code` 不得声称“已测试/已通过”而未实际执行；引擎记录真实 `checks`。
- `read_code` 提供真实代码切片，减少“凭记忆编造 API”。
- `deliberate` 专门找漏洞；`aggregation` 保留 provenance，防止结论无来源。
- 不确定时输出“未验证/未知”，而不是补全一个看似合理的答案。
- 建议对高风险任务为 `verifier` 配置不同模型（`nodeModels.verifier`），降低同模型自我确认偏差。

## 13.9 用户可见错误

错误信息必须包含：

1. 发生了什么（分类 + 简述）。
2. 为什么（provider/工具/配置的具体原因）。
3. 怎么修（可执行建议）。
4. 定位信息（session id、日志路径、相关 artifact）。
5. 退出码含义。

默认不打印堆栈；`--log-level debug` 或日志文件保留详细堆栈。

> **设计理由**：原始需求把“容错”列为现状，但没有策略。重试必须区分幂等性，否则一次超时可能重复执行写操作；JSON 修复必须有限，否则会掩盖模型的结构性错误。
>
> **备选方案**：所有错误统一重试（未采用：重复副作用、放大费用）；自动切换模型直到成功（未采用：成本与质量不可控）。

---

[返回索引](../README.md) · [上一模块](10-security.md) · [下一模块](12-project-context.md)
