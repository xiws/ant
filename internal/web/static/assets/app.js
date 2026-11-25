/* app.js — ant serve UI 启动、Alpine 状态、hash 路由、htmx 片段刷新与 SSE。 */

import { el, copyText, toast } from './util.js';
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
  error: null,
  loading: false,
  streamDown: false,
  pendingInspect: null,
  ui: loadUI(),
  shell: null,
  stream: null,
  streamId: null,
  streamTimer: null,
  reloading: false,
};

const dom = {
  main: document.getElementById('main-content'),
  right: document.getElementById('right'),
};
let searchTimer = null;
let dialogCount = 0;
let dialogClose = null;
let htmxReady = null;

/* ---------- UI 偏好 ---------- */

function loadUI() {
  const defaults = {
    theme: 'auto', density: 'standard', timeMode: 'local',
    rightOpen: true, rightTab: 'changes', sidebarCollapsed: false,
    expandedTurns: new Set(),
    filePreview: null,
    trajDuration: 'actual',
  };
  try {
    const raw = JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
    return { ...defaults, ...raw, expandedTurns: new Set(), filePreview: null };
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

/* ---------- Alpine 根组件 ---------- */

function registerAlpine() {
  if (!window.Alpine || window.__antAlpineRegistered) return;
  window.__antAlpineRegistered = true;
  window.Alpine.data('antShell', () => ({
    ready: false,
    versionText: 'vdev',
    workspace: '',
    modes: MODES,
    modelOptions: [],
    composerMode: 'ask',
    composerModel: '',
    draft: '',
    hasSession: false,
    sidebarCollapsed: state.ui.sidebarCollapsed,
    rightOpen: state.ui.rightOpen,
    mobileSidebarOpen: false,

    init() { return boot(this); },

    toggleSidebar() {
      if (window.innerWidth < 768) {
        this.mobileSidebarOpen = !this.mobileSidebarOpen;
        return;
      }
      this.sidebarCollapsed = !this.sidebarCollapsed;
      state.ui.sidebarCollapsed = this.sidebarCollapsed;
      saveUI();
    },

    createSession() {
      this.mobileSidebarOpen = false;
      go('#/');
      this.$nextTick(() => focusComposer());
    },

    focusComposer() {
      const input = document.getElementById('composer-text');
      if (input) input.focus();
    },

    copyCommand() {
      const command = buildTerminalCommand(this);
      if (!command) return;
      copyText(command);
      toast('已复制终端命令');
    },

    composerHint() {
      const text = String(this.draft || '').trim();
      if (!text) {
        return this.hasSession ? '输入内容后复制续跑命令（Ctrl/⌘+Enter）' : '输入内容后复制新建会话命令（Ctrl/⌘+Enter）';
      }
      return this.hasSession ? '将复制续跑命令，在终端执行' : '将复制新建会话命令，在终端执行';
    },
  }));
}

function loadExternalScript(src) {
  return new Promise((resolve, reject) => {
    const existing = Array.from(document.scripts).find((script) => script.src.endsWith(src));
    if (existing) {
      if (existing.dataset.loaded === '1') resolve();
      else existing.addEventListener('load', () => resolve(), { once: true });
      return;
    }
    const script = document.createElement('script');
    script.src = src;
    script.onload = () => { script.dataset.loaded = '1'; resolve(); };
    script.onerror = () => reject(new Error(`无法加载 ${src}`));
    document.head.appendChild(script);
  });
}

// Alpine 的全局构建会在自身脚本加载后立即 start；先注册 alpine:init 监听，
// 再动态加载本地 vendor 文件，保证组件注册发生在 start 之前。
document.addEventListener('alpine:init', registerAlpine);
loadExternalScript('/assets/alpinejs@3.js').catch((error) => {
  const app = document.getElementById('app');
  if (app) {
    app.removeAttribute('x-cloak');
    app.textContent = error.message;
  }
});

async function ensureHTMX() {
  if (window.htmx) return window.htmx;
  if (!htmxReady) htmxReady = loadExternalScript('/assets/htmx.min.js');
  await htmxReady;
  return window.htmx;
}

/* ---------- 启动 ---------- */

async function boot(shell) {
  dom.main = dom.main || document.getElementById('main-content');
  dom.right = dom.right || document.getElementById('right');
  state.shell = shell;
  if (window.innerWidth >= 768 && window.innerWidth < 1024) {
    state.ui.sidebarCollapsed = true;
  }
  shell.sidebarCollapsed = state.ui.sidebarCollapsed;
  shell.rightOpen = state.ui.rightOpen;
  if (window.innerWidth < 1024) {
    state.ui.rightOpen = false;
    shell.rightOpen = false;
  }
  applyTheme();
  syncShell();

  window.addEventListener('hashchange', () => { void onHash(); });
  window.addEventListener('keydown', onKey);
  window.addEventListener('resize', () => {
    if (window.innerWidth >= 768) shell.mobileSidebarOpen = false;
  });
  window.addEventListener('ant:prompt', (event) => { void showPrompt(event.detail); });
  document.addEventListener('click', onDelegatedClick);

  try {
    const [health, config] = await Promise.all([api.health(), api.config()]);
    state.health = health;
    state.config = config;
    document.title = `ant · ${health.workspace || 'workspace'}`;
  } catch (error) {
    state.error = error;
  }
  syncShell();
  try {
    await ensureHTMX();
    setupHTMX();
  } catch (error) {
    toast(error.message || 'htmx 加载失败');
  }
  await refreshSessions();
  await onHash();
  shell.ready = true;
}

function syncShell() {
  const shell = state.shell;
  if (!shell) return;
  const health = state.health || {};
  const config = state.config || {};
  shell.versionText = health.version ? `v${String(health.version).replace(/^v/, '')}` : 'vdev';
  shell.workspace = health.workspace || '';
  const names = (config.models || []).map((model) => model.name).filter(Boolean);
  const preferred = shell.composerModel || config.defaultModel || (state.session && state.session.meta && state.session.meta.model) || '';
  if (preferred && !names.includes(preferred)) names.unshift(preferred);
  shell.modelOptions = names;
  if (!shell.composerMode) {
    shell.composerMode = config.defaultMode || 'ask';
  }
  if (!shell.composerModel) {
    shell.composerModel = config.defaultModel || names[0] || '';
  }
}

function syncComposerForRoute() {
  const shell = state.shell;
  if (!shell) return;
  const config = state.config || {};
  shell.hasSession = Boolean(state.session && state.route.name === 'session');
  if (state.session && state.route.name === 'session') {
    shell.composerMode = state.session.meta.mode || shell.composerMode || config.defaultMode || 'ask';
    shell.composerModel = state.session.meta.model || shell.composerModel || config.defaultModel || '';
  } else {
    shell.composerMode = config.defaultMode || shell.composerMode || 'ask';
    shell.composerModel = config.defaultModel || shell.composerModel || '';
  }
  syncShell();
}

function setupHTMX() {
  if (!window.htmx || window.__antHTMXReady) return;
  window.__antHTMXReady = true;
  window.htmx.process(document.body);
  document.body.addEventListener('htmx:afterSwap', (event) => {
    if (event.target && event.target.id === 'session-list') {
      syncSessionsFromDOM();
      markActiveSession();
      if (state.route.name === 'home') renderMain();
    }
  });
  document.body.addEventListener('htmx:responseError', (event) => {
    const status = event.detail && event.detail.xhr ? event.detail.xhr.status : 0;
    toast(`会话列表加载失败（HTTP ${status || '?'}）`);
  });
}

function onDelegatedClick(event) {
  const button = event.target && event.target.closest ? event.target.closest('[data-action]') : null;
  if (!button) return;
  switch (button.dataset.action) {
    case 'local-config':
      void showLocalConfig();
      break;
    case 'settings':
      showSettings();
      break;
    default:
      break;
  }
}

/* ---------- 数据 ---------- */

function currentFilters() {
  const form = document.getElementById('session-filters');
  if (!form) return { q: '', mode: '', status: '' };
  return {
    q: (form.querySelector('[name="q"]') || {}).value || '',
    mode: (form.querySelector('[name="mode"]') || {}).value || '',
    status: (form.querySelector('[name="status"]') || {}).value || '',
  };
}

async function refreshSessions() {
  try {
    const result = await api.sessions({ limit: 200, ...currentFilters() });
    if (result.notModified) return;
    state.sessions = result.data.sessions || [];
    if (!state.health) {
      state.health = { workspace: result.data.workspace, home: result.data.home, version: 'dev' };
    }
    syncShell();
    markActiveSession();
    if (state.route.name === 'home') renderMain();
  } catch (error) {
    if (error instanceof ApiError && error.status === 0) toast('与本地服务断开');
    else toast(error.message || '会话列表加载失败');
  }
}

function syncSessionsFromDOM() {
  const nodes = document.querySelectorAll('#session-list [data-session-id]');
  state.sessions = Array.from(nodes).map((node) => ({
    id: node.dataset.sessionId,
    title: node.dataset.title || '',
    mode: node.dataset.mode || '',
    status: node.dataset.status || '',
    updatedAt: node.dataset.updated || '',
    turnCount: Number(node.dataset.turns || 0),
    tokens: node.dataset.tokens || '0',
  }));
}

function requestSessionListRefresh() {
  if (window.htmx) window.htmx.trigger(document.body, 'ant:sessions-refresh');
}

function markActiveSession() {
  document.querySelectorAll('#session-list [data-session-id]').forEach((node) => {
    const active = state.route.id === node.dataset.sessionId;
    node.classList.toggle('session-active', active);
    if (active) node.setAttribute('aria-current', 'page');
    else node.removeAttribute('aria-current');
  });
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
  if (location.hash === hash) { void onHash(); return; }
  location.hash = hash;
}

function sessionHash(view) {
  if (!state.route.id) return '#/';
  return `#/sessions/${encodeURIComponent(state.route.id)}${view === 'trajectory' ? '/trajectory' : ''}`;
}

async function onHash() {
  const route = parseHash();
  const previous = state.route;
  state.route = route;
  if (state.shell) state.shell.mobileSidebarOpen = false;

  if (route.name === 'settings') {
    showSettings();
    return;
  }
  if (route.name !== 'session') {
    closeStream();
    state.session = null;
    state.error = null;
    state.etag = '';
    state.turnLimit = 50;
    syncComposerForRoute();
    markActiveSession();
    renderMain();
    renderRight();
    return;
  }
  markActiveSession();
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
  renderMain();
  renderRight();
  const params = route.query || new URLSearchParams();
  if (params.get('turn')) focusTurn(Number(params.get('turn')));
}

async function loadSession(id, options = {}) {
  if (!id || state.reloading) return;
  state.reloading = true;
  if (!options.ifNoneMatch) {
    state.loading = true;
    state.error = null;
    renderMain();
  }
  const scrollTop = options.preserveScroll && dom.scroll ? dom.scroll.scrollTop : 0;
  try {
    const result = await api.session(id, { limit: state.turnLimit }, options.ifNoneMatch ? state.etag : '');
    if (result.notModified) { state.loading = false; return; }
    state.session = result.data;
    state.etag = result.etag || '';
    state.lastSeq = result.data.lastSeq || 0;
    state.error = null;
    state.loading = false;
    state.streamDown = false;
    ensureRightTab();
    syncComposerForRoute();
    markActiveSession();
    renderMain();
    renderRight();
    if (dom.scroll && scrollTop > 0) dom.scroll.scrollTop = scrollTop;
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
  if (!changes.length && artifacts.length && state.ui.rightTab === 'changes') state.ui.rightTab = 'artifacts';
  if (!changes.length && !artifacts.length && !state.ui.filePreview && state.ui.rightTab === 'changes') setRight(false);
}

function openStream(id) {
  closeStream();
  state.streamId = id;
  state.stream = api.openStream(id, {
    onChanged: (payload) => {
      if (payload && payload.deleted) {
        toast('session 已被删除');
        closeStream();
        requestSessionListRefresh();
        return;
      }
      clearTimeout(state.streamTimer);
      state.streamTimer = setTimeout(() => {
        void loadSession(id, { preserveScroll: true, ifNoneMatch: true });
        void refreshSessions();
        requestSessionListRefresh();
      }, 150);
    },
    onError: () => { state.streamDown = true; updateStreamBanner(); },
    onOpen: () => { if (state.streamDown) { state.streamDown = false; updateStreamBanner(); } },
  });
}

function closeStream() {
  clearTimeout(state.streamTimer);
  if (state.stream) { state.stream.close(); state.stream = null; }
  state.streamId = null;
}

/* ---------- 中栏渲染 ---------- */

function renderMain() {
  if (!dom.main) return;
  const route = state.route;
  if (route.name !== 'session' || !route.id) {
    mount(dom.main, renderEmptyMain(state.sessions));
    dom.scroll = null;
    syncComposerForRoute();
    return;
  }
  if (state.loading && !state.session) {
    mount(dom.main, renderLoading('加载会话…'));
    dom.scroll = null;
    return;
  }
  if (state.error) {
    mount(dom.main, renderErrorCard(state.error, () => void loadSession(route.id)));
    dom.scroll = null;
    return;
  }
  if (!state.session) {
    mount(dom.main, renderLoading('加载会话…'));
    dom.scroll = null;
    return;
  }

  const view = state.session;
  const ctx = makeCtx();
  const nodes = [renderSessionHeader(view, ctx)];
  nodes.push(el('div', { class: 'view-tabs', role: 'tablist' }, [
    el('button', {
      class: 'view-tab', type: 'button', role: 'tab',
      'aria-selected': route.view === 'chat' ? 'true' : 'false', text: '对话',
      onclick: () => go(sessionHash('chat')),
    }),
    el('button', {
      class: 'view-tab', type: 'button', role: 'tab',
      'aria-selected': route.view === 'trajectory' ? 'true' : 'false', text: '轨迹',
      onclick: () => go(sessionHash('trajectory')),
    }),
  ]));
  const scroll = el('div', { class: 'scroll-area' });
  scroll.appendChild(streamBanner());
  scroll.appendChild(renderWarnings(view));
  if (route.view === 'trajectory') {
    scroll.appendChild(renderTrajectory(view, ctx));
    if (state.pendingInspect && ctx.inspectTool) {
      const tool = state.pendingInspect;
      state.pendingInspect = null;
      ctx.inspectTool(tool);
    }
  } else {
    scroll.appendChild(renderChat(view, ctx));
  }
  nodes.push(scroll);
  mount(dom.main, nodes);
  dom.scroll = scroll;
}

function streamBanner() {
  if (!dom.streamBanner) {
    dom.streamBanner = el('div', {
      class: 'banner danger hidden', role: 'alert',
      text: '与本地服务断开，正在自动重连…（关闭页面后需重新运行 ant serve）',
    });
  }
  dom.streamBanner.classList.toggle('hidden', !state.streamDown);
  return dom.streamBanner;
}

function updateStreamBanner() {
  if (dom.streamBanner) dom.streamBanner.classList.toggle('hidden', !state.streamDown);
}

function renderWarnings(view) {
  const warnings = [...(view.warnings || [])];
  if (state.config && state.config.available === false) warnings.push(`配置不可用：${state.config.error || '未知错误'}`);
  else if (state.config && (state.config.warnings || []).length) warnings.push(`配置告警：${state.config.warnings.length} 条（见「本机配置」）`);
  if (warnings.length === 0) return el('div', { class: 'hidden' });
  const shown = warnings.slice(0, 3);
  const rest = warnings.slice(3);
  const nodes = [
    el('strong', { text: `告警（${warnings.length}）：` }),
    ...shown.map((warning) => el('div', { class: 'small', text: warning, title: warning })),
  ];
  if (rest.length) nodes.push(el('details', {}, [
    el('summary', { class: 'small', text: `还有 ${rest.length} 条` }),
    ...rest.map((warning) => el('div', { class: 'small', text: warning, title: warning })),
  ]));
  return el('div', { class: 'banner', role: 'status' }, nodes);
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
    onInspectTool: (tool) => { state.pendingInspect = tool; go(sessionHash('trajectory')); },
  };
}

function setRight(open) {
  state.ui.rightOpen = open;
  if (state.shell) state.shell.rightOpen = open;
  saveUI();
  renderRight();
}

function renderRight() {
  if (!dom.right) return;
  if (!state.ui.rightOpen || !state.session) {
    dom.right.replaceChildren();
    return;
  }
  mount(dom.right, renderRightPanel(state.session, makeCtx()));
}

async function openFilePreview(path) {
  const view = state.session;
  if (!view) return;
  state.ui.filePreview = { name: path, content: '', error: '', source: 'workspace' };
  state.ui.rightTab = 'files';
  setRight(true);
  try {
    const file = await api.workspaceFile(path);
    state.ui.filePreview = {
      name: file.path || path, content: file.content || '', binary: file.binary,
      truncated: file.truncated, source: 'workspace',
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
  setRight(true);
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
        class: 'btn small', type: 'button', text: '查看当前文件',
        onclick: () => { closeDialog(); void openFilePreview(change.path); },
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

function focusComposer() {
  const input = document.getElementById('composer-text');
  if (input) input.focus();
}

function buildTerminalCommand(shell) {
  const draft = String(shell && shell.draft || '').trim();
  if (!draft) return '';
  const mode = shell.composerMode || 'ask';
  const model = shell.composerModel || '';
  const args = ['ant'];
  if (state.route.id && state.session) {
    args.push('-s', state.route.id);
    args.push('-m', mode);
    if (model) args.push('-p', model);
  } else {
    args.push('new', '-m', mode);
    if (model) args.push('-p', model);
  }
  args.push(quoteShellArg(draft));
  return args.join(' ');
}

function quoteShellArg(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`;
}

/* ---------- 快捷键 ---------- */

function onKey(event) {
  const target = event.target;
  const typing = target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA'
    || target.tagName === 'SELECT' || target.isContentEditable);
  if (event.key === 'Escape') {
    if (dialogCount > 0) return;
    if (state.ui.rightOpen) { setRight(false); return; }
    if (state.shell) state.shell.mobileSidebarOpen = false;
    return;
  }
  if (typing) return;
  if (event.metaKey || event.ctrlKey || event.altKey) return;
  switch (event.key) {
    case '/': {
      event.preventDefault();
      const search = document.getElementById('session-search');
      if (search) { search.focus(); search.select(); }
      break;
    }
    case 't':
      if (state.route.name === 'session') {
        event.preventDefault();
        go(sessionHash(state.route.view === 'trajectory' ? 'chat' : 'trajectory'));
      }
      break;
    case '[': moveSession(-1); break;
    case ']': moveSession(1); break;
    case '?': event.preventDefault(); showShortcuts(); break;
    case 'j': moveTrajectoryRow(1); break;
    case 'k': moveTrajectoryRow(-1); break;
    default: break;
  }
}

function moveSession(delta) {
  const nodes = Array.from(document.querySelectorAll('#session-list [data-session-id]'));
  if (nodes.length === 0) return;
  let index = nodes.findIndex((node) => node.dataset.sessionId === state.route.id);
  if (index === -1) index = 0;
  const next = Math.min(nodes.length - 1, Math.max(0, index + delta));
  const id = nodes[next].dataset.sessionId;
  if (id) go(`#/sessions/${encodeURIComponent(id)}`);
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
  const backdrop = el('div', { class: 'modal modal-open dialog-backdrop' });
  const dialog = el('div', {
    class: 'modal-box dialog-box', role: 'dialog', 'aria-modal': 'true', 'aria-label': title,
  });
  const close = () => {
    document.removeEventListener('keydown', onDialogKey);
    backdrop.remove();
    dialogCount = Math.max(0, dialogCount - 1);
    dialogClose = null;
  };
  const onDialogKey = (event) => {
    if (event.key === 'Escape') { event.stopPropagation(); close(); }
  };
  dialog.appendChild(el('div', { class: 'dialog-head' }, [
    el('h2', { text: title }),
    el('span', { class: 'spacer' }),
    options.actions || null,
    el('button', { class: 'btn btn-sm btn-circle btn-ghost', type: 'button', text: '✕', 'aria-label': '关闭', onclick: close }),
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
      class: 'btn btn-sm', type: 'button', text: '刷新',
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
      class: 'btn btn-sm', type: 'button', text: '快捷键', onclick: () => showShortcuts(),
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
  let theme = 'ant';
  if (state.ui.theme === 'auto') {
    theme = window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'ant-light' : 'ant';
  } else if (state.ui.theme === 'light') {
    theme = 'ant-light';
  }
  document.documentElement.dataset.theme = theme;
}

/* ---------- DOM 助手 ---------- */

function mount(parent, children) {
  parent.replaceChildren();
  append(parent, children);
}

function append(parent, children) {
  if (children === null || children === undefined) return;
  if (Array.isArray(children)) { for (const child of children) append(parent, child); return; }
  if (children instanceof Node) { parent.appendChild(children); return; }
  parent.appendChild(document.createTextNode(String(children)));
}

/* ---------- 启动 ---------- */

if (!dom.main || !dom.right) {
  document.addEventListener('DOMContentLoaded', () => {
    dom.main = document.getElementById('main-content');
    dom.right = document.getElementById('right');
  }, { once: true });
}
