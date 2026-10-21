// Live connection (TECH 4.5 to 4.8 and 6): hello with session, watermark and
// pending; ack and reject retry rules; ops_part splitting; snapshot and
// delta_part assembly by generation; resync; fatal; reconnect backoff.

export const FRAME_MAX = 256 * 1024;      // client-to-server frame, after encoding
export const PART_DATA_MAX = 192 * 1024;  // raw bytes of one ops_part data slice
export const PARTS_MAX = 32;
export const BATCH_MAX = 8 * 1024 * 1024;

export class TooLargeError extends Error {}

export function utf8Length(s) {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) { n += 4; i++; }
    else n += 3;
  }
  return n;
}

// charCost returns, for the code unit(s) at i, how many code units they take
// and their UTF-8 size raw and inside a JSON string literal.
function charCost(s, i) {
  const c = s.charCodeAt(i);
  if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
    const d = s.charCodeAt(i + 1);
    if (d >= 0xdc00 && d <= 0xdfff) return [2, 4, 4];
  }
  if (c >= 0xd800 && c <= 0xdfff) return [1, 3, 6]; // lone surrogate: \\udxxx
  if (c === 0x22 || c === 0x5c) return [1, 1, 2];
  if (c === 0x08 || c === 0x09 || c === 0x0a || c === 0x0c || c === 0x0d) return [1, 1, 2];
  if (c < 0x20) return [1, 1, 6];
  if (c < 0x80) return [1, 1, 1];
  if (c < 0x800) return [1, 2, 2];
  return [1, 3, 3];
}

// batchFrames encodes a batch as one ops frame, or as ops_part frames when
// the encoded ops frame exceeds 256 KiB. The parts carry slices of the
// serialized ops array; each whole part frame stays within FRAME_MAX after
// encoding and each slice within PART_DATA_MAX raw bytes.
export function batchFrames(n, ops) {
  const whole = JSON.stringify({ t: 'ops', n, ops });
  if (utf8Length(whole) <= FRAME_MAX) return [whole];
  const data = JSON.stringify(ops);
  if (utf8Length(data) > BATCH_MAX) throw new TooLargeError('batch over 8 MiB');
  const overhead = utf8Length(JSON.stringify({ t: 'ops_part', n, part: PARTS_MAX, parts: PARTS_MAX, data: '' }));
  const budget = FRAME_MAX - overhead;
  const slices = [];
  let start = 0, raw = 0, enc = 0;
  for (let i = 0; i < data.length;) {
    const [units, r, e] = charCost(data, i);
    if (raw + r > PART_DATA_MAX || enc + e > budget) {
      slices.push(data.slice(start, i));
      start = i; raw = 0; enc = 0;
    }
    raw += r; enc += e; i += units;
  }
  slices.push(data.slice(start));
  if (slices.length > PARTS_MAX) throw new TooLargeError('batch needs more than 32 parts');
  return slices.map((s, i) => JSON.stringify({ t: 'ops_part', n, part: i + 1, parts: slices.length, data: s }));
}

// tryBatchFrames is batchFrames, or null when ops cannot be sent at all.
function tryBatchFrames(n, ops) {
  try {
    return batchFrames(n, ops);
  } catch (err) {
    if (err instanceof TooLargeError) return null;
    throw err;
  }
}

// batchFits reports whether ops can be sent as batch n.
export const batchFits = (n, ops) => tryBatchFrames(n, ops) != null;

// queueBatch gives ops the next batch number and queues it, or returns null
// without touching state when the batch cannot be sent at all: the check
// comes before the batch takes a number, so an oversized action never
// consumes one (TECH 4.8, n == max_n + 1). The batch is serialized once;
// the frames are kept on it with the number they encode, and flush encodes
// again only if the batch was renumbered since.
export function queueBatch(state, ops) {
  const n = state.nextN;
  const frames = tryBatchFrames(n, ops);
  if (!frames) return null;
  const r = state.push(ops);
  if (r.batch.n === n) r.batch.encoded = { n, ops: r.batch.ops, frames };
  return r;
}

// framesOf returns the frames for batch b under its current number. The
// cache also checks the ops array, so a batch whose ops are ever replaced is
// encoded again.
function framesOf(b) {
  if (b.encoded && b.encoded.n === b.n && b.encoded.ops === b.ops) return b.encoded.frames;
  b.encoded = null;
  return batchFrames(b.n, b.ops);
}

// Parts collects numbered parts (part/parts) of one message.
export class Parts {
  constructor(parts) { this.parts = parts; this.got = new Map(); }
  add(part, value) { this.got.set(part, value); return this.got.size === this.parts; }
  ordered() { return [...this.got.entries()].sort((a, b) => a[0] - b[0]).map((e) => e[1]); }
}

const RETRY_MIN = 1000, RETRY_MAX = 30000;
// Presence is sent at most 20 times a second, 10 for a guest (TECH 3.4).
const PRES_GAP = 50, GUEST_PRES_GAP = 100;

export class Net {
  // h: {hello(), welcome(msg), snapshot(changed, snap), delta(result, d),
  //     ack(msg, batch, changed), reject(msg, batch, changed), fatal(code, msg),
  //     pres(msg), shared(msg), viewers(msg), level(msg), status(),
  //     tooLarge(batch, changed)}
  constructor(state, h) {
    this.state = state;
    this.h = h;
    this.ws = null;
    this.open = false;      // welcome received on the current socket
    this.welcomed = false;  // welcome received on any socket
    this.restarting = false; // an intentional reconnect awaits its welcome
    this.stopped = false;   // fatal: editing stopped for good
    this.gen = 0;
    this.snap = null;       // {gen, seq, parts: Parts, reset, meta}
    this.held = [];         // deltas that arrived while a snapshot was assembling
    this.deltaParts = null; // {gen, seq, parts: Parts}
    this.inflight = null;   // batch number awaiting ack or reject
    this.retryTimer = null;
    this.retryDelay = RETRY_MIN;
    this.reconnectDelay = RETRY_MIN;
    this.reconnectTimer = null;
    this.failures = 0;
    this.opens = 0;         // welcomes received, over all sockets
    this.lastPres = 0;
    this.presTimer = null;
    this.presNext = null;
    this.presGap = PRES_GAP;
  }

  get connected() { return this.open; }

  // connecting reports a socket that is not open yet but has not failed
  // either: the first connection, or an intentional reconnect (TECH 11.3).
  get connecting() { return !this.open && !this.stopped && (!this.welcomed || this.restarting); }

  start() {
    if (this.stopped) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/_board/ws`);
    this.ws = ws;
    ws.onopen = () => ws.send(JSON.stringify(this.h.hello()));
    ws.onmessage = (ev) => this.#message(ev.data);
    ws.onclose = () => this.#closed(ws);
  }

  // restart reconnects at once, for example after a gap or a name change.
  restart() {
    if (this.ws) { const ws = this.ws; this.ws = null; ws.onclose = null; ws.close(); }
    // Generation, assembly and in-flight state belong to the old socket,
    // also when a fatal code already closed it.
    this.#reset();
    this.restarting = true;
    this.start();
    this.h.status();
  }

  // resume reconnects after a fatal code the visitor can act on: joining
  // again with another name, watching, or trying a full board again.
  resume() {
    this.stopped = false;
    this.restart();
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.retryTimer);
    if (this.ws) { this.ws.onclose = null; this.ws.close(); this.ws = null; }
    this.open = false;
    this.h.status();
  }

  #reset() {
    this.open = false;
    this.gen = 0; // generations count per socket; a new socket starts at 0
    this.snap = null;
    this.held = [];
    this.deltaParts = null;
    this.inflight = null;
    // A transient retry delay belongs to the old socket; the new welcome
    // resends the oldest batch at once.
    clearTimeout(this.retryTimer);
    this.retryTimer = null;
  }

  #closed(ws) {
    if (ws !== this.ws) return;
    this.ws = null;
    this.restarting = false;
    this.#reset();
    this.failures++;
    this.h.status();
    if (this.stopped) return;
    const d = this.reconnectDelay * (0.8 + Math.random() * 0.4);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, RETRY_MAX);
    this.reconnectTimer = setTimeout(() => this.start(), d);
  }

  // withdrawn runs head, a request for the board page, while the
  // connection is down, and reports whether the board stopped being public
  // (TECH 11.3). A reconnect while the request runs proves the board is
  // still reachable, so a result that arrives after it is stale and never
  // reports a withdrawal for the recovered connection. A network error is
  // not a withdrawal: the status already says the connection is down.
  async withdrawn(head) {
    const opens = this.opens;
    let r;
    try {
      r = await head();
    } catch {
      return false;
    }
    return !r.ok && this.opens === opens && !this.open;
  }

  send(obj) {
    if (this.ws && this.open) this.ws.send(typeof obj === 'string' ? obj : JSON.stringify(obj));
  }

  // flush sends the oldest pending batch when nothing is in flight. Batches
  // go one at a time, so a transient refusal never leaves a later batch
  // ahead of an unconsumed number.
  flush() {
    if (!this.open || this.inflight != null || this.retryTimer || this.state.public) return;
    const b = this.state.pending[0];
    if (!b) return;
    let frames;
    try {
      frames = framesOf(b);
    } catch (err) {
      if (!(err instanceof TooLargeError)) throw err;
      // Never sent, so its number is not consumed: drop it and renumber the
      // later batches instead of leaving a gap the server would refuse.
      const { changed } = this.state.discard(b.n);
      this.h.tooLarge(b, changed);
      this.flush();
      return;
    }
    this.inflight = b.n;
    for (const f of frames) this.ws.send(f);
  }

  // presence sends at most 20 presence messages per second (10 for a
  // guest); the latest value wins while throttled.
  presence(p) {
    if (this.state.public) return;
    this.presNext = p;
    if (this.presTimer) return;
    const wait = Math.max(0, this.presGap - (Date.now() - this.lastPres));
    this.presTimer = setTimeout(() => {
      this.presTimer = null;
      this.lastPres = Date.now();
      if (this.presNext) this.send({ t: 'pres', ...this.presNext });
      this.presNext = null;
    }, wait);
  }

  share(ids) { this.send({ t: 'share', ids }); }

  #message(raw) {
    let m;
    try { m = JSON.parse(raw); } catch { return; }
    switch (m.t) {
      case 'welcome': return this.#welcome(m);
      case 'snapshot': return this.#snapshot(m);
      case 'delta': return this.#delta(m);
      case 'delta_part': return this.#deltaPart(m);
      case 'resync':
        this.gen = m.gen;
        this.deltaParts = null;
        this.snap = { gen: m.gen, seq: m.seq, parts: null };
        this.held = [];
        return;
      case 'ack': return this.#ack(m);
      case 'reject': return this.#reject(m);
      case 'pres': return this.h.pres(m);
      case 'shared': return this.h.shared(m);
      case 'viewers': return this.h.viewers(m);
      case 'level': return this.h.level(m);
      case 'fatal':
        this.stop();
        return this.h.fatal(m.code, m);
      default:
    }
  }

  #welcome(m) {
    const st = this.state;
    if (st.session !== m.session) {
      // A new session restarts batch numbers and the meaning of incarnation
      // 0. The server issues one only when no batch is pending (4.8).
      st.session = m.session;
      st.nextN = 1;
      st.created.clear();
      st.unacked.clear();
    }
    // Guests use the private wire form; only viewers get the public one.
    st.public = m.access === 'public';
    this.presGap = m.access === 'guest' ? GUEST_PRES_GAP : PRES_GAP;
    this.open = true;
    this.welcomed = true;
    this.opens++;
    this.restarting = false;
    this.failures = 0;
    this.reconnectDelay = RETRY_MIN;
    this.h.welcome(m);
    this.h.status();
    this.flush();
  }

  #snapshot(m) {
    if (m.gen < this.gen) return;
    this.gen = m.gen;
    if (!this.snap || !this.snap.parts || this.snap.gen !== m.gen || this.snap.seq !== m.seq) {
      this.snap = { gen: m.gen, seq: m.seq, reset: false, meta: null, parts: new Parts(m.parts) };
    }
    const s = this.snap;
    if (m.reset) s.reset = true;
    if (m.meta) s.meta = m.meta;
    if (!s.parts.add(m.part, m.elements)) return;
    this.snap = null;
    const snap = { seq: s.seq, reset: s.reset, meta: s.meta, elements: s.parts.ordered().flat() };
    const changed = this.state.loadSnapshot(snap);
    this.h.snapshot(changed, snap);
    const held = this.held;
    this.held = [];
    for (const d of held) this.#delta(d);
  }

  #delta(d) {
    if (d.gen != null && d.gen < this.gen) return;
    if (this.snap) { this.held.push(d); return; }
    const r = this.state.applyDelta(d);
    if (r.status === 'gap') { this.restart(); return; }
    if (r.status === 'applied') this.h.delta(r, d);
  }

  #deltaPart(m) {
    if (m.gen < this.gen) return;
    if (!this.deltaParts || this.deltaParts.gen !== m.gen || this.deltaParts.seq !== m.seq) {
      this.deltaParts = { gen: m.gen, seq: m.seq, parts: new Parts(m.parts) };
    }
    if (!this.deltaParts.parts.add(m.part, m.data)) return;
    const text = this.deltaParts.parts.ordered().join('');
    this.deltaParts = null;
    let d;
    try { d = JSON.parse(text); } catch { this.restart(); return; }
    d.gen = m.gen;
    this.#delta(d);
  }

  #ack(m) {
    if (m.n === this.inflight) this.inflight = null;
    this.retryDelay = RETRY_MIN;
    const { batch, changed } = this.state.ack(m.n);
    this.h.ack(m, batch, changed);
    this.h.status();
    this.flush();
  }

  #reject(m) {
    if (m.n === this.inflight) this.inflight = null;
    const { batch, changed } = this.state.reject(m.n, m.retry);
    this.h.reject(m, batch, changed);
    if (m.retry) {
      const d = this.retryDelay;
      this.retryDelay = Math.min(this.retryDelay * 2, RETRY_MAX);
      this.retryTimer = setTimeout(() => { this.retryTimer = null; this.flush(); }, d);
    } else {
      this.flush();
    }
    this.h.status();
  }
}
