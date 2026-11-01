'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Net, actionFrames, FRAME_MAX, utf8Length } from '../net.js';
import { Store } from '../state.js';
import { Timers } from './page.mjs';

// Match liveEnvelope/liveHello/liveWatch in protocol.go and live.go, and
// Spill.Part's JSON-string fragments in reader.go, not the legacy t fixtures.
const pageID = 'p_' + 'a'.repeat(26);
const blockID = 'b_' + 'b'.repeat(26);
const databaseID = 'd_' + 'd'.repeat(26);
const rowID = 'r_' + 'r'.repeat(26);
const tree = { kind: 'tree' };
const pageScope = { kind: 'page', id: pageID };
const databaseScope = { kind: 'database', id: databaseID };
const page = (title = 'Page') => ({ kind: 'page', ref: { id: pageID, inc: '5' }, ver: '5', v: {}, title, parent: null, position: 'V' });
const block = () => ({ kind: 'block', ref: { id: blockID, inc: '5' }, ver: '5', v: {}, page: pageID, parent: null, position: 'V', content: { kind: 'paragraph', text: [] } });
const publicItem = ({ ref, ver, v, ...fields }) => ({ id: ref.id, ...fields });

function setup(access = 'private', handlers = {}) {
  const store = new Store();
  const timers = new Timers();
  const sockets = [];
  const net = new Net(store, handlers, {
    timers,
    now: () => timers.now,
    connect: () => {
      const socket = { sent: [], send(raw) { this.sent.push(JSON.parse(raw)); }, close() {} };
      sockets.push(socket);
      return socket;
    },
  });
  net.start();
  const socket = sockets.at(-1);
  socket.onopen();
  const receive = (frame) => socket.onmessage({ data: JSON.stringify(frame) });
  receive(access === 'private'
    ? { type: 'welcome', access, session: 'token', session_id: 'editor' }
    : { type: 'welcome', access });
  return { store, net, socket, receive, timers };
}

function snapshot(receive, scope, seq, items, extra = {}) {
  receive({ type: 'snapshot', generation: 1, seq, scope, spill: 'capture', spill_part: 0, part: 0, parts: 1, data: JSON.stringify({ items }), ...extra });
}

test('client envelopes use only Go type fields and the first watch exceeds hello generation', () => {
  const { store, net, socket, timers } = setup();
  assert.deepEqual(socket.sent[0], { type: 'hello', version: 1, pending: { count: 0 } });
  net.watch(pageID, [databaseID]);
  assert.deepEqual(socket.sent.at(-1), { type: 'watch', generation: 2, page: pageID, visible_databases: [databaseID], pending_scopes: [] });
  timers.advance(50);
  net.presence({ page: pageID, edit: null });
  assert.deepEqual(socket.sent.at(-1), { type: 'presence', page: pageID, edit: null });
  const action = store.pending.push({ action_id: 'rename', ops: [{ op: 'set_page_title', page: page().ref, title: 'Renamed' }] });
  net.flush();
  assert.deepEqual(socket.sent.at(-1), { type: 'action', n: action.n, action_id: 'rename', ops: action.ops });
  assert.ok(socket.sent.every((frame) => !Object.hasOwn(frame, 't')));
});

test('multipart action reassembles the complete action envelope required by liveAssembly', () => {
  const action = { n: '7', action_id: 'large', typing_block: block().ref, close: true, ops: [{ op: 'set_page_title', page: page().ref, title: '한😀"\\'.repeat(40000) }] };
  const encoded = actionFrames(action);
  assert.ok(encoded.length > 1);
  const frames = encoded.map((raw, index) => {
    assert.ok(utf8Length(raw) <= FRAME_MAX);
    const frame = JSON.parse(raw);
    assert.deepEqual(Object.keys(frame).sort(), ['data', 'n', 'part', 'parts', 'type']);
    assert.equal(frame.type, 'action_part');
    assert.equal(frame.n, action.n);
    assert.equal(frame.part, index);
    assert.equal(frame.parts, encoded.length);
    return frame;
  });
  assert.deepEqual(JSON.parse(frames.map((frame) => frame.data).join('')), { type: 'action', ...action });
});

test('snapshot text fragments decode only after the complete unit, even inside a string', () => {
  let changes = 0;
  const { store, receive } = setup('private', { changed: () => changes++ });
  const item = page('A quoted "title" 한😀');
  const raw = JSON.stringify({ items: [item] });
  const cut = raw.indexOf('quoted') + 3;
  const frame = { type: 'snapshot', generation: 1, seq: '10', scope: tree, spill: 'capture', parts: 2 };
  receive({ ...frame, spill_part: 1, part: 1, data: raw.slice(cut) });
  assert.equal(store.canonical.get(pageID), null);
  assert.equal(changes, 0);
  receive({ ...frame, spill_part: 0, part: 0, data: raw.slice(0, cut) });
  assert.deepEqual(store.canonical.get(pageID), item);
  assert.equal(store.view().firstPage().title, item.title);
  assert.equal(changes, 1);
});

test('Go boolean tombstones remove both tree and page items and survive an older capture', () => {
  const { store, receive } = setup();
  snapshot(receive, tree, '10', [page()]);
  snapshot(receive, pageScope, '10', [block()]);
  receive({ type: 'delta', generation: 1, seq: '11', tree: [{ ref: page().ref, deleted: true }], changes: [{ ref: block().ref, deleted: true }], scopes: [] });
  snapshot(receive, pageScope, '10', [block()]);
  assert.equal(store.canonical.get(pageID), null);
  assert.equal(store.canonical.get(blockID), null);
  assert.deepEqual([...store.canonical.items()], []);
  assert.deepEqual([...store.canonical.overlay.keys()].sort(), [blockID, pageID].sort());
});

test('public id/kind records support display indexes, live replacements and boolean deletion', () => {
  const { store, net, socket, receive } = setup('public');
  const publicPage = publicItem(page());
  const publicBlock = publicItem(block());
  snapshot(receive, tree, '10', [publicPage]);
  snapshot(receive, pageScope, '10', [publicBlock]);
  assert.equal(store.view().firstPage().ref.id, pageID);
  assert.equal(store.view().blocks(pageID)[0].ref.id, blockID);
  const displayed = store.canonical.get(pageID);
  assert.deepEqual(displayed.ref, { id: pageID });
  for (const key of ['inc', 'ver', 'v']) assert.equal(Object.hasOwn(displayed, key), false);
  assert.equal(Object.hasOwn(publicPage, 'ref'), false, 'normalizing must not mutate the incoming record');
  receive({ type: 'delta', generation: 1, seq: '11', tree: [{ ...publicPage, title: 'Public rename' }], changes: [{ id: blockID, kind: 'block', deleted: true }], scopes: [] });
  assert.equal(store.view().firstPage().title, 'Public rename');
  assert.deepEqual(store.view().blocks(pageID), []);
  receive({ type: 'delta', generation: 1, seq: '12', tree: [{ id: pageID, kind: 'page', deleted: true }], changes: [], scopes: [] });
  assert.equal(store.view().firstPage(), null);
  net.presence({ page: pageID, edit: null });
  store.pending.push({ action_id: 'forbidden', ops: [] });
  net.flush();
  assert.equal(socket.sent.length, 1, 'public connections do not send presence or actions');
});

test('database string windows install schema then rows without reviving a live deletion', () => {
  const { store, net, receive } = setup('public');
  const database = { id: databaseID, kind: 'database', block: blockID, title: 'Tasks', property_order: [], view_order: [] };
  const row = { id: rowID, kind: 'row', database: databaseID, position: 'V', cells: {} };
  snapshot(receive, databaseScope, '10', [database], { window: 0, rows: 0, final_window: false });
  assert.equal(store.canonical.loading(databaseScope), true);
  assert.equal(net.spill.next_window, 1);
  receive({ type: 'delta', generation: 1, seq: '11', tree: [], changes: [{ id: rowID, kind: 'row', deleted: true }], scopes: [] });
  snapshot(receive, databaseScope, '10', [row], { window: 1, rows: 1, final_window: true, spill_part: 1 });
  assert.equal(store.canonical.loading(databaseScope), false);
  assert.equal(store.canonical.get(databaseID).ref.id, databaseID);
  assert.equal(store.canonical.get(rowID), null);
  assert.equal(net.spill, null);
});

test('Go delta_part envelopes assemble before ack removes the pending action', () => {
  const { store, net, receive } = setup();
  snapshot(receive, tree, '10', [page()]);
  const action = store.pending.push({ action_id: 'rename', ops: [{ op: 'set_page_title', page: page().ref, title: 'Mine' }] });
  net.flush();
  const item = { ...page('Mine'), ver: '11', v: { title: '11' } };
  const delta = JSON.stringify({ type: 'delta', generation: 1, seq: '11', tree: [item], changes: [], scopes: [], from: { session_id: 'editor', n: action.n } });
  const frame = { type: 'delta_part', generation: 1, seq: '11', parts: 2 };
  receive({ ...frame, part: 0, data: delta.slice(0, 19) });
  assert.equal(store.pending.get(action.n).applied, false);
  receive({ ...frame, part: 1, data: delta.slice(19) });
  assert.equal(store.pending.get(action.n).applied, true);
  assert.deepEqual(store.canonical.get(pageID), item);
  receive({ type: 'ack', n: action.n, seq: '11' });
  assert.equal(store.pending.size, 0);
  assert.deepEqual(store.canonical.get(pageID), item);
});

test('legacy incoming t/object snapshots and object tombstones remain readable', () => {
  const { store, receive } = setup();
  receive({ t: 'snapshot', generation: 1, seq: '10', scope: tree, spill: 'old', part: 0, parts: 1, data: { items: [page()] } });
  assert.equal(store.canonical.get(pageID).title, 'Page');
  receive({ t: 'delta', generation: 1, seq: '11', tree: [{ deleted: page().ref }], changes: [] });
  assert.equal(store.canonical.get(pageID), null);
});

test('type is authoritative when an incoming frame also has a legacy t field', () => {
  const { net, receive } = setup();
  receive({ type: 'activity', t: 'fatal', code: 'wrong-discriminator' });
  assert.equal(net.stopped, null);
});
