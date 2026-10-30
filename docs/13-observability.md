> 模块 13 / 18 · [返回索引](../README.md) · [上一模块](12-project-context.md) · [下一模块](14-engineering.md)

# 15. 可观测性与成本

## 15.1 日志

- 默认输出到 stderr 与 `.ant/logs/ant-YYYYMMDD.log`。
- 格式：文本（默认）或 JSONL（`log.format=json`）。
- 级别：
  - `error`：失败与不可恢复错误；
  - `warn`：降级、审批拒绝、压缩、重试；
  - `info`：turn/节点/工具的关键事件；
  - `debug`：请求摘要、prompt hash、完整工具参数（打码）。
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
[ant] 完成 · mode=code · model=deepseek-v4-flash · 12.3s
      节点：code(2.1k in / 0.8k out) → verifier(1.4k in / 0.3k out)
      tokens：in 3.5k / out 1.1k / cached 0 / reasoning 0
      费用：$0.0000（未配置 pricing）
      改动：pkg/test.go
      验证：go test ./... (exit 0)
      session：.ant/sessions/20261030-094704-3f9a2c
```

- 失败时同样打印，并给出失败点与续跑命令。
- `--json` 时摘要作为 `turn_completed` 事件的一部分。

## 15.4 用量与成本

- 每个 `model_request`/`model_response` 记录 usage；按节点、模型、turn、session 聚合。
- 聚合结果写入 `meta.usage` 与 `usage` 事件。
- 费用估算依赖 `pricing`；未配置时只显示 token。
- `maxSessionTokens` 达到 80% warning，达到 100% 中止并保留 checkpoint。
- `ant show <id>` 显示历史用量；`ant sessions` 显示每个 session 的总 token。

## 15.5 调试

- `--log-level debug`：打印请求摘要（消息数、token 估算、工具列表、prompt hash）、工具参数、重试原因。
- `ant show <id> --events`：查看事件流；大字段显示 artifact 路径。
- 不默认打印完整 prompt/消息，避免泄露代码与密钥；调试时按需从 artifact 读取。
- 每个错误都带 session id 与日志路径，便于用户反馈。

> **设计理由**：agent 的行为是非确定性的，“事后能查”比“代码优雅”更重要。日志与事件分工：事件是恢复/审计的事实来源，日志是排障视图；二者都不应把密钥写进去。
>
> **备选方案**：只打印最终结果（未采用：出问题无法定位）；把完整 prompt 写日志（未采用：泄露风险、体积大）。

---

[返回索引](../README.md) · [上一模块](12-project-context.md) · [下一模块](14-engineering.md)
