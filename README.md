# ant 需求与设计规格

| 项 | 内容 |
| --- | --- |
| 版本 | v1.2（设计规格，待实现；文档按模块拆分） |
| 仓库 | `github.com/xiws/ant` |
| 基线 | Go 1.26.8；macOS / Linux / Windows |
| 文档目的 | 把原始需求补全为可直接指导实现、可验收的设计规格 |
| 与原始 README 的关系 | 保留全部原始要点；冲突处以本文档为准，见 [追溯矩阵](docs/16-traceability.md) 与 [ADR](docs/17-adr.md) |
| 需求编号 | `R-<领域>-<序号>` |
| 决策编号 | `D-<序号>`，汇总见 [ADR](docs/17-adr.md) |
| 规范用语 | **必须** = MUST，**应该** = SHOULD，**可以** = MAY |
| 结构 | 本 README 为索引；正文按 20 个模块（0–19）拆分在 `docs/` |

本文档是原始 README 的补全版，也是后续实现的唯一事实来源。实现代码尚未开始。原始 3,000 行长文已按模块拆分，本页只保留索引、阅读路径和关键提示。

## 安全提示

- `otter` 适配器缺少公开协议，标记为实验性；未实现时明确报错。

## 模块索引

| # | 模块 | 文件 | 内容 |
| --- | --- | --- | --- |
| 0 | 总览与架构 | [00-overview.md](docs/00-overview.md) | 术语、定位、原则、架构与数据流 |
| 1 | CLI 规格 | [01-cli.md](docs/01-cli.md) | 命令、参数、退出码、交互、信号 |
| 2 | 配置系统 | [02-config.md](docs/02-config.md) | 配置位置、优先级、合并、models/settings、密钥 |
| 3 | 模型与 Provider | [03-models.md](docs/03-models.md) | 模型别名、能力校验、Provider 适配器、8 个协议不变量 |
| 4 | Prompt 体系 | [04-prompt.md](docs/04-prompt.md) | Prompt 分层、覆盖、缓存、版本 |
| 5 | Agent Node | [05-nodes.md](docs/05-nodes.md) | 节点抽象、权限矩阵、输出契约、并发 |
| 6 | 状态机与模式 | [06-state-machine.md](docs/06-state-machine.md) | code/plan/ask/council、轮次、Plan gate |
| 7 | Session 与断点续传 | [07-session.md](docs/07-session.md) | 事件模型、断点续传、锁、原子性 |
| 8 | 工具系统 | [08-tools.md](docs/08-tools.md) | read/read_code/search/edit/write/bash/ask |
| 9 | 上下文与 Token 管理 | [09-context.md](docs/09-context.md) | 预算、压缩、跨节点上下文、成本 |
| 10 | 安全与审批 | [10-security.md](docs/10-security.md) | workspace 约束、审批分级、硬拒绝、注入 |
| 11 | 容错、重试与幻觉控制 | [11-fault-tolerance.md](docs/11-fault-tolerance.md) | 重试、JSON 修复、循环、降级、崩溃恢复 |
| 12 | 项目级上下文自动初始化 | [12-project-context.md](docs/12-project-context.md) | 扫描、生成、失效、手改保护 |
| 13 | 可观测性与成本 | [13-observability.md](docs/13-observability.md) | 日志、NDJSON、turn 摘要、调试 |
| 14 | 工程实现约束 | [14-engineering.md](docs/14-engineering.md) | 包结构、依赖、跨平台、CI、版本 |
| 15 | 测试策略与验收 | [15-testing.md](docs/15-testing.md) | 单测、fake provider、e2e、验收 |
| 16 | 需求追溯矩阵 | [16-traceability.md](docs/16-traceability.md) | 原需求 → 章节 → 决策 |
| 17 | 设计决策记录（ADR） | [17-adr.md](docs/17-adr.md) | D1–D43 设计决策记录 |
| 18 | 附录 | [18-appendix.md](docs/18-appendix.md) | 配置示例、错误码、CLI 帮助、已知问题 |
| 19 | 模块接口与串联（§21） | [19-interfaces.md](docs/19-interfaces.md) | 各模块对外接口、组合根装配、七条调用链与 10 张图 |

## 阅读路径

| 目的 | 建议顺序 |
| --- | --- |
| 快速了解 | [00](docs/00-overview.md) → [01](docs/01-cli.md) → [05](docs/05-nodes.md) → [06](docs/06-state-machine.md) |
| 实现 | [19](docs/19-interfaces.md) → [00](docs/00-overview.md) → [02](docs/02-config.md) → [03](docs/03-models.md) → [05](docs/05-nodes.md) → [06](docs/06-state-machine.md) → [07](docs/07-session.md) → [08](docs/08-tools.md) → [09](docs/09-context.md) → [10](docs/10-security.md) → [11](docs/11-fault-tolerance.md) |
| 评审 | [00](docs/00-overview.md) → [05](docs/05-nodes.md) → [06](docs/06-state-machine.md) → [08](docs/08-tools.md) → [10](docs/10-security.md) → [15](docs/15-testing.md) → [17](docs/17-adr.md) |
| 追溯原需求 | [16](docs/16-traceability.md) |
| 接口对接/并行开发 | [19](docs/19-interfaces.md) → [05](docs/05-nodes.md) → [08](docs/08-tools.md) → [07](docs/07-session.md) |

## 核心决策速览

| 决策 | 一句话 |
| --- | --- |
| D3 | `ant` 续最近 session；`new` 强制新建；模式切换需确认 |
| D6 | 配置逐键深合并 |
| D9 | 规范键 `defaultModel`，兼容 `defaultProvider` |
| D12 | Prompt 稳定前缀 → 易变尾部，最大化缓存 |
| D14 | verifier/deliberate/aggregation 输出 JSON 契约 |
| D18 | Session 事件溯源 + artifact，可恢复/审计 |
| D23 | `edit` 部分 hunk 匹配失败且不落盘 |
| D32 | 文件工具全部 workspace 约束 + 审批 + 硬拒绝 |
| D43 | 模块间以内部接口对接，组合根集中装配 |

完整 D1–D43 见 [ADR](docs/17-adr.md)。

## 编号与导航

- 章节编号沿用原稿：`§N` / `§N.M`。
- 每个模块顶部和底部都有「返回索引 / 上一模块 / 下一模块」。
- 模块内对 `§` 的引用已转换为可点击链接；跨模块引用会跳转到对应文件。
- 新增/修改设计时，请同步更新对应模块与 [ADR](docs/17-adr.md)，不要只改实现。
- 第 19 号文档使用 §21 编号并排在附录之后，以免重排既有 §1–§20 与交叉引用（见其文首“编号说明”）。

## 实现顺序与结语

本文档把原始 README 中的设想、示例和问题统一为可实现的契约。实现阶段的顺序建议：

0. 按 [§21](docs/19-interfaces.md) 落 `internal/` 接口骨架与组合根（`cli.New`），先让 fake 实现跑通空 turn。
1. `config` + `model` + `provider`（fake provider 测试）。
2. `session` 事件与恢复。
3. `tool` 基础工具（read/search/bash/ask）。
4. `read_code` + `edit`/`write`。
5. `agent` + `statemachine` 四模式。
6. `context` 压缩、项目初始化、审批与可观测性。
7. 跨平台与 CI。

任何偏离本文档的设计，应在 [ADR](docs/17-adr.md) 增加一条决策，而不是静默修改实现。

## 变更记录

见 [附录 · 20.6 变更记录](docs/18-appendix.md#206-变更记录)。
