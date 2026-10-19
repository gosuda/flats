import test from 'node:test';
import assert from 'node:assert/strict';
import { batchFrames, batchFits, queueBatch, utf8Length, FRAME_MAX, PART_DATA_MAX, TooLargeError, Net } from '../net.js';
import { BoardState } from '../state.js';

const sticky = (i, text) => ({ op: 'add', el: { id: 'e_' + String(i).padStart(10, '0'), type: 'sticky', x: i, y: 0, w: 200, h: 200, rot: 0, z: 'V', text } });

test('a small batch is one ops frame', () => {
  const frames = batchFrames(3, [sticky(1, 'hi')]);
  assert.equal(frames.length, 1);
  assert.deepEqual(JSON.parse(frames[0]), { t: 'ops', n: 3, ops: [sticky(1, 'hi')] });
});

function checkParts(frames, ops) {
  assert.ok(frames.length >= 2 && frames.length <= 32, `parts ${frames.length}`);
  let data = '';
  frames.forEach((f, i) => {
    assert.ok(utf8Length(f) <= FRAME_MAX, `frame ${i} is ${utf8Length(f)} bytes`);
    const m = JSON.parse(f);
    assert.equal(m.t, 'ops_part');
    assert.equal(m.part, i + 1);
    assert.equal(m.parts, frames.length);
    assert.ok(utf8Length(m.data) <= PART_DATA_MAX);
    data += m.data;
  });
  assert.deepEqual(JSON.parse(data), ops);
}

test('a large batch splits into ops_part frames within the encoded size limit', () => {
  const ops = Array.from({ length: 400 }, (_, i) => sticky(i, 'x'.repeat(3000)));
  checkParts(batchFrames(7, ops), ops);
});

test('escape-heavy text still fits each encoded frame', () => {
  const ops = Array.from({ length: 300 }, (_, i) => sticky(i, '"\\\n\u0001'.repeat(900)));
  checkParts(batchFrames(7, ops), ops);
});

test('multi-byte text is never cut inside a character', () => {
  const ops = Array.from({ length: 300 }, (_, i) => sticky(i, '회고😀'.repeat(700)));
  const frames = batchFrames(9, ops);
  for (const f of frames) {
    const d = JSON.parse(f).data;
    const last = d.charCodeAt(d.length - 1);
    assert.ok(!(last >= 0xd800 && last <= 0xdbff), 'no dangling high surrogate');
  }
  checkParts(frames, ops);
});

const huge = () => Array.from({ length: 500 }, (_, i) => sticky(i, 'y'.repeat(18000)));

test('a batch over 8 MiB is refused locally', () => {
  const ops = huge();
  assert.throws(() => batchFrames(1, ops), TooLargeError);
  assert.equal(batchFits(1, ops), false);
  assert.equal(batchFits(1, [sticky(1, 'hi')]), true);
});

// ---- connection flow with a fake socket ----

class FakeSocket {
  static last = null;
  constructor(url) { this.url = url; this.sent = []; FakeSocket.last = this; }
  send(s) { this.sent.push(JSON.parse(s)); }
  close() {}
  recv(m) { this.onmessage({ data: JSON.stringify(m) }); }
}

function harness(t) {
  globalThis.WebSocket = FakeSocket;
  globalThis.location = { protocol: 'http:', host: 'board.test' };
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const state = new BoardState();
  const seen = { acks: [], rejects: [], deltas: [], fatal: null, snapshots: 0, statuses: [], tooLarge: [] };
  const net = new Net(state, {
    hello: () => ({ t: 'hello', v: 1, session: state.session || undefined, watermark: state.session ? state.watermark : undefined, pending: { count: state.pending.length, first_n: state.pending[0]?.n }, editor: 'ed' }),
    welcome() {}, status() { seen.statuses.push(net.connecting ? 'connecting' : net.connected ? 'open' : 'offline'); },
    pres() {}, shared() {}, tooLarge(b) { seen.tooLarge.push(b); },
    snapshot() { seen.snapshots++; },
    delta(r) { seen.deltas.push(r); },
    ack(m) { seen.acks.push(m.n); },
    reject(m) { seen.rejects.push(m.n); },
    fatal(code) { seen.fatal = code; },
  });
  net.start();
  const ws = FakeSocket.last;
  ws.onopen();
  return { state, net, ws, seen };
}

test('hello, welcome, snapshot parts, then batches one at a time', (t) => {
  const { state, net, ws, seen } = harness(t);
  assert.equal(ws.sent[0].t, 'hello');
  ws.recv({ t: 'welcome', session: 'S1', expires: 0, access: 'private', me: { key: 'k', name: 'n', verified: false, color: '#000' } });
  ws.recv({ t: 'snapshot', gen: 0, seq: 5, part: 1, parts: 2, elements: [{ id: 'e_aaaaaaaaaa', type: 'text', x: 0, y: 0, w: 1, h: 1, rot: 0, z: 'V', text: '', inc: 1, v: {} }] });
  ws.recv({ t: 'delta', gen: 0, seq: 6, changes: [{ id: 'e_aaaaaaaaaa', inc: 1, set: { text: ['held', 6] } }] });
  ws.recv({ t: 'snapshot', gen: 0, seq: 5, part: 2, parts: 2, elements: [] });
  assert.equal(seen.snapshots, 1);
  assert.equal(state.get('e_aaaaaaaaaa').text, 'held', 'delta held during the snapshot applies after it');
  state.push([{ op: 'meta', g: 'grid', val: false }]);
  state.push([{ op: 'meta', g: 'grid', val: true }]);
  net.flush();
  assert.deepEqual(ws.sent.slice(1).map((m) => m.n), [1], 'second batch waits for the first');
  ws.recv({ t: 'ack', n: 1, seq: 6, results: [] });
  assert.deepEqual(ws.sent.slice(1).map((m) => m.n), [1, 2]);
});

test('a transient reject keeps the number and resends after backoff', (t) => {
  const { state, net, ws, seen } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  state.push([{ op: 'meta', g: 'grid', val: false }]);
  net.flush();
  ws.recv({ t: 'reject', n: 1, code: 'busy', retry: true });
  assert.equal(state.pending.length, 1);
  assert.equal(ws.sent.length, 2);
  t.mock.timers.tick(1000);
  assert.equal(ws.sent.length, 3);
  assert.equal(ws.sent[2].n, 1, 'same batch number');
  ws.recv({ t: 'reject', n: 1, code: 'disk_quota', retry: false });
  assert.equal(state.pending.length, 0);
  assert.deepEqual(seen.rejects, [1, 1]);
});

test('delta_part frames assemble per generation; a resync discards partial parts', (t) => {
  const { state, ws, seen } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  ws.recv({ t: 'snapshot', gen: 0, seq: 1, part: 1, parts: 1, elements: [{ id: 'e_aaaaaaaaaa', type: 'text', x: 0, y: 0, w: 1, h: 1, rot: 0, z: 'V', text: '', inc: 1, v: {} }] });
  const text = JSON.stringify({ t: 'delta', seq: 2, changes: [{ id: 'e_aaaaaaaaaa', inc: 1, set: { text: ['big', 2] } }] });
  ws.recv({ t: 'delta_part', gen: 0, seq: 2, part: 1, parts: 2, data: text.slice(0, 20) });
  ws.recv({ t: 'resync', gen: 1, seq: 9 });
  ws.recv({ t: 'delta_part', gen: 0, seq: 2, part: 2, parts: 2, data: text.slice(20) });
  assert.equal(seen.deltas.length, 0, 'old generation parts are ignored');
  ws.recv({ t: 'snapshot', gen: 1, seq: 9, part: 1, parts: 1, elements: [{ id: 'e_aaaaaaaaaa', type: 'text', x: 0, y: 0, w: 1, h: 1, rot: 0, z: 'V', text: 'resynced', inc: 1, v: {} }] });
  assert.equal(state.watermark, 9);
  const t2 = JSON.stringify({ t: 'delta', seq: 10, changes: [{ id: 'e_aaaaaaaaaa', inc: 1, set: { text: ['after', 10] } }] });
  ws.recv({ t: 'delta_part', gen: 1, seq: 10, part: 2, parts: 2, data: t2.slice(30) });
  ws.recv({ t: 'delta_part', gen: 1, seq: 10, part: 1, parts: 2, data: t2.slice(0, 30) });
  assert.equal(state.get('e_aaaaaaaaaa').text, 'after');
});

test('fatal stops the connection and does not reconnect', (t) => {
  const { net, ws, seen } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  ws.recv({ t: 'fatal', code: 'session_expired' });
  assert.equal(seen.fatal, 'session_expired');
  assert.equal(net.stopped, true);
  const before = FakeSocket.last;
  t.mock.timers.tick(60000);
  assert.equal(FakeSocket.last, before);
});

test('an intentional reconnect shows connecting, not disconnected', (t) => {
  const { net, ws, seen } = harness(t);
  assert.equal(net.connecting, true, 'the first connection');
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  assert.equal(net.connecting, false);
  net.restart(); // for example after the person entered a name
  assert.equal(seen.statuses.at(-1), 'connecting');
  const again = FakeSocket.last;
  assert.notEqual(again, ws);
  again.onopen();
  again.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  assert.equal(seen.statuses.at(-1), 'open');
  assert.ok(!seen.statuses.includes('offline'), `statuses ${seen.statuses}`);
  // A socket that drops is disconnected, and so is a restart that fails.
  again.onclose();
  assert.equal(seen.statuses.at(-1), 'offline');
  net.restart();
  FakeSocket.last.onclose();
  assert.equal(seen.statuses.at(-1), 'offline');
});

const ops1 = (v) => [{ op: 'meta', g: 'grid', val: v }];

test('a too-large batch never consumes a number: the next sent n is the last acked n + 1', (t) => {
  const { state, net, ws, seen } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  state.push(ops1(false));
  net.flush();
  ws.recv({ t: 'ack', n: 1, seq: 1, results: [] });
  const big = state.push(huge()).batch;
  const next = state.push(ops1(true)).batch;
  assert.equal(big.n, 2);
  net.flush();
  assert.deepEqual(seen.tooLarge, [big], 'the person is told');
  assert.ok(!state.pending.includes(big), 'the unsendable batch is dropped');
  const sent = ws.sent.filter((m) => m.t === 'ops' || m.t === 'ops_part');
  assert.deepEqual(sent.map((m) => m.n), [1, 2], 'no gap after the acked batch 1');
  assert.equal(next.n, 2, 'the later batch took the unused number');
  assert.equal(state.push(ops1(false)).batch.n, 3, 'numbering continues without a gap');
  assert.equal(state.get('e_' + '0'.repeat(10)), null, 'its optimistic elements are gone');
});

test('a gap in delta seqs reconnects with the session and watermark', (t) => {
  const { state, ws } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  ws.recv({ t: 'snapshot', gen: 0, seq: 5, part: 1, parts: 1, elements: [] });
  ws.recv({ t: 'delta', gen: 0, seq: 7, changes: [] });
  assert.equal(state.watermark, 5, 'the gapped delta is not applied');
  const again = FakeSocket.last;
  assert.notEqual(again, ws, 'a new socket');
  again.onopen();
  assert.equal(again.sent[0].session, 'S1');
  assert.equal(again.sent[0].watermark, 5);
});

test('a reconnect drops the old transient retry delay', (t) => {
  const { state, net, ws } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  state.push(ops1(false));
  net.flush();
  ws.recv({ t: 'reject', n: 1, code: 'busy', retry: true });
  t.mock.timers.tick(1000);
  ws.recv({ t: 'reject', n: 1, code: 'busy', retry: true });
  assert.ok(net.retryTimer, 'a 2 s retry is scheduled');
  ws.onclose();
  assert.equal(net.retryTimer, null);
  t.mock.timers.tick(1200); // reconnect backoff, at most 1.2 s
  const again = FakeSocket.last;
  assert.notEqual(again, ws);
  again.onopen();
  again.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  assert.deepEqual(again.sent.filter((m) => m.t === 'ops').map((m) => m.n), [1], 'resent on welcome, not after the old delay');
});

// withdrawn is the check app.checkPublic runs while a Public board's
// connection is down (TECH 11.3). The request is deferred so the test
// controls when its answer arrives relative to a reconnect.
test('a withdrawal check answered after a reconnect is stale', async (t) => {
  const { net, ws } = harness(t);
  const welcome = { t: 'welcome', session: 'S1', access: 'public', me: {} };
  ws.recv(welcome);
  const deferred = () => {
    let settle;
    const p = new Promise((resolve, reject) => { settle = { resolve, reject }; });
    return { head: () => p, ...settle };
  };
  // Down, then reconnected while the request runs: a non-OK answer is ignored.
  ws.onclose();
  let d = deferred();
  let check = net.withdrawn(d.head);
  t.mock.timers.tick(1200);
  let sock = FakeSocket.last;
  sock.onopen();
  sock.recv(welcome);
  assert.equal(net.connected, true);
  d.resolve({ ok: false, status: 404 });
  assert.equal(await check, false, 'a stale answer does not report a withdrawal for the recovered connection');

  // Reconnected and down again before the answer: still stale.
  sock.onclose();
  d = deferred();
  check = net.withdrawn(d.head);
  t.mock.timers.tick(2400);
  const between = FakeSocket.last;
  between.onopen();
  between.recv(welcome);
  between.onclose();
  assert.equal(net.connected, false);
  d.resolve({ ok: false, status: 404 });
  assert.equal(await check, false, 'a welcome during the request makes its answer stale');

  // Still down when the answer arrives: a non-OK answer is a withdrawal,
  // an OK answer or a network error is not.
  d = deferred();
  check = net.withdrawn(d.head);
  d.resolve({ ok: false, status: 404 });
  assert.equal(await check, true);
  d = deferred();
  check = net.withdrawn(d.head);
  d.resolve({ ok: true, status: 200 });
  assert.equal(await check, false);
  d = deferred();
  check = net.withdrawn(d.head);
  d.reject(new TypeError('network down'));
  assert.equal(await check, false);
});

// queueBatch is the precheck app.commit runs before a batch takes a number.
test('queueBatch refuses an oversized batch without consuming a number', (t) => {
  const { state, net, ws } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  queueBatch(state, ops1(false));
  net.flush();
  ws.recv({ t: 'ack', n: 1, seq: 1, results: [] });
  const before = { nextN: state.nextN, created: state.created.size };
  assert.equal(queueBatch(state, huge()), null, 'refused');
  assert.equal(state.nextN, before.nextN, 'no number taken');
  assert.equal(state.pending.length, 0, 'nothing queued');
  assert.equal(state.created.size, before.created, 'no optimistic elements recorded');
  assert.equal(state.get('e_' + '0'.repeat(10)), null, 'nothing drawn');
  const { batch } = queueBatch(state, ops1(true));
  assert.equal(batch.n, 2, 'the next action takes the next number');
  net.flush();
  const sent = ws.sent.filter((m) => m.t === 'ops' || m.t === 'ops_part');
  assert.deepEqual(sent.map((m) => m.n), [1, 2], 'no gap');
});

const encodes = (spy) => spy.mock.calls.filter((c) => Array.isArray(c.arguments[0]) || ['ops', 'ops_part'].includes(c.arguments[0]?.t)).length;

test('a queued batch is serialized once for the check and the send', (t) => {
  const { state, net, ws } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  const spy = t.mock.method(JSON, 'stringify');
  queueBatch(state, ops1(false));
  assert.equal(encodes(spy), 1, 'the check encodes the ops frame');
  net.flush();
  assert.equal(encodes(spy), 1, 'flush sends it without encoding again');
  assert.deepEqual(ws.sent.filter((m) => m.t === 'ops').map((m) => m.n), [1]);

  // A split batch, too, is encoded by the check only.
  ws.recv({ t: 'ack', n: 1, seq: 1, results: [] });
  const big = Array.from({ length: 400 }, (_, i) => sticky(i, 'x'.repeat(3000)));
  spy.mock.resetCalls();
  queueBatch(state, big);
  const checked = encodes(spy);
  assert.ok(checked >= 3, `the check splits the batch (${checked} encodings)`);
  net.flush();
  assert.equal(encodes(spy), checked, 'flush adds no encoding');
  const parts = ws.sent.filter((m) => m.t === 'ops_part');
  assert.ok(parts.length >= 2 && parts.every((m) => m.n === 2));
});

test('a renumbered batch is encoded again under its new number', (t) => {
  const { state, net, ws } = harness(t);
  ws.recv({ t: 'welcome', session: 'S1', access: 'private', me: {} });
  const first = queueBatch(state, ops1(false)).batch;
  const second = queueBatch(state, ops1(true)).batch;
  assert.equal(second.encoded.n, 2);
  state.discard(first.n); // the fallback renumbering path
  assert.equal(second.n, 1);
  net.flush();
  const sent = ws.sent.filter((m) => m.t === 'ops');
  assert.deepEqual(sent.map((m) => [m.n, m.ops[0].val]), [[1, true]], 'the frame carries the new number');
});
