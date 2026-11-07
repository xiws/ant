> 模块 20（补充） · [返回索引](../README.md) · [上一模块](19-interfaces.md)

# 22. 本地 Web 服务（`ant serve`）

| 项 | 内容 |
| --- | --- |
| 文档状态 | 已实现（v1.4 一期只读；`internal/web`、`internal/session/archive.go`、`internal/cli/serve.go`） |
| 目标版本 | v1.4 |
| 需求编号 | `R-WEB-<序号>` |
| 决策编号 | D46–D50（暂记于本文 §22.13） |
| 关联章节 | [§3 CLI](01-cli.md)、[§4 配置](02-config.md)、[§9 Session](07-session.md)、[§10 工具](08-tools.md)、[§15 可观测性](13-observability.md)、[§16 工程约束](14-engineering.md)、[§21 接口](19-interfaces.md) |

> **编号说明**：本文件是第 20 号补充模块，正文采用 §22 编号，避免重排既有 §1–§21 与大量交叉引用；阅读顺序上放在 [§21](19-interfaces.md) 之后。D46–D50 先记录在本文，后续合并进 [ADR](17-adr.md) 时只需搬运，不改变语义。

## 22.1 背景与目标

### 22.1.1 参考页面是什么

`http://127.0.0.1:3080/` 是 **DeepSeek Harness（DSH）的 Web GUI**，不是 `ant` 的页面。它默认用启动令牌换取浏览器会话，未授权的裸 URL 会得到 `401 dsh web authentication required`；页面本身是一个三栏单页应用，面向“本地工作区 + 会话历史 + 智能体执行轨迹”的浏览与交互。

本设计**只借鉴它的信息架构与交互组织**，不复刻它的品牌、账号体系或前端框架。`ant serve` 是独立进程，默认监听 `127.0.0.1:7788`，不代理、不复用 3080 的 DSH 进程或启动令牌。它要解决的是同一类问题：把 `<workspace>/.ant/sessions` 里已经存在的事件、产物和差异，用一个本地页面清楚地展示出来。

### 22.1.2 目标

| 目标 | 说明 |
| --- | --- |
| G1 一条命令启动 | `ant serve` 在 workspace 根目录启动，打印本地 URL；不需要任何账号、注册、登录或云端配置 |
| G2 会话历史可读 | 左栏列出当前 workspace 的 session；中栏按 turn 还原用户输入、助手正文、工具调用、审批、计划和本轮摘要 |
| G3 轨迹可查 | 独立的“轨迹”视图按轮次/请求展示模型调用、工具调用、耗时、token、提示词与产物引用 |
| G4 改动可看 | `edit` 显示 unified diff 与 `+/-` 统计；`write` 显示写入内容与新建/覆盖状态；右栏汇总本轮改动文件 |
| G5 数据只来自 workspace 与 home | session 来自 `<workspace>/.ant`；全局配置与模型来自 `$ANT_HOME`（默认 `~/.ant`）；不扫描其他目录 |
| G6 默认安全 | 只读、只监听回环地址、无账号、无 Cookie 身份、无写接口；路径与密钥都有边界 |
| G7 零新增运行时依赖 | 只用 Go 标准库 `net/http` + `embed`；前端零构建、无 Node、无 CDN |

### 22.1.3 非目标（v1.4 明确不做）

- 不做远程访问、多用户、团队协作、云端同步、账号/登录/注册/OAuth/余额/配额。
- 不做 Web 端新建 session、发送消息、批准审批、回答 `ask`；这些属于二期（§22.15）。
- 不做完整 IDE/文件树/在线编辑；右栏只预览与本次会话相关的改动和产物。
- 不跨 workspace 聚合 session；`ant serve` 绑定启动时的工作目录，行为与 `ant sessions` 一致（[§9.8](07-session.md#98-保留与隐私)）。
- 不引入数据库、Web 框架、前端构建链或新的 Go 第三方依赖（[§16.2](14-engineering.md#162-依赖选择)）。
- 不把 reasoning（模型思考）写进历史；当前事件模型没有持久化它（§22.5.6 说明缺口与二期方案）。

## 22.2 参考页面拆解（DSH Web GUI）

以下按页面实际结构拆解，括号中的尺寸是参考实现的默认值，`ant serve` 可以取近似值，不必逐像素复刻。

### 22.2.1 整体骨架

页面没有营销首页，打开即进入工作区。整体是**三栏应用外壳**：

```text
┌──────────────────────────────┬──────────────────────────────────────────────┬──────────────────────────┐
│ 左栏（导航与列表）             │ 中栏（会话主视图）                             │ 右栏（上下文面板）         │
│ 默认 280px，264–420 可拖拽     │ 最小 400px                                    │ 默认约 45%，最小 300px     │
│ 窄屏折叠为 56px 图标栏         │                                              │ 可拖拽、可全屏、可关闭      │
│                              │                                              │                          │
│ 品牌 / 新建会话               │ 会话标题 + 模式/模型/状态                       │ 文件预览 / 改动 / 交付物    │
│ 全局面板入口                  │ [对话] [轨迹]                                 │                          │
│ 工作区 / 会话浏览             │ 消息流 / 轨迹表                                │                          │
│                              │                                              │                          │
│ 底部：设置 + 账号             │ 底部：输入框 / 运行状态                         │                          │
└──────────────────────────────┴──────────────────────────────────────────────┴──────────────────────────┘
```

- 侧栏在视口宽度小于约 1024px 时自动收成图标栏；右栏在中栏被挤压时先让位。
- 右栏不是固定常驻：没有可展示内容时不占轨道；打开文件、改动或交付物时才出现。

### 22.2.2 左栏

自上而下：

1. **品牌行**：产品标识、侧栏折叠按钮；宽屏时品牌本身也是“新建会话”入口。
2. **新建会话**：主按钮，带快捷键提示。
3. **全局面板**：文件、插件等全局入口（按产品能力增减）。
4. **工作区/会话浏览**：核心区域。按工作区分组，再按“今天/昨天/更早”分组；每项显示会话标题、相对时间、状态；支持搜索和过滤。这是页面里信息密度最高的部分。
5. **底部**：`sidebar.footer.action`（附加动作）、设置入口、**账号区**。账号区包含头像/用户名、登录状态、账号菜单（资料、余额/配额、设置、退出登录）。**这就是需求中要删掉的部分。**

### 22.2.3 中栏

1. **会话头部**：标题、session ID、模式、模型、状态；右侧可能有复制路径、刷新、更多操作。
2. **视图切换**：`对话` / `轨迹` 两个 tab。`对话` 是阅读视图，`轨迹` 是执行账本。
3. **消息流**：
   - 用户消息（右侧或带角色标识）；
   - 助手 Markdown 正文；
   - 可折叠的“思考”块；
   - 工具调用行：`read`、`search`、`bash`、`edit`、`write` 等，显示关键参数、状态、耗时；
   - 文件改动卡片：`+n −m`、路径、diff 预览；悬停或展开显示完整 diff；
   - 审批卡片、计划卡片、待办/目标卡片；
   - 本轮摘要：完成/失败、耗时、token、改动文件、验证命令。
4. **底部输入区**：多行输入、附件/图片、`/` 命令、`@` 文件或会话引用、模型/模式选择、发送/停止；运行中支持排队或插话。

### 22.2.4 右栏

右栏是上下文面板，不是固定内容。常见形态：

- 文件预览（带行号、语法高亮、跳转行）；
- 本轮改动列表与 diff；
- 文档/交付物预览；
- 会话统计（turn、step、token、模型用时、工具用时、TTFT、TPS）。

### 22.2.5 轨迹视图

这是“会话历史中的细节”最关键的参考：

- **工具栏**：时长展示方式（实际时长/等宽）、实际时间、展开/收起所有轮次、展开/收起所有调用、搜索轨迹。
- **表格**：按轮次分组，列为 `输入`、`输出`、`思考`、`时间`、`模型`、`工具`；行类型包括用户、助手、工具、子工具、消息、上下文压缩。
- **时间线**：按时间比例画出本轮各调用；没有计时数据时明确显示“无计时数据”。
- **详情检查器**：点击行打开右侧详情，tab 包括 `概述`、`原始输出`、`预览`、`来源`、`参数`、`结果`、`差异`、`计时`、`用量`、`系统提示词`、`工具`、`选项`。
- **历史分页**：更早的轮次按需加载（“加载更早的历史”），避免一次渲染全部。

### 22.2.6 交互细节

- 工具行默认只显示摘要；展开后显示原始参数 JSON 与结果；长输出截断并给出“查看完整产物”。
- diff 卡默认只显示少量上下文，点击展开；行号与 `+/-` 统计清晰，颜色之外还有符号，避免只靠颜色。
- 工具行上的“检查/Inspect”入口可以跳到轨迹视图对应事件。
- 运行中的行有状态动画；失败行红色、被停止行灰色；错误有可复制的错误码/提示。
- 代码块、输出、命令都支持复制；文件路径可点击打开预览。

### 22.2.7 保留 / 删除 / 替换

| 参考页面元素 | `ant serve` 处理 | 理由 |
| --- | --- | --- |
| 三栏骨架、可拖拽宽度 | **保留** | 会话列表 + 阅读 + 上下文是同类信息架构 |
| 左栏新建会话 | **一期移除**（显示“在终端新建”提示） | 一期只读，不打开 session 锁 |
| 工作区/会话列表、搜索、过滤 | **保留** | 核心导航 |
| 对话/轨迹切换 | **保留** | 需求明确要求轨迹 |
| 工具行、diff 卡、审批卡、计划卡 | **保留**（按 ant 事件模型裁剪） | 需求明确要求修改内容 |
| 右栏文件/改动/产物 | **保留** | 方便核对改动 |
| 底部输入框 | **替换为只读状态条** | 一期不写 session；二期恢复 |
| 账号头像、用户名、登录/退出、余额/配额、云端会话 | **全部删除** | 需求明确“不需要账号登录” |
| 账号设置页、登录弹窗、OAuth 回跳 | **全部删除** | 本地单用户，无账号概念 |
| 品牌 Logo/字体/主题 | **替换为 ant 自己的轻量样式** | 不复刻品牌 |
| 插件市场、团队、GitHub 集成等 | **不做** | 超出 v1.4 范围 |

参考页的账号体系主要承载云端模型凭据、余额/配额与跨设备会话；`ant` 的模型凭据来自 workspace/home 的 `models.json` 与环境变量，会话是本机文件。`ant serve` 一期只读展示这些本地数据，不需要账号，也没有可被账号保护的云端资源。

## 22.3 产品需求

### 22.3.1 命令与进程

| 编号 | 级别 | 需求 |
| --- | --- | --- |
| R-WEB-01 | MUST | 提供 `ant serve [--listen addr] [--open]`；无参数时监听 `127.0.0.1:7788` |
| R-WEB-02 | MUST | 启动后打印可点击的本地 URL、workspace、home、只读提示；`--open` 才尝试打开浏览器 |
| R-WEB-03 | MUST | 只接受回环地址；`0.0.0.0`、`::`、非 `localhost`/回环 IP 的主机名一律拒绝（退出码 2） |
| R-WEB-04 | MUST | 不要求账号、登录、注册、访问令牌、Cookie 或任何身份输入；页面打开即可用（v1.4 不使用 token） |
| R-WEB-05 | MUST | 收到 SIGINT/SIGTERM 时优雅关闭（默认 5s），退出码 0 |
| R-WEB-06 | MUST | 不改变既有子命令与 `ant [prompt]` 行为；`ant serve` 不打开/创建 session、不取锁 |
| R-WEB-07 | SHOULD | 配置非法时仍能启动历史浏览，UI 显示配置告警；不因坏配置白屏 |

### 22.3.2 页面与数据

| 编号 | 级别 | 需求 |
| --- | --- | --- |
| R-WEB-08 | MUST | 左栏列出当前 workspace 的全部 session（分页/限制可配），显示标题、模式、模型、状态、turn 数、token、更新时间 |
| R-WEB-09 | MUST | 会话页按 turn 还原用户输入、助手正文、工具调用与结果、审批、计划、压缩、本轮摘要 |
| R-WEB-10 | MUST | 工具调用显示名称、关键参数、状态、耗时、退出码；`edit` 显示 unified diff 与 `+/-`；`write` 显示写入内容与新建/覆盖 |
| R-WEB-11 | MUST | 轨迹视图按轮次展示 `model_request`/`model_response`/`tool_call`/`tool_result`/`node_*`/`approval_*`/`context_compacted` 等事件，并支持详情 |
| R-WEB-12 | MUST | 右栏展示本轮改动文件、当前文件预览、session 产物（artifact）列表与预览 |
| R-WEB-13 | MUST | “本机”面板展示 workspace、home、版本、模型别名（无密钥）、配置来源与告警 |
| R-WEB-14 | MUST | 运行中的 session 在 2s 内反映新增事件；无新事件时不打断阅读（保持滚动位置） |
| R-WEB-15 | SHOULD | 支持窄屏：<1024 侧栏收成图标栏，<860 右栏覆盖显示，<640 单栏 |
| R-WEB-16 | SHOULD | 键盘可达：Tab 焦点、`/` 搜索、`t` 切换轨迹、`[`/`]` 上/下会话、`Esc` 关闭面板 |

### 22.3.3 安全与数据边界

| 编号 | 级别 | 需求 |
| --- | --- | --- |
| R-WEB-17 | MUST | 只读：不写 session、meta、events、artifact、workspace 文件；不创建 `.ant/sessions` |
| R-WEB-18 | MUST | 所有文件读取限制在 workspace 与 home 白名单内；拒绝 `..`、绝对路径、符号链接逃逸 |
| R-WEB-19 | MUST | 不返回 `apiKey`、`Authorization`、环境变量值；`models.json` 原文件不直接下发，只下发解析后的模型元数据 |
| R-WEB-20 | MUST | 响应带 CSP、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`；无 CORS |
| R-WEB-21 | MUST | 无账号相关路由/文案/存储：不出现登录弹窗、头像、账号菜单、Cookie 身份 |
| R-WEB-22 | SHOULD | 测试覆盖路径穿越、Host 校验、304 缓存、坏事件、缺失 artifact、无账号 UI |

## 22.4 信息架构与页面设计

### 22.4.1 `ant serve` 页面线框

```text
┌──────────────────────────────┬──────────────────────────────────────────────┬──────────────────────────┐
│ ant  v1.4            [搜索]  │ 这个项目做了什么                              │ 改动 (3)        [×]      │
│ [全部] [code] [plan] [ask]   │ ask · ornith-1.5:9b · idle · 5 轮 · 163.7k tok│ ─────────────────────    │
│                              │ [对话] [轨迹]                                 │ internal/a.go   +8 −2    │
│ 今天                         │ ┌──────────────────────────────────────────┐ │ internal/b.go   +3 −0    │
│ ● 这个项目做了什么   5 轮    │ │ 你  这个项目做了什么                       │ │ docs/c.md       +12 −4   │
│   20261106-234456-df479f     │ │                                          │ │                          │
│ ● 修复日志级别       2 轮    │ │ 助手  这个项目是 ant ...                   │ │ [查看 diff]              │
│   20261106-...               │ │                                          │ │                          │
│ 昨天                         │ │ ▸ 思考（历史未记录）                       │ │                          │
│ ○ 设计 session 存储  3 轮    │ │ ▸ read  README.md        ok · 0ms         │ │                          │
│   20261106-...               │ │ ▸ edit  internal/x.go    ok · 31ms  +8 −2 │ │                          │
│                              │ │   @@ -12,6 +12,8 @@ ...                   │ │                          │
│                              │ │ ▸ bash  go test ./...    ok · 1.2s  exit 0│ │                          │
│                              │ │                                          │ │                          │
│                              │ │ 本轮完成 · 12.3s · in 9.2k / out 853      │ │                          │
│                              │ │ 改动：internal/x.go                       │ │                          │
│                              │ └──────────────────────────────────────────┘ │                          │
│ ┌──────────────────────────┐ │ 只读模式 · 在终端继续：ant -s <id>   [复制]   │                          │
│ │ ● 本地模式                │ └──────────────────────────────────────────────┘                          │
│ │ /path/to/workspace        │                                                              │
│ │ ant v1.4 · 127.0.0.1:7788 │                                                              │
│ │ [本机配置] [设置]          │                                                              │
│ └──────────────────────────┘ │                                                              │
└──────────────────────────────┴──────────────────────────────────────────────┴──────────────────────────┘
```

与参考页的差异：

- 左下角**没有头像/账号/登录/退出/配额**，替换为“本地模式”状态卡：workspace、home、版本、监听地址、`本机配置`、`设置`。
- 底部输入区在一期是只读状态条，明确告诉用户“去终端继续”，并提供 `ant -s <id>` 复制。
- 左栏顶部“新建会话”一期变为禁用/说明入口；不制造“点了没反应”的假按钮。

### 22.4.2 左栏

| 区域 | 内容 | 交互 |
| --- | --- | --- |
| 品牌行 | `ant`、版本、折叠按钮 | 点击折叠/展开；版本点击打开“关于” |
| 搜索 | 按标题、ID、模型过滤 | `/` 聚焦；输入即筛；清空恢复 |
| 过滤 | `全部 / code / plan / ask / council`；状态 `running / idle / interrupted / failed` | 单选 chip；与搜索叠加 |
| 会话列表 | 标题、ID 短码、相对时间、模式 badge、状态点、turn/token | 点击切换；键盘上下；当前项高亮 |
| 分组 | 今天 / 昨天 / 更早 | 粘性小标题 |
| 空状态 | “当前 workspace 还没有 session” + `ant "..."` 提示 | 复制命令 |
| 底部 | 本地状态卡 + 本机配置 + 设置 | 打开对话框；不出现账号 |

会话项状态点：

| 状态 | 显示 | 判定 |
| --- | --- | --- |
| running | 绿色脉冲 | `meta.status=running` 且 lock 心跳新鲜 |
| idle | 灰色 | `meta.status=idle` |
| interrupted | 琥珀色 | `meta.status=interrupted` 或无完成 turn |
| failed | 红色 | `meta.status=failed` |
| stale | 琥珀色 + “上次运行中断” | `meta.status=running` 但 lock 缺失/心跳过期 |

### 22.4.3 会话页（对话）

**头部**：标题（可编辑？一期不可）、session ID（可复制）、模式 badge、模型别名、状态、更新时间、turn 数、token 总数；右侧“复制 session 路径”“刷新”“在终端继续”。

**Turn 分组**：每个 turn 一个区块，默认展开；turn 头显示 `第 N 轮`、状态、耗时、token；可折叠。滚动时 turn 头吸顶，提供“上一轮/下一轮”导航。

**消息项**：

| 项 | 渲染 |
| --- | --- |
| 用户输入 | 引用块/气泡；纯文本，保留换行；显示 turn 开始时间 |
| 助手正文 | Markdown 子集：标题、段落、列表、引用、代码块、行内代码、粗体、链接；代码块带语言与复制 |
| 思考 | 有 reasoning 产物时才显示；无则显示“历史未记录”，不伪造 |
| 工具调用 | 一行摘要 + 展开详情；按工具类型渲染（§22.5.3） |
| 审批 | `approval_requested` / `approval_resolved` 合成一张卡：类别、命令/路径、原因、决定、范围、时间 |
| 上下文压缩 | 提示条：压缩前/后 token、摘要引用；可展开摘要 |
| 计划 | `turn_completed.planPath` 或 artifact `artifacts/plan.md` / `artifacts/plan/round-N.md`：标题、轮次、Markdown 预览、复制路径 |
| 本轮摘要 | 状态、耗时、usage（in/out/cached/reasoning）、改动文件、验证命令、exitCode、错误 |
| 错误 | `session_failed` / node 失败：错误分类、message、hint、失败 step；可复制 |
| 未知事件 | 不进入对话流；在轨迹里以“未知事件”显示原始 JSON |

**只读状态条**（替换输入框）：

```text
┌──────────────────────────────────────────────────────────────┐
│ 只读模式 · 在终端继续该会话：ant -s 20261106-234456-df479f  [复制] │
│ 需要修改代码或新建会话，请在 workspace 终端运行 ant。            │
└──────────────────────────────────────────────────────────────┘
```

### 22.4.4 轨迹页

**工具栏**：

- 时长：`实际时间` / `等宽`（默认实际时间）；
- `展开所有轮次` / `收起所有轮次`；
- `展开所有调用` / `收起所有调用`；
- 搜索框：按节点、工具名、模型、错误文本过滤；
- 计数：`N 轮 · M 请求 · K 工具调用`。

**表格**：

```text
┌──────┬────────────┬──────────────────────┬──────────────────────┬──────┬────────┬──────────────┬──────────┐
│ #    │ 类型        │ 输入                  │ 输出                  │ 思考  │ 时间    │ 模型          │ 工具/状态 │
├──────┼────────────┼──────────────────────┼──────────────────────┼──────┼────────┼──────────────┼──────────┤
│ 1    │ 用户        │ 这个项目做了什么       │ —                    │ —    │ 0ms    │ —            │ —        │
│ 1.1  │ 请求 #1     │ 3 条消息 / 2.5k tok   │ 79 tok / tool_calls  │ 未记录│ 55.5s  │ ornith-1.5:9b│ read, bash│
│ 1.1.1│ 工具 read   │ {"path":"README.md"}  │ ok / 0ms             │ —    │ 0ms    │ —            │ ok       │
│ 1.1.2│ 工具 bash   │ cat go.mod            │ ok / exit 0 / 7ms    │ —    │ 7ms    │ —            │ ok       │
│ 1.2  │ 请求 #2     │ 6 条消息 / 5.9k tok   │ 774 tok / stop       │ 未记录│ 26.7s  │ ornith-1.5:9b│ —        │
│ 1    │ 本轮完成    │ —                     │ ok / in 9.2k out 853 │ —    │ 84.0s  │ —            │ —        │
└──────┴────────────┴──────────────────────┴──────────────────────┴──────┴────────┴──────────────┴──────────┘
```

**时间线**：在表格上方以水平条展示各请求/工具的实际时长比例；无计时的行标注“未记录”。点击条聚焦对应行。

**详情检查器**（点击行后从右侧滑出）：

| tab | 内容 | 数据来源 |
| --- | --- | --- |
| 概述 | 节点、step、模型、provider、状态、耗时、token | `node_started`、`model_request/response`、`node_completed` |
| 参数 | 模型请求参数、工具参数 JSON | `model_request.data`、`tool_call.data.arguments` |
| 结果 | 模型响应摘要、工具结果 JSON、输出引用 | `model_response.data`、`tool_result.data`、tool message |
| 差异 | 该请求下 `edit` 的 diff、`write` 的写入内容 | `tool_call.data.arguments` |
| 计时 | 开始、总时长、模型耗时、工具耗时、TTFT（未记录） | 事件时间戳与 `durationMs` |
| 用量 | input/output/cached/reasoning、按节点/模型聚合 | `usage`、`model_response.usage`、`node_completed.usage` |
| 系统提示词 | 渲染后的系统消息与工具定义（默认折叠，提示可能含敏感内容） | `node_started.inputRef` artifact |
| 原始 JSON | 原始事件 JSON | `events.jsonl` |

**分页**：默认加载最近 50 个 turn；更早的 turn 通过“加载更早”按 `beforeTurn` 拉取。单 session 事件数超过上限时显示提示，不阻塞查看最近内容。

### 22.4.5 右栏

| tab | 内容 | 交互 |
| --- | --- | --- |
| 改动 | 当前 turn/会话的 `changed` 文件列表，`+/-` 统计，状态（新增/修改/删除） | 点击文件 → 文件预览；点击统计 → diff |
| 文件 | 选中文件的当前内容（带行号）或历史 diff | 复制路径；在系统文件管理器中打开（可选） |
| 产物 | `artifacts/` 列表：名称、大小、类型、时间 | 文本/JSON/Markdown 预览；二进制只给下载 |
| 概览 | session meta、usage、模式、模型、project context hash、prompt 版本、路径 | 复制 ID/路径 |

右栏默认打开“改动”；没有改动时打开“产物”；都没有时收起。

### 22.4.6 “本机配置”与“设置”（无账号）

**本机配置**对话框：

```text
本机配置
├─ 工作区
│  ├─ 路径：/path/to/workspace
│  ├─ session 数：4
│  └─ 项目上下文：.ant/project.md（hash、生成时间）
├─ Home
│  ├─ 路径：/home/me/.ant
│  └─ 文件：models.json 2.2KB / settings.json 120B / prompts/code.md 1.1KB
├─ 模型（只读）
│  └─ 别名 / provider / id / context / tools / streaming（不含 apiKey）
├─ 配置来源
│  └─ defaultModel ← workspace .ant/settings.json
└─ 告警
   └─ 配置字段类型错误 / 模型未配置 / session 坏行
```

**设置**：主题（跟随系统/浅色/深色）、工具详情密度（简洁/标准/详细）、时间显示（本地/UTC）、关于（版本、commit、构建时间、workspace/home、监听地址）。**没有账号 tab、没有登录/退出、没有配额。**

### 22.4.7 空态、加载、错误、运行态

| 场景 | 表现 |
| --- | --- |
| 无 session | 左栏空态 + 终端命令提示；中栏品牌空态 |
| 无选中 | 中栏显示“选择一个会话”；右栏收起 |
| 加载中 | 骨架屏；不阻塞左栏 |
| session 不存在 | 404 卡片：ID、可能原因、返回列表 |
| events 坏行 | 顶部黄色告警：跳过 N 行；其余正常渲染 |
| artifact 缺失 | 对应消息/工具卡显示“内容缺失（artifact 已删除）”，不崩溃 |
| 配置非法 | 顶部黄色横幅 + “本机配置”详情；页面仍可浏览 |
| 运行中 | 头部绿色脉冲 + “运行中”；新事件追加；滚动位置保持 |
| 上次运行中断 | 头部琥珀色“上次运行中断”；显示最后事件时间与失败 step |
| 服务断开 | 顶部红色条“与本地服务断开”；自动重连（指数退避，最大 10s） |

### 22.4.8 响应式与快捷键

| 宽度 | 布局 |
| --- | --- |
| ≥1200 | 三栏常驻 |
| 1024–1199 | 三栏；右栏可覆盖 |
| 768–1023 | 左栏 56px 图标栏；右栏覆盖 |
| <768 | 单栏；左栏抽屉；右栏全屏抽屉 |

快捷键（不与浏览器冲突为准）：

| 键 | 行为 |
| --- | --- |
| `/` | 聚焦会话搜索 |
| `t` | 对话/轨迹切换 |
| `[` / `]` | 上一个/下一个 session |
| `j` / `k` | 下一个/上一个 turn（轨迹页） |
| `Esc` | 关闭对话框/右栏/抽屉 |
| `?` | 快捷键帮助 |

## 22.5 数据来源与投影

### 22.5.1 数据边界

`ant serve` 启动时解析两个根：

| 根 | 解析方式 | 内容 | 用途 |
| --- | --- | --- | --- |
| workspace | `config.Loader` 的 `WorkDir()`；缺省 `os.Getwd()`，符号链接解析 | `.ant/sessions/`、`.ant/project.md`、`.ant/project.meta.json`、`.ant/settings.json`、`.ant/models.json`、`.ant/prompts/` | 会话、项目上下文、工作区配置 |
| home | `$ANT_HOME`，缺省 `os.UserHomeDir()/.ant` | `settings.json`、`models.json`、`prompt.md`、`prompts/<node>.md` | 全局配置、模型别名、全局 prompt |

不读取：

- workspace 之外的项目目录；
- home 下 `$ANT_HOME` 之外的任意文件；
- `.ant/logs/`（体积大且可能含敏感输出；只在终端排障）；
- `.git/`、`.ant/sessions/` 之外的隐藏目录。

> 设计理由：与 [§9.8](07-session.md#98-保留与隐私) 的“session 只属于创建它的 workspace”一致；避免把 Web 服务做成跨项目扫描器，减少隐私与性能风险。

### 22.5.2 事件到视图的映射

| 事件 | 对话流 | 轨迹 | 说明 |
| --- | --- | --- | --- |
| `session_created` | 头部 meta | 头部行 | id/workspace/mode/model |
| `mode_changed` | 系统提示“模式切换” | 系统行 | from/to/reason |
| `turn_started` | 用户消息 | 轮次开始 | input、inputHash、mode、round、turn |
| `node_started` | 不单独显示 | 请求/节点行 | node、model、provider、prompt hash、inputRef |
| `model_request` | 不显示（除错误） | 请求行 | messageCount、estimatedInputTokens、tools、stream、attempt |
| `model_response` | 失败时错误卡 | 请求结果 | finishReason、usage、durationMs、textHash、toolCallCount、错误字段 |
| `model_fallback` | 降级提示 | 请求行子项 | from/to/reason/attempt |
| `message`（assistant） | 助手 Markdown | 请求输出 | `contentRef` 读 artifact；空正文 + toolCalls 不单独显示 |
| `message`（tool） | 工具结果 | 工具结果 | `contentRef` 读 JSON body；按 `toolCallID` 配对 |
| `tool_call` | 工具卡（运行中） | 工具行 | name、arguments、approval |
| `tool_result` | 工具卡（完成/失败） | 工具结果 | status、exitCode、durationMs、truncated、outputRef、error |
| `approval_requested` | 审批卡 | 审批行 | class、command/path、reason |
| `approval_resolved` | 审批卡（决定） | 审批结果 | decision、scope |
| `context_compacted` | 压缩提示 | 压缩行 | before/after tokens、summaryRef |
| `step_completed` | 不单独显示 | 步骤行 | node、status、outputRef、usage、changed |
| `node_completed` | 不单独显示 | 节点行 | node、status、durationMs、usage、verdict（可选） |
| `turn_completed` | 本轮摘要 | 轮次结束 | status、exitCode、usage、changed、checks、planPath |
| `session_interrupted` | 中断横幅 | 结束行 | reason、currentStep |
| `session_failed` | 失败卡 | 结束行 | reason、errorClass、currentStep、message |
| `usage` | 本轮/会话汇总 | 用量行 | input/output/cached/reasoning/estimated |
| 未知类型 | 不显示 | “未知事件” | 原始 JSON；不能中断渲染 |

### 22.5.3 工具卡片

| 工具 | 参数（`tool_call.arguments`） | 结果（tool message `data`） | 渲染 |
| --- | --- | --- | --- |
| `read` | `path`、`startLine`、`endLine` | `absolutePath`、`content`（带行号） | 文件行卡：路径、行号范围、行数；展开看内容 |
| `read_code` | `target`、`depth`、`maxSymbols` | 符号切片文本 | 代码卡：目标、符号数、代码块 |
| `search` | `query`、`path`、`regex` | `matchCount`、`results`、`scannedFiles`、`truncated` | 搜索卡：查询、命中数、文件数；展开看命中 |
| `bash` | `command`、`cwd`、`timeoutMs` | `stdout`、`stderr`、`exitCode`、`durationMs`、`truncated`、`outputRef` | 终端卡：命令、exit badge、耗时；输出折叠，完整输出走 artifact |
| `edit` | `path`、`diff`（unified diff） | `hunks`、`applied`、`added`、`removed`、`bytes`、`sha256` | diff 卡：路径、`+n −m`、hunk 行号、红绿行；展开全部 |
| `write` | `path`、`content`、`expectedHash` | `path`、`bytes`、`sha256`、`created` | 写入卡：路径、新建/覆盖、字节数；内容预览（默认 40 行） |
| `ask` | `question`、`options` | 用户输入 | 问答卡：问题、选项、回答 |

卡片状态：

- `pending`：已发起、无 result；显示“运行中”；
- `ok`：result.ok=true；
- `error`：result.ok=false；显示 `error.code/message/hint`；
- `stopped`：session 中断且无 result；
- `denied`：审批拒绝（approval 数据）。

### 22.5.4 差异（diff）

一期差异有两个来源：

1. **`edit`**：`tool_call.arguments.diff` 本身就是 unified diff，解析后精确展示；工具结果还有 `added`/`removed`/`hunks` 可交叉校验。
2. **`write`**：`tool_call.arguments.content` 是完整新内容。若 `created=true`，显示“新建文件 +N”；若是覆盖，显示“整文件重写”，默认不伪造旧 diff（旧内容没有持久化）。

解析规则：

- 去掉 Markdown 代码围栏（以三个反引号开头/结尾的 diff 块）；
- 支持 `diff --git`、`---`/`+++`、`@@ -a,b +c,d @@`；
- 行类型 ` `、`-`、`+`、`\`；
- 计算 `+/-` 与 hunk 数；
- 单文件最多 2000 行、50 个 hunk，超出标记“已截断”；
- 解析失败时降级为原始文本代码块，并显示 warning。

**二期增强**（不是一期必需）：工具结果增加 `diffRef` artifact，保存 `{path, oldText, newText, unified}`，从而在文件已被后续修改时仍能精确回放历史 diff；事件字段只增不改（[§9.9](07-session.md#99-schema-版本与迁移)）。

### 22.5.5 轨迹的配对算法

事件没有 request ID，按以下顺序投影：

1. `turn_started` → 新建 `TurnView`，记录 `startedAt`、`input`、`mode`、`round`。
2. `node_started` → 新建 `NodeView`；`model`/`provider`/`promptHash`/`inputRef` 记入当前节点。
3. `model_request` → 新建 `RequestView`，`status=pending`，记录 `attempt`、`messageCount`、`tools`、`estimatedInputTokens`。
4. `model_response` → 找同 turn、同 node、`attempt` 最近的 pending request；设置 `durationMs`、`usage`、`finishReason`、`textHash`、`toolCallCount`；有 `errorClass` 时 `status=failed`。
5. `tool_call` → 新建 `ToolView`，按 `id` 建索引；挂到当前 request 与当前 turn。
6. `tool_result` → 按 `id` 找 `ToolView`；设置 status/exitCode/duration/truncated/outputRef/error。
7. `node_completed` → 设置节点 duration/status/usage；若没有 request，也生成一个节点行。
8. `turn_completed` → 关闭 turn；记录 `status`、`exitCode`、`usage`、`changed`、`checks`、`planPath`；生成摘要。
9. `session_interrupted` / `session_failed` → 关闭 turn；记录失败原因；无 `turn_completed` 也生成“未完成”摘要。
10. `usage` → 累加到当前节点/轮次；不单独生成对话项。
11. 未知事件 → 轨迹行 `kind=unknown`，详情显示原始 JSON；不参与对话。

**无 `turn`/无 `node` 的事件**：真实日志里存在会话初始化阶段的模型探测（`model_request`/`model_response` 没有 `turn`，如 `seq=2`），以及 provider 级流式回退产生的无 `node` 响应（如 `seq=6`）。投影规则：

- 无 `turn` 的事件归入“会话初始化”伪轮次，默认折叠；不参与正常 turn 的请求配对。
- 有 `turn` 但无 `node` 的模型响应归入“提供方”伪节点；若同一 turn/attempt 已有 node 级响应，优先使用 node 级响应，前者作为“尝试记录”保留在详情里，不重复生成对话项。
- 同 attempt 多个 node 级响应：保留最后一个成功响应，之前的状态作为 attempts 子行；不丢事件。

容错：

- 缺 `tool_result`：工具卡显示“等待中/已中断”；
- 缺 `model_response`：请求行显示“未完成”；
- 重复 `tool_result`：以最后一个为准，并加 warning；
- artifact 缺失：内容显示占位，不让整个 session 失败；
- `events.jsonl` 尾行损坏：`readEventsFile` 已跳过并给出 warning（[§9.6](07-session.md#96-断点续传算法)）。

### 22.5.6 当前数据缺口

| 缺口 | 影响 | 一期处理 | 二期方案 |
| --- | --- | --- | --- |
| reasoning 未持久化 | 历史无法显示模型思考 | 显示“历史未记录”，不伪造 | `model_response` 增加 `reasoningRef`，artifact 保存 `reasoning-*.txt`；`provider.ChatResponse` 增加 Reasoning 字段 |
| `write` 旧内容未保存 | 覆盖写无法精确 diff | 显示“整文件重写 +N” | tool result 增加 `diffRef`（old/new） |
| TTFT 未记录 | 轨迹无法显示首 token 延迟 | 显示“未记录” | 事件增加 `ttftMs`（流式首 delta 时间） |
| turn 总耗时未显式字段 | 需由首尾事件计算 | 用 `turn_started` 到 `turn_completed` 差值 | `turn_completed` 增加 `durationMs` |
| 模型请求与响应无 request ID | 并发/重试时配对可能歧义 | 按 turn/node/attempt/顺序配对 | 增加 `requestId`（只增字段） |
| planPath 与实际 artifact 路径不一致 | 可能指向不存在的 `<session>/plan.md` | 同时尝试 `artifacts/plan.md`、`artifacts/plan/round-N.md`，找不到给“计划内容缺失” | 修正 `sessionPlanWriter` 返回路径，或统一读取 artifact |

> 这些缺口都不阻塞一期“把已有历史看清楚”。一期坚持**不伪造**：没有的数据明确写“未记录/缺失”，而不是猜。

## 22.6 后端设计

### 22.6.1 包与依赖方向

新增两个包，另加一个组合根命令文件：

```text
internal/session/archive.go      # 只读归档：List / Snapshot / Artifact
internal/web/                    # HTTP 服务、投影、静态资源
internal/cli/serve.go            # 组合根中的 serve 分支
```

依赖方向保持现有分层：

```text
cli ──> web ──> session ──> model
  │       └──> config
  └──> version / logx
```

`web` 不 import `cli`、`agent`、`statemachine`、`tool`；它只消费 `session.Archive`、`config.View` 和 `model` 词汇。这样一期只读不会把整棵运行时依赖树拖进 Web 进程，测试也只需要临时目录。

### 22.6.2 只读归档 `session.Archive`

**为什么不用 `Store.Open`**：`Store.Open` 会 `secureMkdirAll(root/artifacts)`，即使 `Lock=false` 也可能创建目录；Web 服务必须保证“打开一个 session 不产生任何写”。因此新增只读 facade：

```go
package session

// Archive 是 Web/审计使用的只读视图；不取锁、不创建目录、不写文件。
type Archive interface {
	List(ctx context.Context, f ListFilter) ([]Summary, error)
	Snapshot(ctx context.Context, id string) (Snapshot, error)
	Artifact(ctx context.Context, id string, ref model.ArtifactRef) ([]byte, error)
}

type Snapshot struct {
	Meta     Meta
	Events   []model.Event
	Warnings []string
	Lock     LockState
}

type LockState struct {
	Present     bool      `json:"present"`
	Live        bool      `json:"live"`
	Stale       bool      `json:"stale"`
	PID         int       `json:"pid,omitempty"`
	Host        string    `json:"host,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	HeartbeatAt time.Time `json:"heartbeatAt,omitempty"`
}

func NewArchive(workDir string) Archive
```

实现要点：

- 把 `NewStore` 内部拆出一个返回 `*store` 的 `newStore`（现有 `NewStore`/`NewStoreWithClock` 只做接口包装），这样 `archive` 能复用未导出的 `resolveSessionID`/`readMetaFile`/`readEventsFile`，又不需要调用 `Open`。
- `List` 复用现有 `store.List`（只读）并补充 `LockState`/`Live`；不调用 `Open`。
- `Snapshot` 用 `resolveSessionID` 精确解析（Web URL 使用完整 ID；不做前缀歧义）；
- 复用 `readMetaFile`、`readEventsFile`、`readLockOwner`；
- `LockState.Live = present && heartbeat 在 staleAfter（默认 120s）内`；
- `Artifact`：
  - 校验 `ref.SessionID` 为空或等于 `id`；
  - `cleanRelativeName` 后必须位于 `artifacts/`；
  - `filepath.Rel` + `EvalSymlinks` 双重校验；
  - 单文件上限 8 MiB（可配），超出返回 `artifact_too_large`；
  - 只读 `os.ReadFile`，不创建目录。
- 事件读取上限：单 session 64 MiB / 50,000 条；超出返回 `session_too_large` 并建议用 `ant show`/CLI 排障。
- 单元测试必须断言：调用 `Snapshot`/`Artifact` 前后目录树无新增文件（可用 mtime/树快照对比）。

### 22.6.3 Web Server

```go
package web

type Deps struct {
	WorkDir string
	HomeDir string
	Config  config.View      // 可为 nil：配置非法时仍可浏览历史
	Archive session.Archive
	Clock   func() time.Time
	Logger  logx.Logger
	Version string
}

type Server struct { /* mux, projector, archive, cfg, ... */ }

func New(deps Deps) (*Server, error)
func (s *Server) Handler() http.Handler
func (s *Server) Serve(ctx context.Context, ln net.Listener) error
```

`Serve` 内部：

- `http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5s, IdleTimeout: 60s}`；
- 收到 `ctx.Done()` 后 `Shutdown(5s)`；
- 返回 `http.ErrServerClosed` 以外的错误给 CLI。

中间件顺序：

```text
recover → request log → security headers → host check → mux
```

- `recover`：捕获 panic，返回 500 JSON，记录 stack（不返回给浏览器）。
- `host check`：只允许 `localhost`、`127.0.0.1`、`[::1]`（含端口）；`net.SplitHostPort` 解析后转小写、去掉末尾点，忽略 `X-Forwarded-Host`；防 DNS rebinding。
- `security headers`：CSP、nosniff、no-referrer、frame-ancestors none。
- 无 CORS 头；无 Cookie。

### 22.6.4 路由与 API

| 方法 | 路径 | 用途 | 备注 |
| --- | --- | --- | --- |
| GET | `/` | SPA 入口 | `index.html`；`Cache-Control: no-cache` |
| GET | `/assets/*` | 静态资源 | `Cache-Control: public, max-age=3600`；ETag |
| GET | `/api/health` | 健康检查 | `{ok, version, workspace, home, readonly:true}` |
| GET | `/api/sessions` | 会话列表 | `limit`（默认 100，最大 500）、`mode`、`status`、`q` |
| GET | `/api/sessions/{id}` | 会话详情 | 完整 `SessionView`；支持 `beforeTurn`/`limit` |
| GET | `/api/sessions/{id}/events` | 增量事件 | `after=seq`；调试/轮询兜底 |
| GET | `/api/sessions/{id}/stream` | SSE | `changed`/`ping`；不传全量 |
| GET | `/api/sessions/{id}/artifacts/{path...}` | artifact | 仅 `artifacts/`；文本预览/二进制下载 |
| GET | `/api/sessions/{id}/plan` | 计划 | `artifacts/plan.md` / `artifacts/plan/round-N.md`；兼容返回路径里的 `<session>/plan.md` |
| GET | `/api/workspace/file` | 文件预览 | `path` 相对 workspace；排除 `.git`、`.ant/sessions`、`.ant/logs`、`.ant/settings.json`、`.ant/models.json` |
| GET | `/api/config` | 合并配置（脱敏） | settings/models/origins/warnings |
| GET | `/api/home` | home 文件清单 | 只返回名称/大小/时间，不返回内容 |
| GET | `/api/home/prompt` | home prompt 预览 | 仅 `prompt.md`、`prompts/<known>.md` |
| GET | `/api/project` | 项目上下文 | `.ant/project.md` + `.ant/project.meta.json` |

一期**没有任何 POST/PUT/PATCH/DELETE**。这是“无账号、无 CSRF、只读”的直接保证；二期加写接口时再单独设计防护（§22.15）。

artifact URL 约定：`/api/sessions/{id}/artifacts/{path...}` 里的 `path` 是**相对 `artifacts/` 的名字**（如 `message-...txt`、`plan/round-1.md`）；服务端拼回 `artifacts/<path>` 再调用 `Archive.Artifact`。JSON 里的 `ref.name` 仍保留 `artifacts/` 前缀，前端构造 URL 时去掉该前缀，避免出现 `/artifacts/artifacts/...`。

#### `GET /api/sessions/{id}` 响应示例

```json
{
  "id": "20261106-234456-df479f",
  "meta": {
    "schemaVersion": 1,
    "id": "20261106-234456-df479f",
    "workspace": "/mnt/workspace/work/go/ant",
    "createdAt": "2026-11-06T23:44:56.226Z",
    "updatedAt": "2026-11-07T07:22:23.390Z",
    "status": "idle",
    "mode": "ask",
    "title": "这个项目做了什么",
    "model": "ornith-1.5:9b",
    "turnCount": 5,
    "lastSeq": 127,
    "usage": {"input": 160611, "output": 3057, "cached": 127216, "reasoning": 0, "estimated": false}
  },
  "warnings": [],
  "turns": [
    {
      "turn": 1,
      "status": "ok",
      "startedAt": "2026-11-06T23:45:52.610Z",
      "endedAt": "2026-11-06T23:47:16.660Z",
      "durationMs": 84050,
      "input": "这个项目做了什么",
      "mode": "ask",
      "usage": {"input": 9281, "output": 853},
      "changed": [],
      "checks": [],
      "items": [
        {"seq": 2, "kind": "user", "text": "这个项目做了什么"},
        {"seq": 9, "kind": "assistant", "text": "这个项目是 **ant** ...", "ref": {"name": "artifacts/message-...txt", "bytes": 11264}},
        {"seq": 10, "kind": "tool", "tool": {"id": "H5I4...", "name": "read", "args": {"path": "README.md"}, "approval": "auto", "status": "ok", "durationMs": 0}}
      ],
      "requests": [
        {
          "seq": 7,
          "node": "ask",
          "step": "ask.ask",
          "model": "ornith-1.5:9b",
          "provider": "ollama",
          "attempt": 1,
          "status": "ok",
          "durationMs": 55520,
          "messageCount": 3,
          "estimatedInputTokens": 2547,
          "finishReason": "tool_calls",
          "usage": {"input": 2570, "output": 79},
          "toolCalls": 2
        }
      ]
    }
  ],
  "approvals": [],
  "artifacts": [
    {"name": "artifacts/message-20261106-234553-2.txt", "bytes": 11264, "sha256": "sha256:..."}
  ],
  "plan": null
}
```

字段约定：

- 时间一律 RFC3339（UTC 或带偏移；前端本地化）；
- `usage` 字段与 `model.Usage` 同名；
- `text` 只在小于 64 KiB 时内联；更大时只给 `ref`，前端按需拉 artifact；
- `args` 原样 JSON；非法 JSON 退化为字符串；
- 缺失数据用 `null` 或 `"未记录"`，不编造。

#### `GET /api/sessions` 响应示例

```json
{
  "workspace": "/mnt/workspace/work/go/ant",
  "home": "/home/xiw/.ant",
  "sessions": [
    {
      "id": "20261106-234456-df479f",
      "title": "这个项目做了什么",
      "mode": "ask",
      "status": "idle",
      "live": false,
      "stale": false,
      "model": "ornith-1.5:9b",
      "createdAt": "2026-11-06T23:44:56.226Z",
      "updatedAt": "2026-11-07T07:22:23.390Z",
      "turnCount": 5,
      "tokens": 163668,
      "lastEvent": "turn_completed"
    }
  ]
}
```

#### `GET /api/config` 响应

复用 `ant config` 的脱敏投影（[§3.2](01-cli.md#32-命令)）：`settings`、`models`、`origins`、`warnings`。**不返回** `providers` 的 `apiKey`/`headers`，也不返回环境变量。

### 22.6.5 投影器

```go
type Projector struct {
	archive session.Archive
	cfg     config.View
	cache   *viewCache // 可选：按 mtime/size 失效
}

func (p *Projector) List(ctx context.Context, f session.ListFilter) ([]SessionSummary, error)
func (p *Projector) View(ctx context.Context, id string, opt ViewOptions) (*SessionView, error)
```

`View` 的步骤：

1. `archive.Snapshot(ctx, id)`；
2. 按 §22.5.5 顺序遍历事件，构建 `TurnView`/`ItemView`/`RequestView`/`ToolView`；
3. 对每个 `contentRef`/`inputRef`/`outputRef` 按需 `archive.Artifact`，带缓存；
4. 计算 turn/节点/工具/请求的耗时与汇总 usage；
5. 计算 `changes`（`turn_completed.changed` + `edit`/`write` 参数）；
6. 返回 `SessionView`，同时返回 `warnings`（坏行、缺失 artifact、未知事件）。

**性能**：

- 单次投影 O(events + artifacts)；10k 事件目标 < 500ms；
- 缓存键：`id + meta.mtime + events.size + events.mtime`；
- 同一 session 并发请求用 `singleflight` 语义（`sync.Mutex` + map）合并；
- 只对当前详情读取 artifact；列表不读 artifact。

### 22.6.6 缓存与 ETag

- `GET /api/sessions/{id}`：`ETag: "<eventsSize>-<eventsMtime>-<metaMtime>"`；`If-None-Match` 命中返回 304。
- `GET /api/sessions`：`ETag` 由 sessions 目录 mtime + 各 meta mtime 组成；列表较小，可 1s 内存缓存。
- 静态资源：按文件内容 hash 或构建版本做 ETag；HTML no-cache。
- 所有 API 默认 `Cache-Control: no-store`（除 304 协商）。

### 22.6.7 实时更新

一期用 **SSE 通知 + 客户端按需重取**，不把投影逻辑复制到前端：

```text
GET /api/sessions/{id}/stream
Content-Type: text/event-stream

event: changed
data: {"lastSeq":128,"updatedAt":"..."}

event: ping
data: {}
```

- 服务端每 1s `stat` `events.jsonl` 与 `meta.json`；`size/mtime` 变化时发 `changed`；
- 每 15s 发 `ping`，防止代理超时；
- 客户端收到 `changed` 后带 `If-None-Match` 重新 GET `/api/sessions/{id}`；304 则忽略；
- 连接上限 32；超过返回 503；`ctx.Done()` 立即退出，避免 goroutine 泄漏；
- 不支持 SSE 的客户端降级为 2s 轮询 `/api/sessions/{id}/events?after=lastSeq`。

### 22.6.8 错误模型

统一返回：

```json
{
  "error": {
    "class": "not_found",
    "code": "session_not_found",
    "message": "找不到 session ...",
    "hint": "刷新列表，或确认 ant serve 在正确的 workspace 启动",
    "details": {}
  }
}
```

Web 层错误 class 使用 `not_found` / `bad_request` / `too_large` / `unavailable` / `internal` 即可；只有来自 session/config 的 `*model.Error` 才保留其原始 class。HTTP 映射：

| 场景 | 状态码 |
| --- | --- |
| session/artifact 不存在 | 404 |
| 路径非法、越界 | 400 |
| 文件过大 | 413 |
| 并发流过多 | 503 |
| 服务内部错误 | 500 |
| 非回环 Host | 403 |

## 22.7 前端设计

### 22.7.1 零构建

- 原生 ES Modules，`<script type="module" src="/assets/app.js">`；
- 无 React/Vue/Svelte、无 Vite/Webpack、无 npm 依赖、无 CDN；
- 目标浏览器：最近两个大版本的 Chrome/Edge/Firefox/Safari；
- 理由：仓库是纯 Go 工程，构建与测试只需要 `go test`/`go build`；引入 Node 构建链会显著增加发布与供应链成本（[§16.2](14-engineering.md#162-依赖选择)）。

### 22.7.2 模块划分

```text
static/
├── index.html
└── assets/
    ├── app.css          # 设计变量、三栏布局、消息/轨迹/差异样式
    ├── app.js           # 启动、路由、状态、事件绑定
    ├── api.js           # fetch 封装、ETag、SSE、错误转换
    ├── render.js        # 对话/工具卡/摘要/右栏渲染
    ├── trajectory.js    # 轨迹表、时间线、检查器
    ├── diff.js          # diff 行渲染、折叠/展开、复制
    └── markdown.js      # 安全 Markdown 子集
```

| 模块 | 职责 | 关键导出 |
| --- | --- | --- |
| `app.js` | 解析 hash 路由、加载列表/详情、维护 UI 状态、快捷键 | `boot()` |
| `api.js` | `getJSON`、`getText`、`openStream`、`ApiError` | `api.sessions()`、`api.session(id)` |
| `render.js` | 纯函数 `renderSession(view)`、`renderTool(tool)`、`renderTurnSummary(turn)` | `renderChat` |
| `trajectory.js` | `renderTrajectory(view)`、`renderInspector(row)` | `renderTrajectory` |
| `diff.js` | `renderDiff(hunks)`、`diffStats` | `renderDiff` |
| `markdown.js` | `renderMarkdown(text)`，先转义再解析 | `renderMarkdown` |

### 22.7.3 状态与路由

Hash 路由，便于静态托管与刷新：

```text
#/                          → 空态/最近会话
#/sessions/<id>             → 对话
#/sessions/<id>/trajectory  → 轨迹
#/sessions/<id>?turn=3      → 定位到第 3 轮
#/settings                  → 设置对话框
```

状态：

```js
const state = {
  route: { name: 'session', id: null, view: 'chat' },
  workspace: null,
  home: null,
  sessions: [],
  session: null,
  filters: { q: '', mode: '', status: '' },
  ui: { sidebar: true, right: 'changes', theme: 'auto', density: 'standard' },
  lastSeq: 0,
};
```

数据流：

```text
hashchange → route → api.session(id) → render(app)
SSE changed → api.session(id, {ifNoneMatch}) → 304? 忽略 : merge+render
筛选输入 → 本地过滤 sessions（或重新 GET）
```

### 22.7.4 渲染原则

- **服务端投影，前端只渲染**：diff 行号、tool 状态、turn 汇总都在 Go 侧算好；JS 不重复实现事件状态机。
- **增量更新**：新事件只追加/替换对应 turn；不整页重绘；保持滚动位置。
- **虚拟化**：单 session > 200 个 turn 时，只渲染可视 turn + 上下缓冲；轨迹表 > 1000 行时按 turn 折叠。
- **空/错/加载**三态必须有；任何单项失败只降级该卡片。
- **可复制**：session ID、路径、命令、代码、错误、diff 都可复制。
- **可访问**：语义化标签、`aria-live` 状态、焦点环、键盘操作、颜色+符号双编码。

### 22.7.5 安全渲染

- Markdown 先 `escapeHTML`，再解析白名单语法；禁止原始 HTML。
- 链接只允许 `http:`/`https:`/相对路径；`target="_blank"` 加 `rel="noopener noreferrer"`。
- 代码块用 `textContent` 写入，不拼 HTML。
- diff 行文本 `textContent`；行号/符号单独元素。
- 不使用 `innerHTML` 渲染任何服务端文本；模板只拼接结构化字段，文本走 `textContent`。
- CSP 禁止内联脚本；`script-src 'self'`；样式放外部 CSS。

### 22.7.6 视觉规范

| 变量 | 浅色 | 深色 |
| --- | --- | --- |
| 背景 | `#fafafa` | `#111315` |
| 面板 | `#ffffff` | `#181b1f` |
| 边框 | `#e5e7eb` | `#2a2f36` |
| 主文本 | `#18181b` | `#e8eaed` |
| 次文本 | `#6b7280` | `#9aa0a6` |
| 强调 | `#2563eb` | `#60a5fa` |
| 成功 | `#15803d` | `#4ade80` |
| 警告 | `#b45309` | `#fbbf24` |
| 危险 | `#b91c1c` | `#f87171` |

- 字体：系统 UI 字体；代码 `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`。
- 内容最大宽度 860px；表格/代码可横向滚动。
- 模式 badge：`code` 蓝、`plan` 紫、`ask` 绿、`council` 琥珀；状态点另用颜色。
- 动效克制：只用于状态脉冲、面板滑入；尊重 `prefers-reduced-motion`。

## 22.8 文件清单（在什么位置建立什么文件）

### 22.8.1 新增文件

| 路径 | 职责 | 关键内容 | 预估 |
| --- | --- | --- | --- |
| `internal/session/archive.go` | 只读归档 facade | `Archive`/`Snapshot`/`LockState`；`List`/`Snapshot`/`Artifact`；路径与大小上限 | ~220 行 |
| `internal/session/archive_test.go` | 归档单测 | 不写目录、坏行、缺失 artifact、路径穿越、锁状态 | ~200 行 |
| `internal/web/server.go` | Server 生命周期 | `Deps`、`New`、`Handler`、`Serve`、中间件 | ~180 行 |
| `internal/web/routes.go` | 路由与 handler | `GET /api/*`、静态 fallback、错误 JSON、参数解析 | ~300 行 |
| `internal/web/types.go` | API DTO | `SessionSummary`/`SessionView`/`TurnView`/`ItemView`/`ToolView`/`RequestView`/`DiffHunk` | ~220 行 |
| `internal/web/project.go` | 事件投影 | `Projector`、turn/tool/request 状态机、usage 汇总、artifact 读取 | ~500 行 |
| `internal/web/diff.go` | unified diff 解析 | `ParseUnifiedDiff`、`WriteDiff`、行号/统计/截断 | ~200 行 |
| `internal/web/artifacts.go` | 安全读取 | workspace 文件、home 白名单、artifact、大小/符号链接校验、缓存 | ~160 行 |
| `internal/web/stream.go` | SSE | `changed`/`ping`、连接上限、ctx 退出 | ~120 行 |
| `internal/web/static.go` | 嵌入静态资源 | `//go:embed static`、`fs.Sub`、SPA fallback | ~40 行 |
| `internal/web/static/index.html` | 页面骨架 | 三栏、空态、对话框挂载点 | ~90 行 |
| `internal/web/static/assets/app.css` | 样式 | 变量、布局、消息、工具卡、diff、轨迹、响应式 | ~700 行 |
| `internal/web/static/assets/app.js` | 启动/路由/状态 | 初始化、hash 路由、列表、SSE、快捷键 | ~350 行 |
| `internal/web/static/assets/api.js` | API 封装 | fetch、ETag、错误、SSE | ~120 行 |
| `internal/web/static/assets/render.js` | 对话渲染 | 会话头、turn、消息、工具卡、审批、摘要、右栏 | ~600 行 |
| `internal/web/static/assets/trajectory.js` | 轨迹渲染 | 表格、时间线、检查器、搜索/折叠 | ~450 行 |
| `internal/web/static/assets/diff.js` | diff 渲染 | 行号、+/-、折叠、复制 | ~180 行 |
| `internal/web/static/assets/markdown.js` | Markdown 子集 | 转义、代码块、列表、链接 | ~200 行 |
| `internal/web/testdata/...` | 测试 fixture | 合成 session：正常、失败、中断、坏行、缺失 artifact | ~10 个文件 |
| `internal/cli/serve.go` | serve 命令 | 解析路径、best-effort 配置、启动/打印/信号/退出码 | ~140 行 |
| `internal/cli/serve_test.go` | 命令测试 | 参数、回环校验、`:0` 启动、优雅关闭 | ~160 行 |
| `docs/20-web-serve.md` | 本文 | 设计契约 | — |

### 22.8.2 修改文件

| 路径 | 修改点 |
| --- | --- |
| `internal/cli/args.go` | `commandNames` 增加 `serve`；`options` 增加 `listen`/`open`；解析 `--listen`/`--open`；usage/commandHelp 增补 |
| `internal/cli/app.go` | `New` 在 `version`/`help` 之后、`loadConfig` 之前处理 `serve`；`Run` 分发 `runServe`；`Run` 的 ctx 取消→退出码 3 逻辑对 `serve` 特判；信号增加 `SIGTERM`；增加 `homeDirOverride`/`env`/`cfgOverride` 字段；增加 `homeDir()` helper |
| `README.md` | 模块索引增加第 20 行；结构说明从“20 个模块（0–19）”改为“20 个模块 + 补充模块 20” |
| `docs/01-cli.md` | 命令表增加 `ant serve`（建议同步） |
| `docs/17-adr.md` | 合并 D46–D50（建议同步） |
| `docs/16-traceability.md` | 增加 R-WEB 追溯行（建议同步） |

### 22.8.3 明确不新增

- 不新增 `pkg/web` 或公共 SDK（[D37](17-adr.md#19-设计决策记录adr)）。
- 不新增 `go.mod` 依赖。
- 不新增数据库、索引文件、缓存目录。
- 不新增 `.ant/serve/` 状态目录；服务本身不落盘。
- 不新增账号/登录相关表、Cookie、token 文件。

### 22.8.4 目标目录树

```text
cmd/ant/main.go                         # 不变
internal/
├── cli/
│   ├── args.go                          # + serve 命令/参数
│   ├── app.go                           # + serve 分支/分发
│   ├── serve.go                         # 新增
│   └── serve_test.go                    # 新增
├── session/
│   ├── archive.go                       # 新增
│   └── archive_test.go                  # 新增
└── web/
    ├── server.go
    ├── routes.go
    ├── types.go
    ├── project.go
    ├── diff.go
    ├── artifacts.go
    ├── stream.go
    ├── static.go
    ├── static/
    │   ├── index.html
    │   └── assets/
    │       ├── app.css
    │       ├── app.js
    │       ├── api.js
    │       ├── render.js
    │       ├── trajectory.js
    │       ├── diff.js
    │       └── markdown.js
    └── testdata/
docs/20-web-serve.md                    # 本文
```

## 22.9 组合根与命令接入

### 22.9.1 `args.go` 变更

```go
var commandNames = map[string]bool{
	"new": true, "sessions": true, "show": true, "init": true,
	"models": true, "config": true, "version": true, "help": true,
	"serve": true, // 新增
}

type options struct {
	// ...既有字段
	listen string // --listen，默认 127.0.0.1:7788
	open   bool   // --open
}
```

解析：

```go
case "--listen":
	v, err := value()
	if err != nil {
		return opts, err
	}
	opts.listen = v
case "--open":
	opts.open = true
```

默认值与校验：

- `parseArgs` 初始化 `listen: "127.0.0.1:7788"`；
- `opts.command == "serve"` 时：
  - 拒绝 `opts.prompt != ""` 或 `len(opts.rest) > 0`（serve 不接受任务文本）；
  - 拒绝 `--json`（stdout 不输出 NDJSON；服务日志走 stderr）；
  - `--listen` 必须是 `host:port`，端口 1–65535；`host` 为空、`0.0.0.0`、`::` 一律拒绝；
  - 允许 `127.0.0.1:0` 用于测试；实际端口从 listener 读取后打印。

`usageText` 与 `commandHelp` 增补：

```text
ant serve [--listen 127.0.0.1:7788] [--open]

  --listen string   监听地址；只允许回环地址（默认 127.0.0.1:7788）
  --open            启动后尝试用系统浏览器打开
```

### 22.9.2 `app.go` 分支

`New` 在 `version`/`help` 之后、`loadConfig` 之前处理 serve：

```go
if opts.command == "version" || opts.command == "help" {
	return app, nil
}
if opts.command == "serve" {
	// serve 自己做 best-effort 配置加载：配置非法也要能看历史。
	app.homeDirOverride = ov.HomeDir
	app.env = stdio.Env
	app.cfgOverride = ov.Config
	return app, nil
}
```

`Run` 分发：

```go
// version/help/models/config/sessions/show/init 分支保持现状；
// 在它们之后、engine 检查之前加入：
case "serve":
	return a.runServe(ctx)
```

`App` 增加：

```go
homeDirOverride string
env             []string
cfgOverride     config.View // 测试注入用；对应 Overrides.Config
```

`workDir()` 在 `cfg == nil` 时回落到 `workDirOverride`/`os.Getwd()`，与现有逻辑一致；新增 `homeDir()`：

```go
func (a *App) homeDir() string {
	if p, ok := a.cfg.(config.Paths); ok && p.HomeDir() != "" {
		return p.HomeDir()
	}
	if p, ok := a.cfgOverride.(config.Paths); ok && p.HomeDir() != "" {
		return p.HomeDir()
	}
	if a.homeDirOverride != "" {
		return a.homeDirOverride
	}
	if v := envValue(a.env, "ANT_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".ant")
	}
	return ""
}
```

`envValue` 是一个 10 行的 `KEY=VALUE` 线性查找 helper（不要为此引入依赖）。

### 22.9.3 `serve.go` 伪代码

```go
func (a *App) runServe(ctx context.Context) int {
	wd := a.workDir()
	if abs, err := filepath.Abs(wd); err == nil {
		wd = abs
	}

	// 1. 尽力加载配置；失败只告警，不阻塞历史浏览。
	cfg, cfgErr := a.loadConfigBestEffort(ctx, wd)
	if cfgErr != nil {
		fmt.Fprintf(a.stdio.Err, "[ant] 配置告警：%s\n", formatError(cfgErr))
	}

	// 2. 只读归档 + Web 服务。
	archive := session.NewArchive(wd)
	logger := a.serveLogger(cfg)
	srv, err := web.New(web.Deps{
		WorkDir: wd,
		HomeDir: a.homeDir(),
		Config:  cfg,
		Archive: archive,
		Clock:   a.clock,
		Logger:  logger,
		Version: version.Version,
	})
	if err != nil {
		fmt.Fprintln(a.stdio.Err, "[ant] serve 初始化失败："+formatError(err))
		return 1
	}

	// 3. 监听并校验回环。
	ln, err := net.Listen("tcp", a.opts.listen)
	if err != nil {
		fmt.Fprintf(a.stdio.Err, "[ant] 无法监听 %s：%v\n", a.opts.listen, err)
		return 2
	}
	if !isLoopback(ln.Addr()) {
		_ = ln.Close()
		fmt.Fprintln(a.stdio.Err, "[ant] 拒绝非回环地址；ant serve 只允许 127.0.0.1/localhost/[::1]")
		return 2
	}

	url := "http://" + ln.Addr().String()
	fmt.Fprintf(a.stdio.Out, "[ant] serve: %s\n", url)
	fmt.Fprintf(a.stdio.Out, "      workspace: %s\n", wd)
	fmt.Fprintf(a.stdio.Out, "      home:      %s\n", a.homeDir())
	fmt.Fprintln(a.stdio.Out, "      只读模式 · 无需登录 · Ctrl-C 停止")

	if a.opts.open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(a.stdio.Err, "[ant] 无法自动打开浏览器：%v\n", err)
		}
	}

	// 4. 阻塞到信号；Serve 内部负责优雅关闭。
	if err := srv.Serve(ctx, ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(a.stdio.Err, "[ant] serve 异常退出："+formatError(err))
		return 1
	}
	fmt.Fprintln(a.stdio.Out, "[ant] serve 已停止")
	return 0
}
```

辅助：

```go
func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
```

`openBrowser` 放在 `serve.go` 内即可（约 15 行）：`darwin` 用 `open`、`windows` 用 `rundll32 url.dll,FileProtocolHandler`、其他用 `xdg-open`；失败只告警，不影响服务。

`serveLogger` 也放在 `serve.go`：用 `logx.NewWithWriter(logx.LevelInfo, a.stdio.Err)`（或按 `--log-level` 解析），**不要**调用 `newLogger`，因为后者会写 `.ant/logs`，违反只读。

`loadConfigBestEffort` 先看 `a.cfgOverride`（测试注入），否则复用现有 `loadConfig`，但把错误返回给调用方；不要吞掉 warning。配置为 `nil` 时 `web.Deps.Config == nil`，`/api/config` 返回 `{available:false, error:...}`，页面顶部显示配置告警。

### 22.9.4 启动输出与退出码

```text
$ ant serve
[ant] serve: http://127.0.0.1:7788
      workspace: /mnt/workspace/work/go/ant
      home:      /home/xiw/.ant
      只读模式 · 无需登录 · Ctrl-C 停止
```

| 情况 | 退出码 |
| --- | --- |
| 正常停止（SIGINT/SIGTERM，优雅关闭） | 0 |
| 参数/监听地址非法、端口占用 | 2 |
| 服务运行时错误 | 1 |

> 与 [§3.4](01-cli.md#34-退出码) 的“用户中断=3”不同：`serve` 是长期进程，Ctrl-C 是正常停止方式，返回 0 更利于 `systemd`/脚本判断。此例外必须在 `ant help`/文档中写清楚。

**必须同步修改 `Run` 的收尾逻辑**：现有 `Run` 在 `ctx.Err() != nil && code == 0` 时把退出码改成 3（见 `internal/cli/app.go` 的 `Run`）。serve 分支要特判：

```go
code := app.Run(ctx)
if ctx.Err() != nil && code == 0 && app.opts.command != "serve" {
	code = 3
}
```

同时把 `signal.NotifyContext(ctx, os.Interrupt)` 扩为 `os.Interrupt, syscall.SIGTERM`（Windows 下无影响），这样 systemd/容器发 SIGTERM 也能优雅关闭。若不改这两处，文档承诺的“SIGINT 退出 0 / SIGTERM 优雅关闭”不会成立。

### 22.9.5 与现有命令的隔离

- `ant serve` 不读取 `-p/-m/-s/--new`；这些参数在 serve 下直接报用法错误，避免“看起来设置了但没生效”。
- `--log-level` 仍生效：影响服务日志级别，不影响页面数据。
- `--no-color` 对 serve 无意义，可接受但忽略；不要报错。
- `--json` 在 serve 下报用法错误。

## 22.10 安全与隐私

### 22.10.1 威胁模型

`ant serve` 的默认部署是**单用户开发机、回环地址、只读**。它不试图防御：

- 已经拿到当前 OS 用户权限的恶意进程（它们可以直接读 `~/.ant` 与 workspace 文件）；
- 物理访问、内存转储、浏览器扩展读取页面内容；
- 用户主动把页面端口转发到公网。

它要防御：

| 威胁 | 防护 |
| --- | --- |
| 其他本地用户通过 HTTP 读取 session | v1.4 默认信任本机用户（仅回环）；多用户主机必须加 token 或不要运行（见下） |
| 恶意网页通过浏览器访问 `127.0.0.1` | Host 校验 + 无 CORS + 无写接口 + CSP；一期只读，CSRF 无副作用 |
| DNS rebinding | Host 只允许 `localhost`/`127.0.0.1`/`[::1]` |
| 路径穿越/符号链接逃逸 | 见 §22.10.3 |
| 密钥泄露 | 只下发解析后的模型元数据；不返回 provider key/headers/env |
| XSS（会话内容含恶意 HTML） | Markdown 白名单 + `textContent` + CSP |
| 大文件/大 session 拖垮进程 | 大小/条数上限 + 分页 + 413 |

**多用户主机的残余风险**：任何本地用户都能连接回环端口；服务端以启动用户身份读文件。若机器不是单人使用，一期建议不要把 workspace 放在共享目录，或在二期加 `--token`（URL 带随机 token，不弹登录页、不涉及账号）。这不是账号登录，但属于访问控制；默认关闭，保持“无登录”的产品要求。

### 22.10.2 无账号设计

明确不做：

- 不做 `/login`、`/logout`、`/auth/*`、`/account/*`、`/oauth/*`；
- 不设置任何身份 Cookie、`Authorization` 头、localStorage 用户信息；
- 不在 UI 出现头像、用户名、登录/退出、余额、配额、云端会话；
- 不引入账号 SDK、不读取 `~/.dsh` 或任何外部账号配置；
- 不把“监听令牌”当登录：一期直接无令牌；二期如加 token，只在 URL 中传递，不进入 UI。

测试用静态扫描保证嵌入的 `static/` 不含 `login`、`sign in`、`账号`、`登录`、`account` 等账号相关文案（允许“本机配置/设置”）。

### 22.10.3 路径安全

| 输入 | 规则 |
| --- | --- |
| session ID | 完整 ID 正则 `^\d{8}-\d{6}-[0-9a-f]{6}$`；在 `sessions` 根下解析；不允许前缀（避免歧义）；`EvalSymlinks` 后必须仍在根内 |
| artifact 名称 | `cleanRelativeName`；必须以 `artifacts/` 开头；`filepath.Rel` 校验；`EvalSymlinks` 校验；拒绝 `..`、绝对路径、盘符、NUL |
| workspace 文件 | 必须相对路径；`Clean` 后不以 `..` 开头；`EvalSymlinks` 后仍在 workspace 内；拒绝 `.git/`、`.ant/sessions/`、`.ant/logs/`、`.ant/settings.json`、`.ant/models.json`（配置走 `/api/config`） |
| home 文件 | 白名单：`settings.json`、`models.json`、`prompt.md`、`prompts/{code,ask,verifier,deliberate,aggregation}.md`；`settings/models` 只做解析，不下发原文 |
| 查询参数 | `path` 长度 ≤ 1024；拒绝控制字符；错误统一 400，不回显绝对路径 |

大小上限：

| 内容 | 上限 |
| --- | --- |
| `events.jsonl` | 64 MiB / 50,000 事件 |
| 单 artifact | 8 MiB |
| workspace 文件预览 | 2 MiB |
| home prompt | 256 KiB |
| API 响应 | 32 MiB（硬上限） |

### 22.10.4 响应头

```text
Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self';
  script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none';
  frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
```

- 不加 `Access-Control-Allow-Origin`；浏览器同源即可。
- 静态资源可加 `Cache-Control: public, max-age=3600`；API 默认 `no-store`。
- 不使用内联脚本；`index.html` 不写 `onclick`；也不写内联 `style=""`（CSP 未放行 `unsafe-inline`）。

### 22.10.5 只读保证

- `session.Archive` 不调用 `Store.Open`，不创建 `artifacts/`；
- 所有文件操作只有只读 API：`os.Open`/`os.ReadFile`/`os.Stat`/`os.ReadDir`/`filepath.EvalSymlinks`；不调用 `Mkdir`/`WriteFile`/`Remove`/`Lock`；
- 服务日志只写 stderr，不写 `.ant/logs`；
- 前端没有提交按钮/表单；一期没有 POST；
- 测试用目录快照断言 `ant serve` 前后 `.ant` 无变化（除操作系统 atime）。

### 22.10.6 密钥与敏感内容

- `/api/config` 只返回 `settings`、模型别名/能力/价格（可选）、origins、warnings；不返回 `providers` 的 `apiKey`/`headers`。
- `/api/home` 只返回文件元数据（名称/大小/时间），不返回 `models.json`/`settings.json` 原文。
- `.ant/logs` 不提供读取接口；日志可能含 bash 输出与路径。
- artifact/工具输出是用户自己的数据，页面可读；预览时显示“可能含敏感内容”的提示，不自动外发。
- 服务端日志不记录消息正文、工具输出、artifact 内容；只记录方法、路径、状态、耗时、错误码。

## 22.11 测试与验收

### 22.11.1 单元测试

| 包 | 用例 |
| --- | --- |
| `internal/session` | `Archive.Snapshot` 不创建目录；坏行 warning；缺失 meta；缺失 events；lock live/stale；artifact 路径穿越；符号链接逃逸；大小上限；`List` 排序/过滤 |
| `internal/web` | `ParseUnifiedDiff`：标准 diff、代码围栏、无 hunk、坏行号、截断；`WriteDiff`；projector：正常 turn、tool loop、失败、中断、缺 artifact、未知事件、重复 result、plan 路径回退；usage 汇总；ETag/304；Host 校验；CSP；路径 400/404/413 |
| `internal/cli` | `--listen` 校验；serve 拒绝 prompt/`--json`；`127.0.0.1:0` 启动；优雅关闭；配置非法仍启动；端口占用退出 2 |

### 22.11.2 集成测试

- `httptest.NewServer(web.New(...).Handler())` + 临时 workspace fixture：
  - `GET /api/sessions` 返回 2 个 session；
  - `GET /api/sessions/{id}` 返回完整 turns/tools/diffs；
  - `GET /api/sessions/{id}/artifacts/...` 命中；
  - `GET /api/config` 不含 `apiKey`；
  - `GET /api/health` 200；
  - `If-None-Match` 304；
  - `Host: evil.example` 403；
  - `GET /api/workspace/file?path=../../etc/passwd` 400。
- 真实进程 smoke（可选）：`go run ./cmd/ant serve --listen 127.0.0.1:0`，解析 stdout URL，`curl` 健康检查，SIGINT 后退出 0。

### 22.11.3 手工验收

1. 在 workspace 运行 `ant serve`；终端打印 URL、workspace、home、只读提示。
2. 打开页面：无登录页、无账号区、无 401；左栏出现当前 workspace 的 session。
3. 点击一个包含工具调用的 session：用户输入、助手 Markdown、`read`/`bash`/`edit` 卡片、diff、本轮摘要均正确。
4. 切到“轨迹”：请求/工具行、时间、token、详情 tab 正常；“思考”显示“未记录”而不是空白或伪造。
5. 打开右栏“改动”：文件列表与 `+/-` 统计正确；点击文件显示当前内容；点击 diff 展开。
6. 打开“本机配置”：workspace/home/版本/模型/来源/告警正确；无 apiKey。
7. 在另一个终端对同一 session 运行 `ant "..."`：页面 2s 内出现新 turn/新工具行，滚动位置不跳。
8. 窄屏 800px：侧栏收成图标栏；右栏可覆盖；键盘可达。
9. `curl -H 'Host: evil' http://127.0.0.1:7788/api/health` 返回 403；`ant serve --listen 0.0.0.0:7788` 退出 2。
10. Ctrl-C：优雅停止，退出码 0；`.ant` 下无新增文件。

### 22.11.4 性能目标

| 指标 | 目标 |
| --- | --- |
| 列表 100 session | < 100ms（本地） |
| 详情 10k 事件投影 | < 500ms |
| 首屏可交互 | < 300ms（本地） |
| 常驻内存 | < 150MB（100 session 缓存） |
| 静态资源总量 | < 250KB（未压缩） |
| SSE 通知延迟 | < 1.5s（1s 轮询 + 网络） |

### 22.11.5 验收清单

- [ ] R-WEB-01 `ant serve` 可用，默认 `127.0.0.1:7788`
- [ ] R-WEB-02 启动输出 URL/workspace/home/只读提示
- [ ] R-WEB-03 非回环地址拒绝，退出码 2
- [ ] R-WEB-04 全站无登录/账号/令牌/Cookie
- [ ] R-WEB-05 优雅关闭，退出码 0
- [ ] R-WEB-06 既有命令与 `ant "prompt"` 行为不变
- [ ] R-WEB-07 配置非法仍可浏览，显示告警
- [ ] R-WEB-08 左栏 session 列表/搜索/过滤/分组
- [ ] R-WEB-09 会话历史按 turn 还原
- [ ] R-WEB-10 工具卡与 diff/write 展示
- [ ] R-WEB-11 轨迹视图与详情
- [ ] R-WEB-12 右栏改动/文件/产物
- [ ] R-WEB-13 本机配置展示 workspace/home/模型/来源
- [ ] R-WEB-14 运行中 session 2s 内更新
- [ ] R-WEB-15 响应式布局
- [ ] R-WEB-16 键盘可达
- [ ] R-WEB-17 只读，无写盘
- [ ] R-WEB-18 路径/符号链接安全
- [ ] R-WEB-19 无密钥泄露
- [ ] R-WEB-20 安全响应头
- [ ] R-WEB-21 无账号 UI/路由/存储
- [ ] R-WEB-22 测试覆盖关键边界

## 22.12 实施计划

| 阶段 | 内容 | 产出 | 预估 |
| --- | --- | --- | --- |
| P0 骨架 | `session.Archive` + `ant serve` 参数/生命周期 + `/api/health` + 静态壳 | 能启动、能列出 session 空态 | 1–2 天 |
| P1 历史 | 投影器 + `/api/sessions` + `/api/sessions/{id}` + 对话流 + 工具卡 + 摘要 | 核心历史可读 | 3–4 天 |
| P2 细节 | diff 解析 + write 卡片 + 右栏改动/文件/artifact + 轨迹表/检查器 | 轨迹与修改内容完整 | 3–4 天 |
| P3 打磨 | SSE/ETag/分页 + 本机配置 + 响应式/快捷键 + 安全头 + 测试 | 可交付 v1.4 | 2–3 天 |
| P4 二期 | 交互式 turn/审批/ask（§22.15） | 可写 Web | 另立规格 |

建议顺序：先把 **P0+P1** 做成可演示的只读历史，再决定 P2 的右栏是否需要文件树。不要一开始做交互式，否则锁、审批、SSE 双向通道会把一期拖成两个月。

## 22.13 设计决策与权衡

| ID | 决策 | 主要理由 | 未采用的备选 |
| --- | --- | --- | --- |
| D46 | `ant serve` 一期**只读**：只展示历史，不发消息、不审批、不新建 | 不取 session 锁、无写接口、无 CSRF、无审批桥；历史是需求核心；可快速交付 | 一步做完整交互（锁冲突、审批超时、断线重连复杂）；桌面端（跨平台成本高） |
| D47 | **无账号 + 只监听回环**；不加登录页、不加 Cookie 身份 | 本地单用户；账号对本地只读没有安全收益；避免“登录墙”与密钥托管 | URL token（可作为多用户主机的可选二期）；OAuth/账号（违背需求）；远程访问（无 TLS/认证，不做） |
| D48 | 事件投影，**不建数据库/索引** | `events.jsonl` 已是事实来源（[D18](17-adr.md#19-设计决策记录adr)）；只读投影无同步问题；与现有恢复/审计共用数据 | SQLite 索引（依赖、双写、迁移）；预生成 snapshot（版本/失效复杂） |
| D49 | 前端**零构建**，原生 ES Modules + Go `embed` | 单二进制、无 Node 构建依赖、离线可用；只读页面复杂度可控 | React/Vite（构建链、依赖、包体）；服务端模板（交互/局部更新差） |
| D50 | **单 workspace + home 白名单**，不做跨项目扫描 | 与 §9.8 一致；隐私与性能可控；避免把 home 变成任意文件服务器 | 工作区切换器（扫描磁盘）；多 workspace 聚合（隐私/权限复杂） |

### 22.13.1 备选方案对比

**为什么不做“直接读 JSONL 的前端”**：浏览器直接 `fetch` 文件系统不可行；即使通过静态服务器暴露 `.ant`，也会把 artifact 路径、锁文件、日志全部暴露，且无法做投影/脱敏/路径校验。服务端投影是必要的边界。

**为什么不用 `file://` 打开**：ES Modules、fetch、SSE 都受同源限制；`file://` 无法稳定工作。

**为什么不在 serve 里直接 `Store.Open`**：会创建 artifacts 目录、可能取锁；只读服务不应有副作用。`Archive` 是更小的读面。

**为什么不做 WebSocket**：一期只需要“有变化”通知，SSE 足够且是单向的；二期交互再评估 WebSocket 或 fetch streaming。

### 22.13.2 风险与缓解

| 风险 | 缓解 |
| --- | --- |
| 事件 schema 演进导致投影错 | 未知事件不崩溃；golden fixture 覆盖所有已知类型；字段只增不改 |
| 大 session 卡顿 | 事件/响应上限、按 turn 分页、虚拟滚动、ETag 304 |
| `write` 旧内容缺失导致 diff 不完整 | 明确标注“整文件重写”；二期 `diffRef` |
| planPath 与 artifact 路径不一致 | 读取时多路径回退（`artifacts/plan.md`、`artifacts/plan/round-N.md`），并在 warning 中提示 |
| 本地多用户读取 | 默认回环 + 文件权限；文档提示；二期可选 token |
| 前端零构建难维护 | 模块拆分 + 纯函数渲染 + API golden 测试；不引入隐式全局状态 |
| 配置非法导致启动失败 | best-effort 配置；`Config == nil` 时页面降级并显示告警 |

## 22.14 需求追溯

| 需求 | 主要章节 | 验收 |
| --- | --- | --- |
| R-WEB-01/02/05/06 | §22.3.1、§22.9 | `serve_test.go` + 手工 |
| R-WEB-03/17/18/19/20/21 | §22.10 | 路由/路径/静态扫描测试 |
| R-WEB-04 | §22.2.7、§22.10.2 | UI 扫描 + 手工 |
| R-WEB-07 | §22.6.2、§22.9.3 | 配置非法 fixture |
| R-WEB-08 | §22.4.2、§22.6.4 | 列表 API + UI |
| R-WEB-09/10 | §22.4.3、§22.5.2/3/4 | projector + 手工 |
| R-WEB-11 | §22.4.4、§22.5.5 | trajectory fixture + 手工 |
| R-WEB-12 | §22.4.5、§22.6.4 | artifact/file API |
| R-WEB-13 | §22.4.6、§22.6.4 | config API |
| R-WEB-14 | §22.6.7 | SSE 测试 |
| R-WEB-15/16 | §22.4.8 | 手工/可访问性检查 |
| R-WEB-22 | §22.11 | `go test ./...` |

## 22.15 二期：交互式 Web（预留设计）

一期只读已经满足“看清楚历史”；如果后续要在 Web 里继续对话，按下面扩展，不推翻一期接口。

### 22.15.1 新接口

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/api/sessions` | 新建 session（`{mode, model}`） |
| POST | `/api/sessions/{id}/turns` | 发送用户输入（`{input}`） |
| POST | `/api/sessions/{id}/cancel` | 取消当前 turn |
| POST | `/api/approvals/{id}` | 审批决定（`{decision, scope}`） |
| POST | `/api/asks/{id}` | 回答 `ask`（`{answer, selectedId}`） |
| GET | `/api/sessions/{id}/stream` | SSE 增加事件类型：`delta`/`tool`/`approval`/`ask`/`turn`；客户端通过上面的 POST 回答，不是双向 SSE |

### 22.15.2 写接口的本地防护

即使“无账号”，写接口也必须防浏览器跨站触发：

- 只允许 `Content-Type: application/json`；
- 要求自定义头 `X-ANT-Request: 1`（跨站请求会触发 CORS 预检，服务端不返回 CORS 头，预检失败）；
- 校验 `Origin`/`Sec-Fetch-Site` 为同源；
- 请求体上限 1 MiB；每个 session 同时最多 1 个运行 turn；
- 不使用 Cookie 身份；不引入 CSRF token（无会话身份，CSRF token 无意义）；
- 仍只监听回环。

### 22.15.3 运行器与审批桥

- 把 `cli.openAndWire` 拆成可复用的 `internal/runner`（或 `cli` 内部的 `wireRuntime`），让 CLI 与 Web 共用：
  - `agent.Deps.OnDelta` → SSE `delta`/`reasoning`；
  - `approval.Prompter` 的 Web 实现：把问题入队，SSE 推送，等待 POST 回答；超时/断线按“拒绝”处理；
  - `ask` 工具同样走队列；
  - `statemachine.Engine.RunTurn` 在后台 goroutine 执行；`cancel` 取消 context。
- session 锁：`Store.Open(ctx, session.OpenOptions{ID: id, Lock: true})`；若 CLI 正在运行，返回 409 + 锁的 PID/host，不抢占。
- 崩溃恢复：继续复用 [§9.6](07-session.md#96-断点续传算法)；Web 只负责展示恢复决策。

### 22.15.4 二期 UI

- 底部输入框恢复：多行、发送/停止、`@` 文件、`/` 命令（先支持 `/new`、`/mode`）；
- 审批卡变为可操作：允许一次/本 session/拒绝；
- `ask` 卡变为选项按钮 + 自由文本；
- 运行中支持排队/插话（可先只做“排队”，不做插话）；
- 仍然没有账号/登录/云同步。

### 22.15.5 二期的测试重点

- 锁冲突：CLI 持锁时 Web 发送返回 409，且不破坏 CLI；
- 审批超时/断线：不会永久挂起；按拒绝写入事件；
- 取消：`context.Cancel` 后事件、meta、工具子进程都正确收尾；
- 写接口 CSRF：无自定义头/坏 Origin 被拒绝；
- 与 CLI 交替操作同一 session：事件顺序、锁、恢复一致。

---

[返回索引](../README.md) · [上一模块](19-interfaces.md) · [模块 17 ADR](17-adr.md) · [模块 7 Session](07-session.md)