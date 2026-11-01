'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Net, actionFrames } from '../net.js';
import { Store } from '../state.js';
import { FakeSocket, Timers, page, block, para, pid, bid } from './page.mjs';

const databaseID = 'd_' + 'd'.repeat(26);
const databaseScope = { kind: 'database', id: databaseID };
const welcome = { type: 'welcome', access: 'private', session: 'token', session_id: 'editor' };

function setup(handlers = {}) {
  const store = new Store();
  const timers = new Timers();
  const sockets = [];
  const net = new Net(store, handlers, { timers, connect: () => {
    const ws = new FakeSocket();
    sockets.push(ws);
    return ws;
  } });
  net.start();
  const socket = () => sockets.at(-1);
  socket().open();
  socket().recv(welcome);
  return { store, net, timers, socket, sockets };
}

function snapshot(ws, scope, seq, items, extra = {}) {
  ws.recv({ type: 'snapshot', generation: 1, scope, seq, spill: 'capture-' + seq,
    part: 0, parts: 1, spill_part: 0, data: JSON.stringify({ items }), ...extra });
}

function edit(text) {
  return { action_id: 'edit-' + text.slice(0, 8), ops: [{ op: 'set_block', block: block('x', 'a', para('')).ref,
    base_v: '5', content: para(text) }] };
}

function multipart() {
  return { action_id: 'multipart', ops: Array.from({ length: 40 }, (_, i) => ({
    ...edit('x'.repeat(7600)).ops[0], block: block(i ? 'part' + i : 'x', 'a', para('')).ref,
  })) };
}

function save(ws, n, seq, text) {
  ws.recv({ type: 'delta', generation: 1, seq, tree: [], changes: [block('x', 'a', para(text))],
    from: { session_id: 'editor', n } });
  ws.recv({ type: 'ack', n, seq });
}

test('a 1008 resend close without fatal replays once before saving the following edit', () => {
  const { store, net, timers, socket, sockets } = setup();
  snapshot(socket(), { kind: 'tree' }, '10', [page('a')]);
  snapshot(socket(), { kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('old'))]);
  const action = store.pending.push(edit('original'));
  net.flush();
  const sent = socket().sent.at(-1);
  socket().onclose({ code: 1008, reason: 'resend', wasClean: true });
  assert.equal(net.status, 'offline');
  assert.equal(net.stopped, null);
  assert.equal(store.pending.get(action.n), action);
  timers.advance(1000);
  assert.equal(sockets.length, 2);
  assert.deepEqual(socket().open().pending, { count: 1, first_n: action.n });
  socket().recv(welcome);
  assert.deepEqual(socket().frames('action'), [sent]);
  save(socket(), action.n, '11', 'original');
  assert.equal(net.status, 'saved');
  store.pending.push(edit('following'));
  net.flush();
  save(socket(), '2', '12', 'following');
  timers.advance(30000);
  assert.equal(sockets.length, 2);
  assert.deepEqual(socket().frames('action').map((f) => f.n), ['1', '2']);
  assert.equal(store.canonical.get(bid('x')).content.text[0].text, 'following');
  assert.equal(store.pending.size, 0);
  assert.equal(net.status, 'saved');
});

for (const code of ['session_expired', 'access_changed', 'forbidden']) {
  test(`a terminal ${code} fatal keeps pending text and never reconnects`, () => {
    const { store, net, timers, socket, sockets } = setup();
    store.pending.push(edit('keep this text'));
    net.flush();
    socket().recv({ type: 'fatal', code });
    socket().drop();
    timers.advance(120000);
    assert.equal(sockets.length, 1);
    assert.equal(net.status, 'stopped');
    assert.equal(net.stopped.code, code);
    assert.deepEqual(store.unsentText(), ['keep this text']);
  });
}

test('refused multipart parts stay pending and an intervening edit cannot bypass the retry', () => {
  const rejects = [];
  const { store, net, timers, socket } = setup({ reject: (a) => rejects.push(a.n) });
  const action = store.pending.push(multipart());
  const text = action.ops[0].content.text[0].text;
  const original = actionFrames(action).map(JSON.parse);
  assert.equal(original.length, 2);
  net.flush();
  assert.deepEqual(socket().frames('action_part'), original, 'all parts are already queued');
  socket().recv({ type: 'reject', n: action.n, code: 'unavailable', reason: 'assembly', retry: true });
  assert.equal(store.pending.get(action.n), action);
  assert.equal(action.applied, false);
  timers.advance(200);
  store.pending.push(edit('following'));
  net.flush();
  assert.deepEqual(socket().frames('action_part'), original, 'queued edits must wait for the retry timer');
  timers.advance(799);
  net.flush();
  assert.deepEqual(socket().frames('action_part'), original);
  timers.advance(1);
  assert.deepEqual(socket().frames('action_part'), [...original, ...original]);
  assert.equal(store.pending.get(action.n), action);
  assert.deepEqual(socket().frames('action'), [], 'n+1 cannot overtake the refused n');
  save(socket(), action.n, '11', text);
  assert.equal(store.canonical.get(bid('x')).content.text[0].text, text);
  assert.deepEqual(socket().frames('action').map((f) => f.n), ['2']);
  save(socket(), '2', '12', 'following');
  timers.advance(30000);
  assert.deepEqual(socket().frames('action_part'), [...original, ...original]);
  assert.deepEqual(rejects, []);
  assert.equal(store.pending.size, 0);
  assert.equal(net.status, 'saved');
});

test('a receipt received during backoff cancels its retry and unblocks the following action', () => {
  const { store, net, timers, socket } = setup();
  store.pending.push(edit('first'));
  store.pending.push(edit('following'));
  net.flush();
  socket().recv({ type: 'reject', n: '1', code: 'unavailable', retry: true });
  save(socket(), '1', '11', 'first');
  assert.deepEqual(socket().frames('action').map((f) => f.n), ['1', '2']);
  assert.equal(net.retryTimer, null);
  save(socket(), '2', '12', 'following');
  timers.advance(30000);
  assert.deepEqual(socket().frames('action').map((f) => f.n), ['1', '2']);
  assert.equal(net.status, 'saved');
});

test('a close during multipart backoff reconnects with the original parts and no stale retry', () => {
  const { store, net, timers, socket, sockets } = setup();
  const action = store.pending.push(multipart());
  net.flush();
  const parts = socket().frames('action_part');
  socket().recv({ type: 'reject', n: action.n, code: 'unavailable', reason: 'assembly', retry: true });
  timers.advance(200);
  socket().onclose({ code: 1008, reason: 'resend' });
  timers.advance(1000);
  socket().open();
  socket().recv(welcome);
  assert.equal(sockets.length, 2);
  assert.deepEqual(socket().frames('action_part'), parts);
  save(socket(), action.n, '11', 'saved');
  timers.advance(30000);
  assert.deepEqual(socket().frames('action_part'), parts);
  assert.equal(net.status, 'saved');
});

function schema(database = databaseID) {
  const id = database.slice(2);
  return [
    { kind: 'database', ref: { id: database, inc: '5' }, block: bid('db'), title: 'Tasks' },
    { kind: 'property', ref: { id: 'pr_' + id, inc: '5' }, database, definition: { type: 'select', name: 'Status' } },
    { kind: 'option', ref: { id: 'o_' + id, inc: '5' }, property: 'pr_' + id, name: 'Open', color: 'blue' },
    { kind: 'view', ref: { id: 'v_' + id, inc: '5' }, database, definition: { name: 'All' } },
  ];
}

for (const kind of ['property', 'option', 'view']) {
  for (const source of ['snapshot', 'delta']) {
    test(`a replay receipt settles after an absent ${source} ${kind} is reconciled without row hydration`, () => {
      const old = schema();
      const removed = old.find((item) => item.kind === kind);
      const retained = old.filter((item) => item !== removed && !(kind === 'property' && item.kind === 'option'));
      const other = schema('d_' + 'e'.repeat(26));
      const row = { kind: 'row', ref: { id: 'r_' + 'r'.repeat(26), inc: '5' }, database: databaseID, cells: {}, position: 'V' };
      let acked = 0;
      const { store, net, timers, socket } = setup({ ack: () => {
        acked++;
        assert.equal(store.canonical.get(removed.ref.id), null, 'the receipt must not settle stale schema');
        if (kind === 'property') assert.equal(store.canonical.get(old[2].ref.id), null, 'the removed property also owned an option');
        assert.equal(store.canonical.loading(databaseScope), true, 'unrelated row windows are not needed by this receipt');
        assert.deepEqual(store.canonical.get(row.ref.id), row, 'pending rows must not be cleared with schema');
      } });
      if (source === 'snapshot') {
        snapshot(socket(), databaseScope, '10', old, { window: 0, final_window: false });
        snapshot(socket(), databaseScope, '10', [row], { window: 1, final_window: true });
      } else socket().recv({ type: 'delta', generation: 1, seq: '10', tree: [], changes: [...old, row] });
      snapshot(socket(), { kind: 'database', id: other[0].ref.id }, '10', other, { window: 0, final_window: true });
      const action = store.pending.push({ action_id: 'remove-schema', ops: [{ op: 'delete_' + kind, [kind]: removed.ref }] });
      net.flush();
      socket().drop();
      timers.advance(1000);
      socket().open();
      socket().recv(welcome);
      const raw = JSON.stringify({ items: retained });
      const frame = { type: 'snapshot', generation: 1, scope: databaseScope, seq: '20', spill: 'receipt',
        window: 0, final_window: false, parts: 2 };
      socket().recv({ ...frame, part: 0, spill_part: 0, data: raw.slice(0, 17) });
      assert.ok(store.canonical.get(removed.ref.id), 'a partial schema unit cannot remove anything');
      assert.equal(store.pending.get(action.n), action);
      socket().recv({ ...frame, part: 1, spill_part: 1, data: raw.slice(17) });
      snapshot(socket(), { kind: 'tree' }, '20', [page('a')]);
      socket().recv({ type: 'ack', n: action.n, seq: '20' });
      assert.equal(acked, 1);
      assert.equal(store.pending.size, 0);
      for (const item of other) assert.deepEqual(store.canonical.get(item.ref.id), item);
      for (const item of retained) assert.deepEqual(store.canonical.get(item.ref.id), item);
    });
  }
}

test('a schema-only receipt preserves older row values and deletion overlays until rows finish', () => {
  const { store, socket } = setup();
  const items = schema();
  const row = (id, title) => ({ kind: 'row', ref: { id: 'r_' + id.repeat(26), inc: '5' },
    database: databaseID, cells: { title }, position: 'V' });
  const retained = row('r', 'retained'), deleted = row('s', 'deleted'), created = row('t', 'created');
  snapshot(socket(), databaseScope, '10', items, { window: 0, final_window: false });
  snapshot(socket(), databaseScope, '10', [retained, deleted], { window: 1, final_window: true });
  socket().recv({ type: 'delta', generation: 1, seq: '11', tree: [], changes: [
    { ...retained, cells: { title: 'updated' } }, created,
    { ref: deleted.ref, deleted: true }, { ref: row('u', '').ref, deleted: true },
  ] });
  snapshot(socket(), databaseScope, '20', [items[0]], { window: 0, final_window: false });
  assert.equal(store.canonical.get(retained.ref.id).cells.title, 'updated');
  assert.deepEqual(store.canonical.get(created.ref.id), created);
  assert.equal(store.canonical.get(deleted.ref.id), null);
  assert.ok(store.canonical.overlay.get(deleted.ref.id).deleted);
  assert.ok(store.canonical.overlay.get(row('u', '').ref.id).deleted, 'unknown row tombstones have no schema owner');
  assert.equal(store.canonical.loading(databaseScope), true);
});

test('a complete schema window subsumes old schema but preserves newer values and tombstones', () => {
  const { store, socket } = setup();
  const old = schema();
  snapshot(socket(), databaseScope, '20', old, { window: 0, final_window: true });
  const newer = { ...old[1], definition: { type: 'select', name: 'Newer' }, ver: '21' };
  socket().recv({ type: 'delta', generation: 1, seq: '21', tree: [], changes: [newer, { ref: old[3].ref, deleted: true }] });
  snapshot(socket(), databaseScope, '20', [old[0], old[3]], { window: 0, final_window: false });
  assert.deepEqual(store.canonical.get(newer.ref.id), newer);
  assert.equal(store.canonical.get(old[2].ref.id), null, 'old absent option is subsumed immediately');
  assert.equal(store.canonical.get(old[3].ref.id), null, 'a newer tombstone still wins over captured schema');
  assert.equal(store.canonical.loading(databaseScope), true);
});
