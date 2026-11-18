'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { SERVER_FRAME_MAX } from '../protocol.js';
import { FakeServer, cellOut, find, key, notices, settle, shownAt, startApp, type } from './server.mjs';

const NOW = Date.parse('2026-11-13T12:00:00Z');
const REVS = [
  { number: 3, kind: 'person', authors: ['Ann', 'Jun'], created: '2026-11-13T11:58:00Z', pinned: false, tabs: ['Budget'] },
  { number: 2, kind: 'agent', authors: ['Agent'], message: 'Offsite budget', created: '2026-11-13T09:00:00Z', pinned: true, tabs: [{ id: 1, name: 'Budget' }, { id: 9, name: 'Travel' }] },
  { number: 1, kind: 'created', created: '2026-11-12T09:00:00Z', pinned: false },
];
const REV2_TABS = [{ id: 1, name: 'Budget', rows: 5, cols: 3, frozen_rows: 0, frozen_cols: 0 }, { id: 9, name: 'Travel', rows: 4, cols: 2, frozen_rows: 1, frozen_cols: 0 }];
const frames = (tab, cells) => ({ frames: [
  { type: 'tab_head', generation: 0, tab, seq: '0', rows: [[700, 4]], cols: [[800, 2]], widths: {}, frozen_rows: tab === 9 ? 1 : 0, frozen_cols: 0, styles: [{}] },
  { type: 'rows', generation: 0, tab, part: 0, rows: cells },
  { type: 'tab_done', generation: 0, tab, parts: 1 },
] });

// lineBody is a response body that streams lines: each a text, or a
// function that gives the next text and null after the last. It hands
// out one line at a time, in chunks of at most 64 KiB, and makes the next
// chunk only when it is read.
function lineBody(lines) {
  const next = typeof lines === 'function' ? lines : (() => { let i = 0; return () => (i < lines.length ? lines[i++] : null); })();
  const encoder = new TextEncoder();
  let pending = null;
  let at = 0;
  return new ReadableStream({
    async pull(c) {
      // A chunk arrives in a task of its own, as from the network.
      await new Promise((r) => setImmediate(r));
      if (!pending || at >= pending.length) {
        const text = next();
        if (text == null) { c.close(); return; }
        pending = encoder.encode(`${text}\n`);
        at = 0;
      }
      c.enqueue(pending.subarray(at, at + 65536));
      at += 65536;
    },
  }, { highWaterMark: 0 });
}

// fakeFetch answers the History routes of section 17 and records the calls.
function fakeFetch(extra = {}) {
  const calls = [];
  const routes = {
    'GET /_sheet/api/revisions?limit=50': { revisions: REVS, next: 0 },
    'GET /_sheet/api/revisions/2': { date: '2026-11-13', tabs: REV2_TABS },
    'GET /_sheet/api/revisions/2/tabs/1': frames(1, [[700, [cellOut(800, { kind: 't', value: 'Old item' }), cellOut(801, { kind: 'nf', value: 42, shown: '$42' })]]]),
    'GET /_sheet/api/revisions/2/tabs/9': frames(9, [[700, [cellOut(800, { kind: 't', value: 'Route' })]]]),
    'POST /_sheet/api/pin': { revision: 2, pinned: false },
    'POST /_sheet/api/recover': { tab: 9 },
    'POST /_sheet/api/rollback': { revision: 4 },
    ...extra,
  };
  const fn = async (url, init = {}) => {
    const key = `${init.method} ${url}`;
    calls.push({ key, body: init.body ? JSON.parse(init.body) : undefined, headers: init.headers });
    let r = routes[key];
    if (!r) throw new Error(`unexpected request ${key}`);
    // A list of answers is given one request at a time.
    if (Array.isArray(r)) r = r.length > 1 ? r.shift() : r[0];
    const status = r.status ?? 200;
    // A tab of a revision is its frames, one to a line.
    if (r.frames && status < 300) return { status, ok: true, body: lineBody(r.frames.map((f) => JSON.stringify(f))) };
    if (r.lines) return { status, ok: true, body: lineBody(r.lines()) };
    return { status, ok: status < 300, json: async () => r.json ?? r };
  };
  fn.calls = calls;
  return fn;
}

async function open(extra) {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '0,0': 'Item' } }, { id: 2, name: 'Notes', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  const fetch = fakeFetch(extra);
  t.app.history.fetch = fetch;
  t.app.history.now = () => NOW;
  t.app.histBtn.click();
  await settle();
  return { ...t, fetch, panel: t.app.histEl, rev: (n) => t.app.histEl.querySelectorAll('.rev').find((b) => b.getAttribute('data-key') === `rev-${n}`) };
}

test('History lists revisions with kind, authors, time, message and changed tabs', async () => {
  const t = await open();
  assert.equal(t.app.histBtn.getAttribute('aria-pressed'), 'true');
  assert.equal(t.panel.hidden, false);
  assert.equal(t.panel.getAttribute('aria-label'), 'History');
  assert.deepEqual(t.panel.querySelectorAll('.rev').map((b) => b.children.map((c) => c.textContent)), [
    ['Revision 3', 'Edits · Ann, Jun · 2 min ago', 'Changed: Budget'],
    ['Revision 2', 'Agent write · Agent · 3 h ago', 'Offsite budget', 'Changed: Budget, Travel'],
    ['Revision 1', 'Created · 1 day ago'],
  ]);
  assert.ok(t.rev(2).querySelector('.pin'), 'a pinned revision is marked');
  assert.equal(t.doc.activeElement, find(t.panel, 'Close History'), 'the panel takes focus when it opens');
  assert.deepEqual(t.fetch.calls.map((c) => c.key), ['GET /_sheet/api/revisions?limit=50']);
});

test('an earlier revision opens read-only in the same grid, with its calculated values', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  assert.equal(shownAt(t, 0, 0), 'Old item');
  assert.equal(shownAt(t, 0, 1), '$42');
  assert.equal(t.rev(2).getAttribute('aria-current'), 'true');
  assert.match(t.app.banners.textContent, /Viewing revision 2 \(Agent write, Agent\)\. Read-only, with the values it calculates\./);
  assert.deepEqual(t.app.tabs.list.children.map((el) => [el.textContent, el.getAttribute('aria-selected')]), [['Budget', 'true'], ['Travel', 'false']], 'the tab bar lists the revision\'s tabs');
  assert.equal(t.app.tabs.addBtn.hidden, true);
  // nothing can be edited in a revision
  t.grid.input.focus();
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.match(notices(t)[0][1], /A revision is read-only/);
  key(t, 'Delete');
  assert.equal(t.server.socket.frames('action').length, 0);
  // the other tab of the revision
  t.app.tabs.list.children[1].click();
  await settle();
  assert.equal(shownAt(t, 0, 0), 'Route');
  assert.equal(t.grid.top.children.length, 2, 'with its frozen row');
  // live changes keep arriving underneath and show after going back
  t.server.setCell(1, 0, 0, 'Changed meanwhile');
  assert.equal(shownAt(t, 0, 0), 'Route');
  find(t.app.banners, 'Back to current').click();
  assert.equal(shownAt(t, 0, 0), 'Changed meanwhile');
  assert.equal(t.app.banners.children.length, 0);
  assert.equal(t.app.tabs.list.children[1].textContent, 'Notes');
});

test('an edit still open on the live tab when a revision opens is closed and its text offered back', async () => {
  for (const where of ['cell', 'bar']) {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '0,0': 'Item', '0,2': '=B5' } }, { id: 2, name: 'Notes', rows: 5, cols: 3 }] });
    const t = await startApp({ server });
    server.manual.add('peek');
    t.app.history.fetch = fakeFetch();
    t.app.history.now = () => NOW;
    // The formula's text is still on its way, so opening History commits nothing.
    t.sel.set(0, 2);
    t.app.changed();
    if (where === 'cell') key(t, 'F2');
    else { fire(t.app.bar, 'mousedown'); t.app.bar.focus(); }
    type(t, '+1');
    const peek = server.socket.frames('peek')[0];
    t.app.histBtn.click();
    await settle();
    assert.equal(t.editor.ed?.waiting, true);
    t.app.histEl.querySelectorAll('.rev').find((b) => b.getAttribute('data-key') === 'rev-2').click();
    await until(() => t.app.view || t.app.history.error, 'the revision opens');
    assert.equal(t.app.history.error, '');
    assert.equal(shownAt(t, 0, 0), 'Old item');
    assert.equal(t.editor.ed, null, `${where}: no live edit stays over the revision`);
    assert.ok(notices(t).some(([, text, kept]) => /A revision opened while you were typing/.test(text) && kept === '+1'), `${where}: the text is offered back`);
    // the answer that comes later opens nothing over the revision
    server.socket.recv({ type: 'peeked', id: peek.id, seq: server.seq.toString(), edit: '=B5', formula: true });
    assert.equal(t.editor.ed, null);
    assert.equal(t.grid.editing, false);
    assert.equal(server.socket.frames('action').length, 0);
  }
});

test('pin and unpin a revision from the panel', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  const pin = find(t.panel, 'Unpin revision 2');
  assert.equal(pin.getAttribute('aria-pressed'), 'true');
  pin.click();
  await settle();
  const call = t.fetch.calls.at(-1);
  assert.deepEqual([call.key, call.body], ['POST /_sheet/api/pin', { revision: 2, pinned: false }]);
  assert.equal(call.headers['X-Flats-Sheet'], '1');
  assert.ok(find(t.panel, 'Pin revision 2'));
  assert.equal(t.rev(2).querySelector('.pin'), null);
  assert.match(t.panel.textContent, /Unpinned revision 2\./);
});

test('rollback asks inline in the panel, and Escape or Cancel leaves the sheet as it is', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  find(t.panel, 'Roll back to this revision…').click();
  const ask = t.panel.querySelector('.confirm-acts');
  assert.equal(ask.getAttribute('role'), 'alertdialog');
  assert.match(ask.textContent, /Roll back the whole sheet to revision 2\? Every tab returns to how it was then, as a new revision\. Everyone’s undo history is cleared\./);
  assert.equal(t.doc.activeElement, find(t.panel, 'Cancel'), 'focus goes to the safe answer');
  find(t.panel, 'Cancel').click();
  assert.equal(t.panel.querySelector('.confirm-acts'), null);
  assert.equal(t.doc.activeElement, find(t.panel, 'Roll back to this revision…'), 'focus returns to the button that asked');
  assert.equal(t.fetch.calls.some((c) => c.key.includes('rollback')), false);
  find(t.panel, 'Roll back to this revision…').click();
  find(t.panel, 'Roll back').click();
  await settle();
  assert.deepEqual(t.fetch.calls.filter((c) => c.key === 'POST /_sheet/api/rollback').map((c) => c.body), [{ revision: 2, session: t.net.session }], 'the rollback is credited to this session');
  assert.match(t.panel.textContent, /Rolled the sheet back to revision 2\. Undo history was cleared for everyone\./);
  assert.equal(t.app.view, null, 'the grid shows the current sheet again');
  assert.equal(shownAt(t, 0, 0), 'Item');
});

test('a tab of a revision is restored into the current sheet, with the name it will get', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  // a tab named Budget exists now, so the restored one gets another name
  assert.ok(find(t.panel, 'Restore tab “Budget” as “Budget (restored)”'));
  t.app.tabs.list.children[1].click();
  await settle();
  find(t.panel, 'Restore tab “Travel”').click();
  await settle();
  const call = t.fetch.calls.at(-1);
  // the session makes the recovery this person's edit, with an undo entry
  assert.deepEqual([call.key, call.body], ['POST /_sheet/api/recover', { revision: 2, tab: 9, session: t.net.session }]);
  assert.match(t.panel.textContent, /Restored tab “Travel” at the end of the tab bar\./);
  assert.equal(t.app.view, null);
  // the tab itself arrives as an ordinary delta
  t.server.tabs.push(t.server.tabs[1]);
  t.server.push({ tabs: [...t.server.tabInfo().slice(0, 2), { id: 9, name: 'Travel', rows: 4, cols: 2, frozen_rows: 1, frozen_cols: 0 }] });
  assert.deepEqual(t.app.tabs.list.children.map((el) => el.textContent), ['Budget', 'Notes', 'Travel']);
});

test('a second and third restored name are numbered', async () => {
  const t = await open();
  t.server.push({ tabs: [...t.server.tabInfo(), { id: 5, name: 'budget (RESTORED)', rows: 1, cols: 1, frozen_rows: 0, frozen_cols: 0 }] });
  t.rev(2).click();
  await settle();
  assert.ok(find(t.panel, 'Restore tab “Budget” as “Budget (restored 2)”'), 'names are compared without regard to capitals');
});

test('a refused History request says why and leaves the panel usable', async () => {
  const t = await open({ 'POST /_sheet/api/rollback': { status: 422, json: { code: 'sheet_capacity', reason: 'disk_quota' } }, 'GET /_sheet/api/revisions/1': { status: 404, json: { code: 'not_found' } } });
  t.rev(1).click();
  await settle();
  assert.match(t.panel.querySelector('[role="alert"]').textContent, /That revision or tab is no longer retained/);
  assert.equal(t.app.view, null);
  t.rev(2).click();
  await settle();
  find(t.panel, 'Roll back to this revision…').click();
  find(t.panel, 'Roll back').click();
  await settle();
  assert.match(t.panel.querySelector('[role="alert"]').textContent, /over its disk quota/);
  assert.ok(find(t.panel, 'Roll back to this revision…'), 'the revision stays open');
});

test('a revision the host is still calculating is asked for again', async () => {
  const busy = { status: 503, json: { code: 'unavailable', reason: 'calculating', retry: true } };
  const t = await open({ 'GET /_sheet/api/revisions/2': [busy, busy, { date: '2026-11-13', tabs: REV2_TABS }] });
  t.rev(2).click();
  await settle();
  assert.equal(t.app.view, null, 'nothing is shown while the host calculates');
  assert.equal(t.panel.querySelector('[role="alert"]'), null);
  for (let i = 0; i < 2; i++) { t.timers.advance(300); await settle(); }
  assert.equal(t.fetch.calls.filter((c) => c.key === 'GET /_sheet/api/revisions/2').length, 3);
  assert.equal(shownAt(t, 0, 0), 'Old item');
  // any other 503 is an answer
  const down = await open({ 'GET /_sheet/api/revisions/2': { status: 503, json: { code: 'unavailable', reason: 'history_backend' } } });
  down.rev(2).click();
  await settle();
  assert.match(down.panel.querySelector('[role="alert"]').textContent, /History is not available right now/);
});

test('closing History returns to the current sheet and gives focus back to the grid', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  find(t.panel, 'Close History').click();
  assert.equal(t.panel.hidden, true);
  assert.equal(t.app.view, null);
  assert.equal(shownAt(t, 0, 0), 'Item');
  assert.equal(t.app.histBtn.getAttribute('aria-pressed'), 'false');
  assert.equal(t.doc.activeElement, t.grid.input);
});

// ---- the live sheet under an open revision ----

async function viewing(extra) {
  const t = await open(extra);
  t.rev(2).click();
  await settle();
  assert.equal(shownAt(t, 0, 0), 'Old item');
  const onRevision = (why) => {
    assert.equal(shownAt(t, 0, 0), 'Old item', why);
    assert.equal(shownAt(t, 0, 1), '$42', why);
    assert.equal(t.grid.model, t.app.view.model, why);
    assert.match(t.app.banners.textContent, /Viewing revision 2/, why);
    assert.deepEqual(t.app.tabs.list.children.map((el) => el.textContent), ['Budget', 'Travel'], why);
  };
  return { ...t, onRevision, back: () => find(t.app.banners, 'Back to current').click() };
}

test('a new snapshot of the live sheet does not replace an open revision', async () => {
  // a reconnect the server cannot replay
  let t = await viewing();
  t.server.setCell(1, 0, 0, 'New item');
  t.server.resume = 'snapshot';
  t.server.socket.drop();
  t.server.setCell(1, 0, 1, 'Written while away');
  t.timers.advance(1000);
  t.server.socket.open();
  assert.equal(t.model.cellAt(0, 1).shown, 'Written while away', 'the live model took the snapshot');
  t.onRevision('after the snapshot of a reconnect');
  t.server.setCell(1, 1, 0, 'Later');
  t.onRevision('after a delta on the new connection');
  t.back();
  assert.deepEqual([shownAt(t, 0, 0), shownAt(t, 0, 1), shownAt(t, 1, 0)], ['New item', 'Written while away', 'Later'], 'going back shows the latest live state');
  assert.equal(t.grid.model, t.model);
  assert.equal(t.app.banners.children.length, 0);

  // a resync of a connection that fell behind
  t = await viewing();
  t.server.setCell(1, 0, 0, 'Resynced');
  t.server.socket.recv({ type: 'resync', generation: 7, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  t.server.snapshot(t.server.socket, 7, 1);
  t.onRevision('after a resync');
  t.back();
  assert.equal(shownAt(t, 0, 0), 'Resynced');

  // a connection that starts over, as after a rollback by someone else
  t = await viewing();
  const before = t.server.socket;
  t.server.tab(1).cells.set(`${t.model.rowAt(0)}:${t.model.colAt(0)}`, { kind: 't', value: 'Restored' });
  before.recv({ type: 'reset', seq: String(++t.server.seq) });
  t.server.socket.open();
  assert.notEqual(t.server.socket, before);
  assert.deepEqual(t.server.socket.frames('watch').map((m) => m.tab), [1], 'the live tab is watched again, not a tab of the revision');
  t.onRevision('after the connection started over');
  t.back();
  assert.equal(shownAt(t, 0, 0), 'Restored');
});

test('the live tab deleted under an open revision is replaced by another live tab, and the revision stays', async () => {
  const t = await viewing();
  t.server.deleteTab(1);
  assert.equal(t.server.socket.frames('watch').at(-1).tab, 2, 'another live tab is watched');
  assert.equal(t.model.tab.id, 2);
  t.onRevision('after the live tab was deleted');
  assert.equal(t.fetch.calls.filter((c) => c.key.includes('/tabs/')).length, 1, 'no tab of the revision was loaded for it');
  t.back();
  assert.equal(t.grid.model, t.model);
  assert.deepEqual(t.app.tabs.list.children.map((el) => [el.textContent, el.getAttribute('aria-selected')]), [['Notes', 'true']]);
});

test('live structure, widths and agent notes under a revision leave its grid alone', async () => {
  const t = await viewing();
  const geometry = () => [t.grid.heights.n, t.grid.cols.n, t.grid.cols.size(0), t.grid.overlays.flash];
  const before = geometry();
  t.server.insertRows(1, 0, 3);
  t.server.push({ tab: 1, widths: [[t.model.colAt(0), 300]] });
  t.server.setCell(1, 0, 0, 'Agent wrote', { note: { agent: true, tab: 1, cells: 1 } });
  assert.deepEqual(geometry(), before, 'the grid keeps the rows, columns and widths of the revision');
  t.onRevision('after live changes');
  t.back();
  assert.equal(t.grid.heights.n, 23);
  assert.equal(t.grid.cols.size(0), 300);
});

test('the formula bar of a revision shows that revision\'s formula text, never the live cell\'s', async () => {
  // The revision's tab uses the ids of the live tab, as a real one does:
  // its B1 is a formula, and so is the live B1, with other text.
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '0,0': 'Item', '0,1': '=LIVE()' } }] });
  const t = await startApp({ server });
  const [row, col] = [t.model.rowAt(0), t.model.colAt(1)];
  const tabFrames = { frames: [
    { type: 'tab_head', generation: 0, tab: 1, seq: '0', rows: [[row, 4]], cols: [[t.model.colAt(0), 2]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] },
    { type: 'rows', generation: 0, tab: 1, part: 0, rows: [[row, [cellOut(t.model.colAt(0), { kind: 't', value: 'Old item' }), cellOut(col, { kind: 'nf', value: 42 })]]] },
    { type: 'tab_done', generation: 0, tab: 1, parts: 1 },
  ] };
  const cellRoute = `GET /_sheet/api/revisions/2/tabs/1/cell?row=${row}&col=${col}`;
  const fetch = fakeFetch({ 'GET /_sheet/api/revisions/2/tabs/1': tabFrames, [cellRoute]: { edit: '=A1*2', formula: true } });
  t.app.history.fetch = fetch;
  t.app.history.now = () => NOW;
  // the live formula is on the bar, and cached
  key(t, 'ArrowRight');
  t.timers.advance(60);
  assert.equal(t.app.bar.value, '=LIVE()');
  const livePeeks = server.socket.frames('peek').length;
  t.app.histBtn.click();
  await settle();
  t.app.histEl.querySelectorAll('.rev').find((b) => b.getAttribute('data-key') === 'rev-2').click();
  await settle();
  t.grid.input.focus();
  key(t, 'ArrowRight');
  assert.equal(shownAt(t, 0, 1), '42');
  assert.equal(t.app.bar.value, '', 'nothing is shown before the revision\'s text is here');
  t.timers.advance(60);
  await settle();
  assert.equal(t.app.bar.value, '=A1*2');
  assert.equal(t.app.bar.readOnly, true);
  assert.equal(server.socket.frames('peek').length, livePeeks, 'the live sheet is not asked');
  // the text is kept while the revision's tab is open
  key(t, 'ArrowLeft');
  assert.equal(t.app.bar.value, 'Old item');
  key(t, 'ArrowRight');
  t.timers.advance(60);
  await settle();
  assert.equal(t.app.bar.value, '=A1*2');
  assert.equal(fetch.calls.filter((c) => c.key === cellRoute).length, 1);
  // back on the live sheet the bar shows the live text again
  find(t.app.banners, 'Back to current').click();
  t.timers.advance(60);
  assert.equal(t.app.bar.value, '=LIVE()');
});

test('an answer for a tab of the revision that is no longer the chosen one is dropped', async () => {
  // The frames of tab 1 arrive after those of tab 9, which was chosen later.
  let release;
  const slow = new Promise((done) => { release = done; });
  const t = await open();
  const quick = t.app.history.fetch;
  t.app.history.fetch = async (url, init) => {
    const res = await quick(url, init);
    if (url.endsWith('/revisions/2/tabs/1') && t.fetch.calls.filter((c) => c.key.endsWith('/tabs/1')).length > 1) await slow;
    return res;
  };
  t.rev(2).click();
  await settle();
  t.app.tabs.list.children[1].click(); // Travel
  await settle();
  assert.equal(shownAt(t, 0, 0), 'Route');
  t.app.tabs.list.children[0].click(); // Budget: held back
  t.app.tabs.list.children[1].click(); // Travel again, answered at once
  await settle();
  release();
  await settle();
  assert.equal(shownAt(t, 0, 0), 'Route', 'the tab chosen last is the one shown');
  assert.equal(t.app.view.tab, 9);
});

test('text copied from a revision is pasted as text, not copied from the live cells of the same ids', async () => {
  // The revision's tab uses the ids of the live tab, as a real one does.
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 20, cols: 4, cells: { '0,0': 'Live item', '0,1': '=LIVE()' } }] });
  const t = await startApp({ server });
  const [row, colA, colB] = [t.model.rowAt(0), t.model.colAt(0), t.model.colAt(1)];
  const fetch = fakeFetch({ 'GET /_sheet/api/revisions/2/tabs/1': { frames: [
    { type: 'tab_head', generation: 0, tab: 1, seq: '0', rows: [[row, 4]], cols: [[colA, 2]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] },
    { type: 'rows', generation: 0, tab: 1, part: 0, rows: [[row, [cellOut(colA, { kind: 't', value: 'Old item' }), cellOut(colB, { kind: 'nf', value: 42 })]]] },
    { type: 'tab_done', generation: 0, tab: 1, parts: 1 },
  ] } });
  t.app.history.fetch = fetch;
  t.app.history.now = () => NOW;
  t.app.histBtn.click();
  await settle();
  t.app.histEl.querySelectorAll('.rev').find((b) => b.getAttribute('data-key') === 'rev-2').click();
  await settle();
  t.grid.input.focus();
  key(t, 'ArrowRight', { shiftKey: true });
  let copied = '';
  fire(t.doc, 'copy', { clipboardData: { setData: (_, text) => { copied = text; } } });
  assert.equal(copied, 'Old item\t42');
  find(t.app.banners, 'Back to current').click();
  t.grid.input.focus();
  key(t, 'ArrowDown');
  fire(t.doc, 'paste', { clipboardData: { getData: () => copied } });
  const op = server.socket.frames('action').at(-1).ops[0];
  assert.equal(op.op, 'write', 'the revision\'s shown text is written');
  assert.deepEqual(op.values, [['Old item', '42']]);
});

test('the restored tab is named as the host named it, also when its delta arrives before the answer', async () => {
  const t = await open();
  t.rev(2).click();
  await settle();
  t.app.tabs.list.children[1].click(); // Travel: no live tab has that name
  await settle();
  // The recovered tab reaches the live tab list before the request returns.
  const quick = t.app.history.fetch;
  t.app.history.fetch = async (url, init) => {
    const res = await quick(url, init);
    if (url.endsWith('/recover')) t.server.push({ tabs: [...t.server.tabInfo(), { id: 9, name: 'Travel', rows: 4, cols: 2, frozen_rows: 1, frozen_cols: 0 }] });
    return res;
  };
  find(t.panel, 'Restore tab “Travel”').click();
  await settle();
  assert.match(t.panel.textContent, /Restored tab “Travel” at the end of the tab bar\./);
});

// until waits for reached() while a response body streams in.
async function until(reached, what) {
  for (let i = 0; i < 200000 && !reached(); i++) await new Promise((r) => setImmediate(r));
  assert.ok(reached(), what);
}

// The text of a cell that the sheet's limits allow 1,600 of in one tab: an
// apostrophe, an equals sign and 30,000 U+0001. A frame carries it as its
// value and as its edit text, each control as six characters of JSON, so
// the tab's frames hold more characters than one JavaScript string can.
const CONTROLS = '\u0001'.repeat(30000);
const VALUE = `=${CONTROLS}`;
const EDIT = `'${VALUE}`;
const ESCAPED_ROWS = 1600;
const escapedTab = { id: 1, name: 'Budget', rows: ESCAPED_ROWS, cols: 1, frozen_rows: 0, frozen_cols: 0 };

// escapedLines gives the frames of that tab one line at a time, one row a
// frame, and counts their characters.
function escapedLines(count, { rows = ESCAPED_ROWS, end = true, extra = [] } = {}) {
  const at = (i) => {
    if (i === 0) return JSON.stringify({ type: 'tab_head', generation: 0, tab: 1, seq: '0', rows: [[700, ESCAPED_ROWS]], cols: [[800, 1]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}, { bold: true }] });
    if (i <= rows) return JSON.stringify({ type: 'rows', generation: 0, tab: 1, part: i - 1, rows: [[699 + i, [[800, 't', VALUE, null, EDIT, 1]]]] });
    if (end && i === rows + 1) return JSON.stringify({ type: 'tab_done', generation: 0, tab: 1, parts: rows });
    return extra[i - rows - (end ? 2 : 1)] ?? null;
  };
  let i = 0;
  return () => {
    const text = at(i++);
    if (text == null) return null;
    count.lines++;
    count.chars += text.length + 1;
    count.longest = Math.max(count.longest, text.length);
    return text;
  };
}

test('a revision tab whose frames hold more characters than one string can opens, read a frame at a time', async () => {
  const count = { lines: 0, chars: 0, longest: 0 };
  const t = await open({ 'GET /_sheet/api/revisions/2': { date: '2026-11-13', tabs: [escapedTab] }, 'GET /_sheet/api/revisions/2/tabs/1': { lines: () => escapedLines(count) } });
  t.rev(2).click();
  await until(() => t.app.view?.model && !t.app.view.model.tab.loading, 'the revision tab loaded');
  assert.ok(count.chars > 2 ** 29, `the response holds ${count.chars} characters`);
  assert.ok(count.longest <= SERVER_FRAME_MAX, `its longest line holds ${count.longest} characters`);
  const model = t.app.view.model;
  assert.equal(model.rowCount, ESCAPED_ROWS);
  for (const r of [0, 799, ESCAPED_ROWS - 1]) {
    const cell = model.cellAt(r, 0);
    assert.equal(cell.value, VALUE, `row ${r}`);
    assert.equal(cell.edit, EDIT, `row ${r}`);
    assert.equal(model.format(cell).bold, true, `row ${r}`);
  }
  assert.equal(shownAt(t, 0, 0), VALUE);
  assert.match(t.app.banners.textContent, /Viewing revision 2/);
});

test('a revision tab that ends early, holds a line longer than a frame, frames out of order or a field that is null is an invalid response', async () => {
  const long = `{"type":"rows","generation":0,"tab":1,"part":0,"rows":[[700,[[800,"t","${'x'.repeat(SERVER_FRAME_MAX)}"]]]]}`;
  for (const [what, lines, asked] of [
    ['no tab_done', (count) => escapedLines(count, { rows: 3, end: false })],
    ['a line longer than a frame', (count) => escapedLines(count, { rows: 0, end: false, extra: [long, JSON.stringify({ type: 'tab_done', generation: 0, tab: 1, parts: 1 })] }), 2],
    ['rows out of order', (count) => escapedLines(count, { rows: 0, end: false, extra: [JSON.stringify({ type: 'rows', generation: 0, tab: 1, part: 1, rows: [] })] })],
    ['a frame of another tab', (count) => escapedLines(count, { rows: 0, end: false, extra: [JSON.stringify({ type: 'tab_done', generation: 0, tab: 2, parts: 0 })] })],
    ['frames after the end', (count) => escapedLines(count, { rows: 0, extra: [JSON.stringify({ type: 'rows', generation: 0, tab: 1, part: 0, rows: [] })] })],
    ['a field that is null', () => {
      const head = { type: 'tab_head', generation: 0, tab: 1, seq: '0', rows: [[700, 1]], cols: [[800, 1]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}], more: null };
      const all = [JSON.stringify(head), JSON.stringify({ type: 'tab_done', generation: 0, tab: 1, parts: 0 })];
      return () => all.shift() ?? null;
    }],
    ['a cell whose column is null', () => {
      const head = { type: 'tab_head', generation: 0, tab: 1, seq: '0', rows: [[700, 1]], cols: [[800, 1]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] };
      const all = [head, { type: 'rows', generation: 0, tab: 1, part: 0, rows: [[700, [[null, 't', 'x']]]] }, { type: 'tab_done', generation: 0, tab: 1, parts: 1 }].map((f) => JSON.stringify(f));
      return () => all.shift() ?? null;
    }],
  ]) {
    const count = { lines: 0, chars: 0, longest: 0 };
    const t = await open({ 'GET /_sheet/api/revisions/2': { date: '2026-11-13', tabs: [escapedTab] }, 'GET /_sheet/api/revisions/2/tabs/1': { lines: () => lines(count) } });
    t.rev(2).click();
    await until(() => t.panel.querySelector('[role="alert"]'), what);
    assert.match(t.panel.querySelector('[role="alert"]').textContent, /The sheet returned an invalid response\./, what);
    assert.equal(t.app.view, null, what);
    if (asked != null) assert.equal(count.lines, asked, `${what}: the body is not read past the long line`);
  }
});

test('a revision whose tab list or date holds a null, or a cell answer whose text is null, is an invalid response', async () => {
  for (const [what, json] of [
    ['a tab list that is null', { date: '2026-11-13', tabs: null }],
    ['a tab whose name is null', { date: '2026-11-13', tabs: [{ ...REV2_TABS[0], name: null }] }],
    ['a date that is null', { date: null, tabs: REV2_TABS }],
  ]) {
    const t = await open({ 'GET /_sheet/api/revisions/2': json });
    t.rev(2).click();
    await until(() => t.panel.querySelector('[role="alert"]'), what);
    assert.match(t.panel.querySelector('[role="alert"]').textContent, /The sheet returned an invalid response\./, what);
    assert.equal(t.app.view, null, what);
  }
  for (const json of [{ edit: null, formula: true }, { edit: '=A1', formula: null }]) {
    const t = await open({ 'GET /_sheet/api/revisions/2/tabs/1/cell?row=700&col=801': json });
    t.rev(2).click();
    await settle();
    t.grid.input.focus();
    key(t, 'ArrowRight');
    t.timers.advance(60);
    await settle();
    assert.equal(t.app.bar.value, '', `${JSON.stringify(json)}: no text is shown`);
    assert.equal(t.app.view.model.peeked(700, 801), null, `${JSON.stringify(json)}: and none is kept`);
    assert.match(t.panel.querySelector('[role="alert"]')?.textContent ?? '', /The sheet returned an invalid response\./);
  }
});

test('a revision tab the person leaves while it arrives is read no further', async () => {
  const count = { lines: 0, chars: 0, longest: 0 };
  const t = await open({ 'GET /_sheet/api/revisions/2': { date: '2026-11-13', tabs: [escapedTab] }, 'GET /_sheet/api/revisions/2/tabs/1': { lines: () => escapedLines(count) } });
  t.rev(2).click();
  await until(() => count.lines > 5, 'the tab started to arrive');
  t.app.histBtn.click();
  await settle();
  const read = count.lines;
  for (let i = 0; i < 2000; i++) await new Promise((r) => setImmediate(r));
  assert.ok(count.lines <= read + 1, `${count.lines - read} more lines were read after the person left`);
  assert.ok(count.lines < ESCAPED_ROWS, `${count.lines} lines of ${ESCAPED_ROWS + 2} were read`);
  assert.equal(t.app.view, null);
});
