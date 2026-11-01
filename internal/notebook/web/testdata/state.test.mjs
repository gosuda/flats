'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Store, Canonical, applyLocal, positionBetween, convertContent } from '../state.js';
import { page, block, para, pid, bid } from './page.mjs';

const rid = (tag) => 'r_' + tag.padEnd(26, 'a');
const did = 'd_' + 'd'.repeat(26);
const row = (tag, title, { position = 'V', inc = '5' } = {}) => ({ kind: 'row', ref: { id: rid(tag), inc }, ver: inc, v: {}, database: did, position, cells: { title } });
const dbScope = { kind: 'database', id: did };

test('a delta replaces whole items and a deletion record removes one', () => {
  const c = new Canonical();
  c.install({ kind: 'tree' }, '10', [page('a'), page('b', { position: 'k' })]);
  c.applyDelta({ seq: '11', tree: [{ ...page('a', { title: 'Renamed' }), ver: '11', v: { title: '11' } }], changes: [] });
  c.applyDelta({ seq: '12', tree: [{ deleted: { id: pid('b'), inc: '5' } }], changes: [] });
  assert.equal(c.get(pid('a')).title, 'Renamed');
  assert.equal(c.get(pid('b')), null);
  assert.equal(c.seq, '12');
});

test('delta values win over a base window captured earlier, and tombstones stay', () => {
  const c = new Canonical();
  c.install(dbScope, '20', [], { window: 0, final: false });
  // live deltas above the capture sequence arrive before the row windows
  c.applyDelta({ seq: '21', changes: [{ ...row('1', 'live'), ver: '21' }] });
  c.applyDelta({ seq: '22', changes: [{ deleted: { id: rid('2'), inc: '5' } }] });
  c.applyDelta({ seq: '23', changes: [row('9', 'created', { position: 'z', inc: '23' })] });
  c.install(dbScope, '20', [row('1', 'old'), row('2', 'gone'), row('3', 'kept')], { window: 1, final: true });
  const rows = [...c.items()].filter((i) => i.kind === 'row').map((i) => [i.ref.id, i.cells.title]).sort();
  assert.deepEqual(rows, [[rid('1'), 'live'], [rid('3'), 'kept'], [rid('9'), 'created']]);
  assert.equal(c.loading(dbScope), false);
});

test('a database stays loading until its final window and earlier windows stay readable', () => {
  const c = new Canonical();
  c.install(dbScope, '30', [], { window: 0, final: false });
  c.install(dbScope, '30', [row('1', 'one')], { window: 1, final: false });
  assert.equal(c.loading(dbScope), true);
  assert.equal(c.get(rid('1')).cells.title, 'one');
  c.install(dbScope, '30', [row('2', 'two')], { window: 2, final: true });
  assert.equal(c.loading(dbScope), false);
  assert.ok(c.get(rid('1')) && c.get(rid('2')));
});

test('a fresh capture subsumes older overlay entries and keeps newer ones', () => {
  const c = new Canonical();
  c.install({ kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('x'))]);
  c.applyDelta({ seq: '11', changes: [block('x', 'a', para('x at 11'), { v: { content: '11' } })] });
  c.applyDelta({ seq: '13', changes: [block('y', 'a', para('y at 13'), { inc: '13' })] });
  c.install({ kind: 'page', id: pid('a') }, '12', [block('x', 'a', para('x at 12'), { v: { content: '12' } })]);
  assert.equal(c.get(bid('x')).content.text[0].text, 'x at 12');
  assert.equal(c.get(bid('y')).content.text[0].text, 'y at 13', 'created after the capture, kept from the overlay');
  assert.equal(c.overlay.has(bid('x')), false);
});

test('a page capture drops base blocks it no longer contains', () => {
  const c = new Canonical();
  c.install({ kind: 'tree' }, '10', [page('a')]);
  c.install({ kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('x')), block('y', 'a', para('y'))]);
  c.install({ kind: 'page', id: pid('a') }, '14', [block('x', 'a', para('x'))]);
  assert.equal(c.get(bid('y')), null);
  assert.ok(c.get(pid('a')), 'other scopes keep their items');
});

function databaseItems(database = did, owner = bid('db')) {
  const property = { kind: 'property', ref: { id: 'pr_' + database.slice(2), inc: '5' }, database, definition: { name: 'Status', type: 'select' } };
  return [
    { kind: 'database', ref: { id: database, inc: '5' }, block: owner, title: 'Tasks' },
    property,
    { kind: 'option', ref: { id: 'o_' + database.slice(2), inc: '5' }, property: property.ref.id, name: 'Open', color: 'blue' },
    { kind: 'view', ref: { id: 'v_' + database.slice(2), inc: '5' }, database, definition: { name: 'All', display: { layout: 'table', visible: [property.ref], widths: [] }, sort: [], filter: [] } },
    { ...row('old', 'Old'), ref: { id: 'r_' + database.slice(2), inc: '5' }, database },
  ];
}

for (const kind of ['property', 'option', 'view', 'row']) {
  test(`a completed database capture removes an absent ${kind} from page and database state`, () => {
    const c = new Canonical();
    const items = databaseItems();
    const owner = block('db', 'a', { kind: 'database', database: did });
    const deleted = items.find((i) => i.kind === kind);
    const retained = items.filter((i) => i !== deleted && !(kind === 'property' && i.kind === 'option'));
    c.install({ kind: 'page', id: pid('a') }, '10', [owner, ...items.filter((i) => i.kind !== 'row')]);
    c.install(dbScope, '10', items, { window: 0, final: true });
    c.install(dbScope, '20', retained.filter((i) => i.kind !== 'row'), { window: 0, final: false });
    if (kind === 'row') assert.ok(c.get(deleted.ref.id), 'rows remain readable until their capture completes');
    else assert.equal(c.get(deleted.ref.id), null, 'the completed schema unit already conveys schema absence');
    assert.equal(c.loading(dbScope), true);
    c.install(dbScope, '20', retained.filter((i) => i.kind === 'row'), { window: 1, final: true });
    assert.equal(c.get(deleted.ref.id), null);
    assert.deepEqual([...c.items()].map((i) => i.ref.id).sort(), [owner, ...retained].map((i) => i.ref.id).sort());
    assert.equal(c.loading(dbScope), false);
  });
}

for (const scopeKind of ['page', 'database']) {
  test(`a completed ${scopeKind} capture reconciles dependent schema without losing other owners`, () => {
    const c = new Canonical();
    const items = databaseItems();
    const other = databaseItems('d_' + 'e'.repeat(26), bid('other'));
    const owner = block('db', 'a', { kind: 'database', database: did });
    const otherOwner = block('other', 'b', { kind: 'database', database: other[0].ref.id });
    c.install({ kind: 'tree' }, '10', [page('a'), page('b')]);
    c.install({ kind: 'page', id: pid('a') }, '10', [owner, ...items.filter((i) => i.kind !== 'row')]);
    c.install(dbScope, '10', items, { window: 0, final: true });
    c.install({ kind: 'page', id: pid('b') }, '10', [otherOwner, ...other.filter((i) => i.kind !== 'row')]);
    c.install({ kind: 'database', id: other[0].ref.id }, '10', other, { window: 0, final: true });
    c.install({ kind: scopeKind, id: scopeKind === 'page' ? pid('a') : did }, '20', []);
    const retained = [page('a'), page('b'), otherOwner, ...other, ...(scopeKind === 'page' ? [items.at(-1)] : [owner])];
    assert.deepEqual([...c.items()].map((i) => i.ref.id).sort(), retained.map((i) => i.ref.id).sort());
    for (const item of items.filter((i) => scopeKind !== 'page' || i.kind !== 'row')) assert.equal(c.get(item.ref.id), null);
    for (const item of other) assert.deepEqual(c.get(item.ref.id), item);
  });
}

for (const source of ['base', 'overlay']) {
  test(`a completed database capture removes absent ${source} schema despite owner deletion order`, () => {
    const c = new Canonical();
    const items = databaseItems();
    if (source === 'base') c.install(dbScope, '10', items);
    else c.applyDelta({ seq: '10', changes: items });
    c.install(dbScope, '20', []);
    for (const item of items) assert.equal(c.get(item.ref.id), null);
    assert.deepEqual([...c.items()], []);
    assert.equal(c.overlay.size, 0);
  });
}

test('a database capture preserves other database schema on the same page', () => {
  const c = new Canonical();
  const items = databaseItems();
  const other = databaseItems('d_' + 'e'.repeat(26), bid('other'));
  const owners = [block('db', 'a', { kind: 'database', database: did }), block('other', 'a', { kind: 'database', database: other[0].ref.id })];
  c.install({ kind: 'page', id: pid('a') }, '10', [...owners, ...items.slice(0, -1), ...other.slice(0, -1)]);
  c.install(dbScope, '10', items);
  c.install(dbScope, '20', [items[0]]);
  const retained = [...owners, items[0], ...other.slice(0, -1)];
  assert.deepEqual([...c.items()].map((i) => i.ref.id).sort(), retained.map((i) => i.ref.id).sort());
  for (const item of retained) assert.deepEqual(c.get(item.ref.id), item);
});

for (const kind of ['database', 'property', 'option', 'view', 'row']) {
  test(`database reconciliation preserves newer edits and creations and a ${kind} tombstone`, () => {
    const c = new Canonical();
    const items = databaseItems();
    const created = databaseItems('d_' + 'f'.repeat(26), bid('new'));
    c.install(dbScope, '10', items);
    c.install(dbScope, '20', [items[0]], { window: 0, final: false });
    const edits = items.map((i) => ({ ...i, ver: '21' }));
    const creations = created.slice(1).map((i) => i.kind === 'option' ? i : { ...i, database: did });
    c.applyDelta({ seq: '21', changes: [...edits, ...creations] });
    const deleted = items.find((i) => i.kind === kind);
    c.applyDelta({ seq: '22', changes: [{ ref: deleted.ref, deleted: true }] });
    c.install(dbScope, '20', [items.at(-1)], { window: 1, final: true });
    const retained = [...edits.filter((i) => i.kind !== kind), ...creations];
    for (const item of retained) assert.deepEqual(c.get(item.ref.id), item);
    assert.equal(c.get(deleted.ref.id), null, 'a later deletion cannot be revived by capture reconciliation');
    assert.deepEqual([...c.items()].map((i) => i.ref.id).sort(), retained.map((i) => i.ref.id).sort());
    assert.equal(c.seq, '22');
  });
}

test('pending actions overlay canonical state until their delta arrives', () => {
  const s = new Store();
  s.canonical.install({ kind: 'tree' }, '10', [page('a')]);
  s.canonical.install({ kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('server'))]);
  const a = s.pending.push({ action_id: 'a1', ops: [{ op: 'set_block', block: { id: bid('x'), inc: '5' }, content: para('mine'), base_v: '5' }] });
  assert.equal(s.view().get(bid('x')).content.text[0].text, 'mine');
  // another writer's change underneath the pending one does not show yet
  s.canonical.applyDelta({ seq: '11', changes: [block('x', 'a', para('theirs'), { v: { content: '11' } })] });
  assert.equal(s.view().get(bid('x')).content.text[0].text, 'mine');
  // the delta tagged with our action settles the overlay; the ack only removes it
  s.canonical.applyDelta({ seq: '12', changes: [block('x', 'a', para('mine'), { v: { content: '12' } })], from: { session_id: 'me', n: a.n } });
  s.pending.markApplied(a.n);
  assert.equal(s.view().get(bid('x')).content.text[0].text, 'mine');
  assert.equal(s.view().get(bid('x')).v.content, '12');
  s.pending.remove(a.n);
  assert.equal(s.pending.size, 0);
  assert.equal(s.view().get(bid('x')).content.text[0].text, 'mine');
});

test('a removed pending action leaves canonical state as the server has it', () => {
  const s = new Store();
  s.canonical.install({ kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('server'))]);
  const a = s.pending.push({ action_id: 'a1', ops: [{ op: 'delete_blocks', blocks: [{ id: bid('x'), inc: '5' }] }] });
  assert.equal(s.view().get(bid('x')), null);
  s.pending.remove(a.n);
  assert.equal(s.view().get(bid('x')).content.text[0].text, 'server');
});

test('pending action numbers increase and never change', () => {
  const s = new Store();
  const a = s.pending.push({ action_id: 'a', ops: [] }), b = s.pending.push({ action_id: 'b', ops: [] });
  s.pending.remove(a.n);
  const c = s.pending.push({ action_id: 'c', ops: [] });
  assert.deepEqual([a.n, b.n, c.n], ['1', '2', '3']);
});

test('optimistic split places the new block right after the split block', () => {
  const items = new Map();
  for (const it of [page('a'), block('x', 'a', para('one'), { position: 'V' }), block('y', 'a', para('two'), { position: 'k' })]) items.set(it.ref.id, it);
  const tmp = new Map();
  applyLocal(items, { op: 'create_block', ref: 'split', proposed_id: bid('n'), place: { page: { id: pid('a'), inc: '5' }, parent: null, after: { id: bid('x'), inc: '5' } }, content: para('new') }, tmp);
  const n = items.get(bid('n'));
  assert.ok(n.position > 'V' && n.position < 'k', n.position);
  assert.equal(tmp.get('split'), bid('n'));
});

test('optimistic kind change moves children out when the new kind holds none', () => {
  const items = new Map();
  for (const it of [page('a'), block('l', 'a', para('list', 'bullet')), block('c', 'a', para('child'), { parent: 'l' }), block('z', 'a', para('after'), { position: 'k' })]) items.set(it.ref.id, it);
  applyLocal(items, { op: 'turn_blocks', blocks: [{ id: bid('l'), inc: '5' }], kind: 'paragraph' }, new Map());
  const c = items.get(bid('c'));
  assert.equal(c.parent, null);
  assert.ok(c.position > 'V' && c.position < 'k');
});

test('a stale incarnation does not change the local display', () => {
  const items = new Map([[bid('x'), block('x', 'a', para('x'), { inc: '9' })]]);
  assert.equal(applyLocal(items, { op: 'set_block', block: { id: bid('x'), inc: '5' }, content: para('stale'), base_v: '5' }, new Map()), false);
  assert.equal(items.get(bid('x')).content.text[0].text, 'x');
});

test('conversions follow the kind table', () => {
  const marked = { kind: 'bullet', text: [{ text: 'a', marks: ['bold'], link: { url: 'https://x.test' } }] };
  assert.deepEqual(convertContent(marked, 'todo'), { kind: 'todo', text: marked.text, checked: false });
  assert.deepEqual(convertContent(marked, 'code'), { kind: 'code', text: 'a', language: '' });
  assert.deepEqual(convertContent({ kind: 'code', text: 'x\ny', language: 'go' }, 'quote'), { kind: 'quote', text: [{ text: 'x\ny', marks: [] }] });
  assert.equal(convertContent({ kind: 'divider' }, 'paragraph'), null);
  assert.equal(convertContent(marked, 'divider'), null);
});

test('positionBetween stays strictly between its bounds', () => {
  for (const [a, b] of [['', ''], ['V', 'W'], ['V', 'V1'], ['', '1'], ['zz', '']]) {
    const p = positionBetween(a, b);
    assert.ok((!a || p > a) && (!b || p < b), `${a} < ${p} < ${b}`);
    assert.ok(!p.endsWith('0'));
  }
});

test('pending scopes name the pages unconfirmed actions touch', () => {
  const s = new Store();
  s.canonical.install({ kind: 'tree' }, '10', [page('a'), page('b')]);
  s.canonical.install({ kind: 'page', id: pid('a') }, '10', [block('x', 'a', para('x'))]);
  s.pending.push({ action_id: 'a', ops: [{ op: 'set_block', block: { id: bid('x'), inc: '5' }, content: para('y'), base_v: '5' }] });
  s.pending.push({ action_id: 'b', ops: [{ op: 'set_page_title', page: { id: pid('b'), inc: '5' }, title: 'B' }] });
  assert.deepEqual(s.pendingScopes(), [{ kind: 'page', id: pid('a') }, { kind: 'page', id: pid('b') }]);
  assert.deepEqual(s.unsentText(), ['y', 'B']);
});
