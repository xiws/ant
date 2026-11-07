/* trajectory.js — 轨迹表、时间线、详情检查器 */

import {
  el, badge, copyButton, duration, tokens, formatTime, json, truncate,
} from './util.js';
import { renderDiff } from './diff.js';
import { api } from './api.js';

export function renderTrajectory(view, ctx) {
  const root = el('div', { class: 'traj content-narrow' });
  const rows = Array.isArray(view.trajectory) ? view.trajectory : [];
  const state = {
    durationMode: ctx.ui.trajDuration || 'actual',
    hideTools: false,
    foldedTurns: new Set(),
    query: '',
    selected: null,
    tab: '概述',
  };

  const toolbar = el('div', { class: 'traj-toolbar' });
  const tableWrap = el('div', { class: 'traj-table-wrap' });
  const timelineWrap = el('div');
  const inspectorWrap = el('div');

  const durationChip = el('button', {
    class: 'chip', type: 'button', text: '实际时间',
    'aria-pressed': state.durationMode === 'actual' ? 'true' : 'false',
    onclick: () => {
      state.durationMode = state.durationMode === 'actual' ? 'equal' : 'actual';
      ctx.ui.trajDuration = state.durationMode;
      durationChip.textContent = state.durationMode === 'actual' ? '实际时间' : '等宽';
      durationChip.setAttribute('aria-pressed', 'true');
      renderTimeline();
    },
  });

  const foldAll = el('button', {
    class: 'btn small', type: 'button', text: '收起所有轮次',
    onclick: () => {
      const turns = new Set(rows.map((row) => row.turn));
      if (state.foldedTurns.size === 0) {
        state.foldedTurns = turns;
        foldAll.textContent = '展开所有轮次';
      } else {
        state.foldedTurns = new Set();
        foldAll.textContent = '收起所有轮次';
      }
      renderTable();
    },
  });

  const toggleTools = el('button', {
    class: 'btn small', type: 'button', text: '收起所有调用',
    onclick: () => {
      state.hideTools = !state.hideTools;
      toggleTools.textContent = state.hideTools ? '展开所有调用' : '收起所有调用';
      renderTable();
    },
  });

  const search = el('input', {
    type: 'search', placeholder: '搜索轨迹（工具、模型、错误…）',
    'aria-label': '搜索轨迹',
    oninput: (event) => { state.query = event.target.value.trim().toLowerCase(); renderTable(); },
  });

  const count = el('span', { class: 'traj-count' });
  const requestCount = rows.filter((row) => row.kind === 'request').length;
  const toolCount = rows.filter((row) => row.kind === 'tool').length;
  const turnCount = rows.filter((row) => row.kind === 'turn').length;
  count.textContent = `${turnCount} 轮 · ${requestCount} 请求 · ${toolCount} 工具调用`;

  toolbar.appendChild(durationChip);
  toolbar.appendChild(foldAll);
  toolbar.appendChild(toggleTools);
  toolbar.appendChild(search);
  toolbar.appendChild(count);
  root.appendChild(toolbar);
  root.appendChild(timelineWrap);
  root.appendChild(tableWrap);
  root.appendChild(inspectorWrap);

  function renderTimeline() {
    timelineWrap.replaceChildren();
    const timed = rows.filter((row) => row.durationMs > 0 && (row.kind === 'request' || row.kind === 'tool' || row.kind === 'turn'));
    if (timed.length === 0) {
      timelineWrap.appendChild(el('div', { class: 'timeline timeline-empty', text: '无计时数据' }));
      return;
    }
    const max = Math.max(...timed.map((row) => row.durationMs));
    const maxCount = Math.max(...timed.map((row) => row.turn)) + 1;
    const bar = el('div', { class: 'timeline', role: 'img', 'aria-label': '各请求与工具的相对耗时' });
    for (const row of timed) {
      const width = state.durationMode === 'equal'
        ? `${100 / (timed.length || 1)}%`
        : `${Math.max(1, (row.durationMs / max) * 100)}%`;
      const color = row.kind === 'request' ? 'var(--accent)' : row.kind === 'tool' ? 'var(--purple)' : 'var(--muted)';
      bar.appendChild(el('button', {
        class: 'bar', type: 'button',
        style: { width, background: color, opacity: 0.5 + (row.turn / maxCount) * 0.5 },
        title: `${row.label} · ${duration(row.durationMs)}`,
        'aria-label': `${row.label} ${duration(row.durationMs)}`,
        onclick: () => selectRow(row),
      }));
    }
    timelineWrap.appendChild(bar);
  }

  function visibleRows() {
    if (!state.query) return rows;
    return rows.filter((row) => {
      const haystack = [
        row.label, row.input, row.output, row.model, row.tools, row.status,
        row.kind, row.request && row.request.node, row.tool && row.tool.name,
        row.error && row.error.message, row.error && row.error.class,
      ].filter(Boolean).join(' ').toLowerCase();
      return haystack.includes(state.query);
    });
  }

  function renderTable() {
    tableWrap.replaceChildren();
    const list = visibleRows();
    if (list.length === 0) {
      tableWrap.appendChild(el('div', { class: 'right-empty', text: '没有匹配的轨迹行' }));
      return;
    }
    const table = el('table', { class: 'traj-table' });
    table.appendChild(el('thead', {}, el('tr', {}, [
      el('th', { text: '#' }), el('th', { text: '类型' }), el('th', { text: '输入' }),
      el('th', { text: '输出' }), el('th', { text: '思考' }), el('th', { text: '时间' }),
      el('th', { text: '模型' }), el('th', { text: '工具 / 状态' }),
    ])));
    const tbody = el('tbody');
    for (const row of list) {
      const hidden = state.foldedTurns.has(row.turn) && row.kind !== 'turn';
      if (hidden) continue;
      if (state.hideTools && row.kind === 'tool') continue;
      const tr = el('tr', {
        class: `row kind-${row.kind}`,
        dataset: { rowId: row.id },
        'aria-selected': state.selected === row ? 'true' : 'false',
        onclick: () => selectRow(row),
      }, [
        el('td', { class: 'cell-num', text: row.id }),
        el('td', { text: rowLabel(row) }),
        el('td', { class: 'cell-input', title: row.input || '', text: row.input || '—' }),
        el('td', { class: 'cell-input', title: row.output || '', text: row.output || '—' }),
        el('td', { class: 'cell-num', text: row.thinking || '—' }),
        el('td', { class: 'cell-num', text: row.durationLabel || duration(row.durationMs) }),
        el('td', { class: 'cell-num', text: row.model || '—' }),
        el('td', {}, [
          row.tools ? el('span', { class: 'muted small', text: row.tools }) : null,
          row.status ? el('span', { class: 'row-tag', text: row.status }) : null,
        ]),
      ]);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    tableWrap.appendChild(table);
  }

  function selectRow(row) {
    state.selected = row;
    state.tab = '概述';
    for (const tr of root.querySelectorAll('tr.row')) {
      tr.setAttribute('aria-selected', tr.dataset.rowId === row.id ? 'true' : 'false');
    }
    renderInspector(row);
    inspectorWrap.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  function renderInspector(row) {
    inspectorWrap.replaceChildren();
    const tabs = availableTabs(row);
    if (!tabs.includes(state.tab)) state.tab = tabs[0];
    const box = el('div', { class: 'inspector' });
    box.appendChild(el('div', { class: 'inspector-head' }, [
      el('strong', { text: `${row.id} · ${row.label}` }),
      badge(row.status || row.kind, row.status === 'failed' || row.status === 'error' ? 'failed' : ''),
      el('span', { class: 'spacer' }),
      copyButton(json({ id: row.id, kind: row.kind, seq: row.seq }), '复制定位'),
      el('button', {
        class: 'btn small ghost', type: 'button', text: '×',
        onclick: () => { inspectorWrap.replaceChildren(); state.selected = null; },
      }),
    ]));
    box.appendChild(el('div', { class: 'inspector-tabs' }, tabs.map((tab) => el('button', {
      class: 'inspector-tab', type: 'button', 'aria-selected': tab === state.tab ? 'true' : 'false',
      text: tab,
      onclick: () => { state.tab = tab; renderInspector(row); },
    }))));
    const body = el('div', { class: 'inspector-body' });
    body.appendChild(inspectorContent(row, state.tab, ctx, view.id));
    box.appendChild(body);
    inspectorWrap.appendChild(box);
  }

  ctx.inspectTool = (tool) => {
    const row = rows.find((candidate) => candidate.tool && candidate.tool.id === tool.id);
    if (row) selectRow(row);
  };

  renderTimeline();
  renderTable();
  return root;
}

function rowLabel(row) {
  switch (row.kind) {
    case 'user': return '用户';
    case 'request': return `请求 #${String(row.id).split('.').pop()}`;
    case 'tool': return `工具 ${row.tool ? row.tool.name : ''}`;
    case 'node': return '节点';
    case 'approval': return '审批';
    case 'compaction': return '压缩';
    case 'error': return '错误';
    case 'unknown': return '未知事件';
    case 'turn': return '本轮';
    default: return row.kind;
  }
}

function availableTabs(row) {
  const tabs = ['概述', '计时', '用量', '原始 JSON'];
  if (row.request || row.tool) tabs.splice(1, 0, '参数');
  if (row.request || row.tool) tabs.splice(2, 0, '结果');
  if (row.tool && row.tool.diff) tabs.splice(3, 0, '差异');
  if (row.request && row.request.inputRef) tabs.splice(tabs.length - 1, 0, '系统提示词');
  return Array.from(new Set(tabs));
}

function inspectorContent(row, tab, ctx, sessionID) {
  switch (tab) {
    case '概述':
      return el('dl', { class: 'kv' }, [
        el('dt', { text: '节点' }), el('dd', { text: (row.request && row.request.node) || (row.tool && row.tool.node) || '—' }),
        el('dt', { text: '类型' }), el('dd', { text: row.kind }),
        el('dt', { text: '状态' }), el('dd', { text: row.status || '—' }),
        el('dt', { text: 'seq' }), el('dd', { text: String(row.seq || 0) }),
        el('dt', { text: '模型' }), el('dd', { text: row.model || '未记录' }),
        el('dt', { text: '耗时' }), el('dd', { text: row.durationLabel || duration(row.durationMs) }),
        el('dt', { text: '输入' }), el('dd', { text: row.input || '—' }),
        el('dt', { text: '输出' }), el('dd', { text: row.output || '—' }),
        el('dt', { text: '思考' }), el('dd', { text: row.thinking || '未记录' }),
      ]);
    case '参数':
      return preBlock(row.request ? row.request.request : (row.tool ? row.tool.args : null));
    case '结果':
      if (row.request) return preBlock(row.request.response);
      if (row.tool) {
        const wrap = el('div');
        if (row.tool.error) wrap.appendChild(preBlock(row.tool.error));
        if (row.tool.result !== undefined) wrap.appendChild(preBlock(row.tool.result));
        if (row.tool.read && row.tool.read.content) wrap.appendChild(preBlock(row.tool.read.content));
        if (row.tool.bash && (row.tool.bash.stdout || row.tool.bash.stderr)) {
          wrap.appendChild(preBlock(`${row.tool.bash.stdout || ''}${row.tool.bash.stderr || ''}`));
        }
        if (row.tool.search && row.tool.search.results) wrap.appendChild(preBlock(row.tool.search.results.join('\n')));
        if (row.tool.write && row.tool.write.content) wrap.appendChild(preBlock(row.tool.write.content));
        if (!wrap.firstChild) wrap.appendChild(el('div', { class: 'muted small', text: '没有结果数据' }));
        return wrap;
      }
      return el('div', { class: 'muted small', text: '没有结果数据' });
    case '差异':
      return row.tool && row.tool.diff ? renderDiff(row.tool.diff) : el('div', { class: 'muted small', text: '没有 diff' });
    case '计时':
      return el('dl', { class: 'kv' }, [
        el('dt', { text: '开始' }), el('dd', { text: row.request ? formatTime(row.request.startedAt, ctx.ui.timeMode) : '—' }),
        el('dt', { text: '结束' }), el('dd', { text: row.request && row.request.endedAt ? formatTime(row.request.endedAt, ctx.ui.timeMode) : '—' }),
        el('dt', { text: '总时长' }), el('dd', { text: duration(row.durationMs) }),
        el('dt', { text: 'TTFT' }), el('dd', { text: '未记录' }),
      ]);
    case '用量': {
      const usage = (row.request && row.request.usage) || (row.tool && row.tool.usage) || null;
      if (!usage) return el('div', { class: 'muted small', text: '未记录用量' });
      return el('dl', { class: 'kv' }, [
        el('dt', { text: 'input' }), el('dd', { text: tokens(usage.input) }),
        el('dt', { text: 'output' }), el('dd', { text: tokens(usage.output) }),
        el('dt', { text: 'cached' }), el('dd', { text: tokens(usage.cached) }),
        el('dt', { text: 'reasoning' }), el('dd', { text: tokens(usage.reasoning) }),
        el('dt', { text: 'estimated' }), el('dd', { text: usage.estimated ? 'yes' : 'no' }),
      ]);
    }
    case '系统提示词': {
      const wrap = el('div');
      const ref = row.request && row.request.inputRef;
      wrap.appendChild(el('div', { class: 'warn-text', text: '系统提示词与工具定义可能包含敏感内容；默认不加载。' }));
      const box = el('div');
      wrap.appendChild(el('button', {
        class: 'btn small', type: 'button', text: '加载系统提示词',
        onclick: async () => {
          box.replaceChildren(el('div', { class: 'muted small', text: '加载中…' }));
          try {
            const text = await api.artifactText(sessionID, ref.name);
            box.replaceChildren(preBlock(text));
          } catch (error) {
            box.replaceChildren(el('div', { class: 'missing', text: error.message }));
          }
        },
      }));
      wrap.appendChild(box);
      wrap.appendChild(el('div', { class: 'muted small', text: ref ? ref.name : '' }));
      return wrap;
    }
    case '原始 JSON': {
      if (row.raw) return preBlock(row.raw);
      if (row.request) {
        return preBlock({
          request: row.request.request,
          response: row.request.response,
        });
      }
      if (row.tool) {
        return preBlock({
          args: row.tool.args,
          result: row.tool.result,
          error: row.tool.error,
          status: row.tool.status,
        });
      }
      return preBlock({ label: row.label, kind: row.kind, seq: row.seq, status: row.status });
    }
    default:
      return el('div', { class: 'muted small', text: '无数据' });
  }
}

function preBlock(value) {
  const text = typeof value === 'string' ? value : json(value);
  if (!text || text === 'null') return el('div', { class: 'muted small', text: '（空）' });
  return el('pre', { class: 'raw-json', text: truncate(text, 200000) });
}
