// Zoom bar and menu, frame list, locked elements list, Find and Send to
// agent (rules 8, 9, 26; TECH 6.2, 11.2).

import { $, h, icon, menu, popover, toast, announce, plural, replace } from './ui.js';
import { KIND_NAMES } from './schema.js';
import { elementCenter, center } from './geom.js';
import { findMatches } from './find.js';
import * as ops from './ops.js';

export class ZoomBar {
  constructor(app) {
    this.app = app;
    this.pct = $('.zoom [data-act=menu]');
    $('.zoom [data-act=out]').addEventListener('click', () => app.zoomBy(0.8));
    $('.zoom [data-act=in]').addEventListener('click', () => app.zoomBy(1.25));
    $('.zoom [data-act=fit]').addEventListener('click', () => app.fitAll());
    this.pct.addEventListener('click', () => this.open());
  }

  update() {
    const p = Math.round(this.app.cam.z * 100);
    this.pct.textContent = p + '%';
    this.pct.setAttribute('aria-label', `Zoom menu, now ${p}%`);
  }

  open() {
    const app = this.app;
    const at = (z) => () => app.zoomAt(app.renderer.w / 2, app.renderer.h / 2, z);
    const grid = app.doc.meta().grid;
    const m = menu([
      { note: `Now ${Math.round(app.cam.z * 100)}%` },
      { label: '50%', run: at(0.5) }, { label: '100%', run: at(1) }, { label: '200%', run: at(2) },
      '-',
      { label: 'Fit all', icon: 'fit', kbd: '⇧1', run: () => app.fitAll() },
      { label: 'Fit selection', icon: 'fit', kbd: '⇧2', disabled: !app.sel.size, run: () => app.fitSelection() },
      '-',
      { label: 'Show dot grid', icon: 'grid', checked: !!grid, run: () => this.toggleGrid(!grid) },
    ], 'Zoom menu');
    const r = this.pct.getBoundingClientRect();
    popover(m, r.right - 240, r.top - 300, { opener: this.pct, menu: true });
  }

  // toggleGrid changes the board's grid setting for everyone; a viewer who
  // cannot edit changes only their own view.
  toggleGrid(on) {
    const app = this.app;
    if (app.canEdit) app.commit([{ op: 'meta', g: 'grid', val: on }]);
    else { app.gridOverride = on; app.renderer.invalidateAll(); }
  }
}

// ---- frame list (rule 9) ----

export class FrameList {
  constructor(app) {
    this.app = app;
    this.el = $('#frame-list');
    this.btn = $('#frames-btn');
    this.timer = 0;
  }

  get open() { return !this.el.hidden; }

  toggle() {
    this.el.hidden = !this.el.hidden;
    this.btn.setAttribute('aria-expanded', String(this.open));
    document.body.classList.toggle('frames-open', this.open);
    this.app.renderer.resize();
    if (this.open) this.render();
  }

  refresh() {
    if (!this.open || this.timer) return;
    this.timer = setTimeout(() => { this.timer = 0; this.render(); }, 300);
  }

  render() {
    const app = this.app;
    const frames = [...app.doc.all()].filter((el) => el.type === 'frame').sort((a, b) => a.y - b.y || a.x - b.x);
    const counts = new Map();
    for (const el of app.doc.all()) if (el.frame) counts.set(el.frame, (counts.get(el.frame) || 0) + 1);
    const v = app.renderer.viewRect();
    const [cx, cy] = center(v);
    const cur = ops.topFrameAt(app.doc, cx, cy);
    const items = frames.map((f) => h('li', {}, h('button', {
      type: 'button', class: 'frame-row', 'aria-current': cur && cur.id === f.id ? 'true' : null,
      onclick: () => app.fitRect({ x: f.x, y: f.y, w: f.w, h: f.h }, { max: 2 }),
    }, icon('frame'), h('span', { class: 'grow', text: f.text || 'Frame' }), h('span', { class: 'note', text: plural(counts.get(f.id) || 0, 'element') }))));
    replace(this.el,
      h('h2', {}, h('span', { text: plural(frames.length, 'frame') }),
        h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Close the frame list', onclick: () => this.toggle() }, icon('close'))),
      h('p', { class: 'ph', text: frames.length ? 'Select a frame to zoom to it.' : 'This board has no frames yet.' }),
      h('ul', { class: 'plain list' }, items));
  }
}

// ---- locked elements (rule 3) ----

export class LockedList {
  constructor(app) { this.app = app; this.box = null; }

  refresh() { if (this.box && this.box.isConnected) this.render(); }

  open() {
    this.box = h('section', { class: 'pop locked-pop', 'aria-labelledby': 'locked-t' });
    this.render();
    popover(this.box, innerWidth - 400, 64);
  }

  render() {
    const app = this.app;
    const locked = [...app.doc.all()].filter((el) => el.locked);
    const frameName = (el) => { const f = el.frame && app.doc.get(el.frame); return f ? f.text || 'Frame' : ''; };
    const rows = locked.map((el) => {
      const label = (el.text || el.alt || KIND_NAMES[el.type]).split('\n')[0].slice(0, 60);
      const fn = frameName(el);
      return h('li', {}, icon(el.type === 'shape' ? 'rect' : el.type),
        h('span', { class: 'grow', text: fn ? `${label} · ${fn}` : label }),
        h('button', { type: 'button', class: 'btn ghost', 'aria-label': `Unlock ${label}`, disabled: !app.canEdit, onclick: () => app.commit(ops.lockOps(app.state, [el.id], false)) },
          icon('unlock'), h('span', { text: 'Unlock' })));
    });
    replace(this.box,
      h('h2', { id: 'locked-t', text: `${plural(locked.length, 'locked element')}` }),
      h('ul', { class: 'plain' }, rows),
      h('p', { class: 'note foot', text: 'A lock only prevents accidental moves. Anyone can unlock it, and moving its frame or group still moves it. An "is editing" mark is a notice, not a lock.' }));
  }
}

// ---- find (rule 9, 11.2) ----

export class FindBar {
  constructor(app) {
    this.app = app;
    this.box = null;
    this.matches = [];
    this.i = -1;
    this.q = '';
  }

  open() {
    if (this.box) { this.input.focus(); this.input.select(); return; }
    const app = this.app;
    this.input = h('input', { id: 'find-q', type: 'text', autocomplete: 'off', placeholder: 'Find on board' });
    this.count = h('span', { class: 'note count', 'aria-live': 'polite' });
    this.input.addEventListener('input', () => { this.q = this.input.value; this.search(true); });
    this.input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); this.step(e.shiftKey ? -1 : 1); }
      if (e.key === 'Escape') { e.preventDefault(); this.close(); }
    });
    this.box = h('div', { class: 'pop findbar', role: 'search' }, icon('search'),
      h('label', { class: 'sr', for: 'find-q', text: 'Find on board' }), this.input, this.count,
      h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Previous result (⇧↵)', onclick: () => this.step(-1) }, icon('up')),
      h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Next result (↵)', onclick: () => this.step(1) }, icon('down')),
      h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Close Find (Esc)', onclick: () => this.close() }, icon('close')));
    document.body.appendChild(this.box);
    this.input.focus();
    app.renderer.dynamic();
  }

  close() {
    if (!this.box) return;
    this.box.remove();
    this.box = null;
    this.matches = [];
    this.i = -1;
    this.app.renderer.dynamic();
    this.app.focusBoard();
  }

  refresh() { if (this.box) this.search(false); }

  search(jump) {
    const app = this.app;
    const cur = this.matches[this.i];
    const get = (id) => app.doc.get(id);
    this.matches = findMatches(app.doc.all(), this.q, (el) => elementCenter(el, get));
    this.i = this.matches.indexOf(cur);
    if (this.i < 0 && this.matches.length) this.i = 0;
    this.show(jump);
  }

  step(d) {
    if (!this.matches.length) return;
    this.i = (this.i + d + this.matches.length) % this.matches.length;
    this.show(true);
  }

  show(jump) {
    const n = this.matches.length;
    this.count.textContent = !this.q.trim() ? '' : n ? `${this.i + 1} of ${n}` : 'No matching text';
    if (jump && n) this.app.reveal(this.matches[this.i]);
    this.app.renderer.dynamic();
  }

  draw(ctx, r) {
    if (!this.box) return;
    const t = r.theme;
    this.matches.forEach((id, i) => {
      const el = this.app.doc.get(id);
      if (!el) return;
      ctx.strokeStyle = t.accent;
      ctx.globalAlpha = i === this.i ? 1 : 0.45;
      ctx.lineWidth = i === this.i ? 3 : 2;
      r.outline(ctx, el, 4);
      ctx.stroke();
    });
    ctx.globalAlpha = 1;
  }
}

// ---- send to agent (rule 26, TECH 6.2) ----

export class ShareBox {
  constructor(app) {
    this.app = app;
    this.btn = $('#share-btn');
    this.btn.addEventListener('click', () => this.send());
    this.queue = [];
  }

  update() {
    const on = this.app.canEdit && this.app.sel.size > 0;
    this.btn.disabled = !on;
    this.btn.classList.toggle('primary', on);
    this.btn.classList.toggle('ghost', !on);
  }

  send() {
    const app = this.app;
    const els = app.selected();
    if (!els.length || !app.canEdit) return;
    this.queue.push({ ids: els.map((el) => el.id), types: new Set(els.map((el) => el.type)), at: new Date() });
    app.net.share(els.map((el) => el.id));
  }

  shared(m) {
    const req = this.queue.shift();
    if (!req) return;
    const app = this.app;
    const n = req.ids.length;
    const what = req.types.size === 1 ? plural(n, KIND_NAMES[[...req.types][0]].toLowerCase()) : plural(n, 'element');
    const phrase = `#${m.n} group these by topic and summarize them`;
    const me = app.me || {};
    const box = h('section', { class: 'pop share-pop', role: 'dialog', 'aria-labelledby': 'share-t' },
      h('div', { class: 'row' }, icon('send'), h('strong', { id: 'share-t', text: `Sent ${what} to the agent` })),
      h('p', { class: 'note', text: 'Tell the agent this number. It lasts up to 24 hours and disappears when the board restarts.' }),
      h('div', { class: 'share-n' }, h('span', { class: 'num', text: `#${m.n}` }), h('span', { class: 'grow', text: `"${phrase}"` }),
        h('button', { type: 'button', class: 'btn ghost', onclick: () => {
          navigator.clipboard.writeText(`#${m.n}`).then(() => announce(`Copied #${m.n}`), () => toast('Could not copy. Select the number and copy it.'));
        } }, icon('copy'), h('span', { text: 'Copy' }))),
      h('dl', { class: 'facts' },
        h('dt', { class: 'note', text: 'Sent by' }), h('dd', {}, me.name || app.name || 'You', ' ',
          me.verified ? h('span', { class: 'badge ok' }, icon('check'), 'Verified') : h('span', { class: 'badge', text: 'Unverified' })),
        h('dt', { class: 'note', text: 'Time' }), h('dd', { text: req.at.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false }) })),
      h('p', { class: 'note', text: 'Only this selection is sent, under this number. Cursors and live selections are never sent. The agent can also read the whole board.' }));
    const r = this.btn.getBoundingClientRect();
    popover(box, r.right - 400, r.bottom + 8, { opener: this.btn });
  }
}
