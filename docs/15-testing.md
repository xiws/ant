> 模块 15 / 19 · [返回索引](../README.md) · [上一模块](14-engineering.md) · [下一模块](16-traceability.md)

# 17. 测试策略与验收

## 17.1 单元测试

| 模块 | 必测点 |
| --- | --- |
| `config` | 逐键合并、数组替换、`null`、CLI 覆盖、未知字段 warning、类型错误、密钥打码、`${ENV}` 展开、唯一性校验 |
| `session` | 事件折叠、尾行截断、缺失 tool 结果修复、恢复位置、锁 stale、原子写、schema 版本 |
| `context` | 预算公式、压缩阶段、tool_call 配对不变量、摘要失败兜底、pinned 消息 |
| `tool` | Schema 校验、类型转换、路径越界/符号链接/保护路径、截断、错误码 |
| `readcode` | 符号索引、歧义、循环、depth/maxSymbols、生成文件跳过、输出格式 |
| `edit` | `ErrNoHunks`、`PartialError` 不落盘、未变化、CRLF、hash 冲突、多文件拒绝 |
| `write` | 新建/覆盖、hash 前置条件、二进制拒绝、保护路径 |
| `bash` | 超时杀进程、非零退出码、输出截断、环境剥离、跨平台 shell |
| `search` | 字面量/正则、忽略规则、二进制跳过、上限 |
| `statemachine` | 四模式转移、轮次上限、无进展、Plan gate、模式切换 |
| `provider` | 流式累积、缺 id/name、断流、usage 映射、错误分类 |
| `projectctx` | hash 失效、标记保护、大小限制、失败兜底 |
| `approval` | 类别作用域、硬拒绝、`--yes`、非交互 |

## 17.2 Fake Provider

用标准库 `httptest` 实现可控的 OpenAI 兼容端点，覆盖：

- 普通文本流；
- 分片 `tool_calls`（按 index 交织）；
- 缺失 `id`/`name`；
- 429/500/401；
- 流中途断开；
- `finish_reason=length`；
- 非法 JSON 参数；
- `context_length_exceeded`；
- 非流式响应。

CI 不调用真实 API；所有测试用 fake provider。

## 17.3 集成与 e2e

- 临时 workspace（含 `go.mod`、`main.go`、测试文件）+ fake provider。
- 场景：
  1. code 模式：读取 → 修改 → verifier 执行 `go test` → 通过。
  2. verifier 失败 → 回 code → 修复 → 通过。
  3. SIGINT 中断 → 恢复 → 不重复已执行副作用。
  4. plan 模式：3 个 deliberate 并发 → aggregation → verifier → plan.md → Plan gate。
  5. council 模式：一轮评审 + 修复。
  6. 审批拒绝 → 模型收到错误 → 换方案。
  7. 上下文压缩后继续对话。
  8. 项目上下文生成与手改保护。
  9. 越界路径/符号链接被拒绝。
  10. 多平台路径与 shell。
- 需要 Go 工具链的测试在 CI 中标记；本地无 Go 时跳过并明确原因。

## 17.4 Golden / 快照测试

- prompt 组装结果（稳定前缀）快照。
- `read_code` 输出格式快照（含 `---` 分隔符）。
- session 事件 JSON 结构快照（字段只增不改）。
- CLI help/错误消息快照。
- 配置示例必须能被真实 loader 解析。

## 17.5 手工验收场景（文档层面）

阅读本文档后，应能按以下场景走通，不产生疑问：

1. 首次 `ant "..."`：新建 session、执行、生成 project 上下文。
2. 再次 `ant "..."`：续最近 session。
3. `ant new -m plan`：进入 REPL，规划 → 评审 → 验证 → plan.md → Plan gate。
4. 审批：越界读 → `n` 拒绝 → 模型换方案；`a` 本 session 放行同类。
5. Ctrl-C：中断 → 再次运行 → 恢复且不重放写操作。
6. Provider 429：退避重试 → 成功/失败提示。
7. 上下文超限：压缩 → 继续；压缩失败 → 明确报错。
8. `read_code`：输出格式与原始需求示例一致。
9. `edit`：部分 hunk 不匹配 → 不落盘、返回上下文、模型重试。
10. 项目上下文：手改标记外内容 → 重新生成不覆盖。

## 17.6 验收清单

- [ ] 原始 README 每个点都有归属章节（[§18](16-traceability.md#18-需求追溯矩阵)）。
- [ ] 每个设计点有「设计理由」；关键决策有「备选方案」。
- [ ] 所有 JSON 示例可解析；示例中的 key 已替换为环境变量。
- [ ] 术语一致：无 `defaultProvider`（兼容说明除外）、无 `node_name`、无 `Edit`/`readCode` 旧名混用。
- [ ] CLI、配置、事件、工具、节点输出契约前后一致。
- [ ] 无 TBD/待定；`otter` 作为唯一外部依赖有明确 fallback。
- [ ] 无真实密钥（`sk-` 明文）。
- [ ] 十个手工场景可走通。
- [ ] 已知问题（key 泄露、CI 错配）已记录。

> **设计理由**：本文档是设计规格，验收重点是“完整、一致、可实现”，而不是代码覆盖率。把 fake provider 与 e2e 场景写进规格，可以保证后续实现不会偏离 CLI/协议契约。
>
> **备选方案**：只做文档审阅（未采用：无法发现协议级不一致，例如流式分片与恢复配对）。

---

[返回索引](../README.md) · [上一模块](14-engineering.md) · [下一模块](16-traceability.md)
