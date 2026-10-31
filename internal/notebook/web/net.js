// Live connection (TECH 9, 13, Appendix A): hello and welcome, watch,
// snapshot assembly by generation, scope and window, delta and delta_part
// assembly, resync, ordered action sending with exact resend, acks and
// rejects, and reconnect backoff. The transport is injectable so tests can
// play the server.
'use strict';


import { Store } from './state.js';

export const FRAME_MAX = 256 * 1024; // client frame, encoded UTF-8 bytes
export const ACTION_MAX = 8 * 1024 * 1024;
export const PARTS_MAX = 32;
const RETRY_MIN = 1000, RETRY_MAX = 30000;
const PRESENCE_GAP = 50; // at most 20 presence updates a second

export class TooLargeError extends Error {}

export function utf8Length(s) {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) { n += 4; i++; }
    else n += 3;
  }
  return n;
}

// jsonCost is the encoded size of the code unit(s) at i inside a JSON
// string literal, and how many code units they take.
function jsonCost(s, i) {
  const c = s.charCodeAt(i);
  if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) return [2, 4];
  if (c >= 0xd800 && c <= 0xdfff) return [1, 6];
  if (c === 0x22 || c === 0x5c || c === 0x08 || c === 0x09 || c === 0x0a || c === 0x0c || c === 0x0d) return [1, 2];
  if (c < 0x20) return [1, 6];
  return [1, c < 0x80 ? 1 : c < 0x800 ? 2 : 3];
}

// actionFrames encodes an action as one frame, or as zero-based action_part
// frames carrying slices of its JSON text when it exceeds the frame limit.
export function actionFrames(action) {
  const body = { type: 'action', n: action.n, action_id: action.action_id, ops: action.ops };
  if (action.typing_block) body.typing_block = action.typing_block;
  if (action.close) body.close = true;
  const whole = JSON.stringify(body);
  if (utf8Length(whole) <= FRAME_MAX) return [whole];
  const data = whole;
  if (utf8Length(data) > ACTION_MAX) throw new TooLargeError('action over 8 MiB');
  const budget = FRAME_MAX - utf8Length(JSON.stringify({ type: 'action_part', n: action.n, part: PARTS_MAX, parts: PARTS_MAX, data: '' }));
  const slices = [];
  let start = 0, enc = 0;
  for (let i = 0; i < data.length;) {
    const [units, e] = jsonCost(data, i);
    if (enc + e > budget) { slices.push(data.slice(start, i)); start = i; enc = 0; }
    enc += e;
    i += units;
  }
  slices.push(data.slice(start));
  if (slices.length > PARTS_MAX) throw new TooLargeError('action needs more than 32 parts');
  return slices.map((s, i) => JSON.stringify({ type: 'action_part', n: action.n, part: i, parts: slices.length, data: s }));
}

function defaultConnect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return new WebSocket(`${proto}//${location.host}/_notebook/ws`);
}

// Net drives one tab's connection. Handlers (all optional):
// welcome(msg), changed(), ack(action, msg), reject(action, msg),
// stopped(code, detail), presence(msg), activity(msg),
// confirmCreation(ref), viewAdjustment(msg), overwrote(list), status().
export class Net {
  constructor(store = new Store(), handlers = {}, { connect = defaultConnect, timers = globalThis, now = () => Date.now() } = {}) {
    this.store = store;
    this.h = handlers;
    this.connect = connect;
    this.timers = timers;
    this.now = now;
    this.ws = null;
    this.open = false;
    this.stopped = null;       // {code, detail} once editing stopped for good
    this.access = null;
    this.session = null;       // resume token, in memory only
    this.sessionId = null;
    this.name = null;
    this.generation = 0;       // watch and snapshot generation of this socket
    this.deltaFloor = 0;       // delta_part generations below this are stale
    this.lastSeq = null;       // last delta sequence applied on any socket
    this.snapParts = new Map(); // generation|scope|seq|window -> {parts, got}
    this.deltaParts = null;
    this.watching = { page: null, visible_databases: [] };
    this.inflight = null;      // n awaiting ack or reject
    this.retryTimer = null;
    this.reconnectTimer = null;
    this.reconnectDelay = RETRY_MIN;
    this.retryDelay = RETRY_MIN;
    this.spill = null;         // {spill, next_window} of an unfinished database capture
    this.presenceAt = 0;
    this.presenceTimer = null;
    this.presenceNext = null;
  }

  get status() {
    if (this.stopped) return 'stopped';
    if (!this.open) return 'offline';
    return this.store.pending.size ? 'saving' : 'saved';
  }

  start() {
    if (this.stopped) return;
    const ws = this.connect();
    this.ws = ws;
    ws.onopen = () => this.#send(this.hello());
    ws.onmessage = (ev) => this.receive(JSON.parse(ev.data));
    ws.onclose = () => this.#closed(ws);
  }

  stop(code, detail = {}) {
    this.stopped = { code, detail };
    this.timers.clearTimeout(this.retryTimer);
    this.timers.clearTimeout(this.reconnectTimer);
    if (this.ws) { this.ws.onclose = null; this.ws.close(); this.ws = null; }
    this.open = false;
    this.h.stopped?.(code, detail);
    this.h.status?.();
  }

  hello() {
    const p = this.store.pending;
    const m = { type: 'hello', version: 1, pending: { count: p.size } };
    if (p.size) m.pending.first_n = p.actions[0].n;
    if (this.session) m.session = this.session;
    if (this.lastSeq != null) m.watermark = this.lastSeq;
    if (this.name) m.name = this.name;
    if (this.watching.page) m.page = this.watching.page;
    const scopes = this.store.pendingScopes();
    if (scopes.length) m.pending_scopes = scopes;
    if (this.spill) { m.spill = this.spill.spill; m.next_window = this.spill.next_window; }
    return m;
  }

  // watch selects the page and visible databases. A new generation makes
  // older snapshot parts stale; scopes touched by pending actions stay
  // subscribed until those actions settle.
  watch(page, databases = []) {
    this.watching = { page, visible_databases: [...databases] };
    if (!this.open) return;
    this.generation++;
    this.#send({ type: 'watch', generation: this.generation, page, visible_databases: this.watching.visible_databases, pending_scopes: this.store.pendingScopes() });
  }

  // flush sends the oldest unsent action when nothing is in flight. One
  // action is in flight at a time so a retryable refusal of n never leaves
  // a gap before n+1 (TECH 9).
  flush() {
    if (!this.open || this.inflight || this.retryTimer != null || this.stopped || this.access !== 'private') return;
    const a = this.store.pending.actions[0];
    if (!a || a.held) return;
    let frames;
    try {
      frames = actionFrames(a);
    } catch (err) {
      if (!(err instanceof TooLargeError)) throw err;
      this.stop('too_large', { n: a.n });
      return;
    }
    a.sent = true;
    this.inflight = a.n;
    for (const f of frames) this.ws.send(f);
    this.h.status?.();
  }

  presence(msg) {
    if (!this.open || this.access !== 'private') return;
    this.presenceNext = msg;
    const wait = this.presenceAt + PRESENCE_GAP - this.now();
    if (wait > 0) {
      if (!this.presenceTimer) this.presenceTimer = this.timers.setTimeout(() => { this.presenceTimer = null; this.presence(this.presenceNext); }, wait);
      return;
    }
    this.presenceAt = this.now();
    this.#send({ ...msg, type: 'presence' });
  }

  receive(m) {
    switch (m.type ?? m.t) {
      case 'welcome': return this.#welcome(m);
      case 'snapshot': return this.#snapshot(m);
      case 'delta': return this.#delta(m);
      case 'delta_part': return this.#deltaPart(m);
      case 'resync': return this.#resync(m);
      case 'ack': return this.#ack(m);
      case 'reject': return this.#reject(m);
      case 'fatal':
        if (m.code === 'session_expired' && !this.store.pending.size) {
          // The server refuses unknown tokens even when there is nothing to replay.
          this.session = null;
          this.sessionId = null;
          this.store.pending.nextN = 1n;
          this.lastSeq = null;
          this.spill = null;
          const ws = this.ws;
          ws.close();
          return this.#closed(ws);
        }
        return this.stop(m.code);
      case 'presence': return this.h.presence?.(m);
      case 'activity': return this.h.activity?.(m);
      case 'view_adjustment': return this.h.viewAdjustment?.(m);
    }
  }

  #send(m) { if (this.ws) this.ws.send(JSON.stringify(m)); }

  #welcome(m) {
    const resumed = this.session != null;
    if (resumed && m.session && m.session !== this.session) {
      // The server no longer knows this tab's session, so unconfirmed
      // actions cannot be replayed safely; never renumber them (rule 40).
      if (this.store.pending.size) { this.stop('session_expired'); return; }
      this.store.pending.nextN = 1n;
    }
    this.open = true;
    this.access = m.access;
    if (m.session) this.session = m.session;
    if (m.session_id) this.sessionId = m.session_id;
    // Go starts the hello subscription at generation 1 and ignores watches
    // at or below it. Legacy in-process fixtures start at generation 0.
    if (m.type === 'welcome') this.generation = Math.max(this.generation, 1);
    this.reconnectDelay = RETRY_MIN;
    this.inflight = null;
    this.h.welcome?.(m);
    // hello named the page; visible databases need their own watch.
    if (this.watching.page && this.watching.visible_databases.length) this.watch(this.watching.page, this.watching.visible_databases);
    this.flush();
    this.h.status?.();
  }

  #snapshot(m) {
    if (m.generation < this.generation) return;
    if (m.generation > this.generation) this.generation = m.generation;
    const key = [m.generation, JSON.stringify(m.scope), m.seq, m.window ?? ''].join('|');
    let a = this.snapParts.get(key);
    if (!a) { a = { parts: m.parts, got: new Map() }; this.snapParts.set(key, a); }
    a.got.set(m.part, m.data);
    if (m.scope.kind === 'database' && m.window != null) this.spill = { spill: m.spill, next_window: m.window };
    if (a.got.size < a.parts) return;
    this.snapParts.delete(key);
    const items = [], assets = {};
    const parts = Array.from({ length: a.parts }, (_, i) => a.got.get(i));
    // Spill parts split JSON text, including within an item or string value.
    // Older in-process fixtures provide one decoded object per part instead.
    const units = typeof parts[0] === 'string' ? [JSON.parse(parts.join(''))] : parts;
    for (const d of units) {
      items.push(...(d.items || []));
      Object.assign(assets, d.assets || {});
    }
    const final = m.window == null || !!m.final_window;
    this.store.canonical.install(m.scope, m.seq, items, { window: m.window ?? null, final, assets });
    if (m.scope.kind === 'database') this.spill = final ? null : { spill: m.spill, next_window: m.window + 1 };
    if (this.lastSeq == null) this.lastSeq = m.seq;
    this.h.changed?.();
  }

  #deltaPart(m) {
    if (m.generation < this.deltaFloor) return;
    const d = this.deltaParts;
    if (!d || d.generation !== m.generation || d.seq !== m.seq) this.deltaParts = { generation: m.generation, seq: m.seq, parts: m.parts, got: new Map() };
    this.deltaParts.got.set(m.part, m.data);
    if (this.deltaParts.got.size < this.deltaParts.parts) return;
    const { got, parts } = this.deltaParts;
    this.deltaParts = null;
    let text = '';
    for (let i = 0; i < parts; i++) text += got.get(i);
    this.#delta({ ...JSON.parse(text), generation: m.generation, seq: m.seq });
  }

  // #delta applies one complete delta. Sequence numbers, not generations,
  // decide duplicates and gaps: the stream is consecutive per connection.
  #delta(d) {
    if (this.lastSeq != null) {
      const next = BigInt(this.lastSeq) + 1n;
      const seq = BigInt(d.seq);
      if (seq < next) return; // duplicate
      if (seq > next) { this.#gap(); return; }
    }
    this.lastSeq = d.seq;
    const c = this.store.canonical;
    if (d.reset) c.reset();
    c.applyDelta(d);
    if (d.from && d.from.session_id === this.sessionId) {
      const action = this.store.pending.get(d.from.n);
      this.store.pending.markApplied(d.from.n);
      if (!d.reset && action) for (const op of action.ops) this.#confirmCreation(op, d.seq);
    }
    if (d.overwrote && d.overwrote.length) this.h.overwrote?.(d.overwrote, d);
    this.h.changed?.(d);
  }

  #resync(m) {
    this.generation = Math.max(this.generation, m.generation);
    this.deltaFloor = m.generation;
    this.snapParts.clear();
    this.deltaParts = null;
    this.lastSeq = m.seq;
  }

  // A sequence gap means a delta was lost; reconnecting resumes from the
  // watermark or captures fresh scopes.
  #gap() {
    this.lastSeq = null;
    if (this.ws) { const ws = this.ws; this.ws = null; ws.onclose = null; ws.close(); }
    this.#reset();
    this.start();
  }

  #confirmCreation(op, seq) {
    if (op.op !== 'create_block') return;
    const b = this.store.canonical.get(op.proposed_id);
    if (b && b.kind === 'block' && b.ref.inc === seq) this.h.confirmCreation?.(b.ref);
  }

  #ack(m) {
    if (this.inflight === m.n) this.inflight = null;
    const a = this.store.pending.remove(m.n);
    this.retryDelay = RETRY_MIN;
    if (a) {
      // Receipt hydration has no attributed delta; confirm before any ack observer renders.
      for (const g of m.outcomes || []) {
        if (g.status === 'applied' || g.status === 'adjusted') {
          for (const i of g.indexes) this.#confirmCreation(a.ops[i], m.seq);
        }
      }
      this.timers.clearTimeout(this.retryTimer);
      this.retryTimer = null;
      this.h.ack?.(a, m);
    }
    this.h.changed?.();
    this.flush();
    this.h.status?.();
  }

  #reject(m) {
    const a = m.n != null ? this.store.pending.get(m.n) : null;
    if (m.n != null && this.inflight === m.n) this.inflight = null;
    if (m.retry) {
      // A retryable admission failure does not consume n: resend the same
      // action, and everything later waits behind it.
      this.timers.clearTimeout(this.retryTimer);
      this.retryTimer = this.timers.setTimeout(() => { this.retryTimer = null; this.flush(); }, this.retryDelay);
      this.retryDelay = Math.min(this.retryDelay * 2, RETRY_MAX);
      this.h.status?.();
      return;
    }
    if (m.code === 'expired' || m.code === 'conflict' || m.code === 'session') {
      this.stop(m.code, m.detail);
      return;
    }
    // A permanent refusal consumes n; the action's optimistic display is
    // removed and its text offered back.
    if (a) { this.store.pending.remove(a.n); this.h.reject?.(a, m); }
    this.h.changed?.();
    this.flush();
    this.h.status?.();
  }

  #reset() {
    this.open = false;
    this.inflight = null;
    this.generation = 0;
    this.deltaFloor = 0;
    this.snapParts.clear();
    this.deltaParts = null;
    this.timers.clearTimeout(this.retryTimer);
    this.retryTimer = null;
  }

  #closed(ws) {
    if (ws !== this.ws) return;
    this.ws = null;
    this.#reset();
    this.h.status?.();
    if (this.stopped) return;
    this.reconnectTimer = this.timers.setTimeout(() => this.start(), this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, RETRY_MAX);
  }
}
