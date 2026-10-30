> 模块 17 / 18 · [返回索引](../README.md) · [上一模块](16-traceability.md) · [下一模块](18-appendix.md)

# 19. 设计决策记录（ADR）

| ID | 决策 | 主要理由 | 未采用的备选 |
| --- | --- | --- | --- |
| D1 | README 单文件作为唯一事实来源 | 仓库只有 README；避免多文档漂移 | 拆分 `docs/`（可在规模变大后再做） |
| D2 | 文档中文、标识符英文 | 与原始需求语言一致；代码/协议保持通用 | 全英文/全中文 |
| D3 | `ant` 续最近 session；`new` 强制新建；模式切换需确认 | 符合“继续上一个话题”的直觉，避免误切状态机 | 每次新建（丢失上下文） |
| D4 | 保留 `-p`，长名 `--model`；新增 `--node-model` | `-p` 的值是模型别名；节点级覆盖需要显式入口 | 改名为 `--provider`（更误导） |
| D5 | 明确退出码 0–5 | 脚本/CI 需要确定行为 | 只返回 0/1 |
| D6 | 配置逐键深合并 | 项目只覆盖需要的键；数组整体替换可预测 | 整文件覆盖 |
| D7 | 严格 JSON + `schemaVersion` | 避免自写 JSONC；可演化 | JSONC/TOML |
| D8 | 密钥用 `apiKeyEnv`/`${ENV}`，日志打码 | 防止再次泄露；便于 CI | 明文存配置 |
| D9 | 规范键 `defaultModel`，兼容 `defaultProvider` | 原值实际是模型；兼容原文示例 | 只保留 `defaultProvider` |
| D10 | 别名解析：`provider:name` → 唯一 `name` → 唯一 `id` | 同时兼容 name/id 写法；歧义时报错 | 只接受 name |
| D11 | 能力前置校验；`strict` 默认，`degraded` 仅 ask | 避免跑到一半才发现不支持工具 | 静默降级 |
| D12 | Prompt 稳定前缀 → 易变尾部 | 命中前缀缓存；降低成本 | 每轮全量重拼 |
| D13 | 模式 overlay 不可覆盖 | 输出契约与状态机解析绑定 | 允许用户改（易破坏解析） |
| D14 | 节点输出契约：verifier/deliberate/aggregation 用 JSON | 状态机需要机器可判定结果 | 全部自由文本 |
| D15 | deliberate 三维度并发且互相隔离 | 保留独立视角，减少 Anchoring | 单节点多轮自评 |
| D16 | code 模式最多 3 轮 + 无进展中止 | 控制成本，防止空转 | 无限重试 |
| D17 | plan 通过后写 `plan.md`，Plan gate 确认后执行 | 计划与执行分离，人在回路 | 自动执行 |
| D18 | Session 事件溯源 + artifact | 恢复/审计/成本共用一份数据；可读可 diff | SQLite/全量快照 |
| D19 | 恢复时只读工具重放，副作用工具询问；合成 interrupted tool 消息 | 防止重复副作用；保证协议合法 | 全部重放/全部跳过 |
| D20 | 心跳文件锁 | 跨平台一致；不依赖 PID 探测 | 系统 flock/数据库锁 |
| D21 | 工具名 snake_case；拆分 `read` 与 `read_code` | 统一命名；文件读取与依赖切片粒度不同 | 保留 camelCase |
| D22 | 只读工具并发、写工具串行 | 降低延迟且避免竞态 | 全串行/全并发 |
| D23 | `edit` 部分 hunk 匹配 = 失败且不落盘 | hunkpatch 明确会静默丢编辑 | 接受部分应用 |
| D24 | `write` 覆盖前校验 hash/最后读取版本 | 防止丢更新 | 直接覆盖 |
| D25 | bash 默认 120s；Windows 用 `cmd.exe /C` | 构建/测试需要时间；`&&` 兼容性 | 5s；PowerShell 默认 |
| D26 | 审批按类别、`all` 限本 session；硬拒绝清单 | 防止“all”放大到灾难命令 | 全局永久放行 |
| D27 | ask 模式单节点、只读 | 与 code/plan 的职责区分 | 允许 ask 改代码 |
| D28 | council 在 verifier 前加三方评审 | 高风险任务需要对抗性检查 | 只靠 verifier |
| D29 | 上下文压缩四阶段 + 协议不变量 | 兼顾成本与协议正确性 | 超限直接报错 |
| D30 | 缓存靠稳定前缀，不依赖专有 API | 兼容所有 OpenAI 兼容 provider | 仅支持显式缓存协议 |
| D31 | 项目上下文按 hash 失效 + 标记保护 | 省 token、利缓存、不覆盖手改 | 每轮重新生成 |
| D32 | 文件工具全部 workspace 约束；bash 启发式 + 审批 | 原需求只约束 readCode/bash；实际风险更广 | 只约束原工具 |
| D33 | 重试区分幂等性；只重试模型调用 | 避免重复副作用 | 所有错误统一重试 |
| D34 | 循环检测：3 警告/5 中止 + 轮次上限 | 防止死循环与 token 爆炸 | 只靠 maxTurns |
| D35 | v1 不引入 DB/TUI/OS 沙箱 | 控制复杂度与跨平台成本 | 一步到位做完整沙箱 |
| D36 | `otter` 标记实验性，未实现时明确报错 | 缺协议文档，不臆造 | 猜测协议实现 |
| D37 | 不承诺 `pkg/` 公共 API | 避免过早固定库接口 | 先发 SDK |
| D38 | 现有 CI 视为已知问题，给出正确规格 | 当前 workflow 构建不存在的包 | 继续沿用 |
| D39 | 用 `nodeModels` 映射替代单个 `node_name` | 一个字段无法表达多节点模型选择 | 保留 `node_name`（语义不明） |
| D40 | 把 8 个工具调用坑写成协议不变量 | 消息一致性/流式/循环必须由引擎保证，不能靠提示词 | 只写进 system prompt |
| D41 | `read_code` 用 Go AST 静态分析，符号歧义报错 | 换 token 省成本；歧义猜测会喂错代码 | 依赖 gopls/LSP |
| D42 | 工程约束：stdlib 优先、固定包结构、CI 重写规格 | 控制依赖与跨平台风险；现有 CI 不可用 | 引入 agent 框架/数据库 |

---

[返回索引](../README.md) · [上一模块](16-traceability.md) · [下一模块](18-appendix.md)
