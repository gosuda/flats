// DOM helpers shared by the board app: element builder, icons, toasts,
// in-page dialogs and menus. Nothing here uses inline style attributes, so
// the app runs under the shell's CSP (style-src 'self').

export const $ = (sel, root = document) => root.querySelector(sel);

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'style') Object.assign(el.style, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return el;
}

const ICONS = {
  select: 'M5 3l12 8-5.5 1.5L9 18z',
  hand: 'M8 11V5.5a1.5 1.5 0 013 0V10m0-5.5a1.5 1.5 0 013 0V10m0-3.5a1.5 1.5 0 013 0V13c0 4-2.5 7-6.5 7-3 0-4.6-1.6-6-4l-2-3.4a1.5 1.5 0 012.6-1.5L8 13',
  sticky: 'M4 4h16v10l-6 6H4zM14 20v-6h6',
  text: 'M5 6V4h14v2M12 4v16M9 20h6',
  rect: 'M6 5h12a2 2 0 012 2v10a2 2 0 01-2 2H6a2 2 0 01-2-2V7a2 2 0 012-2z',
  ellipse: 'M12 5c4.4 0 8 3.1 8 7s-3.6 7-8 7-8-3.1-8-7 3.6-7 8-7z',
  diamond: 'M12 3l9 9-9 9-9-9z',
  connector: 'M5 19L19 5M12 5h7v7',
  elbow: 'M4 18h8V6h8M17 3l3 3-3 3',
  straight: 'M4 20L20 4M13 4h7v7',
  pen: 'M4 20l4-1 11-11-3-3L5 16zM14 6l3 3',
  marker: 'M6 20h12M8 16l8-12 3 2-8 12H8z',
  eraser: 'M7 20h12M5 15l9-9 5 5-8 8H8z',
  image: 'M5.5 4.5h13a2 2 0 012 2v11a2 2 0 01-2 2h-13a2 2 0 01-2-2v-11a2 2 0 012-2zM9 8.2a1.8 1.8 0 110 3.6 1.8 1.8 0 010-3.6zM20 16l-5-5-9 8.5',
  frame: 'M7 3v18M17 3v18M3 7h18M3 17h18',
  undo: 'M9 7H4V2M4 7a9 9 0 1 1-1 6',
  redo: 'M15 7h5V2M20 7a9 9 0 1 0 1 6',
  search: 'M11 4.5a6.5 6.5 0 110 13 6.5 6.5 0 010-13zM20 20l-4-4',
  history: 'M3 12a9 9 0 1 0 3-6.7M3 4v5h5M12 7.5V12l3 2',
  download: 'M12 4v11M7 10l5 5 5-5M5 20h14',
  upload: 'M12 16V5M7 10l5-5 5 5M5 20h14',
  send: 'M4 12l16-8-6 16-2.5-6.5zM11.5 13.5L20 4',
  minus: 'M5 12h14',
  plus: 'M12 5v14M5 12h14',
  fit: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  pin: 'M9 4h6l-1 6 4 3H6l4-3zM12 13v7',
  check: 'M5 12.5l4.5 4.5L19 7',
  lock: 'M7 11h10a2 2 0 012 2v5a2 2 0 01-2 2H7a2 2 0 01-2-2v-5a2 2 0 012-2zM8 11V8a4 4 0 018 0v3',
  unlock: 'M7 11h10a2 2 0 012 2v5a2 2 0 01-2 2H7a2 2 0 01-2-2v-5a2 2 0 012-2zM8 11V8a4 4 0 017.5-2',
  eye: 'M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12zM12 9.4a2.6 2.6 0 110 5.2 2.6 2.6 0 010-5.2z',
  copy: 'M10 8h8a2 2 0 012 2v8a2 2 0 01-2 2h-8a2 2 0 01-2-2v-8a2 2 0 012-2zM16 8V5a1 1 0 00-1-1H5a1 1 0 00-1 1v10a1 1 0 001 1h3',
  close: 'M6 6l12 12M18 6L6 18',
  board: 'M5 4h14a2 2 0 012 2v12a2 2 0 01-2 2H5a2 2 0 01-2-2V6a2 2 0 012-2zM7 9h4v4H7zM14 8h3M14 12h3M7 16h10',
  warn: 'M12 3l10 18H2zM12 10v5M12 18v.01',
  'wifi-off': 'M3 3l18 18M8.5 16.5a5 5 0 017 0M5 12.5a10 10 0 015-2.6M14 10a10 10 0 015 2.5M2 8.8a15 15 0 015-2.6M17 6.4a15 15 0 015 2.4M12 20h.01',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  list: 'M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01',
  up: 'M6 15l6-6 6 6',
  down: 'M6 9l6 6 6-6',
  'al-left': 'M4 3v18M8 7h10M8 12h6M8 17h12',
  'al-center': 'M12 3v18M6 7h12M8 12h8M5 17h14',
  'al-right': 'M20 3v18M6 7h10M10 12h6M4 17h12',
  'al-top': 'M3 4h18M7 8v10M12 8v6M17 8v12',
  'al-middle': 'M3 12h18M7 6v12M12 8v8M17 5v14',
  'al-bottom': 'M3 20h18M7 6v10M12 10v6M17 4v12',
  'dist-h': 'M4 4v16M20 4v16M10 8h4v8h-4z',
  'dist-v': 'M4 4h16M4 20h16M8 10h8v4H8z',
  group: 'M3 3h18v18H3zM7 7h6v6H7zM11 11h6v6h-6z',
  front: 'M10 9h9a1 1 0 011 1v9a1 1 0 01-1 1h-9a1 1 0 01-1-1v-9a1 1 0 011-1zM4 15V4h11',
  back: 'M5 4h9a1 1 0 011 1v9a1 1 0 01-1 1H5a1 1 0 01-1-1V5a1 1 0 011-1zM9 20h11V9',
  'arrow-end': 'M4 12h14M13 7l5 5-5 5',
  'arrow-start': 'M20 12H6M11 7l-5 5 5 5',
  dup: 'M10 8h8a2 2 0 012 2v8a2 2 0 01-2 2h-8a2 2 0 01-2-2v-8a2 2 0 012-2zM6 4h8a2 2 0 012 2v0M4 6v8a2 2 0 002 2',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13',
  go: 'M5 12h14M13 6l6 6-6 6',
  grid: 'M5 5h.01M12 5h.01M19 5h.01M5 12h.01M12 12h.01M19 12h.01M5 19h.01M12 19h.01M19 19h.01',
  label: 'M4 7h12l4 5-4 5H4z',
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

// replace swaps an element's children, skipping null and false the way h does.
export const replace = (el, ...kids) => el.replaceChildren(...kids.flat().filter((k) => k != null && k !== false));

export const btn = (cls, label, onclick, ...children) =>
  h('button', { type: 'button', class: cls, 'aria-label': label || null, title: label || null, onclick }, ...children);

// ---- toasts and live announcements ----

export const TOAST_PLAIN_MAX = 4, TOAST_KEEP_MAX = 8;

export function toast(content, { error = false, actions = [], timeout } = {}) {
  const host = $('.toasts');
  const close = () => t.remove();
  const t = h('div', { class: 'toast' + (error ? ' error' : ''), role: error ? 'alert' : 'status' },
    error ? icon('warn') : null,
    h('span', { class: 'toast-text' }, content),
    actions.map(([label, fn]) => h('button', { type: 'button', class: 'btn', onclick: () => { close(); fn(); } }, label)),
    h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Dismiss', onclick: close }, icon('close')));
  const ms = timeout ?? (actions.length ? 10000 : error ? 7000 : 4000);
  // A toast with an action ("Keep my text") or without a timeout may be the
  // only way to recover something, so it outlives plain toasts. Kept toasts
  // are still capped so repeated notices cannot pile up without bound; the
  // newest one always stays.
  if (actions.length || !ms) t.dataset.keep = '';
  // An identical notice without actions replaces its older copy. Toasts with
  // actions are never merged: each may hold different text to recover.
  if (typeof content === 'string' && !actions.length) {
    t.dataset.key = (error ? 'e:' : 'i:') + content;
    for (const c of [...host.children]) if (c.dataset.key === t.dataset.key) c.remove();
  }
  host.appendChild(t);
  const kids = [...host.children];
  const plain = kids.filter((c) => !('keep' in c.dataset));
  while (plain.length > TOAST_PLAIN_MAX) plain.shift().remove();
  const kept = kids.filter((c) => 'keep' in c.dataset);
  while (kept.length > TOAST_KEEP_MAX) kept.shift().remove();
  if (ms) setTimeout(close, ms);
  return t;
}

export function announce(text) {
  const live = $('#announce');
  live.textContent = '';
  requestAnimationFrame(() => { live.textContent = text; });
}

// ---- dialogs ----

// dialog opens a modal <dialog>. The background is inert while it is open,
// Escape or Cancel closes it, and focus returns to the opener.
export function dialog({ title, labelledBy, describe, body, actions, className = '', alert = false, onClose }) {
  const opener = document.activeElement;
  const id = 'dlg-' + Math.random().toString(36).slice(2, 8);
  const d = h('dialog', { class: 'pop dialog ' + className, 'aria-labelledby': labelledBy || id + '-t', role: alert ? 'alertdialog' : null });
  if (title) d.appendChild(h('h2', { id: id + '-t', text: title }));
  if (describe) d.appendChild(h('p', { class: 'note', text: describe }));
  for (const b of [].concat(body || [])) if (b) d.appendChild(b);
  if (actions) d.appendChild(actions);
  const close = (v) => { if (d.open) d.close(); return v; };
  d.addEventListener('close', () => {
    d.remove();
    if (onClose) onClose();
    if (opener && opener.isConnected && opener.focus) opener.focus();
  });
  document.body.appendChild(d);
  d.showModal();
  const first = d.querySelector('input:checked, input, textarea, select, button.primary, button');
  if (first) first.focus();
  return { el: d, close };
}

// confirmDialog asks inside the page (never native confirm()).
export function confirmDialog({ title, body, ok, danger = false, extra }) {
  return new Promise((resolve) => {
    let result = false;
    const acts = h('div', { class: 'acts' },
      extra || null, h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => dlg.close() }),
      h('button', { type: 'button', class: 'btn ' + (danger ? 'danger' : 'primary'), text: ok, onclick: () => { result = true; dlg.close(); } }));
    const dlg = dialog({ title, body, actions: acts, alert: danger, onClose: () => resolve(result) });
  });
}

// ---- menus and popovers ----

let openPop = null;

export function closePopover() {
  if (openPop) { const p = openPop; openPop = null; p.close(); }
}

// popover places el at (x, y) in viewport coordinates and closes it on an
// outside press or Escape. Menus get arrow-key navigation.
export function popover(el, x, y, { opener, menu = false } = {}) {
  closePopover();
  document.body.appendChild(el);
  const r = el.getBoundingClientRect();
  el.style.left = Math.max(8, Math.min(x, innerWidth - r.width - 8)) + 'px';
  el.style.top = Math.max(60, Math.min(y, innerHeight - r.height - 8)) + 'px';
  const items = () => [...el.querySelectorAll('[role^=menuitem]:not([aria-disabled=true])')];
  const onDown = (e) => { if (!el.contains(e.target) && e.target !== opener && !(opener && opener.contains(e.target))) closePopover(); };
  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closePopover(); if (opener) opener.focus(); return; }
    if (!menu || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
    e.preventDefault();
    const list = items();
    const i = list.indexOf(document.activeElement);
    const n = e.key === 'ArrowDown' ? (i + 1) % list.length : (i - 1 + list.length) % list.length;
    if (list[n]) list[n].focus();
  };
  // The outside-press listener starts after the press that opened the
  // popover; closing first cancels it, so it never outlives this popover.
  const arm = setTimeout(() => document.addEventListener('pointerdown', onDown, true));
  el.addEventListener('keydown', onKey);
  openPop = {
    el,
    close() {
      clearTimeout(arm);
      document.removeEventListener('pointerdown', onDown, true);
      el.remove();
      if (opener) opener.setAttribute('aria-expanded', 'false');
    },
  };
  if (opener) opener.setAttribute('aria-expanded', 'true');
  if (menu) { const f = items()[0]; if (f) f.focus(); }
  return openPop;
}

export function menu(items, label) {
  const ul = h('ul', { class: 'pop menu', role: 'menu', 'aria-label': label });
  for (const it of items) {
    if (it === '-') { ul.appendChild(h('li', { class: 'msep', role: 'separator' })); continue; }
    if (it.note) { ul.appendChild(h('li', { role: 'none' }, h('p', { class: 'note menu-note', text: it.note }))); continue; }
    const b = h('button', {
      type: 'button', role: it.checked != null ? 'menuitemcheckbox' : 'menuitem', 'aria-checked': it.checked != null ? String(it.checked) : null,
      'aria-disabled': it.disabled ? 'true' : null, class: (it.danger ? 'danger' : '') + (it.current ? ' is-current' : ''),
      onclick: () => { if (it.disabled) return; closePopover(); it.run(); },
    }, it.icon ? icon(it.icon) : null, h('span', { text: it.label }), it.kbd ? h('span', { class: 'kbd', text: it.kbd }) : null,
    it.checked ? icon('check') : null);
    ul.appendChild(h('li', { role: 'none' }, b));
  }
  return ul;
}

// ---- formatting ----

export function plural(n, one, many) { return `${n.toLocaleString('en-US')} ${n === 1 ? one : many || one + 's'}`; }

export function relTime(sec) {
  const d = Date.now() / 1000 - sec;
  if (d < 60) return 'just now';
  if (d < 3600) return `${Math.floor(d / 60)} min ago`;
  const t = new Date(sec * 1000);
  const now = new Date();
  const hm = t.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false });
  if (t.toDateString() === now.toDateString()) return hm;
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (t.toDateString() === y.toDateString()) return 'yesterday ' + hm;
  return t.toLocaleDateString('en-US', { month: 'short', day: 'numeric' }) + ' ' + hm;
}

export function download(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = h('a', { href: url, download: name, class: 'sr' });
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
