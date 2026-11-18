'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, find, key, notices, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ access: 'public', tabs: [
  { id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '0,0': 'Item', '1,0': 'Venue', '1,1': '1200', '2,1': '=B2*2' } },
  { id: 2, name: 'Notes', rows: 5, cols: 3, cells: { '0,0': 'Hello' } },
] });
const sentTypes = (t) => t.server.sockets.flatMap((s) => s.sent.map((m) => m.type));

test('a Public route shows the sheet read-only, without status, people or History', async () => {
  const t = await startApp({ server: sheet() });
  assert.equal(shownAt(t, 1, 0), 'Venue');
  assert.equal(t.app.badge.hidden, false);
  assert.equal(t.app.badge.textContent, 'Public · read-only');
  assert.equal(t.app.status.el.hidden, true, 'nothing is saved on a Public route');
  assert.equal(t.app.histBtn.hidden, true);
  assert.equal(t.app.facesEl.children.length, 0);
  assert.equal(t.app.tabs.addBtn.hidden, true);
  for (const b of t.app.toolbar.el.querySelectorAll('button, select')) assert.equal(b.disabled, true, `${b.getAttribute('aria-label') || b.textContent} is off`);
  assert.equal(t.grid.el.querySelector('.handle'), null, 'no fill handle');
  assert.equal(t.app.bar.readOnly, true);
});

test('every way to edit is refused in the page before anything is sent', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.equal(t.grid.input.value, '', 'the typed character is dropped');
  assert.match(notices(t)[0][1], /This sheet is Public here: it can be read, copied and downloaded, not edited\./);
  key(t, 'F2');
  key(t, 'Enter');
  key(t, 'Delete');
  key(t, 'z', { ctrlKey: true });
  key(t, 'b', { ctrlKey: true });
  key(t, 'd', { ctrlKey: true });
  key(t, 'F10', { shiftKey: true });
  t.grid.h.dbl({ zone: 'cell', r: 1, c: 0 }, {});
  t.grid.h.context({ zone: 'cell', r: 1, c: 0 }, { preventDefault() {} });
  t.grid.h.down({ zone: 'resize', r: 0, c: 0 }, { preventDefault() {}, clientX: 100 });
  t.grid.h.up({});
  const data = { 'text/plain': 'pasted' };
  fire(t.doc, 'paste', { clipboardData: { getData: (k) => data[k] } });
  fire(t.doc, 'cut', { clipboardData: { setData() {} } });
  t.app.toggleHistory();
  t.app.bar.focus();
  fire(t.app.bar, 'keydown', { key: 'Enter' });
  assert.equal(t.editor.open, false);
  assert.equal(t.app.history.open, false);
  assert.equal(t.doc.querySelector('.menu'), null, 'no editing menu opens');
  assert.deepEqual(sentTypes(t), ['hello', 'watch'], 'no action, presence or rebase leaves the page');
});

test('a Public viewer scrolls, selects, switches tabs, copies and downloads', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.input.focus();
  key(t, 'ArrowDown');
  key(t, 'ArrowRight', { shiftKey: true });
  assert.deepEqual(t.sel.box(), { r1: 1, c1: 0, r2: 1, c2: 1 });
  const data = {};
  fire(t.doc, 'copy', { clipboardData: { setData: (k, v) => { data[k] = v; } } });
  assert.equal(data['text/plain'], 'Venue\t1200');
  assert.equal(find(t.app.top, 'Download this tab as CSV').disabled, false);
  t.app.tabs.list.children[1].click();
  assert.equal(shownAt(t, 0, 0), 'Hello');
  // the formula text of a cell can be read; it comes through peek
  t.app.tabs.list.children[0].click();
  t.sel.set(2, 1);
  t.app.changed();
  t.timers.advance(60);
  assert.equal(t.app.bar.value, '=B2*2');
  assert.deepEqual(sentTypes(t), ['hello', 'watch', 'watch', 'watch', 'peek']);
});

test('live changes reach a Public viewer, and a dropped connection takes a new snapshot', async () => {
  const t = await startApp({ server: sheet() });
  t.server.setCell(1, 1, 1, '1500');
  assert.equal(shownAt(t, 1, 1), '1500');
  t.server.socket.drop();
  t.server.setCell(1, 1, 1, '1600');
  t.timers.advance(1000);
  const hello = t.server.socket.open();
  assert.equal(hello.session, undefined, 'a Public connection has no session');
  assert.equal(shownAt(t, 1, 1), '1600');
});

test('a Public sheet that is withdrawn leaves the screen', async () => {
  const t = await startApp({ server: sheet() });
  assert.equal(t.app.cover.hidden, true);
  // the host closes the socket and refuses every new one
  t.server.connect = () => { const ws = new (t.server.socket.constructor)(null); t.server.sockets.push(ws); return ws; };
  t.server.sockets[0].drop();
  assert.equal(t.app.cover.hidden, true, 'one lost connection is retried');
  for (const wait of [1000, 2000]) { t.timers.advance(wait); t.server.socket.drop(); }
  assert.equal(t.app.cover.hidden, false);
  assert.equal(t.app.cover.querySelector('[role="alert"] h2').textContent, 'This sheet is not available here');
  assert.match(t.app.cover.textContent, /is no longer available there\. The connection was closed and its content was removed from this screen/);
  assert.equal(t.model.tab, null, 'the content is gone from the page');
  assert.equal(t.grid.el.hidden, true);
  assert.equal(t.app.tabs.el.hidden, true);
  t.timers.advance(60000);
  assert.equal(t.server.sockets.length, 3, 'it does not keep reconnecting');
  find(t.app.cover, 'Try again').click();
  assert.equal(location.reloads, 1);
});

test('before the first welcome the page says it is connecting, then that it cannot reach the sheet', async () => {
  const t = await startApp({ server: sheet(), open: false });
  assert.equal(t.app.status.el.lastChild.textContent, 'Connecting…');
  assert.equal(t.app.banners.children.length, 0);
  t.server.socket.drop();
  assert.match(t.app.banners.textContent, /Could not reach the sheet\. Trying again…/);
  t.timers.advance(1000);
  t.server.socket.open();
  assert.equal(t.app.banners.children.length, 0);
  assert.equal(shownAt(t, 1, 0), 'Venue');
});

test('a browser that kept a name from a Private route loads the sheet read-only once the route is Public', async () => {
  // A person gives a name on a route without a verified identity, and
  // the browser keeps it for this origin.
  const server = new FakeServer({ me: null, tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '1,0': 'Venue' } }] });
  const first = await startApp({ server });
  const dialog = first.doc.querySelector('.dialog');
  dialog.querySelector('input').value = 'Dana';
  find(dialog, 'Continue').click();
  assert.equal(server.socket.open().name, 'Dana');
  assert.deepEqual(first.net.me, { label: 'Dana', verified: false });
  const storage = globalThis.localStorage;
  assert.equal(storage.getItem('flats-sheet-name'), 'Dana');
  // The operator makes the same address Public, and the page is loaded
  // again there with what the browser kept.
  server.access = 'public';
  const t = await startApp({ server, storage });
  assert.deepEqual(t.server.socket.sent[0], { type: 'hello', version: 1, pending: 0, name: 'Dana' }, 'the kept name goes with the first hello');
  assert.equal(t.net.stopped, null);
  assert.equal(t.net.access, 'public');
  assert.equal(shownAt(t, 1, 0), 'Venue');
  assert.equal(t.app.badge.textContent, 'Public · read-only');
  assert.equal(t.app.cover.hidden, true);
  assert.equal(t.doc.querySelector('.dialog'), null, 'no name is asked for');
  assert.equal(t.net.me, null, 'the reader has no name');
  assert.equal(t.net.session, null);
  assert.equal(t.app.facesEl.children.length, 0);
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.match(notices(t).at(-1)[1], /This sheet is Public here: it can be read, copied and downloaded, not edited\./);
  assert.deepEqual(t.server.socket.sent.map((m) => m.type), ['hello', 'watch'], 'no action, presence or rebase leaves the page');
  assert.equal(storage.getItem('flats-sheet-name'), 'Dana', 'the name is kept for a Private route');
});

test('the scripted server ends a Public hello that names a session, a watermark or pending actions, as the host does', () => {
  const server = sheet();
  const answer = (hello) => {
    const ws = server.connect();
    const got = [];
    ws.onmessage = (ev) => got.push(JSON.parse(ev.data));
    ws.send(JSON.stringify(hello));
    return [got.map((m) => m.code ?? m.type), ws.closed];
  };
  assert.deepEqual(answer({ type: 'hello', version: 1, pending: 0, name: 'Ann' }), [['welcome'], false], 'a name is not read');
  assert.deepEqual(answer({ type: 'hello', version: 1, pending: 0, name: 'Ann', session: '' }), [['welcome'], false], 'an empty session names none');
  assert.deepEqual(answer({ type: 'hello', version: 1, pending: 0, name: 'Ann', session: 'tok1' }), [['forbidden'], true]);
  assert.deepEqual(answer({ type: 'hello', version: 1, pending: 1, name: 'Ann' }), [['forbidden'], true]);
  assert.deepEqual(answer({ type: 'hello', version: 1, pending: 0, watermark: '10', tab: 1 }), [['forbidden'], true]);
});
