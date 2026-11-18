// A null in a server frame (TECH 12.5, Appendix A) is refused where the
// page would read it, at whatever depth it stands: a structural edit that
// names its tab as null is not read as one of the watched tab, a label
// that is null is not read as no label, and an answer whose id or number
// is null does not leave the request it answers waiting. The two nulls the
// appendix allows, the `after` of an insert at the top and a CellOut's
// skipped items, are taken. Each case is played through the whole client.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { nullPath } from '../protocol.js';
import { FakeServer, key, notices, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 6, cols: 3, cells: { '0,0': 'top', '5,0': 'bottom' } }, { id: 2, name: 'Other', rows: 4, cols: 2 }] });
const raw = (t, m) => t.server.socket.onmessage({ data: JSON.stringify(m) });

test('the nulls Appendix A allows are found apart from every other one', () => {
  const cell = [7, 't', 'x', null, 'edit'];
  const allowed = [
    { type: 'delta', seq: '11', struct: [{ op: 'insert_rows', tab: 1, ids: [[40, 1]], after: null }] },
    { type: 'delta_part', seq: '11', part: 0, parts: 2, cells: [[3, [cell, [8, 'n', null, null, null, 2]]]] },
    { type: 'rows', generation: 1, tab: 1, part: 0, rows: [[3, [cell]]] },
  ];
  for (const m of allowed) assert.equal(nullPath(m), null, JSON.stringify(m));
  const refused = [
    [{ type: 'delta', seq: '11', struct: [{ op: 'delete_rows', tab: null, ids: [[40, 1]] }] }, 'struct.0.tab'],
    [{ type: 'delta', seq: '11', struct: [{ op: 'delete_rows', tab: 1, ids: [[40, null]] }] }, 'struct.0.ids.0.1'],
    [{ type: 'delta', seq: '11', cells: [[3, [[null, 't', 'x']]]] }, 'cells.0.1.0.0'],
    [{ type: 'delta', seq: '11', cells: [[3, [[7, 't', 'x', null, null, null]]]] }, 'cells.0.1.0.5'],
    [{ type: 'delta', seq: '11', cells: [[null, []]] }, 'cells.0.0'],
    [{ type: 'delta', seq: '11', tabs: [{ id: 1, name: null, rows: 6, cols: 3, frozen_rows: 0, frozen_cols: 0 }] }, 'tabs.0.name'],
    [{ type: 'delta', seq: '11', from: { session_id: 's1', n: null } }, 'from.n'],
    [{ type: 'delta', seq: '11', note: { agent: true, tab: null, cells: 1 } }, 'note.tab'],
    [{ type: 'rows', generation: 1, tab: 1, part: 0, rows: [[3, [[7, null, 'x']]]] }, 'rows.0.1.0.1'],
    [{ type: 'tab_head', generation: 1, tab: 1, seq: '10', rows: [[1, 6]], cols: [[1, 3]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{ bold: null }] }, 'styles.0.bold'],
    [{ type: 'welcome', access: 'private', seq: '10', me: { label: null, verified: false } }, 'me.label'],
    [{ type: 'ack', n: '1', seq: '11', results: [{ status: 'applied', reason: null }] }, 'results.0.reason'],
    // `after` and a CellOut's items are allowed in those places only.
    [{ type: 'rows', generation: 1, tab: 1, part: 0, rows: [[3, [cell]]], after: null }, 'after'],
    [{ type: 'delta', seq: '11', struct: [{ op: 'rename_tab', tab: 1, name: 'x', after: null }] }, 'struct.0.after'],
  ];
  for (const [m, at] of refused) assert.equal(nullPath(m), at, JSON.stringify(m));
});

// Each delta would change the grid in a way the page would read wrongly:
// a deletion of the watched tab's row, a tab renamed to nothing, an author
// whose action number is null.
const BAD_DELTAS = {
  'a structural edit whose tab is null': (t) => ({ type: 'delta', seq: '11', struct: [{ op: 'delete_rows', tab: null, ids: [[t.model.rowAt(0), 1]] }] }),
  'a structural edit without a tab': (t) => ({ type: 'delta', seq: '11', struct: [{ op: 'delete_rows', ids: [[t.model.rowAt(0), 1]] }] }),
  'a structural edit of no known operation': (t) => ({ type: 'delta', seq: '11', struct: [{ op: 'drop_rows', tab: 1, ids: [[t.model.rowAt(0), 1]] }] }),
  'a delta part whose structural edit names its tab as null': (t) => ({ type: 'delta_part', seq: '11', part: 0, parts: 1, struct: [{ op: 'delete_rows', tab: null, ids: [[t.model.rowAt(0), 1]] }] }),
  'a tab list entry whose name is null': () => ({ type: 'delta', seq: '11', tabs: [{ id: 1, name: null, rows: 6, cols: 3, frozen_rows: 0, frozen_cols: 0 }] }),
  'an author whose action number is null': () => ({ type: 'delta', seq: '11', from: { session_id: 's1', n: null } }),
  'a cell whose column is null': (t) => ({ type: 'delta', seq: '11', cells: [[t.model.rowAt(0), [[null, 't', 'x']]]] }),
};

for (const [name, make] of Object.entries(BAD_DELTAS)) {
  test(`${name} is refused before any of it is taken, and a new snapshot replaces the grid`, async () => {
    const t = await startApp({ server: sheet() });
    t.sel.set(1, 1);
    type(t, 'draft');
    const rows = t.model.tab.rows.slice();
    assert.throws(() => raw(t, make(t)), TypeError, name);
    assert.deepEqual(t.model.tab.rows, rows, 'no row was deleted');
    assert.deepEqual([t.model.tabs[0].name, t.model.seq], ['Data', '10'], 'nothing of the delta was installed');
    assert.equal(t.model.stale, true, 'the grid is behind');
    assert.equal(t.net.open, false, 'the connection ended');
    assert.ok(notices(t).some(([, , kept]) => kept === 'draft'), 'the editor gave its text back');
    t.timers.advance(1000);
    assert.equal('watermark' in t.server.socket.open(), false, 'the next connection resumes nothing');
    assert.deepEqual([t.model.stale, t.model.tab.loading, t.app.canWrite()], [false, false, true]);
  });
}

test('an insert at the top, whose after is null, and a cell that skips its shown text are taken', async () => {
  const t = await startApp({ server: sheet() });
  const first = t.model.rowAt(0);
  raw(t, { type: 'delta', seq: '11', struct: [{ op: 'insert_rows', tab: 1, ids: [[900, 1]], after: null }], cells: [[first, [[t.model.colAt(1), 't', 'x', null, '\'x']]]] });
  assert.deepEqual([t.model.rowAt(0), t.model.rowAt(1)], [900, first]);
  assert.deepEqual([t.model.shown(first, t.model.colAt(1)), t.model.cellAt(1, 1).edit], ['x', '\'x']);
  assert.deepEqual([t.model.seq, t.model.stale, t.net.open], ['11', false, true]);
});

test('a status whose calculating mark is null is not read as the end of a calculation', async () => {
  const t = await startApp({ server: sheet() });
  raw(t, { type: 'status', calculating: true });
  assert.equal(t.app.calculating, true);
  assert.throws(() => raw(t, { type: 'status', calculating: null }), TypeError);
  assert.equal(t.app.calculating, true);
  assert.equal(t.net.open, true, 'the connection goes on');
});

test('an ack whose action number is null leaves no action waiting: the next connection sends it again and its receipt answers it', async () => {
  const t = await startApp({ server: sheet() });
  // The host commits the action and keeps its ack back.
  t.server.holdAcks = true;
  t.sel.set(2, 1);
  type(t, 'once');
  key(t, 'Enter');
  assert.deepEqual([t.net.pending.length, t.server.applied.length], [1, 1]);
  const [n] = t.net.pending.map((a) => a.n);
  assert.throws(() => raw(t, { type: 'ack', n: null, seq: '11', results: [{ status: 'applied', applied: 1 }] }), TypeError);
  assert.equal(t.net.open, false, 'the connection ended');
  assert.equal(t.net.pending.length, 1, 'the action is still pending, and Saved stays off');
  assert.equal(t.net.status, 'offline');
  t.timers.advance(1000);
  const ws = t.server.socket;
  ws.open();
  assert.deepEqual(ws.frames('action').map((a) => a.n), [n], 'the action went out again with its number');
  assert.equal(t.net.pending.length, 0, 'and its receipt answered it');
  assert.equal(t.server.applied.length, 1, 'it applied once');
  assert.equal(t.net.status, 'saved');
});

test('an ack whose number does not fit ends the connection even when it matches no action', async () => {
  const t = await startApp({ server: sheet() });
  for (const n of [7, '', 'one']) {
    assert.throws(() => raw(t, { type: 'ack', n, seq: '11', results: [] }), TypeError, JSON.stringify(n));
    assert.equal(t.net.open, false);
    t.timers.advance(30000);
    t.server.socket.open();
    assert.equal(t.net.open, true);
  }
});
