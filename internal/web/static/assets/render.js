/* render.js — 会话头、turn、消息、工具卡、审批、摘要与右栏 */

import {
  el, badge, copyButton, tokens, duration, formatTime, relativeTime, truncate, json,
} from './util.js';
import { renderMarkdown } from './markdown.js';
import { renderDiff, diffStats } from './diff.js';
import { api } from './api.js';

const toolLabels = {
  read: 'read', read_code: 'read_code', search: 'search', bash: 'bash',
  edit: 'edit', write: 'write', ask: 'ask',
};

export function renderSessionHeader(view, ctx) {
  const meta = view.meta || {};
  const header = el('header', { class: 'session-header' });
  const title = meta.title || '（无标题会话）';
  header.appendChild(el('div', { class: 'session-header-top' }, [
    el('h1', { text: title }),
    el('div', { class: 'actions' }, [
      copyButton(meta.id || '', '复制 ID'),
      el('button', {
        class: 'btn small', type: 'button', text: '刷新', title: '刷新（不会写入任何文件）',
        onclick: () => ctx.onRefresh && ctx.onRefresh(),
      }),
      el('button', {
        class: 'btn small', type: 'button', text: ctx.ui.rightOpen ? '关闭右栏' : '打开右栏',
        onclick: () => ctx.onToggleRight && ctx.onToggleRight(),
      }),
    ]),
  ]));

  const statusText = view.lock && view.lock.live && meta.status === 'running' ? 'running' : meta.status;
  const statusBadge = badge(statusText || 'unknown', statusText || '');
  const subline = el('div', { class: 'subline' }, [
    el('span', { class: 'session-id', text: meta.id || '' }),
    badge(meta.mode || '—', `mode-${meta.mode || ''}`),
    el('span', { text: meta.model || '未记录模型' }),
    statusBadge,
    el('span', { text: `${view.stats ? view.stats.turns : meta.turnCount || 0} 轮` }),
    el('span', { text: `${tokens(view.stats ? view.stats.usage.input + view.stats.usage.output : 0)} tok` }),
    el('span', { text: `更新 ${relativeTime(meta.updatedAt)}` }),
  ]);
  header.appendChild(subline);

  if (meta.status === 'running' && !(view.lock && view.lock.live)) {
    header.appendChild(el('div', { class: 'banner' }, [
      'meta.status=running，但锁心跳已过期：上次运行可能中断。',
      el('span', { class: 'muted', text: `最后事件：${relativeTime(meta.updatedAt)}` }),
    ]));
  }
  if (view.lock && view.lock.live) {
    header.appendChild(el('div', { class: 'banner' }, [
      el('span', { class: 'dot running' }),
      '运行中：新事件会自动刷新（SSE 通知）。',
    ]));
  }
  return header;
}

export function renderChat(view, ctx) {
  const container = el('div', { class: 'content-narrow' });
  const turns = Array.isArray(view.turns) ? view.turns : [];
  if (turns.length === 0) {
    container.appendChild(el('div', { class: 'empty-state' }, [
      el('p', { text: '这个 session 还没有事件。' }),
      el('p', { class: 'small muted', text: '在终端继续对话后，刷新页面即可看到内容。' }),
    ]));
    return container;
  }
  for (const turn of turns) {
    container.appendChild(renderTurn(turn, ctx));
  }
  if (view.hasMore) {
    container.appendChild(el('div', { class: 'right-empty' }, [
      el('button', {
        class: 'btn small', type: 'button', text: '加载更早的轮次',
        onclick: () => ctx.onLoadEarlier && ctx.onLoadEarlier(view.firstTurn),
      }),
    ]));
  }
  return container;
}

function renderTurn(turn, ctx) {
  const box = el('section', { class: 'turn', id: `turn-${turn.turn}` });
  const collapsed = Boolean(ctx.ui.collapsedTurns && ctx.ui.collapsedTurns.has(turn.turn));
  const head = el('div', { class: 'turn-head' }, [
    el('span', { class: 'turn-title', text: turn.pseudo ? '会话初始化' : `第 ${turn.turn} 轮` }),
    badge(turn.status || 'unknown', turn.status || ''),
    turn.durationMs ? el('span', { text: duration(turn.durationMs) }) : null,
    turn.usage ? el('span', { text: `in ${tokens(turn.usage.input)} / out ${tokens(turn.usage.output)}` }) : null,
    turn.mode ? badge(turn.mode, `mode-${turn.mode}`) : null,
    el('button', {
      class: 'btn small ghost', type: 'button', text: collapsed ? '展开' : '收起',
      onclick: (event) => {
        event.stopPropagation();
        if (!ctx.ui.collapsedTurns) ctx.ui.collapsedTurns = new Set();
        if (ctx.ui.collapsedTurns.has(turn.turn)) ctx.ui.collapsedTurns.delete(turn.turn);
        else ctx.ui.collapsedTurns.add(turn.turn);
        box.querySelector('.turn-body').classList.toggle('collapsed');
        event.target.textContent = ctx.ui.collapsedTurns.has(turn.turn) ? '展开' : '收起';
      },
    }),
  ]);
  box.appendChild(head);

  const body = el('div', { class: 'turn-body' + (collapsed ? ' collapsed' : '') });
  for (const item of turn.items || []) {
    const node = renderItem(item, turn, ctx);
    if (node) body.appendChild(node);
  }
  if (!turn.pseudo) body.appendChild(renderTurnSummary(turn, ctx));
  box.appendChild(body);
  return box;
}

function renderItem(item, turn, ctx) {
  switch (item.kind) {
    case 'user':
      return el('div', { class: 'msg user' }, [
        el('div', { class: 'msg-head' }, [
          el('span', { class: 'msg-role', text: '你' }),
          el('span', { class: 'muted small', text: formatTime(item.ts, ctx.ui.timeMode) }),
        ]),
        el('div', { class: 'msg-bubble', text: item.text || '' }),
      ]);
    case 'assistant': {
      const content = el('div', { class: 'msg assistant' });
      const head = el('div', { class: 'msg-head' }, [
        el('span', { class: 'msg-role', text: '助手' }),
        el('span', { class: 'muted small', text: formatTime(item.ts, ctx.ui.timeMode) }),
        item.ref ? copyButton(item.ref.name, '复制引用') : null,
      ]);
      content.appendChild(head);
      if (item.refMissing) {
        content.appendChild(el('div', { class: 'missing', text: '内容缺失（artifact 已删除）' }));
      } else if (item.text) {
        content.appendChild(renderMarkdown(item.text));
      } else if (item.ref) {
        content.appendChild(el('div', { class: 'missing', text: '正文未内联，请点开 artifact。' }));
      }
      if (item.refCut) {
        content.appendChild(el('div', { class: 'warn-text' }, [
          '内容较大，仅内联前 64 KiB。',
          el('a', {
            href: api.artifactURL(turnViewID(ctx), item.ref.name), target: '_blank',
            rel: 'noopener noreferrer', text: '查看 artifact',
          }),
        ]));
      }
      return content;
    }
    case 'tool':
      return item.tool ? renderTool(item.tool, ctx) : null;
    case 'approval':
      return renderApproval(item.approval, ctx);
    case 'compaction':
      return renderCompaction(item.compaction, ctx);
    case 'error':
      return renderError(item.error, ctx);
    case 'notice':
      return el('div', { class: 'muted small', text: item.notice || '' });
    default:
      return null;
  }
}

function turnViewID(ctx) {
  return (ctx.view && ctx.view.id) || (ctx.state && ctx.state.route && ctx.state.route.id) || '';
}

export function renderTool(tool, ctx) {
  const card = el('div', { class: 'tool-card', dataset: { toolId: tool.id } });
  if (ctx.ui && ctx.ui.density === 'detailed') card.classList.add('open');
  const head = el('button', {
    class: 'tool-head', type: 'button', 'aria-expanded': card.classList.contains('open') ? 'true' : 'false',
    onclick: () => {
      const open = card.classList.toggle('open');
      head.setAttribute('aria-expanded', open ? 'true' : 'false');
    },
  }, [
    el('span', { class: 'row-tag', text: '工具' }),
    el('span', { class: 'tool-name', text: toolLabels[tool.name] || tool.name }),
    el('span', { class: 'tool-summary', text: tool.summary || compactArgs(tool.args) }),
    el('span', { class: `tool-status ${tool.status || ''}`, text: statusLabel(tool) }),
    tool.durationMs ? el('span', { class: 'muted small', text: duration(tool.durationMs) }) : null,
  ]);
  card.appendChild(head);

  const body = el('div', { class: 'tool-body' });
  if (tool.warnings && tool.warnings.length) {
    for (const warning of tool.warnings) body.appendChild(el('div', { class: 'warn-text', text: warning }));
  }
  if (tool.error) {
    body.appendChild(errorBlock(tool.error));
  }

  const args = tool.args || {};
  switch (tool.name) {
    case 'read':
      body.appendChild(readBody(tool, ctx));
      break;
    case 'read_code':
      body.appendChild(readCodeBody(tool));
      break;
    case 'search':
      body.appendChild(searchBody(tool));
      break;
    case 'bash':
      body.appendChild(bashBody(tool, ctx));
      break;
    case 'edit':
      if (tool.diff) body.appendChild(renderDiff(tool.diff));
      else body.appendChild(el('div', { class: 'missing', text: '没有可展示的 diff' }));
      break;
    case 'write':
      body.appendChild(writeBody(tool));
      break;
    case 'ask':
      body.appendChild(askBody(tool));
      break;
    default:
      body.appendChild(section('参数', el('pre', { class: 'output', text: json(args) })));
      if (tool.result !== undefined) {
        body.appendChild(section('结果', el('pre', { class: 'output', text: json(tool.result) })));
      }
  }

  if (tool.outputRef) {
    const id = turnViewID(ctx);
    body.appendChild(section('完整产物', el('a', {
      href: api.artifactURL(id, tool.outputRef), target: '_blank', rel: 'noopener noreferrer',
      text: tool.outputRef,
    })));
  }
  if (tool.resultMissing) {
    body.appendChild(el('div', { class: 'missing', text: '结果缺失（artifact 已删除），工具状态来自事件。' }));
  }
  body.appendChild(el('div', { class: 'tool-section' }, [
    el('button', {
      class: 'btn small ghost', type: 'button', text: '在轨迹中检查',
      onclick: () => ctx.onInspectTool && ctx.onInspectTool(tool),
    }),
  ]));
  card.appendChild(body);
  return card;
}

function readBody(tool, ctx) {
  const read = tool.read || {};
  const wrap = el('div');
  wrap.appendChild(el('dl', { class: 'kv' }, [
    el('dt', { text: '路径' }), el('dd', { text: read.path || argPath(tool.args) }),
    el('dt', { text: '行范围' }), el('dd', { text: readRange(read) }),
    el('dt', { text: '总行数' }), el('dd', { text: read.totalLines ? String(read.totalLines) : '未记录' }),
  ]));
  if (read.content) {
    wrap.appendChild(section('内容', el('pre', { class: 'output', text: read.content })));
  } else if (read.missing) {
    wrap.appendChild(el('div', { class: 'missing', text: '内容缺失（artifact 已删除）' }));
  }
  return wrap;
}

function readCodeBody(tool) {
  const read = tool.read || {};
  return el('div', {}, [
    el('dl', { class: 'kv' }, [
      el('dt', { text: '目标' }), el('dd', { text: read.path || '—' }),
    ]),
    read.content ? section('符号', el('pre', { class: 'output', text: read.content })) : null,
  ]);
}

function searchBody(tool) {
  const search = tool.search || {};
  const wrap = el('div');
  wrap.appendChild(el('dl', { class: 'kv' }, [
    el('dt', { text: '查询' }), el('dd', { text: search.query || '' }),
    el('dt', { text: '范围' }), el('dd', { text: search.path || 'workspace' }),
    el('dt', { text: '命中' }), el('dd', { text: `${search.matchCount || 0} 处 · ${search.scannedFiles || 0} 个文件` }),
  ]));
  if (Array.isArray(search.results) && search.results.length) {
    wrap.appendChild(section('结果', el('pre', { class: 'output', text: search.results.join('\n') })));
  }
  if (search.truncated) wrap.appendChild(el('div', { class: 'warn-text', text: '结果已截断' }));
  return wrap;
}

function bashBody(tool, ctx) {
  const bash = tool.bash || {};
  const wrap = el('div');
  wrap.appendChild(el('dl', { class: 'kv' }, [
    el('dt', { text: '命令' }), el('dd', { class: 'code', text: bash.command || compactArgs(tool.args) }),
    el('dt', { text: '状态' }), el('dd', { text: `exit ${bash.exitCode ?? '—'}${bash.timedOut ? ' · 超时' : ''}` }),
    el('dt', { text: '耗时' }), el('dd', { text: duration(tool.durationMs) }),
  ]));
  if (bash.stdout) wrap.appendChild(section('stdout', el('pre', { class: 'output', text: bash.stdout })));
  if (bash.stderr) wrap.appendChild(section('stderr', el('pre', { class: 'output err', text: bash.stderr })));
  if (!bash.stdout && !bash.stderr) wrap.appendChild(el('div', { class: 'muted small', text: '（无输出）' }));
  if (tool.truncated) wrap.appendChild(el('div', { class: 'warn-text', text: '输出已截断，完整输出见 artifact。' }));
  return wrap;
}

function writeBody(tool) {
  const write = tool.write || {};
  const wrap = el('div');
  wrap.appendChild(el('dl', { class: 'kv' }, [
    el('dt', { text: '路径' }), el('dd', { text: write.path || '' }),
    el('dt', { text: '状态' }), el('dd', { text: write.created ? '新建文件' : '覆盖写入（旧内容未持久化）' }),
    el('dt', { text: '字节' }), el('dd', { text: String(write.bytes || 0) }),
  ]));
  if (write.content) {
    wrap.appendChild(section(write.previewTruncated ? '内容预览（前 40 行）' : '写入内容',
      el('pre', { class: 'output', text: write.content })));
  }
  if (write.missing) wrap.appendChild(el('div', { class: 'missing', text: '写入结果缺失（artifact 已删除）' }));
  return wrap;
}

function askBody(tool) {
  const ask = tool.ask || {};
  return el('div', {}, [
    el('div', { class: 'card-body', text: ask.question || '' }),
    Array.isArray(ask.options) && ask.options.length
      ? el('ul', {}, ask.options.map((option) => el('li', { text: option })))
      : null,
    el('dl', { class: 'kv' }, [
      el('dt', { text: '回答' }), el('dd', { text: ask.answer || ask.selected || '未记录' }),
    ]),
  ]);
}

function renderApproval(approval, ctx) {
  if (!approval) return null;
  const decision = approval.decision || 'pending';
  return el('div', { class: 'card approval' }, [
    el('div', { class: 'card-head' }, [
      el('span', { text: '审批' }),
      badge(approval.class || 'unknown', decision === 'deny' ? 'failed' : decision === 'allow' ? 'ok' : 'interrupted'),
      el('span', { class: 'muted small', text: formatTime(approval.ts, ctx.ui.timeMode) }),
    ]),
    el('div', { class: 'card-body' }, [
      el('dl', { class: 'kv' }, [
        el('dt', { text: '动作' }), el('dd', { text: approval.action || approval.class || '—' }),
        approval.command ? el('dt', { text: '命令' }) : null,
        approval.command ? el('dd', { class: 'code', text: approval.command }) : null,
        Array.isArray(approval.paths) && approval.paths.length ? el('dt', { text: '路径' }) : null,
        Array.isArray(approval.paths) && approval.paths.length ? el('dd', { text: approval.paths.join('\n') }) : null,
        el('dt', { text: '决定' }), el('dd', { text: `${decision}${approval.scope ? ` · ${approval.scope}` : ''}` }),
        approval.reason ? el('dt', { text: '原因' }) : null,
        approval.reason ? el('dd', { text: approval.reason }) : null,
      ]),
    ]),
  ]);
}

function renderCompaction(compaction, ctx) {
  if (!compaction) return null;
  const body = el('div', { class: 'card-body' }, [
    el('span', { class: 'muted small', text: `${compaction.beforeTokens} → ${compaction.afterTokens} tok` }),
    compaction.removed ? el('span', { class: 'muted small', text: ` · 移除 ${compaction.removed} 条 / 保留 ${compaction.kept} 条` }) : null,
  ]);
  if (compaction.summary) {
    body.appendChild(el('details', {}, [
      el('summary', { class: 'small', text: '查看压缩摘要' }),
      renderMarkdown(compaction.summary),
    ]));
  } else if (compaction.missing) {
    body.appendChild(el('div', { class: 'missing', text: '摘要缺失（artifact 已删除）' }));
  }
  return el('div', { class: 'card compaction' }, [
    el('div', { class: 'card-head' }, [el('span', { text: '上下文压缩' })]),
    body,
  ]);
}

function renderError(error, ctx) {
  if (!error) return null;
  return el('div', { class: 'card error' }, [
    el('div', { class: 'card-head' }, [
      el('span', { text: '错误' }),
      error.class ? badge(error.class, 'failed') : null,
      error.code ? el('span', { class: 'muted small', text: error.code }) : null,
    ]),
    el('div', { class: 'card-body' }, [
      el('div', { text: error.message || '未记录错误信息' }),
      error.hint ? el('div', { class: 'muted small', text: `提示：${error.hint}` }) : null,
      error.step ? el('div', { class: 'muted small', text: `step: ${error.step}` }) : null,
    ]),
  ]);
}

function renderTurnSummary(turn, ctx) {
  const box = el('div', { class: 'summary-line' });
  const status = turn.status || 'unknown';
  box.appendChild(el('strong', { text: turnLabel(status) }));
  if (turn.durationMs) box.appendChild(el('span', { text: duration(turn.durationMs) }));
  if (turn.usage) {
    box.appendChild(el('span', { text: `in ${tokens(turn.usage.input)} / out ${tokens(turn.usage.output)}${turn.usage.cached ? ` / cached ${tokens(turn.usage.cached)}` : ''}` }));
  }
  if (turn.exitCode !== undefined && turn.exitCode !== null) box.appendChild(el('span', { text: `exit ${turn.exitCode}` }));
  if (Array.isArray(turn.changed) && turn.changed.length) {
    box.appendChild(el('span', { text: `改动：${turn.changed.join(', ')}` }));
  }
  const wrapper = el('div', { class: 'msg' }, [box]);
  if (Array.isArray(turn.checks) && turn.checks.length) {
    const list = el('ul', { class: 'check-list' });
    for (const check of turn.checks) {
      list.appendChild(el('li', { text: `${check.cmd || 'check'} · exit ${check.exitCode ?? '—'}` }));
    }
    wrapper.appendChild(list);
  }
  if (turn.planPath) {
    wrapper.appendChild(el('div', { class: 'card' }, [
      el('div', { class: 'card-head' }, [
        el('span', { text: '计划' }),
        badge('plan', 'mode-plan'),
      ]),
      el('div', { class: 'card-body' }, [
        el('div', { class: 'code small', text: turn.planPath }),
        el('div', { class: 'tool-section' }, [
          el('button', {
            class: 'btn small', type: 'button', text: '查看计划',
            onclick: () => ctx.onOpenPlan && ctx.onOpenPlan(),
          }),
          ' ',
          copyButton(turn.planPath, '复制路径', 'btn small ghost'),
        ]),
      ]),
    ]));
  }
  return wrapper;
}

function turnLabel(status) {
  switch (status) {
    case 'ok': return '本轮完成';
    case 'failed': return '本轮失败';
    case 'interrupted': return '本轮中断';
    case 'running': return '本轮运行中';
    default: return '本轮状态未知';
  }
}

function statusLabel(tool) {
  const status = tool.status || 'unknown';
  const showExit = tool.exitCode !== undefined && tool.exitCode !== null
    && (tool.name === 'bash' || tool.exitCode !== 0);
  if (status === 'ok') return `ok${showExit ? ` · exit ${tool.exitCode}` : ''}`;
  if (status === 'error') return tool.error && tool.error.code ? `error · ${tool.error.code}` : 'error';
  if (status === 'denied') return 'denied';
  if (status === 'stopped') return '已中断';
  if (status === 'pending') return '运行中';
  return status;
}

function errorBlock(error) {
  return el('div', { class: 'card error' }, [
    el('div', { class: 'card-body' }, [
      el('div', { text: error.message || error.code || '未知错误' }),
      error.hint ? el('div', { class: 'muted small', text: `提示：${error.hint}` }) : null,
    ]),
  ]);
}

function section(label, content) {
  if (!content) return null;
  return el('div', { class: 'tool-section' }, [
    el('div', { class: 'label', text: label }),
    content,
  ]);
}

function argPath(args) {
  return args && typeof args === 'object' && args.path ? args.path : '';
}

function readRange(read) {
  if (!read || (!read.startLine && !read.endLine)) return '全文';
  return `${read.startLine || 1}–${read.endLine || '末尾'}`;
}

export function compactArgs(args) {
  if (args === null || args === undefined) return '';
  if (typeof args === 'string') return truncate(args, 120);
  if (typeof args === 'object') {
    const keys = ['path', 'command', 'query', 'target', 'question'];
    for (const key of keys) {
      if (args[key]) return truncate(String(args[key]), 120);
    }
    for (const [key, value] of Object.entries(args)) {
      if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
        return truncate(`${key}=${value}`, 120);
      }
    }
  }
  return truncate(json(args), 120);
}

/* ---------- 右栏 ---------- */

export function renderRightPanel(view, ctx) {
  const tabs = [
    { id: 'changes', label: `改动${view.changes && view.changes.length ? ` (${view.changes.length})` : ''}` },
    { id: 'files', label: '文件' },
    { id: 'artifacts', label: `产物${view.artifacts && view.artifacts.length ? ` (${view.artifacts.length})` : ''}` },
    { id: 'overview', label: '概览' },
  ];
  const active = ctx.ui.rightTab || 'changes';
  const panel = el('div', { class: 'right-inner' });
  panel.appendChild(el('div', { class: 'right-head' }, [
    el('strong', { text: '上下文' }),
    el('span', { class: 'spacer' }),
    el('button', {
      class: 'btn small ghost', type: 'button', text: '×', title: '关闭右栏（Esc）',
      onclick: () => ctx.onToggleRight && ctx.onToggleRight(false),
    }),
  ]));
  panel.appendChild(el('div', { class: 'right-tabs', role: 'tablist' }, tabs.map((tab) => el('button', {
    class: 'right-tab', type: 'button', role: 'tab',
    'aria-selected': tab.id === active ? 'true' : 'false',
    text: tab.label,
    onclick: () => { ctx.ui.rightTab = tab.id; ctx.onRightTab && ctx.onRightTab(tab.id); },
  }))));
  const body = el('div', { class: 'right-body', role: 'tabpanel' });

  if (active === 'changes') body.appendChild(renderChanges(view, ctx));
  else if (active === 'files') body.appendChild(renderFileTab(view, ctx));
  else if (active === 'artifacts') body.appendChild(renderArtifacts(view, ctx));
  else body.appendChild(renderOverview(view, ctx));
  panel.appendChild(body);
  return panel;
}

function renderChanges(view, ctx) {
  const wrap = el('div');
  const changes = view.changes || [];
  if (!changes.length) {
    wrap.appendChild(el('div', { class: 'right-empty', text: '本会话没有记录到文件改动。' }));
    return wrap;
  }
  for (const change of changes) {
    wrap.appendChild(el('div', {
      class: 'change-row',
      title: `${change.path}\n${change.source || ''}${change.turn ? ` · 第 ${change.turn} 轮` : ''}`,
    }, [
      el('button', {
        class: 'change-path', type: 'button', style: { textAlign: 'left', background: 'none', border: 'none', padding: '0' },
        text: change.path,
        onclick: () => ctx.onOpenFile && ctx.onOpenFile(change.path),
      }),
      el('button', {
        class: 'change-stat', type: 'button', style: { background: 'none', border: 'none' },
        title: '查看 diff / 写入内容',
        onclick: () => ctx.onOpenChange && ctx.onOpenChange(change),
      }, [
        el('span', { class: 'add', text: `+${change.added || 0}` }), ' ',
        el('span', { class: 'del', text: `−${change.removed || 0}` }),
        change.status === 'created' ? el('span', { class: 'badge ok', text: '新增' }) : null,
      ]),
    ]));
  }
  wrap.appendChild(el('div', { class: 'muted small', text: '点击文件查看当前内容；edit 卡片内可查看 diff。' }));
  return wrap;
}

function renderFileTab(view, ctx) {
  const preview = ctx.ui.filePreview;
  if (!preview) {
    return el('div', { class: 'right-empty', text: '从“改动”或“产物”选择文件查看预览。' });
  }
  const wrap = el('div');
  wrap.appendChild(el('div', { class: 'file-preview-head' }, [
    el('span', { text: preview.name || '' }),
    el('span', { class: 'spacer' }),
    copyButton(preview.name || '', '复制路径', 'btn small ghost'),
    preview.source === 'artifact' ? el('a', {
      class: 'btn small ghost',
      href: api.artifactURL(view.id, preview.name || ''),
      target: '_blank', rel: 'noopener noreferrer', text: '原文件',
    }) : null,
  ]));
  if (preview.error) {
    wrap.appendChild(el('div', { class: 'missing', text: preview.error }));
    return wrap;
  }
  if (preview.binary) {
    wrap.appendChild(el('div', { class: 'missing', text: '二进制文件，不在页面内预览。' }));
    return wrap;
  }
  const lines = String(preview.content || '').split('\n');
  const linesBox = el('div', { class: 'file-lines' });
  lines.forEach((text, index) => {
    linesBox.appendChild(el('div', { class: 'file-line' }, [
      el('span', { class: 'ln', text: String(index + 1) }),
      el('span', { class: 'text', text }),
    ]));
  });
  wrap.appendChild(linesBox);
  if (preview.truncated) {
    wrap.appendChild(el('div', { class: 'warn-text', text: '文件较大，仅显示前面部分（上限 2 MiB）。' }));
  }
  return wrap;
}

function renderArtifacts(view, ctx) {
  const wrap = el('div');
  const artifacts = view.artifacts || [];
  if (!artifacts.length) {
    wrap.appendChild(el('div', { class: 'right-empty', text: '这个 session 还没有 artifact。' }));
    return wrap;
  }
  for (const artifact of artifacts) {
    wrap.appendChild(el('button', {
      class: 'artifact-row', type: 'button',
      title: `${artifact.name} · ${artifact.bytes} bytes`,
      onclick: () => ctx.onOpenArtifact && ctx.onOpenArtifact(artifact),
    }, [
      el('span', { class: 'artifact-name', text: artifact.name }),
      el('span', { class: 'muted small', text: formatBytes(artifact.bytes) }),
      artifact.previewable ? el('span', { class: 'row-tag', text: '预览' }) : el('span', { class: 'row-tag', text: '下载' }),
    ]));
  }
  wrap.appendChild(el('div', { class: 'muted small', text: 'artifact 可能包含敏感内容，只在本地展示。' }));
  return wrap;
}

function renderOverview(view, ctx) {
  const meta = view.meta || {};
  const stats = view.stats || {};
  const wrap = el('div');
  wrap.appendChild(el('dl', { class: 'overview-list' }, [
    el('dt', { text: 'Session' }), el('dd', { class: 'code', text: meta.id || '' }),
    el('dt', { text: '状态' }), el('dd', { text: meta.status || '—' }),
    el('dt', { text: '模式 / 模型' }), el('dd', { text: `${meta.mode || '—'} / ${meta.model || '—'}` }),
    el('dt', { text: '轮次' }), el('dd', { text: String(stats.turns || meta.turnCount || 0) }),
    el('dt', { text: '请求 / 工具' }), el('dd', { text: `${stats.requests || 0} / ${stats.toolCalls || 0}` }),
    el('dt', { text: 'Token' }), el('dd', { text: `in ${tokens((stats.usage || {}).input)} / out ${tokens((stats.usage || {}).output)} / cached ${tokens((stats.usage || {}).cached)}` }),
    el('dt', { text: '创建' }), el('dd', { text: formatTime(meta.createdAt, ctx.ui.timeMode) }),
    el('dt', { text: '更新' }), el('dd', { text: formatTime(meta.updatedAt, ctx.ui.timeMode) }),
    el('dt', { text: '项目上下文' }), el('dd', { class: 'code', text: meta.projectContextHash || '未记录' }),
    el('dt', { text: 'Workspace' }), el('dd', { class: 'code', text: meta.workspace || '' }),
  ]));
  wrap.appendChild(el('div', { class: 'tool-section' }, [
    el('div', { class: 'label', text: '操作' }),
    el('div', {}, [
      copyButton(meta.id || '', '复制 ID', 'btn small'),
      ' ',
      copyButton(`ant -s ${meta.id || ''}`, '复制续跑命令', 'btn small'),
    ]),
  ]));
  return wrap;
}

function formatBytes(bytes) {
  const value = Number(bytes) || 0;
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

/* ---------- 空态 / 加载 ---------- */

export function renderEmptyMain(sessions) {
  const wrap = el('div', { class: 'content-narrow' });
  if (!sessions || sessions.length === 0) {
    wrap.appendChild(el('div', { class: 'empty-state' }, [
      el('h2', { text: '当前 workspace 还没有 session' }),
      el('p', { text: '在终端运行：' }),
      el('p', {}, el('code', { text: 'ant "你的任务"' })),
      el('p', { class: 'small muted', text: '会话创建后，刷新此页面即可浏览。' }),
    ]));
    return wrap;
  }
  wrap.appendChild(el('div', { class: 'empty-state' }, [
    el('h2', { text: '选择一个会话' }),
    el('p', { class: 'small muted', text: '左栏列出了当前 workspace 的会话历史；ant serve 一期只读。' }),
  ]));
  return wrap;
}

export function renderLoading(text = '加载中…') {
  return el('div', { class: 'empty-state' }, [
    el('div', { class: 'dot running' }),
    el('p', { text }),
  ]);
}

export function renderErrorCard(error, retry) {
  return el('div', { class: 'content-narrow' }, [
    el('div', { class: 'card error' }, [
      el('div', { class: 'card-head' }, [el('span', { text: '加载失败' }), badge(error.status ? `HTTP ${error.status}` : 'error', 'failed')]),
      el('div', { class: 'card-body' }, [
        el('div', { text: error.message || String(error) }),
        error.hint ? el('div', { class: 'muted small', text: `提示：${error.hint}` }) : null,
        error.code ? el('div', { class: 'muted small', text: `code: ${error.code}` }) : null,
        retry ? el('button', { class: 'btn small', type: 'button', text: '重试', onclick: retry }) : null,
      ]),
    ]),
  ]);
}

/* ---------- 对话框内容 ---------- */

export function renderLocalConfig(health, config, project) {
  const wrap = el('div');
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: '工作区' }),
    el('dl', { class: 'overview-list' }, [
      el('dt', { text: '路径' }), el('dd', { class: 'code', text: health.workspace || '' }),
      el('dt', { text: '项目上下文' }), el('dd', { dataset: { role: 'project' },
        text: project && project.exists
          ? `${project.path} · ${project.hash || '无 hash'} · ${formatBytes(project.bytes)}`
          : '尚未生成（ant init）',
      }),
    ]),
  ]));
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: 'Home' }),
    el('dl', { class: 'overview-list' }, [
      el('dt', { text: '路径' }), el('dd', { class: 'code', text: health.home || '' }),
    ]),
    homeTable(),
  ]));
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: '模型（只读，不含密钥）' }),
    config && config.available
      ? modelTable(config.models || [])
      : el('div', { class: 'missing', text: (config && config.error) || '配置不可用' }),
  ]));
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: '配置来源' }),
    config && config.origins
      ? originTable(config.origins)
      : el('div', { class: 'muted small', text: '无来源信息' }),
  ]));
  const warnings = (config && config.warnings) || [];
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: '告警' }),
    warnings.length
      ? el('ul', {}, warnings.map((warning) => el('li', { class: 'warn-text', text: warning })))
      : el('div', { class: 'muted small', text: '没有配置告警' }),
  ]));
  return wrap;
}

function homeTable() {
  const box = el('div', { class: 'home-files', dataset: { role: 'home-files' } });
  box.appendChild(el('div', { class: 'muted small', text: '加载中…' }));
  return box;
}

export function fillHomeTable(box, home) {
  const files = (home && home.files) || [];
  if (!files.length) {
    box.textContent = 'home 下没有白名单内的配置文件';
    return;
  }
  const table = el('table');
  table.appendChild(el('thead', {}, el('tr', {}, [
    el('th', { text: '文件' }), el('th', { text: '大小' }), el('th', { text: '修改时间' }), el('th', { text: '内容' }),
  ])));
  const tbody = el('tbody');
  for (const file of files) {
    tbody.appendChild(el('tr', {}, [
      el('td', { class: 'code', text: file.name }),
      el('td', { text: formatBytes(file.bytes) }),
      el('td', { text: formatTime(file.modTime) }),
      el('td', {}, file.name.endsWith('.md')
        ? el('button', {
            class: 'btn small ghost', type: 'button', text: '预览',
            dataset: { prompt: file.name }, onclick: () => window.dispatchEvent(new CustomEvent('ant:prompt', { detail: file.name })),
          })
        : el('span', { class: 'muted small', text: '只解析' })),
    ]));
  }
  table.appendChild(tbody);
  box.replaceChildren(table);
}

function modelTable(models) {
  if (!models.length) return el('div', { class: 'muted small', text: '没有配置模型' });
  const table = el('table');
  table.appendChild(el('thead', {}, el('tr', {}, [
    el('th', { text: '别名' }), el('th', { text: 'provider' }), el('th', { text: 'id' }),
    el('th', { text: 'context' }), el('th', { text: 'tools' }), el('th', { text: 'stream' }),
  ])));
  const tbody = el('tbody');
  for (const model of models) {
    tbody.appendChild(el('tr', {}, [
      el('td', { text: model.name }),
      el('td', { text: model.provider || '' }),
      el('td', { class: 'code', text: model.id || '' }),
      el('td', { text: String(model.contextWindow || 0) }),
      el('td', { text: model.supportsTools ? 'yes' : 'no' }),
      el('td', { text: model.supportsStreaming ? 'yes' : 'no' }),
    ]));
  }
  table.appendChild(tbody);
  return table;
}

function originTable(origins) {
  const entries = Object.entries(origins);
  if (!entries.length) return el('div', { class: 'muted small', text: '无来源信息' });
  const table = el('table');
  table.appendChild(el('thead', {}, el('tr', {}, [
    el('th', { text: '键' }), el('th', { text: '来源' }),
  ])));
  const tbody = el('tbody');
  for (const [key, origin] of entries.sort((a, b) => a[0].localeCompare(b[0]))) {
    tbody.appendChild(el('tr', {}, [el('td', { class: 'code', text: key }), el('td', { text: origin })]));
  }
  table.appendChild(tbody);
  return table;
}

export function renderSettings(health, ui, ctx) {
  const wrap = el('div');
  const select = (label, value, options, onChange) => el('label', {
    class: 'dialog-section',
    style: { display: 'flex', gap: '8px', alignItems: 'center' },
  }, [
    el('span', { style: { minWidth: '88px' }, text: label }),
    el('select', {
      onchange: (event) => onChange(event.target.value),
    }, options.map(([id, text]) => el('option', { value: id, selected: id === value, text }))),
  ]);
  wrap.appendChild(select('主题', ui.theme, [['auto', '跟随系统'], ['light', '浅色'], ['dark', '深色']], (v) => ctx.onTheme(v)));
  wrap.appendChild(select('工具详情', ui.density, [['concise', '简洁'], ['standard', '标准'], ['detailed', '详细']], (v) => ctx.onDensity(v)));
  wrap.appendChild(select('时间显示', ui.timeMode, [['local', '本地时间'], ['utc', 'UTC']], (v) => ctx.onTimeMode(v)));
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('h3', { text: '关于' }),
    el('dl', { class: 'overview-list' }, [
      el('dt', { text: '版本' }), el('dd', { text: health.version || 'dev' }),
      el('dt', { text: 'Workspace' }), el('dd', { class: 'code', text: health.workspace || '' }),
      el('dt', { text: 'Home' }), el('dd', { class: 'code', text: health.home || '' }),
      el('dt', { text: '监听' }), el('dd', { class: 'code', text: window.location.origin }),
      el('dt', { text: '模式' }), el('dd', { text: '只读 · 仅回环 · 本地单用户' }),
    ]),
  ]));
  wrap.appendChild(el('div', { class: 'dialog-section' }, [
    el('div', { class: 'muted small', text: '设置只保存在当前浏览器 localStorage；服务端不保存偏好。' }),
  ]));
  return wrap;
}

export function renderShortcuts() {
  const rows = [
    ['/', '聚焦会话搜索'],
    ['t', '对话 / 轨迹切换'],
    ['[ / ]', '上一个 / 下一个 session'],
    ['j / k', '轨迹中下一个 / 上一个 turn'],
    ['Esc', '关闭对话框 / 右栏'],
    ['?', '快捷键帮助'],
  ];
  return el('table', {}, [
    el('thead', {}, el('tr', {}, [el('th', { text: '键' }), el('th', { text: '行为' })])),
    el('tbody', {}, rows.map(([key, behavior]) => el('tr', {}, [
      el('td', { class: 'code', text: key }),
      el('td', { text: behavior }),
    ]))),
  ]);
}

export { diffStats };
