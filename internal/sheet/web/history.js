// History (rules 51 and 52, TECH 16.4 and 19.6): the list of revisions,
// an earlier revision opened read-only in the same grid with the values it
// calculates, pin and unpin, rollback of the whole sheet and recovery of one
// tab. Every confirmation is inline in the panel. The panel is Private
// only; the server refuses these routes on a Public connection.
'use strict';

import { announce } from './a11y.js';
import { button, h, icon, replace } from './dom.js';
import { Model } from './model.js';
import { SERVER_FRAME_MAX, frameType, joinHead, nullPath, tabListError } from './protocol.js';

const API = '/_sheet/api';
const PAGE_SIZE = 50;
const POST_HEADERS = { 'Content-Type': 'application/json', 'X-Flats-Sheet': '1' };
const CALC_WAIT = 300;  // between tries while the host calculates a revision
const CALC_TRIES = 100;
const KIND_LABELS = { created: 'Created', person: 'Edits', agent: 'Agent write', rollback: 'Rollback' };

function relativeTime(created, now) {
  const t = Date.parse(created);
  if (Number.isNaN(t)) return String(created ?? '');
  const s = Math.floor((now - t) / 1000);
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const hr = Math.floor(m / 60);
  if (hr < 24) return `${hr} h ago`;
  const d = Math.floor(hr / 24);
  if (d < 30) return d === 1 ? '1 day ago' : `${d} days ago`;
  return new Date(t).toISOString().slice(0, 10);
}

function errorText(data, status) {
  if (data?.code === 'sheet_capacity') {
    return data.reason === 'disk_quota'
      ? 'The sheet is over its disk quota. Free storage through retention settings or raise the quota.'
      : `A limit of the sheet would be passed (${data.limit || data.reason || 'limit'}).`;
  }
  if (data?.code === 'not_found') return 'That revision or tab is no longer retained. Refresh History and choose another.';
  if (data?.code === 'forbidden') return 'History needs a Private route and a request from the sheet’s own origin.';
  if (data?.code === 'unavailable') return 'History is not available right now. Try again shortly.';
  if (data?.code === 'conflict') return `The sheet changed (${data.reason || 'conflict'}). Refresh History before continuing.`;
  return `The request failed (${data?.code || status}${data?.reason ? `: ${data.reason}` : ''}).`;
}

// tabName reads a changed tab of a revision summary, which names it
// directly or as an object with a name.
const tabName = (t) => (typeof t === 'string' ? t : String(t?.name ?? ''));

// DECODE_STEP is how many bytes of the body are decoded to text at a time.
const DECODE_STEP = 64 * 1024;

// readTabStream builds a read-only model of one tab of a revision from the
// body of its response: the tab_head, tab_more, rows and tab_done frames
// of a snapshot, one to a line (TECH 16.4). The body is read as it
// arrives and one frame is parsed at a time, so no text of the tab's size
// is built, however large the tab. A line longer than a server frame,
// frames out of order and a body that ends before tab_done throw. When
// stale() turns true the reading stops and the answer is null.
export async function readTabStream(tabs, body, stale = () => false) {
  const m = new Model();
  m.setTabs(tabs);
  let head = null;
  let tab = null;
  let parts = 0;
  let step = 'head'; // head, more, rows, done
  const bad = (why) => { throw new TypeError(`revision tab: ${why}`); };
  const take = (f) => {
    const type = frameType(f);
    const nulled = nullPath(f);
    if (nulled != null) bad(`a ${type} frame whose ${nulled} is null`);
    if (tab != null && f.tab !== tab) bad('a frame of another tab');
    switch (type) {
      case 'tab_head':
        if (step !== 'head') bad('a second head');
        tab = f.tab;
        if (f.more) { head = f; step = 'more'; } else { m.openHead(f); step = 'rows'; }
        return;
      case 'tab_more':
        if (step !== 'more') bad('tab_more without a head');
        joinHead(head, f);
        if (!head.more) { m.openHead(head); head = null; step = 'rows'; }
        return;
      case 'rows':
        if (step !== 'rows' || f.part !== parts) bad('rows out of order');
        m.addRows(f);
        parts++;
        return;
      case 'tab_done':
        if (step !== 'rows' || f.parts !== parts) bad('an end that does not match the rows');
        m.done();
        step = 'done';
        return;
    }
    bad(`a frame of the kind ${type}`);
  };
  // A line that is still arriving is kept in pieces and joined once it is
  // whole. A line holds one frame, which is at most SERVER_FRAME_MAX bytes
  // and so at most as many code units.
  let line = [];
  let size = 0;
  const feed = (text) => {
    let start = 0;
    for (let end; (end = text.indexOf('\n', start)) >= 0; start = end + 1) {
      if (size + end - start > SERVER_FRAME_MAX) bad('a line longer than a frame');
      if (step === 'done') bad('frames after the end');
      line.push(text.slice(start, end));
      const whole = line.join('');
      line = [];
      size = 0;
      take(JSON.parse(whole));
    }
    if (start === text.length) return;
    line.push(text.slice(start));
    size += text.length - start;
    if (size > SERVER_FRAME_MAX) bad('a line longer than a frame');
  };
  const reader = body.getReader();
  const decoder = new TextDecoder();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (stale()) { reader.cancel().catch(() => {}); return null; }
      if (done) { feed(decoder.decode()); break; }
      for (let i = 0; i < value.length; i += DECODE_STEP) feed(decoder.decode(value.subarray(i, i + DECODE_STEP), { stream: true }));
    }
  } catch (err) {
    reader.cancel().catch(() => {});
    throw err;
  }
  if (size || step !== 'done') bad('the tab ends before tab_done');
  return m;
}

// History draws the panel into host. opts: fetch, now, timers, onClose(),
// onView(view or null) with view = {revision, tabs, tab, model},
// onRolledBack(), onRecovered(result), onPeek() when the text of a cell of
// the open revision arrived, liveTabs() for the names in use, session()
// for the token of this browser tab's session.
export class History {
  constructor(host, { fetch = globalThis.fetch, now = () => Date.now(), timers = globalThis, onClose, onView, onRolledBack, onRecovered, onPeek, liveTabs = () => [], session = () => null } = {}) {
    this.host = host;
    // Called unbound: window.fetch throws "Illegal invocation" as a method.
    this.fetch = (...args) => fetch(...args);
    this.now = now;
    this.timers = timers;
    this.session = session;
    this.onClose = onClose;
    this.onView = onView;
    this.onRolledBack = onRolledBack;
    this.onRecovered = onRecovered;
    this.onPeek = onPeek;
    this.liveTabs = liveTabs;
    this.open = false;
    this.gen = 0;
    this.#clear();
    host.hidden = true;
    host.addEventListener('keydown', (e) => {
      if (!this.open || e.key !== 'Escape') return;
      e.preventDefault();
      e.stopPropagation();
      if (this.confirm && !this.busy) { this.confirm = null; this.focusKey = 'rollback'; this.render(); } else this.close();
    });
  }

  #clear() {
    this.revisions = [];
    this.next = null;
    this.loading = false;
    this.error = '';
    this.status = '';
    this.view = null;    // {revision, rev, tabs, tab, model, load, asked}
    this.confirm = null; // 'rollback' while the inline confirmation shows
    this.busy = false;
    this.focusKey = null;
  }

  toggle(open = !this.open) {
    if (open === this.open) return;
    this.open = open;
    this.gen++;
    const viewing = !!this.view;
    this.#clear();
    if (viewing) this.onView?.(null);
    if (!open) { this.render(); return; }
    this.focusKey = 'close';
    this.#load(false);
  }

  close() {
    this.toggle(false);
    this.onClose?.();
  }

  // #signed adds the session to a request that changes the sheet, so the
  // change is the person's own: credited to their name and, for a
  // recovered tab, on their undo history (rule 23).
  #signed(body) {
    const session = this.session();
    return session ? { ...body, session } : body;
  }

  // #request sends a request and reads a successful answer with read, by
  // default as one JSON value. An error answer is a small JSON value.
  async #request(method, path, body, read = (res) => res.json()) {
    const init = { method, credentials: 'same-origin' };
    if (body !== undefined) { init.headers = { ...POST_HEADERS }; init.body = JSON.stringify(body); }
    let res;
    let data = null;
    for (let tries = 1; ; tries++) {
      try {
        res = await this.fetch(path, init);
      } catch {
        // A request that changes the sheet may have arrived although its
        // answer did not: the outcome is not known, so the message does not
        // say that nothing happened.
        throw new Error(method === 'GET' ? 'Could not reach the sheet. Check the connection and try again.'
          : 'The host did not answer, so it is not known whether this was done. Look at the tab bar and refresh History before you try again.');
      }
      if (res.ok) break;
      try { data = await res.json(); } catch { data = null; }
      // The host calculates a revision before it serves it, and answers
      // meanwhile that it is calculating.
      if (res.status !== 503 || data?.reason !== 'calculating' || tries >= CALC_TRIES) break;
      await new Promise((done) => { this.timers.setTimeout(done, CALC_WAIT); });
    }
    if (!res.ok) throw new Error(errorText(data, res.status));
    try { data = await read(res); } catch { data = null; }
    if (!data || typeof data !== 'object') throw new Error('The sheet returned an invalid response.');
    return data;
  }

  async #load(more) {
    const gen = this.gen;
    this.loading = true;
    this.error = '';
    this.render();
    const before = more && this.next != null ? `&before=${encodeURIComponent(this.next)}` : '';
    try {
      const data = await this.#request('GET', `${API}/revisions?limit=${PAGE_SIZE}${before}`);
      if (gen !== this.gen) return;
      const list = data.revisions || [];
      this.revisions = more ? this.revisions.concat(list) : list;
      this.next = data.next > 0 ? data.next : null;
    } catch (e) {
      if (gen !== this.gen) return;
      this.error = e.message;
    }
    this.loading = false;
    this.render();
  }

  // openRevision shows revision n read-only: its tab list, then its first
  // tab, or the tab the person had open when the revision has it.
  async openRevision(n, preferTab = null) {
    const gen = this.gen;
    this.error = '';
    this.status = '';
    this.confirm = null;
    this.busy = true;
    this.render();
    try {
      const data = await this.#request('GET', `${API}/revisions/${encodeURIComponent(n)}`);
      if (gen !== this.gen) return;
      // The tab list names the tabs the stream of each is read against.
      if (typeof data.date !== 'string' || tabListError(data.tabs)) throw new Error('The sheet returned an invalid response.');
      const tabs = data.tabs;
      if (!tabs.length) throw new Error('This revision has no tabs.');
      const rev = this.revisions.find((r) => r.number === n) || { number: n };
      this.view = { revision: n, rev, tabs, tab: null, model: null, load: 0, asked: new Set() };
      this.busy = false;
      await this.openTab((tabs.find((t) => t.id === preferTab) || tabs[0]).id);
    } catch (e) {
      if (gen !== this.gen) return;
      this.busy = false;
      this.error = e.message;
      this.render();
    }
  }

  // openTab loads one tab of the open revision. When the person asks for
  // another tab before the answer is here, only the newest request shows.
  async openTab(id) {
    const gen = this.gen;
    const view = this.view;
    if (!view) return;
    const load = ++view.load;
    const stale = () => gen !== this.gen || this.view !== view || view.load !== load;
    this.busy = true;
    this.render();
    try {
      const model = await this.#request('GET', `${API}/revisions/${encodeURIComponent(view.revision)}/tabs/${encodeURIComponent(id)}`, undefined,
        (res) => readTabStream(view.tabs, res.body, stale));
      if (stale()) return;
      view.model = model;
      view.tab = id;
      view.asked = new Set();
      this.focusKey = `rev-${view.revision}`;
      this.onView?.(view);
      announce(`Viewing revision ${view.revision}, tab ${view.tabs.find((t) => t.id === id).name}. Read-only.`);
    } catch (e) {
      if (stale()) return;
      this.error = e.message;
    }
    this.busy = false;
    this.render();
  }

  // peek reads the content of one cell of the open revision as it would be
  // typed, for the formula bar: the revision's frames carry no formula
  // text (TECH 16.4). The answer belongs to the tab model it was asked
  // for; one that arrives after the person moved to another tab or left the
  // revision is dropped. A cell is asked for once.
  async peek(row, col) {
    const gen = this.gen;
    const view = this.view;
    const model = view?.model;
    const key = `${row}:${col}`;
    if (!model || model.peeked(row, col) || view.asked.has(key)) return;
    view.asked.add(key);
    const stale = () => gen !== this.gen || this.view !== view || view.model !== model;
    try {
      const data = await this.#request('GET', `${API}/revisions/${encodeURIComponent(view.revision)}/tabs/${encodeURIComponent(view.tab)}/cell?row=${encodeURIComponent(row)}&col=${encodeURIComponent(col)}`);
      if (stale()) return;
      if (typeof data.edit !== 'string' || typeof data.formula !== 'boolean') throw new Error('The sheet returned an invalid response.');
      model.setPeek(row, col, { edit: data.edit, formula: data.formula });
      this.onPeek?.();
    } catch (e) {
      if (stale()) return;
      this.error = e.message;
      this.render();
    }
  }

  // back returns from a revision to the current sheet.
  back() {
    if (!this.view) return;
    this.view = null;
    this.confirm = null;
    this.onView?.(null);
    this.render();
  }

  // restoredName is the name tab recovery will give the open tab: its own
  // when no live tab has it, else with "(restored)" (rule 52).
  restoredName() {
    const view = this.view;
    const name = view.tabs.find((t) => t.id === view.tab).name;
    const used = new Set(this.liveTabs().map((t) => t.name.toLowerCase()));
    if (!used.has(name.toLowerCase())) return { name, as: name };
    for (let i = 1; ; i++) {
      const as = i === 1 ? `${name} (restored)` : `${name} (restored ${i})`;
      if (!used.has(as.toLowerCase())) return { name, as };
    }
  }

  async recover() {
    const view = this.view;
    if (!view || this.busy) return;
    const gen = this.gen;
    // The name is worked out before the request: once the tab is back, the
    // live tab list holds it and would count it as a name in use.
    const { name, as: planned } = this.restoredName();
    this.busy = true;
    this.error = '';
    this.render();
    try {
      const result = await this.#request('POST', `${API}/recover`, this.#signed({ revision: view.revision, tab: view.tab }));
      if (gen !== this.gen) return;
      const as = this.liveTabs().find((t) => t.id === result.tab)?.name ?? planned;
      this.status = as === name ? `Restored tab “${name}” at the end of the tab bar.` : `Restored tab “${name}” as “${as}”.`;
      this.view = null;
      this.onView?.(null);
      this.onRecovered?.(result);
    } catch (e) {
      if (gen !== this.gen) return;
      this.error = e.message;
    }
    this.busy = false;
    this.render();
  }

  async rollback() {
    const view = this.view;
    if (!view || this.busy) return;
    const gen = this.gen;
    this.busy = true;
    this.error = '';
    this.render();
    try {
      await this.#request('POST', `${API}/rollback`, this.#signed({ revision: view.revision }));
      if (gen !== this.gen) return;
      this.status = `Rolled the sheet back to revision ${view.revision}. Undo history was cleared for everyone.`;
      this.view = null;
      this.confirm = null;
      this.busy = false;
      this.onView?.(null);
      this.onRolledBack?.();
      await this.#load(false);
      return;
    } catch (e) {
      if (gen !== this.gen) return;
      this.error = e.message;
    }
    this.busy = false;
    this.confirm = null;
    this.render();
  }

  async pin(rev) {
    if (this.busy) return;
    const gen = this.gen;
    const pinned = !rev.pinned;
    this.busy = true;
    this.error = '';
    this.focusKey = 'pin';
    this.render();
    try {
      const body = await this.#request('POST', `${API}/pin`, { revision: rev.number, pinned });
      if (gen !== this.gen) return;
      rev.pinned = typeof body.pinned === 'boolean' ? body.pinned : pinned;
      this.status = `${rev.pinned ? 'Pinned' : 'Unpinned'} revision ${rev.number}.`;
    } catch (e) {
      if (gen !== this.gen) return;
      this.error = e.message;
    }
    this.busy = false;
    this.render();
  }

  // banner is the strip above the grid while a revision is open.
  banner() {
    const v = this.view;
    if (!v?.model) return null;
    const r = v.rev;
    const who = Array.isArray(r.authors) && r.authors.length ? `, ${r.authors.join(', ')}` : '';
    return h('div', { class: 'banner', role: 'group', 'aria-label': `Revision ${v.revision}` },
      h('span', { class: 'grow', text: `Viewing revision ${v.revision} (${KIND_LABELS[r.kind] || 'Revision'}${who}). Read-only, with the values it calculates.` }),
      button('Back to current', () => this.back(), { class: 'btn' }));
  }

  render() {
    const host = this.host;
    host.hidden = !this.open;
    if (!this.open) { replace(host); return; }
    // Drawing again replaces the buttons; focus stays on the one of the same key.
    const held = host.contains(document.activeElement) ? document.activeElement.getAttribute('data-key') : null;
    const list = h('ol', { 'aria-label': 'Revisions' }, this.revisions.map((rev) => h('li', {}, this.#row(rev))));
    replace(host,
      h('header', {}, h('h2', { text: 'History' }), button('Close', () => this.close(), { class: 'tb', 'aria-label': 'Close History', 'data-key': 'close' }, icon('close'))),
      this.error ? h('p', { class: 'hist-msg bad', role: 'alert', text: this.error }) : null,
      this.status ? h('p', { class: 'hist-msg', role: 'status', text: this.status }) : null,
      this.loading && !this.revisions.length ? h('p', { class: 'hist-msg', text: 'Loading…' }) : null,
      !this.loading && !this.revisions.length && !this.error ? h('p', { class: 'hist-msg', text: 'No revisions yet.' }) : null,
      list,
      this.next != null ? button(this.loading ? 'Loading…' : 'Load more', () => this.#load(true), { class: 'btn more', disabled: this.loading, 'data-key': 'more' }) : null,
      this.#actions());
    const key = this.focusKey || held;
    this.focusKey = null;
    const target = key && host.querySelector(`[data-key="${key}"]`);
    if (target) target.focus();
    if (this.error) announce(this.error); else if (this.status) announce(this.status);
  }

  #row(rev) {
    const n = rev.number;
    const authors = Array.isArray(rev.authors) ? rev.authors.filter(Boolean) : [];
    const tabs = (Array.isArray(rev.tabs) ? rev.tabs : []).map(tabName).filter(Boolean);
    const current = this.view?.revision === n;
    return h('button', { type: 'button', class: 'rev', 'data-key': `rev-${n}`, 'aria-current': current ? 'true' : null, disabled: this.busy, onclick: () => this.openRevision(n, this.view?.tab) },
      h('span', { class: 'l1' }, `Revision ${n}`, rev.pinned ? h('span', { class: 'pin' }, icon('pin', 'Pinned')) : null),
      h('span', { class: 'l2', text: [KIND_LABELS[rev.kind] || String(rev.kind ?? ''), authors.join(', '), relativeTime(rev.created, this.now())].filter(Boolean).join(' · ') }),
      rev.message ? h('span', { class: 'l2', text: rev.message }) : null,
      tabs.length ? h('span', { class: 'tabsch', text: `Changed: ${tabs.join(', ')}` }) : null);
  }

  // #actions is the panel's foot while a revision is open: restore its open
  // tab, pin, and roll back with an inline confirmation.
  #actions() {
    const v = this.view;
    if (!v?.model) return null;
    const n = v.revision;
    if (this.confirm === 'rollback') {
      return h('div', { class: 'acts confirm-acts', role: 'alertdialog', 'aria-label': 'Roll back' },
        h('p', { text: `Roll back the whole sheet to revision ${n}? Every tab returns to how it was then, as a new revision. Everyone’s undo history is cleared.` }),
        button('Roll back', () => this.rollback(), { class: 'btn danger', disabled: this.busy, 'data-key': 'rollback-yes' }),
        button('Cancel', () => { this.confirm = null; this.focusKey = 'rollback'; this.render(); }, { class: 'btn', disabled: this.busy, 'data-key': 'rollback-no' }));
    }
    const { name, as } = this.restoredName();
    return h('div', { class: 'acts' },
      button(as === name ? `Restore tab “${name}”` : `Restore tab “${name}” as “${as}”`, () => this.recover(), { class: 'btn', disabled: this.busy, 'data-key': 'recover' }),
      button(v.rev.pinned ? `Unpin revision ${n}` : `Pin revision ${n}`, () => this.pin(v.rev), { class: 'btn', 'aria-pressed': String(!!v.rev.pinned), disabled: this.busy, 'data-key': 'pin' }),
      button('Roll back to this revision…', () => { this.confirm = 'rollback'; this.focusKey = 'rollback-no'; this.render(); }, { class: 'btn', disabled: this.busy, 'data-key': 'rollback' }));
  }
}
