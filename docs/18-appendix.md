> 模块 18 / 19 · [返回索引](../README.md) · [上一模块](17-adr.md) · [下一模块](19-interfaces.md)

# 20. 附录

## 20.1 `models.json` 完整示例

> 说明：`otter` 适配器为实验性，未实现前选中其模型会返回 `adapter_not_implemented`；`apiKey` 使用环境变量。

```json
{
  "schemaVersion": 1,
  "providers": {
    "ollama": {
      "name": "Ollama (Local)",
      "api": "openai-completions",
      "baseUrl": "http://localhost:11434/v1",
      "apiKey": "ollama",
      "timeoutMs": 120000,
      "maxConcurrency": 1,
      "models": [
        {
          "id": "ornith:latest",
          "name": "ornith",
          "contextWindow": 65536,
          "maxOutputTokens": 4096,
          "supportsTools": true,
          "supportsStreaming": true,
          "supportsParallelToolCalls": true,
          "reasoning": false,
          "defaultTemperature": 0.2
        },
        {
          "id": "ornith-1.5:9b",
          "name": "ornith-1.5:9b",
          "contextWindow": 65536,
          "maxOutputTokens": 4096,
          "supportsTools": true,
          "supportsStreaming": true,
          "supportsParallelToolCalls": true,
          "reasoning": false
        }
      ]
    },
    "qwen_deepseek": {
      "name": "deepseek-v4-flash",
      "api": "openai-completions",
      "baseUrl": "https://dashscope.aliyuncs.com/compatible-mode/v1",
      "apiKeyEnv": "DASHSCOPE_API_KEY",
      "models": [
        {
          "id": "deepseek-v4-flash",
          "name": "deepseek-v4-flash",
          "contextWindow": 1048576,
          "maxOutputTokens": 8192,
          "supportsTools": true,
          "reasoning": false
        },
        {
          "id": "qwen3.7-flash-2026-07-15",
          "name": "qwen3.7-flash_snap",
          "contextWindow": 32768,
          "maxOutputTokens": 4096,
          "supportsTools": true,
          "reasoning": false
        }
      ]
    },
    "otter": {
      "name": "Otter (Web)",
      "api": "otter",
      "apiKeyEnv": "OTTER_API_KEY",
      "models": [
        {
          "id": "deepseek",
          "name": "DeepSeek (otter)",
          "contextWindow": 65536,
          "supportsTools": false
        },
        {
          "id": "chatgpt",
          "name": "ChatGPT (otter)",
          "contextWindow": 32768,
          "supportsTools": false
        },
        {
          "id": "gemini",
          "name": "Gemini (otter)",
          "contextWindow": 1048576,
          "supportsTools": false
        }
      ]
    }
  }
}
```

## 20.2 `settings.json` 完整示例

```json
{
  "schemaVersion": 1,
  "defaultModel": "qwen3.7-flash_snap",
  "defaultMode": "code",
  "nodeModels": {
    "code": "deepseek-v4-flash",
    "ask": "qwen3.7-flash_snap",
    "verifier": "qwen3.7-flash_snap",
    "deliberate": "deepseek-v4-flash",
    "aggregation": "deepseek-v4-flash"
  },
  "modelPolicy": "strict",
  "fallbackModels": [],
  "stream": true,
  "log": { "level": "info", "format": "text", "file": "" },
  "ui": { "color": "auto" },
  "approval": {
    "policy": "risk-based",
    "allow": [],
    "deny": []
  },
  "limits": {
    "maxToolTurns": 12,
    "maxRounds": { "code": 3, "plan": 3, "council": 3 },
    "toolTimeoutMs": 5000,
    "bashTimeoutMs": 120000,
    "bashMaxTimeoutMs": 600000,
    "maxToolOutputBytes": 16384,
    "maxFileReadBytes": 262144,
    "maxFileReadLines": 2000,
    "maxWriteBytes": 2097152,
    "maxIdenticalToolCalls": 3,
    "nodeTimeoutMs": 900000,
    "maxSessionTokens": 0
  },
  "context": {
    "autoCompact": true,
    "compactAt": 0.8,
    "keepRecentTurns": 6,
    "summaryMaxTokens": 4096,
    "summaryModel": "",
    "safetyMarginRatio": 0.05
  },
  "retry": {
    "attempts": 3,
    "baseDelayMs": 1000,
    "maxDelayMs": 30000,
    "jitterRatio": 0.2,
    "retryOn": [408, 429, 500, 502, 503, 504, "network", "stream"]
  },
  "sandbox": {
    "workspaceOnly": true,
    "allowReadPaths": [],
    "allowWritePaths": [],
    "protectedPaths": [".git", ".ant"],
    "network": "ask",
    "shell": "",
    "hardDenyExtra": []
  },
  "project": {
    "autoInit": true,
    "projectModel": "",
    "maxFiles": 500,
    "maxBytes": 32768,
    "maxOutputBytes": 8192,
    "depth": 3,
    "exclude": []
  }
}
```

## 20.3 CLI 帮助（目标形态）

```text
ant - AI coding agent

Usage:
  ant [flags] [prompt...]
  ant new [flags] [prompt...]
  ant sessions [flags]
  ant show <session-id>
  ant init [--force]
  ant models
  ant config
  ant version
  ant help

Modes:
  code     修改代码并由 verifier 验证（默认）
  plan     规划 -> 三方评审 -> 聚合 -> 验证，输出 plan.md
  ask      只读问答
  council  修改 -> 三方评审 -> 聚合 -> 验证

Flags:
  -p, --model string       模型别名（models[].name）
  -m, --mode string        模式：code|plan|ask|council
  -s, --session string     指定 session（支持短前缀）
      --new                强制新建 session
      --node-model strings 节点级模型覆盖，如 code=deepseek-v4-flash
  -y, --yes                自动批准非硬拒绝操作（危险）
      --non-interactive    禁止交互；需要审批/提问时退出码 4
      --json               stdout 输出 NDJSON 事件
      --log-level string   error|warn|info|debug
      --no-color           关闭颜色
  -h, --help               帮助
      --version            版本

Exit codes:
  0 成功  1 任务失败  2 用法/配置错误  3 中断  4 需要审批/提问  5 Provider 失败
```

## 20.4 错误码汇总

| code | 类别 | 是否重试 | 用户提示要点 |
| --- | --- | --- | --- |
| `config_invalid` | 配置 | 否 | 文件、字段、期望类型 |
| `provider_auth` | Provider | 否 | 检查 key/权限/`baseUrl` |
| `provider_rate_limit` | Provider | 是 | 退避后重试；最终失败给建议 |
| `provider_unavailable` | Provider | 是 | 网络/服务状态；可配 fallback |
| `provider_invalid_request` | Provider | 否 | 请求被拒；打印摘要（无密钥） |
| `context_overflow` | 上下文 | 压缩后 | 缩小任务/新 session/调大窗口 |
| `tool_invalid_args` | 工具 | 模型自愈 | 具体字段与 schema |
| `tool_timeout` | 工具 | 只读 1 次 | 超时命令/工具；建议拆分 |
| `tool_failed` | 工具 | 否 | 工具内错误与 session id |
| `approval_denied` | 安全 | 否 | 用户拒绝；模型需换方案 |
| `verification_failed` | 任务 | 按轮次 | 失败点、命令、退出码 |
| `loop_detected` | 任务 | 否 | 重复调用/无进展；保留 checkpoint |
| `interrupted` | 用户 | 用户决定 | 续跑命令 |
| `internal` | 未知 | 否 | session id + 日志路径 |

## 20.5 已知问题与后续工作

| 问题 | 影响 | 处理 |
| --- | --- | --- |
| `otter` 协议未公开 | 无法实现适配器 | 标记实验性；选中时报 `adapter_not_implemented`；待补充协议文档 |
| 无 OS 级沙箱 | bash 仍可能越界 | 审批 + 硬拒绝 + 路径校验缓解；后续可接 sandbox-exec/landlock |
| 实现代码尚未开始 | 只有设计规格 | 按 [§16](14-engineering.md#16-工程实现约束) 包结构、[§17](15-testing.md#17-测试策略与验收) 测试策略推进 |
| `pkg/` 为空 | 无公共库 API | 有意保留；避免过早承诺 |

## 20.6 变更记录

| 版本 | 日期 | 变更 |
| --- | --- | --- |
| v1.0 | 2026-10-30 | 基于原始 README 补全为完整设计规格；补全 CLI/配置/模型/Prompt/节点/状态机/Session/工具/上下文/安全/容错/项目初始化/可观测性/工程/测试；修正命名冲突；移除明文密钥 |
| v1.1 | 2026-10-30 | 按模块拆分到 `docs/`；README 改为索引；`§` 引用转为链接 |
| v1.2 | 2026-10-30 | 新增 19 号文档 [§21 模块接口与串联](19-interfaces.md#21-模块接口与串联实现契约)：各模块内部接口、组合根装配、七条调用链；补 D43 |

---

[返回索引](../README.md) · [上一模块](17-adr.md) · [下一模块](19-interfaces.md)
