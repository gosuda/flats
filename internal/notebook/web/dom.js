// DOM helpers shared by the notebook modules. Text goes through text nodes
// and attributes only; nothing here parses HTML, and nothing sets inline
// style attributes, so the shell's CSP (style-src 'self') holds.
'use strict';


export const $ = (sel, root = document) => root.querySelector(sel);

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  append(el, children);
  return el;
}

export function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return el;
}

export const replace = (el, ...kids) => { el.replaceChildren(); return append(el, kids); };

const ICONS = {
  chev: 'M9 6l6 6-6 6',
  page: 'M6 3h8l4 4v14H6zM14 3v4h4',
  plus: 'M12 5v14M5 12h14',
  grip: 'M9 6h.01M15 6h.01M9 12h.01M15 12h.01M9 18h.01M15 18h.01',
  history: 'M3 12a9 9 0 1 0 3-6.7M3 4v5h5M12 7.5V12l3 2',
  info: 'M12 3a9 9 0 110 18 9 9 0 010-18zM12 11v6M12 7.5v.01',
  warn: 'M12 3l10 18H2zM12 10v5M12 18v.01',
  ok: 'M12 3a9 9 0 110 18 9 9 0 010-18zM8 12.5l3 3 5-6',
  bad: 'M12 3a9 9 0 110 18 9 9 0 010-18zM9 9l6 6M15 9l-6 6',
  menu: 'M4 6h16M4 12h16M4 18h16',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13',
  close: 'M6 6l12 12M18 6L6 18',
  link: 'M10 14a4 4 0 005.7 0l3-3a4 4 0 00-5.7-5.7l-1 1M14 10a4 4 0 00-5.7 0l-3 3a4 4 0 005.7 5.7l1-1',
  copy: 'M10 8h8a2 2 0 012 2v8a2 2 0 01-2 2h-8a2 2 0 01-2-2v-8a2 2 0 012-2zM16 8V5a1 1 0 00-1-1H5a1 1 0 00-1 1v10a1 1 0 001 1h3',
  download: 'M12 4v11M7 10l5 5 5-5M5 20h14',
  eye: 'M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12zM12 9.4a2.6 2.6 0 110 5.2 2.6 2.6 0 010-5.2z',
  table: 'M4 5h16v14H4zM4 10h16M10 10v9',
  image: 'M5.5 4.5h13a2 2 0 012 2v11a2 2 0 01-2 2h-13a2 2 0 01-2-2v-11a2 2 0 012-2zM9 8.2a1.8 1.8 0 110 3.6 1.8 1.8 0 010-3.6zM20 16l-5-5-9 8.5',
  text: 'M5 6V4h14v2M12 4v16M9 20h6',
  list: 'M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01',
  check: 'M5 12.5l4.5 4.5L19 7',
  quote: 'M7 7h4v6H7zM7 13c0 2-1 3-3 4M15 7h4v6h-4zM15 13c0 2-1 3-3 4',
  code: 'M8 8l-4 4 4 4M16 8l4 4-4 4',
  minus: 'M5 12h14',
  bot: 'M6 9h12v10H6zM12 4v5M9.5 14h.01M14.5 14h.01',
};

export function icon(name, label) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('class', 'i');
  svg.setAttribute('viewBox', '0 0 24 24');
  if (label) { svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', label); } else svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', ICONS[name] || ICONS.page);
  svg.appendChild(p);
  return svg;
}

export const plural = (n, one, many = one + 's') => `${n} ${n === 1 ? one : many}`;

// toast shows a short notice. Toasts with actions stay longer, because an
// action can be the only way to recover something.
export function toast(content, { actions = [], timeout, error = false } = {}) {
  const host = $('.toasts');
  if (!host) return null;
  const close = () => t.remove();
  const t = h('div', { class: 'toast' + (error ? ' error' : ''), role: error ? 'alert' : 'status' },
    h('span', { class: 'toast-text' }, content),
    actions.map(([label, fn]) => h('button', { type: 'button', class: 'btn', onclick: () => { close(); fn(); } }, label)),
    h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Dismiss', onclick: close }, icon('close')));
  host.appendChild(t);
  while (host.children.length > 4) host.firstChild.remove();
  const ms = timeout ?? (actions.length ? 10000 : 4000);
  if (ms) setTimeout(close, ms);
  return t;
}
