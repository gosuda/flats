// A page for looking at the client without a host: the real modules and
// styles against the scripted fake server of the Node tests, in place of a
// WebSocket. Serve the web directory over HTTP and open
// testdata/preview.html. The query picks the state: ?access=public,
// ?people=1, ?edit=1, ?frozen=<columns> and ?scroll=<top>,<left>.
'use strict';

import { FakeServer, cellOut } from './server.mjs';

const q = new URLSearchParams(location.search);
const money = (v) => ({ kind: 'n', value: v, shown: `$${v.toLocaleString('en-US')}`, edit: String(v), style: 2 });
const items = [
  ['Venue hire, 2 days', 'Venue', 2, 1800, 'Deposit paid'], ['Lunch catering', 'Food', 24, 32, ''], ['Dinner, day 1', 'Food', 24, 58, 'Set menu B'],
  ['Coffee and snacks', 'Food', 2, 240, ''], ['Train tickets', 'Travel', 18, 96, 'Group fare until Mar 1'], ['Shuttle bus', 'Travel', 2, 420, ''],
  ['Hotel rooms', 'Lodging', 12, 165, 'Twin rooms'], ['Facilitator', 'Program', 1, 1500, ''],
  ['Workshop materials', 'Program', 24, 12, 'Notebooks, markers and sticky notes for every table'], ['Team activity', 'Program', 24, 45, ''],
];
const cells = {};
['Item', 'Category', 'Qty', 'Unit cost', 'Amount', 'Notes'].forEach((t, c) => { cells[`0,${c}`] = { kind: 't', value: t, style: 1 }; });
items.forEach(([item, cat, qty, cost, note], i) => {
  const r = i + 1;
  cells[`${r},0`] = item;
  cells[`${r},1`] = cat;
  cells[`${r},2`] = { kind: 'n', value: qty };
  cells[`${r},3`] = money(cost);
  cells[`${r},4`] = { ...money(qty * cost), kind: 'nf', formula: `=C${r + 1}*D${r + 1}` };
  if (note) cells[`${r},5`] = r === 9 ? { kind: 't', value: note, style: 4 } : note;
});
cells['12,0'] = { kind: 't', value: 'Total', style: 1 };
cells['12,4'] = { kind: 'nf', value: 13656, shown: '$13,656', style: 3, formula: '=SUM(E2:E11)' };
cells['14,0'] = 'Share of budget';
cells['14,4'] = { kind: 'ef', value: '#DIV/0!', formula: '=E13/0' };
cells['15,0'] = 'Head count';
cells['15,4'] = { kind: 'n', value: 123456789012345 };
cells['16,0'] = 'Over the cap?';
cells['16,4'] = { kind: 't', value: 'Within cap', style: 5 };

const server = new FakeServer({
  access: q.get('access') === 'public' ? 'public' : 'private',
  tabs: [{ id: 1, name: 'Budget', rows: 1000, cols: 26, cells, frozen_rows: 1, frozen_cols: Number(q.get('frozen')) || 0 }, { id: 2, name: 'Summary', rows: 1000, cols: 26, cells: { '0,0': 'Category' } }, { id: 3, name: 'Tracker', rows: 1000, cols: 26 }],
});
const budget = server.tabs[0];
budget.styles.push({ bold: true }, { number: { kind: 'currency', symbol: '$', decimals: 0 } }, { bold: true, number: { kind: 'currency', symbol: '$', decimals: 0 } }, { wrap: true }, { fill: 'yellow-100', text_color: 'orange-800' });
budget.widths[budget.cols[0]] = 190;
budget.widths[budget.cols[5]] = 230;

globalThis.sheetTest = true;
const { App } = await import('../app.js');
const functions = await fetch('./functions.json').then((r) => r.json());
const app = new App({ functions, connect: () => { const ws = server.connect(); setTimeout(() => ws.open(), 0); return ws; } });
globalThis.sheet = app;
globalThis.sheetServer = server;

setTimeout(() => {
  if (q.get('people')) {
    server.socket.recv({ type: 'presence', people: [
      { session_id: 'x1', label: 'Mina', verified: true, color: 0, tab: 1, active: [budget.rows[3], budget.cols[2]], range: { rows: [[budget.rows[3], 3]], cols: [[budget.cols[2], 2]] } },
      { session_id: 'x2', label: 'Jun', verified: false, color: 1, tab: 1, active: [budget.rows[7], budget.cols[0]], editing: true },
    ] });
    server.push({ tab: 1, cells: [[budget.rows[18], [cellOut(budget.cols[0], { kind: 't', value: 'Added by the agent' })]]], note: { agent: true, tab: 1, cells: 1 } });
  }
  if (q.get('edit')) {
    app.selection.set(13, 4);
    app.changed();
    app.grid.input.value = '=su';
    app.grid.input.dispatchEvent(new Event('input'));
  } else {
    app.selection.range(4, 2, 4, 4);
    app.changed();
  }
  if (q.get('scroll')) {
    const [top, left] = q.get('scroll').split(',').map(Number);
    app.grid.el.scrollTop = top;
    app.grid.el.scrollLeft = left;
  }
  document.documentElement.dataset.ready = '1';
}, 50);
