> 模块 16 / 19 · [返回索引](../README.md) · [上一模块](15-testing.md) · [下一模块](17-adr.md)

# 18. 需求追溯矩阵

下表把原始 README 的每个部分映射到本文档章节。状态说明：**保留** = 原意不变；**扩展** = 补全细节；**修正** = 原文存在冲突或安全问题。

| 原始需求（原文） | 本文档 | 决策 | 状态 |
| --- | --- | --- | --- |
| `ant "prompt"` 续最近 session / 新建 | [§3.2](01-cli.md#32-命令) | D3 | 扩展 |
| `ant -p providerNameCode "prompt"` | [§3.2](01-cli.md#32-命令)、[§5.1](03-models.md#51-模型解析) | D4 | 修正（值是模型别名） |
| `ant sessions` | [§3.2](01-cli.md#32-命令)、[§9](07-session.md#9-session-与断点续传) | D3 | 扩展 |
| `ant new` | [§3.2](01-cli.md#32-命令)、[§3.5](01-cli.md#35-交互与-repl) | D3 | 扩展 |
| `ant new --mode plan -p ... "prompt"` | [§3.3](01-cli.md#33-参数) | D4 | 扩展 |
| `ant new -m plan` | [§3.3](01-cli.md#33-参数)、[§3.5](01-cli.md#35-交互与-repl) | D4 | 修正（统一 `-m/--mode`；无 prompt 进 REPL） |
| 配置优先级 cli > workspace > home | [§4.2](02-config.md#42-优先级与合并) | D6 | 扩展（逐键合并） |
| session 文件 `workspace/.ant/sessions` | [§9.1](07-session.md#91-目录布局) | D18 | 扩展（每 session 目录） |
| 状态机断点续传、节点输入/输出/工具/Token | [§9.3](07-session.md#93-事件模型)、[§9.5](07-session.md#95-每个节点记录什么)、[§9.6](07-session.md#96-断点续传算法) | D18、D19 | 扩展 |
| `models.json` provider/model 结构 | [§4.4](02-config.md#44-modelsjson-schema)、[§5](03-models.md#5-模型与-provider) | D10、D11 | 扩展 |
| `settings.json` 的 `defaultProvider` | [§4.5](02-config.md#45-settingsjson-schema)、[§5.1](03-models.md#51-模型解析) | D9 | 修正（`defaultModel` 优先，兼容旧键） |
| `settings.json` 的 `node_name` | [§4.5](02-config.md#45-settingsjson-schema)、[§7.8](05-nodes.md#78-节点模型选择) | D39 | 修正（改为 `nodeModels` 映射） |
| `prompt.md` | [§6](04-prompt.md#6-prompt-体系) | D12 | 扩展 |
| OpenAI API 兼容 | [§5.3](03-models.md#53-provider-接口)、[§5.4](03-models.md#54-openai-completions-协议) | D10、D11 | 扩展 |
| 8 个工具调用坑 | [§5.7](03-models.md#57-openai-工具调用协议不变量原始-8-个坑)、[§13](11-fault-tolerance.md#13-容错重试与幻觉控制) | D40、D29 | 扩展为约束 |
| `readCode` 方法依赖切片 | [§10.6](08-tools.md#106-read_code) | D41、D21 | 扩展（算法/边界/格式） |
| 节点权限中的 `read`（原文工具章节未定义） | [§10.5](08-tools.md#105-read) | D21 | 修正（新增 `read`） |
| 工具组一起执行 | [§10.2](08-tools.md#102-并发策略) | D22 | 扩展（读并发/写串行 + id 绑定） |
| `Edit` + hunkpatch | [§10.8](08-tools.md#108-edit) | D23 | 扩展（`PartialError` 不落盘） |
| `write` 新建/整体重写 | [§10.9](08-tools.md#109-write) | D24 | 扩展（hash 前置条件） |
| `bash` 支持 mac/Linux/Windows | [§10.10](08-tools.md#1010-bash) | D25 | 扩展（超时/进程/输出/审批） |
| `ask` 选项 1/2/3 或自由输入 | [§10.11](08-tools.md#1011-ask) | D26 | 扩展 |
| 每个 agent node 不同 system prompt | [§6](04-prompt.md#6-prompt-体系)、[§7](05-nodes.md#7-agent-node) | D12、D14 | 扩展 |
| global prompt / project prompt | [§6.1](04-prompt.md#61-组装顺序稳定-易变)、[§14](12-project-context.md#14-项目级上下文自动初始化) | D12、D31 | 扩展 |
| code node 权限与核心逻辑 | [§7.2](05-nodes.md#72-节点清单)、[§7.3](05-nodes.md#73-权限矩阵)、[§8.2](06-state-machine.md#82-code-模式默认) | D14 | 扩展 |
| ask node 权限 | [§7.2](05-nodes.md#72-节点清单)、[§7.3](05-nodes.md#73-权限矩阵)、[§8.4](06-state-machine.md#84-ask-模式) | D14 | 扩展 |
| Verifier node 验证 | [§7.4.1](05-nodes.md#741-verifier)、[§8.7](06-state-machine.md#87-验证标准) | D14、D16 | 扩展（结构化 verdict） |
| deliberate node 找茬 | [§7.4.2](05-nodes.md#742-deliberate)、[§7.5](05-nodes.md#75-deliberate-的三个维度) | D15 | 扩展（三维度隔离） |
| 节点通过 name 命名 | [§7.1](05-nodes.md#71-抽象) | D14 | 保留 |
| aggregation node | [§7.4.3](05-nodes.md#743-aggregation)、[§7.6](05-nodes.md#76-fan-out-与-join) | D15 | 扩展（provenance） |
| code mode：code → verification → 回 code | [§8.2](06-state-machine.md#82-code-模式默认) | D16 | 扩展（轮次/无进展/退出码） |
| plan mode：ask → deliberate×3 → aggregation → verification → 回 ask | [§8.3](06-state-machine.md#83-plan-模式) | D17 | 扩展（Plan gate/plan.md） |
| ask mode：ask node | [§8.4](06-state-machine.md#84-ask-模式) | D27 | 扩展 |
| council agent mode | [§8.5](06-state-machine.md#85-council-模式) | D28 | 扩展 |
| 上下文压缩 | [§11.3](09-context.md#113-压缩算法) | D29 | 扩展 |
| Prompt 缓存 | [§6.3](04-prompt.md#63-缓存友好的结构)、[§11.6](09-context.md#116-prompt-缓存与用量) | D30 | 扩展 |
| readCode/bash 限制在 workspace，越界 Y/n/all | [§12](10-security.md#12-安全与审批) | D32 | 扩展（所有文件工具 + 审批类别 + 硬拒绝） |
| 容错、重试、幻觉处理 | [§13](11-fault-tolerance.md#13-容错重试与幻觉控制) | D33、D34 | 扩展 |
| 项目级上下文自动初始化 | [§14](12-project-context.md#14-项目级上下文自动初始化) | D31 | 扩展 |
| 非功能需求（原文未提） | [§15](13-observability.md#15-可观测性与成本)、[§16](14-engineering.md#16-工程实现约束)、[§17](15-testing.md#17-测试策略与验收) | D42 | 新增 |
| 模块间接口与串联（原文未提） | [§21](19-interfaces.md#21-模块接口与串联实现契约) | D43 | 新增 |

---

[返回索引](../README.md) · [上一模块](15-testing.md) · [下一模块](17-adr.md)
