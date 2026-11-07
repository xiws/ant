/* diff.js — unified diff 渲染（文本一律 textContent） */

import { el } from './util.js';

export function renderDiff(diff, options = {}) {
  if (!diff) return null;
  if (diff.empty && !diff.raw) {
    return el('div', { class: 'missing', text: diff.warning || '（没有可展示的 diff）' });
  }
  const wrapper = el('div', { class: 'diff' });
  const body = el('div', { class: 'diff-body' + (options.collapsible !== false ? ' limited' : '') });

  const stat = el('span', { class: 'diff-stat' }, [
    el('span', { class: 'add', text: `+${diff.added || 0}` }),
    ' ',
    el('span', { class: 'del', text: `−${diff.removed || 0}` }),
  ]);
  const expander = options.collapsible !== false
    ? el('button', {
        class: 'btn small ghost',
        type: 'button',
        text: '展开全部',
        onclick: () => {
          const limited = body.classList.toggle('limited');
          expander.textContent = limited ? '展开全部' : '收起';
        },
      })
    : null;
  wrapper.appendChild(el('div', { class: 'diff-head' }, [
    el('span', { text: diff.path || 'diff' }),
    el('span', { class: 'muted small', text: `${diff.hunkCount || 0} hunk` }),
    stat,
    expander,
  ]));

  if (diff.empty || !Array.isArray(diff.hunks) || diff.hunks.length === 0) {
    body.appendChild(el('pre', { class: 'output', text: diff.raw || diff.warning || '' }));
    if (diff.warning) wrapper.appendChild(el('div', { class: 'warn-text', text: diff.warning }));
    wrapper.appendChild(body);
    return wrapper;
  }

  for (const hunk of diff.hunks) {
    const header = hunk.header ? ` ${hunk.header}` : '';
    body.appendChild(el('div', {
      class: 'diff-hunk-head',
      text: `@@ -${hunk.oldStart},${hunk.oldCount} +${hunk.newStart},${hunk.newCount} @@${header}`,
    }));
    for (const line of hunk.lines || []) {
      body.appendChild(diffLine(line));
    }
  }
  if (diff.truncated) {
    body.appendChild(el('div', { class: 'diff-hunk-head', text: '… 已截断（超过 2000 行或 50 个 hunk）' }));
  }
  if (diff.warning) {
    body.appendChild(el('div', { class: 'warn-text', text: diff.warning }));
  }
  wrapper.appendChild(body);
  return wrapper;
}

function diffLine(line) {
  const sign = line.kind === 'add' ? '+' : line.kind === 'del' ? '−' : line.kind === 'note' ? '\\' : ' ';
  const row = el('div', { class: `diff-line ${line.kind || 'ctx'}` });
  row.appendChild(el('span', { class: 'ln', text: line.oldNo ? String(line.oldNo) : '' }));
  row.appendChild(el('span', { class: 'ln', text: line.newNo ? String(line.newNo) : '' }));
  row.appendChild(el('span', { class: 'sign', text: sign, 'aria-hidden': 'true' }));
  row.appendChild(el('span', { class: 'text', text: line.text == null ? '' : line.text }));
  return row;
}

export function diffStats(diff) {
  if (!diff) return '';
  return `+${diff.added || 0} −${diff.removed || 0}`;
}
