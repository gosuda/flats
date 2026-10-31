// page.mjs starts the real notebook app under Node on the fake DOM, with a
// fake socket the test plays the server through, and builders for wire
// items in the Appendix A codec.
'use strict';


import { installDOM, fire } from './dom.mjs';

export class FakeSocket {
  static all = [];
  constructor() { this.sent = []; this.closed = false; FakeSocket.all.push(this); }
  send(s) { this.sent.push(JSON.parse(s)); }
  close() { this.closed = true; }
  open() { this.onopen(); return this.sent.at(-1); }
  recv(m) { this.onmessage({ data: JSON.stringify(m) }); }
  drop() { this.closed = true; this.onclose && this.onclose(); }
  frames(t) { return this.sent.filter((m) => (m.type || m.t) === t); }
}

// Manual timers: tests advance time explicitly.
export class Timers {
  constructor() { this.now = 0; this.queue = []; this.seq = 0; }
  setTimeout(fn, ms = 0) { const id = ++this.seq; this.queue.push({ id, at: this.now + ms, fn }); return id; }
  clearTimeout(id) { this.queue = this.queue.filter((t) => t.id !== id); }
  advance(ms) {
    const end = this.now + ms;
    for (;;) {
      this.queue.sort((a, b) => a.at - b.at || a.id - b.id);
      const t = this.queue[0];
      if (!t || t.at > end) break;
      this.queue.shift();
      this.now = t.at;
      t.fn();
    }
    this.now = end;
  }
}

// Ids are kind prefix + 26 base32 characters; tag pads a readable name.
export const pid = (tag) => 'p_' + tag.padEnd(26, 'a');
export const bid = (tag) => 'b_' + tag.padEnd(26, 'a');

export const page = (tag, { title = 'Page ' + tag, parent = null, position = 'V', inc = '5', ver } = {}) =>
  ({ kind: 'page', ref: { id: pid(tag), inc }, ver: ver || inc, v: {}, title, parent: parent ? pid(parent) : null, position });

export const run = (text, ...marks) => ({ text, marks });

export const block = (tag, pageTag, content, { parent = null, position = 'V', inc = '5', v } = {}) => ({
  kind: 'block', ref: { id: bid(tag), inc }, ver: (v && v.content) || inc, v: v || {}, page: pid(pageTag),
  parent: parent ? bid(parent) : null, position, content,
});

export const para = (text, kind = 'paragraph') => {
  const c = { kind, text: text ? [run(text)] : [] };
  if (kind === 'todo') c.checked = false;
  if (kind === 'toggle') c.open_default = false;
  if (kind === 'callout') c.tone = 'neutral';
  return c;
};

// startApp loads the app with a fake connection. With welcome and items it
// also plays the server's welcome and the tree and page snapshots.
export async function startApp({ hash = '', access = 'private', pages = [], blocks = [], seq = '10', fetch = async () => { throw new Error('no network in tests'); } } = {}) {
  const doc = installDOM();
  doc.body.append(doc.createElement('div'));
  doc.body.firstChild.setAttribute('id', 'app');
  const toasts = doc.createElement('div'); toasts.className = 'toasts'; doc.body.append(toasts);
  const live = doc.createElement('div'); live.setAttribute('id', 'announce'); doc.body.append(live);
  if (hash) location.hash = hash;
  globalThis.notebookTest = true;
  FakeSocket.all = [];
  const timers = new Timers();
  const { App } = await import('../app.js');
  const app = new App({ connect: () => new FakeSocket(), timers, now: () => timers.now, fetch });
  const socket = () => FakeSocket.all.at(-1);
  const t = { app, doc, socket, timers, fire };
  if (access) {
    t.hello = socket().open();
    socket().recv({ t: 'welcome', access, session: 'tok', session_id: 'me', me: { label: 'Ann', verified: true } });
    socket().recv({ t: 'snapshot', generation: 0, seq, scope: { kind: 'tree' }, spill: 's1', spill_part: 0, part: 0, parts: 1, data: { items: pages } });
    const shown = app.page;
    const watch = socket().frames('watch').at(-1);
    if (shown) socket().recv({ t: 'snapshot', generation: watch ? watch.generation : 0, seq, scope: { kind: 'page', id: shown }, spill: 's1', spill_part: 1, part: 0, parts: 1, data: { items: blocks.filter((b) => b.page === shown) } });
  }
  return t;
}

// editable returns the contenteditable of a block.
export const editable = (app, tag, field = 'text') => app.editableOf(bid(tag), field);

// caretAt focuses a block's editable and places the caret at code point
// offset cp (and optionally selects to end).
export function caretAt(app, tag, start, end = start) {
  const el = editable(app, tag);
  el.focus();
  app.focusBlock(bid(tag), start);
  if (end !== start) {
    const s = getSelection();
    const a = { node: s.anchorNode, offset: s.anchorOffset };
    app.focusBlock(bid(tag), end);
    s.setBaseAndExtent(a.node, a.offset, s.focusNode, s.focusOffset);
  }
  return el;
}

export const lastAction = (socket) => socket().frames('action').at(-1);

// queued is the newest action the tab queued, sent or waiting behind the
// one in flight.
export const queued = (app) => app.store.pending.actions.at(-1);

// ackAll acknowledges every action in order, as applied.
export function ackAll(app, socket) {
  for (let i = 0; i < 50 && app.store.pending.size; i++) {
    const a = socket().frames('action').at(-1);
    socket().recv({ t: 'ack', n: a.n, seq: '0', outcomes: [{ indexes: a.ops.map((_, j) => j), status: 'applied' }], summary: { applied: a.ops.length, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
  }
}
