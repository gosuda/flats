'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Net, actionFrames, utf8Length, FRAME_MAX } from '../net.js';
import { Store } from '../state.js';
import { FakeSocket, Timers, page, block, para, pid, bid } from './page.mjs';

function setup() {
  FakeSocket.all = [];
  const store = new Store();
  const timers = new Timers();
  const events = [];
  const net = new Net(store, {
    ack: (a, m) => events.push(['ack', a.n, m]),
    reject: (a, m) => events.push(['reject', a.n, m.code]),
    stopped: (code) => events.push(['stopped', code]),
  }, { connect: () => new FakeSocket(), timers });
  net.start();
  const ws = () => FakeSocket.all.at(-1);
  return { store, net, timers, events, ws };
}

const welcome = (session = 'tok') => ({ t: 'welcome', access: 'private', session, session_id: 'me' });
const tree = (seq, items, generation = 0) => ({ t: 'snapshot', generation, seq, scope: { kind: 'tree' }, spill: 's', spill_part: 0, part: 0, parts: 1, data: { items } });
const setOp = (text) => ({ op: 'set_block', block: { id: bid('x'), inc: '5' }, content: para(text), base_v: '5' });

test('hello carries the session, watermark and pending count for a resume', () => {
  const { store, net, ws, timers } = setup();
  assert.deepEqual(ws().open(), { type: 'hello', version: 1, pending: { count: 0 } });
  ws().recv(welcome());
  ws().recv(tree('10', [page('a')]));
  ws().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [] });
  store.pending.push({ action_id: 'a', ops: [setOp('x')] });
  net.watch(pid('a'));
  ws().drop();
  timers.advance(1000);
  const hello = ws().open();
  assert.equal(hello.session, 'tok');
  assert.equal(hello.watermark, '11');
  assert.deepEqual(hello.pending, { count: 1, first_n: '1' });
  assert.equal(hello.page, pid('a'));
});

test('actions are sent one at a time in order, and an ack removes but does not apply', () => {
  const { store, net, ws, events } = setup();
  ws().open();
  ws().recv(welcome());
  store.pending.push({ action_id: 'a', ops: [setOp('one')] });
  store.pending.push({ action_id: 'b', ops: [setOp('two')] });
  net.flush();
  assert.deepEqual(ws().frames('action').map((f) => f.n), ['1']);
  ws().recv({ t: 'ack', n: '1', seq: '12', outcomes: [{ indexes: [0], status: 'applied' }], summary: { applied: 1, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
  assert.deepEqual(ws().frames('action').map((f) => f.n), ['1', '2']);
  assert.equal(store.pending.size, 1);
  assert.equal(store.canonical.get(bid('x')), null, 'the ack carried no content');
  assert.deepEqual(events.map((e) => e[0]), ['ack']);
});

test('reconnect resends the exact unconfirmed action under its original number', () => {
  const { store, net, ws, timers } = setup();
  ws().open();
  ws().recv(welcome());
  store.pending.push({ action_id: 'a', ops: [setOp('text')], typing_block: { id: bid('x'), inc: '5' } });
  net.flush();
  const first = ws().frames('action')[0];
  ws().drop();
  assert.equal(net.status, 'offline');
  timers.advance(1000);
  ws().open();
  ws().recv(welcome());
  assert.deepEqual(ws().frames('action'), [first]);
});

test('a retryable reject resends the same number later; later actions wait', () => {
  const { store, net, ws, timers } = setup();
  ws().open();
  ws().recv(welcome());
  store.pending.push({ action_id: 'a', ops: [setOp('1')] });
  store.pending.push({ action_id: 'b', ops: [setOp('2')] });
  net.flush();
  ws().recv({ t: 'reject', n: '1', code: 'busy', retry: true, detail: {} });
  timers.advance(999);
  assert.equal(ws().frames('action').length, 1);
  timers.advance(1);
  assert.deepEqual(ws().frames('action').map((f) => f.n), ['1', '1']);
});

test('a protocol close resends undo unchanged and unblocks the following edit after its receipt', () => {
  const { store, net, ws, timers } = setup();
  ws().open();
  ws().recv({ type: 'welcome', access: 'private', session: 'tok', session_id: 'me' });
  store.pending.push({ action_id: 'undo', ops: [{ op: 'undo' }] });
  store.pending.push({ action_id: 'edit', ops: [setOp('after undo')] });
  net.flush();
  const undo = ws().frames('action')[0];
  ws().drop();
  assert.equal(net.inflight, null);
  assert.deepEqual(store.pending.actions.map((a) => a.n), ['1', '2']);
  timers.advance(1000);
  assert.deepEqual(ws().open().pending, { count: 2, first_n: '1' });
  ws().recv({ type: 'welcome', access: 'private', session: 'tok', session_id: 'me' });
  assert.deepEqual(ws().frames('action'), [undo]);
  ws().recv({ type: 'reject', n: '1', code: 'invalid', reason: 'ops', retry: false });
  assert.deepEqual(ws().frames('action').map((a) => a.n), ['1', '2']);
  ws().recv({ type: 'ack', n: '2', seq: '12', outcomes: [] });
  assert.equal(net.inflight, null);
  assert.equal(net.status, 'saved');
});

test('a permanent reject consumes the number and drops that action', () => {
  const { store, net, ws, events } = setup();
  ws().open();
  ws().recv(welcome());
  store.pending.push({ action_id: 'a', ops: [setOp('1')] });
  store.pending.push({ action_id: 'b', ops: [setOp('2')] });
  net.flush();
  ws().recv({ t: 'reject', n: '1', code: 'quota', retry: false, detail: {} });
  assert.deepEqual(events, [['reject', '1', 'quota']]);
  assert.deepEqual(store.pending.actions.map((a) => a.n), ['2']);
  assert.deepEqual(ws().frames('action').map((f) => f.n), ['1', '2']);
});

test('an expired receipt or unknown session stops editing and keeps the pending text', () => {
  const { store, net, ws, events } = setup();
  ws().open();
  ws().recv(welcome('tok'));
  store.pending.push({ action_id: 'a', ops: [setOp('keep me')] });
  ws().drop();
  net.start();
  ws().open();
  ws().recv(welcome('other'));
  assert.deepEqual(events, [['stopped', 'session_expired']]);
  assert.equal(net.status, 'stopped');
  assert.deepEqual(store.unsentText(), ['keep me']);
  assert.equal(ws().closed, true);
});

test('a delta tagged with this session settles its pending overlay', () => {
  const { store, ws } = setup();
  ws().open();
  ws().recv(welcome());
  ws().recv(tree('10', [page('a')]));
  const a = store.pending.push({ action_id: 'a', ops: [{ op: 'set_page_title', page: { id: pid('a'), inc: '5' }, title: 'Mine' }] });
  ws().recv({ t: 'delta', generation: 0, seq: '11', tree: [{ ...page('a', { title: 'Mine' }), ver: '11' }], changes: [], from: { session_id: 'me', n: a.n } });
  assert.equal(store.pending.get(a.n).applied, true);
  assert.equal(store.view().get(pid('a')).title, 'Mine');
});

test('duplicate deltas are ignored and a gap reconnects', () => {
  const { store, ws } = setup();
  ws().open();
  ws().recv(welcome());
  ws().recv(tree('10', [page('a')]));
  ws().recv({ t: 'delta', generation: 0, seq: '11', tree: [page('a', { title: 'one' })], changes: [] });
  ws().recv({ t: 'delta', generation: 0, seq: '11', tree: [page('a', { title: 'dup' })], changes: [] });
  assert.equal(store.canonical.get(pid('a')).title, 'one');
  const before = FakeSocket.all.length;
  ws().recv({ t: 'delta', generation: 0, seq: '13', tree: [page('a', { title: 'skipped' })], changes: [] });
  assert.equal(store.canonical.get(pid('a')).title, 'one');
  assert.equal(FakeSocket.all.length, before + 1, 'a new socket');
});

test('delta parts apply once, after the last part', () => {
  const { store, ws } = setup();
  ws().open();
  ws().recv(welcome());
  ws().recv(tree('10', [page('a')]));
  const body = JSON.stringify({ tree: [page('a', { title: 'big' })], changes: [] });
  ws().recv({ t: 'delta_part', generation: 0, seq: '11', part: 1, parts: 2, data: body.slice(10) });
  assert.equal(store.canonical.get(pid('a')).title, 'Page a');
  ws().recv({ t: 'delta_part', generation: 0, seq: '11', part: 0, parts: 2, data: body.slice(0, 10) });
  assert.equal(store.canonical.get(pid('a')).title, 'big');
});

test('resync drops partial deltas and snapshots of older generations', () => {
  const { store, ws } = setup();
  ws().open();
  ws().recv(welcome());
  ws().recv(tree('10', [page('a')]));
  ws().recv({ t: 'delta_part', generation: 0, seq: '11', part: 0, parts: 2, data: '{"tree":' });
  ws().recv({ t: 'resync', generation: 1, seq: '20' });
  ws().recv({ t: 'delta_part', generation: 0, seq: '11', part: 1, parts: 2, data: '[],"changes":[]}' });
  ws().recv({ t: 'snapshot', generation: 0, seq: '20', scope: { kind: 'tree' }, spill: 's', spill_part: 0, part: 0, parts: 1, data: { items: [page('a', { title: 'old gen' })] } });
  assert.equal(store.canonical.get(pid('a')).title, 'Page a');
  ws().recv(tree('20', [page('a', { title: 'resynced' })], 1));
  ws().recv({ t: 'delta', generation: 1, seq: '21', tree: [page('a', { title: 'live' })], changes: [] });
  assert.equal(store.canonical.get(pid('a')).title, 'live');
});

test('snapshot parts install only when all parts of one window arrive', () => {
  const { store, ws } = setup();
  ws().open();
  ws().recv(welcome());
  const scope = { kind: 'page', id: pid('a') };
  ws().recv({ t: 'snapshot', generation: 0, seq: '10', scope, spill: 's', spill_part: 0, part: 1, parts: 2, data: { items: [block('y', 'a', para('y'))] } });
  assert.equal(store.canonical.get(bid('y')), null);
  ws().recv({ t: 'snapshot', generation: 0, seq: '10', scope, spill: 's', spill_part: 1, part: 0, parts: 2, data: { items: [block('x', 'a', para('x'))] } });
  assert.ok(store.canonical.get(bid('x')) && store.canonical.get(bid('y')));
});

test('an unfinished database capture is resumed with its spill and next window', () => {
  const { net, ws } = setup();
  ws().open();
  ws().recv(welcome());
  const scope = { kind: 'database', id: 'd_' + 'd'.repeat(26) };
  ws().recv({ t: 'snapshot', generation: 0, seq: '10', scope, spill: 'sp', window: 0, spill_part: 0, part: 0, parts: 1, data: { items: [] } });
  ws().recv({ t: 'snapshot', generation: 0, seq: '10', scope, spill: 'sp', window: 1, rows: 1000, spill_part: 1, part: 0, parts: 2, data: { items: [] } });
  ws().drop();
  net.start();
  const hello = ws().open();
  assert.equal(hello.spill, 'sp');
  assert.equal(hello.next_window, 1, 'the partial window restarts');
});

test('an oversized action is split into zero-based parts within the frame limit', () => {
  const big = 'x한😀"\\'.repeat(60000);
  const frames = actionFrames({ n: '7', action_id: 'a', ops: [{ op: 'set_page_title', page: { id: pid('a'), inc: '5' }, title: big }] });
  assert.ok(frames.length > 1);
  let data = '';
  frames.forEach((f, i) => {
    assert.ok(utf8Length(f) <= FRAME_MAX);
    const m = JSON.parse(f);
    assert.equal(m.type, 'action_part');
    assert.equal(m.part, i);
    assert.equal(m.parts, frames.length);
    data += m.data;
  });
  assert.equal(JSON.parse(data).ops[0].title, big);
});

test('reconnect backs off from one second to thirty', () => {
  const { ws, timers } = setup();
  const delays = [];
  for (let i = 0; i < 7; i++) {
    const n = FakeSocket.all.length;
    ws().drop();
    let waited = 0;
    while (FakeSocket.all.length === n) { timers.advance(500); waited += 500; }
    delays.push(waited);
  }
  assert.deepEqual(delays, [1000, 2000, 4000, 8000, 16000, 30000, 30000]);
});
