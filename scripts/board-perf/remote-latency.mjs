// Remote display latency and concurrent-editing consistency (board.md
// success criteria, "Remote display" and "Concurrent editing").
//
//   node remote-latency.mjs --url http://<slug>.localhost:<port> --expected 5000 [--minutes 10] [--bots 5]
//        [--rate 2] [--zoom 0.8] [--owned 5] [--headless] [--inject-divergence] [--out result.json]
//
// An observer: Chrome opens the board at --zoom (default 80%) over the first
// frame's sticky grid.
// Load bots: --bots WebSocket clients in this Node process. Each runs the
// board's own client modules (internal/board/web/state.js and net.js: the
// same state machine, batching, ack and reconnect rules as the canvas) and
// edits --rate times per second. Each edit is one batch: it sets the text of
// a sticky to a unique token and moves it a few pixels. Every bot owns
// --owned (default 5) stickies in the observer's view; one edit in three instead goes to a pool
// of four stickies that every bot edits (text and color), so the server
// orders conflicting writes.
//
// Latency of one edit = the observer's display time minus the bot's send
// time, both as wall-clock milliseconds on this machine (performance.timeOrigin
// + performance.now() in Node and in the page):
// - send: the bot's WebSocket send of the batch frame.
// - display: the observer applies the delta carrying the token (its own
//   delta handler), the board's renderer draws the change in the next
//   animation frame, and display is the start of the frame after that one,
//   by which the drawn frame has been handed to the screen.
// The server's receipt time is not observable from outside the server, so
// the result is send-to-display: an upper bound of receipt-to-display that
// also contains the client-to-server hop (loopback here).
//
// After the load the bots stop, everything settles, and the check compares
// every element on every screen (each bot's client state, the observer's
// page state) with a fresh snapshot from the server, and checks that the
// last acknowledged write of each bundle is the one that stayed.

import { writeFileSync } from 'node:fs';
import { createHash, randomBytes } from 'node:crypto';
import { args, watchdog, launchChrome, waitReady, nameScript, stats, environment, uptime, sleep } from './lib.mjs';

const opt = args({ url: '', expected: 5000, minutes: 10, bots: 5, rate: 2, zoom: 0.8, owned: 5, width: 1440, height: 900, scale: 0, chrome: '', out: '' });
if (!opt.url) { console.error('usage: node remote-latency.mjs --url <board url> --expected <elements>'); process.exit(2); }
watchdog((opt.minutes * 60 + 600) * 1000);

const web = new URL('../../internal/board/web/', import.meta.url);
const origin = new URL(opt.url).origin;
globalThis.location = new URL(opt.url);
const nowMs = () => performance.timeOrigin + performance.now();

// sendLog records when each bot socket sent each batch number.
const NativeWS = globalThis.WebSocket;
class BotSocket extends NativeWS {
  constructor(url) {
    super(url, { headers: { Origin: origin } });
    this.bot = BotSocket.current;
  }
  send(data) {
    if (this.bot && typeof data === 'string') {
      const m = /^\{"t":"ops(?:_part)?","n":(\d+)/.exec(data);
      if (m && !this.bot.sentAt.has(Number(m[1]))) this.bot.sentAt.set(Number(m[1]), nowMs());
    }
    super.send(data);
  }
}
globalThis.WebSocket = BotSocket;
const { BoardState } = await import(new URL('state.js', web));
const { Net, queueBatch } = await import(new URL('net.js', web));

// canonical is JSON with sorted keys.
const canonical = (v) => JSON.stringify(v, (k, x) => (x && typeof x === 'object' && !Array.isArray(x) ? Object.fromEntries(Object.keys(x).sort().map((key) => [key, x[key]])) : x));
const RUNTIME = ['inc', 'v', 'end_inc', 'deleted', 'del_seq'];
const contentOf = (el) => Object.fromEntries(Object.entries(el).filter(([k]) => !RUNTIME.includes(k)));

class Bot {
  constructor(i, owned, shared) {
    this.i = i;
    this.name = `load bot ${i + 1}`;
    this.editor = 'perfbot' + i + randomBytes(6).toString('hex');
    this.owned = owned;
    this.shared = shared;
    this.state = new BoardState();
    this.sentAt = new Map(); // batch n -> send time
    this.batches = new Map(); // batch n -> {tokens, ops}
    this.acks = []; // {n, seq, results, at}
    this.rejects = [];
    this.fatal = null;
    this.welcomes = 0;
    this.k = 0;
    const self = this;
    this.net = new Net(this.state, {
      hello: () => {
        const st = self.state;
        const m = { t: 'hello', v: 1, pending: { count: st.pending.length }, editor: self.editor, name: self.name };
        if (st.session) { m.session = st.session; m.watermark = st.watermark; }
        if (st.pending.length) m.pending.first_n = st.pending[0].n;
        return m;
      },
      welcome: () => { self.welcomes++; },
      snapshot: () => {},
      delta: () => {},
      ack: (m) => { self.acks.push({ n: m.n, seq: m.seq, results: m.results, at: nowMs() }); },
      reject: (m) => { self.rejects.push({ n: m.n, code: m.code, retry: m.retry }); },
      fatal: (code) => { self.fatal = code; },
      pres: () => {}, shared: () => {}, status: () => {}, tooLarge: () => { self.rejects.push({ code: 'too_large' }); },
    });
  }

  // start connects. Net reconnects by calling this.net.start again, so the
  // wrapper tags every socket of this bot.
  start() {
    const own = this.net.start.bind(this.net);
    this.net.start = () => { BotSocket.current = this; try { own(); } finally { BotSocket.current = null; } };
    this.net.start();
  }
  get ready() { return this.net.connected && this.state.server.size > 0; }

  edit() {
    const st = this.state;
    const k = this.k++;
    const sharedTurn = k % 3 === 2;
    const id = sharedTurn ? this.shared[(k / 3 | 0) % this.shared.length] : this.owned[k % this.owned.length];
    const el = st.get(id);
    if (!el) return;
    const token = `b${this.i}#${k}`;
    const ops = [{ op: 'set', id, inc: st.incOf(id), g: 'text', val: `${token} edit from ${this.name}` }];
    if (sharedTurn) {
      ops.push({ op: 'set', id, inc: st.incOf(id), g: 'style.color', val: ['yellow', 'pink', 'blue', 'green', 'orange'][this.i % 5] });
    } else {
      const dx = (k % 2 ? 3 : -3);
      ops.push({ op: 'set', id, inc: st.incOf(id), g: 'geom', val: { x: Math.round(el.x) + dx, y: Math.round(el.y), w: el.w, h: el.h, rot: el.rot || 0 } });
    }
    const r = queueBatch(st, ops);
    if (!r) return;
    this.batches.set(r.batch.n, { token, ops });
    this.net.flush();
  }
}

// rawSnapshot reads the board's current state on a fresh connection.
function rawSnapshot() {
  return new Promise((resolve, reject) => {
    const ws = new NativeWS(opt.url.replace(/^http/, 'ws') + '/_board/ws', { headers: { Origin: origin } });
    const els = [];
    let meta = null;
    const t = setTimeout(() => { ws.close(); reject(new Error('snapshot timed out')); }, 60000);
    ws.onopen = () => ws.send(JSON.stringify({ t: 'hello', v: 1, pending: { count: 0 }, editor: 'perfcheck' + randomBytes(6).toString('hex'), name: 'consistency check' }));
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.t !== 'snapshot') return;
      els.push(...m.elements);
      if (m.meta) meta = m.meta;
      if (m.part === m.parts) { clearTimeout(t); ws.close(); resolve({ seq: m.seq, elements: els, meta }); }
    };
    ws.onerror = (e) => { clearTimeout(t); reject(new Error('snapshot socket: ' + (e.message || 'error'))); };
  });
}

// observerHook runs in the page: it records when each token's delta is
// applied and displayed.
const observerHook = `(() => {
  const b = window.board;
  const seen = window.__seen = [];
  const orig = b.onDelta;
  b.onDelta = function (r, d) {
    orig.call(this, r, d);
    const applied = performance.now();
    const tokens = [];
    for (const c of d.changes || []) {
      const t = c.set && c.set.text;
      if (!t) continue;
      const m = /^(b\\d+#\\d+) /.exec(Array.isArray(t) ? t[0] : t);
      if (m) tokens.push(m[1]);
    }
    if (!tokens.length) return;
    requestAnimationFrame(() => {
      const drawn = performance.now();
      const pending = b.renderer.raf !== 0;
      requestAnimationFrame((ts) => {
        for (const tok of tokens) seen.push([tok, performance.timeOrigin + applied, performance.timeOrigin + drawn, performance.timeOrigin + ts, d.seq, pending]);
      });
    });
  };
  return true;
})()`;

const before = uptime();
const chrome = await launchChrome({ headless: !!opt.headless, width: opt.width, height: opt.height, scale: opt.scale || 0, chrome: opt.chrome || undefined });
let result;
const bots = [];
try {
  const page = await chrome.cdp.firstPage();
  const exceptions = [];
  chrome.cdp.on((m) => {
    if (m.method === 'Runtime.exceptionThrown') exceptions.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
  });
  await page.send('Page.addScriptToEvaluateOnNewDocument', { source: nameScript('perf observer') });
  const viewport = await page.setWindow(opt.width, opt.height);
  await page.navigate(opt.url);
  await waitReady(page, opt.expected);
  // View the first frame's sticky grid at 100% and pick the stickies fully
  // in view, nearest the center first.
  const ids = await page.eval(`(() => {
    const b = window.board;
    const stickies = [...b.state.all()].filter((e) => e.type === 'sticky');
    let minX = Infinity, minY = Infinity;
    for (const e of stickies) { if (e.x + e.y < minX + minY) { minX = e.x; minY = e.y; } }
    b.setCam(minX - 40, minY - 40, ${opt.zoom});
    const v = b.renderer.viewRect();
    const inside = stickies.filter((e) => e.x >= v.x && e.y >= v.y && e.x + e.w <= v.x + v.w && e.y + e.h <= v.y + v.h);
    const cx = v.x + v.w / 2, cy = v.y + v.h / 2;
    inside.sort((a, c) => Math.hypot(a.x - cx, a.y - cy) - Math.hypot(c.x - cx, c.y - cy));
    return inside.map((e) => e.id);
  })()`);
  const need = opt.bots * opt.owned + 4;
  if (ids.length < need) throw new Error(`only ${ids.length} stickies in view, need ${need}`);
  await page.eval(observerHook);
  const shared = ids.slice(0, 4);
  for (let i = 0; i < opt.bots; i++) bots.push(new Bot(i, ids.slice(4 + i * opt.owned, 4 + (i + 1) * opt.owned), shared));
  for (const b of bots) b.start();
  for (let i = 0; i < 600 && !bots.every((b) => b.ready); i++) await sleep(100);
  if (!bots.every((b) => b.ready)) throw new Error('bots did not connect');
  const env = await environment(chrome.cdp);
  console.error(`observer view (zoom ${opt.zoom}) holds ${ids.length} stickies; ${opt.bots} bots editing ${opt.rate}/s each for ${opt.minutes} min; ${uptime()}`);

  // Load: each bot edits on its own timer, phase-shifted.
  const period = 1000 / opt.rate;
  const t0 = Date.now();
  const end = t0 + opt.minutes * 60000;
  const timers = bots.map((b, i) => setTimeout(function tick() {
    if (Date.now() >= end) return;
    b.edit();
    b.timer = setTimeout(tick, period);
  }, (i * period) / opt.bots));
  const loads = [];
  while (Date.now() < end) {
    await sleep(Math.min(30000, end - Date.now()));
    loads.push(uptime());
    const sent = bots.reduce((a, b) => a + b.sentAt.size, 0);
    const acked = bots.reduce((a, b) => a + b.acks.length, 0);
    console.error(`${Math.round((Date.now() - t0) / 1000)} s: sent ${sent}, acked ${acked}, pending ${bots.map((b) => b.state.pending.length).join('/')}; ${loads.at(-1)}`);
  }
  for (const t of timers) clearTimeout(t);
  for (const b of bots) clearTimeout(b.timer);

  // Settle: every batch acknowledged, every screen at the server's seq.
  let snap = null;
  for (let i = 0; i < 120; i++) {
    await sleep(500);
    if (bots.some((b) => b.state.pending.length)) continue;
    snap = await rawSnapshot();
    const obsSeq = await page.eval('window.board.state.watermark');
    if (obsSeq === snap.seq && bots.every((b) => b.state.watermark === snap.seq)) break;
    snap = null;
  }
  const settled = !!snap;
  if (!snap) snap = await rawSnapshot();
  await sleep(500);

  // Latency.
  const seen = await page.evalLarge('window.__seen');
  const firstSeen = new Map();
  for (const [tok, applied, drawn, display, seq, pending] of seen) if (!firstSeen.has(tok)) firstSeen.set(tok, { applied, drawn, display, seq, pending });
  const lat = [], latDrawn = [], latApplied = [], ackLat = [];
  let missing = 0, total = 0, notDrawnYet = 0;
  for (const b of bots) {
    const ackAt = new Map(b.acks.map((a) => [a.n, a.at]));
    for (const [n, { token }] of b.batches) {
      const sent = b.sentAt.get(n);
      if (sent == null) continue;
      total++;
      if (ackAt.has(n)) ackLat.push(ackAt.get(n) - sent);
      const s = firstSeen.get(token);
      if (!s) { missing++; continue; }
      if (s.pending) notDrawnYet++;
      lat.push(s.display - sent);
      latDrawn.push(s.drawn - sent);
      latApplied.push(s.applied - sent);
    }
  }

  // --inject-divergence is the checker's negative control: it changes one
  // element in the first bot's copy, which the check below must report.
  if (opt['inject-divergence']) {
    const st = bots[0].state;
    const id = bots[0].owned[0];
    st.server.set(id, { ...st.server.get(id), text: 'injected divergence' });
  }

  // Consistency: every screen equals the server, in content and in full.
  const server = new Map(snap.elements.map((e) => [e.id, e]));
  const screens = { observer: await page.evalLarge(`Object.fromEntries([...window.board.state.all()].map((e) => [e.id, e]))`) };
  const observerServer = await page.evalLarge(`Object.fromEntries([...window.board.state.server].map(([id, e]) => [id, e]))`);
  for (const b of bots) screens[b.name] = Object.fromEntries([...b.state.all()].map((e) => [e.id, e]));
  const diff = (els, full) => {
    const bad = [];
    const keys = new Set([...server.keys(), ...Object.keys(els)]);
    for (const id of keys) {
      const a = server.get(id), c = els[id];
      if (!a || !c) { bad.push(id); continue; }
      if ((full ? canonical(a) : canonical(contentOf(a))) !== (full ? canonical(c) : canonical(contentOf(c)))) bad.push(id);
    }
    return bad;
  };
  const consistency = {};
  for (const [name, els] of Object.entries(screens)) consistency[name] = { elements: Object.keys(els).length, contentMismatch: diff(els, false), fullMismatch: diff(name === 'observer' ? observerServer : Object.fromEntries(bots.find((b) => b.name === name).state.server), true) };
  const hash = (m) => createHash('sha256').update(canonical([...m.values()].map(contentOf).sort((a, c) => (a.id < c.id ? -1 : 1)))).digest('hex').slice(0, 16);

  // Must-apply: the last acknowledged ok write of each (element, bundle)
  // stays.
  const lastWrite = new Map(); // id|g -> {seq, val, bot}
  const voids = {};
  for (const b of bots) {
    for (const a of b.acks) {
      const batch = b.batches.get(a.n);
      if (!batch) continue;
      batch.ops.forEach((o, i) => {
        const res = Array.isArray(a.results) ? a.results[i] : null;
        if (res !== 'ok') { voids[String(res)] = (voids[String(res)] || 0) + 1; return; }
        const key = o.id + '|' + o.g;
        const prev = lastWrite.get(key);
        if (!prev || a.seq > prev.seq) lastWrite.set(key, { seq: a.seq, val: o.val, bot: b.i });
      });
    }
  }
  const lost = [];
  for (const [key, w] of lastWrite) {
    const [id, g] = key.split('|');
    const el = server.get(id);
    let got;
    if (!el) got = undefined;
    else if (g === 'geom') got = { x: el.x, y: el.y, w: el.w, h: el.h, rot: el.rot };
    else if (g.startsWith('style.')) got = el.style[g.slice(6)];
    else got = el[g];
    if (canonical(got) !== canonical(w.val)) lost.push({ id, g, want: w.val, got, seq: w.seq });
  }

  result = {
    kind: 'remote-latency', injectedDivergence: !!opt['inject-divergence'], url: opt.url, observerZoom: opt.zoom, ownedPerBot: opt.owned, sharedPool: 4, expected: opt.expected, minutes: opt.minutes, bots: opt.bots, ratePerBot: opt.rate,
    headless: !!opt.headless, env, viewport, uptimeBefore: before, uptimeDuring: loads, uptimeAfter: uptime(),
    edits: { sent: total, displayed: lat.length, neverDisplayed: missing, drawnFrameStillPending: notDrawnYet },
    sendToDisplayMs: stats(lat), sendToDrawMs: stats(latDrawn), sendToApplyMs: stats(latApplied), sendToAckMs: stats(ackLat),
    rejects: bots.flatMap((b) => b.rejects), fatal: bots.map((b) => b.fatal).filter(Boolean), reconnects: bots.map((b) => b.welcomes - 1),
    settled, serverSeq: snap.seq,
    consistency: Object.fromEntries(Object.entries(consistency).map(([k, v]) => [k, { elements: v.elements, contentMismatches: v.contentMismatch.length, fullMismatches: v.fullMismatch.length, sample: [...v.contentMismatch, ...v.fullMismatch].slice(0, 5) }])),
    contentHash: { server: hash(server), observer: hash(new Map(Object.entries(screens.observer))), ...Object.fromEntries(bots.map((b) => [b.name, hash(new Map(Object.entries(screens[b.name])))])) },
    mustApply: { bundlesChecked: lastWrite.size, lost: lost.length, sample: lost.slice(0, 5), voidResults: voids },
    exceptions,
  };
  console.log(JSON.stringify(result, null, 1));
  if (opt.out) writeFileSync(opt.out, JSON.stringify({ ...result, rawLatency: lat }, null, 1));
} finally {
  for (const b of bots) b.net.stop();
  await chrome.close();
}

// Leftover sockets or timers must not keep the process alive.
process.exit(process.exitCode || 0);
