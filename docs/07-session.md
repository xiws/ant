> 模块 7 / 19 · [返回索引](../README.md) · [上一模块](06-state-machine.md) · [下一模块](08-tools.md)

# 9. Session 与断点续传

## 9.1 目录布局

```text
<workspace>/.ant/sessions/<id>/
├── meta.json          # 会话元数据（原子替换）
├── events.jsonl       # 追加写事件日志（唯一事实来源）
├── artifacts/         # 大输出、plan、渲染后的节点输入
│   ├── 0003-code-input.json
│   ├── 0005-tool-call_1.out
│   └── plan/round-1.md
└── lock               # PID + 心跳
```

**ID 格式**：`YYYYMMDD-HHMMSS-<6hex>`（UTC）。例如 `20261030-094704-3f9a2c`。

- 按时间可排序；人可读；无需额外依赖；6 位随机降低同秒冲突。
- `ant sessions` 显示短 ID；`ant show`/`-s` 接受唯一前缀。

## 9.2 `meta.json`

```json
{
  "schemaVersion": 1,
  "id": "20261030-094704-3f9a2c",
  "workspace": "/Users/me/project",
  "createdAt": "2026-10-30T09:47:04.123Z",
  "updatedAt": "2026-10-30T10:02:11.456Z",
  "status": "idle",
  "mode": "code",
  "title": "给 Test 增加边界检查",
  "model": "deepseek-v4-flash",
  "turnCount": 1,
  "lastSeq": 42,
  "usage": {
    "input": 1234,
    "output": 567,
    "cached": 0,
    "reasoning": 0,
    "estimated": false
  },
  "projectContextHash": "sha256:...",
  "promptVersions": { "code": "1", "verifier": "1" }
}
```

**status**：

| 值 | 含义 |
| --- | --- |
| `idle` | 没有进行中的 turn，可接受新输入 |
| `running` | 进程持有锁并正在执行 |
| `interrupted` | 用户中断，可从断点继续 |
| `failed` | 发生不可自动恢复的错误，保留上下文供续跑/排查 |

不存在 `done`：session 永远可以继续追加新 turn。

## 9.3 事件模型

`events.jsonl` 是 append-only 的 JSON Lines；每行一个事件。

**公共字段**：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `seq` | int | 从 1 开始单调递增；恢复时以最大值为准 |
| `ts` | string | UTC RFC3339 毫秒 |
| `type` | string | 事件类型 |
| `turn` | int | 所属 turn，1 开始 |
| `step` | string | 可选，如 `code`、`plan.round-1.verifier` |
| `node` | string | 可选，节点名 |
| `data` | object | 类型相关 |

**事件类型**：

| type | data 关键字段 |
| --- | --- |
| `session_created` | `id, workspace, mode, model` |
| `mode_changed` | `from, to, reason, confirmedByUser` |
| `turn_started` | `input, inputHash` |
| `node_started` | `node, model, promptVersion, promptHash, inputRef` |
| `model_request` | `messageCount, estimatedInputTokens, tools, stream` |
| `model_response` | `finishReason, usage, textHash, toolCallCount, durationMs` |
| `model_fallback` | `from, to, reason, attempt` |
| `message` | `role, contentHash, contentRef`（完整内容在 artifact） |
| `tool_call` | `id, name, arguments, approval` |
| `tool_result` | `id, status, exitCode, durationMs, outputRef, truncated, error` |
| `approval_requested` | `class, command/path, reason` |
| `approval_resolved` | `class, decision, scope` |
| `context_compacted` | `beforeTokens, afterTokens, summaryRef, replacedRange` |
| `step_completed` | `node, status, outputHash, outputRef, usage` |
| `node_completed` | `node, status, verdict, usage` |
| `turn_completed` | `status, usage, summary` |
| `session_interrupted` | `reason, currentStep` |
| `session_failed` | `reason, errorClass, currentStep` |
| `usage` | `input, output, cached, reasoning, estimated` |

**示例**：

```jsonl
{"seq":1,"ts":"2026-10-30T09:47:04.123Z","type":"session_created","data":{"id":"20261030-094704-3f9a2c","workspace":"/Users/me/project","mode":"code","model":"deepseek-v4-flash"}}
{"seq":2,"ts":"2026-10-30T09:47:05.001Z","type":"turn_started","turn":1,"data":{"input":"给 Test 增加边界检查","inputHash":"sha256:..."}}
{"seq":3,"ts":"2026-10-30T09:47:05.010Z","type":"node_started","turn":1,"step":"code","node":"code","data":{"model":"deepseek-v4-flash","promptVersion":"1","promptHash":"sha256:...","inputRef":"artifacts/0003-code-input.json"}}
{"seq":4,"ts":"2026-10-30T09:47:08.200Z","type":"tool_call","turn":1,"step":"code","node":"code","data":{"id":"call_1","name":"read","arguments":{"path":"pkg/test.go"},"approval":"auto"}}
{"seq":5,"ts":"2026-10-30T09:47:08.215Z","type":"tool_result","turn":1,"step":"code","node":"code","data":{"id":"call_1","status":"ok","exitCode":0,"durationMs":15,"outputRef":"artifacts/0005-tool-call_1.out","truncated":false}}
{"seq":6,"ts":"2026-10-30T09:47:12.900Z","type":"usage","turn":1,"node":"code","data":{"input":1234,"output":567,"cached":0,"reasoning":0,"estimated":false}}
```

## 9.4 NDJSON 输出

`--json` 模式下，stdout 输出与事件日志同构的事件（可裁剪 `data` 中的大字段，用 artifact 引用），一行一个 JSON。用于 CI、外部 UI、成本统计。

## 9.5 每个节点记录什么

| 要求（原始需求） | 落点 |
| --- | --- |
| 节点输入 | `node_started.inputRef` + `artifacts/...-input.json`（渲染后的消息、prompt 版本、工具定义 hash） |
| 节点输出 | `step_completed.outputRef` / `model_response` + `node_completed` |
| 调用的工具 | `tool_call` + `tool_result`（参数、审批、退出码、耗时、输出引用） |
| Token 消耗 | `usage` 事件 + `meta.usage`，按模型/节点/turn 聚合；区分实测与估算 |

- 完整内容写 artifact，事件只存 hash/引用：避免事件文件膨胀，同时保留可复现性。
- 流式文本：只在模型响应结束后写 `model_response`；中途 token 增量不逐条写事件（否则事件爆炸）。
- 工具原始输出超过 `maxToolOutputBytes`：事件记 `truncated=true`，全文写 artifact。
- 敏感信息：provider key 不入事件；bash 命令/输出可能含敏感内容，文件权限 0600，文档提示不要提交。

## 9.6 断点续传算法

启动一个已有 session 时：

1. **取锁**：读取 `lock`；若持有者心跳超过 120s，视为 stale，接管并 warning；否则报错并显示 PID。
2. **读日志**：读 `meta.json` 与 `events.jsonl`；最后一行若不是完整 JSON（进程崩溃），丢弃该行并 warning，不回写。
3. **折叠状态**：按 `seq` 顺序重建
   - 消息历史（含 assistant tool_calls 与 tool results；按 `contentRef` 读取 artifact 还原）；
   - 当前 turn/step/round；
   - 工具调用状态（pending/ok/failed/timeout/denied）；
   - 本 session 的审批放行记录；
   - usage 汇总；
   - 最新压缩摘要。
4. **判断位置**：
   - 最后事件是 `turn_completed` → 空闲，等待新输入。
   - 最后事件是 `node_completed` 但 turn 未完成 → 从下一个 step 继续。
   - 最后事件是 `step_completed` → 从下一个 step 继续。
   - 最后是 `model_request` 无 `model_response` → 重新发起该次模型调用（无副作用）。
   - 最后是 `tool_call` 无 `tool_result` → 见第 5 步。
   - 最后是 `session_interrupted`/`session_failed` → 从记录的 `currentStep` 继续。
5. **修复工具配对**（关键）：
   - 只读工具（read/read_code/search）：可直接重放。
   - 副作用工具（write/edit/bash）：**不得静默重放**。交互式询问“上次可能已执行，重跑/跳过？”；非交互直接中止并说明。
   - 若选择跳过或工具无法执行，在发下一次模型请求前，为每个仍缺失的 `tool_call_id` 合成一条 `role:"tool"` 消息，内容 `{"status":"interrupted","message":"..."}`，保证协议合法。
6. **继续执行**：从断点 step 开始；已完成的 step 不重跑。
7. **记录决策**：恢复过程中的重放/跳过/接管锁都写事件，便于审计。若 artifact 缺失，记录 warning 并用占位消息继续，不静默丢弃 tool 配对。

## 9.7 锁与原子性

- **锁**：`lock` 文件内容 `{pid, host, startedAt, heartbeatAt}`；执行期间每 30s 更新心跳。
- **stale 判定**：心跳超过 120s，或 host 不同且 pid 不存在（跨主机无法判断时只看心跳）。
- **并发**：同一 session 第二个进程直接报错，提示 PID 与 session 路径。
- **meta 原子写**：写临时文件 → `fsync` → rename；Windows 上 rename 覆盖失败时先删除目标再 rename，并接受短暂窗口。
- **events 追加**：`O_APPEND` 打开，单行写入后 `Sync`；写失败则中止 turn，避免状态与日志不一致。
- **artifact 原子写**：同 meta。
- **崩溃**：最坏丢失最后一行事件；恢复算法按 [§9.6](#96-断点续传算法) 处理，不会把半行 JSON 当有效事件。

## 9.8 保留与隐私

- 默认**不自动删除** session；用户手动清理或未来提供 `ant clean`（不在 v1 合同内）。
- 目录 0700、文件 0600；自动维护 `.ant/.gitignore`。
- 不跨 workspace 读取；`ant sessions` 只列当前 workspace。
- session 可能包含代码片段、命令输出、路径；文档明确提示“不要提交 sessions/”。
- 多个 session 可并存；同一 session 串行；同一 workspace 的不同 session 可并行（各自锁）。

## 9.9 Schema 版本与迁移

- 当前 `schemaVersion: 1`。
- 读取时遇到未知事件类型：忽略并 warning（前向兼容）。
- 不做原地改写；未来版本通过新字段/新事件类型迁移，旧版本只读。
- 事件字段只增不改语义；破坏性变更提升 `schemaVersion` 并在启动时提示。

## 9.10 v1 不做快照

- 恢复时折叠全部事件，复杂度 O(n)；预期单 session < 1 万事件，耗时可接受。
- 后续如需要，可增加 `snapshot.json`（记录 `lastSeq` + 折叠结果），事件契约不变。
- 不引入数据库：JSONL 可读、可 diff、可手工排查，跨平台无额外依赖。

> **设计理由**：断点续传最容易错在两件事——消息链缺 tool 结果（API 400）和重放已执行的副作用。前者用“合成 interrupted tool 消息”修复，后者用“只读重放、写操作询问”保证安全。心跳锁避免依赖跨平台 PID 探测；事件溯源让恢复、审计、成本统计共用一份数据。
>
> **备选方案**：用 SQLite 存状态（未采用：依赖重、不可读、跨平台构建成本高）；每次 turn 覆盖保存完整快照（未采用：写入放大且难以审计增量）。

---

[返回索引](../README.md) · [上一模块](06-state-machine.md) · [下一模块](08-tools.md)
