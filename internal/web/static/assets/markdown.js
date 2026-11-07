/* markdown.js — 安全的 Markdown 子集（先转义，白名单语法，禁止原始 HTML） */

import { el, copyButton } from './util.js';

const blockStart = /^\s{0,3}(#{1,6}\s*|[-+]\s*|\*\s+|\d+[.)]\s+|>|```|-{3,}\s*$|\*{3,}\s*$)/;

export function renderMarkdown(text) {
  const root = el('div', { class: 'md' });
  const source = String(text == null ? '' : text).replaceAll('\r\n', '\n');
  const lines = source.split('\n');
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s{0,3}```([^\s`]*)\s*$/);
    if (fence) {
      const lang = fence[1] || '';
      i += 1;
      const body = [];
      while (i < lines.length && !/^\s{0,3}```\s*$/.test(lines[i])) {
        body.push(lines[i]);
        i += 1;
      }
      if (i < lines.length) i += 1;
      root.appendChild(codeBlock(body.join('\n'), lang));
      continue;
    }
    if (/^\s*$/.test(line)) { i += 1; continue; }

    const heading = line.match(/^\s{0,3}(#{1,6})\s*(\S.*)$/);
    if (heading) {
      const level = Math.min(4, heading[1].length);
      root.appendChild(el(`h${level}`, {}, inline(heading[2].trim())));
      i += 1;
      continue;
    }
    if (/^\s*(-{3,}|\*{3,})\s*$/.test(line)) {
      root.appendChild(el('hr'));
      i += 1;
      continue;
    }
    if (/^\s{0,3}>\s?/.test(line)) {
      const quote = [];
      while (i < lines.length && /^\s{0,3}>\s?/.test(lines[i])) {
        quote.push(lines[i].replace(/^\s{0,3}>\s?/, ''));
        i += 1;
      }
      root.appendChild(el('blockquote', {}, inline(quote.join(' '))));
      continue;
    }
    if (isBullet(line)) {
      const list = el('ul');
      while (i < lines.length && isBullet(lines[i])) {
        list.appendChild(el('li', {}, inline(bulletText(lines[i]))));
        i += 1;
      }
      root.appendChild(list);
      continue;
    }
    if (/^\s{0,3}\d+[.)]\s+/.test(line)) {
      const list = el('ol');
      while (i < lines.length && /^\s{0,3}\d+[.)]\s+/.test(lines[i])) {
        list.appendChild(el('li', {}, inline(lines[i].replace(/^\s{0,3}\d+[.)]\s+/, ''))));
        i += 1;
      }
      root.appendChild(list);
      continue;
    }

    const paragraph = [line.trim()];
    i += 1;
    while (i < lines.length && !/^\s*$/.test(lines[i]) && !blockStart.test(lines[i])) {
      paragraph.push(lines[i].trim());
      i += 1;
    }
    root.appendChild(el('p', {}, inline(paragraph.join(' '))));
  }
  return root;
}

function codeBlock(text, lang) {
  const wrapper = el('div', { class: 'code-block' });
  if (lang) wrapper.appendChild(el('span', { class: 'code-lang', text: lang }));
  wrapper.appendChild(copyButton(text, '复制', 'btn small ghost copy-btn'));
  const code = el('code', { text });
  wrapper.appendChild(el('pre', { class: lang ? `language-${lang}` : '' }, code));
  return wrapper;
}

function isBullet(line) {
  if (/^\s{0,3}\*\s+/.test(line)) return true;
  if (/^\s{0,3}[-+]\s*\S/.test(line)) return !/^\s{0,3}[-+]{3,}\s*$/.test(line);
  return false;
}

function bulletText(line) {
  return line.replace(/^\s{0,3}(\*\s+|[-+]\s*)/, '');
}

const inlinePattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|\[[^\]\n]+\]\([^)\s]+\))/g;

function inline(text) {
  const nodes = [];
  const source = String(text == null ? '' : text);
  let last = 0;
  for (const match of source.matchAll(inlinePattern)) {
    if (match.index > last) nodes.push(document.createTextNode(source.slice(last, match.index)));
    const token = match[0];
    if (token.startsWith('`')) {
      nodes.push(el('code', { text: token.slice(1, -1) }));
    } else if (token.startsWith('**')) {
      nodes.push(el('strong', {}, inline(token.slice(2, -2))));
    } else if (token.startsWith('*')) {
      nodes.push(el('em', {}, inline(token.slice(1, -1))));
    } else {
      const link = token.match(/^\[([^\]]+)\]\(([^)]+)\)$/);
      const href = link ? safeHref(link[2]) : null;
      if (href) {
        nodes.push(el('a', {
          href,
          target: '_blank',
          rel: 'noopener noreferrer',
          text: link[1],
        }));
      } else {
        nodes.push(document.createTextNode(token));
      }
    }
    last = match.index + token.length;
  }
  if (last < source.length) nodes.push(document.createTextNode(source.slice(last)));
  return nodes;
}

function safeHref(raw) {
  const href = String(raw || '').trim();
  if (!href) return null;
  if (href.startsWith('http://') || href.startsWith('https://')) return href;
  if (href.startsWith('/') || href.startsWith('#') || href.startsWith('./') || href.startsWith('../')) return href;
  // 其他协议（javascript:/data: 等）一律不生成链接。
  return null;
}
