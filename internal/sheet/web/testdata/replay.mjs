// Plays frames a real server sent, in the order it sent them, into the
// real client, and prints what the client holds and draws afterwards
// (clientconform_test.go). The steps come as JSON on standard input: a
// list of {"recv": "<frame text>"} and {"open": <tab id>}, the second for
// the moment the recording connection asked for another tab. The client
// runs on the fake DOM over a socket that only records what it sends.
'use strict';

import { readFileSync } from 'node:fs';
import { installDOM } from './dom.mjs';
import { FakeSocket, Timers, notices, shownAt } from './server.mjs';
import { DEFAULT_WIDTH } from '../protocol.js';

const steps = JSON.parse(readFileSync(0, 'utf8'));
const doc = installDOM({});
globalThis.sheetTest = true;
const timers = new Timers();
const { App } = await import('../app.js');
const sockets = [];
const connect = () => { const ws = new FakeSocket(null); sockets.push(ws); return ws; };
const measure = { text: (s) => s.length * 7, height: (el) => (el.textContent.split('\n').length * 18) + 10 };
const fetch = async () => { throw new Error('no network in tests'); };
// A frame the client fails to take ends the replay: what it would print
// is no state the frames gave it.
const report = (err) => { throw err; };
const app = new App({ connect, timers, now: () => timers.now, measure, raf: (fn) => fn(), functions: [], fetch, report });
app.grid.el.clientWidth = 1000;
app.grid.el.clientHeight = 400;
app.grid.el.rect = { left: 0, top: 0, width: 1000, height: 400 };
sockets[0].open();
// The longest style list the client held at any step.
let stylesPeak = 0;
for (const step of steps) {
  if (step.open !== undefined) app.openTab(step.open);
  else sockets[0].recv(JSON.parse(step.recv));
  stylesPeak = Math.max(stylesPeak, app.model.tab ? app.model.tab.styles.length : 0);
}

const model = app.model;
const t = model.tab;
const out = {
  seq: model.seq,
  date: model.date,
  tabs: model.tabs,
  sockets: sockets.length,
  styles_peak: stylesPeak,
  watches: sockets[0].frames('watch'),
  notices: notices({ app }).map(([, text]) => text),
  tab: null,
  drawn: [],
};
if (t) {
  const cells = {};
  for (const [row, list] of t.cells) {
    for (const [col, c] of list) {
      cells[`${row}/${col}`] = { kind: c.kind, formula: c.formula, value: c.value, shown: c.shown, edit: c.edit, format: model.format(c) };
    }
  }
  const widths = {};
  for (const [col, w] of t.widths) if (w !== DEFAULT_WIDTH) widths[col] = w;
  out.tab = { id: t.id, name: t.name, loading: t.loading, styles: t.styles.length, rows: t.rows, cols: t.cols, frozen_rows: t.frozenRows, frozen_cols: t.frozenCols, widths, cells };
  // What the grid drew of the first rows and columns: null for a cell
  // that is outside the drawn part.
  for (let r = 0; r < Math.min(8, model.rowCount); r++) {
    const line = [];
    for (let c = 0; c < Math.min(6, model.colCount); c++) line.push(shownAt({ grid: app.grid, doc }, r, c) ?? null);
    out.drawn.push(line);
  }
}
process.stdout.write(JSON.stringify(out));
