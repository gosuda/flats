// History panel (rules 45 to 47, TECH 7): list revisions, open one
// read-only, pin and unpin, roll back after an in-page confirmation, and copy
// elements of an earlier revision into the current board.

import { $, h, icon, toast, confirmDialog, relTime, plural, replace } from './ui.js';
import { SpatialIndex } from './index.js';
import { elementBounds } from './geom.js';

const KINDS = { agent: 'Agent', person: 'People', rollback: 'Rollback', created: 'Created', import: 'Import' };

export async function boardAPI(path, { method = 'GET', body, raw } = {}) {
  const init = { method, headers: {} };
  if (method !== 'GET') init.headers['X-Flats-Board'] = '1';
  if (body !== undefined) {
    if (raw) init.body = body;
    else { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
  }
  const r = await fetch(path, init);
  let data = null;
  try { data = await r.json(); } catch { data = null; }
  if (!r.ok) {
    const err = new Error((data && (data.message || data.error)) || r.statusText || 'Request failed');
    err.status = r.status;
    err.code = data && (data.code || data.category);
    throw err;
  }
  return data;
}

function revTitle(rv) {
  if (rv.message) return rv.message;
  if (rv.kind === 'person') return "People's edits";
  if (rv.kind === 'rollback') return rv.rollback_of ? `Rolled back to r${rv.rollback_of}` : 'Rollback';
  if (rv.kind === 'created') return 'Board created';
  if (rv.kind === 'import') return 'Imported a data file';
  return 'Agent edit';
}

function revChanges(rv) {
  if (rv.kind === 'rollback') return 'whole board';
  const parts = [];
  if (rv.added) parts.push(`+${rv.added}`);
  if (rv.changed) parts.push(`${rv.changed} changed`);
  if (rv.removed) parts.push(`${rv.removed} removed`);
  return parts.join(' · ');
}

const authors = (rv) => (rv.authors || []).join(', ');

export class History {
  constructor(app) {
    this.app = app;
    this.panel = $('#history');
    this.btn = $('#history-btn');
    this.btn.addEventListener('click', () => (this.panel.hidden ? this.open() : this.close()));
    this.revs = [];
    this.info = {};
    this.viewing = null; // revision number shown read-only
    this.timer = 0;
    this.panel.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.stopPropagation(); this.close(); } });
  }

  async open() {
    if (this.app.isPublic) return;
    this.panel.hidden = false;
    this.btn.setAttribute('aria-expanded', 'true');
    document.body.classList.add('history-open');
    this.app.renderer.resize();
    this.render(true);
    await this.load();
    const first = this.panel.querySelector('button');
    if (first) first.focus();
  }

  close() {
    if (this.viewing != null) this.backToLive();
    this.panel.hidden = true;
    this.btn.setAttribute('aria-expanded', 'false');
    document.body.classList.remove('history-open');
    this.app.renderer.resize();
    this.btn.focus();
  }

  deltaArrived() {
    if (this.panel.hidden || this.timer) return;
    this.timer = setTimeout(() => { this.timer = 0; this.load(); }, 6000);
  }

  async load(more = false) {
    try {
      const q = more && this.revs.length ? `?before=${this.revs[this.revs.length - 1].number}` : '';
      const data = await boardAPI('/_board/api/revisions' + q);
      const list = data.revisions || [];
      this.revs = more ? [...this.revs, ...list] : list;
      this.info = data;
      this.render(false);
    } catch (err) {
      this.render(false, err.message);
    }
  }

  render(loading, error) {
    const d = this.info;
    const pins = d.pins ?? this.revs.filter((r) => r.pinned).length;
    const summary = [];
    if (d.count != null) summary.push(plural(d.count, 'revision'));
    if (d.pin_quota) summary.push(`${pins}/${d.pin_quota} pinned`);
    let text = summary.join(' · ');
    if (d.revision_quota) text += `${text ? '. ' : ''}Unpinned revisions beyond ${d.revision_quota} are deleted automatically, oldest first.`;
    const rows = this.revs.map((rv) => this.row(rv));
    replace(this.panel,
      h('h2', {}, h('span', { text: 'History' }), h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Close History', onclick: () => this.close() }, icon('close'))),
      h('p', { class: 'ph', text: loading ? 'Loading revisions…' : text }),
      error ? h('p', { class: 'refusal', role: 'alert', text: error }) : null,
      h('ul', { class: 'plain revs' }, rows),
      h('div', { class: 'hist-foot' },
        d.more ? h('button', { type: 'button', class: 'btn ghost wide', text: 'Show earlier revisions', onclick: () => this.load(true) }) : null,
        d.pin_quota ? h('p', { class: 'note warnline' }, icon('warn'), h('span', { text: `When ${d.pin_quota} revisions are pinned, a new pin is refused. Unpin another revision first.` })) : null));
  }

  row(rv) {
    const cur = this.viewing === rv.number;
    const ch = revChanges(rv);
    const meta = [KINDS[rv.kind] || rv.kind, authors(rv), ch].filter(Boolean).join(' · ');
    const pin = h('button', {
      type: 'button', class: 'btn icon pin' + (rv.pinned ? ' on' : ''), 'aria-pressed': String(!!rv.pinned),
      'aria-label': `Pin r${rv.number}`, title: rv.pinned ? `Unpin r${rv.number}` : `Pin r${rv.number}`,
      onclick: () => this.pin(rv),
    }, icon('pin'));
    return h('li', { class: cur ? 'current' : '' },
      h('button', { type: 'button', class: 'rev', 'aria-current': cur ? 'true' : null, onclick: () => this.view(rv) },
        h('span', { class: 'num', text: `r${rv.number}` }), h('span', { class: 'msg', text: revTitle(rv) }),
        h('span', { class: 'note', text: rv.at ? relTime(rv.at) : '' }), h('span', {}), h('span', { class: 'note meta', text: meta })),
      pin);
  }

  async pin(rv) {
    try {
      const r = await boardAPI(`/_board/api/revisions/${rv.number}/pin`, { method: 'POST', body: { pinned: !rv.pinned } });
      rv.pinned = r.pinned;
      this.load();
    } catch (err) {
      toast(err.message, { error: true });
    }
  }

  // ---- read-only revision view ----

  async view(rv) {
    const app = this.app;
    try {
      const data = await boardAPI(`/_board/api/revisions/${rv.number}/elements`);
      const els = new Map((data.elements || []).map((el) => [el.id, el]));
      const index = new SpatialIndex();
      const get = (id) => els.get(id) || null;
      for (const el of els.values()) {
        index.set(el.id, elementBounds(el, get));
        if (el.type === 'connector') index.setConnector(el.id, el.ends.filter((e) => e.el).map((e) => e.el));
      }
      const meta = { title: '', grid: true, ...(data.meta || {}) };
      if (app.editor) app.editor.commit();
      this.viewing = rv.number;
      this.rev = rv;
      app.doc = { get, all: () => els.values(), index, meta: () => meta, live: false };
      app.select([]);
      app.applyMode();
      app.renderer.invalidateAll();
      app.a11y.dirty();
      this.banner();
      this.render(false);
    } catch (err) {
      toast(`Could not open r${rv.number}: ${err.message}`, { error: true });
    }
  }

  banner() {
    const app = this.app;
    const rv = this.rev;
    const n = app.sel.size;
    const b = h('div', { class: 'banner warn revision', role: 'status' }, icon('eye'),
      h('span', {}, h('strong', { text: `r${rv.number}` }), ` (${rv.at ? relTime(rv.at) : ''}) is open read-only. Select elements to copy them into the current board.`),
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn primary', disabled: !n, onclick: () => this.recover() }, n ? `Copy ${plural(n, 'selected element')} to the current board` : 'Select elements to copy'),
      h('button', { type: 'button', class: 'btn ghost', onclick: () => this.rollback(rv) }, 'Roll back to this revision…'),
      h('button', { type: 'button', class: 'btn', onclick: () => this.backToLive() }, 'Back to the current board'));
    app.banner('revision', b);
  }

  selectionChanged() { if (this.viewing != null) this.banner(); }

  backToLive() {
    const app = this.app;
    this.viewing = null;
    app.doc = app.live;
    app.select([]);
    app.banner(null);
    app.rebuild();
    app.applyMode();
    if (!this.panel.hidden) this.render(false);
  }

  async recover() {
    const app = this.app;
    const ids = [...app.sel];
    if (!ids.length) return;
    try {
      const r = await boardAPI('/_board/api/recover', { method: 'POST', body: { revision: this.viewing, ids } });
      const n = (r.ids && r.ids.length) || ids.length;
      this.backToLive();
      toast(`Copied ${plural(n, 'element')} from r${this.rev.number} into the current board.`);
      if (Array.isArray(r.ids)) setTimeout(() => app.select(r.ids), 300);
    } catch (err) {
      toast(`Could not copy: ${err.message}`, { error: true });
    }
  }

  async rollback(rv) {
    const when = rv.at ? relTime(rv.at) : '';
    const ok = await confirmDialog({
      title: `Roll the whole board back to r${rv.number}?`,
      danger: true,
      ok: 'Roll back the whole board',
      body: [
        h('p', { class: 'note', text: `r${rv.number}: ${[KINDS[rv.kind] || rv.kind, authors(rv), when].filter(Boolean).join(' · ')}` }),
        h('ul', { class: 'impact' },
          h('li', {}, `Every element becomes as it was in r${rv.number}. `, h('strong', { text: 'Edits made after it are rolled back too, including other people\'s edits that are not part of a revision yet.' })),
          h('li', { text: 'Those edits are first kept as a new revision, and the rollback is recorded as a revision too, so it can be rolled back.' }),
          h('li', { text: 'Everyone\'s undo history on this board is cleared.' })),
        h('p', { class: 'note', text: `To keep only some elements, open r${rv.number} and copy the ones you need into the current board.` }),
      ],
      extra: this.viewing === rv.number ? null : h('button', { type: 'button', class: 'btn ghost', onclick: (e) => { e.target.closest('dialog').close(); this.view(rv); } }, icon('eye'), h('span', { text: `Open r${rv.number}` })),
    });
    if (!ok) return;
    try {
      const r = await boardAPI(`/_board/api/revisions/${rv.number}/rollback`, { method: 'POST', body: {} });
      if (this.viewing != null) this.backToLive();
      toast(`Rolled the board back to r${rv.number}${r.revision ? ` (r${r.revision})` : ''}. Everyone's undo history was cleared.`);
      this.load();
    } catch (err) {
      toast(`Rollback failed: ${err.message}`, { error: true });
    }
  }
}
