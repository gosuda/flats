// Live connection (TECH 12, Appendix A): hello and welcome, watch, snapshot
// frames by generation, delta and delta_part assembly, sequence gaps,
// action numbering with the pending queue and its exact resend, acks, peek
// and rebase requests, presence pacing and reconnect backoff. The transport
// is injectable so tests can play the server.
'use strict';

import { FRAME_MAX, VERSION, TooLargeError, ackError, actionFrames, actionShapeError, deltaError, deltaWhole, frame, frameType, joinDeltaPart, joinHead, nullError, overwrittenError, resyncError, seqCmp, seqNext, snapshotFrameError, utf8Length, welcomeError } from './protocol.js';

const RETRY_MIN = 1000;
const RETRY_MAX = 30000;
// How often one action is answered "unavailable" before the page says that
// the host has been busy for a while. The action stays in the queue and is
// asked again after that, as before: only the words change.
const BUSY_TRIES = 4;
// The server drops a presence update above 20 a second without an answer,
// and a dropped one could be the last of a move. The client stays well
// under that rate, so none is dropped.
const PRESENCE_GAP = 60;
// The frames that carry the state of the grid: the welcome, which names
// the tab list and the place of the stream, a snapshot, the deltas and a
// resync, which names the generation of the next snapshot and the tab
// list.
const STATE_FRAMES = new Set(['welcome', 'tab_head', 'tab_more', 'rows', 'tab_done', 'delta', 'delta_part', 'resync']);
const SNAPSHOT_FRAMES = new Set(['tab_head', 'tab_more', 'rows', 'tab_done']);

function defaultConnect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return new WebSocket(`${proto}//${location.host}/_sheet/ws`);
}

// Net drives one browser tab's connection. Handlers (all optional):
// resumable(), welcome(msg, {resumed, replayed}), tabHead(msg), rows(msg), tabDone(msg),
// delta(msg, size) with size() the encoded bytes of the delta, resync(msg), reset(msg), ack(action, msg), peeked(msg),
// rebased(msg), presence(msg), overwritten(msg), calculating(bool),
// behind() when the grid the owner holds is no state of the stream,
// stopped(code, detail), status().
//
// welcome, tabHead, rows, tabDone, delta and resync carry the state of the
// grid. A frame of one of them that this Net refuses, or whose handler
// throws, has not been taken. So has text that is no frame, and a delta
// that does not follow the one before it. The owner is told at once
// (behind), the connection ends as lost() says, and an error goes on to
// the caller.
export class Net {
  constructor(handlers = {}, { connect = defaultConnect, timers = globalThis, now = () => Date.now() } = {}) {
    this.h = handlers;
    this.connect = connect;
    this.timers = timers;
    this.now = now;
    this.ws = null;
    this.open = false;
    this.stopped = null;   // {code, detail} once editing stopped for good
    this.access = null;
    this.session = null;   // resume token, in memory only
    this.sessionId = null;
    this.me = null;
    this.name = null;      // label for routes without a verified identity
    this.tab = null;       // watched tab id
    this.generation = 0;   // newest watch or resync generation on this socket
    // lastSeq is the place of the stream. It is the sequence of the last
    // delta the owner took, or of the snapshot or welcome it goes on from.
    this.lastSeq = null;
    this.resuming = false; // hello asked to resume the watched tab from lastSeq
    this.pending = [];     // unconfirmed actions in order: {n, base, ops, sent, meta}
    this.nextN = 1;
    this.nextId = 1;       // peek and rebase request ids
    this.deltaParts = null; // {seq, parts, next, delta, bytes}: a delta whose parts are arriving
    this.head = null;      // a tab_head whose tab_more frames are still arriving
    this.rowParts = null;  // rows frames taken of the snapshot whose head came last
    this.rowGeneration = null; // the generation of that snapshot
    this.reconnectTimer = null;
    this.reconnectDelay = RETRY_MIN;
    this.failures = 0;     // sockets closed in a row since the last welcome
    this.busy = false;     // the last connection ended because the host could not take an action now
    this.single = false;   // actions go out one at a time: the host is or was busy
    this.presenceAt = Number.NEGATIVE_INFINITY;
    this.presenceTimer = null;
    this.presenceNext = null;
  }

  get status() {
    if (this.stopped) return 'stopped';
    if (!this.open) return 'offline';
    return this.pending.length ? 'saving' : 'saved';
  }

  // busyLong says that an action has been answered "unavailable" BUSY_TRIES
  // times or more and is still not answered for good.
  get busyLong() { return this.pending.some((a) => (a.refusals || 0) >= BUSY_TRIES); }

  get writable() { return this.open && this.access === 'private' && !this.stopped; }

  // start opens a socket. The page can ask for one while an earlier one is
  // still opening or waits for its welcome ("Retry now", "Ask now"). That
  // one is closed first, so the Net never has two. A handler acts only
  // while its socket is the current one. A socket that was left sends no
  // hello, and its frames and its end are passed over.
  start() {
    if (this.stopped) return;
    this.timers.clearTimeout(this.reconnectTimer);
    this.#drop();
    const ws = this.connect();
    this.ws = ws;
    ws.onopen = () => { if (ws === this.ws) ws.send(JSON.stringify(this.hello())); };
    ws.onmessage = (ev) => this.#frame(ws, ev.data);
    ws.onclose = () => this.#closed(ws);
  }

  stop(code, detail = {}) {
    this.stopped = { code, detail };
    this.timers.clearTimeout(this.reconnectTimer);
    this.#drop();
    this.h.stopped?.(code, detail);
    this.h.status?.();
  }

  // restart opens a new socket now: after the person chose a name, and when
  // the connection must start from a new snapshot. A fresh start tells the
  // owner at once that its grid is behind: what it shows may be no state
  // of the next stream, and no edit may be queued against it meanwhile.
  restart({ fresh = false } = {}) {
    if (this.stopped) return;
    if (fresh) {
      this.lastSeq = null;
      this.h.behind?.();
    }
    this.start();
  }

  // lost ends the connection because the owner could not take a frame it
  // carried, so what the owner holds is no state of the stream. Nothing is
  // resumed, and the next connection starts from a new snapshot. It opens
  // after the wait a closed connection has, so a frame that fails every
  // time is not asked for in a tight loop.
  lost() {
    this.lastSeq = null;
    const ws = this.ws;
    if (!ws || this.stopped) return;
    ws.close();
    this.#closed(ws);
  }

  // rename gives the server the name the person chose. A session keeps the
  // label it was opened with, so the name opens a new session. It has
  // nothing to carry over: a route that asks for a name accepts no edit
  // before it has one.
  rename(name) {
    this.name = name;
    this.session = null;
    this.sessionId = null;
    this.nextN = 1;
    this.restart({ fresh: true });
  }

  // hello resumes the watched tab from the last received sequence when
  // there is one and the owner's grid can go on from it (the resumable
  // handler; a grid that waits for a new snapshot cannot). Without a
  // watermark the server starts over and the client watches its tab after
  // the welcome.
  hello() {
    const m = frame('hello', { version: VERSION, pending: this.pending.length });
    if (this.session) m.session = this.session;
    this.resuming = this.session != null && this.lastSeq != null && this.tab != null && (this.h.resumable ? this.h.resumable() : true);
    if (this.resuming) { m.watermark = this.lastSeq; m.tab = this.tab; }
    if (this.name) m.name = this.name;
    return m;
  }

  // watch asks for a snapshot of tab under a new generation, which makes
  // frames of older generations stale.
  watch(tab) {
    this.tab = tab;
    if (!this.open) return;
    this.generation++;
    this.#send(frame('watch', { generation: this.generation, tab }));
  }

  // queue numbers an action and puts it in the pending queue; flush sends
  // it. base is the sequence the screen had applied when the person acted;
  // meta stays local for notices.
  queue(ops, base, meta = {}) {
    // Every action goes through here. One the host would refuse for its
    // shape is a mistake of the caller and is never sent.
    const wrong = actionShapeError(ops);
    if (wrong) throw new Error(`not an action: ${wrong}`);
    if (this.stopped || this.access !== 'private') return null;
    const a = { n: String(this.nextN++), base, ops, sent: false, meta };
    this.pending.push(a);
    this.h.status?.();
    return a;
  }

  // fits says whether the next action queued can be sent if it holds ops
  // on base. Its frames are the ones flush makes for it on every
  // connection.
  fits(ops, base) {
    try {
      actionFrames({ n: String(this.nextN), base, ops });
    } catch (err) {
      if (!(err instanceof TooLargeError)) throw err;
      return false;
    }
    return true;
  }

  // flush sends every action that this socket has not carried yet, in
  // order. After a reconnect that is every unconfirmed action, with its
  // original n, base and content (rule 39). After the host answered
  // "unavailable" only one action is on its way at a time: the next goes
  // out when that one is answered.
  flush() {
    if (!this.writable) return;
    // An answer can shorten the queue while it is walked.
    for (const a of [...this.pending]) {
      if (a.sent && this.single) return;
      if (a.sent) continue;
      let frames;
      try {
        frames = actionFrames(a);
      } catch (err) {
        if (!(err instanceof TooLargeError)) throw err;
        // The page asks fits before it queues an action, so only a caller
        // that did not ask comes here.
        this.stop('too_large', { n: a.n });
        return;
      }
      a.sent = true;
      for (const f of frames) this.ws.send(f);
      if (this.single) return;
    }
  }

  // requestId numbers a peek or a rebase, so the caller can file the
  // request before its answer can arrive.
  requestId() { return this.nextId++; }

  peek(id, tab, row, col) {
    if (!this.open) return false;
    this.#send(frame('peek', { id, tab, row, col }));
    return true;
  }

  // rebaseFits says whether a rebase of these fields is a frame the host
  // reads, whichever id it gets.
  rebaseFits(fields) { return utf8Length(JSON.stringify(frame('rebase', { id: Number.MAX_SAFE_INTEGER, ...fields }))) <= FRAME_MAX; }

  rebase(id, fields) {
    if (!this.writable) return false;
    this.#send(frame('rebase', { id, ...fields }));
    return true;
  }

  presence(msg) {
    if (!this.writable) return;
    this.presenceNext = msg;
    const wait = this.presenceAt + PRESENCE_GAP - this.now();
    if (wait > 0) {
      if (!this.presenceTimer) this.presenceTimer = this.timers.setTimeout(() => { this.presenceTimer = null; this.presence(this.presenceNext); }, wait);
      return;
    }
    this.presenceAt = this.now();
    this.#send(frame('presence', msg));
  }

  // #frame takes the text of one frame from socket ws. A socket this Net
  // has left can still deliver what it had received, and none of that is
  // taken.
  #frame(ws, text) {
    if (ws !== this.ws) return;
    let m;
    try {
      m = JSON.parse(text);
    } catch (err) {
      this.#untaken(ws);
      throw err;
    }
    this.receive(m, text);
  }

  // #untaken ends the connection for a frame of socket ws that was not
  // taken: text that is no frame, which could have been any frame of the
  // grid, or a frame of the grid that was refused. The owner marks its
  // grid behind at once, so no edit starts on it before a new snapshot,
  // and the connection ends as lost() says (TECH 12.5).
  #untaken(ws) {
    this.h.behind?.();
    if (ws === this.ws) this.lost();
  }

  // receive handles one server frame. text is the frame as it arrived; a
  // caller without it leaves the size of a delta to be worked out.
  receive(m, text = null) {
    const ws = this.ws;
    let type;
    try {
      type = frameType(m);
    } catch (err) {
      this.#untaken(ws);
      throw err;
    }
    if (!STATE_FRAMES.has(type)) return this.#dispatch(type, m, text);
    try {
      // A frame of the grid that holds null where Appendix A allows none
      // does not fit, whichever kind it is and at whatever depth the null
      // stands (TECH 12.5). A delta is read in the fields the page acts on
      // before any of it is taken.
      const wrong = type === 'delta' || type === 'delta_part' ? deltaError(m) : nullError(m) ?? (SNAPSHOT_FRAMES.has(type) ? snapshotFrameError(m) : null);
      if (wrong) throw new TypeError(wrong);
      return this.#dispatch(type, m, text);
    } catch (err) {
      this.#untaken(ws);
      throw err;
    }
  }

  #dispatch(type, m, text) {
    switch (type) {
      case 'welcome': return this.#welcome(m);
      case 'status':
        if (typeof m.calculating !== 'boolean') throw new TypeError('a status without a calculating mark');
        return this.h.calculating?.(m.calculating);
      case 'tab_head': return this.#tabHead(m);
      case 'tab_more': return this.#tabMore(m);
      case 'rows': return this.#rows(m);
      case 'tab_done': return this.#tabDone(m);
      case 'delta':
        if (this.deltaParts) throw new TypeError(`delta ${m.seq} inside the parts of ${this.deltaParts.seq}`);
        return this.#delta(m, () => utf8Length(text ?? JSON.stringify(m)));
      case 'delta_part': return this.#deltaPart(m, text);
      case 'resync': return this.#resync(m);
      case 'reset': return this.#reset(m);
      case 'ack': return this.#ack(m);
      case 'overwritten': return this.#shown(m, this.h.overwritten, overwrittenError);
      case 'presence': return this.#shown(m, this.h.presence);
      case 'peeked': return this.h.peeked?.(m);
      case 'rebased': return this.h.rebased?.(m);
      case 'fatal': return this.#fatal(m);
    }
  }

  // #shown hands a frame that the page only shows to its handler: the
  // notice of an overwritten cell, or the people of the sheet. One that
  // holds a null, or that check finds does not fit, is not shown at all.
  #shown(m, handler, check = nullError) {
    const wrong = check(m);
    if (wrong) throw new TypeError(wrong);
    return handler?.call(this.h, m);
  }

  #send(m) { if (this.ws) this.ws.send(JSON.stringify(m)); }

  #welcome(m) {
    const wrong = welcomeError(m);
    if (wrong) throw new TypeError(wrong);
    // A resumed stream goes on from the watermark. The welcome of such a
    // stream carries no tab list and no date, since the host has them only
    // as they are now. The page keeps its own, which are those of the
    // watermark, and the replayed deltas bring each change in its place
    // (TECH 12.5). When the server can no longer replay from there, its
    // welcome names the current sequence with the tab list and the date of
    // that sequence, and a snapshot of the watched tab follows unasked.
    const replayed = m.tabs === undefined;
    if (replayed && !(this.resuming && m.access === 'private' && m.seq === this.lastSeq)) {
      throw new TypeError(`a welcome without a tab list at ${m.seq}, which resumes no stream of this page`);
    }
    const known = this.session != null;
    if (known && m.session !== undefined && m.session !== this.session) {
      // The server no longer knows this tab's session, so unconfirmed
      // actions cannot be replayed safely; never renumber them (rule 40).
      if (this.pending.length) { this.stop('session_expired'); return; }
      this.nextN = 1;
      this.resuming = false;
    }
    this.open = true;
    this.access = m.access;
    if (m.session !== undefined) this.session = m.session;
    if (m.session_id !== undefined) this.sessionId = m.session_id;
    this.me = m.me || null;
    if (m.access !== 'private') this.resuming = false; // a Public connection always gets a new snapshot
    if (!replayed) this.lastSeq = m.seq;
    // While an action is tried again on a busy host the wait keeps growing.
    if (!this.single) this.reconnectDelay = RETRY_MIN;
    this.failures = 0;
    this.busy = false;
    this.h.welcome?.(m, { resumed: this.resuming, replayed });
    this.flush();
    this.h.status?.();
  }

  // A tab_head starts a snapshot: the answer to a watch, or the server's
  // own after a resync or a resume it could not replay. Deltas up to its
  // sequence are part of it and never arrive.
  #tabHead(m) {
    if (m.generation < this.generation) {
      if (this.head === m) this.head = null;
      return;
    }
    // A head larger than one frame counts once its tab_more frames are in.
    this.head = m.more ? m : null;
    if (m.more) return;
    this.generation = m.generation;
    this.tab = m.tab;
    if (this.lastSeq == null || seqCmp(m.seq, this.lastSeq) > 0) this.lastSeq = m.seq;
    this.rowParts = 0;
    this.rowGeneration = m.generation;
    this.h.tabHead?.(m);
  }

  // A tab_more continues the head that came last. One of the same
  // generation that names another tab could be any part of that head.
  // One of an older generation belongs to a snapshot the page has left. Any
  // other continues no head the page has, and the snapshot it is part of
  // would never be whole.
  #tabMore(m) {
    const head = this.head;
    if (!head || head.generation !== m.generation) {
      if (m.generation < this.generation) return;
      throw new TypeError(`a tab_more of generation ${m.generation} that continues no head`);
    }
    if (head.tab !== m.tab) throw new TypeError(`a tab_more of tab ${m.tab} for the head of tab ${head.tab}`);
    this.#tabHead(joinHead(head, m));
  }

  // The rows frames of a snapshot are numbered from 0, in order, and its
  // tab_done counts them. A frame of the snapshot that went missing, or
  // came in another place, is found before the snapshot counts as whole.
  // Only a frame of an older generation than the newest one is passed
  // over. Any other continues the snapshot whose head came last. When that
  // head is not whole, or is of another generation, or its snapshot has
  // ended, the snapshot the frame belongs to would never end.
  #continues(m) {
    if (m.generation < this.generation) return false;
    if (this.head || this.rowParts == null || m.generation !== this.rowGeneration) {
      throw new TypeError(`a ${m.type} of generation ${m.generation} that continues no snapshot under way`);
    }
    return true;
  }

  #rows(m) {
    if (!this.#continues(m)) return;
    if (m.tab !== this.tab || m.part !== this.rowParts) throw new TypeError(`rows part ${m.part} of tab ${m.tab} where part ${this.rowParts} of tab ${this.tab} is due`);
    this.rowParts++;
    this.h.rows?.(m);
  }

  #tabDone(m) {
    if (!this.#continues(m)) return;
    if (m.tab !== this.tab || m.parts !== this.rowParts) throw new TypeError(`a snapshot of tab ${m.tab} that ends after ${m.parts} parts where ${this.rowParts} came`);
    this.rowParts = null;
    this.h.tabDone?.(m);
  }

  // delta_part frames carry the fields of one delta, parts numbered from
  // 0, each a frame of its own (TECH 12). They are joined as values, so no
  // text of the whole delta is built, and the delta is taken with its last
  // part. A part whose number or count is not a whole number of parts, and
  // parts that do not follow one another in order, end the connection as a
  // frame that could not be read does, before anything is taken.
  #deltaPart(m, text) {
    const d = this.deltaParts;
    const counted = Number.isInteger(m.parts) && m.parts > 0 && Number.isInteger(m.part) && m.part >= 0 && m.part < m.parts;
    const first = m.part === 0;
    if (!counted || (first ? d != null : (!d || d.seq !== m.seq || d.parts !== m.parts || d.next !== m.part))) {
      throw new TypeError(`delta part ${m.part} of ${m.parts} for ${m.seq} does not follow`);
    }
    const asm = first ? { seq: m.seq, parts: m.parts, next: 0, delta: { type: 'delta', seq: m.seq }, bytes: 0 } : d;
    this.deltaParts = asm;
    joinDeltaPart(asm.delta, m);
    asm.bytes += utf8Length(text ?? JSON.stringify(m));
    if (++asm.next < asm.parts) return;
    this.deltaParts = null;
    if (!deltaWhole(asm.delta)) throw new TypeError(`delta ${m.seq} ends inside an edit`);
    this.#delta(asm.delta, () => asm.bytes);
  }

  // The stream is consecutive per connection: a lower sequence is a
  // duplicate, and a gap means a delta was lost. The stream moves to a
  // delta once the owner took it, so a delta the owner could not take is
  // never behind the place a hello resumes from. An owner that ended the
  // connection while it had the delta, or took a snapshot of a later
  // sequence, has set the place itself.
  #delta(d, size) {
    if (this.lastSeq != null) {
      const next = seqNext(this.lastSeq);
      const c = seqCmp(d.seq, next);
      if (c < 0) return;
      if (c > 0) { this.restart({ fresh: true }); return; }
    }
    const ws = this.ws;
    const from = this.lastSeq;
    this.h.delta?.(d, size);
    if (this.ws === ws && this.lastSeq === from) this.lastSeq = d.seq;
  }

  #resync(m) {
    const wrong = resyncError(m);
    if (wrong) throw new TypeError(wrong);
    this.generation = Math.max(this.generation, m.generation);
    this.deltaParts = null;
    this.lastSeq = m.seq;
    this.h.resync?.(m);
  }

  // After a rollback every tab, row and column may differ, the tab list
  // included, so the connection starts over with a new welcome and a new
  // snapshot. The owner's reset handler marks its grid behind. Unconfirmed
  // actions are sent again as they are.
  #reset(m) {
    this.h.reset?.(m);
    this.restart({ fresh: true });
  }

  // An ack that does not fit its action says nothing certain about it.
  // The action stays queued, the connection ends and the next one sends
  // it again with its number, which the host answers from its receipt. An
  // ack is read before it is matched. One whose number does not fit could
  // answer any action in flight, so it ends the connection as well, and
  // every action on it is sent again.
  #ack(m) {
    const i = this.pending.findIndex((a) => a.n === m.n);
    const wrong = ackError(m, i < 0 ? null : this.pending[i].ops.length);
    if (wrong) {
      const ws = this.ws;
      if (ws) { ws.close(); this.#closed(ws); }
      throw new TypeError(`${wrong} (action ${JSON.stringify(m.n)})`);
    }
    if (i < 0) return;
    const [a] = this.pending.splice(i, 1);
    this.#answered();
    this.h.ack?.(a, m);
    this.h.status?.();
  }

  // #answered runs when an action left the queue: the next one goes out,
  // and with an empty queue sending returns to its usual pace.
  #answered() {
    if (!this.single) return;
    if (this.pending.length) { this.flush(); return; }
    this.single = false;
    this.reconnectDelay = RETRY_MIN;
  }

  // #fatal ends the connection. "unavailable" means the host could not
  // take something now: it is calculating, it stopped the sheet after idle
  // time and opens it again on the next connection, its writer is busy, or
  // it could not write its storage. It says nothing certain about the
  // actions in flight. One of them may have been committed with its ack
  // lost, or overtaken by this frame; "unavailable" can also be the answer
  // to asking again for an action that was committed before. So the client
  // never makes up an outcome: every unanswered action stays in the queue
  // and is sent again with its number, until an ack says what became of it
  // or the host ends the session for good (which stops editing and offers
  // the unconfirmed text for copying). The host keeps a receipt for each
  // number, so an action applies once however often it is sent.
  //
  // The waits between connections double up to RETRY_MAX and start over
  // when the queue is empty, so a host that stays busy is asked more and
  // more slowly. Actions then go out one at a time, in order, which keeps
  // the load on a busy host small. The action that was alone in flight
  // counts the answer, for the words on the page (busyLong).
  #fatal(m) {
    if (m.code === 'unavailable') {
      const ws = this.ws;
      const sent = this.pending.filter((a) => a.sent);
      if (sent.length) this.single = true;
      if (sent.length === 1) sent[0].refusals = (sent[0].refusals || 0) + 1;
      this.busy = true;
      ws.close();
      return this.#closed(ws);
    }
    // A session the host let go with nothing pending is replaced by a new
    // one. The host may have been rolled back meanwhile without this page
    // hearing of it, so the grid is behind until the new snapshot.
    if (m.code === 'session_expired' && !this.pending.length) {
      this.session = null;
      this.sessionId = null;
      this.nextN = 1;
      return this.restart({ fresh: true });
    }
    return this.stop(m.code, m.message == null ? {} : { message: m.message });
  }

  #drop() {
    if (this.ws) { const ws = this.ws; this.ws = null; ws.onclose = null; ws.close(); }
    this.#offline();
  }

  #offline() {
    this.open = false;
    this.generation = 0;
    this.deltaParts = null;
    this.head = null;
    this.rowParts = null;
    this.rowGeneration = null;
    for (const a of this.pending) a.sent = false;
    this.timers.clearTimeout(this.presenceTimer);
    this.presenceTimer = null;
  }

  #closed(ws) {
    if (ws !== this.ws) return;
    this.ws = null;
    this.failures++;
    this.#offline();
    this.h.status?.();
    if (this.stopped) return;
    this.reconnectTimer = this.timers.setTimeout(() => this.start(), this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, RETRY_MAX);
  }
}
