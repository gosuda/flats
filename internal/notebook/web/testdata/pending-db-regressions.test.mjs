'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Store } from '../state.js';
import { Net } from '../net.js';
import { startApp, page, block, pid, bid, FakeSocket, Timers } from './page.mjs';

const id = (prefix, tag) => prefix + tag.repeat(25) + 'a';
const dbid = id('d_', 'd'), title = id('pr_', 't'), status = id('pr_', 's');
const option = id('o_', 'o'), viewid = id('v_', 'v'), rowid = id('r_', 'r');
const target = (id, inc = '5') => ({ id, inc });
const item = (kind, id, fields) => ({ kind, ref: target(id), ver: '5', v: {}, ...fields });
const definition = {
  name: 'Tasks', display: { layout: 'table', visible: [title, status], widths: [] }, sort: [],
  filter: [{ property: title, op: 'contains', value: 'Task' }],
};
const schema = () => [
  block('db', 'a', { kind: 'database', database: dbid }),
  item('database', dbid, { block: bid('db'), title: 'Tasks', property_order: [title, status], view_order: [viewid] }),
  item('property', title, { database: dbid, definition: { name: 'Name', type: 'title' }, option_order: [] }),
  item('property', status, { database: dbid, definition: { name: 'Status', type: 'select' }, option_order: [option] }),
  item('option', option, { property: status, name: 'Open', color: 'gray' }),
  item('view', viewid, { database: dbid, definition }),
];
const row = (id = rowid, value = 'Task one') => item('row', id, {
  database: dbid, position: 'V', cells: { [title]: { type: 'title', value } },
});
const scopes = (database = dbid) => [{ kind: 'page', id: pid('a') }, { kind: 'database', id: database }];
const sorted = (list) => list.map((s) => s.kind + ':' + s.id).sort();
const expectScopes = (actual, expected = scopes()) => assert.deepEqual(sorted(actual || []), sorted(expected));
const ack = (n, seq, status = 'applied', count = 1) => ({
  t: 'ack', n, seq, outcomes: [{ indexes: Array.from({ length: count }, (_, i) => i), status }],
  summary: { applied: status === 'applied' ? count : 0, ignored: 0, refused: status === 'refused' ? count : 0, copied_roots: [], complete: true },
  results: [], results_sample: { total: 0, complete: true },
});

async function databaseApp(t) {
  const app = await startApp({ pages: [page('a'), page('b')], blocks: schema().slice(0, 1) });
  app.socket().recv({
    t: 'snapshot', generation: app.app.net.generation, seq: '10', scope: { kind: 'page', id: pid('a') },
    spill: 'page', spill_part: 0, part: 0, parts: 1, data: { items: schema() },
  });
  t.after(() => { for (const m of app.app.databases.values()) m.handle.stop(); app.app.net.stop('test'); });
  app.window = (window, items, final_window) => app.socket().recv({
    t: 'snapshot', generation: app.app.net.generation, seq: '10', scope: { kind: 'database', id: dbid },
    spill: 'db', window, rows: items.filter((i) => i.kind === 'row').length, final_window,
    spill_part: window, part: 0, parts: 1, data: { items },
  });
  app.window(0, schema().slice(1), false);
  app.window(1, [row()], true);
  app.handle = () => app.app.databases.get(bid('db')).handle;
  return app;
}

test('consecutive view controls compose over actual unacknowledged actions', async (t) => {
  const a = await databaseApp(t);
  const root = a.handle().root;
  root.querySelectorAll('button').find((b) => b.textContent === 'View settings').click();
  const pop = root.querySelector('.db-pop');
  const shown = pop.querySelectorAll('.db-visible label').find((n) => n.textContent === 'Status').querySelector('input');
  shown.checked = false;
  a.fire(shown, 'change');
  const name = pop.querySelectorAll('input').find((n) => n.getAttribute('aria-label') === 'View name');
  name.value = 'My tasks';
  a.fire(name, 'change');
  const actions = a.app.store.pending.actions;
  assert.equal(actions.length, 2, 'both controls remain unacknowledged');
  assert.equal(actions[1].ops[0].definition.name, 'My tasks');
  assert.deepEqual(actions[1].ops[0].definition.display.visible, [target(title)]);
  assert.deepEqual(actions[1].ops[0].definition.filter, [{ property: target(title), op: 'contains', value: 'Task' }]);
  assert.deepEqual(a.app.view().get(viewid).definition, actions[1].ops[0].definition);
});

test('a new row keeps its open cell editor when creation is confirmed', async (t) => {
  const a = await databaseApp(t);
  const db = a.handle();
  db.root.querySelectorAll('button').find((b) => b.textContent === 'New row').click();
  const action = a.socket().frames('action').at(-1);
  const id = action.ops[0].proposed_id;
  const cell = db.root.querySelector(`tr[data-row="${id}"] td[data-prop="${title}"]`);
  a.fire(cell, 'dblclick');
  const input = cell.querySelector('input');
  input.value = 'Task drafted';
  const confirmed = { ...row(id, ''), ref: target(id, '11'), ver: '11' };
  a.socket().recv({ t: 'delta', seq: '11', tree: [], changes: [confirmed], from: { session_id: 'me', n: action.n } });
  a.socket().recv(ack(action.n, '11'));
  assert.ok(document.activeElement === input);
  assert.ok(input.isConnected);
  assert.equal(input.value, 'Task drafted');
  a.fire(input, 'keydown', { key: 'Enter' });
  assert.equal(a.socket().frames('action').at(-1).ops[0].cell.value, 'Task drafted');
});

for (const intervening of ['delta', 'hydration']) {
  test(`a pending filtered-out creation survives an intervening ${intervening}`, async (t) => {
    const a = await databaseApp(t);
    const db = a.handle();
    db.root.querySelectorAll('button').find((b) => b.textContent === 'New row').click();
    const action = a.socket().frames('action').at(-1);
    const created = action.ops[0].proposed_id;
    assert.ok(db.kept.has(created), 'the creator retains the proposed row immediately');
    if (intervening === 'delta') {
      a.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [row(rowid, 'Task updated')] });
    } else {
      a.window(0, schema().slice(1), false);
      a.window(1, [row()], true);
    }
    assert.ok(db.kept.has(created), 'unrelated canonical updates cannot look like deletion of a pending row');
    assert.ok(db.order.includes(created));
    a.socket().recv({ t: 'delta', generation: 0, seq: intervening === 'delta' ? '12' : '11', tree: [], changes: [row(created, '')], from: { session_id: 'me', n: action.n } });
    a.socket().recv(ack(action.n, intervening === 'delta' ? '12' : '11'));
    assert.ok(db.order.includes(created), 'canonical settlement preserves local filter retention');
    assert.match(db.root.querySelector(`tr[data-row="${created}"]`).textContent, /Hidden by this view/);
    location.hash = '#/' + pid('b');
    location.hash = '#/' + pid('a');
    assert.ok(!a.handle().order.includes(created), 'leaving the view ends its retention');
  });
}

test('leaving the view ends retention even while creation is pending', async (t) => {
  const a = await databaseApp(t);
  const created = a.handle().createRow({ [title]: '' });
  assert.ok(a.handle().order.includes(created));
  location.hash = '#/' + pid('b');
  expectScopes(a.socket().frames('watch').at(-1).pending_scopes);
  location.hash = '#/' + pid('a');
  assert.ok(!a.handle().kept.has(created));
  assert.ok(!a.handle().order.includes(created));
  assert.ok(a.app.store.view().get(created), 'leaving forgets retention, not the pending action');
});

test('refusal removes the optimistic creation and its retention', async (t) => {
  const a = await databaseApp(t);
  const db = a.handle();
  const created = db.createRow({ [title]: '' });
  assert.ok(db.order.includes(created));
  a.socket().recv(ack('1', '10', 'refused'));
  assert.equal(a.app.store.view().get(created), null);
  assert.ok(!db.order.includes(created));
  assert.ok(!db.kept.has(created));
});

test('pending rows keep typed cells and placement without overwriting a canonical arrival', (t) => {
  const { store } = connection(t);
  const op = {
    op: 'create_row', ref: 'new', proposed_id: id('r_', 'e'), database: target(dbid), before: target(rowid),
    cells: [{ property: target(title), value: 'Draft' }, { property: target(status), value: target(option) }],
  };
  store.pending.push({ action_id: 'create', ops: [op] });
  const pending = store.view().get(op.proposed_id);
  assert.deepEqual(pending.cells, { [title]: { type: 'title', value: 'Draft' }, [status]: { type: 'select', value: option } });
  assert.ok(pending.position < store.view().get(rowid).position);
  store.canonical.install({ kind: 'database', id: dbid }, '11', schema().slice(1), { window: 0, final: false });
  store.canonical.install({ kind: 'database', id: dbid }, '11', [row(), row(op.proposed_id, 'Canonical')], { window: 1, final: true });
  assert.equal(store.view().get(op.proposed_id).cells[title].value, 'Canonical');
  assert.equal(store.view().get(op.proposed_id).ref.inc, '5');
});

test('a confirmed deletion removes an own row retained by the filter', async (t) => {
  const a = await databaseApp(t);
  const db = a.handle();
  const created = db.createRow({ [title]: '' });
  a.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [row(created, '')], from: { session_id: 'me', n: '1' } });
  a.socket().recv(ack('1', '11'));
  assert.ok(db.order.includes(created));
  a.socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [], changes: [{ deleted: target(created) }] });
  assert.ok(!db.order.includes(created));
  assert.ok(!db.kept.has(created));
});

function connection(t) {
  const store = new Store();
  store.canonical.install({ kind: 'tree' }, '10', [page('a'), page('b')]);
  store.canonical.install({ kind: 'page', id: pid('a') }, '10', schema());
  store.canonical.install({ kind: 'database', id: dbid }, '10', schema().slice(1), { window: 0, final: false });
  store.canonical.install({ kind: 'database', id: dbid }, '10', [row()], { window: 1, final: true });
  const timers = new Timers();
  const sockets = [];
  const net = new Net(store, {}, { connect: () => { const s = new FakeSocket(); sockets.push(s); return s; }, timers });
  net.start();
  sockets[0].open();
  sockets[0].recv({ t: 'welcome', access: 'private', session: 'token', session_id: 'me' });
  t.after(() => net.stop('test'));
  return { store, net, timers, sockets };
}

const createdDB = id('d_', 'e'), createdProp = id('pr_', 'e'), createdOption = id('o_', 'e');
const createdRow = id('r_', 'e'), createdView = id('v_', 'e'), createdStatus = id('pr_', 'f');
const viewDef = { name: 'All', display: { layout: 'table', visible: [target(title)], widths: [] }, sort: [], filter: [] };
const createDatabase = {
  op: 'create_database', ref: 'db', proposed_id: createdDB, block_ref: 'owner', block_proposed_id: bid('newdb'),
  title_property_proposed_id: createdProp, view_proposed_id: createdView,
  place: { page: target(pid('a')), parent: null, before: null }, title: 'New database',
};
const databaseOps = [
  createDatabase,
  { op: 'set_database_title', database: target(dbid), title: 'Renamed' },
  { op: 'create_property', ref: 'prop', proposed_id: createdProp, database: target(dbid), definition: { name: 'Text', type: 'text' } },
  { op: 'set_property', property: target(status), definition: { name: 'State', type: 'select' } },
  { op: 'delete_property', property: target(status) },
  { op: 'set_property_order', database: target(dbid), properties: [target(status), target(title)] },
  { op: 'create_option', ref: 'option', proposed_id: createdOption, property: target(status), name: 'Closed', color: 'blue' },
  { op: 'set_option', option: target(option), name: 'Renamed', color: 'blue' },
  { op: 'delete_option', option: target(option) },
  { op: 'set_option_order', property: target(status), options: [target(option)] },
  { op: 'create_row', ref: 'row', proposed_id: createdRow, database: target(dbid), cells: [], before: null, view: target(viewid) },
  { op: 'set_cell', row: target(rowid), cell: { property: target(title), value: 'Edited' } },
  { op: 'move_rows', rows: [target(rowid)], before: null },
  { op: 'delete_rows', rows: [target(rowid)] },
  { op: 'create_view', ref: 'view', proposed_id: createdView, database: target(dbid), definition: viewDef },
  { op: 'set_view', view: target(viewid), definition: viewDef },
  { op: 'delete_view', view: target(viewid) },
  { op: 'set_view_order', database: target(dbid), views: [target(viewid)] },
];

for (const op of databaseOps) {
  test(`${op.op} retains owner-page and database scopes on watch and reconnect`, (t) => {
    const { store, net, timers, sockets } = connection(t);
    const a = store.pending.push({ action_id: 'edit', ops: [op] });
    net.flush();
    const expected = scopes(op.op === 'create_database' ? createdDB : dbid);
    // Owners may disappear before the receipt, but pending scopes must not.
    store.canonical.applyDelta({ seq: '11', changes: [...schema(), row()].map((i) => ({ deleted: i.ref })) });
    net.watch(pid('b'), []);
    expectScopes(sockets[0].frames('watch').at(-1).pending_scopes, expected);
    sockets[0].drop();
    timers.advance(1000);
    expectScopes(sockets[1].open().pending_scopes, expected);
    sockets[1].recv({ t: 'welcome', access: 'private', session: 'token', session_id: 'me' });
    sockets[1].recv(ack(a.n, '11'));
    net.watch(pid('b'), []);
    assert.deepEqual(sockets[1].frames('watch').at(-1).pending_scopes, []);
    assert.equal(net.hello().pending_scopes, undefined);
  });
}

test('reset and retry keep pending scopes; permanent refusal releases them', (t) => {
  const { store, net, sockets } = connection(t);
  const a = store.pending.push({ action_id: 'edit', ops: [databaseOps.find((op) => op.op === 'set_cell')] });
  store.canonical.reset();
  net.watch(pid('b'));
  expectScopes(sockets[0].frames('watch').at(-1).pending_scopes);
  sockets[0].recv({ t: 'reject', n: a.n, code: 'unavailable', retry: true, detail: {} });
  expectScopes(net.hello().pending_scopes);
  sockets[0].recv({ t: 'reject', n: a.n, code: 'invalid', retry: false, detail: {} });
  assert.equal(net.hello().pending_scopes, undefined);
});

test('temporary and zero-incarnation schema/row targets resolve through pending creates', (t) => {
  const { store, net, sockets } = connection(t);
  const ops = [
    createDatabase,
    { op: 'create_property', ref: 'status', proposed_id: createdStatus, database: { tmp: 'db' }, definition: { name: 'Status', type: 'select' } },
    { op: 'create_option', ref: 'open', proposed_id: createdOption, property: { tmp: 'status' }, name: 'Open', color: 'gray' },
    { op: 'create_row', ref: 'row', proposed_id: createdRow, database: { tmp: 'db' }, cells: [{ property: { tmp: 'status' }, value: { tmp: 'open' } }], before: null, view: { tmp: 'db.view' } },
    { op: 'set_cell', row: { tmp: 'row' }, cell: { property: { tmp: 'db.title' }, value: 'Draft' } },
    { op: 'set_option', option: { tmp: 'open' }, name: 'Waiting', color: 'blue' },
    { op: 'set_view', view: { tmp: 'db.view' }, definition: { ...viewDef, display: { layout: 'table', visible: [{ tmp: 'db.title' }], widths: [] } } },
  ];
  const first = store.pending.push({ action_id: 'create', ops });
  const second = store.pending.push({ action_id: 'later', ops: [{ op: 'set_cell', row: target(createdRow, '0'), cell: { property: target(createdProp, '0'), value: 'Later' } }] });
  const third = store.pending.push({ action_id: 'option', ops: [{ op: 'set_option', option: target(createdOption, '0'), name: 'Later', color: 'red' }] });
  net.watch(pid('b'));
  expectScopes(sockets[0].frames('watch').at(-1).pending_scopes, scopes(createdDB));
  store.pending.remove(first.n);
  expectScopes(net.hello().pending_scopes, scopes(createdDB));
  store.pending.remove(second.n);
  expectScopes(net.hello().pending_scopes, scopes(createdDB));
  store.pending.remove(third.n);
  assert.equal(net.hello().pending_scopes, undefined);
});

test('real shell navigation and reconnect retain a pending cell edit until canonical settlement', async (t) => {
  const a = await databaseApp(t);
  a.app.queue([{ op: 'set_cell', row: target(rowid), cell: { property: target(title), value: 'Task edited' } }]);
  location.hash = '#/' + pid('b');
  expectScopes(a.socket().frames('watch').at(-1).pending_scopes);
  a.socket().drop();
  a.timers.advance(1000);
  const hello = a.socket().open();
  assert.equal(hello.page, pid('b'));
  expectScopes(hello.pending_scopes);
  a.socket().recv({ t: 'welcome', access: 'private', session: 'tok', session_id: 'me' });
  a.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [row(rowid, 'Task edited')], from: { session_id: 'me', n: '1' } });
  expectScopes(a.app.net.hello().pending_scopes, scopes());
  a.socket().recv(ack('1', '11'));
  assert.equal(a.app.net.hello().pending_scopes, undefined);
  location.hash = '#/' + pid('a');
  assert.deepEqual(a.socket().frames('watch').at(-1).pending_scopes, []);
});
