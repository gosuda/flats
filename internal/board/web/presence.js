// Presence (rules 22 to 24, TECH 6.1): other editors' cursors, selections,
// drags and editing marks on the dynamic layer, avatars in the top bar and
// the participant list. Presence is never stored; on a Public route only
// guests send and receive it, and viewers appear as numbered entries
// (board-public-edit.md rules 10 to 13).

import { $, h, icon, popover, replace, plural } from './ui.js';
import { badge } from './guest.js';
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
    this.viewers = { count: 0, labels: [] };
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
    for (const k of [...this.peers.keys()]) if (now - this.peers.get(k).at > STALE) { this.forget(k); gone = true; }
    if (gone) { this.avatars(); this.app.renderer.dynamic(); }
    if (this.live()) this.app.net.presence({ ...this.mine });
  }

  receive(m) {
    if (!m.key || (this.app.me && m.key === this.app.me.key)) return;
    if (m.gone) {
      this.forget(m.key);
      this.avatars();
      this.app.renderer.dynamic();
      return;
    }
    const p = this.peers.get(m.key) || { key: m.key, sel: [] };
    for (const k of ['name', 'color', 'c', 'vp', 'sel', 'drag', 'edit']) if (m[k] !== undefined) p[k] = m[k];
    if (m.guest !== undefined) p.guest = m.guest === true;
    const isNew = !this.peers.has(m.key);
    p.at = Date.now();
    this.peers.set(m.key, p);
    if (p.name) {
      if (m.key.startsWith('guest:')) this.nameGuest(m.key, p.name);
      else this.app.names.set(m.key, p.name);
    }
    if (isNew) this.avatars();
    this.app.renderer.dynamic();
  }

  // forget removes a peer that left or went silent. A guest's name goes with
  // it, so its creator lines read "Guest" as in a tab opened after it left
  // (board-public-edit.md rule 13); its next presence names it again.
  forget(key) {
    this.peers.delete(key);
    if (key.startsWith('guest:')) this.nameGuest(key, '');
  }

  // nameGuest sets or (with '') forgets a guest's name. Creator lines are on
  // the static layer and in the element list, so both follow the name.
  nameGuest(key, name) {
    const names = this.app.names;
    if ((names.get(key) || '') === name) return;
    if (name) names.set(key, name);
    else names.delete(key);
    this.app.renderer.invalidateAll();
    this.app.a11y.relabel(key);
  }

  reset() {
    for (const k of [...this.peers.keys()]) this.forget(k);
    this.viewers = { count: 0, labels: [] };
    this.app.renderer.dynamic();
  }

  setViewers(v) {
    this.viewers = v;
    this.avatars();
  }

  editingBy(id) {
    for (const p of this.peers.values()) if (p.edit === id) return p;
    return null;
  }

  // ---- top bar ----

  people() {
    const me = this.app.me;
    const list = [...this.peers.values()].sort((a, b) => String(a.name).localeCompare(String(b.name)));
    if (me && !this.app.isPublic) {
      list.push({ key: me.key, name: me.name || me.label || this.app.name || 'You', color: me.color, me: true, verified: !!me.verified, guest: this.app.isGuest });
    }
    return list;
  }

  // kind is how a person is marked: a verified or unverified member, a
  // guest, or, for a guest's view of members, "member".
  kind(p) {
    if (p.guest) return 'guest';
    if (this.app.isGuest) return 'member';
    return (p.me ? p.verified : verifiedKey(p.key)) ? 'verified' : 'unverified';
  }

  avatar(p) {
    const verified = this.kind(p) === 'verified';
    const av = h('span', { class: 'av', 'aria-hidden': 'true' }, String(p.name || '?').slice(0, 1).toUpperCase(),
      verified ? h('span', { class: 'ver' }, icon('check')) : h('span', { class: 'unvb', text: '?' }));
    av.style.background = p.color || '#566056';
    return av;
  }

  avatars() {
    if (this.app.isPublic) { this.btn.hidden = true; return; }
    const ppl = this.people();
    const viewers = this.viewers.count;
    this.btn.hidden = !ppl.length && !viewers;
    const shown = ppl.slice(-avatarSlots());
    const names = ppl.map((p) => `${p.name}${p.me ? ' (you)' : ''} (${this.kind(p)})`).join(', ');
    this.btn.setAttribute('aria-label', `${plural(ppl.length, 'person', 'people')} on the board: ${names}${viewers ? `, and ${plural(viewers, 'viewer')}` : ''}`);
    const more = ppl.length - shown.length + viewers;
    replace(this.btn, ...shown.map((p) => this.avatar(p)), more ? h('span', { class: 'more-count', text: `+${more}` }) : null);
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
      const kind = this.kind(p);
      const mark = kind === 'verified' ? h('span', { class: 'badge ok' }, icon('check'), 'Verified with Tailscale')
        : kind === 'unverified' ? h('span', { class: 'badge', text: 'Unverified · name they entered' })
          : badge(kind);
      const go = p.me ? h('span', { class: 'note', text: 'You' })
        : h('button', { type: 'button', class: 'btn ghost', 'aria-label': `Go to where ${p.name} is looking`, disabled: !p.vp, onclick: () => this.goTo(p) }, icon('go'), h('span', { text: 'Go' }));
      const w = where(p);
      return h('li', { class: 'person' }, this.avatar(p),
        h('div', { class: 'who' }, h('strong', { text: p.name || 'Someone' }), h('span', { class: 'meta' }, mark, w ? h('span', { class: 'note', text: w }) : null)),
        go);
    });
    const v = this.viewers;
    const viewerRows = v.labels.length
      ? v.labels.map((label) => h('li', { class: 'person' }, viewerAvatar(), h('div', { class: 'who' }, h('strong', { text: label })), badge('viewer')))
      : v.count ? [h('li', { class: 'person' }, viewerAvatar(), h('div', { class: 'who' }, h('strong', { text: plural(v.count, 'viewer') }), h('span', { class: 'note', text: 'Watching without a name' })))] : [];
    const guests = this.app.isGuest || ppl.some((p) => p.guest);
    const foot = this.app.isGuest
      ? "Guest names are typed by the guest and aren't checked. History and sending to an agent are for members only."
      : `Verified names come from a Tailscale login. On Local and NetBird connections, each person's entered name shows as it is.${guests ? " Guest names are typed by the guest and aren't checked." : ''}`;
    const total = ppl.length + v.count;
    const el = h('section', { class: 'pop people-pop', role: 'dialog', 'aria-labelledby': 'people-t' },
      h('h2', { id: 'people-t', text: `${plural(total, 'person', 'people')} on the board now` }),
      h('ul', { class: 'plain' }, rows, viewerRows),
      h('p', { class: 'note foot', text: foot }));
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
        if (!tagged) { this.tagAt(ctx, r, ghost, `${name} is moving`, color, p.guest); tagged = true; }
      }
      for (const id of p.sel || []) {
        const el = drag[id] ? null : doc.get(id);
        if (!el) continue;
        r.outline(ctx, el, 5);
        ctx.stroke();
        if (!tagged) { this.tagAt(ctx, r, el, p.edit === id ? `${name} is editing` : name, color, p.guest); tagged = true; }
      }
      if (p.edit && !(p.sel || []).includes(p.edit)) {
        const el = doc.get(p.edit);
        if (el) { r.outline(ctx, el, 5); ctx.stroke(); this.tagAt(ctx, r, el, `${name} is editing`, color, p.guest); }
      }
      ctx.setLineDash([]);
      if (p.c) this.cursor(ctx, r, p.c, name, color, p.guest);
    }
  }

  tagAt(ctx, r, el, text, color, guest) {
    const b = el.type === 'connector' || el.type === 'pen' ? r.app.doc.index.bounds(el.id) : el;
    if (!b) return;
    const [x, y] = r.toScreen(b.x, b.y);
    r.tag(ctx, text, x - 5, y - 8, color);
    if (guest) guestMark(ctx, x - 5 + ctx.measureText(text).width + 15, y - 26, color);
  }

  cursor(ctx, r, c, name, color, guest) {
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
    if (guest) guestMark(ctx, x + 17 + w, y + 16, color);
  }
}

// guestMark draws the guest badge on the canvas next to a name tag, as a
// separate pill so the typed name can never draw or hide it (rule 13).
function guestMark(ctx, x, y, color) {
  ctx.font = `600 10px ${FONT}`;
  const w = ctx.measureText('guest').width + 10;
  ctx.fillStyle = '#ffffff';
  ctx.strokeStyle = color;
  ctx.lineWidth = 1.2;
  ctx.beginPath();
  if (ctx.roundRect) ctx.roundRect(x, y, w, 18, 9); else ctx.rect(x, y, w, 18);
  ctx.fill();
  ctx.stroke();
  ctx.fillStyle = '#242924';
  ctx.textBaseline = 'middle';
  ctx.fillText('guest', x + 5, y + 9);
}

function viewerAvatar() {
  return h('span', { class: 'av viewer', 'aria-hidden': 'true', text: 'V' });
}
