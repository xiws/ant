> 模块 2 / 18 · [返回索引](../README.md) · [上一模块](01-cli.md) · [下一模块](03-models.md)

# 4. 配置系统

## 4.1 文件与目录

```text
$ANT_HOME/                      # 默认 ~/.ant，可用环境变量覆盖
├── models.json                 # provider 与模型
├── settings.json               # 行为配置
├── prompt.md                   # 用户全局 prompt
├── prompts/<node>.md           # 节点 prompt 覆盖
└── logs/

<workspace>/.ant/               # 项目级配置（同名文件覆盖 home）
├── models.json
├── settings.json
├── prompt.md
├── prompts/<node>.md
├── project.md                  # 自动生成的项目上下文
├── project.meta.json           # 生成时间、hash、git commit
├── sessions/<id>/              # 会话（不应提交 git）
└── logs/
```

## 4.2 优先级与合并

**逐键优先级**：CLI 参数 > workspace `.ant` > home `.ant` > 内置默认值。

**合并规则**：

| 值类型 | 规则 | 示例 |
| --- | --- | --- |
| 对象 | 深合并，递归到叶子 | home 的 `nodeModels.code` 与 workspace 的 `nodeModels.verifier` 同时生效 |
| 标量 | 高优先级覆盖 | workspace `defaultMode` 覆盖 home |
| 数组 | 整体替换，不拼接 | workspace `sandbox.allowReadPaths` 完全替换 home |
| `null` | 删除该键，回落到更低层/默认值 | `"summaryModel": null` |
| 未知字段 | 保留并 warning，不致命 | 为未来版本预留 |

**环境变量的边界**：

- `ANT_HOME`：改变全局配置目录。
- `NO_COLOR`：关闭颜色。
- `models.json` 的 `apiKey`、`baseUrl`、`headers` 支持 `${VAR}` 展开；也支持 `apiKeyEnv`。
- 其他配置**不**通过环境变量覆盖，避免“配置到底从哪来”不可追踪。

## 4.3 JSON 规则与校验

- 严格 JSON：UTF-8、无注释、无尾随逗号、无 NaN/Infinity。
- 每个文件必须有 `schemaVersion`（缺失时按 1 处理并 warning）；当前为 `1`。
- 未知字段：warning，继续运行（前向兼容）。
- 类型错误：fatal，报错包含文件路径与 JSON 位置，例如 `settings.json: limits.maxToolTurns 期望 integer，实际 "12"`。
- 文件不存在：不是错误；使用内置默认值。
- 文件超过 1 MB：拒绝并报错。
- 配置加载完成后立即做语义校验（唯一性、范围、引用存在性），失败则不启动 session。

## 4.4 `models.json` schema

**顶层**：

```json
{
  "schemaVersion": 1,
  "providers": {}
}
```

**Provider 字段**：

| 字段 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `name` | string | 否 | provider key | 显示名称 |
| `api` | string | 是 | — | `openai-completions` 或 `otter` |
| `baseUrl` | string | 条件 | — | `openai-completions` 必填；自定义适配器按契约 |
| `apiKey` | string | 否 | `""` | 支持 `${VAR}`；本地 Ollama 可为占位值 |
| `apiKeyEnv` | string | 否 | `""` | 与 `apiKey` 二选一；优先使用 |
| `headers` | object | 否 | `{}` | 额外请求头，值支持 `${VAR}` |
| `timeoutMs` | integer | 否 | `120000` | 单次 HTTP 请求超时 |
| `maxConcurrency` | integer | 否 | `1` | 同 provider 并发请求上限 |
| `retry` | object | 否 | 继承 `settings.retry` | provider 级覆盖 |
| `models` | array | 是 | — | 至少一个模型 |

**Model 字段**：

| 字段 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | string | 是 | — | 请求体中的真实模型 id |
| `name` | string | 是 | — | 全局唯一别名，供 CLI/配置使用 |
| `contextWindow` | integer | 是 | — | 上下文窗口 token 数，> 0 |
| `maxOutputTokens` | integer | 否 | `4096` | 必须小于 `contextWindow` |
| `supportsTools` | bool | 否 | `false` | 是否支持 function calling |
| `supportsStreaming` | bool | 否 | `true` | 是否支持 SSE |
| `supportsParallelToolCalls` | bool | 否 | 与 `supportsTools` 相同 | 是否支持并行工具调用 |
| `reasoning` | bool | 否 | `false` | 推理模型，参数与用量语义不同 |
| `defaultTemperature` | number | 否 | `0.2` | 推理模型忽略 |
| `pricing` | object | 否 | 无 | `{inputPer1M, outputPer1M, cachedInputPer1M}`，用于费用估算 |
| `notes` | string | 否 | `""` | 展示用备注 |

**校验规则**：

1. `name` 非空且在所有 provider 中全局唯一。
2. `id` 非空；同一 provider 内不重复。
3. `contextWindow > 0`；`maxOutputTokens < contextWindow`。
4. `api=openai-completions` 时 `baseUrl` 必须是合法 URL。
5. `apiKey` 与 `apiKeyEnv` 同时存在时，`apiKeyEnv` 优先。
6. 未知 `api` 值：fatal（避免把请求发到错误协议）。

## 4.5 `settings.json` schema

所有字段可缺省；下表为默认值。

| 分组 | 字段 | 默认 | 说明 |
| --- | --- | --- | --- |
| 模型 | `defaultModel` | 无 | 默认模型别名 |
| 模型 | `defaultProvider` | 无 | 兼容原始 README 的旧键，语义同 `defaultModel`；同时存在时 `defaultModel` 优先 |
| 模型 | `nodeModels` | `{}` | 节点 → 模型别名映射；原 `node_name` 的替代 |
| 模型 | `modelPolicy` | `strict` | `strict`：模型必须支持节点所需工具；`degraded`：仅 `ask` 模式允许无工具模型 |
| 模型 | `fallbackModels` | `[]` | 仅 provider 不可用/限流时按顺序尝试；默认关闭 |
| 模式 | `defaultMode` | `code` | 新 session 的模式 |
| 输出 | `stream` | `true` | 是否流式 |
| 日志 | `log.level` | `info` | `error`\|`warn`\|`info`\|`debug` |
| 日志 | `log.format` | `text` | `text`\|`json` |
| 日志 | `log.file` | `""` | 空则写 `.ant/logs/ant-YYYYMMDD.log` |
| UI | `ui.color` | `auto` | `auto`\|`always`\|`never` |
| 审批 | `approval.policy` | `risk-based` | `risk-based`\|`always`\|`never` |
| 审批 | `approval.allow` | `[]` | 额外放行的命令/路径 glob |
| 审批 | `approval.deny` | `[]` | 额外拒绝的 glob，优先于 allow |
| 限额 | `limits.maxToolTurns` | `12` | 单节点单轮最多工具调用轮数 |
| 限额 | `limits.maxRounds` | `{code:3, plan:3, council:3}` | 模式循环上限 |
| 限额 | `limits.toolTimeoutMs` | `5000` | read/read_code/search/edit/write 的超时 |
| 限额 | `limits.bashTimeoutMs` | `120000` | bash 默认超时 |
| 限额 | `limits.bashMaxTimeoutMs` | `600000` | bash 允许的最大超时 |
| 限额 | `limits.maxToolOutputBytes` | `16384` | 回传模型的工具输出上限 |
| 限额 | `limits.maxFileReadBytes` | `262144` | 单次读取上限 |
| 限额 | `limits.maxFileReadLines` | `2000` | 单次读取行数上限 |
| 限额 | `limits.maxWriteBytes` | `2097152` | 单次写入上限 |
| 限额 | `limits.maxIdenticalToolCalls` | `3` | 相同调用警告阈值；达到 5 次中止 |
| 限额 | `limits.nodeTimeoutMs` | `900000` | 单节点总超时 |
| 限额 | `limits.maxSessionTokens` | `0` | 0 表示不限；达到 80% 告警 |
| 上下文 | `context.autoCompact` | `true` | 是否自动压缩 |
| 上下文 | `context.compactAt` | `0.8` | 输入预算使用率触发阈值 |
| 上下文 | `context.keepRecentTurns` | `6` | 压缩后保留的最近轮数 |
| 上下文 | `context.summaryMaxTokens` | `4096` | 摘要上限 |
| 上下文 | `context.summaryModel` | `""` | 空则用 `defaultModel` |
| 上下文 | `context.safetyMarginRatio` | `0.05` | 预留安全余量 |
| 重试 | `retry.attempts` | `3` | 模型请求总尝试次数 |
| 重试 | `retry.baseDelayMs` | `1000` | 指数退避基数 |
| 重试 | `retry.maxDelayMs` | `30000` | 退避上限 |
| 重试 | `retry.jitterRatio` | `0.2` | 抖动比例 |
| 重试 | `retry.retryOn` | `[408,429,500,502,503,504,"network","stream"]` | 可重试错误 |
| 沙箱 | `sandbox.workspaceOnly` | `true` | 文件工具限制在 workspace |
| 沙箱 | `sandbox.allowReadPaths` | `[]` | 额外只读目录（绝对路径） |
| 沙箱 | `sandbox.allowWritePaths` | `[]` | 额外可写目录（绝对路径） |
| 沙箱 | `sandbox.protectedPaths` | `[".git",".ant"]` | workspace 内禁止写的路径 |
| 沙箱 | `sandbox.network` | `ask` | `ask`\|`allow`\|`deny`，用于 bash 网络命令 |
| 沙箱 | `sandbox.shell` | `""` | 覆盖默认 shell；空 = POSIX `sh` / Windows `cmd.exe` |
| 沙箱 | `sandbox.hardDenyExtra` | `[]` | 追加硬拒绝命令模式 |
| 项目 | `project.autoInit` | `true` | 是否自动生成项目上下文 |
| 项目 | `project.projectModel` | `""` | 空则用 `defaultModel` |
| 项目 | `project.maxFiles` | `500` | 扫描文件上限 |
| 项目 | `project.maxBytes` | `32768` | 发送给模型的上限 |
| 项目 | `project.maxOutputBytes` | `8192` | `project.md` 上限 |
| 项目 | `project.depth` | `3` | 目录树深度 |
| 项目 | `project.exclude` | `[]` | 额外排除 glob |

**最小可用配置**：`log` 也接受字符串简写（如 `"log": "info"`），等价于 `{"level": "info"}`；这是原始 README 的写法，必须兼容。

```json
{
  "schemaVersion": 1,
  "defaultModel": "deepseek-v4-flash",
  "log": { "level": "info", "format": "text", "file": "" }
}
```

## 4.6 密钥与权限

- 推荐 `apiKeyEnv: "DASHSCOPE_API_KEY"`，不要把密钥写入仓库。
- 若使用字面 `apiKey`：
  - 启动时 warning；
  - 文件权限宽于 `0600` 时 warning；
  - 日志、`ant config`、`ant models`、错误信息一律打码为 `<redacted>`。
- 密钥只出现在 Provider 适配器的 `Authorization` 头中；**绝不**写入 prompt、session 事件或工具输出。
- 原始 README 中的真实 key 必须轮换；仅从文档删除不能消除 git 历史中的泄露。
- 自动维护 `<workspace>/.ant/.gitignore`：

```gitignore
sessions/
artifacts/
logs/
```

（`models.json`、`settings.json`、`prompt.md`、`project.md` 允许提交，前提是密钥已用环境变量。）

> **设计理由**：原始需求只写“配置优先级”，没有说明整文件覆盖还是逐键合并。逐键深合并才符合“项目覆盖全局”的直觉，也允许只覆盖 `nodeModels` 的一个节点；严格 JSON 避免自写 JSONC 解析器；环境变量只用于密钥和路径，能让“配置从哪来”始终可追踪。
>
> **备选方案**：整文件覆盖（未采用：改一个键就要复制整份配置）；支持 JSONC 注释（未采用：需要自定义解析器，编辑器 schema 也不统一）。

---

[返回索引](../README.md) · [上一模块](01-cli.md) · [下一模块](03-models.md)
