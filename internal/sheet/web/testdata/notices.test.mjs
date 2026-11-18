// The words of two notices: what an undo or redo skipped and why (rule 23),
// and what an agent's call did (rule 27).
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { agentNotice } from '../presence.js';
import { undoSkipText } from '../protocol.js';
import { FakeServer, key, notices, startApp, structField } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 30, cols: 6, cells: { '0,0': 'Item' } }, { id: 2, name: 'Notes', rows: 5, cols: 3 }] });
const last = (t) => notices(t).at(-1);

test('each reason an undo skips a change for has its own words, for undo and for redo', async () => {
  const t = await startApp({ server: sheet() });
  const undo = (result, redo = false) => {
    t.server.undoResult = result;
    key(t, 'z', redo ? { ctrlKey: true, shiftKey: true } : { ctrlKey: true });
    assert.equal(last(t)[0], 'warn');
    return last(t)[1];
  };
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'last_tab' }),
    'Your undo skipped 1 change: it would delete the only tab of the sheet. A sheet keeps at least one tab. Nothing was undone.');
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'last_tab' }, true),
    'Your redo skipped 1 change: it would delete the only tab of the sheet. A sheet keeps at least one tab. Nothing was redone.');
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'empty_tab' }),
    'Your undo skipped 1 change: it would delete the only rows or the only columns of a tab. A tab keeps at least one row and one column. Nothing was undone.');
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'empty_tab' }, true),
    'Your redo skipped 1 change: it would delete the only rows or the only columns of a tab. A tab keeps at least one row and one column. Nothing was redone.');
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'tab_name_taken' }),
    'Your undo skipped 1 change: another tab now has the name it would give back. Both tabs keep their names. Nothing was undone.');
  assert.equal(undo({ applied: 0, skipped: 1, reason: 'tab_name_taken' }, true),
    'Your redo skipped 1 change: another tab now has the name it would give back. Both tabs keep their names. Nothing was redone.');
  assert.equal(notices(t).length, 1, 'each notice replaces the one before');
  // the step left the history: the next undo goes out and reaches the one before
  t.server.undoResult = { applied: 2, skipped: 0 };
  const sent = t.server.socket.frames('action').length;
  key(t, 'z', { ctrlKey: true });
  assert.equal(t.server.socket.frames('action').length, sent + 1);
  assert.deepEqual(t.server.socket.frames('action').at(-1).ops, [{ op: 'undo' }]);
});

test('a tab that was recovered since, and a host that lost what it kept, have their own words', () => {
  assert.equal(undoSkipText(false, { applied: 0, skipped: 1, reason: 'recovered' }),
    'Your undo skipped 1 change: someone has recovered that tab from an earlier revision since. The recovered tab stays as it is. Nothing was undone.');
  assert.equal(undoSkipText(true, { applied: 2, skipped: 1, reason: 'recovered' }),
    'Your redo skipped 1 change: someone has recovered that tab from an earlier revision since. The recovered tab stays as it is. The rest was redone.');
  // lost as a whole step, and lost for some of the changes of a step: a
  // change is one value group of a cell, so the count is of changes, not cells
  assert.equal(undoSkipText(false, { applied: 0, skipped: 1, reason: 'lost' }),
    'This step could not be undone: the host no longer has what it had saved for it. Earlier steps are still available.');
  assert.equal(undoSkipText(false, { applied: 5, skipped: 3, reason: 'lost' }),
    'Your undo skipped 3 changes: the host no longer has what it had saved for it. The other skipped changes can have other reasons. The rest was undone.');
  assert.equal(undoSkipText(true, { applied: 0, skipped: 4, reason: 'lost' }),
    'Your redo skipped 4 changes: the host no longer has what it had saved for it. The other skipped changes can have other reasons. Nothing was redone.');
  for (const reason of ['changed', 'written', 'sorted', 'last_tab', 'empty_tab', 'tab_name_taken', 'recovered', 'lost']) {
    assert.doesNotMatch(undoSkipText(false, { applied: 1, skipped: 7, reason }), /\bcells?\b/, `${reason}: the count is of changes`);
    assert.doesNotMatch(undoSkipText(false, { applied: 1, skipped: 7, reason }), /no longer fits/, `${reason} has words of its own`);
  }
});

test('a reason the client does not know blames no one', () => {
  assert.equal(undoSkipText(false, { applied: 0, skipped: 1, reason: 'frozen_by_policy' }),
    'Your undo skipped 1 change: it no longer fits the sheet as it is now (the host says: frozen_by_policy). Nothing was undone.');
  assert.equal(undoSkipText(true, { applied: 3, skipped: 1 }),
    'Your redo skipped 1 change: it no longer fits the sheet as it is now. The rest was redone.');
  for (const reason of ['frozen_by_policy', undefined]) assert.doesNotMatch(undoSkipText(false, { applied: 0, skipped: 1, reason }), /someone/);
});

test('the notice says how much was undone, and names the one reason only for the change it belongs to', () => {
  // one reason, the rest applied
  assert.equal(undoSkipText(false, { applied: 4, skipped: 1, reason: 'changed' }),
    'Your undo skipped 1 change: someone else changed the same value after you. Their value stays. The rest was undone.');
  // several skipped changes: the ack names the reason of the first; the others can differ
  assert.equal(undoSkipText(false, { applied: 1, skipped: 2, reason: 'written' }),
    'Your undo skipped 2 changes: someone else has written in the rows, columns or tab it would remove. They stay, with their work. The other skipped change can have another reason. The rest was undone.');
  assert.equal(undoSkipText(true, { applied: 2, skipped: 5, reason: 'tab_name_taken' }),
    'Your redo skipped 5 changes: another tab now has the name it would give back. Both tabs keep their names. The other skipped changes can have other reasons. The rest was redone.');
  // everything skipped
  assert.equal(undoSkipText(false, { applied: 0, skipped: 3, reason: 'sorted' }),
    'Your undo skipped 3 changes: someone else sorted these rows after you. Their order stays. The other skipped changes can have other reasons. Nothing was undone.');
  assert.equal(undoSkipText(false, { applied: 0, skipped: 1, reason: 'lost' }),
    'This step could not be undone: the host no longer has what it had saved for it. Earlier steps are still available.');
});

test('the agent notice says what the call did and never counts zero cells', async () => {
  const t = await startApp({ server: sheet() });
  const said = (fields) => { t.server.push(fields); assert.equal(last(t)[0], 'ok'); return last(t)[1]; };
  const tabs = (list) => list.map(([id, name]) => ({ id, name, rows: 5, cols: 3, frozen_rows: 0, frozen_cols: 0 }));
  // a call that only deletes a tab
  assert.equal(said({ struct: [{ op: 'delete_tab', tab: 2 }], tabs: tabs([[1, 'Budget']]), note: { agent: true, tab: 0, cells: 0 } }),
    'The agent deleted the tab “Notes”.');
  // cells, as before
  assert.equal(said({ tab: 1, cells: [[t.model.rowAt(2), [[t.model.colAt(0), 't', 'x']]]], note: { agent: true, tab: 1, cells: 42 } }),
    'The agent wrote 42 cells in Budget.');
  assert.equal(said({ tab: 1, cells: [[t.model.rowAt(2), [[t.model.colAt(0), 't', 'y']]]], note: { agent: true, tab: 1, cells: 1 } }),
    'The agent wrote 1 cell in Budget.');
  // tabs added, renamed and moved
  assert.equal(said({ struct: [{ op: 'add_tab', tab: 3 }], tabs: tabs([[1, 'Budget'], [3, 'Costs']]), note: { agent: true, tab: 0, cells: 0 } }),
    'The agent added the tab “Costs”.');
  assert.equal(said({ struct: [{ op: 'rename_tab', tab: 3, name: 'Q1 costs' }], tabs: tabs([[1, 'Budget'], [3, 'Q1 costs']]), note: { agent: true, tab: 0, cells: 0 } }),
    'The agent renamed the tab “Costs” to “Q1 costs”.');
  assert.equal(said({ struct: [{ op: 'move_tab', tab: 3 }], tabs: tabs([[3, 'Q1 costs'], [1, 'Budget']]), note: { agent: true, tab: 0, cells: 0 } }),
    'The agent moved the tab “Q1 costs”.');
  // rows and columns, on the watched tab and on another
  const row = t.model.rowAt(5);
  assert.equal(said({ tab: 1, struct: [{ op: 'insert_rows', tab: 1, ids: [[9000, 3]], after: row }], note: { agent: true, tab: 0, cells: 0 } }),
    'The agent inserted 3 rows in Budget.');
  assert.equal(said({ struct: structField([{ op: 'delete_cols', tab: 3, ids: [[9100, 1], [9105, 1]] }], 1), touched: [3], note: { agent: true, tab: 0, cells: 0 } }),
    'The agent deleted 2 columns in Q1 costs.');
  assert.equal(said({ tab: 1, struct: [{ op: 'sort', tab: 1, positions: [[0, 2]], ids: [t.model.rowAt(1), t.model.rowAt(0)] }], note: { agent: true, tab: 0, cells: 0 } }),
    'The agent sorted rows in Budget.');
  // several things in one call stay one line
  assert.equal(said({ tab: 1, struct: [{ op: 'add_tab', tab: 4 }, { op: 'insert_rows', tab: 1, ids: [[9200, 2]], after: row }], tabs: tabs([[3, 'Q1 costs'], [1, 'Budget'], [4, 'New']]),
    cells: [[row, [[t.model.colAt(0), 't', 'z']]]], note: { agent: true, tab: 1, cells: 7 } }),
  'The agent wrote 7 cells in Budget, added the tab “New” and inserted 2 rows in Budget.');
  // a call that changed formats or widths only
  assert.equal(said({ tab: 1, widths: [[t.model.colAt(0), 200]], note: { agent: true, tab: 0, cells: 0 } }), 'The agent changed the sheet.');
  for (const [, text] of notices(t)) assert.doesNotMatch(text, /\b0 cells?\b/);
});

test('an agent call with many kinds of change names three and says there were more', () => {
  const before = [{ id: 1, name: 'A' }, { id: 2, name: 'B' }, { id: 3, name: 'C' }];
  const after = [{ id: 1, name: 'A' }, { id: 4, name: 'D' }, { id: 5, name: 'E' }];
  const edits = [
    { op: 'add_tab', tab: 4 }, { op: 'add_tab', tab: 5 }, { op: 'delete_tab', tab: 2 }, { op: 'delete_tab', tab: 3 },
    { op: 'insert_rows', tab: 1, ids: [[10, 2]], after: null }, { op: 'insert_rows', tab: 4, ids: [[20, 1]], after: null },
    { op: 'delete_rows', tab: 1, ids: [[30, 4]] },
  ];
  assert.equal(agentNotice({ tab: 1, cells: 12 }, edits, before, after), 'The agent wrote 12 cells in A, added 2 tabs, deleted 2 tabs and made other changes.');
  assert.equal(agentNotice({ tab: 0, cells: 0 }, edits.slice(4), before, after), 'The agent inserted 3 rows and deleted 4 rows in A.');
  // a tab that was added and renamed in the same call has no earlier name
  assert.equal(agentNotice({ tab: 0, cells: 0 }, [{ op: 'rename_tab', tab: 9, name: 'Fresh' }], before, after), 'The agent renamed a tab to “Fresh”.');
  assert.equal(agentNotice({ tab: 9, cells: 3 }, [], before, after), 'The agent wrote 3 cells.', 'cells of a tab that is gone are still counted');
});
