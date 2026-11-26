/* api.js — fetch 封装、ETag、SSE、错误转换 */

export class ApiError extends Error {
  constructor(payload, status) {
    const body = payload && payload.error ? payload.error : {};
    super(body.message || `请求失败（HTTP ${status}）`);
    this.name = 'ApiError';
    this.status = status;
    this.class = body.class || 'unknown';
    this.code = body.code || 'unknown';
    this.hint = body.hint || '';
    this.details = body.details || null;
  }
}

async function request(path, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (options.etag) headers['If-None-Match'] = options.etag;
  let body = options.body;
  if (body !== undefined && typeof body !== 'string') {
    body = JSON.stringify(body);
  }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    headers['X-ANT-Request'] = '1';
  }
  let response;
  try {
    response = await fetch(path, {
      method: options.method || 'GET',
      headers,
      body,
      cache: 'no-store',
      credentials: 'omit',
    });
  } catch (err) {
    throw new ApiError({ error: { class: 'network', code: 'fetch_failed', message: `无法连接本地服务：${err.message}` } }, 0);
  }
  const etag = response.headers.get('ETag') || '';
  if (response.status === 304) return { notModified: true, etag };
  const contentType = response.headers.get('content-type') || '';
  if (!response.ok) {
    let payload = null;
    if (contentType.includes('application/json')) {
      payload = await response.json().catch(() => null);
    }
    throw new ApiError(payload, response.status);
  }
  if (contentType.includes('application/json')) {
    return { data: await response.json(), etag };
  }
  return { data: await response.text(), etag };
}

function query(params) {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params || {})) {
    if (value === null || value === undefined || value === '') continue;
    search.set(key, String(value));
  }
  const text = search.toString();
  return text ? `?${text}` : '';
}

export const api = {
  health() {
    return request('/api/health').then((r) => r.data);
  },
  sessions(params, etag) {
    return request(`/api/sessions${query(params)}`, { etag });
  },
  session(id, params, etag) {
    return request(`/api/sessions/${encodeURIComponent(id)}${query(params)}`, { etag });
  },
  startSession(payload) {
    return request('/api/sessions', { method: 'POST', body: payload }).then((r) => r.data);
  },
  startTurn(id, payload) {
    return request(`/api/sessions/${encodeURIComponent(id)}/turns`, { method: 'POST', body: payload }).then((r) => r.data);
  },
  cancelTurn(id) {
    return request(`/api/sessions/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: {} }).then((r) => r.data);
  },
  approve(promptId, decision) {
    return request(`/api/approvals/${encodeURIComponent(promptId)}`, {
      method: 'POST', body: { decision },
    }).then((r) => r.data);
  },
  answerAsk(promptId, payload) {
    return request(`/api/asks/${encodeURIComponent(promptId)}`, {
      method: 'POST', body: payload,
    }).then((r) => r.data);
  },
  events(id, after) {
    return request(`/api/sessions/${encodeURIComponent(id)}/events${query({ after })}`).then((r) => r.data);
  },
  config() {
    return request('/api/config').then((r) => r.data);
  },
  home() {
    return request('/api/home').then((r) => r.data);
  },
  homePrompt(name) {
    return request(`/api/home/prompt${query({ name })}`).then((r) => r.data);
  },
  project() {
    return request('/api/project').then((r) => r.data);
  },
  workspaceFile(path) {
    return request(`/api/workspace/file${query({ path })}`).then((r) => r.data);
  },
  artifactURL(id, name) {
    const rel = String(name || '').replace(/^artifacts\//, '');
    const encoded = rel.split('/').map((part) => encodeURIComponent(part)).join('/');
    return `/api/sessions/${encodeURIComponent(id)}/artifacts/${encoded}`;
  },
  artifactText(id, name) {
    return request(api.artifactURL(id, name)).then((r) => r.data);
  },
  plan(id) {
    return request(`/api/sessions/${encodeURIComponent(id)}/plan`).then((r) => r.data);
  },
  openStream(id, handlers = {}) {
    if (typeof EventSource === 'undefined') {
      // 极旧浏览器降级：每 2s 触发一次 changed，由调用方带 If-None-Match 重取。
      const timer = setInterval(() => {
        if (handlers.onChanged) handlers.onChanged({ fallback: true });
      }, 2000);
      if (handlers.onOpen) handlers.onOpen();
      return { close() { clearInterval(timer); } };
    }
    let source = null;
    let closed = false;
    let retry = 1000;
    let timer = null;

    const connect = () => {
      if (closed) return;
      source = new EventSource(`/api/sessions/${encodeURIComponent(id)}/stream`);
      source.addEventListener('open', () => {
        retry = 1000;
        if (handlers.onOpen) handlers.onOpen();
      });
      source.addEventListener('changed', (event) => {
        let payload = {};
        try { payload = JSON.parse(event.data); } catch (_) { payload = {}; }
        if (handlers.onChanged) handlers.onChanged(payload);
      });
      source.addEventListener('ping', () => {
        if (handlers.onPing) handlers.onPing();
      });
      for (const [name, handler] of [
        ['status', 'onStatus'],
        ['event', 'onEvent'],
        ['delta', 'onDelta'],
        ['reasoning', 'onReasoning'],
        ['approval', 'onApproval'],
        ['ask', 'onAsk'],
        ['run_done', 'onRunDone'],
        ['run_error', 'onRunError'],
      ]) {
        source.addEventListener(name, (event) => {
          let payload = {};
          try { payload = JSON.parse(event.data); } catch (_) { payload = {}; }
          if (handlers[handler]) handlers[handler](payload);
        });
      }
      source.onerror = () => {
        if (source) source.close();
        source = null;
        if (closed) return;
        if (handlers.onError) handlers.onError();
        timer = setTimeout(connect, retry);
        retry = Math.min(10000, retry * 2);
      };
    };
    connect();
    return {
      close() {
        closed = true;
        clearTimeout(timer);
        if (source) source.close();
        source = null;
      },
    };
  },
};
