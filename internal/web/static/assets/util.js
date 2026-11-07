/* util.js — 无依赖 DOM/格式化助手（零构建，只用 textContent 写文本） */

export function el(tag, attrs = null, children = null) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [key, value] of Object.entries(attrs)) {
      if (value === null || value === undefined || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = value;
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else if (key === 'style') Object.assign(node.style, value);
      else if (key === 'aria') {
        for (const [k, v] of Object.entries(value)) node.setAttribute(`aria-${k}`, String(v));
      } else if (key.startsWith('on') && typeof value === 'function') {
        node.addEventListener(key.slice(2).toLowerCase(), value);
      } else {
        node.setAttribute(key, value === true ? '' : String(value));
      }
    }
  }
  append(node, children);
  return node;
}

export function append(parent, children) {
  if (children === null || children === undefined) return parent;
  if (Array.isArray(children)) {
    for (const child of children) append(parent, child);
    return parent;
  }
  if (children instanceof Node) {
    parent.appendChild(children);
    return parent;
  }
  parent.appendChild(document.createTextNode(String(children)));
  return parent;
}

export function clear(node) {
  while (node && node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function mount(parent, children) {
  clear(parent);
  append(parent, children);
  return parent;
}

export function icon(name) {
  const map = {
    search: '⌕', close: '×', copy: '复制', chevron: '▸', collapse: '⇤',
    expand: '⇥', refresh: '↻', folder: '▤', settings: '⚙', help: '?',
    menu: '☰', left: '‹', right: '›', down: '▾', up: '▴',
  };
  return el('span', { class: 'icon-glyph', 'aria-hidden': 'true', text: map[name] || name });
}

export function badge(text, cls = '') {
  return el('span', { class: `badge ${cls}`.trim(), text });
}

export function chip(text, pressed, onclick) {
  return el('button', {
    class: 'chip',
    type: 'button',
    'aria-pressed': pressed ? 'true' : 'false',
    onclick,
  }, text);
}

export function copyText(text) {
  const value = String(text == null ? '' : text);
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(value).then(
      () => toast('已复制'),
      () => fallbackCopy(value),
    );
    return;
  }
  fallbackCopy(value);
}

function fallbackCopy(value) {
  const textarea = el('textarea', { class: 'sr-only' });
  textarea.value = value;
  document.body.appendChild(textarea);
  textarea.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  document.body.removeChild(textarea);
  toast(ok ? '已复制' : '复制失败，请手动选择');
}

export function copyButton(text, label = '复制', cls = 'btn small ghost') {
  return el('button', {
    class: cls,
    type: 'button',
    title: `复制 ${text}`,
    onclick: (event) => { event.stopPropagation(); copyText(text); },
  }, label);
}

let toastTimer = null;
export function toast(message) {
  let node = document.getElementById('toast');
  if (!node) {
    node = el('div', { id: 'toast', class: 'toast', role: 'status', 'aria-live': 'polite' });
    document.body.appendChild(node);
  }
  node.textContent = message;
  node.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.remove('show'), 1400);
}

export function tokens(n) {
  const value = Number(n) || 0;
  if (value < 1000) return String(value);
  if (value < 1000000) return `${(value / 1000).toFixed(1)}k`;
  return `${(value / 1000000).toFixed(2)}M`;
}

export function duration(ms) {
  const value = Number(ms) || 0;
  if (value <= 0) return '0ms';
  if (value < 1000) return `${Math.round(value)}ms`;
  if (value < 60000) return `${(value / 1000).toFixed(1)}s`;
  return `${(value / 60000).toFixed(1)}m`;
}

export function formatTime(value, mode = 'local') {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  if (mode === 'utc') {
    return date.toISOString().replace('T', ' ').slice(0, 19) + 'Z';
  }
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function timeOnly(value, mode = 'local') {
  const text = formatTime(value, mode);
  return text.length > 11 ? text.slice(11) : text;
}

export function relativeTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  const diff = Date.now() - date.getTime();
  if (diff < 60 * 1000) return '刚刚';
  if (diff < 60 * 60 * 1000) return `${Math.floor(diff / 60000)} 分钟前`;
  if (diff < 24 * 60 * 60 * 1000) return `${Math.floor(diff / 3600000)} 小时前`;
  if (diff < 7 * 24 * 60 * 60 * 1000) return `${Math.floor(diff / 86400000)} 天前`;
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getMonth() + 1}-${pad(date.getDate())}`;
}

export function dayGroup(value) {
  const date = new Date(value || Date.now());
  const today = new Date();
  const start = new Date(today.getFullYear(), today.getMonth(), today.getDate());
  const target = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const diff = Math.round((start - target) / 86400000);
  if (diff <= 0) return '今天';
  if (diff === 1) return '昨天';
  return '更早';
}

export function debounce(fn, wait = 120) {
  let timer = null;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
}

export function escapeHTML(text) {
  return String(text == null ? '' : text)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

export function truncate(text, max = 120) {
  const value = String(text == null ? '' : text).replaceAll('\n', ' ');
  return value.length > max ? `${value.slice(0, max)}…` : value;
}

export function json(value) {
  try { return JSON.stringify(value, null, 2); } catch (_) { return String(value); }
}
