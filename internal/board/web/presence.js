// Presence (rules 22 to 24, TECH 6.1): other editors' cursors, selections,
// drags and editing marks on the dynamic layer, avatars in the top bar and
// the participant list. Presence is never stored and never sent on Public.

import { $, h, icon, popover, replace } from './ui.js';
import { FONT } from './render.js';
import { topFrameAt } from './ops.js';

const STALE = 30000;
const KEEPALIVE = 10000;

// verified reports whether an editor key is a trusted Tailscale identity.
// The server sets keys; a client value never earns the mark (rule 23).
export const verifiedKey = (key) => typeof key === 'string' && key.startsWith('ts:');

// avatarSlots is how many avatars the header shows before "+N"; narrow
// windows keep the header to one row.
const NARROW = '(max-width: 960px)', PHONE = '(max-width: 640px)';
function avatarSlots() {
  if (typeof matchMedia !== 'function') return 5;
  if (matchMedia(PHONE).matches) return 1;
  return matchMedia(NARROW).matches ? 2 : 5;
}

export class Presence {
  constructor(app) {
    this.app = app;
    this.peers = new Map(); // key -> {key, name, color, c, vp, sel, drag, edit, at}
    this.mine = { c: null, vp: null, sel: [], drag: null, edit: null };
    this.btn = $('.people');
    this.btn.addEventListener('click', () => this.openList());
    if (typeof matchMedia === 'function') for (const q of [NARROW, PHONE]) matchMedia(q).addEventListener('change', () => this.avatars());
    setInterval(() => this.tick(), KEEPALIVE);
  }

  live() { return this.app.canEdit && !this.app.isPublic; }

  send(partial) {
    Object.assign(this.mine, partial);
    if (this.live()) this.app.net.presence({ ...this.mine });
  }

  sendAll() { this.send({}); this.avatars(); }

  tick() {
    const now = Date.now();
    let gone = false;
    for (const [k, p] of this.peers) if (now - p.at > STALE) { this.peers.delete(k); gone = true; }
    if (gone) { this.avatars(); this.app.renderer.dynamic(); }
    if (this.live()) this.app.net.presence({ ...this.mine });
  }

  receive(m) {
    if (!m.key || (this.app.me && m.key === this.app.me.key)) return;
    if (m.gone) { this.peers.delete(m.key); this.avatars(); this.app.renderer.dynamic(); return; }
    const p = this.peers.get(m.key) || { key: m.key, sel: [] };
    for (const k of ['name', 'color', 'c', 'vp', 'sel', 'drag', 'edit']) if (m[k] !== undefined) p[k] = m[k];
    const isNew = !this.peers.has(m.key);
    p.at = Date.now();
    this.peers.set(m.key, p);
    if (p.name) this.app.names.set(m.key, p.name);
    if (isNew) this.avatars();
    this.app.renderer.dynamic();
  }

  editingBy(id) {
    for (const p of this.peers.values()) if (p.edit === id) return p;
    return null;
  }

  // ---- top bar ----

  people() {
    const me = this.app.me;
    const list = [...this.peers.values()].sort((a, b) => String(a.name).localeCompare(String(b.name)));
    if (me) list.push({ key: me.key, name: me.name || this.app.name || 'You', color: me.color, me: true, verified: !!me.verified });
    return list;
  }

  avatar(p) {
    const verified = p.me ? p.verified : verifiedKey(p.key);
    const av = h('span', { class: 'av', 'aria-hidden': 'true' }, String(p.name || '?').slice(0, 1).toUpperCase(),
      verified ? h('span', { class: 'ver' }, icon('check')) : h('span', { class: 'unvb', text: '?' }));
    av.style.background = p.color || '#566056';
    return av;
  }

  avatars() {
    if (this.app.isPublic) { this.btn.hidden = true; return; }
    const ppl = this.people();
    this.btn.hidden = !ppl.length;
    const shown = ppl.slice(-avatarSlots());
    const names = ppl.map((p) => `${p.name}${p.me ? ' (you)' : ''} (${(p.me ? p.verified : verifiedKey(p.key)) ? 'verified' : 'unverified'})`).join(', ');
    this.btn.setAttribute('aria-label', `${ppl.length} ${ppl.length === 1 ? 'person' : 'people'} on the board: ${names}`);
    replace(this.btn, ...shown.map((p) => this.avatar(p)), ppl.length > shown.length ? h('span', { class: 'more-count', text: `+${ppl.length - shown.length}` }) : null);
  }

  openList() {
    const app = this.app;
    const ppl = this.people();
    const where = (p) => {
      if (!p.vp) return '';
      const f = topFrameAt(app.doc, p.vp[0] + p.vp[2] / 2, p.vp[1] + p.vp[3] / 2);
      return f ? f.text || 'Frame' : '';
    };
    const rows = ppl.map((p) => {
      const verified = p.me ? p.verified : verifiedKey(p.key);
      const badge = verified
        ? h('span', { class: 'badge ok' }, icon('check'), 'Verified with Tailscale')
        : h('span', { class: 'badge', text: 'Unverified · name they entered' });
      const go = p.me ? h('span', { class: 'note', text: 'You' })
        : h('button', { type: 'button', class: 'btn ghost', 'aria-label': `Go to where ${p.name} is looking`, disabled: !p.vp, onclick: () => this.goTo(p) }, icon('go'), h('span', { text: 'Go' }));
      const w = where(p);
      return h('li', { class: 'person' }, this.avatar(p),
        h('div', { class: 'who' }, h('strong', { text: p.name || 'Someone' }), h('span', { class: 'meta' }, badge, w ? h('span', { class: 'note', text: w }) : null)),
        go);
    });
    const el = h('section', { class: 'pop people-pop', role: 'dialog', 'aria-labelledby': 'people-t' },
      h('h2', { id: 'people-t', text: `${ppl.length} ${ppl.length === 1 ? 'person' : 'people'} on the board now` }),
      h('ul', { class: 'plain' }, rows),
      h('p', { class: 'note foot', text: 'Verified names come from a Tailscale login. On Local and NetBird connections, each person\'s entered name shows as it is.' }));
    const r = this.btn.getBoundingClientRect();
    popover(el, r.right - 400, r.bottom + 6, { opener: this.btn });
  }

  goTo(p) {
    const [x, y, w, hgt] = p.vp;
    this.app.fitRect({ x, y, w, h: hgt }, { max: 4, pad: 0 });
  }

  // ---- dynamic layer ----

  draw(ctx, r) {
    if (this.app.isPublic || !this.app.doc.live) return;
    const doc = this.app.doc;
    for (const p of this.peers.values()) {
      const color = p.color || '#566056';
      const name = p.name || 'Someone';
      ctx.setLineDash([5, 4]);
      ctx.lineWidth = 2;
      ctx.strokeStyle = color;
      const drag = p.drag || {};
      let tagged = false;
      for (const [id, g] of Object.entries(drag)) {
        const el = doc.get(id);
        if (!el || !Array.isArray(g)) continue;
        const ghost = { ...el, x: g[0], y: g[1], w: g[2], h: g[3], rot: g[4] || 0 };
        r.outline(ctx, ghost, 5);
        ctx.stroke();
        if (!tagged) { this.tagAt(ctx, r, ghost, `${name} is moving`, color); tagged = true; }
      }
      for (const id of p.sel || []) {
        const el = drag[id] ? null : doc.get(id);
        if (!el) continue;
        r.outline(ctx, el, 5);
        ctx.stroke();
        if (!tagged) { this.tagAt(ctx, r, el, p.edit === id ? `${name} is editing` : name, color); tagged = true; }
      }
      if (p.edit && !(p.sel || []).includes(p.edit)) {
        const el = doc.get(p.edit);
        if (el) { r.outline(ctx, el, 5); ctx.stroke(); this.tagAt(ctx, r, el, `${name} is editing`, color); }
      }
      ctx.setLineDash([]);
      if (p.c) this.cursor(ctx, r, p.c, name, color);
    }
  }

  tagAt(ctx, r, el, text, color) {
    const b = el.type === 'connector' || el.type === 'pen' ? r.app.doc.index.bounds(el.id) : el;
    if (!b) return;
    const [x, y] = r.toScreen(b.x, b.y);
    r.tag(ctx, text, x - 5, y - 8, color);
  }

  cursor(ctx, r, c, name, color) {
    const [x, y] = r.toScreen(c[0], c[1]);
    if (x < -20 || y < -20 || x > r.w + 20 || y > r.h + 20) return;
    ctx.fillStyle = color;
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = 1.4;
    ctx.beginPath();
    ctx.moveTo(x, y); ctx.lineTo(x + 15, y + 9); ctx.lineTo(x + 8.5, y + 10.6); ctx.lineTo(x + 5.5, y + 18);
    ctx.closePath();
    ctx.fill();
    ctx.stroke();
    ctx.font = `600 11px ${FONT}`;
    const w = ctx.measureText(name).width + 14;
    ctx.beginPath();
    if (ctx.roundRect) ctx.roundRect(x + 14, y + 16, w, 18, 9); else ctx.rect(x + 14, y + 16, w, 18);
    ctx.fill();
    ctx.fillStyle = '#ffffff';
    ctx.textBaseline = 'middle';
    ctx.fillText(name, x + 21, y + 25);
  }
}
