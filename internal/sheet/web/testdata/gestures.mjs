// Plays frames a real server sent into the real client, as replay.mjs
// does, then makes the gestures of a person and prints, as JSON, what the
// client did for them (paste_values_test.go): {"frames": [...]}, the
// action frames it sent, {"requests": [...]}, the watch, peek and rebase
// frames it sent, {"notices": [...]}, the text of each notice on its
// page, {"kept": [...]}, the text each notice offers back or null, and
// what the gestures below note. The input is {"steps": [...],
// "gestures": [...], "loading": bool} on standard input, with the steps of
// replay.mjs, loading true when the steps end before the client can edit,
// as inside a snapshot, and these gestures:
//   {"select": [r1, c1, r2, c2]}  select a range by positions
//   {"copy": true}                copy the selection (Ctrl or Cmd+C); the
//                                 text it gave, or null, is in
//                                 {"copies": [...]}
//   {"cut": true}                 cut the selection (Ctrl or Cmd+X)
//   {"paste": "all" | "values"}   paste the last copy (Ctrl or Cmd+V, or
//                                 Ctrl or Cmd+Shift+V for values only), or
//                                 the text of "text" from another program
//   {"undo": true}                undo (Ctrl or Cmd+Z)
//   {"type": "text"}              type text into the grid or the editor
//   {"key": "Enter"}              press a key where the focus is, with
//                                 Shift for "shift": true
//   {"barType": "text"}           type text at the end of the formula bar
//   {"barInsert": [at, "text"]}   type text at position at of the formula bar
//   {"compose": "text"}           compose text at the end of the focused
//                                 text area, as an input method does
//   {"outside": true}             press the mouse outside the grid and
//                                 the formula bar
//   {"bar": true}                 press the formula bar
//   {"find": "text"}              open Find (Ctrl or Cmd+F) and type text
//                                 in its field
//   {"menu": [r, c]}              right-click the cell at (r, c)
//   {"down": {zone, r, c, x}}     press the mouse on a part of the grid:
//                                 a cell, a fill handle or a column edge
//                                 ("resize"), x pixels from the left
//   {"move": {zone, r, c, x}}     move the pressed mouse over the grid
//   {"up": true}                  let the mouse button go
//   {"click": "text"}             click the button of the open menu or
//                                 dialog, or else of the toolbar, whose
//                                 text starts with text or whose label
//                                 is text
//   {"field": [id, value]}        set a field of the open dialog
//   {"edit": true}                note the text of the open editor, or
//                                 null, in {"edits": [...]}
//   {"wait": ms}                  let the client's clock run on
//   {"drop": true}                lose the connection and open the next
//                                 one after the client's wait; its hello
//                                 is in {"hellos": [...]}
//   {"look": true}                note the names of the tabs on the tab
//                                 bar, in {"looks": [...]}
//   {"steps": [...]}              take more recorded steps, such as the
//                                 opening of another tab or the answer
//                                 to an action, on the newest connection
//   {"clipboard": "text"}         put text on the system clipboard; a read
//                                 of it waits for the next read gesture
//   {"read": true}                let the waiting clipboard reads finish
//   {"hold": true}                hold the drawing of the screen: what
//                                 arrives changes the model, and the
//                                 screen is drawn at the next draw
//   {"draw": true}                draw what was held, and stop holding
'use strict';

import { readFileSync } from 'node:fs';
import { fire, installDOM } from './dom.mjs';
import { FakeSocket, Timers, type } from './server.mjs';

const { steps, gestures, loading } = JSON.parse(readFileSync(0, 'utf8'));
const doc = installDOM({});
globalThis.sheetTest = true;
const timers = new Timers();
const { App } = await import('../app.js');
const sockets = [];
const connect = () => { const ws = new FakeSocket(null); sockets.push(ws); return ws; };
const measure = { text: (s) => s.length * 7, height: (el) => (el.textContent.split('\n').length * 18) + 10 };
const fetch = async () => { throw new Error('no network in tests'); };
const report = (err) => { throw err; };
// Frames held by a hold gesture, or null while the screen is drawn at once.
let held = null;
const raf = (fn) => (held ? held.push(fn) : fn());
const app = new App({ connect, timers, now: () => timers.now, measure, raf, functions: [], fetch, report });
app.grid.el.clientWidth = 1000;
app.grid.el.clientHeight = 400;
app.grid.el.rect = { left: 0, top: 0, width: 1000, height: 400 };
sockets[0].open();
const take = (list) => {
  for (const step of list) {
    if (step.open !== undefined) app.openTab(step.open);
    else sockets.at(-1).recv(JSON.parse(step.recv));
  }
};
take(steps);
if (sockets.length !== 1 || (!loading && !app.canWrite())) throw new Error('the client cannot edit the replayed sheet');

const clip = (text = '') => {
  const data = { 'text/plain': text };
  return { clipboardData: { setData: (k, v) => { data[k] = v; }, getData: (k) => data[k] }, data };
};
const pointer = ({ x = 0, ...hit }) => [hit, { preventDefault() {}, clientX: x, clientY: 0, button: 0 }];
const open = () => doc.querySelector('.menu') || doc.querySelector('.pop:not([hidden])') || doc.querySelector('.dialog:not([hidden])');
const buttonOf = (root, text) => root?.querySelectorAll('button').find((b) => b.textContent.startsWith(text) || b.getAttribute('aria-label') === text);
const sent = sockets[0].sent.length;
let copied = null;
const copies = [];
const looks = [];
const edits = [];
// Clipboard reads the page started, each waiting for a read gesture.
let reads = [];
app.focusGrid();
for (const g of gestures) {
  if (g.select) app.selection.range(...g.select);
  else if (g.steps) take(g.steps);
  else if (g.type != null) type({ editor: app.editor, grid: app.grid }, g.type);
  else if (g.key) fire(doc.activeElement, 'keydown', { key: g.key, shiftKey: !!g.shift });
  else if (g.barType != null) {
    app.bar.value += g.barType;
    app.bar.selectionStart = app.bar.selectionEnd = app.bar.value.length;
    fire(app.bar, 'input');
  } else if (g.barInsert) {
    const [at, text] = g.barInsert;
    app.bar.value = app.bar.value.slice(0, at) + text + app.bar.value.slice(at);
    app.bar.selectionStart = app.bar.selectionEnd = at + text.length;
    fire(app.bar, 'input');
  } else if (g.compose != null) {
    const ta = doc.activeElement;
    fire(ta, 'compositionstart');
    ta.value += g.compose;
    ta.selectionStart = ta.selectionEnd = ta.value.length;
    fire(ta, 'compositionend');
  }
  else if (g.outside) fire(doc.body, 'mousedown', { button: 0 });
  else if (g.bar) { fire(app.bar, 'mousedown', { button: 0 }); app.bar.focus(); }
  else if (g.find != null) {
    fire(doc.activeElement, 'keydown', { key: 'f', ctrlKey: true });
    app.find.input.value = g.find;
    fire(app.find.input, 'input');
  } else if (g.down) app.grid.h.down(...pointer(g.down));
  else if (g.move) app.grid.h.drag(...pointer(g.move));
  else if (g.up) app.grid.h.up({});
  else if (g.click) {
    const b = buttonOf(open(), g.click) || buttonOf(app.toolbar.el, g.click);
    if (!b) throw new Error(`no button ${g.click}`);
    b.click();
  } else if (g.field) {
    const el = open()?.querySelectorAll('select, input').find((x) => x.id === g.field[0]);
    if (!el) throw new Error(`no field ${g.field[0]}`);
    el.value = g.field[1];
    fire(el, 'change');
  } else if (g.menu) app.grid.h.context({ zone: 'cell', r: g.menu[0], c: g.menu[1] }, { preventDefault() {}, button: 2, clientX: 0, clientY: 0 });
  else if (g.edit) edits.push(app.editor.open ? app.editor.text : null);
  else if (g.wait) timers.advance(g.wait);
  else if (g.look) looks.push(app.tabs.el.querySelectorAll('[role="tab"]').map((el) => el.textContent));
  else if (g.drop) {
    const n = sockets.length;
    sockets.at(-1).drop();
    for (let waited = 0; sockets.length === n && waited < 60000; waited += 1000) timers.advance(1000);
    if (sockets.length === n) throw new Error('the client did not connect again');
    sockets.at(-1).open();
  }
  else if (g.clipboard != null) {
    const text = g.clipboard;
    globalThis.navigator.clipboard.readText = () => new Promise((done) => { reads.push(() => done(text)); });
  } else if (g.read) {
    const waiting = reads;
    reads = [];
    for (const done of waiting) done();
    for (let i = 0; i < 8; i++) await new Promise((r) => setImmediate(r));
  } else if (g.copy || g.cut) {
    const e = clip(null);
    fire(doc, g.cut ? 'cut' : 'copy', e);
    copies.push(e.data['text/plain']);
    if (e.data['text/plain'] != null) copied = e.data['text/plain'];
  } else if (g.paste) {
    if (g.paste === 'values') fire(doc.activeElement, 'keydown', { key: 'v', ctrlKey: true, shiftKey: true });
    fire(doc, 'paste', clip(g.text ?? copied));
  } else if (g.undo) fire(doc.activeElement, 'keydown', { key: 'z', ctrlKey: true });
  else if (g.hold) held ??= [];
  else if (g.draw) {
    const list = held ?? [];
    held = null;
    for (const fn of list) fn();
  }
}
const after = sockets.flatMap((ws, i) => ws.sent.slice(i ? 0 : sent));
const frames = after.filter((m) => m.type === 'action');
const requests = after.filter((m) => m.type === 'watch' || m.type === 'peek' || m.type === 'rebase');
const hellos = sockets.flatMap((ws) => ws.frames('hello'));
const notices = app.toasts.children.map((el) => el.querySelector('p').textContent);
const kept = app.toasts.children.map((el) => el.querySelector('.kept')?.textContent ?? null);
process.stdout.write(JSON.stringify({ frames, requests, notices, kept, hellos, looks, edits, copies }));
