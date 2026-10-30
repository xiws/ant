> 模块 12 / 18 · [返回索引](../README.md) · [上一模块](11-fault-tolerance.md) · [下一模块](13-observability.md)

# 14. 项目级上下文自动初始化

## 14.1 目标与触发

- 目标：首次进入项目时生成一份稳定、可复用的项目摘要，减少后续每次分析的成本，并作为 prompt 的稳定前缀。
- 触发：
  - session 启动时 `project.autoInit=true` 且 `.ant/project.md` 缺失；
  - `project.meta.json` 的 hash 与当前项目不一致；
  - 用户执行 `ant init` 或 `ant init --force`。
- 初始化失败不阻塞 session，只 warning。

## 14.2 失效判定（hash）

`project.meta.json` 记录：

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-30T09:47:04.123Z",
  "model": "deepseek-v4-flash",
  "gitCommit": "30f30d6",
  "hash": "sha256:...",
  "fileCount": 128,
  "bytesSent": 24576
}
```

hash 输入（排序后拼接再 sha256）：

- 文件树摘要：相对路径 + 大小（深度 `project.depth`，上限 `maxFiles`）；
- 关键清单文件：`go.mod`、`go.sum`、`package.json`、`pyproject.toml`、`Cargo.toml`、`Makefile`、`README.md`、CI 配置；
- git HEAD（若存在）；
- 生成器版本与 prompt 版本。

**不**对每个文件内容做全量 hash：成本高且易因无关改动频繁失效；关键清单 + 文件树足以覆盖大多数结构性变化。

## 14.3 生成流程

1. **扫描**：从 workspace 遍历，遵循 `.gitignore` + 内置排除（`.git`、`.ant`、`vendor`、`node_modules`、`bin`、`dist`）；跳过二进制、超大文件；上限 `maxFiles`。
2. **确定性骨架**（不需要模型）：
   - 项目类型（Go/Node/Python/Rust/...）；
   - module 名、语言版本；
   - 目录树（深度 `project.depth`）；
   - 入口（`cmd/`、`main.go`、`package.json` scripts 等）；
   - 依赖摘要（数量、关键依赖）；
   - 构建/测试/lint 命令（来自 Makefile/CI/package.json）。
3. **选择关键文件**：README、主入口、配置文件、CI；总大小 ≤ `project.maxBytes`（32 KB）；排除 `.env`、密钥、证书、大二进制。
4. **模型摘要**（可选）：用 `project.projectModel`（默认 `defaultModel`）把骨架 + 关键文件摘要为 Markdown；prompt 要求：
   - 不得编造未出现的信息；
   - 引用文件路径；
   - 只输出约定小节；
   - 不超过 `project.maxOutputBytes`。
5. **校验**：小节齐全、大小合规；不合法则退回确定性骨架。
6. **写入**：原子写 `project.md` 与 `project.meta.json`；持有 `.ant/project.lock` 防并发。

## 14.4 `project.md` 格式

```markdown
# 项目上下文

<!-- ant:generated:start -->
## 项目用途
## 技术栈
## 目录结构
## 入口
## 构建 / 测试 / Lint
## 约定与风格
## 关键文件
## 已知坑
## 验收规则
<!-- ant:generated:end -->
```

- 所有节点都接收 `project.md`（截断到预算），作为稳定前缀的一部分。
- “验收规则”小节允许用户填写项目特定的通过标准，verifier 优先使用（[§8.7](06-state-machine.md#87-验证标准)）。

## 14.5 用户手改保护

- 自动生成内容必须包在 `<!-- ant:generated:start/end -->` 之间。
- 重新生成只替换标记内内容，标记外用户内容原样保留。
- 如果用户删除了标记：
  - 视为用户完全接管；
  - 不再自动覆盖，只提示“project.md 已被手动修改，`ant init --force` 可重建”；
  - `--force` 也只重建标记块，若仍无标记则整体覆盖前需二次确认。
- 用户可以在标记外添加项目特有说明，持久保留。

## 14.6 隐私与成本

- 不联网；只读本地文件。
- 排除 `.env`、密钥文件、证书、大二进制、`node_modules`/`vendor`。
- 发送给模型的只是骨架 + 关键文件摘要，上限 32 KB。
- 每次 hash 变化只生成一次；`project.md` 作为稳定前缀复用，提升缓存命中。
- 用户可 `project.autoInit=false` 关闭。

## 14.7 失败处理

- 扫描/模型失败：warning，session 继续，不带项目上下文。
- 写入失败：warning，不阻塞。
- 并发生成：后到进程等待或跳过，不覆盖正在写入的文件。
- 生成的 `project.md` 含明显错误时，用户可手改或 `ant init --force`。

> **设计理由**：这是“减少后续项目分析成本”的关键。但若每轮都重新分析，会反向烧钱并破坏缓存；用 hash 失效 + 一次性生成 + 稳定前缀，才同时满足成本与缓存。标记保护避免覆盖用户手写内容。
>
> **备选方案**：每次启动重新生成（未采用：成本高、缓存失效）；完全手工维护（未采用：新项目上手成本高）。

---

[返回索引](../README.md) · [上一模块](11-fault-tolerance.md) · [下一模块](13-observability.md)
