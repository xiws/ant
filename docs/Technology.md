# 技术使用文档：daisyUI + htmx + Alpine.js

> 适用模块：[§20 本地 Web 服务](20-web-serve.md)（`ant serve`）。
>
> 本文记录前端三项技术的版本、本地加载方式、职责边界、代码范式与安全约束。目标不是介绍三个库的完整 API，而是说明在 `ant` 这个**零构建、单二进制、本地只读**页面中怎么用、为什么这样用、以及扩展时不要踩哪些坑。

## 1. 技术栈与文件

| 技术 | 版本 | 本地文件 | 在 `ant serve` 中的职责 |
| --- | --- | --- | --- |
| daisyUI | 5.8.2 | `internal/web/static/assets/daisyui@5.css` | 按钮、输入框、下拉、badge、loading、modal 等组件与主题色变量 |
| htmx | 2.0.11 | `internal/web/static/assets/htmx.min.js` | 左栏会话列表、左下角状态卡的 HTML 片段刷新 |
| Alpine.js | 3.17.5 | `internal/web/static/assets/alpinejs@3.js` | 三栏外壳状态、移动端抽屉、composer、折叠、主题/模型下拉绑定 |
| 原生 ES Modules | 浏览器内置 | `app.js`、`render.js` 等 | 复杂会话/轨迹渲染、hash 路由、fetch、SSE、键盘快捷键 |

不引入 Node、npm、Vite、Webpack，也不访问 CDN。所有 vendor 文件通过 Go `//go:embed static` 打进 `ant` 二进制。

`browser@4.js`（`@tailwindcss/browser`）**不是运行必需文件**。当前没有加载它：

- daisyUI 的 CDN 独立 CSS 已包含组件样式；
- 页面布局、主题和业务样式写在 `app.css`；
- Tailwind 浏览器运行时会在页面里动态注入 `<style>`，与严格 CSP 冲突，且增加运行时开销。

## 2. 加载顺序

### 2.1 `index.html` 只加载一个入口

```html
<link rel="stylesheet" href="/assets/daisyui@5.css">
<link rel="stylesheet" href="/assets/app.css">
<script type="module" src="/assets/app.js"></script>
```

`app.js` 负责动态加载其余 vendor：

```js
document.addEventListener('alpine:init', registerAlpine);
loadExternalScript('/assets/alpinejs@3.js');

async function ensureHTMX() {
  if (window.htmx) return window.htmx;
  if (!htmxReady) htmxReady = loadExternalScript('/assets/htmx.min.js');
  await htmxReady;
  return window.htmx;
}
```

### 2.2 为什么 Alpine 要动态加载

Alpine 的全局构建在脚本执行后会通过 `queueMicrotask(() => Alpine.start())` 立即启动，并派发 `alpine:init`。如果直接在 `index.html` 里用普通 `<script>` 加载 Alpine，再加载 ES Module 的 `app.js`，存在 `alpine:init` 在组件注册之前已经派发的竞态。

因此采用：

1. `app.js` 先注册 `document.addEventListener('alpine:init', registerAlpine)`；
2. 再动态插入 `/assets/alpinejs@3.js`；
3. Alpine 启动时派发 `alpine:init`，`registerAlpine()` 调用 `Alpine.data('antShell', ...)`；
4. `x-data="antShell"` 的根组件完成初始化。

htmx 没有同样的启动竞态；在 `boot()` 中 `await ensureHTMX()` 后调用 `htmx.process(document.body)`，确保首屏注入的 `hx-*` 属性被处理。

## 3. daisyUI 使用规范

### 3.1 自定义主题

`app.css` 定义两个主题：

- `[data-theme="ant"]`：深色；
- `[data-theme="ant-light"]`：浅色。

daisyUI 5 使用 `--color-*` 变量驱动组件颜色，例如：

```css
[data-theme="ant"] {
  color-scheme: dark;
  --color-base-100: #0f1115;
  --color-base-200: #16191e;
  --color-base-300: #232830;
  --color-base-content: #e8eaed;
  --color-primary: #4b7bec;
  --color-primary-content: #ffffff;
  --radius-box: 0.9rem;
}
```

主题切换由 `app.js` 控制：

```js
function applyTheme() {
  let theme = 'ant';
  if (state.ui.theme === 'auto') {
    theme = matchMedia('(prefers-color-scheme: light)').matches ? 'ant-light' : 'ant';
  } else if (state.ui.theme === 'light') {
    theme = 'ant-light';
  }
  document.documentElement.dataset.theme = theme;
}
```

### 3.2 使用到的 daisyUI 组件

| 组件 | 示例 class | 使用位置 |
| --- | --- | --- |
| Button | `btn btn-primary btn-circle btn-sm` | composer 发送按钮、对话框关闭按钮 |
| Outline button | `btn btn-outline btn-sm` | 左栏“创建会话” |
| Badge | `badge badge-sm` | mode、只读提示、状态 badge |
| Input | `input input-sm` | 左栏搜索框 |
| Select | `select select-xs` | mode/status 过滤、composer mode/model |
| Textarea | 自定义 `.composer-input` + daisyUI 基础 | 中栏输入框 |
| Loading | `loading loading-spinner loading-xs` | 会话列表骨架 |
| Modal | `modal modal-open` + `modal-box` | 本机配置、设置、快捷键、diff 对话框 |

### 3.3 自定义 CSS 与 daisyUI 的覆盖关系

daisyUI 的规则位于 cascade layer（`@layer daisyui.*`）。`app.css` 中未分层的规则优先级更高，会覆盖 daisyUI 同名属性。

因此：

- 不要把旧项目里的通用 `.btn { ... }` 基础样式重新加回来，否则会覆盖 daisyUI 的按钮主题；
- 兼容旧渲染代码时只保留窄范围修饰类，例如 `.btn.small`、`.btn.ghost`；
- 业务布局类使用 `app-shell`、`composer-card`、`session-item` 等命名，避免和 daisyUI 组件类冲突；
- 新组件优先使用 daisyUI class，只有 ant 特有布局/视觉才写自定义 CSS。

## 4. htmx 使用规范

### 4.1 片段端点

| 方法 | 路由 | 返回 | 用途 |
| --- | --- | --- | --- |
| GET | `/ui/sessions?q=&mode=&status=&limit=` | `text/html; charset=utf-8` | 左栏会话列表片段 |
| GET | `/ui/status` | `text/html; charset=utf-8` | 左下角“本地模式 · 只读”状态卡 |

两个端点都：

- 只读，无写副作用；
- 使用 `Cache-Control: no-store`；
- 动态文本由 Go `html.EscapeString` 转义；
- 与 JSON API 使用同一个 `Projector.List` 数据源，不维护第二套业务状态机。

### 4.2 `index.html` 中的典型用法

```html
<form id="session-filters"
      hx-get="/ui/sessions"
      hx-target="#session-list"
      hx-swap="innerHTML"
      hx-trigger="input changed delay:250ms, change"
      hx-indicator="#session-loading">
  <input name="q" type="search" aria-label="搜索会话">
  <select name="mode">...</select>
  <select name="status">...</select>
</form>

<div id="session-list"
     hx-get="/ui/sessions"
     hx-include="#session-filters"
     hx-trigger="load, ant:sessions-refresh from:body"
     hx-swap="innerHTML"></div>
```

关键点：

- `hx-trigger="input changed delay:250ms, change"`：输入防抖，下拉立即触发；
- `hx-include="#session-filters"`：首次 `load` 和自定义刷新事件都带上当前筛选条件；
- `hx-indicator`：请求期间显示 daisyUI spinner；
- 表单只发 GET，不会提交写请求。

### 4.3 自定义刷新事件

当 SSE 发现 session 列表需要更新时，`app.js` 触发：

```js
function requestSessionListRefresh() {
  if (window.htmx) htmx.trigger(document.body, 'ant:sessions-refresh');
}
```

对应 `hx-trigger="load, ant:sessions-refresh from:body"`。状态卡同理使用 `ant:status-refresh`。

### 4.4 afterSwap 同步与 active 状态

htmx 只负责替换 DOM，业务高亮由 `app.js` 在 `htmx:afterSwap` 中同步：

```js
document.body.addEventListener('htmx:afterSwap', (event) => {
  if (event.target && event.target.id === 'session-list') {
    syncSessionsFromDOM();
    markActiveSession();
  }
});
```

`syncSessionsFromDOM()` 从片段元素的 `data-*` 属性读取列表摘要，供键盘 `[`/`]` 切换和空态判断；`markActiveSession()` 根据当前 hash 路由添加 `session-active` 与 `aria-current="page"`。

### 4.5 错误处理

```js
document.body.addEventListener('htmx:responseError', (event) => {
  const status = event.detail && event.detail.xhr ? event.detail.xhr.status : 0;
  toast(`会话列表加载失败（HTTP ${status || '?'}）`);
});
```

`/ui/sessions?mode=nope` 等非法参数返回 400；片段端点不应返回半截 HTML。

### 4.6 什么时候不要用 htmx

以下内容继续走 JSON API + `render.js`：

- 会话详情页的 turn/工具卡/diff/审批/计划；
- 轨迹表与检查器；
- 右栏文件预览、artifact 预览；
- SSE 通知后的增量重取。

原因：这些数据已经由 Go 投影为结构化 JSON，需要复杂客户端组件、折叠状态、滚动位置保持与 ETag/304 协议。强行改成 HTML 片段会重复服务端渲染逻辑，并增大 XSS 与状态同步成本。

## 5. Alpine.js 使用规范

### 5.1 根组件

`app.js` 注册 `antShell`：

```js
window.Alpine.data('antShell', () => ({
  ready: false,
  versionText: 'vdev',
  workspace: '',
  modes: ['code', 'plan', 'ask', 'council'],
  modelOptions: [],
  composerMode: 'ask',
  composerModel: '',
  draft: '',
  hasSession: false,
  sidebarCollapsed: state.ui.sidebarCollapsed,
  rightOpen: state.ui.rightOpen,
  mobileSidebarOpen: false,

  init() { return boot(this); },
  toggleSidebar() { /* 折叠/移动端抽屉 */ },
  createSession() { /* 回到空态并聚焦 composer */ },
  copyCommand() { /* 复制终端命令，不发 POST */ },
  composerHint() { /* 根据 hasSession/draft 生成提示 */ },
}));
```

### 5.2 常用指令

| 指令 | 用法 |
| --- | --- |
| `x-data="antShell"` | 根组件 |
| `x-init="init()"` | 启动数据加载 |
| `x-text` | 版本号、提示文本 |
| `x-model` | composer mode/model/draft |
| `x-for` | 生成 mode/model 下拉选项 |
| `:class` | 侧栏折叠、移动端抽屉、右栏关闭状态 |
| `@click` | 折叠、创建会话、复制命令、打开移动端侧栏 |
| `x-cloak` | 防止 Alpine 初始化前闪现未绑定布局 |

`x-cloak` 对应 CSS：

```css
[x-cloak] { display: none !important; }
```

### 5.3 状态边界

Alpine 只管理**外壳交互状态**：

- 侧栏折叠/移动端抽屉；
- 右栏开关的显示类；
- composer 的 mode、model、draft；
- 版本号、模型别名选项等轻量展示字段。

会话详情、turn 列表、轨迹行、diff 等大对象保留在 `app.js` 的普通 `state` 中，由 `render.js` 命令式渲染。这样避免：

- 把大型历史树塞进 Alpine 响应式代理导致性能下降；
- Alpine 模板和现有 DOM 渲染两套逻辑互相覆盖；
- 滚动位置、折叠状态、检查器在响应式重绘中被重置。

### 5.4 与普通状态同步

当路由或会话变化时，`app.js` 显式同步外壳状态：

```js
function syncComposerForRoute() {
  const shell = state.shell;
  shell.hasSession = Boolean(state.session && state.route.name === 'session');
  if (shell.hasSession) {
    shell.composerMode = state.session.meta.mode || shell.composerMode;
    shell.composerModel = state.session.meta.model || shell.composerModel;
  }
  syncShell(); // 版本、workspace、模型选项
}
```

### 5.5 CSP 注意事项

Alpine 标准构建使用 `new Function` 解析 `x-*` 表达式，因此 CSP 的 `script-src` 必须包含 `'unsafe-eval'`：

```text
Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self';
  script-src 'self' 'unsafe-eval'; connect-src 'self'; base-uri 'none';
  form-action 'none'; frame-ancestors 'none'
```

这是当前本地单用户、只读页面接受的权衡。约束仍然保留：

- 不加载 CDN 脚本；
- `x-*` 表达式只存在于仓库内静态 `index.html`，不由模型/会话内容生成；
- 所有会话文本先转义；
- `style-src 'self'`，不写内联样式；
- 若未来需要移除 `'unsafe-eval'`，应替换为 Alpine CSP build，并同步调整 CSP、测试和本文。

## 6. 三者如何协作

```text
┌──────────────────────────┐
│ index.html               │
│  daisyUI class           │
│  Alpine x-* / htmx hx-*  │
└─────────────┬────────────┘
              │
   ┌──────────┴───────────┐
   │                      │
   ▼                      ▼
Alpine 根组件          htmx 片段刷新
外壳状态/抽屉/composer   左栏列表/状态卡
   │                      │
   ▼                      ▼
app.js 路由/API/SSE     GET /ui/sessions
   │                    GET /ui/status
   ▼                      │
JSON API + render.js ◄────┘
会话详情/轨迹/右栏
```

推荐决策：

| 场景 | 方案 |
| --- | --- |
| 简单、服务端可一次性渲染的列表/状态卡 | htmx HTML 片段 |
| 频繁局部交互、纯客户端 UI 状态 | Alpine |
| 复杂结构化历史、diff、轨迹、检查器 | JSON API + `render.js` |
| 实时“有新事件”通知 | SSE（`EventSource`）+ ETag 重取 |
| 写操作 | v1.4 不提供；二期再设计 runner/锁/审批桥 |

## 7. 安全约束

### 7.1 XSS

- 模型/会话 Markdown 先转义再解析白名单；
- 工具输出、diff、代码块用 `textContent`；
- `/ui/*` 片段中的所有动态值用 `html.EscapeString`；
- 不在 htmx 片段中直接嵌入模型生成内容；
- Alpine 表达式只写在静态模板中，不拼接用户字符串。

### 7.2 请求

- `/ui/*` 仅 GET；
- 无写接口、无 Cookie 身份、无 CORS；
- Host 校验、路径校验、响应头由 Go 中间件统一处理；
- htmx 表单不会触发浏览器原生提交（只有 `hx-get`）。

### 7.3 只读

composer 的发送按钮只是复制命令：

```text
新会话：ant new -m <mode> -p <model> '<输入>'
续跑：  ant -s <session-id> -m <mode> -p <model> '<输入>'
```

不调用 `POST /api/sessions`、`POST /api/sessions/{id}/turns`，不写 session/meta/events/artifact，不抢锁。

## 8. 开发与测试

### 8.1 运行

```bash
go run ./cmd/ant serve
# 打开 http://127.0.0.1:7788/
```

若环境对 Go 构建缓存目录只读，可显式指定：

```bash
GOCACHE=/tmp/gocache go test ./...
GOCACHE=/tmp/gocache go run ./cmd/ant serve --listen 127.0.0.1:7788
```

### 8.2 片段端点自测

```bash
curl -s http://127.0.0.1:7788/ui/sessions | head
curl -s 'http://127.0.0.1:7788/ui/sessions?mode=ask' | head
curl -s http://127.0.0.1:7788/ui/status
```

### 8.3 自动化测试

`internal/web/routes_test.go` 覆盖：

- `/ui/sessions` 的 HTML/转义、过滤与非法 mode；
- `/ui/status` 的只读状态卡字段；
- 静态资源 304；
- Host 校验、CSP、无写方法；
- 无账号 UI 静态扫描。

运行：

```bash
GOCACHE=/tmp/gocache go test ./internal/web
```

### 8.4 浏览器验收

1. 首屏加载后左栏出现 htmx 会话列表，底部状态卡出现 workspace/版本/监听地址；
2. 输入搜索或切换 mode/status 时列表局部刷新，不整页刷新；
3. 点击会话后中/右栏由 JSON API 渲染，composer 显示当前 session 的 mode/model；
4. 输入 composer 内容后点击发送或 `Ctrl/⌘+Enter`，只复制终端命令并出现 toast；
5. 关闭 JavaScript 时显示 noscript 提示；
6. 浏览器 Console 不应出现 CSP/EvalError；若出现，检查 CSP 是否包含 `'unsafe-eval'`（Alpine 标准构建）。

## 9. 扩展指南

### 9.1 新增一个 htmx 片段

1. 在 `internal/web/ui.go` 增加 handler；
2. 所有动态文本用 `html.EscapeString`；
3. 在 `routes.go` 注册 `GET` 路由；
4. 在 `index.html` 用 `hx-get`、`hx-target`、`hx-swap` 指向它；
5. 如果片段需要参与客户端状态，使用 `data-*` 回传轻量字段，并在 `htmx:afterSwap` 同步；
6. 在 `routes_test.go` 增加状态码、Content-Type、转义与过滤测试。

### 9.2 新增 Alpine 外壳状态

1. 在 `antShell` 返回对象中增加字段/方法；
2. 仅放 UI 状态，不放大型会话树；
3. 使用 `x-model`、`:class`、`x-text` 等静态指令绑定；
4. 若来自 async API，显式在 `syncShell()`、`syncComposerForRoute()` 或对应回调中赋值；
5. 不要用 `innerHTML` 拼接服务端文本。

### 9.3 二期交互式 Web 的边界

二期若要真正提交消息：

- 需要新增 POST 路由、SSE 双向事件与 session 锁；
- 需要 runner、审批桥、`ask` 队列、CSRF/Origin 校验；
- composer 应从“复制命令”升级为“提交 turn / 停止 turn”；
- 此时仍建议保留 htmx 管理简单的列表/状态，Alpine 管理外壳，复杂轨迹继续走 JSON + `render.js`。

## 10. 相关文件

| 文件 | 说明 |
| --- | --- |
| `internal/web/static/index.html` | daisyUI/Alpine/htmx 页面骨架 |
| `internal/web/static/assets/app.css` | ant 主题与业务样式 |
| `internal/web/static/assets/app.js` | Alpine 根组件、路由、API、SSE |
| `internal/web/ui.go` | htmx HTML 片段端点 |
| `internal/web/routes.go` | `/ui/*` 与 `/api/*` 路由 |
| `internal/web/server.go` | CSP 与安全响应头 |
| `internal/web/routes_test.go` | UI 片段与安全测试 |
| `docs/20-web-serve.md` | `ant serve` 产品/接口/页面契约 |
