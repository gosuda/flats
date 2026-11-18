// Runs the scripted server through the cases the engine is checked against
// (fakeconform_test.go) and prints what it did as JSON: what came of the
// text of each frame of frames.json and what the frame left in the sheet,
// what a hello with an unknown session gets, and the format lists two
// connections on different tabs receive.
'use strict';

import { readFileSync } from 'node:fs';
import { DEFAULT_WIDTH } from '../protocol.js';
import { FakeServer, Refused } from './server.mjs';

const HELLO = { type: 'hello', version: 1, name: 'Ann', pending: 0 };

function dial(server, hello = HELLO) {
  const ws = server.connect();
  ws.got = [];
  ws.onmessage = (ev) => ws.got.push(JSON.parse(ev.data));
  ws.send(JSON.stringify(hello));
  return ws;
}

const sheet = () => new FakeServer({ me: null, tabs: [{ id: 1, name: 'Sheet1', rows: 4, cols: 3, cells: { '0,0': 'a' } }, { id: 2, name: 'Two', rows: 4, cols: 3 }] });

// ---- the frame table ----

// The sheet of the table: a, b, c and d in A1:B2 of the first tab, and a
// second tab, as the engine's test builds it. Both tabs have the 1,000
// rows and 26 columns of a new tab on the engine, so "all" names as many
// here as there.
const tableSheet = () => new FakeServer({ me: null, newTab: { rows: 1000, cols: 26 }, tabs: [{ id: 1, name: 'Sheet1', rows: 1000, cols: 26, cells: { '0,0': 'a', '0,1': 'b', '1,0': 'c', '1,1': 'd' } }, { id: 2, name: 'Other', rows: 1000, cols: 26 }] });

// typedText is the text of a cell as a person would type it.
const typedText = (c) => (c == null ? '' : c.formula ?? (c.value == null ? '' : String(c.value)));
const stateOf = (server) => JSON.stringify([server.seq, server.tabs.map((t) => [t.id, t.name, t.rows, t.cols, t.frozen_rows, t.frozen_cols, t.widths, [...t.cells]])]);
// columnOf is the position of a column by its letters: A is 0, Z 25, AA 26.
const columnOf = (letters) => [...letters].reduce((n, ch) => n * 26 + ch.charCodeAt(0) - 64, 0) - 1;
const lettersOf = (c) => (c < 26 ? '' : String.fromCharCode(64 + Math.floor(c / 26))) + String.fromCharCode(65 + (c % 26));
// units writes a text as its UTF-16 code units, four hex digits each. The
// text itself would not do: what reads this output as JSON turns half of a
// surrogate pair into U+FFFD, and so would hide a text that still holds
// the half.
const units = (text) => Array.from({ length: text.length }, (_, i) => text.charCodeAt(i).toString(16).padStart(4, '0')).join('');
const cellAt = (server, at) => {
  const [, letters, digits] = /^([A-Z]+)([0-9]+)$/.exec(at);
  return server.cell(server.tabs[0].id, Number(digits) - 1, columnOf(letters));
};

const rows = {};
for (const row of JSON.parse(readFileSync(new URL('./frames.json', import.meta.url), 'utf8'))) {
  if (row.kind === 'call') continue; // the row is an agent's call, and the scripted server speaks to a browser
  const server = tableSheet();
  const t = server.tab(1);
  // The longer names go first, because $row2 is not $row followed by a 2.
  const text = row.raw.replaceAll('$other', '2').replaceAll('$row2', String(t.rows[1])).replaceAll('$col2', String(t.cols[1]))
    .replaceAll('$tab', '1').replaceAll('$row', String(t.rows[0])).replaceAll('$col', String(t.cols[0])).replaceAll('$seq', String(server.seq));
  const out = { verdict: 'read', why: '' };
  const was = stateOf(server);
  try {
    const ws = row.kind === 'hello' ? server.connect() : dial(server);
    ws.got = [];
    ws.onmessage = (ev) => ws.got.push(JSON.parse(ev.data));
    ws.send(text);
    const fatal = ws.got.find((m) => m.type === 'fatal');
    const refused = ws.got.find((m) => m.type === 'ack')?.results.find((r) => r.status !== 'applied');
    if (fatal) out.verdict = fatal.code;
    else if (refused) out.verdict = `${refused.status}:${refused.reason}`;
  } catch (err) {
    if (!(err instanceof Refused)) throw err;
    out.verdict = err.outcome;
    out.why = err.message.slice(0, 200);
  }
  out.changed = stateOf(server) !== was;
  // What the frame left, as the table names it: the cells and formats the
  // row asks about, of the first tab, the tab list, and the widths of the
  // first tab.
  const first = server.tabs[0];
  out.cells = {};
  for (const at of Object.keys(row.cells ?? {})) out.cells[at] = units(typedText(cellAt(server, at)));
  out.formats = {};
  for (const at of Object.keys(row.formats ?? {})) out.formats[at] = server.styles[cellAt(server, at)?.style ?? 0];
  out.tabs = server.tabs.map((t) => `${t.name} ${t.rows.length}x${t.cols.length} ${t.frozen_rows},${t.frozen_cols}`);
  out.widths = {};
  first.cols.forEach((col, c) => { if (col in first.widths && first.widths[col] !== DEFAULT_WIDTH) out.widths[lettersOf(c)] = first.widths[col]; });
  rows[row.name] = out;
}

// ---- a session the server does not know ----

const expired = [
  { type: 'hello', version: 1, session: 'no-such-token', pending: 0 },
  { type: 'hello', version: 1, session: 'no-such-token', pending: 2 },
  { type: 'hello', version: 1, pending: 2 },
].map((hello) => {
  const first = dial(sheet(), hello).got[0];
  return first.code ? `${first.type} ${first.code}` : first.type;
});

// ---- the format list on two connections ----

const styles = [];
const shape = (what, f) => styles.push(`${what} styles=${JSON.stringify(f.styles ?? null)} touched=${'touched' in f} cells=${'cells' in f}`);
const last = (ws, type) => ws.got.findLast((m) => m.type === type);
const server = sheet();
const one = server.tab(1);
const a = dial(server);
a.send(JSON.stringify({ type: 'watch', generation: 1, tab: 1 }));
const b = dial(server, { ...HELLO, name: 'Ben' });
b.send(JSON.stringify({ type: 'watch', generation: 1, tab: 2 }));
shape('head of the other tab', last(b, 'tab_head'));
a.send(JSON.stringify({ type: 'action', n: '1', base: String(server.seq), ops: [{ op: 'format', tab: 1, rows: [[one.rows[0], 1]], cols: [[one.cols[0], 1]], set: { bold: true } }] }));
shape('delta on the watching connection', last(a, 'delta'));
shape('delta on the other connection', last(b, 'delta'));
b.send(JSON.stringify({ type: 'watch', generation: 2, tab: 1 }));
shape('head of the formatted tab', last(b, 'tab_head'));
const cell = last(b, 'rows').rows.find(([row]) => row === one.rows[0])[1].find(([col]) => col === one.cols[0]);
styles.push(`formatted cell style=${cell[5]}`);

// ---- structural operations on two connections ----

// structShape names what a delta says about structure: each operation with
// its fields and the tab it names, and whether the tab list and the touched
// list are there.
const structs = [];
const structShape = (what, f) => structs.push(`${what} struct=${JSON.stringify((f.struct ?? []).map((e) => [e.op, Object.keys(e).sort().join(), e.tab === 1 ? 'first tab' : `tab ${e.tab > 2 ? 'new' : e.tab}`]))} tabs=${'tabs' in f} touched=${'touched' in f}`);
b.send(JSON.stringify({ type: 'watch', generation: 3, tab: 2 }));
const act = (ws, n, op) => ws.send(JSON.stringify({ type: 'action', n, base: String(server.seq), ops: [op] }));
for (const [name, n, op] of [
  ['insert rows', '2', { op: 'insert_rows', tab: 1, beside: one.rows[0], side: 'before', count: 2 }],
  ['delete rows', '3', { op: 'delete_rows', tab: 1, ids: [[one.rows[1], 1]] }],
  ['rename tab', '4', { op: 'rename_tab', tab: 1, name: 'Renamed' }],
  ['add tab', '5', { op: 'add_tab', name: 'Third' }],
  ['move tab', '6', () => ({ op: 'move_tab', tab: 3, before: 1 })],
  ['delete tab', '7', () => ({ op: 'delete_tab', tab: 3 })],
]) {
  act(a, n, typeof op === 'function' ? op() : op);
  structShape(`${name}, watching the tab`, last(a, 'delta'));
  structShape(`${name}, watching another tab`, last(b, 'delta'));
}
process.stdout.write(JSON.stringify({ rows, expired, styles, structs }));
