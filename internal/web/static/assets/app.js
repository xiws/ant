/* app.js — 启动、hash 路由、状态、SSE、快捷键（一期只读） */

import {
  el, icon, mount, badge, chip, copyButton, copyText, toast, tokens, duration,
  relativeTime, formatTime, dayGroup, truncate, json,
} from './util.js';
import { api, ApiError } from './api.js';
import {
  renderSessionHeader, renderChat, renderEmptyMain, renderLoading, renderErrorCard,
  renderRightPanel, renderLocalConfig, fillHomeTable, renderSettings, renderShortcuts,
} from './render.js';
import { renderTrajectory } from './trajectory.js';

const STORAGE_KEY = 'ant.serve.ui';
const MODES = ['code', 'plan', 'ask', 'council'];
const STATUSES = ['running', 'idle', 'interrupted', 'failed'];

const state = {
  health: null,
  config: null,
  project: null,
  sessions: [],
  session: null,
  etag: '',
  lastSeq: 0,
  turnLimit: 50,
  route: { name: 'home', id: null, view: 'chat' },
  filters: { q: '', mode: '', status: '' },
  error: null,
  loading: false,
  streamDown: false,
  pendingInspect: null,
  ui: loadUI(),
  stream: null,
  streamId: null,
  streamTimer: null,
  connectionEl: null,
  reloading: false,
};

const dom = {};
let searchTimer = null;
let dialogCount = 0;

/* ---------- UI 偏好 ---------- */

function loadUI() {
  const defaults = {
    theme: 'auto', density: 'standard', timeMode: 'local',
    rightOpen: true, rightTab: 'changes', sidebarCollapsed: false,
    collapsedTurns: new Set(),
    filePreview: null,
    trajDuration: 'actual',
  };
  try {
    const raw = JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
    return {
      ...defaults,
      ...raw,
      collapsedTurns: new Set(),
      filePreview: null,
    };
  } catch (_) {
    return defaults;
  }
}

function saveUI() {
  try {
    const { theme, density, timeMode, rightOpen, rightTab, sidebarCollapsed, trajDuration } = state.ui;
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ theme, density, timeMode, rightOpen, rightTab, sidebarCollapsed, trajDuration }));
  } catch (_) { /* localStorage 不可用时静默 */ }
}

/* ---------- 启动 ---------- */

async function boot() {
  // 窄屏默认收起右栏，避免首屏被上下文面板覆盖。
  if (window.innerWidth < 1024) state.ui.rightOpen = false;
  buildShell();
  applyTheme();
  window.addEventListener('hashchange', () => { void onHash(); });
  window.addEventListener('keydown', onKey);
  window.addEventListener('resize', () => { renderLayoutClasses(); });
  window.addEventListener('ant:prompt', (event) => { void showPrompt(event.detail); });
  try {
    state.health = await api.health();
    state.config = await api.config();
    document.title = `ant · ${state.health.workspace || 'workspace'}`;
  } catch (error) {
    state.error = error;
  }
  renderSidebar();
  await refreshSessions();
  await onHash();
}

/* ---------- 布局骨架 ---------- */

function buildShell() {
  const app = document.getElementById('app');
  const layout = el('div', { class: 'layout', id: 'layout' });
  dom.layout = layout;

  /* 左栏 */
  const sidebar = el('aside', { id: 'sidebar', 'aria-label': '会话导航' });
  const brand = el('div', { class: 'brand' }, [
    el('span', { class: 'brand-logo', text: 'ant' }),
    el('span', { class: 'brand-version', text: state.health ? `v${state.health.version || 'dev'}` : 'vdev' }),
    el('button', {
      class: 'btn icon ghost', type: 'button', 'aria-label': '折叠侧栏', title: '折叠/展开侧栏',
      onclick: toggleSidebar,
    }, '«'),
  ]);
  sidebar.appendChild(brand);

  const scroller = el('div', { class: 'sidebar-scroll' });
  const search = el('input', {
    id: 'session-search', type: 'search', placeholder: '搜索标题 / ID / 模型（/）',
    'aria-label': '搜索会话',
    oninput: (event) => {
      state.filters.q = event.target.value;
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => { void refreshSessions(); }, 180);
    },
  });
  scroller.appendChild(el('div', { class: 'sidebar-search' }, [
    el('span', { class: 'icon-search', 'aria-hidden': 'true', text: '⌕' }),
    search,
  ]));
  dom.search = search;
  dom.modeRow = el('div', { class: 'filter-row', role: 'group', 'aria-label': '模式过滤' });
  dom.statusRow = el('div', { class: 'filter-row', role: 'group', 'aria-label': '状态过滤' });
  scroller.appendChild(dom.modeRow);
  scroller.appendChild(dom.statusRow);
  dom.list = el('div', { id: 'session-list', role: 'list' });
  scroller.appendChild(dom.list);
  sidebar.appendChild(scroller);

  dom.localCard = el('div', { class: 'sidebar-footer' });
  sidebar.appendChild(dom.localCard);
  dom.sidebar = sidebar;
  layout.appendChild(sidebar);

  /* 中栏 */
  dom.main = el('main', { id: 'main', 'aria-label': '会话内容' });
  layout.appendChild(dom.main);

  /* 右栏 */
  dom.right = el('aside', { id: 'right', 'aria-label': '上下文面板' });
  layout.appendChild(dom.right);

  dom.mobileMenu = el('button', {
    class: 'btn icon mobile-menu', type: 'button', 'aria-label': '打开侧栏', title: '打开侧栏',
    onclick: toggleSidebar,
  }, '☰');
  layout.appendChild(dom.mobileMenu);
  app.replaceChildren(layout);
  renderSidebar();
}

function toggleSidebar() {
  if (window.innerWidth < 768) {
    dom.layout.classList.toggle('sidebar-open');
    return;
  }
  state.ui.sidebarCollapsed = !state.ui.sidebarCollapsed;
  saveUI();
  renderLayoutClasses();
}

function renderLayoutClasses() {
  const classes = ['layout'];
  const mobileSidebarOpen = dom.layout.classList.contains('sidebar-open');
  if (!state.ui.rightOpen) classes.push('right-closed');
  else classes.push('right-open');
  if (state.ui.sidebarCollapsed && window.innerWidth >= 768) classes.push('sidebar-collapsed');
  if (mobileSidebarOpen) classes.push('sidebar-open');
  dom.layout.className = classes.join(' ');
}

/* ---------- 左栏 ---------- */

function renderSidebar() {
  if (!dom.list) return;
  renderLayoutClasses();
  renderFilterRows();
  renderSessionList();
  renderLocalCard();
  const version = dom.sidebar.querySelector('.brand-version');
  if (version) version.textContent = state.health ? `v${state.health.version || 'dev'}` : 'vdev';
}

function renderFilterRows() {
  mount(dom.modeRow, [
    chip('全部', state.filters.mode === '', () => setFilter('mode', '')),
    ...MODES.map((mode) => chip(mode, state.filters.mode === mode, () => setFilter('mode', mode))),
  ]);
  mount(dom.statusRow, [
    chip('任意状态', state.filters.status === '', () => setFilter('status', '')),
    ...STATUSES.map((status) => chip(status, state.filters.status === status, () => setFilter('status', status))),
  ]);
}

function setFilter(key, value) {
  state.filters[key] = value;
  renderFilterRows();
  void refreshSessions();
}

function renderSessionList() {
  if (!dom.list) return;
  const sessions = state.sessions || [];
  if (sessions.length === 0) {
    const hasFilters = state.filters.q || state.filters.mode || state.filters.status;
    mount(dom.list, el('div', { class: 'empty-state' }, [
      el('p', { text: hasFilters ? '没有匹配的 session' : '当前 workspace 还没有 session' }),
      hasFilters ? el('button', {
        class: 'btn small', type: 'button', text: '清空过滤',
        onclick: () => {
          state.filters = { q: '', mode: '', status: '' };
          dom.search.value = '';
          renderSidebar();
          void refreshSessions();
        },
      }) : el('p', {}, el('code', { text: 'ant "你的任务"' })),
    ]));
    return;
  }
  const groups = new Map();
  for (const session of sessions) {
    const group = dayGroup(session.updatedAt);
    if (!groups.has(group)) groups.set(group, []);
    groups.get(group).push(session);
  }
  const nodes = [];
  for (const group of ['今天', '昨天', '更早']) {
    const items = groups.get(group);
    if (!items || items.length === 0) continue;
    nodes.push(el('div', { class: 'session-group' }, [
      el('div', { class: 'session-group-title', text: group }),
      ...items.map(renderSessionItem),
    ]));
  }
  mount(dom.list, nodes);
}

function renderSessionItem(session) {
  const statusClass = session.live ? 'running' : session.stale ? 'stale' : session.status;
  const current = state.route.id === session.id;
  return el('button', {
    class: 'session-item', type: 'button', role: 'listitem',
    'aria-current': current ? 'true' : 'false',
    title: `${session.id}\n${session.title || ''}`,
    onclick: () => go(`#/sessions/${encodeURIComponent(session.id)}`),
  }, [
    el('span', { class: `dot ${statusClass}`, title: sessionStatusText(session) }),
    el('span', {}, [
      el('span', { class: 'session-title', text: session.title || '（无标题会话）' }),
      el('span', { class: 'session-meta' }, [
        badge(session.mode || '—', `mode-${session.mode || ''}`),
        el('span', { text: `${session.turnCount || 0} 轮` }),
        el('span', { text: `${tokens(session.tokens)} tok` }),
        el('span', { text: relativeTime(session.updatedAt) }),
      ]),
      el('span', { class: 'session-id', text: session.id }),
    ]),
  ]);
}

function sessionStatusText(session) {
  if (session.live) return '运行中';
  if (session.stale) return '上次运行中断（锁心跳过期）';
  return session.status || 'unknown';
}

function renderLocalCard() {
  if (!dom.localCard) return;
  const health = state.health || {};
  const workspace = health.workspace || '—';
  const collapsedActions = el('div', { class: 'collapsed-actions' }, [
    el('button', {
      class: 'btn icon', type: 'button', title: '本机配置', 'aria-label': '本机配置',
      onclick: () => void showLocalConfig(),
    }, 'ⓘ'),
    el('button', {
      class: 'btn icon', type: 'button', title: '设置', 'aria-label': '设置',
      onclick: () => void showSettings(),
    }, '⚙'),
  ]);
  mount(dom.localCard, [el('div', { class: 'local-card' }, [
    el('div', { class: 'row' }, [
      el('span', { class: 'dot running' }),
      el('strong', { text: '本地模式 · 只读' }),
    ]),
    el('div', { class: 'path', text: workspace, title: workspace }),
    el('div', { class: 'row muted small' }, [
      el('span', { text: `ant v${health.version || 'dev'}` }),
      el('span', { text: ' · ' }),
      el('span', { text: window.location.host }),
    ]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn small', type: 'button', text: '本机配置', onclick: () => void showLocalConfig() }),
      el('button', { class: 'btn small', type: 'button', text: '设置', onclick: () => void showSettings() }),
    ]),
  ]), collapsedActions]);
}

/* ---------- 数据 ---------- */

async function refreshSessions() {
  try {
    const result = await api.sessions({
      limit: 200,
      q: state.filters.q,
      mode: state.filters.mode,
      status: state.filters.status,
    });
    if (result.notModified) return;
    state.sessions = result.data.sessions || [];
    if (!state.health) {
      state.health = { workspace: result.data.workspace, home: result.data.home, version: state.health ? state.health.version : 'dev' };
    }
    renderSessionList();
  } catch (error) {
    if (error instanceof ApiError && error.status === 0) {
      toast('与本地服务断开');
    } else {
      toast(error.message || '会话列表加载失败');
    }
  }
}

async function onHash() {
  const route = parseHash();
  const previous = state.route;
  state.route = route;

  if (route.name === 'settings') {
    void showSettings();
    return;
  }
  if (route.name !== 'session') {
    closeStream();
    state.session = null;
    state.error = null;
    state.etag = '';
    state.turnLimit = 50;
    renderSidebar();
    renderMain();
    renderRight();
    return;
  }
  if (previous.id !== route.id) {
    closeStream();
    state.session = null;
    state.error = null;
    state.etag = '';
    state.turnLimit = 50;
    state.ui.filePreview = null;
    await loadSession(route.id, { preserveScroll: false });
    return;
  }
  renderSidebar();
  renderMain();
  renderRight();
  const params = route.query || new URLSearchParams();
  if (params.get('turn')) {
    focusTurn(Number(params.get('turn')));
  }
}

async function loadSession(id, options = {}) {
  if (!id) return;
  if (state.reloading) return;
  state.reloading = true;
  if (!options.ifNoneMatch) {
    state.loading = true;
    state.error = null;
    renderMain();
  }
  const scrollTop = options.preserveScroll && dom.scroll ? dom.scroll.scrollTop : 0;
  try {
    const result = await api.session(id, { limit: state.turnLimit }, options.ifNoneMatch ? state.etag : '');
    if (result.notModified) {
      state.loading = false;
      return;
    }
    state.session = result.data;
    state.etag = result.etag || '';
    state.lastSeq = result.data.lastSeq || 0;
    state.error = null;
    state.loading = false;
    state.streamDown = false;
    ensureRightTab();
    renderSidebar();
    renderMain();
    renderRight();
    restoreScroll(scrollTop);
    if (state.streamId !== id) openStream(id);
    const params = parseHash().query;
    if (params && params.get('turn')) focusTurn(Number(params.get('turn')));
  } catch (error) {
    state.loading = false;
    state.error = error;
    renderMain();
  } finally {
    state.reloading = false;
  }
}

function ensureRightTab() {
  const view = state.session;
  if (!view) return;
  const changes = view.changes || [];
  const artifacts = view.artifacts || [];
  if (!changes.length && artifacts.length && state.ui.rightTab === 'changes') {
    state.ui.rightTab = 'artifacts';
  }
  if (!changes.length && !artifacts.length && !state.ui.filePreview && state.ui.rightTab === 'changes') {
    state.ui.rightOpen = false;
  }
}

function restoreScroll(top) {
  if (dom.scroll && top > 0) dom.scroll.scrollTop = top;
}

function openStream(id) {
  closeStream();
  state.streamId = id;
  state.stream = api.openStream(id, {
    onChanged: (payload) => {
      if (payload && payload.deleted) {
        toast('session 已被删除');
        closeStream();
        void refreshSessions();
        return;
      }
      // 1s 内合并多次通知；客户端按需重取，304 则忽略。
      clearTimeout(state.streamTimer);
      state.streamTimer = setTimeout(() => {
        void loadSession(id, { preserveScroll: true, ifNoneMatch: true });
        void refreshSessions();
      }, 150);
    },
    onError: () => {
      state.streamDown = true;
      updateStreamBanner();
    },
    onOpen: () => {
      if (state.streamDown) {
        state.streamDown = false;
        updateStreamBanner();
      }
    },
  });
}

function closeStream() {
  clearTimeout(state.streamTimer);
  if (state.stream) {
    state.stream.close();
    state.stream = null;
  }
  state.streamId = null;
}

/* ---------- 路由 ---------- */

function parseHash() {
  const raw = location.hash.replace(/^#/, '');
  if (raw.startsWith('/settings')) return { name: 'settings' };
  const match = raw.match(/^\/sessions\/([^/?]+)(\/trajectory)?(\?.*)?$/);
  if (match) {
    return {
      name: 'session',
      id: decodeURIComponent(match[1]),
      view: match[2] ? 'trajectory' : 'chat',
      query: new URLSearchParams(match[3] || ''),
    };
  }
  return { name: 'home', id: null, view: 'chat' };
}

function go(hash) {
  if (location.hash === hash) {
    void onHash();
    return;
  }
  location.hash = hash;
}

function sessionHash(view) {
  if (!state.route.id) return '#/';
  return `#/sessions/${encodeURIComponent(state.route.id)}${view === 'trajectory' ? '/trajectory' : ''}`;
}

/* ---------- 中栏渲染 ---------- */

function renderMain() {
  if (!dom.main) return;
  const route = state.route;
  if (route.name !== 'session' || !route.id) {
    mount(dom.main, el('div', { class: 'scroll-area' }, renderEmptyMain(state.sessions)));
    dom.scroll = dom.main.querySelector('.scroll-area');
    return;
  }
  if (state.loading && !state.session) {
    mount(dom.main, el('div', { class: 'scroll-area' }, renderLoading('加载会话…')));
    return;
  }
  if (state.error) {
    mount(dom.main, el('div', { class: 'scroll-area' }, renderErrorCard(state.error, () => void loadSession(route.id))));
    return;
  }
  if (!state.session) {
    mount(dom.main, el('div', { class: 'scroll-area' }, renderLoading('加载会话…')));
    return;
  }

  const view = state.session;
  const ctx = makeCtx();
  const nodes = [renderSessionHeader(view, ctx)];
  nodes.push(el('div', { class: 'view-tabs', role: 'tablist' }, [
    el('button', {
      class: 'view-tab', type: 'button', role: 'tab',
      'aria-selected': route.view === 'chat' ? 'true' : 'false',
      text: '对话',
      onclick: () => go(sessionHash('chat')),
    }),
    el('button', {
      class: 'view-tab', type: 'button', role: 'tab',
      'aria-selected': route.view === 'trajectory' ? 'true' : 'false',
      text: '轨迹',
      onclick: () => go(sessionHash('trajectory')),
    }),
  ]));
  const scroll = el('div', { class: 'scroll-area' });
  scroll.appendChild(streamBanner());
  scroll.appendChild(renderWarnings(view));
  if (route.view === 'trajectory') {
    scroll.appendChild(renderTrajectory(view, ctx));
    mount(dom.main, [...nodes, scroll, renderReadonlyBar(view)]);
    dom.scroll = scroll;
    if (state.pendingInspect && ctx.inspectTool) {
      const tool = state.pendingInspect;
      state.pendingInspect = null;
      ctx.inspectTool(tool);
    }
  } else {
    scroll.appendChild(renderChat(view, ctx));
    mount(dom.main, [...nodes, scroll, renderReadonlyBar(view)]);
    dom.scroll = scroll;
  }
}

function streamBanner() {
  if (!state.connectionEl) {
    state.connectionEl = el('div', {
      class: 'banner danger hidden', role: 'alert',
      text: '与本地服务断开，正在自动重连…（关闭页面后需重新运行 ant serve）',
    });
  }
  state.connectionEl.classList.toggle('hidden', !state.streamDown);
  return state.connectionEl;
}

function updateStreamBanner() {
  if (state.connectionEl) {
    state.connectionEl.classList.toggle('hidden', !state.streamDown);
  }
}

function renderWarnings(view) {
  const warnings = [...(view.warnings || [])];
  if (state.config && state.config.available === false) {
    warnings.push(`配置不可用：${state.config.error || '未知错误'}`);
  } else if (state.config && (state.config.warnings || []).length) {
    warnings.push(`配置告警：${state.config.warnings.length} 条（见「本机配置」）`);
  }
  if (warnings.length === 0) return el('div', { class: 'hidden' });
  const shown = warnings.slice(0, 3);
  const rest = warnings.slice(3);
  const nodes = [
    el('strong', { text: `告警（${warnings.length}）：` }),
    ...shown.map((warning) => el('div', { class: 'small', text: warning, title: warning })),
  ];
  if (rest.length) {
    nodes.push(el('details', {}, [
      el('summary', { class: 'small', text: `还有 ${rest.length} 条` }),
      ...rest.map((warning) => el('div', { class: 'small', text: warning, title: warning })),
    ]));
  }
  return el('div', { class: 'banner', role: 'status' }, nodes);
}

function renderReadonlyBar(view) {
  const command = `ant -s ${view.meta.id}`;
  return el('div', { class: 'readonly-bar' }, [
    el('span', { text: '只读模式 · 在终端继续该会话：' }),
    el('code', { class: 'cmd', text: command }),
    copyButton(command, '复制', 'btn small ghost'),
    el('span', { class: 'spacer' }),
    el('span', { class: 'muted small', text: '需要修改代码或新建会话，请在 workspace 终端运行 ant。' }),
  ]);
}

function makeCtx() {
  return {
    state,
    ui: state.ui,
    view: state.session,
    onRefresh: () => void loadSession(state.route.id, { preserveScroll: true }),
    onToggleRight: (open) => setRight(open === undefined ? !state.ui.rightOpen : open),
    onRightTab: (tab) => { state.ui.rightTab = tab; saveUI(); renderRight(); },
    onOpenFile: (path) => void openFilePreview(path),
    onOpenChange: (change) => void openChange(change),
    onOpenArtifact: (artifact) => void openArtifact(artifact),
    onOpenPlan: () => void openPlan(),
    onLoadEarlier: (firstTurn) => void loadEarlier(firstTurn),
    onInspectTool: (tool) => {
      state.pendingInspect = tool;
      go(sessionHash('trajectory'));
    },
  };
}

function setRight(open) {
  state.ui.rightOpen = open;
  saveUI();
  renderLayoutClasses();
  renderRight();
}

function renderRight() {
  if (!dom.right) return;
  if (!state.ui.rightOpen || !state.session) {
    dom.right.replaceChildren();
    renderLayoutClasses();
    return;
  }
  const ctx = makeCtx();
  mount(dom.right, renderRightPanel(state.session, ctx));
  renderLayoutClasses();
}

async function openFilePreview(path) {
  const view = state.session;
  if (!view) return;
  state.ui.filePreview = { name: path, content: '', error: '', source: 'workspace' };
  state.ui.rightTab = 'files';
  state.ui.rightOpen = true;
  renderRight();
  try {
    const file = await api.workspaceFile(path);
    state.ui.filePreview = {
      name: file.path || path,
      content: file.content || '',
      binary: file.binary,
      truncated: file.truncated,
      source: 'workspace',
    };
  } catch (error) {
    state.ui.filePreview = { name: path, error: error.message || '无法读取文件', source: 'workspace' };
  }
  renderRight();
}

async function openArtifact(artifact) {
  const view = state.session;
  if (!view) return;
  state.ui.rightTab = 'files';
  state.ui.rightOpen = true;
  if (!artifact.previewable) {
    state.ui.filePreview = { name: artifact.name, binary: true, source: 'artifact' };
    renderRight();
    return;
  }
  state.ui.filePreview = { name: artifact.name, content: '', error: '', source: 'artifact' };
  renderRight();
  try {
    const text = await api.artifactText(view.id, artifact.name);
    state.ui.filePreview = { name: artifact.name, content: text, source: 'artifact' };
  } catch (error) {
    state.ui.filePreview = { name: artifact.name, error: error.message || '无法读取 artifact', source: 'artifact' };
  }
  renderRight();
}

function findChangeTool(path) {
  const view = state.session;
  if (!view) return null;
  for (const turn of view.turns || []) {
    for (const item of turn.items || []) {
      const tool = item.tool;
      if (!tool) continue;
      if (tool.name === 'edit' && tool.diff && tool.diff.path === path) return tool;
      if (tool.name === 'write' && tool.write && tool.write.path === path) return tool;
    }
  }
  return null;
}

function openChange(change) {
  const tool = findChangeTool(change.path);
  if (tool && tool.diff) {
    openDialog(`diff · ${change.path}`, el('div', {}, [
      renderDiffBlock(tool.diff),
      el('div', { class: 'tool-section' }, el('button', {
        class: 'btn small', type: 'button', text: '查看当前文件', onclick: () => { closeDialog(); void openFilePreview(change.path); },
      })),
    ]));
    return;
  }
  if (tool && tool.write) {
    openDialog(`write · ${change.path}`, el('div', {}, [
      el('div', { class: 'muted small', text: tool.write.created ? '新建文件' : '覆盖写入（旧内容未持久化）' }),
      el('pre', { class: 'raw-json', text: tool.write.content || '' }),
    ]));
    return;
  }
  void openFilePreview(change.path);
}

function renderDiffBlock(diff) {
  // 延迟 import 以避免 render.js 与 diff.js 的循环依赖。
  const wrap = el('div');
  import('./diff.js').then(({ renderDiff }) => {
    const node = renderDiff(diff);
    if (node) wrap.appendChild(node);
    else wrap.appendChild(el('div', { class: 'muted small', text: '没有 diff' }));
  });
  return wrap;
}

async function openPlan() {
  const view = state.session;
  if (!view) return;
  try {
    const plan = await api.plan(view.id);
    openDialog(`计划 · ${plan.name || 'plan.md'}`, el('div', {}, [
      el('div', { class: 'muted small', text: plan.source || '' }),
      el('pre', { class: 'raw-json', text: plan.body || '' }),
    ]));
  } catch (error) {
    openDialog('计划', el('div', { class: 'missing', text: error.message || '没有计划文件' }));
  }
}

async function loadEarlier(firstTurn) {
  const view = state.session;
  if (!view || !firstTurn) return;
  try {
    const result = await api.session(view.id, { beforeTurn: firstTurn, limit: 50 });
    const older = result.data;
    const seen = new Set((view.turns || []).map((turn) => turn.turn));
    const olderTurns = (older.turns || []).filter((turn) => !seen.has(turn.turn));
    state.session = {
      ...view,
      turns: [...olderTurns, ...(view.turns || [])],
      trajectory: [...(older.trajectory || []), ...(view.trajectory || [])],
      hasMore: older.hasMore,
      firstTurn: older.firstTurn,
    };
    state.turnLimit = Math.min(500, state.turnLimit + 50);
    const top = dom.scroll ? dom.scroll.scrollTop : 0;
    renderMain();
    if (dom.scroll) dom.scroll.scrollTop = top;
  } catch (error) {
    toast(error.message || '加载更早轮次失败');
  }
}

function focusTurn(turn) {
  if (!turn) return;
  const node = document.getElementById(`turn-${turn}`);
  if (node) {
    node.scrollIntoView({ block: 'start', behavior: 'smooth' });
    node.classList.add('turn-focus');
    setTimeout(() => node.classList.remove('turn-focus'), 1600);
  }
}

/* ---------- 快捷键 ---------- */

function onKey(event) {
  const target = event.target;
  const typing = target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA'
    || target.tagName === 'SELECT' || target.isContentEditable);

  if (event.key === 'Escape') {
    if (dialogCount > 0) return; // 对话框自己处理
    if (state.ui.rightOpen) { setRight(false); return; }
    dom.layout.classList.remove('sidebar-open');
    return;
  }
  if (typing) return;
  if (event.metaKey || event.ctrlKey || event.altKey) return;

  switch (event.key) {
    case '/':
      event.preventDefault();
      dom.search.focus();
      dom.search.select();
      break;
    case 't':
      if (state.route.name === 'session') {
        event.preventDefault();
        go(sessionHash(state.route.view === 'trajectory' ? 'chat' : 'trajectory'));
      }
      break;
    case '[':
      moveSession(-1);
      break;
    case ']':
      moveSession(1);
      break;
    case '?':
      event.preventDefault();
      showShortcuts();
      break;
    case 'j':
      moveTrajectoryRow(1);
      break;
    case 'k':
      moveTrajectoryRow(-1);
      break;
    default:
      break;
  }
}

function moveSession(delta) {
  const sessions = state.sessions || [];
  if (sessions.length === 0) return;
  const index = sessions.findIndex((session) => session.id === state.route.id);
  const next = Math.min(sessions.length - 1, Math.max(0, (index === -1 ? 0 : index) + delta));
  const target = sessions[next];
  if (target) go(`#/sessions/${encodeURIComponent(target.id)}`);
}

function moveTrajectoryRow(delta) {
  if (state.route.view !== 'trajectory') return;
  const rows = Array.from(document.querySelectorAll('.traj-table tr.row[data-row-id]'));
  if (!rows.length) return;
  const selected = rows.findIndex((row) => row.getAttribute('aria-selected') === 'true');
  const next = Math.min(rows.length - 1, Math.max(0, (selected === -1 ? 0 : selected) + delta));
  rows[next].click();
  rows[next].scrollIntoView({ block: 'nearest' });
}

/* ---------- 对话框 ---------- */

function openDialog(title, content, options = {}) {
  const backdrop = el('div', { class: 'dialog-backdrop' });
  const dialog = el('div', { class: 'dialog', role: 'dialog', 'aria-modal': 'true', 'aria-label': title });
  const close = () => {
    document.removeEventListener('keydown', onDialogKey);
    backdrop.remove();
    dialogCount = Math.max(0, dialogCount - 1);
  };
  const onDialogKey = (event) => {
    if (event.key === 'Escape') { event.stopPropagation(); close(); }
  };
  dialog.appendChild(el('div', { class: 'dialog-head' }, [
    el('h2', { text: title }),
    el('span', { class: 'spacer' }),
    options.actions || null,
    el('button', { class: 'btn small ghost', type: 'button', text: '×', 'aria-label': '关闭', onclick: close }),
  ]));
  const body = el('div', { class: 'dialog-body' });
  if (typeof content === 'string') body.appendChild(el('div', { text: content }));
  else body.appendChild(content);
  dialog.appendChild(body);
  backdrop.appendChild(dialog);
  backdrop.addEventListener('mousedown', (event) => { if (event.target === backdrop) close(); });
  document.addEventListener('keydown', onDialogKey);
  document.body.appendChild(backdrop);
  dialogCount += 1;
  dialogClose = close;
  return { close };
}

let dialogClose = null;

function closeDialog() {
  if (dialogClose) dialogClose();
  dialogClose = null;
}

async function showLocalConfig() {
  const content = renderLocalConfig(
    state.health || {},
    state.config || { available: false, error: '配置未加载' },
    state.project,
  );
  openDialog('本机配置', content, {
    actions: el('button', {
      class: 'btn small', type: 'button', text: '刷新',
      onclick: () => void showLocalConfig(),
    }),
  });
  try {
    const [home, project] = await Promise.all([api.home(), api.project()]);
    state.project = project;
    const table = content.querySelector('[data-role="home-files"]');
    if (table) fillHomeTable(table, home);
    const projectCell = content.querySelector('[data-role="project"]');
    if (projectCell && project && project.exists) {
      projectCell.textContent = `${project.path} · ${project.hash || '无 hash'} · ${project.bytes} 字节`;
    }
  } catch (error) {
    const table = content.querySelector('[data-role="home-files"]');
    if (table) table.textContent = `home 读取失败：${error.message}`;
  }
}

function showSettings() {
  const content = renderSettings(state.health || {}, state.ui, {
    onTheme: (value) => { state.ui.theme = value; saveUI(); applyTheme(); },
    onDensity: (value) => { state.ui.density = value; saveUI(); renderMain(); },
    onTimeMode: (value) => { state.ui.timeMode = value; saveUI(); renderMain(); },
  });
  openDialog('设置', content, {
    actions: el('button', {
      class: 'btn small', type: 'button', text: '快捷键',
      onclick: () => showShortcuts(),
    }),
  });
  if (location.hash === '#/settings') location.hash = '#/';
}

function showShortcuts() {
  openDialog('快捷键', renderShortcuts());
}

async function showPrompt(name) {
  try {
    const prompt = await api.homePrompt(name);
    openDialog(`prompt · ${prompt.name}`, el('div', {}, [
      el('div', { class: 'warn-text', text: 'prompt 可能包含敏感内容；只在本地展示。' }),
      el('pre', { class: 'raw-json', text: prompt.content }),
    ]));
  } catch (error) {
    openDialog('prompt', el('div', { class: 'missing', text: error.message || '无法读取 prompt' }));
  }
}

/* ---------- 主题 ---------- */

function applyTheme() {
  if (state.ui.theme === 'auto') {
    document.documentElement.removeAttribute('data-theme');
  } else {
    document.documentElement.dataset.theme = state.ui.theme;
  }
}

/* ---------- 启动 ---------- */

boot().catch((error) => {
  const app = document.getElementById('app');
  if (app) {
    app.replaceChildren(renderErrorCard(error, () => window.location.reload()));
  }
});
