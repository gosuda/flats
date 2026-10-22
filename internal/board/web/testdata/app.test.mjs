// App-level tests: the real board app started from index.html (page.mjs),
// with the test playing the server over the fake socket.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp } from './page.mjs';

const GUEST_KEY = 'guest:0123456789abcdef';
const sticky = (id, text, by) => ({ id, type: 'sticky', x: 0, y: 0, w: 200, h: 160, rot: 0, z: 'V', inc: 1, text, ...(by ? { by } : {}) });
const snapshot = (elements = [], title = 'Plan') => ({ t: 'snapshot', gen: 0, seq: 1, part: 1, parts: 1, meta: { title }, elements });
const guestWelcome = (session, label = 'Ann') => ({ t: 'welcome', v: 1, session, access: 'guest', level: 'edit', me: { key: GUEST_KEY, label }, guest_token: 'gt' });
const viewerWelcome = { t: 'welcome', v: 1, access: 'public', level: 'edit', access_epoch: 4, me: { viewer: 3 }, viewer_token: 'vt' };
// liveGuest is the browser storage of a tab with a guest session.
const liveGuest = { local: { 'flats-board-guest-name': 'Ann' }, session: { 'flats-board-joined': '1', 'flats-board-guest-token': 'gt' } };

async function page(t, opts) {
  t.mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: 1e12 });
  return startApp(opts);
}

const banner = (doc) => doc.querySelector('.banners').children[0] || null;
const textOf = (n) => (n ? n.textContent + n.children.map(textOf).join('') : '');
const has = (doc, cls) => doc.body.classList.contains(cls);
// settle waits for an async download (the recovery file is built in the
// background) without the mocked timers.
async function settle(downloads, n = 1) {
  for (let i = 0; i < 50 && downloads.length < n; i++) await new Promise((r) => setImmediate(r));
}

const zipText = async (blob) => Buffer.from(await blob.arrayBuffer()).toString('latin1');

// guestWithEdit runs a guest connection up to an unconfirmed edit on the board.
function guestWithEdit(app, socket, session = 'S1', text = 'unsent') {
  const ws = socket();
  ws.open();
  ws.recv(guestWelcome(session));
  ws.recv(snapshot([sticky('e_0000000001', 'hi')]));
  app.commit([{ op: 'set', id: 'e_0000000001', inc: 1, g: 'text', val: text }]);
  assert.equal(app.state.pending.length, 1);
  return ws;
}

test('editing_ended shows the banner as a viewer, and joining again clears it', async (t) => {
  const { app, doc, socket, downloads } = await page(t, { local: { 'flats-board-guest-name': 'Ann' } });
  let ws = guestWithEdit(app, socket);
  ws.recv({ t: 'fatal', code: 'editing_ended' });
  assert.equal(app.bannerKind, 'ended');
  assert.match(textOf(banner(doc)), /The owner turned off guest editing\. 1 of your latest changes wasn't saved/);
  assert.equal(app.state.pending.length, 0, 'unsent edits went into the recovery file');
  assert.ok(app.exporter.prepared, 'the recovery file was prepared');
  // The tab reconnects as a viewer, without asking to join.
  ws = socket();
  assert.equal(ws.open().guest, undefined);
  ws.recv(viewerWelcome);
  assert.ok(app.isPublic && has(doc, 'public'));
  assert.equal(app.bannerKind, 'ended', 'the banner stays while the visitor watches');
  // Guest editing comes back and the visitor joins again.
  ws.recv({ t: 'level', access: 'edit' });
  app.joinAs('Ann');
  ws = socket();
  assert.deepEqual(ws.open().guest, { name: 'Ann', token: 'gt' });
  ws.recv(guestWelcome('S2'));
  assert.ok(app.isGuest);
  assert.equal(app.bannerKind, null, 'a successful guest welcome clears the banner');
  assert.equal(banner(doc), null);
  assert.ok(!has(doc, 'has-banner'));
  assert.equal(app.guestEnded, false);
  // The changes that were not saved stay downloadable after the banner.
  const kept = doc.querySelector('.toasts').children.find((c) => /weren't saved/.test(textOf(c)));
  assert.ok(kept && 'keep' in kept.dataset, 'a kept toast offers the file');
  kept.querySelector('button.btn').click();
  await settle(downloads);
  assert.equal(downloads.length, 1);
  assert.equal(downloads[0].type, 'application/zip');
});

test('a guest that reconnects as a viewer ends its editing once and can join again', async (t) => {
  const { app, doc, socket } = await page(t, { local: { 'flats-board-guest-name': 'Ann' } });
  let ws = guestWithEdit(app, socket);
  app.select(['e_0000000001']);
  // The connection drops; meanwhile the owner stops guest editing, so the
  // reconnect is admitted as a viewer without editing_ended.
  ws.drop();
  t.mock.timers.tick(2000);
  ws = socket();
  assert.deepEqual(ws.open().guest, { name: 'Ann', token: 'gt' }, 'it still asks to join');
  ws.recv(viewerWelcome);
  assert.ok(app.isPublic);
  assert.equal(app.bannerKind, 'ended');
  assert.match(textOf(banner(doc)), /1 of your latest changes wasn't saved/);
  assert.equal(app.state.pending.length, 0);
  assert.equal(app.sel.size, 0, 'nothing of the guest editing stays selected');
  assert.equal(app.join, false);
  assert.equal(app.guestEnded, true);
  assert.equal(app.stopped, null, 'not a fatal stop: the viewer keeps watching live');
  // A second viewer welcome (another reconnect) does not end it again.
  ws.drop();
  t.mock.timers.tick(5000);
  ws = socket();
  assert.equal(ws.open().guest, undefined, 'a viewer reconnect does not ask to join');
  ws.recv(viewerWelcome);
  assert.equal(app.bannerKind, 'ended');
  // Joining again sends no old session and no old edits.
  app.joinAs('Ann');
  ws = socket();
  const hello = ws.open();
  assert.equal(hello.session, undefined);
  assert.equal(hello.pending.count, 0);
  ws.recv(guestWelcome('S2'));
  assert.equal(app.bannerKind, null);
  assert.equal(ws.sent.filter((m) => m.t === 'ops').length, 0);
  assert.ok(doc.querySelector('.toasts').children.some((c) => /weren't saved/.test(textOf(c))));
});

test('each ended guest session keeps its own recovery file', async (t) => {
  const { app, doc, socket, downloads } = await page(t, { local: { 'flats-board-guest-name': 'Ann' } });
  let ws = guestWithEdit(app, socket, 'S1', 'first-unsent');
  ws.recv({ t: 'fatal', code: 'editing_ended' });
  ws = socket();
  ws.open();
  ws.recv(viewerWelcome);
  ws.recv({ t: 'level', access: 'edit' });
  app.joinAs('Ann');
  ws = guestWithEdit(app, socket, 'S2', 'second-unsent');
  const kept = doc.querySelector('.toasts').children.find((c) => /weren't saved/.test(textOf(c)));
  ws.recv({ t: 'fatal', code: 'editing_ended' });
  banner(doc).querySelector('button.ghost').click();
  await settle(downloads, 1);
  kept.querySelector('button.btn').click();
  await settle(downloads, 2);
  assert.equal(downloads.length, 2);
  const [second, first] = await Promise.all(downloads.map(zipText));
  assert.match(second, /second-unsent/);
  assert.doesNotMatch(second, /first-unsent/);
  assert.match(first, /first-unsent/);
  assert.doesNotMatch(first, /second-unsent/);
});

test('ending guest editing with nothing unsent leaves no file to offer on rejoining', async (t) => {
  const { app, doc, socket } = await page(t, { local: { 'flats-board-guest-name': 'Ann' } });
  let ws = socket();
  ws.open();
  ws.recv(guestWelcome('S1'));
  ws.recv(snapshot([sticky('e_0000000001', 'hi')]));
  ws.recv({ t: 'fatal', code: 'editing_ended' });
  assert.match(textOf(banner(doc)), /You can keep watching live\.$/);
  ws = socket();
  ws.open();
  ws.recv(viewerWelcome);
  ws.recv({ t: 'level', access: 'edit' });
  app.joinAs('Ann');
  ws = socket();
  ws.open();
  ws.recv(guestWelcome('S2'));
  assert.equal(app.bannerKind, null);
  assert.ok(!doc.querySelector('.toasts').children.some((c) => /weren't saved/.test(textOf(c))));
});

test('a visitor refused as full at the first hello sees a viewer header', async (t) => {
  const { app, doc, socket } = await page(t, liveGuest);
  let ws = socket();
  assert.deepEqual(ws.open().guest, { name: 'Ann', token: 'gt' }, "this tab's guest session joins at once");
  ws.recv({ t: 'fatal', code: 'full' });
  assert.ok(has(doc, 'public'), 'History and Send to agent are hidden (.priv-only)');
  assert.equal(doc.querySelector('#export-btn .lbl').textContent, 'Download PNG');
  const chip = doc.querySelector('#access-chip');
  assert.equal(chip.hidden, false);
  assert.equal(chip.textContent, 'View only');
  assert.ok(has(doc, 'readonly'));
  const dlg = doc.querySelector('dialog');
  assert.match(textOf(dlg), /The most people this board allows are editing it right now/, 'the title placeholder is not used as the name');
  // Watch reconnects as a viewer without asking to join, and is a View
  // only choice: the name popup does not open.
  dlg.querySelector('button.ghost').click();
  ws = socket();
  assert.equal(ws.open().guest, undefined);
  ws.recv(viewerWelcome);
  ws.recv(snapshot([], 'Plan'));
  assert.ok(has(doc, 'public'));
  assert.equal(app.boardName(), 'Plan');
  assert.equal(doc.querySelector('dialog'), null);
  assert.equal(sessionStorage.getItem('flats-board-view-only'), '4');
  assert.equal(sessionStorage.getItem('flats-board-joined'), null);
});

test('a member refused for another reason keeps the member header', async (t) => {
  const { doc, socket } = await page(t);
  const ws = socket();
  ws.open();
  ws.recv({ t: 'welcome', v: 1, session: 'S', access: 'private', me: { key: 'ts:pat@example.com', name: 'pat', verified: true } });
  ws.recv({ t: 'fatal', code: 'deleted' });
  assert.ok(!has(doc, 'public'));
  assert.equal(doc.querySelector('#export-btn .lbl').textContent, 'Export');
});

const popup = (doc) => [...doc.querySelectorAll('dialog')].find((d) => d.open) || null;
const button = (root, text) => root.querySelectorAll('button').find((b) => b.textContent === text) || null;
const meText = (doc) => textOf(doc.querySelector('#me-label'));

test('arriving at an editable board opens the popup; a remembered name only fills it', async (t) => {
  const { app, doc, socket } = await page(t, { local: { 'flats-board-guest-name': 'Ann' } });
  let ws = socket();
  assert.equal(ws.open().guest, undefined, 'a remembered name does not join by itself');
  ws.recv(viewerWelcome);
  ws.recv(snapshot([], 'Plan'));
  const dlg = popup(doc);
  assert.ok(dlg, 'the popup opened');
  assert.match(textOf(dlg), /Anyone with this link can edit this board\. Pick the name others will see next to your cursor, or just watch\./);
  assert.equal(dlg.querySelector('#guest-name').value, 'Ann');
  assert.equal(textOf(dlg.querySelector('#guest-name-preview')), 'Shown as Ann guest to other editors. Nobody checks it.');
  assert.ok(button(dlg, 'View only') && button(dlg, 'Join and edit'));
  assert.equal(doc.querySelector('.join'), null, 'no bottom join panel');
  dlg.querySelector('form').dispatch('submit');
  assert.equal(popup(doc), null);
  ws = socket();
  assert.deepEqual(ws.open().guest, { name: 'Ann' });
  assert.equal(sessionStorage.getItem('flats-board-joined'), '1');
  ws.recv(guestWelcome('S1'));
  assert.ok(app.isGuest);
  assert.match(meText(doc), /You are Ann guest Change name/);
  // Change name has no View only.
  button(doc.querySelector('#me-label'), 'Change name').click();
  assert.ok(popup(doc) && !button(popup(doc), 'View only'));
  popup(doc).close();
  assert.ok(app.isGuest, 'closing Change name keeps the guest');
  assert.equal(sessionStorage.getItem('flats-board-view-only'), null);
});

test('View only keeps the tab a viewer until the level becomes edit again', async (t) => {
  const { app, doc, socket } = await page(t);
  let ws = socket();
  ws.open();
  ws.recv(viewerWelcome);
  button(popup(doc), 'View only').click();
  assert.equal(popup(doc), null);
  assert.equal(sessionStorage.getItem('flats-board-view-only'), '4');
  assert.match(meText(doc), /You are Viewer 3 Join to edit/);
  // A reconnect in the same edit period does not ask again.
  ws.drop();
  t.mock.timers.tick(2000);
  ws = socket();
  assert.equal(ws.open().guest, undefined);
  ws.recv(viewerWelcome);
  assert.equal(popup(doc), null);
  // View only again, then edit again: a newer epoch opens the popup.
  ws.recv({ t: 'level', access: 'view', access_epoch: 5 });
  assert.doesNotMatch(meText(doc), /Join to edit/);
  ws.recv({ t: 'level', access: 'edit', access_epoch: 6 });
  assert.ok(popup(doc), 'the popup opens for the new edit period');
  // Closing it (Esc) is View only too.
  popup(doc).close();
  assert.equal(sessionStorage.getItem('flats-board-view-only'), '6');
  // Join to edit reopens it.
  button(doc.querySelector('#me-label'), 'Join to edit').click();
  const dlg = popup(doc);
  assert.ok(dlg);
  dlg.querySelector('#guest-name').value = 'Bo';
  dlg.querySelector('form').dispatch('submit');
  assert.deepEqual(socket().open().guest, { name: 'Bo' });
  assert.ok(!app.isGuest, 'a guest only after the welcome');
});

test('a reconnect that missed a view, edit, view, edit cycle opens the popup', async (t) => {
  const { doc, socket } = await page(t, { session: { 'flats-board-view-only': '4' } });
  const ws = socket();
  ws.open();
  ws.recv(viewerWelcome);
  assert.equal(popup(doc), null, 'the choice holds in its edit period');
  ws.drop();
  t.mock.timers.tick(2000);
  socket().open();
  socket().recv({ ...viewerWelcome, access_epoch: 7 });
  assert.ok(popup(doc));
});

test('the level dropping to view closes the popup without a View only choice', async (t) => {
  const { doc, socket } = await page(t);
  const ws = socket();
  ws.open();
  ws.recv(viewerWelcome);
  assert.ok(popup(doc));
  ws.recv({ t: 'level', access: 'view', access_epoch: 5 });
  assert.equal(popup(doc), null);
  assert.equal(sessionStorage.getItem('flats-board-view-only'), null);
});

test("a live guest session rejoins after a reload without the popup", async (t) => {
  const { app, doc, socket } = await page(t, liveGuest);
  const ws = socket();
  assert.deepEqual(ws.open().guest, { name: 'Ann', token: 'gt' });
  ws.recv(guestWelcome('S1'));
  assert.ok(app.isGuest);
  assert.equal(popup(doc), null);
});

test('a guest name the server refuses: closing the popup watches and records View only at the next welcome', async (t) => {
  const { app, doc, socket } = await page(t, liveGuest);
  let ws = socket();
  ws.open();
  ws.recv({ t: 'fatal', code: 'name_invalid', detail: 'bad name' });
  assert.ok(popup(doc));
  button(popup(doc), 'View only').click();
  ws = socket();
  assert.equal(ws.open().guest, undefined);
  ws.recv({ ...viewerWelcome, access_epoch: 9 });
  assert.equal(popup(doc), null);
  assert.equal(sessionStorage.getItem('flats-board-view-only'), '9');
  assert.ok(app.isPublic);
});

test('every page sends a nickname: the typed name, else a kept generated one', async (t) => {
  let { socket } = await page(t, { local: { 'flats-board-name': 'Kim' } });
  assert.equal(socket().open().nick, 'Kim');
  t.mock.timers.reset();
  ({ socket } = await page(t));
  const nick = socket().open().nick;
  assert.match(nick, /^[A-Z][a-z]+ [A-Z][a-z]+$/);
  assert.equal(localStorage.getItem('flats-board-nick'), nick);
  t.mock.timers.reset();
  ({ socket } = await page(t, { local: { 'flats-board-nick': nick } }));
  assert.equal(socket().open().nick, nick, 'kept across visits');
});

test("a departed guest's name leaves the creator line of a member's tab", async (t) => {
  const { app, socket } = await page(t);
  const ws = socket();
  ws.open();
  ws.recv({ t: 'welcome', v: 1, session: 'S', access: 'private', me: { key: 'ts:pat@example.com', name: 'pat', verified: true } });
  const frame = { id: 'e_0000000003', type: 'frame', x: -500, y: -500, w: 400, h: 400, rot: 0, z: 'U', inc: 1, text: 'Work', by: GUEST_KEY };
  ws.recv(snapshot([sticky('e_0000000001', 'hi', GUEST_KEY), sticky('e_0000000002', 'yo', 'browser:kim'), frame,
    { ...sticky('e_0000000004', 'in', 'browser:kim'), x: -450, y: -450, frame: frame.id }]));
  const frameLabel = () => app.a11y.item(frame.id).getAttribute('aria-label');
  const { authorOf } = await import('../schema.js');
  const creator = (id) => {
    const name = authorOf(app.state.get(id), app.names).name;
    // The element list says the same as the canvas.
    if (id === 'e_0000000001') assert.match(app.a11y.item(id).getAttribute('aria-label'), new RegExp(`Created by ${name} \\(guest\\)$`));
    return name;
  };
  app.a11y.build();
  assert.equal(frameLabel(), 'Frame Work, 1 element');
  assert.equal(creator('e_0000000001'), 'Guest', 'nobody has said who the guest is yet');
  ws.recv({ t: 'pres', key: GUEST_KEY, name: 'Ann', guest: true, color: 1 });
  ws.recv({ t: 'pres', key: 'browser:kim', name: 'Kim', color: 2 });
  assert.equal(creator('e_0000000001'), 'Ann');
  app.renderer.full = false;
  ws.recv({ t: 'pres', key: GUEST_KEY, gone: true });
  assert.equal(creator('e_0000000001'), 'Guest', 'as in a tab opened after the guest left');
  assert.ok(app.renderer.full, 'the static layer with the creator lines is redrawn');
  assert.equal(frameLabel(), 'Frame Work, 1 element', 'a guest-created frame keeps its frame label');
  ws.recv({ t: 'pres', key: 'browser:kim', gone: true });
  assert.equal(creator('e_0000000002'), 'Kim', 'member names are kept');
  // The guest comes back.
  ws.recv({ t: 'pres', key: GUEST_KEY, name: 'Ann', guest: true, color: 1 });
  assert.equal(creator('e_0000000001'), 'Ann');
  // A guest that goes silent (its gone frame was missed, for example while
  // this tab reconnected) loses its name with its presence entry.
  ws.drop();
  t.mock.timers.tick(2000);
  socket().open();
  socket().recv({ t: 'welcome', v: 1, session: 'S', access: 'private', me: { key: 'ts:pat@example.com', name: 'pat', verified: true } });
  assert.equal(creator('e_0000000001'), 'Ann', 'still present as far as this tab knows');
  t.mock.timers.tick(40000);
  assert.ok(!app.presence.peers.has(GUEST_KEY));
  assert.equal(creator('e_0000000001'), 'Guest');
  app.a11y.build();
  assert.equal(frameLabel(), 'Frame Work, 1 element');
  assert.equal(creator('e_0000000001'), 'Guest', 'a rebuilt list agrees');
});
