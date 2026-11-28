> 模块 12 / 19 · [返回索引](../README.md) · [上一模块](11-fault-tolerance.md) · [下一模块](13-observability.md)

# 14. 项目级上下文初始化

## 14.1 目标与触发

- 目标：首次进入项目时**创建一个空白的 `.ant/project.md`**，项目背景由用户自己填写；ant 不再自动代写正文。
- 触发（自动模式）：
  - session 启动时 `project.autoInit=true` 且 `.ant/project.md` 缺失 → 创建 0 字节空白文件；
  - `project.meta.json` 的 hash 与当前项目不一致（例如每次 git 提交后）→ 重新扫描并刷新 meta；
    `project.md` 已存在则原样保留，不改写内容。
- 显式触发：
  - `ant init`：生成确定性/模型骨架（保留标记外的用户内容）；
  - `ant init --force`：重新生成并整体覆盖（无标记内容先备份）。
- 初始化失败不阻塞 session，只 warning。

## 14.2 失效判定（hash）

`project.meta.json` 记录：

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-30T09:47:04.123Z",
  "model": "manual",
  "gitCommit": "30f30d6",
  "hash": "sha256:...",
  "fileCount": 128,
  "bytesSent": 0
}
```

- `model`：`manual`（自动模式创建的空白文件）、`deterministic`（确定性骨架）或模型别名。
- `bytesSent`：发送给模型的字节数；自动模式恒为 0。

hash 输入（排序后拼接再 sha256）：

- 文件树摘要：相对路径 + 大小（深度 `project.depth`，上限 `maxFiles`）；
- 关键清单文件：`go.mod`、`go.sum`、`package.json`、`pyproject.toml`、`Cargo.toml`、`Makefile`、`README.md`、CI 配置；
- git HEAD（若存在）；
- 生成器版本与 prompt 版本。

hash 只用于判断“是否需要重新扫描/刷新 meta、是否需要补建空白文件”：

- hash 与 meta 一致且 `project.md` 存在 → 直接复用，不写盘；
- hash 不一致 → 刷新 meta；`project.md` 缺失时补建空白文件，已存在则不动；
- `project.md` 自身的内容不参与 hash。

**不**对每个文件内容做全量 hash：成本高且易因无关改动频繁失效；关键清单 + 文件树足以覆盖大多数结构性变化。

## 14.3 流程

自动模式（session 启动 / hash 失效）：

1. **扫描** workspace（遵循 `.gitignore` 与内置排除）并计算 hash；
2. hash 与 meta 一致且 `project.md` 存在 → 结束；
3. 否则持 `.ant/project.lock` 后复查；
4. `project.md` 缺失 → 原子写 0 字节空白文件；已存在 → 不改写；
5. 原子写 `project.meta.json`（hash / gitCommit / generatedAt / fileCount / model=manual）。

显式模式（`ant init` / `ant init --force`）：

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

- 默认是一个空白文件（0 字节），内容完全由用户维护。
- 建议至少覆盖：项目用途、技术栈、目录结构、构建/测试/lint 命令、验收规则。
- “验收规则”小节尤其重要：verifier 优先使用用户填写的项目特定通过标准（[§8.7](06-state-machine.md#87-验证标准)）。
- `ant init` / `--force` 生成的骨架形如：

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

## 14.5 用户手改保护

- **自动模式永不改写已存在的 `project.md`**：无论是否含标记、是否由模型生成，用户内容原样保留。
- `ant init`（非 force）：重新生成只替换 `<!-- ant:generated:start/end -->` 标记块，标记外用户内容原样保留。
- 如果用户删除了标记：
  - 视为用户完全接管；
  - 不再自动覆盖，只提示“project.md 已被手动修改，`ant init --force` 可重建”；
  - `--force` 整体覆盖前先把原内容备份为 `project.md.bak`。
- 用户可以在标记外添加项目特有说明，持久保留。

## 14.6 隐私与成本

- 自动模式不联网、不调用模型、不发送任何仓库内容；只读本地文件并创建空白文件。
- 只有显式 `ant init` 才可能调用模型摘要（骨架 + 关键文件摘要，上限 32 KB）。
- 不读取 `.env`、密钥文件、证书、大二进制、`node_modules`/`vendor`。
- 用户可 `project.autoInit=false` 关闭自动初始化（此时连空白文件也不会自动创建，但仍可使用 `ant init`）。

## 14.7 失败处理

- 扫描/写入失败：warning，session 继续，不带项目上下文。
- 写入失败：warning，不阻塞；原子写保证不会留下半截文件。
- 并发初始化：后到进程通过 `.ant/project.lock` 跳过，不覆盖正在写入的文件。
- 需要重新开始：删除 `project.md`（下次自动补建空白文件）或 `ant init --force`（重新生成骨架）。

> **设计理由**：项目背景属于用户的判断，自动生成容易与真实约定不符，且每次提交后重写会制造无意义的 churn。
> 改为“自动只创建空白文件、显式 `ant init` 才生成骨架”，既保留上手引导，又让 `project.md` 成为稳定、可手填的 prompt 前缀。
>
> **备选方案**：每次启动重新生成（未采用：成本高、缓存失效、覆盖用户内容）；完全手工维护且不创建文件（未采用：新项目不知道从哪里开始）。

---

[返回索引](../README.md) · [上一模块](11-fault-tolerance.md) · [下一模块](13-observability.md)
