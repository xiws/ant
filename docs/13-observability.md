> 模块 13 / 19 · [返回索引](../README.md) · [上一模块](12-project-context.md) · [下一模块](14-engineering.md)

# 15. 可观测性与成本

## 15.1 日志

- 默认输出到 stderr 与 `.ant/logs/ant-YYYYMMDD.log`。
- 格式：文本（默认）或 JSONL（`log.format=json`）。
  - 文本格式时间戳使用本地时间（毫秒），便于直接对照终端；JSONL 仍用 UTC RFC3339。
- 级别：
  - `error`：失败与不可恢复错误；
  - `warn`：降级、审批拒绝、压缩、重试；
  - `info`：turn/节点/工具的关键事件（`turn_started`、`node_completed`、`tool_call` 等），只带 `turn`/`node`/`step` 等定位字段，不带 `data`；
  - `debug`：每个事件（含 `model_request`/`model_response`/`message`/`usage` 等明细）与摘要后的 `data`、prompt hash、完整工具参数（打码）。
- `session_failed` 无论级别都按 `error` 记录，并附带失败信息。
- 轮转：按日期；单文件超过 50 MB 时切分；默认不自动删除（保留策略后续可配置）。
- 打码：`Authorization`、`apiKey`、`*_TOKEN`、`*_API_KEY` 值统一替换为 `***`。
- 日志不替代 session 事件：日志用于排障，事件用于恢复/审计。

## 15.2 NDJSON 事件流

`--json` 时 stdout 输出：

```jsonl
{"type":"turn_started","ts":"2026-10-30T09:47:05.001Z","turn":1,"data":{"inputHash":"sha256:..."}}
{"type":"tool_call","ts":"2026-10-30T09:47:08.200Z","turn":1,"node":"code","data":{"id":"call_1","name":"read"}}
{"type":"tool_result","ts":"2026-10-30T09:47:08.215Z","turn":1,"node":"code","data":{"id":"call_1","status":"ok","durationMs":15}}
{"type":"turn_completed","ts":"2026-10-30T09:48:00.000Z","turn":1,"data":{"status":"ok","usage":{"input":1234,"output":567}}}
```

- 事件 schema 与 session 事件同构；大字段用 `outputRef` 代替。
- 供 CI、外部 UI、成本统计消费；不保证逐字节稳定，但字段只增不改。

## 15.3 Turn 摘要

每个 turn 结束打印：

```text
▌ 完成 · mode=code · model=deepseek-v4-flash · 12.3s
      节点：code(2.1k in / 0.8k out) → verifier(1.4k in / 0.3k out)
      tokens：in 3.5k / out 1.1k / cached 0 / reasoning 0
      费用：$0.0000（未配置 pricing）
      改动：pkg/test.go
      验证：go test ./... (exit 0)
      session：.ant/sessions/20261030-094704-3f9a2c
```

- 失败/中断时状态块变红/橙，并给出失败点与续跑命令；纯文本回退时首行为 `[ant] 完成 · …`。
- `--json` 时摘要作为 `turn_completed` 事件的一部分。

## 15.4 用量与成本

- 每个 `model_request`/`model_response` 记录 usage；按节点、模型、turn、session 聚合。
- 聚合结果写入 `meta.usage` 与 `usage` 事件。
- 费用估算依赖 `pricing`；未配置时只显示 token。
- `maxSessionTokens` 达到 80% warning，达到 100% 中止并保留 checkpoint。
- `ant show <id>` 显示历史用量；`ant sessions` 显示每个 session 的总 token。

## 15.5 调试

三种手段分工：

| 手段 | 用途 | 输出 |
| --- | --- | --- |
| `--log-level debug` | 模块级排障、重试/降级原因 | stderr + `.ant/logs/`，事件摘要与打码后的 data |
| `--verbose` / `settings.ui.verbose` | 实时看 agent 链路 | stdout：节点 prompt 版本/hash/input artifact、每轮请求/响应摘要、工具完整参数、工具结果预览与完整 artifact 路径 |
| `ant export <id>` | 事后完整复盘/分享 | 单文件 HTML 或 Markdown：有效 prompt、请求消息、响应、工具调用/结果、节点输出、usage、压缩/审批记录 |

- `ant show <id> --events`：查看原始事件流；大字段显示 artifact 路径。
- `ant export` 默认写到 `<session>/trace.html`；`--format md -o -` 可输出 Markdown 到 stdout。
- 实时 verbose 的预览有上限（参数约 2 KiB、结果约 4 KiB）；完整内容以 artifact 与导出文件为准。
- `--json` 下忽略 `--verbose`，stdout 保持纯 NDJSON。
- 不默认打印完整 prompt/消息，避免泄露代码与密钥；verbose/export 是显式 opt-in，导出文件权限 0600。
- 每个错误都带 session id 与日志路径，便于用户反馈。

> **设计理由**：agent 的行为是非确定性的，“事后能查”比“代码优雅”更重要。日志与事件分工：事件是恢复/审计的事实来源，日志是排障视图；二者都不应把密钥写进去。
>
> **备选方案**：只打印最终结果（未采用：出问题无法定位）；把完整 prompt 写日志（未采用：泄露风险、体积大）。

---

[返回索引](../README.md) · [上一模块](12-project-context.md) · [下一模块](14-engineering.md)
