// Flats design canvas app. Plain ES module, no dependencies.
// It shows a design's artboards on a pan/zoom canvas and, on a Private
// route, edits layout, notes, text and tweaks through /_design/api/ops.

import { createWheelPan } from './wheel.js';

const $ = (sel, root = document) => root.querySelector(sel);

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return el;
}

const ICONS = {
  minus: 'M5 12h14', plus: 'M12 5v14M5 12h14', fit: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  edit: 'M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4', history: 'M12 8v4l3 2M3 12a9 9 0 1 0 3-6.7M3 4v4h4',
  more: 'M5 12h.01M12 12h.01M19 12h.01', note: 'M5 4h14v11l-5 5H5zM14 20v-5h5', close: 'M6 6l12 12M18 6L6 18',
  open: 'M14 4h6v6M20 4l-8 8M10 6H5a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-5',
  prev: 'M15 18l-6-6 6-6', next: 'M9 18l6-6-6-6', pin: 'M9 4h6l-1 6 4 4H6l4-4zM12 14v6',
};
function icon(name, label) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('width', '18'); svg.setAttribute('height', '18');
  svg.setAttribute('fill', 'none'); svg.setAttribute('stroke', 'currentColor'); svg.setAttribute('stroke-width', '1.8');
  svg.setAttribute('stroke-linecap', 'round'); svg.setAttribute('stroke-linejoin', 'round');
  if (label) { svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', label); } else svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  p.setAttribute('d', ICONS[name]); svg.appendChild(p);
  return svg;
}
const iconBtn = (name, label, onclick, extra = {}) => h('button', Object.assign({ type: 'button', class: 'icon-btn', 'aria-label': label, title: label, onclick }, extra), icon(name));

// ---- state ----
const editor = (() => {
  try {
    let id = sessionStorage.getItem('flats-design-editor');
    if (!id) { id = crypto.getRandomValues(new Uint32Array(2)).join('-'); sessionStorage.setItem('flats-design-editor', id); }
    return id;
  } catch { return 'e' + Math.random().toString(36).slice(2); }
})();

const S = {
  state: null,           // last /_design/api/state
  view: { x: 40, y: 40, z: 1 },
  editing: false,
  selected: null,        // board path
  focus: null,           // { path }
  revision: null,        // viewing revision number (read-only), or null for live
  revState: null,        // canvas of the viewed revision
  panel: null,           // 'tweaks' | 'revisions' | null
  frames: new Map(),     // path -> { el, iframe, hash, loaded }
  cap: null,             // head capability for artboard frames
  revCap: null,          // capability of the viewed revision
  textEdit: null,        // open text editor
};
const priv = () => S.state && S.state.access === 'private';
const canEdit = () => priv() && S.revision == null;

// ---- api ----
async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: {} };
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.headers['X-Flats-Design'] = '1';
    init.body = JSON.stringify(opts.body);
  }
  const r = await fetch(path, init);
  let data = null;
  try { data = await r.json(); } catch {}
  if (!r.ok) {
    const err = new Error((data && (data.error || data.message)) || r.statusText);
    err.status = r.status; err.code = data && data.code;
    throw err;
  }
  return data;
}

async function op(o) {
  try {
    await api('/_design/api/ops', { method: 'POST', body: { editor, op: o } });
    return true;
  } catch (err) {
    if (err.status === 409) { toast('Someone changed this part meanwhile. Showing the current design.', true); await refresh(); }
    else toast(err.message || 'Could not save the change', true);
    return false;
  }
}

// ---- toasts & dialogs ----
function toast(msg, error) {
  const t = h('div', { class: 'toast' + (error ? ' error' : ''), role: error ? 'alert' : 'status', text: msg });
  $('.toasts').appendChild(t);
  setTimeout(() => t.remove(), error ? 6000 : 3200);
}
function confirmDialog(title, body, okLabel, danger) {
  return new Promise((resolve) => {
    const d = h('dialog', {},
      h('h3', { text: title }), h('p', { text: body }),
      h('div', { class: 'acts' },
        h('button', { type: 'button', class: 'btn', text: 'Cancel', onclick: () => { d.close(); resolve(false); } }),
        h('button', { type: 'button', class: 'btn ' + (danger ? 'danger' : 'primary'), text: okLabel, onclick: () => { d.close(); resolve(true); } })));
    d.addEventListener('close', () => { d.remove(); resolve(false); });
    document.body.appendChild(d); d.showModal();
  });
}

// ---- url helpers ----
function fileURL(path, hash) {
  const enc = path.split('/').map(encodeURIComponent).join('/');
  const cap = S.revision != null ? S.revCap : S.cap;
  return `/_design/c/${cap}/${enc}${hash ? '?v=' + hash : ''}`;
}
function directURL(path) { return '/' + path.split('/').map(encodeURIComponent).join('/'); }
async function session(revision) {
  const r = await api('/_design/api/session', { method: 'POST', body: revision ? { revision } : {} });
  return r.cap;
}
// renewCapability replaces the head capability after a reconnect (the host
// or the design may have restarted, which drops tokens) and reloads frames
// that used the old one.
async function renewCapability() {
  try {
    const cap = await session();
    const revCap = S.revision != null ? await session(S.revision) : null;
    if (cap === S.cap && revCap === S.revCap) return;
    S.cap = cap;
    if (S.revision != null) S.revCap = revCap;
    for (const [path, f] of S.frames) if (f.loaded) loadFrame(f, path, f.hash);
    if (S.focus) openFocus(S.focus.path);
  } catch {}
}
function genOf(path) {
  const f = S.state && S.state.files.find((x) => x.path === path);
  return f ? f.gen || 0 : 0;
}
const canvas = () => (S.revision != null ? S.revState : S.state.canvas);
// hashOf identifies a file's served bytes: its hash and, for the live
// design, its generation (a rollback renews the generation of unchanged
// files, and the bridge must carry the current one).
function hashOf(path) {
  const files = S.revision != null ? S.revFiles : S.state.files;
  const f = files && files.find((x) => x.path === path);
  return f ? f.hash.slice(0, 12) + (f.gen ? 'g' + f.gen : '') : '';
}

// ---- toolbar ----
// visibilityBadge shows who can open the design, and that a public visitor
// can only look.
function visibilityBadge() {
  const pub = !priv() || S.state.visibility === 'public';
  return h('span', { class: 'badge' + (pub ? ' live' : ''), text: pub ? (priv() ? 'Public' : 'Public · view only') : 'Private' });
}
function renderBar() {
  const bar = $('.bar');
  bar.replaceChildren();
  const st = S.state;
  const c = canvas();
  bar.append(
    h('span', { class: 'title', text: (c && c.title) || st.slug }),
    h('span', { class: 'meta', text: S.revision != null ? `Revision ${S.revision}` : `Revision ${st.revision}` }),
    visibilityBadge(),
    h('span', { class: 'spacer' }),
    h('div', { class: 'zoom', role: 'group', 'aria-label': 'Zoom' },
      iconBtn('minus', 'Zoom out', () => zoomBy(1 / 1.25)),
      h('output', { 'aria-live': 'polite', text: Math.round(S.view.z * 100) + '%' }),
      iconBtn('plus', 'Zoom in', () => zoomBy(1.25))),
    iconBtn('fit', 'Fit to window (F)', fit),
  );
  if (canEdit()) {
    bar.append(
      h('button', { type: 'button', class: 'btn', 'aria-pressed': String(S.editing), title: 'Edit layout, text and tweaks (E)', onclick: toggleEdit }, icon('edit'), h('span', { class: 'hide-narrow', text: 'Edit' })),
    );
    if (S.editing) bar.append(h('button', { type: 'button', class: 'btn', onclick: addNote }, icon('note'), h('span', { class: 'hide-narrow', text: 'Note' })));
  }
  if (priv()) bar.append(h('button', { type: 'button', class: 'btn', 'aria-pressed': String(S.panel === 'revisions'), onclick: () => togglePanel('revisions') }, icon('history'), h('span', { class: 'hide-narrow', text: 'History' })));
  const menu = h('div', { class: 'menu' });
  const list = h('div', { class: 'menu-list', hidden: true },
    h('a', { href: '/_design/export.zip', download: true, text: 'Download HTML (.zip)' }),
    h('button', { type: 'button', text: 'Print or save as PDF', onclick: () => { list.hidden = true; printAll(); } }));
  menu.append(iconBtn('more', 'More', () => { list.hidden = !list.hidden; }, { 'aria-haspopup': 'true' }), list);
  document.addEventListener('click', (e) => { if (!menu.contains(e.target)) list.hidden = true; }, { once: true });
  bar.append(menu);
}

// ---- canvas transform ----
function applyView() {
  $('.world').style.transform = `translate(${S.view.x}px, ${S.view.y}px) scale(${S.view.z})`;
  const out = $('.zoom output');
  if (out) out.textContent = Math.round(S.view.z * 100) + '%';
  // keep titles readable: counter-scale strips
  // and keep each strip within its board's on-screen width so titles of
  // neighbouring boards never overlap at low zoom
  for (const f of S.frames.values()) {
    f.strip.style.transform = `scale(${1 / S.view.z})`;
    f.strip.style.maxWidth = (parseFloat(f.el.style.width) || 0) * S.view.z + 'px';
  }
  loadVisible();
}
function zoomAt(factor, cx, cy) {
  const z = Math.min(4, Math.max(0.05, S.view.z * factor));
  const k = z / S.view.z;
  S.view.x = cx - (cx - S.view.x) * k;
  S.view.y = cy - (cy - S.view.y) * k;
  S.view.z = z;
  applyView();
}
function zoomBy(f) { const vp = $('.viewport').getBoundingClientRect(); zoomAt(f, vp.width / 2, vp.height / 2); }
function bounds() {
  const c = canvas();
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  for (const b of Object.values(c.boards || {})) { x0 = Math.min(x0, b.x); y0 = Math.min(y0, b.y - 30); x1 = Math.max(x1, b.x + b.w); y1 = Math.max(y1, b.y + b.h); }
  for (const n of Object.values(c.notes || {})) { x0 = Math.min(x0, n.x); y0 = Math.min(y0, n.y); x1 = Math.max(x1, n.x + (n.w || 240)); y1 = Math.max(y1, n.y + 80); }
  return isFinite(x0) ? { x0, y0, x1, y1 } : { x0: 0, y0: 0, x1: 800, y1: 600 };
}
function fit() {
  const vp = $('.viewport').getBoundingClientRect();
  const b = bounds();
  const pad = 48;
  const z = Math.min(2, Math.max(0.05, Math.min((vp.width - pad * 2) / (b.x1 - b.x0), (vp.height - pad * 2) / (b.y1 - b.y0))));
  S.view = { z, x: (vp.width - (b.x1 - b.x0) * z) / 2 - b.x0 * z, y: (vp.height - (b.y1 - b.y0) * z) / 2 - b.y0 * z };
  applyView();
}

function setupViewport() {
  const vp = $('.viewport');
  const wheel = createWheelPan();
  let pan = null, space = false;
  vp.addEventListener('wheel', (e) => {
    e.preventDefault();
    const r = vp.getBoundingClientRect();
    if (e.ctrlKey || e.metaKey) { wheel.reset(); zoomAt(Math.exp(-e.deltaY * 0.01), e.clientX - r.left, e.clientY - r.top); }
    else { const p = wheel.pan(e); S.view.x -= p.x; S.view.y -= p.y; applyView(); }
  }, { passive: false });
  vp.addEventListener('pointerdown', (e) => {
    const onBackground = e.target === vp || e.target.classList.contains('world');
    if (!(onBackground || space || e.button === 1)) return;
    if (onBackground && !space) selectBoard(null);
    pan = { x: e.clientX, y: e.clientY, vx: S.view.x, vy: S.view.y };
    vp.setPointerCapture(e.pointerId); vp.classList.add('panning');
  });
  vp.addEventListener('pointermove', (e) => { if (!pan) return; S.view.x = pan.vx + e.clientX - pan.x; S.view.y = pan.vy + e.clientY - pan.y; applyView(); });
  const end = () => { pan = null; vp.classList.remove('panning'); };
  vp.addEventListener('pointerup', end); vp.addEventListener('pointercancel', end);
  window.addEventListener('keydown', (e) => {
    const ae = document.activeElement;
    const typing = !!ae && (/^(INPUT|TEXTAREA|SELECT)$/.test(ae.tagName) || ae.isContentEditable);
    if (e.code === 'Space' && !typing) { space = true; vp.classList.add('space'); }
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'f' || e.key === 'F') { if (!S.focus) fit(); }
    else if ((e.key === 'e' || e.key === 'E') && canEdit() && !S.focus) toggleEdit();
    else if (e.key === 'Escape') { if (S.focus) closeFocus(); else if (S.editing) toggleEdit(); }
    else if (e.key === '+' || e.key === '=') zoomBy(1.25);
    else if (e.key === '-') zoomBy(1 / 1.25);
  });
  // These resets are best-effort when focus is inside an artboard iframe.
  window.addEventListener('blur', wheel.reset);
  window.addEventListener('keyup', (e) => { if (e.key === 'Shift') wheel.reset(); if (e.code === 'Space') { space = false; vp.classList.remove('space'); } });
  window.addEventListener('resize', loadVisible);
}

// ---- boards ----
// assetSignature changes when any non-artboard file (shared CSS, scripts,
// images, data) changes, so frames that may use it reload.
function assetSignature() {
  const files = S.revision != null ? S.revFiles : S.state.files;
  // Artboards reload on their own hash; every other file (including HTML a
  // board embeds) may be used by any of them.
  const boards = (canvas() && canvas().boards) || {};
  return (files || []).filter((f) => !boards[f.path] && f.path !== 'canvas.json').map((f) => f.path + ':' + f.hash).join('|');
}

function renderWorld() {
  const world = $('.world');
  const c = canvas();
  const keep = new Set();
  const sig = assetSignature();
  const assetsChanged = S.assetSig != null && S.assetSig !== sig;
  S.assetSig = sig;
  // Back-to-front order is z-index: a mounted board is never moved in the
  // DOM, which would restart its iframe and lose prototype state.
  (c.order || Object.keys(c.boards)).forEach((path, i) => {
    const b = c.boards[path];
    if (!b) return;
    keep.add(path);
    let f = S.frames.get(path);
    if (!f) { f = makeFrame(path); S.frames.set(path, f); world.appendChild(f.el); }
    f.el.style.zIndex = String(i + 1);
    f.el.style.left = b.x + 'px'; f.el.style.top = b.y + 'px';
    f.el.style.width = b.w + 'px'; f.el.style.height = b.h + 'px';
    f.name.textContent = b.title || path.replace(/\.html?$/i, '');
    f.el.classList.toggle('selected', S.selected === path);
    const hash = hashOf(path);
    if (f.loaded && (f.hash !== hash || assetsChanged)) loadFrame(f, path, hash);
    f.hash = hash;
  });
  for (const [path, f] of S.frames) if (!keep.has(path)) { f.el.remove(); S.frames.delete(path); }
  // Keep a note that is being edited; rebuild the others.
  const editingNote = S.noteEditing && world.querySelector(`.note[data-id="${CSS.escape(S.noteEditing)}"]`);
  world.querySelectorAll('.note').forEach((n) => { if (n !== editingNote) n.remove(); });
  for (const [id, n] of Object.entries(c.notes || {})) if (!editingNote || id !== S.noteEditing) world.appendChild(makeNote(id, n));
  applyView();
}

function makeFrame(path) {
  const el = h('div', { class: 'board', 'data-path': path });
  const name = h('span', { class: 'name' });
  const strip = h('div', { class: 'strip' }, name,
    h('button', { type: 'button', title: 'Open full window', onclick: (e) => { e.stopPropagation(); openFocus(path); } }, 'Open'));
  const frame = h('div', { class: 'frame' }, h('div', { class: 'placeholder', text: 'Loading…' }));
  const shield = h('div', { class: 'shield', ondblclick: () => openFocus(path), onclick: () => selectBoard(path) });
  const handle = h('div', { class: 'handle', title: 'Resize' });
  el.append(strip, frame, shield, handle);
  const f = { el, strip, name, frame, iframe: null, hash: '', loaded: false };
  // move by the strip, resize by the handle (edit mode)
  dragger(strip, path, 'move');
  dragger(handle, path, 'resize');
  el.addEventListener('pointerdown', () => selectBoard(path));
  name.addEventListener('dblclick', () => { if (S.editing) renameBoard(path); });
  return f;
}

// Artboard isolation comes from the host: every design file response carries
// a CSP sandbox, so frames run at an opaque origin. The iframe sandbox
// attribute is left off because some embedded browsers refuse sandboxed
// frames outright.
function loadFrame(f, path, hash) {
  f.loaded = true;
  f.error && f.error.remove(); f.error = null;
  const iframe = h('iframe', {
    title: `Artboard ${path}`, loading: 'lazy', referrerpolicy: 'no-referrer',
   
    src: fileURL(path, hash),
  });
  f.iframe = iframe;
  f.frame.replaceChildren(iframe);
}

function loadVisible() {
  const vp = $('.viewport').getBoundingClientRect();
  const margin = 400;
  for (const [path, f] of S.frames) {
    if (f.loaded) continue;
    const r = f.el.getBoundingClientRect();
    if (r.right > vp.left - margin && r.left < vp.right + margin && r.bottom > vp.top - margin && r.top < vp.bottom + margin) loadFrame(f, path, f.hash);
  }
}

function selectBoard(path) {
  if (S.selected === path) return;
  S.selected = path;
  for (const [p, f] of S.frames) f.el.classList.toggle('selected', p === path);
  if (S.panel === 'tweaks' || (S.editing && path)) showTweaks();
}

function dragger(handleEl, path, kind) {
  let start = null;
  handleEl.addEventListener('pointerdown', (e) => {
    if (!S.editing || e.button !== 0 || e.target.tagName === 'BUTTON') return;
    e.preventDefault(); e.stopPropagation();
    const b = canvas().boards[path];
    start = { x: e.clientX, y: e.clientY, b: { ...b }, gen: genOf('canvas.json') };
    handleEl.setPointerCapture(e.pointerId);
    selectBoard(path);
  });
  handleEl.addEventListener('pointermove', (e) => {
    if (!start) return;
    const dx = (e.clientX - start.x) / S.view.z, dy = (e.clientY - start.y) / S.view.z;
    const f = S.frames.get(path);
    if (kind === 'move') { f.el.style.left = Math.round(start.b.x + dx) + 'px'; f.el.style.top = Math.round(start.b.y + dy) + 'px'; }
    else { f.el.style.width = clamp(Math.round(start.b.w + dx), 40, 8000) + 'px'; f.el.style.height = clamp(Math.round(start.b.h + dy), 40, 8000) + 'px'; f.strip.style.maxWidth = parseFloat(f.el.style.width) * S.view.z + 'px'; }
  });
  handleEl.addEventListener('pointerup', async (e) => {
    if (!start) return;
    const s = start; start = null;
    const dx = (e.clientX - s.x) / S.view.z, dy = (e.clientY - s.y) / S.view.z;
    if (Math.abs(dx) < 1 && Math.abs(dy) < 1) return;
    const fields = kind === 'move' ? { x: Math.round(s.b.x + dx), y: Math.round(s.b.y + dy) } : { w: clamp(Math.round(s.b.w + dx), 40, 8000), h: clamp(Math.round(s.b.h + dy), 40, 8000) };
    const base = kind === 'move' ? { x: s.b.x, y: s.b.y } : { w: s.b.w, h: s.b.h };
    if (!(await op({ op: 'board.set', gen: s.gen, path, fields, base }))) renderWorld();
  });
}
const clamp = (v, a, b) => Math.min(b, Math.max(a, v));

async function renameBoard(path) {
  const b = canvas().boards[path];
  const title = prompt('Artboard title', b.title || '');
  if (title == null || title === (b.title || '')) return;
  await op({ op: 'board.set', gen: genOf('canvas.json'), path, fields: { title }, base: { title: b.title || '' } });
}

// ---- notes ----
function makeNote(id, n) {
  const el = h('div', { class: 'note', 'data-id': id, 'data-color': n.color || 'yellow', tabindex: '0', style: { left: n.x + 'px', top: n.y + 'px', width: (n.w || 240) + 'px' } });
  const textEl = h('div', { text: n.text });
  el.append(textEl);
  if (canEdit()) {
    el.append(h('div', { class: 'note-tools' },
      h('button', { type: 'button', text: 'Edit', onclick: (e) => { e.stopPropagation(); editNote(el, textEl, id, n); } }),
      h('button', { type: 'button', text: 'Delete', onclick: async (e) => { e.stopPropagation(); if (await confirmDialog('Delete this note?', n.text.slice(0, 120), 'Delete', true)) op({ op: 'note.delete', gen: genOf('canvas.json'), id, base: { x: n.x, y: n.y, w: n.w || 0, text: n.text, color: n.color || '' } }); } })));
    let start = null;
    el.addEventListener('pointerdown', (e) => {
      if (!S.editing || e.target.closest('button,textarea')) return;
      e.stopPropagation(); start = { x: e.clientX, y: e.clientY, gen: genOf('canvas.json') }; el.setPointerCapture(e.pointerId);
    });
    el.addEventListener('pointermove', (e) => { if (!start) return; el.style.left = Math.round(n.x + (e.clientX - start.x) / S.view.z) + 'px'; el.style.top = Math.round(n.y + (e.clientY - start.y) / S.view.z) + 'px'; });
    el.addEventListener('pointerup', (e) => {
      if (!start) return;
      const dx = (e.clientX - start.x) / S.view.z, dy = (e.clientY - start.y) / S.view.z; const gen = start.gen; start = null;
      if (Math.abs(dx) < 1 && Math.abs(dy) < 1) return;
      op({ op: 'note.set', gen, id, fields: { x: Math.round(n.x + dx), y: Math.round(n.y + dy) }, base: { x: n.x, y: n.y } });
    });
    el.addEventListener('dblclick', () => { if (S.editing) editNote(el, textEl, id, n); });
  }
  return el;
}
function editNote(el, textEl, id, n) {
  const ta = h('textarea', { 'aria-label': 'Note text' });
  ta.value = n.text;
  textEl.replaceWith(ta); ta.focus();
  S.noteEditing = id;
  const gen = genOf('canvas.json');
  let finished = false;
  const done = async (save) => {
    // Replacing the textarea blurs it; the flag keeps that blur from saving
    // a cancelled edit or submitting twice.
    if (finished) return;
    finished = true;
    if (S.noteEditing === id) S.noteEditing = null;
    ta.replaceWith(textEl);
    if (save && ta.value !== n.text && ta.value.trim()) await op({ op: 'note.set', gen, id, fields: { text: ta.value }, base: { text: n.text } });
    else renderWorld();
  };
  ta.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); done(false); } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) done(true); });
  ta.addEventListener('blur', () => done(true), { once: true });
}
async function addNote() {
  const vp = $('.viewport').getBoundingClientRect();
  const x = Math.round((vp.width / 2 - S.view.x) / S.view.z - 120), y = Math.round((vp.height / 2 - S.view.y) / S.view.z - 40);
  await op({ op: 'note.add', gen: genOf('canvas.json'), fields: { x, y, w: 240, text: 'New note', color: 'yellow' } });
}

// ---- edit mode ----
function toggleEdit() {
  S.pickTicket = (S.pickTicket || 0) + 1;
  closeTextEditor();
  S.editing = !S.editing;
  document.body.classList.toggle('editing', S.editing);
  for (const f of S.frames.values()) f.iframe && f.iframe.contentWindow && f.iframe.contentWindow.postMessage({ flats: 'design', type: 'mode', mode: S.editing ? 'edit' : 'view' }, '*');
  if (!S.editing && S.panel === 'tweaks') closePanel();
  if (S.editing && S.selected) showTweaks();
  renderBar(); renderWorld();
}

// ---- bridge messages ----
function frameFor(win) {
  for (const [path, f] of S.frames) if (f.iframe && f.iframe.contentWindow === win) return { path, f };
  const fi = $('.focus iframe');
  if (fi && fi.contentWindow === win) return { path: S.focus.path, f: null, focus: true };
  return null;
}
window.addEventListener('message', async (e) => {
  const m = e.data;
  if (!m || m.flats !== 'design') return;
  const hit = frameFor(e.source);
  if (!hit) return;
  if (m.type === 'ready') {
    e.source.postMessage({ flats: 'design', type: 'boards', paths: Object.keys(canvas().boards || {}) }, '*');
    if (hit.f && S.editing) e.source.postMessage({ flats: 'design', type: 'mode', mode: 'edit' }, '*');
    if (hit.focus) e.source.postMessage({ flats: 'design', type: 'mode', mode: 'play' }, '*');
  } else if (m.type === 'error' && hit.f) {
    if (!hit.f.error) { hit.f.error = h('div', { class: 'error', role: 'status' }); hit.f.el.appendChild(hit.f.error); }
    hit.f.error.textContent = 'Error: ' + String(m.message).slice(0, 200);
  } else if (m.type === 'navigate') {
    const target = resolvePath(hit.path, String(m.href || ''));
    if (target && canvas().boards[target]) openFocus(target);
  } else if (m.type === 'pick' && hit.f && canEdit() && S.editing) {
    selectBoard(hit.path);
    if (Number.isInteger(m.node) && m.node >= 0 && m.rect) openTextEditor(hit.path, hit.f, m);
    else closeTextEditor();
  }
});

// ---- text editor (owned by the canvas, never by the artboard) ----
function cleanInline(htmlText) {
  const allowed = new Set(['B', 'STRONG', 'I', 'EM', 'U', 'S', 'SMALL', 'SUB', 'SUP', 'CODE', 'MARK', 'SPAN', 'BR']);
  const doc = new DOMParser().parseFromString('<div>' + htmlText + '</div>', 'text/html');
  const walk = (node) => {
    for (const c of [...node.childNodes]) {
      if (c.nodeType === 1) {
        if (!allowed.has(c.tagName)) { walk(c); c.replaceWith(...c.childNodes); continue; }
        for (const a of [...c.attributes]) if (a.name !== 'class' && a.name !== 'style') c.removeAttribute(a.name);
        walk(c);
      } else if (c.nodeType !== 3) c.remove();
    }
  };
  const root = doc.body.firstChild;
  walk(root);
  return root.innerHTML;
}
function closeTextEditor() {
  const t = S.textEdit;
  if (!t) return;
  S.textEdit = null;
  t.box.remove();
  t.win && t.win.postMessage({ flats: 'design', type: 'outline', node: -1 }, '*');
}
async function openTextEditor(path, f, m) {
  closeTextEditor();
  // Only the latest pick may open an editor: a slower earlier request, or
  // one answered after edit mode ended, is dropped.
  const ticket = (S.pickTicket = (S.pickTicket || 0) + 1);
  let node;
  try { node = await api(`/_design/api/node?path=${encodeURIComponent(path)}&node=${m.node}&gen=${Number(m.gen) || 0}`); }
  catch (err) {
    toast(err.status === 409 ? 'This artboard changed. Showing the current version; click the text again.' : err.message, true);
    if (err.status === 409) { await refresh().catch(() => {}); loadFrame(f, path, hashOf(path)); }
    return;
  }
  if (ticket !== S.pickTicket || !S.editing || !canEdit() || !f.iframe) return;
  closeTextEditor();
  const ir = f.iframe.getBoundingClientRect();
  const scale = ir.width / (f.iframe.offsetWidth || ir.width) || 1;
  const st = m.style || {};
  const field = h('div', { class: 'text-editor-field', contenteditable: 'true', role: 'textbox', 'aria-multiline': 'true', 'aria-label': 'Edit text' });
  field.innerHTML = cleanInline(node.inner);
  Object.assign(field.style, { font: st.font || '', color: st.color || '', textAlign: st.align || '', lineHeight: st.lineHeight || '', letterSpacing: st.letterSpacing || '', padding: st.padding || '', background: st.background && st.background !== 'rgba(0, 0, 0, 0)' ? st.background : '#fff' });
  const done = h('button', { type: 'button', class: 'btn primary', text: 'Done' });
  const cancel = h('button', { type: 'button', class: 'btn', text: 'Cancel' });
  // The field is drawn at the artboard's scale over the text; the action bar
  // stays at normal size below it.
  const left = ir.left + m.rect.x * scale, top = ir.top + m.rect.y * scale;
  const fieldWrap = h('div', { class: 'text-editor-scale', style: { width: Math.max(80, m.rect.w) + 'px', transform: `scale(${scale})` } }, field);
  const acts = h('div', { class: 'text-editor-acts' }, h('span', { text: 'Enter to save · Shift+Enter new line · Esc to cancel' }), cancel, done);
  const box = h('div', { class: 'text-editor', style: { left: left + 'px', top: top + 'px' } }, fieldWrap, acts);
  requestAnimationFrame(() => { acts.style.marginTop = (fieldWrap.getBoundingClientRect().height - fieldWrap.offsetHeight) + 'px'; });
  document.body.appendChild(box);
  f.iframe.contentWindow && f.iframe.contentWindow.postMessage({ flats: 'design', type: 'outline', node: m.node }, '*');
  S.textEdit = { box, win: f.iframe.contentWindow };
  const before = field.innerHTML;
  const commit = async () => {
    if (S.textEdit?.box !== box) return;
    const htmlText = field.innerHTML;
    closeTextEditor();
    if (htmlText === before) return;
    if (!field.textContent.trim()) { toast('Text cannot be empty. Ask an agent to remove the element instead.', true); return; }
    await op({ op: 'text.set', path, gen: node.gen, node: m.node, tag: node.tag, inner_hash: node.inner_hash, html: htmlText });
  };
  done.addEventListener('click', commit);
  cancel.addEventListener('click', closeTextEditor);
  field.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeTextEditor(); }
    else if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); commit(); }
    else if (e.key === 'Enter' && e.shiftKey) { e.preventDefault(); document.execCommand('insertLineBreak'); }
  });
  field.addEventListener('paste', (e) => { e.preventDefault(); document.execCommand('insertText', false, (e.clipboardData || window.clipboardData).getData('text/plain')); });
  setTimeout(() => { field.focus(); const r = document.createRange(); r.selectNodeContents(field); const sel = getSelection(); sel.removeAllRanges(); sel.addRange(r); }, 0);
}
function resolvePath(from, href) {
  const base = from.includes('/') ? from.slice(0, from.lastIndexOf('/') + 1) : '';
  const parts = (href.startsWith('/') ? href.slice(1) : base + href).split('/');
  const out = [];
  for (const p of parts) { if (p === '..') out.pop(); else if (p && p !== '.') out.push(decodeURIComponent(p)); }
  return out.join('/');
}

// ---- focus / play ----
function openFocus(path) {
  closeFocus();
  const c = canvas();
  const b = c.boards[path];
  if (!b) return;
  S.focus = { path, key: hashOf(path) + '|' + assetSignature() };
  const order = c.order || Object.keys(c.boards);
  const i = order.indexOf(path);
  const iframe = h('iframe', { title: `Artboard ${path}`, src: fileURL(path, hashOf(path)) });
  if (!b.fill) { iframe.style.width = b.w + 'px'; iframe.style.height = b.h + 'px'; }
  const box = h('div', { class: 'focus', role: 'dialog', 'aria-label': `Artboard ${b.title || path}` },
    h('div', { class: 'bar' },
      iconBtn('prev', 'Previous artboard', () => order[i - 1] && openFocus(order[i - 1]), { disabled: i <= 0 }),
      iconBtn('next', 'Next artboard', () => order[i + 1] && openFocus(order[i + 1]), { disabled: i >= order.length - 1 }),
      h('span', { class: 'title', text: b.title || path }),
      h('span', { class: 'meta', text: `${i + 1} of ${order.length} · links play the prototype` }),
      h('span', { class: 'spacer' }),
      h('a', { class: 'btn', href: S.revision != null ? fileURL(path, hashOf(path)) : directURL(path), target: '_blank', rel: 'noopener' }, icon('open'), h('span', { class: 'hide-narrow', text: 'New tab' })),
      iconBtn('close', 'Close (Esc)', closeFocus)),
    h('div', { class: 'stage' + (b.fill ? ' fill' : '') }, iframe));
  document.body.appendChild(box);
  history.replaceState(null, '', '#' + encodeURIComponent(path));
}
// sendBoards tells every artboard frame which paths are artboards, so their
// bridges take over only links to them.
function sendBoards() {
  const paths = Object.keys(canvas().boards || {});
  const wins = [...S.frames.values()].map((f) => f.iframe && f.iframe.contentWindow);
  const fi = $('.focus iframe');
  if (fi) wins.push(fi.contentWindow);
  for (const w of wins) if (w) w.postMessage({ flats: 'design', type: 'boards', paths }, '*');
}

// refreshFocus reloads the full-window artboard when its file or the shared
// assets changed, and closes it when its board is gone.
function refreshFocus() {
  if (!S.focus || S.revision != null) return;
  const path = S.focus.path;
  if (!canvas().boards[path]) { closeFocus(); return; }
  const key = hashOf(path) + '|' + assetSignature();
  if (S.focus.key && S.focus.key !== key) openFocus(path);
  else S.focus.key = key;
}

function closeFocus() {
  const box = $('.focus');
  if (box) box.remove();
  if (S.focus) history.replaceState(null, '', location.pathname);
  S.focus = null;
}

// ---- panels ----
function togglePanel(name) { if (S.panel === name) closePanel(); else if (name === 'revisions') showRevisions(); else showTweaks(); }
function closePanel() { const p = $('.panel'); if (p) p.remove(); S.panel = null; renderBar(); }
function panel(title, ...body) {
  const old = $('.panel'); if (old) old.remove();
  const p = h('aside', { class: 'panel', 'aria-label': title },
    h('header', {}, h('h2', { text: title }), iconBtn('close', 'Close panel', closePanel)),
    h('div', { class: 'body' }, ...body));
  document.body.appendChild(p);
  return p;
}

function showTweaks() {
  if (!S.editing || !canEdit()) return;
  S.panel = 'tweaks';
  const path = S.selected;
  const tw = path && S.state.artboards && S.state.artboards[path] && S.state.artboards[path].tweaks;
  if (!path) { panel('Tweaks', h('p', { class: 'empty', text: 'Select an artboard to adjust its tweaks. Click text inside an artboard to retype it.' })); renderBar(); return; }
  const names = tw ? Object.keys(tw).sort() : [];
  if (!names.length) { panel('Tweaks · ' + path, h('p', { class: 'empty', text: 'This artboard declares no tweaks. Click text inside it to retype it.' })); renderBar(); return; }
  const f = S.frames.get(path);
  const preview = (name, css) => f && f.iframe && f.iframe.contentWindow && f.iframe.contentWindow.postMessage({ flats: 'design', type: 'tweak', name, css }, '*');
  const fields = names.map((name) => tweakField(path, name, tw[name], preview));
  panel('Tweaks · ' + ((canvas().boards[path] || {}).title || path), ...fields);
  renderBar();
}

function tweakField(path, name, t, preview) {
  const id = 'tw-' + name;
  const label = h('label', { for: id, text: t.label || name });
  // A refused save must not leave the live preview applied: reload the
  // frame (dropping the preview) and redraw the panel from saved values.
  const save = async (value) => {
    if (await op({ op: 'tweak.set', gen: genOf(path), path, name, value, base_value: t.value })) return;
    const f = S.frames.get(path);
    if (f && f.loaded) loadFrame(f, path, f.hash);
    showTweaks();
  };
  let control;
  if (t.type === 'color') {
    // The picker edits RGB only; an 8-digit value keeps its alpha.
    const full = /^#[0-9a-f]{3}$/i.test(t.value) ? '#' + t.value.slice(1).split('').map((c) => c + c).join('') : String(t.value);
    const v = /^#[0-9a-f]{6}([0-9a-f]{2})?$/i.test(full) ? full.slice(0, 7) : '#000000';
    const alpha = /^#[0-9a-f]{8}$/i.test(full) ? full.slice(7) : '';
    control = h('input', { id, type: 'color', value: v, oninput: (e) => preview(name, e.target.value + alpha), onchange: (e) => save(e.target.value + alpha) });
    control = h('div', { class: 'row' }, control, h('code', { text: t.value }));
  } else if (t.type === 'number') {
    const attrs = { id, type: 'number', value: t.value, step: t.step ?? 'any' };
    if (t.min != null) attrs.min = t.min; if (t.max != null) attrs.max = t.max;
    const num = h('input', Object.assign({}, attrs, { onchange: (e) => { const v = Number(e.target.value); if (Number.isFinite(v)) save(v); } }));
    const kids = [num];
    if (t.min != null && t.max != null) {
      kids.unshift(h('input', { type: 'range', 'aria-label': (t.label || name) + ' slider', min: t.min, max: t.max, step: t.step ?? 'any', value: t.value,
        oninput: (e) => { num.value = e.target.value; preview(name, e.target.value + (t.unit || '')); },
        onchange: (e) => save(Number(e.target.value)) }));
    }
    if (t.unit) kids.push(h('span', { text: t.unit }));
    control = h('div', { class: 'row' }, ...kids);
  } else if (t.type === 'select') {
    control = h('select', { id, onchange: (e) => save(e.target.value) }, ...(t.options || []).map((o) => h('option', { value: o, selected: o === t.value, text: o })));
  } else if (t.type === 'boolean') {
    control = h('div', { class: 'row' }, h('input', { id, type: 'checkbox', checked: !!t.value, onchange: (e) => save(e.target.checked) }), h('span', { text: t.value ? 'On' : 'Off' }));
  } else {
    control = h('input', { id, type: 'text', value: t.value, maxlength: '200', onchange: (e) => save(e.target.value) });
  }
  return h('div', { class: 'field' }, label, control);
}

async function showRevisions() {
  S.panel = 'revisions';
  const p = panel('History', h('p', { class: 'empty', text: 'Loading…' }));
  renderBar();
  let revs;
  try { revs = await api('/_design/api/revisions'); } catch (err) { p.querySelector('.body').replaceChildren(h('p', { class: 'empty', text: err.message })); return; }
  const items = (revs.revisions || []).map((r) => revItem(r, revs));
  if (revs.next) {
    let next = revs.next;
    const more = h('button', { type: 'button', class: 'btn', text: 'Load older revisions' });
    more.addEventListener('click', async () => {
      more.disabled = true;
      try {
        const page = await api(`/_design/api/revisions?before=${next}`);
        for (const r of page.revisions || []) more.before(revItem(r, page));
        next = page.next;
        if (!next) more.remove();
      } catch (err) { toast(err.message, true); }
      more.disabled = false;
    });
    items.push(more);
  }
  p.querySelector('.body').replaceChildren(...[
    h('p', { class: 'empty', text: `${revs.pins} of ${revs.pin_quota || '∞'} pins used · keeps ${revs.revision_quota || 'all'} unpinned revisions` }),
    S.state.pending ? h('div', { class: 'rev pending' }, h('div', { class: 'head' }, h('span', { class: 'num', text: 'Unsaved edits' })),
      h('div', { class: 'paths', text: 'Recent edits in the canvas become a revision after 30 seconds without changes (or 5 minutes), or before an agent saves or anyone restores.' })) : null,
    ...items].filter(Boolean));
}
function ago(ts) {
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  return new Date(ts).toLocaleString();
}
function revItem(r, revs) {
  const who = { agent: 'Agent', person: 'Person', rollback: 'Rollback', created: 'Created' }[r.kind] || r.kind;
  const authors = (r.authors || []).filter((a) => a && a !== who).join(', ');
  const current = r.number === S.state.revision && !S.state.pending;
  const pinBtn = h('button', { type: 'button', class: 'btn pin', 'aria-pressed': String(!!r.pinned), title: r.pinned ? 'Unpin: allow automatic deletion' : 'Pin: never delete automatically',
    onclick: async () => {
      try { await api(`/_design/api/revisions/${r.number}/pin`, { method: 'POST', body: { pinned: !r.pinned } }); showRevisions(); }
      catch (err) { toast(err.message, true); }
    } }, icon('pin'), r.pinned ? 'Pinned' : 'Pin');
  const paths = h('div', { class: 'paths' });
  const acts = h('div', { class: 'acts' }, pinBtn,
    h('button', { type: 'button', class: 'btn', text: 'Changes', onclick: async () => {
      try {
        const d = await api(`/_design/api/revisions/${r.number}`);
        const ch = (d.revision && d.revision.changes) || {};
        const parts = [['Added', ch.added], ['Changed', ch.changed], ['Removed', ch.removed]].filter(([, v]) => v && v.length).map(([k, v]) => `${k}: ${v.join(', ')}`);
        paths.textContent = parts.join(' · ') || 'No file changes';
      } catch (err) { toast(err.message, true); }
    } }));
  if (!current) {
    acts.append(
      h('button', { type: 'button', class: 'btn', text: 'View', onclick: () => viewRevision(r.number) }),
      h('button', { type: 'button', class: 'btn', text: 'Restore', onclick: () => rollback(r.number) }));
  }
  return h('div', { class: 'rev' + (current ? ' current' : '') },
    h('div', { class: 'head' }, h('span', { class: 'num', text: '#' + r.number }), h('span', { class: 'who', text: `${who}${authors ? ' · ' + authors : ''} · ${ago(r.created_at)}` })),
    r.message ? h('div', { class: 'msg', text: r.message }) : null,
    r.rollback_of && !r.message ? h('div', { class: 'paths', text: `Restored #${r.rollback_of}` }) : null,
    paths, acts);
}
async function rollback(n) {
  if (!(await confirmDialog(`Restore revision ${n}?`, 'The design returns to exactly that revision. This is saved as a new revision, so you can undo it the same way.', 'Restore'))) return;
  try {
    await api(`/_design/api/revisions/${n}/rollback`, { method: 'POST', body: {} });
    toast(`Restored revision ${n}.`);
    leaveRevision();
    await refresh();
    if (S.panel === 'revisions') showRevisions();
  } catch (err) { toast(err.message, true); }
}
async function viewRevision(n) {
  try {
    const d = await api(`/_design/api/revisions/${n}`);
    const cap = await session(n);
    const raw = await (await fetch(`/_design/c/${cap}/canvas.json`)).json();
    // Normalize as the server does for the live canvas: boards missing from
    // order are appended in path order.
    raw.boards = raw.boards || {}; raw.notes = raw.notes || {};
    const order = (raw.order || []).filter((k, i, a) => raw.boards[k] && a.indexOf(k) === i);
    for (const k of Object.keys(raw.boards).sort()) if (!order.includes(k)) order.push(k);
    raw.order = order;
    S.revision = n; S.revCap = cap; S.revState = raw; S.revFiles = d.files || [];
    if (S.editing) toggleEdit();
    for (const f of S.frames.values()) f.el.remove();
    S.frames.clear();
    showBanner(n);
    renderBar(); renderWorld();
  } catch (err) { toast('Could not open that revision: ' + err.message, true); }
}
function leaveRevision() {
  if (S.revision == null) return;
  S.revision = null; S.revCap = null; S.revState = null; S.revFiles = null;
  const b = $('.banner'); if (b) b.remove();
  for (const f of S.frames.values()) f.el.remove();
  S.frames.clear();
  renderBar(); renderWorld();
}
function showBanner(n) {
  const old = $('.banner'); if (old) old.remove();
  document.body.appendChild(h('div', { class: 'banner', role: 'status' },
    h('span', { text: `Viewing revision ${n} (read-only)` }),
    h('button', { type: 'button', class: 'btn', text: 'Back to current', onclick: leaveRevision }),
    h('button', { type: 'button', class: 'btn', text: 'Restore this', onclick: () => rollback(n) })));
}

// ---- print ----
function printAll() {
  const c = canvas();
  const root = $('.print-root');
  const order = c.order || Object.keys(c.boards);
  const css = order.map((p, i) => `@page b${i}{size:${c.boards[p].w}px ${c.boards[p].h}px;margin:0}.print-page.b${i}{page:b${i};width:${c.boards[p].w}px;height:${c.boards[p].h}px}`).join('');
  root.replaceChildren();
  // The shell's CSP allows no inline <style>; a constructed stylesheet is
  // CSSOM and needs no exception.
  try {
    if (S.printSheet) document.adoptedStyleSheets = document.adoptedStyleSheets.filter((x) => x !== S.printSheet);
    S.printSheet = new CSSStyleSheet();
    S.printSheet.replaceSync(css);
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, S.printSheet];
  } catch { /* old browsers print at the default page size */ }
  let pending = order.length;
  const done = () => { if (--pending <= 0) setTimeout(() => window.print(), 300); };
  order.forEach((p, i) => {
    const b = c.boards[p];
    const iframe = h('iframe', { title: p, src: fileURL(p, hashOf(p)), style: { width: b.w + 'px', height: b.h + 'px' }, onload: done });
    root.appendChild(h('div', { class: `print-page b${i}` }, iframe));
  });
  if (!order.length) toast('Nothing to print');
}

// ---- live state ----
async function refresh() {
  const prevRev = S.state && S.state.revision;
  const prevPending = S.state && S.state.pending;
  S.state = await api('/_design/api/state');
  document.title = (S.state.canvas.title || S.state.slug) + ' · Design';
  if (S.revision == null) { renderBar(); renderWorld(); refreshFocus(); sendBoards(); }
  else { const b = $('.bar .badge'); if (b) b.replaceWith(visibilityBadge()); } // a revision view keeps its canvas
  if (S.panel === 'tweaks') showTweaks();
  if (S.panel === 'revisions' && (prevRev !== S.state.revision || prevPending !== S.state.pending)) showRevisions();
}

// subscribe opens the event stream before state is read: every hello (first
// connect and each reconnect) and every newer change refetches state.
function subscribe() {
  let timer = null;
  const later = () => { clearTimeout(timer); timer = setTimeout(() => refresh().catch(() => {}), 80); };
  return new Promise((resolve) => {
    let first = true;
    const connect = () => {
      const es = new EventSource('/_design/api/events');
      es.addEventListener('hello', () => {
        if (first) { first = false; resolve(); return; }
        renewCapability().finally(later);
      });
      es.addEventListener('change', (e) => {
        let seq = 0; try { seq = JSON.parse(e.data).seq || 0; } catch {}
        // seq 0 is a metadata-only change (a pin): the content is the same,
        // but the history panel must reload.
        if (!seq && S.panel === 'revisions') showRevisions();
        if (!S.state || !seq || seq > S.state.seq) later();
      });
      es.addEventListener('resync', later);
      es.onerror = () => { es.close(); if (first) { first = false; resolve(); } setTimeout(connect, 3000); };
    };
    connect();
  });
}

async function boot() {
  document.body.append(
    h('header', { class: 'bar' }),
    h('main', { class: 'viewport', 'aria-label': 'Design canvas' }, h('div', { class: 'world' })),
    h('div', { class: 'toasts', 'aria-live': 'polite' }),
    h('div', { class: 'print-root', 'aria-hidden': 'true' }));
  setupViewport();
  try {
    await subscribe();
    S.cap = await session();
    await refresh();
  } catch (err) { document.body.replaceChildren(h('p', { style: { padding: '24px' }, text: 'Could not load this design: ' + err.message })); return; }
  fit();
  const c = canvas();
  const hash = decodeURIComponent(location.hash.slice(1));
  if (hash && c.boards[hash]) openFocus(hash);
  else if (c.launch && c.launch.view === 'focused' && c.boards[c.launch.board]) openFocus(c.launch.board);
}
boot();
