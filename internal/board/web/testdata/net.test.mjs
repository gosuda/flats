import test from 'node:test';
import assert from 'node:assert/strict';
import { batchFrames, utf8Length, FRAME_MAX, PART_DATA_MAX, TooLargeError, Net } from '../net.js';
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

test('a batch over 8 MiB is refused locally', () => {
  const ops = Array.from({ length: 500 }, (_, i) => sticky(i, 'y'.repeat(18000)));
  assert.throws(() => batchFrames(1, ops), TooLargeError);
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
  const seen = { acks: [], rejects: [], deltas: [], fatal: null, snapshots: 0 };
  const net = new Net(state, {
    hello: () => ({ t: 'hello', v: 1, session: state.session || undefined, watermark: state.session ? state.watermark : undefined, pending: { count: state.pending.length, first_n: state.pending[0]?.n }, editor: 'ed' }),
    welcome() {}, status() {}, pres() {}, shared() {}, tooLarge() {},
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
