'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeServer, key, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 6, cells: { '0,0': 'Item' } }, { id: 2, name: 'Notes', rows: 5, cols: 3 }] });
const people = (t, list) => t.server.socket.recv({ type: 'presence', people: list });

test('this tab tells the others its tab, active cell, range and whether it is editing', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  assert.deepEqual(t.server.presence[0], { type: 'presence', tab: 1, active: [m.rowAt(0), m.colAt(0)], editing: false });
  key(t, 'ArrowDown');
  key(t, 'ArrowDown', { shiftKey: true });
  key(t, 'ArrowRight', { shiftKey: true });
  t.timers.advance(60);
  assert.deepEqual(t.server.presence.at(-1), { type: 'presence', tab: 1, active: [m.rowAt(1), m.colAt(0)], range: { rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 2]] }, editing: false });
  const sent = t.server.presence.length;
  t.app.changed();
  t.timers.advance(60);
  assert.equal(t.server.presence.length, sent, 'an unchanged selection is not sent again');
  type(t, 'x');
  t.timers.advance(60);
  assert.equal(t.server.presence.at(-1).editing, true);
});

test('other people show as outlines with their name, and a cell being edited is marked', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  people(t, [
    { session_id: 's1', label: 'Ann', verified: true, color: 0, tab: 1, active: [m.rowAt(0), m.colAt(0)] },
    { session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [m.rowAt(3), m.colAt(2)], range: { rows: [[m.rowAt(3), 3]], cols: [[m.colAt(2), 2]] } },
    { session_id: 's8', label: 'Jun', verified: false, color: 1, tab: 1, active: [m.rowAt(8), m.colAt(1)], editing: true },
    { session_id: 's9', label: 'Dana', verified: true, color: 2, tab: 2, active: [5000, 5001] },
  ]);
  const cell = (r, c) => t.grid.rowEls.get(m.rowAt(r))._c[c];
  assert.equal(cell(3, 2).className, 'gc eg pc1 et el');
  assert.equal(cell(5, 3).className, 'gc eg pc1 eb er');
  assert.equal(cell(4, 2).className, 'gc eg pc1 el');
  assert.equal(cell(3, 2).querySelector('.who').textContent, 'Mina', 'the name goes with the color');
  assert.equal(cell(8, 1).className, 'gc eg pc2 et eb el er');
  assert.equal(cell(8, 1).querySelector('.who').textContent, 'Jun is editing');
  assert.equal(t.grid.activeEl.querySelector('.who'), null, 'the own session is left out');
  // a tab someone else has open is marked in the tab bar; a person on another tab leaves no outline here
  assert.ok(t.app.tabs.list.children[0].querySelector('.pdot'));
  assert.ok(t.app.tabs.list.children[1].querySelector('.pdot'));
  assert.equal(t.grid.el.querySelectorAll('.pc3').length, 0);
  // moving onto a cell someone edits says so: the mark is not a lock
  t.sel.set(8, 1);
  t.app.changed();
  assert.equal(t.grid.input.getAttribute('aria-label'), 'B9, blank, edited by Jun');
  type(t, 'mine');
  assert.equal(t.editor.open, true);
});

test('the people button names everyone and marks unverified names', async () => {
  const t = await startApp({ server: sheet() });
  people(t, [{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 2, active: [1, 1] }, { session_id: 's8', label: 'jun', verified: false, color: 1, tab: 1, active: [1, 1], editing: true }]);
  const btn = t.app.facesEl.querySelector('button');
  assert.equal(btn.getAttribute('aria-label'), 'People here: Ann (you), Mina, jun');
  assert.deepEqual(btn.children.map((el) => [el.textContent, el.className]), [['A', 'face me'], ['M', 'face pcb1'], ['J', 'face pcb2 unv']]);
  btn.click();
  const dialog = t.doc.querySelector('.pop.people');
  assert.equal(dialog.getAttribute('role'), 'dialog');
  assert.equal(dialog.getAttribute('aria-label'), 'People here');
  assert.equal(dialog.querySelector('h2').textContent, '3 people here');
  assert.deepEqual(dialog.querySelectorAll('.p').map((el) => el.textContent), ['AAnn (you) · Verified', 'MMina · VerifiedOn Notes', 'Jjun · Unverified nameOn Budget, editing a cell']);
});

test('presence follows a person through inserts, and leaves with a dropped connection list', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  const row = m.rowAt(3);
  people(t, [{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [row, m.colAt(2)] }]);
  t.server.insertRows(1, 0, 2);
  assert.ok(t.grid.rowEls.get(row)._c[2].classList.contains('eg'), 'the outline is by ids, so it stays on its cell');
  assert.equal(m.rowIndex(row), 5);
  people(t, []);
  assert.equal(t.grid.rowEls.get(row)._c[2].classList.contains('eg'), false);
});

test('a range costs what is on screen, whatever it names: huge, overlapping and stale runs', async () => {
  const { work, cleanRuns } = await import('../presence.js');
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Large', rows: 5000, cols: 60 }] });
  const t = await startApp({ server, width: 46 + 8 * 104, height: 28 + 12 * 28 });
  const m = t.model;
  const tab = server.tab(1);
  const everyRow = [[tab.rows[0], tab.rows.length]];
  const everyCol = [[tab.cols[0], tab.cols.length]];
  const classes = (r, c) => { const row = t.grid.rowEls.get(m.rowAt(r)); const el = row && [...row._f, ...row._c].find((x) => x.getAttribute('aria-colindex') === String(c + 2)); return el ? el.className : null; };
  const paintWith = (list) => { work.steps = 0; people(t, list); return work.steps; };
  const drawn = () => t.grid.rowEls.size * Math.max(...[...t.grid.rowEls.values()].map((row) => row._c.length));

  // a run of a billion ids, most of which no tab ever had
  const huge = paintWith([{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [m.rowAt(2), m.colAt(1)], range: { rows: [[1, 1000000000]], cols: [[m.colAt(1), 1]] } }]);
  assert.ok(huge < 20 * drawn(), `a billion ids cost ${huge} steps for ${drawn()} drawn cells`);
  assert.equal(classes(2, 1), 'gc eg pc1 el er', 'every row of column B is in the range');
  assert.equal(classes(0, 1), 'gc eg pc1 et el er');
  assert.equal(classes(2, 2), 'gc');

  // a normal selection of the whole tab: one run for the rows, one for the columns
  const whole = paintWith([{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [m.rowAt(0), m.colAt(0)], range: { rows: everyRow, cols: everyCol } }]);
  assert.ok(whole < 20 * drawn(), `a whole tab of 5,000 rows cost ${whole} steps`);
  assert.equal(t.grid.activeEl.className, 'gc act eg pc1 et el', 'the range starts in the own active cell');
  assert.equal(classes(0, 1), 'gc eg pc1 et');
  assert.equal(classes(3, 0), 'gc eg pc1 el');
  assert.equal(classes(5, 4), 'gc eg pc1', 'inside the range no edge is drawn');
  assert.equal(t.grid.rowEls.get(m.rowAt(0))._c[1].querySelector('.who'), null);
  // scrolled to the end, the range is drawn there without another message
  t.grid.el.scrollTop = 4990 * 28;
  t.grid.el.scrollLeft = 55 * 104;
  work.steps = 0;
  t.grid.paint();
  assert.ok(work.steps < 20 * drawn());
  assert.equal(classes(4999, 59), 'gc eg pc1 eb er');
  t.grid.el.scrollTop = 0;
  t.grid.el.scrollLeft = 0;

  // 64 runs that overlap each other and reach far outside the tab, with ids that left the tab
  const gone = tab.rows[3];
  server.deleteRows(1, 3, 1);
  const overlapping = Array.from({ length: 64 }, (_, i) => [tab.rows[0] + i, 500000000]);
  const many = paintWith(Array.from({ length: 40 }, (_, i) => ({ session_id: `p${i}`, label: `P${i}`, verified: true, color: i, tab: 1, active: [m.rowAt(1), m.colAt(1)], range: { rows: [...overlapping.slice(1), [gone, 1]], cols: overlapping } })));
  assert.ok(many < 40 * 20 * drawn(), `40 people with 64 overlapping runs each cost ${many} steps`);
  assert.deepEqual(cleanRuns(overlapping), [[tab.rows[0], tab.rows[0] + 63 + 500000000]], 'overlapping runs are one span');
  assert.equal(classes(0, 1), 'gc', 'the first row is in no run');
  assert.equal(classes(1, 1), 'gc eg pc1 et', 'the range starts under it and covers every column');
  assert.equal(classes(2, 1), 'gc eg pc1');

  // "all" is every row or every column. The range is drawn over the cells
  // on screen that lie in it, and none of its ids are listed
  const mina = (range) => [{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [m.rowAt(2), m.colAt(1)], range }];
  const column = paintWith(mina({ rows: 'all', cols: [[m.colAt(1), 1]] }));
  assert.ok(column < 20 * drawn(), `a whole column of 5,000 rows cost ${column} steps`);
  assert.equal(classes(0, 1), 'gc eg pc1 et el er', 'column B is outlined from the first row on');
  assert.equal(classes(2, 1), 'gc eg pc1 el er');
  assert.equal(classes(7, 1), 'gc eg pc1 el er', 'and on every row that is drawn');
  assert.equal(classes(2, 0), 'gc', 'no other column is in the range');
  assert.equal(classes(2, 2), 'gc');
  const line = paintWith(mina({ rows: [[m.rowAt(2), 1]], cols: 'all' }));
  assert.ok(line < 20 * drawn(), `a whole row of 60 columns cost ${line} steps`);
  assert.equal(classes(2, 0), 'gc eg pc1 et eb el', 'row 3 is outlined from the first column on');
  assert.equal(classes(2, 4), 'gc eg pc1 et eb');
  assert.equal(classes(1, 4), 'gc', 'no other row is in the range');
  assert.equal(classes(3, 4), 'gc');
  paintWith(mina({ rows: 'all', cols: 'all' }));
  assert.equal(t.grid.activeEl.className, 'gc act eg pc1 et el', 'every row and every column is the whole tab, from the own active cell on');
  assert.equal(classes(0, 1), 'gc eg pc1 et');
  assert.equal(classes(5, 4), 'gc eg pc1');
  t.grid.el.scrollTop = 4990 * 28;
  t.grid.el.scrollLeft = 55 * 104;
  work.steps = 0;
  t.grid.paint();
  assert.ok(work.steps < 20 * drawn(), 'scrolled to the end it costs what is on screen');
  assert.equal(classes(4998, 59), 'gc eg pc1 eb er', 'and reaches the last row and column');
  t.grid.el.scrollTop = 0;
  t.grid.el.scrollLeft = 0;
  assert.deepEqual(cleanRuns('all'), [[0, Infinity]], 'all is one span that holds every id');

  // runs that are no runs fall back to the active cell
  for (const rows of [[[1, -5]], [[1.5, 2]], [['1', 2]], [[1]], 'some', Array.from({ length: 65 }, (_, i) => [i + 1, 1]), [[1, Number.MAX_SAFE_INTEGER + 2]]]) {
    people(t, [{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: [m.rowAt(2), m.colAt(1)], range: { rows, cols: everyCol } }]);
    assert.equal(classes(2, 1), 'gc eg pc1 et eb el er', JSON.stringify(rows).slice(0, 40));
    assert.equal(classes(2, 2), 'gc');
  }
  // an active cell that is not a pair is ignored
  people(t, [{ session_id: 's7', label: 'Mina', verified: true, color: 0, tab: 1, active: 7, range: { rows: everyRow, cols: everyCol } }]);
  assert.equal(classes(2, 1), 'gc');
});

test('the client never sends a presence frame the host refuses', async () => {
  // The scripted server holds every presence frame to the engine's rule and
  // fails the test on one it would refuse.
  const { presenceMessage } = await import('../presence.js');
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Large', rows: 5000, cols: 60, cells: { '0,0': 'x' } }, { id: 2, name: 'Other', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  const m = t.model;
  const tab = server.tab(1);
  const said = () => { t.app.changed(); t.timers.advance(60); return server.presence.at(-1); };

  // a whole tab: one run of rows and one of columns
  key(t, 'a', { ctrlKey: true });
  assert.deepEqual(said().range, { rows: [[tab.rows[0], 5000]], cols: [[tab.cols[0], 60]] });

  // a range across rows that someone deletes: only rows that are still there, each once
  t.sel.range(2, 0, 8, 1);
  assert.deepEqual(said().range.rows, [[tab.rows[2], 7]]);
  server.deleteRows(1, 4, 2);
  const across = said().range.rows;
  assert.deepEqual(across, [[m.rowAt(2), 2], [m.rowAt(4), 3]], 'the deleted rows are not named');
  assert.equal(across.flatMap(([first, count]) => Array.from({ length: count }, (_, i) => first + i)).every((id) => m.rowIndex(id) >= 0), true);

  // rows left in more than 64 runs: the range is not sent, the active cell is
  for (let i = 0; i < 70; i++) server.deleteRows(1, 10 + i, 1);
  t.sel.range(0, 0, 200, 0);
  const many = said();
  assert.equal(many.range, undefined, 'a selection in 71 runs tells the active cell only');
  assert.deepEqual(many.active, [m.rowAt(0), m.colAt(0)]);
  // and so does a whole tab whose rows a sort has shuffled
  server.sortRows(1);
  key(t, 'a', { ctrlKey: true });
  assert.equal(said().range, undefined);

  // a selection made before a new snapshot replaced the rows: nothing is
  // said from the stale grid, and afterwards only ids of the new rows
  t.sel.range(1, 0, 3, 0);
  said();
  const before = server.presence.length;
  const ws = server.socket;
  const recv = ws.recv;
  ws.recv = () => {};
  server.deleteRows(1, 0, 4);
  ws.recv = recv;
  ws.recv({ type: 'resync', generation: t.net.generation + 3, seq: String(server.seq), date: server.date, tabs: server.tabInfo() });
  key(t, 'ArrowDown', { shiftKey: true });
  t.app.changed();
  t.timers.advance(60);
  assert.equal(server.presence.length, before, 'a stale grid says nothing');
  server.snapshot(ws, t.net.generation, 1);
  t.sel.range(1, 0, 3, 0);
  const after = said();
  assert.ok(server.presence.length > before);
  assert.equal(after.range.rows.every(([first]) => m.rowIndex(first) >= 0), true);

  // after a reconnect the same selection is told again: the new connection knows nothing
  const told = server.presence.length;
  server.socket.drop();
  t.timers.advance(1000);
  server.socket.open();
  assert.deepEqual(said(), after);
  assert.ok(server.presence.length > told);

  // a tab without a cell to name says nothing
  assert.equal(presenceMessage({ tab: { id: 1 }, rowAt: () => undefined, colAt: () => 5 }, { active: () => ({ r: 0, c: 0 }), single: true }, false), null);
});
