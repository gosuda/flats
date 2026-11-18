// DOM helpers shared by the sheet modules. Text goes through text nodes and
// attributes only; nothing here parses HTML, and nothing sets an inline
// style attribute, so the shell's CSP (style-src 'self') holds. Positions
// and sizes are set through the CSSOM (`el.style.left = ...`).
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
  undo: 'M9 14 4 9l5-5M4 9h10a6 6 0 0 1 0 12h-3',
  redo: 'm15 14 5-5-5-5M20 9H10a6 6 0 0 0 0 12h3',
  left: 'M4 6h16M4 12h10M4 18h14',
  center: 'M4 6h16M7 12h10M5 18h14',
  right: 'M4 6h16M10 12h10M6 18h14',
  wrap: 'M4 6h16M4 12h13a3 3 0 0 1 0 6h-4m0 0 2-2m-2 2 2 2M4 18h5',
  freeze: 'M3 9h18M9 3v18M3 3h18v18H3z',
  asc: 'M7 4v16m0 0-3-3m3 3 3-3M13 6h4M13 12h6M13 18h8',
  desc: 'M7 4v16m0 0-3-3m3 3 3-3M13 6h8M13 12h6M13 18h4',
  history: 'M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5M12 7v5l3 2',
  rows: 'M3 5h18v14H3zM3 10h18M3 15h18M9 5v14',
  plus: 'M12 5v14M5 12h14',
  find: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20 20l-4-4',
  download: 'M12 4v11m0 0-4-4m4 4 4-4M5 20h14',
  sheet: 'M4 4h16v16H4zM4 9h16M4 14h16M10 4v16',
  close: 'M6 6l12 12M18 6 6 18',
  check: 'M5 12.5l4.5 4.5L19 7',
  pin: 'M9 4h6l-1 6 3 3H7l3-3-1-6zM12 13v7',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  keys: 'M3 7h18v10H3zM7 11h.01M11 11h.01M15 11h.01M8 14h8',
  up: 'M12 19V5m0 0-6 6m6-6 6 6',
  down: 'M12 5v14m0 0-6-6m6 6 6-6',
  bot: 'M6 9h12v10H6zM12 4v5M9.5 14h.01M14.5 14h.01',
};

export function icon(name, label) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('class', 'i');
  svg.setAttribute('viewBox', '0 0 24 24');
  if (label) { svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', label); } else svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', ICONS[name]);
  svg.appendChild(p);
  return svg;
}

export const plural = (n, one, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// button builds a labeled button; an icon button names itself through
// aria-label and shows the same words on hover.
export function button(label, onclick, attrs = {}, ...kids) {
  return h('button', { type: 'button', onclick, ...attrs }, kids.length ? kids : label);
}

export function iconButton(name, label, onclick, attrs = {}) {
  return h('button', { type: 'button', class: 'tb', 'aria-label': label, title: label, onclick, ...attrs }, typeof name === 'string' ? icon(name) : name);
}
