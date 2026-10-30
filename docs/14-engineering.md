> 模块 14 / 18 · [返回索引](../README.md) · [上一模块](13-observability.md) · [下一模块](15-testing.md)

# 16. 工程实现约束

## 16.1 建议包结构

```text
cmd/ant/main.go                  # 薄入口：解析参数、调用 internal/cli
internal/cli/                    # 命令、REPL、渲染、退出码
internal/config/                 # 加载、合并、校验、打码
internal/model/                  # 统一消息、tool_call、usage、能力
internal/provider/               # Provider 接口 + openai-completions + otter 桩
internal/prompt/                 # 分层组装、版本、hash
internal/agent/                  # Node 接口、5 个节点、内置 prompts
internal/statemachine/           # 模式图、转移、轮次、fan-out
internal/session/                # meta、events、artifact、锁、恢复、压缩记录
internal/tool/                   # 注册表、校验、执行、截断、审批接入
internal/tool/tools/             # read/readcode/search/edit/write/bash/ask
internal/readcode/               # Go 符号索引与依赖遍历
internal/context/                # token 估算、预算、压缩、缓存指标
internal/projectctx/             # 扫描、hash、生成、标记保护
internal/approval/               # 风险分类、硬拒绝、审批交互
internal/logx/                   # 日志、打码、NDJSON
internal/version/                # 版本注入
pkg/                             # v1 保留为空，不承诺公共 API
```

- 依赖方向：`cli → agent/statemachine → tool/provider/session → model/config`；禁止反向依赖。
- `internal/readcode` 与工具实现分离，便于单测。
- `pkg/` 不作为库承诺；避免过早固定 API。

## 16.2 依赖选择

| 依赖 | 用途 | 理由 |
| --- | --- | --- |
| Go 标准库 | 绝大多数功能 | 减少供应链与构建风险 |
| `github.com/zbysir/hunkpatch` | `edit` 的模糊 diff 应用 | 原始需求指定；Go 原生移植；有 `PartialError` 可处理 |
| `golang.org/x/mod` | 解析 `go.mod` | Go 官方维护 |
| `github.com/sabhiram/go-gitignore`（或等价轻量库） | 解析 `.gitignore` | search/project 扫描需要；避免依赖系统 git |
| `github.com/peterh/liner`（或等价轻量 readline） | REPL 历史/行编辑 | 非 TTY 时回退 `bufio` |
| `golang.org/x/term` | TTY 检测 | Go 官方维护 |

- **不引入**：数据库、TUI 框架、Web 框架、通用 agent 框架、重型 diff 库。
- 所有依赖必须支持 `CGO_ENABLED=0` 与三大平台。
- 依赖版本锁定在 `go.mod`；新增依赖需在评审中说明用途与替代方案。

## 16.3 跨平台

| 场景 | 处理 |
| --- | --- |
| 路径分隔符 | 一律 `filepath`；输出展示用 `/` 归一化 |
| Shell | POSIX `sh -c`；Windows `cmd.exe /C`；可配置 |
| 行尾 | 检测并保留 LF/CRLF；diff 应用前后转换 |
| 进程终止 | Unix 进程组 SIGTERM→SIGKILL；Windows `taskkill /T /F` |
| 信号 | `os.Interrupt`；Windows 控制台事件 |
| 文件锁 | 心跳文件，跨平台一致 |
| 终端颜色 | `NO_COLOR`、`TERM=dumb`、非 TTY 关闭 |
| 文件权限 | Unix 0600/0700；Windows 尽力而为 |
| 长路径/Unicode | 使用 Go 原生路径处理；测试含空格与 CJK 路径 |

## 16.4 性能目标

- 启动到可交互 < 100 ms（不阻塞在项目扫描）。
- 项目上下文生成异步/带进度，不阻塞首轮对话。
- 流式渲染，不等待完整响应。
- `read_code` 按需解析包并缓存索引（session 内）。
- `search` 流式遍历，达到上限即停。
- session 折叠 1 万事件 < 1 s。
- 工具输出截断避免大内存复制。

## 16.5 版本与兼容

- 语义化版本；`ant version` 输出版本、commit、构建日期。
- 配置与 session 都有 `schemaVersion`；破坏性变更提升版本。
- 事件字段只增不改；新增事件类型时旧版本忽略并 warning。
- prompt 文件带版本；节点输出契约变更必须同步提升模式版本。
- 不承诺跨大版本的 session 兼容；文档明确迁移策略。

## 16.6 构建与 CI

**已知问题**：当前 `.github/workflows/build.yml` 来自另一个项目（`notify` / `./cmd/notify` / `SimpleNotify`），与本仓库不匹配：

- `APP_NAME=notify`、`MAIN_PKG=./cmd/notify`：本仓库没有该包，构建会失败；
- `PROJECT_PATH` 指向 `SimpleNotify`；
- 末尾的 `notify` job 使用与本项目无关的 SMTP secrets。

**本文档要求的 CI 规格**（本次不修改文件，留给实现任务）：

1. 触发：`push` 到 `main`、`pull_request`、`workflow_dispatch`、tag `v*`。
2. `test` job：
   - `gofmt -l .`（无输出）；
   - `go vet ./...`；
   - `go test ./...`；
   - Linux 上 `go test -race ./...`。
3. `build` job 矩阵：
   - `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`；
   - `CGO_ENABLED=0`；`-trimpath -ldflags "-s -w -X .../version.Version=<tag>"`；
   - 产物命名 `ant-<goos>-<goarch>[.exe]`。
4. `release` job：仅在 tag 时创建 release，上传产物。
5. 删除与本项目无关的 `notify` job。
6. 不把真实 API key 写入 CI；测试用 fake provider。

## 16.7 安装与发布

- `go install github.com/xiws/ant/cmd/ant@latest`。
- Release 二进制覆盖三大平台。
- 不提供自动更新（v1 非目标）。
- 版本输出用于 issue 诊断。

> **设计理由**：原始 README 没有工程约束，但“能跨平台跑、能被 CI 验证”是 agent 可信使用的前提。现有 CI 明显是从别的项目复制的，必须在实现阶段替换；本文档先把它记录为已知问题，避免误以为 CI 已可用。
>
> **备选方案**：引入完整 agent 框架（未采用：依赖重、可控性差）；用 SQLite/Web 框架（未采用：超出本地 CLI 范围）。

---

[返回索引](../README.md) · [上一模块](13-observability.md) · [下一模块](15-testing.md)
