import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import {
  cleanGuestName, guestNameError, runeCount, viewerLabel, viewersOf, helloFields, byKind, who, badge, nameWithBadge,
  rejectMessage, guestBatchFits, dropPending, finishGuestEditing, overwriteNotice, activityNotice, stored, GUEST_NAME_MAX, GUEST_BATCH_OPS, GUEST_FATALS,
} from '../guest.js';
import { guestCredit, creditText } from '../history.js';
import { Net, queueBatch } from '../net.js';
import { BoardState } from '../state.js';
import { TextEditor } from '../textedit.js';
import { THEMES } from '../theme.js';
import { buildRecovery } from '../recovery.js';

test('guest names lose control and format characters and are 1 to 40 characters', () => {
  assert.equal(cleanGuestName('  Mina  '), 'Mina');
  assert.equal(cleanGuestName('Mi\u0000na\u0007'), 'Mina', 'C0 controls');
  assert.equal(cleanGuestName('Mi\u0085na\u009f'), 'Mina', 'C1 controls');
  assert.equal(cleanGuestName('‮Mina​⁦'), 'Mina', 'bidi and zero-width format characters');
  assert.equal(cleanGuestName('​ ​'), '');
  assert.equal(guestNameError(''), 'Type a name to join.');
  assert.equal(guestNameError(' ​\n'), 'Type a name to join.', 'a name that cleans to nothing is refused');
  assert.equal(guestNameError('a'.repeat(GUEST_NAME_MAX)), '');
  assert.match(guestNameError('a'.repeat(GUEST_NAME_MAX + 1)), /at most 40/);
  // Characters, not UTF-16 code units: 40 emoji fit.
  const emoji = '🙂'.repeat(GUEST_NAME_MAX);
  assert.equal(runeCount(emoji), 40);
  assert.equal(guestNameError(emoji), '');
  assert.equal(guestNameError(`  ${'b'.repeat(GUEST_NAME_MAX)}​  `), '', 'counted after cleaning');
});

test('viewer labels and the viewers frame', () => {
  assert.equal(viewerLabel({ label: 'Viewer 3', viewer: 3 }), 'Viewer 3');
  assert.equal(viewerLabel({ viewer: 7 }), 'Viewer 7');
  assert.equal(viewerLabel(null), 'Viewer');
  assert.deepEqual(viewersOf({ t: 'viewers', n: [3, 1, 2] }), { count: 3, labels: ['Viewer 1', 'Viewer 2', 'Viewer 3'] });
  assert.deepEqual(viewersOf({ t: 'viewers', n: [] }), { count: 0, labels: [] });
  assert.deepEqual(viewersOf({ t: 'viewers', count: 27 }), { count: 27, labels: [] }, 'above 20 only a count');
  const many = Array.from({ length: 21 }, (_, i) => i + 1);
  assert.deepEqual(viewersOf({ t: 'viewers', n: many }), { count: 21, labels: [] }, 'never more than 20 entries');
  assert.deepEqual(viewersOf({ t: 'viewers', n: [1, 'x', -2, 2.5] }), { count: 1, labels: ['Viewer 1'] }, 'malformed entries are ignored');
});

test('hello asks to join only when the visitor chose to, and carries the tab tokens', () => {
  assert.deepEqual(helloFields({ join: false, name: 'Mina', guestToken: 'aa', viewerToken: '' }), {});
  assert.deepEqual(helloFields({ join: true, name: 'Mina', guestToken: '', viewerToken: '' }), { guest: { name: 'Mina' } });
  assert.deepEqual(helloFields({ join: true, name: 'Mina', guestToken: 'aa', viewerToken: 'bb' }), { guest: { name: 'Mina', token: 'aa' }, viewer_token: 'bb' });
  assert.deepEqual(helloFields({ join: true, name: '', guestToken: 'aa', viewerToken: 'bb' }), { viewer_token: 'bb' }, 'no guest without a name');
});

test('names and tokens live in browser storage, and blocked storage is harmless', () => {
  const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m }; };
  globalThis.localStorage = mem();
  globalThis.sessionStorage = mem();
  stored.setName('Mina');
  stored.setGuestToken('aa');
  stored.setViewerToken('bb');
  assert.equal(localStorage.m.get('flats-board-guest-name'), 'Mina');
  assert.equal(sessionStorage.m.get('flats-board-guest-token'), 'aa');
  assert.equal(sessionStorage.m.get('flats-board-viewer-token'), 'bb');
  assert.equal(stored.name(), 'Mina');
  stored.setName('');
  assert.equal(stored.name(), '');
  const blocked = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); }, removeItem() { throw new Error('denied'); } };
  globalThis.localStorage = blocked;
  globalThis.sessionStorage = blocked;
  assert.equal(stored.name(), '');
  assert.doesNotThrow(() => stored.setGuestToken('cc'));
  delete globalThis.localStorage;
  delete globalThis.sessionStorage;
});

test('authors: a guest socket gets kinds, a member socket objects', () => {
  assert.equal(byKind('guest'), 'guest');
  assert.equal(byKind({ kind: 'agent', label: 'Helper' }), 'agent');
  assert.equal(byKind(null), '');
  assert.deepEqual(who('member'), { text: 'A member', guest: false });
  assert.deepEqual(who('guest'), { text: 'A guest', guest: false }, 'no name, so no badge after a generic word');
  assert.deepEqual(who({ kind: 'guest', label: 'Mina' }), { text: 'Mina', guest: true });
  assert.deepEqual(who({ kind: 'person', label: 'Ann' }), { text: 'Ann', guest: false });
  assert.deepEqual(who(undefined), { text: 'Someone', guest: false });
});

test('the guest badge is a separate element next to the name text', () => {
  installDOM();
  const b = badge('guest');
  assert.equal(b.tagName, 'SPAN');
  assert.equal(b.className, 'badge guest');
  assert.equal(b.textContent, 'guest');
  // A name that imitates the badge stays plain text before the real badge.
  const parts = nameWithBadge('Mina guest ✓ verified', true);
  assert.equal(parts[0], 'Mina guest ✓ verified');
  assert.equal(parts[2].className, 'badge guest');
  assert.deepEqual(nameWithBadge('Ann', false), ['Ann']);
});

test('revision credits list guests with the badge and "and N more guests"', () => {
  installDOM();
  assert.equal(guestCredit({ authors: ['ann'] }), null);
  const c = guestCredit({ guest_authors: ['Mina', 'Rui', 'Kai', 'Sol', 'Jo'], guests_more: 3 });
  const text = (n) => (n.tagName ? n.children.map(text).join('') || n.textContent : n.textContent);
  assert.equal(text(c), 'Mina, Rui, Kai, Sol, Jo guest and 3 more guests');
  assert.ok(c.children.some((k) => k.className === 'badge guest'));
  assert.equal(text(guestCredit({ guest_authors: ['Mina'] })), 'Mina guest');
  assert.equal(creditText({ guest_authors: ['Mina'], guests_more: 0 }), 'Mina (guest)');
  assert.equal(creditText({ guest_authors: ['Mina', 'Rui'], guests_more: 1 }), 'Mina, Rui (guests) and 1 more guest');
  assert.equal(creditText({}), '');
});

test('guest refusals have messages; other codes keep the generic path', () => {
  for (const code of ['guest_rate', 'guest_size', 'guest_room', 'guest_asset', 'guest_revive', 'permission']) {
    assert.ok(rejectMessage(code), code);
  }
  assert.match(rejectMessage('guest_rate'), /too fast/);
  assert.equal(rejectMessage('limit'), null);
  assert.ok(GUEST_FATALS.has('editing_ended') && GUEST_FATALS.has('full') && GUEST_FATALS.has('name_invalid'));
  assert.ok(!GUEST_FATALS.has('protocol'));
});

test('a guest batch is one ops frame of at most 64 KiB and 100 operations', () => {
  const op = (i) => ({ op: 'set', id: 'e_' + String(i).padStart(10, '0'), inc: 1, g: 'text', val: 'x' });
  assert.ok(guestBatchFits(1, Array.from({ length: GUEST_BATCH_OPS }, (_, i) => op(i))));
  assert.ok(!guestBatchFits(1, Array.from({ length: GUEST_BATCH_OPS + 1 }, (_, i) => op(i))));
  assert.ok(!guestBatchFits(1, [{ ...op(0), val: 'y'.repeat(64 * 1024) }]), 'over 64 KiB never becomes ops_part');
  assert.ok(guestBatchFits(1, [{ ...op(0), val: 'y'.repeat(60 * 1024) }]));
});

class FakeSocket {
  static last = null;
  constructor() { this.sent = []; FakeSocket.last = this; }
  send(s) { this.sent.push(JSON.parse(s)); }
  close() {}
  recv(m) { this.onmessage({ data: JSON.stringify(m) }); }
}

function harness(t) {
  globalThis.WebSocket = FakeSocket;
  globalThis.location = { protocol: 'http:', host: 'board.test' };
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  const state = new BoardState();
  const seen = { viewers: [], levels: [], fatal: null, welcomes: 0, snapshots: 0 };
  const net = new Net(state, {
    hello: () => ({ t: 'hello', v: 1, session: state.session || undefined, pending: { count: state.pending.length, first_n: state.pending[0]?.n }, editor: 'ed' }),
    welcome() { seen.welcomes++; }, status() {}, pres() {}, shared() {}, tooLarge() {}, snapshot() { seen.snapshots++; }, delta() {}, ack() {}, reject() {},
    viewers(m) { seen.viewers.push(m); },
    level(m) { seen.levels.push(m.access); },
    fatal(code, m) { seen.fatal = { code, m }; },
  });
  net.start();
  FakeSocket.last.onopen();
  return { state, net, seen };
}

test('a guest uses the private wire form and sends presence at most 10 times a second', (t) => {
  const { state, net } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', session: 'S1', access: 'guest', level: 'edit', me: { key: 'p:1', label: 'Mina', guest: true } });
  assert.equal(state.public, false, 'guests merge with versions like members');
  for (let i = 0; i < 20; i++) { net.presence({ c: [i, i] }); t.mock.timers.tick(10); }
  const pres = ws.sent.filter((m) => m.t === 'pres');
  assert.ok(pres.length <= 3, `${pres.length} presence frames in 200 ms`);
  assert.ok(pres.length >= 1);
});

test('a viewer gets the public form and sends no presence', (t) => {
  const { state, net } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', access: 'public', level: 'view', me: { label: 'Viewer 2', viewer: 2 } });
  assert.equal(state.public, true);
  net.presence({ c: [1, 1] });
  t.mock.timers.tick(200);
  assert.equal(ws.sent.filter((m) => m.t === 'pres').length, 0);
});

test('viewers and level frames reach the app; a guest fatal code keeps its message and can resume', (t) => {
  const { net, seen } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', access: 'public', level: 'view', me: { label: 'Viewer 1', viewer: 1 } });
  ws.recv({ t: 'viewers', n: [1, 2] });
  ws.recv({ t: 'level', access: 'edit' });
  assert.deepEqual(seen.viewers, [{ t: 'viewers', n: [1, 2] }]);
  assert.deepEqual(seen.levels, ['edit']);
  ws.recv({ t: 'fatal', code: 'name_invalid', detail: 'Use a shorter name.' });
  assert.equal(seen.fatal.code, 'name_invalid');
  assert.equal(seen.fatal.m.detail, 'Use a shorter name.');
  assert.equal(net.stopped, true);
  t.mock.timers.tick(60000);
  assert.equal(FakeSocket.last, ws, 'a fatal code does not reconnect by itself');
  net.resume();
  assert.notEqual(FakeSocket.last, ws, 'resume opens a new socket');
  assert.equal(net.stopped, false);
  FakeSocket.last.onopen();
  assert.equal(FakeSocket.last.sent[0].t, 'hello');
  FakeSocket.last.recv({ t: 'welcome', access: 'public', level: 'edit', me: { label: 'Viewer 1', viewer: 1 } });
  assert.equal(seen.welcomes, 2);
});

const EL = { id: 'e_aaaaaaaaaa', type: 'text', x: 0, y: 0, w: 10, h: 10, rot: 0, z: 'V', text: 'a', inc: 1, v: {} };
const guestWelcome = (session) => ({ t: 'welcome', session, access: 'guest', level: 'edit', me: { key: 'p:1', label: 'Mina', guest: true } });
const viewerWelcome = { t: 'welcome', access: 'public', level: 'view', me: { label: 'Viewer 1', viewer: 1 } };
const textOp = (text) => ({ op: 'set', id: EL.id, inc: 1, g: 'text', val: text });

test('a resumed connection starts with fresh socket state after a resync and an in-flight batch', (t) => {
  const { state, net, seen } = harness(t);
  let ws = FakeSocket.last;
  ws.recv(guestWelcome('S1'));
  ws.recv({ t: 'snapshot', gen: 0, seq: 1, part: 1, parts: 1, elements: [EL] });
  ws.recv({ t: 'resync', gen: 3, seq: 1 });
  ws.recv({ t: 'snapshot', gen: 3, seq: 1, part: 1, parts: 1, elements: [EL] });
  state.push([textOp('b')]);
  net.flush();
  assert.equal(net.inflight, 1);
  // The board is full on this attempt; trying again resends the batch.
  ws.recv({ t: 'fatal', code: 'full' });
  net.resume();
  ws = FakeSocket.last;
  ws.onopen();
  assert.equal(net.inflight, null);
  assert.equal(net.gen, 0, 'generations count per socket');
  ws.recv(guestWelcome('S1'));
  assert.deepEqual(ws.sent.filter((m) => m.t === 'ops').map((m) => m.n), [1], 'the batch is sent again on the new socket');
  const snaps = seen.snapshots;
  ws.recv({ t: 'snapshot', gen: 0, seq: 2, part: 1, parts: 1, elements: [{ ...EL, text: 'b' }] });
  assert.equal(seen.snapshots, snaps + 1, 'the new socket\'s generation 0 snapshot is applied');
  // Editing ends after another resync: the viewer snapshot still applies.
  ws.recv({ t: 'resync', gen: 2, seq: 2 });
  ws.recv({ t: 'fatal', code: 'editing_ended' });
  net.resume();
  ws = FakeSocket.last;
  ws.onopen();
  ws.recv(viewerWelcome);
  ws.recv({ t: 'snapshot', gen: 0, seq: 2, part: 1, parts: 1, elements: [{ id: EL.id, type: 'text', x: 0, y: 0, w: 10, h: 10, rot: 0, z: 'V', text: 'b' }] });
  assert.equal(seen.snapshots, snaps + 2);
  assert.equal(state.get(EL.id).text, 'b');
});

test('a guest that comes back as a viewer drops its orphaned batches and can join again cleanly', (t) => {
  const { state, net } = harness(t);
  let ws = FakeSocket.last;
  ws.recv(guestWelcome('S1'));
  ws.recv({ t: 'snapshot', gen: 0, seq: 1, part: 1, parts: 1, elements: [EL] });
  state.push([textOp('unsent')]);
  net.flush();
  // Disconnected; the owner stops guest editing meanwhile, so the reconnect
  // is admitted as a viewer without editing_ended.
  ws.onclose();
  t.mock.timers.tick(2000);
  ws = FakeSocket.last;
  ws.onopen();
  ws.recv(viewerWelcome);
  assert.equal(state.public, true);
  assert.equal(state.pending.length, 1, 'the net layer keeps them for the app to handle');
  const changed = dropPending(state);
  assert.ok(changed.has(EL.id));
  assert.equal(state.pending.length, 0);
  ws.recv({ t: 'snapshot', gen: 0, seq: 1, part: 1, parts: 1, elements: [{ id: EL.id, type: 'text', x: 0, y: 0, w: 10, h: 10, rot: 0, z: 'V', text: 'a' }] });
  assert.equal(state.get(EL.id).text, 'a', 'unconfirmed text is no longer overlaid on the public board');
  // Joining later starts a new session and resends nothing.
  net.restart();
  ws = FakeSocket.last;
  ws.onopen();
  assert.equal(ws.sent[0].session, undefined);
  assert.equal(ws.sent[0].pending.count, 0);
  ws.recv(guestWelcome('S2'));
  assert.equal(ws.sent.filter((m) => m.t === 'ops').length, 0);
});

test('authorship fields in element sets are accepted plain or versioned', () => {
  const state = new BoardState();
  state.loadSnapshot({ seq: 1, elements: [EL] });
  const ta = { text: { k: 'guest', n: 'Mina' } };
  assert.equal(state.applyDelta({ seq: 2, changes: [{ id: EL.id, inc: 1, set: { ta, lk: 'guest' } }] }).status, 'applied');
  assert.deepEqual(state.get(EL.id).ta, ta);
  assert.equal(state.get(EL.id).lk, 'guest');
  assert.equal(state.applyDelta({ seq: 3, changes: [{ id: EL.id, inc: 1, set: { lk: ['member', 4], text: ['c', 4] } }] }).status, 'applied');
  assert.equal(state.get(EL.id).lk, 'member');
  assert.equal(state.get(EL.id).text, 'c');
});

const textOf = (n) => (n.tagName ? n.children.map(textOf).join('') || n.textContent : n.textContent);

test('notices mark a guest author from a member socket with the guest badge', () => {
  installDOM();
  // The exact member-facing author of a guest edit.
  const by = { kind: 'person', label: 'Operator', guest: true };
  assert.deepEqual(who(by), { text: 'Operator', guest: true });
  const n = overwriteNotice(by, 'Plan');
  assert.equal(textOf(n), 'Operator guest just changed "Plan", so the text you wrote was overwritten.');
  assert.ok(n.children.some((c) => c.className === 'badge guest'), 'the badge is its own element');
  const a = activityNotice(by, 'changed 2 elements');
  assert.equal(textOf(a), 'Operator guest changed 2 elements.');
  assert.ok(a.children.some((c) => c.className === 'badge guest'));
  // A member named like a guest gets no badge; an agent keeps its label.
  const member = overwriteNotice({ kind: 'person', label: 'Mina guest' }, '');
  assert.ok(!member.children.some((c) => c.className === 'badge guest'));
  assert.equal(textOf(member), 'Mina guest just changed this element, so the text you wrote was overwritten.');
  assert.equal(textOf(activityNotice({ kind: 'agent', label: 'Helper' }, 'added 1 element')), 'Helper added 1 element.');
  assert.equal(textOf(activityNotice(null, 'added 1 element')), 'The agent added 1 element.');
});

// The transition App.welcome (a guest reconnected as a viewer) and
// App.editingEnded run, with the real TextEditor and board state: the text
// in an open editor reaches the recovery file before editing is disabled.
test('ending guest editing keeps the text of an open editor for recovery', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  installDOM([{ cls: 'board-ui' }, { cls: 'toasts' }]);
  const ID = 'e_shape00001';
  const state = new BoardState();
  state.session = 'S';
  state.loadSnapshot({ seq: 10, elements: [{ id: ID, type: 'shape', x: 0, y: 0, w: 200, h: 100, rot: 0, z: 'V', text: 'start', inc: 1, v: { geom: 1, text: 10 } }] });
  let recovery = null;
  const app = {
    state, cam: { z: 1 }, editor: null, canEdit: true, changedIds: [],
    renderer: { theme: THEMES.light, setHidden() {} },
    presence: { send() {} },
    styleBar: { hide() {}, update() {} },
    toScreen: (x, y) => [x, y],
    commit(list) { return this.canEdit && list.length ? queueBatch(state, list).batch : null; },
    changed(ids) { this.changedIds.push(...ids); },
    focusBoard() {},
    exporter: { prepareRecovery() { recovery = buildRecovery(state); } },
  };
  const ed = new TextEditor(app, state.get(ID));
  app.editor = ed;
  ed.ta.value = 'typed while connected';
  const unsent = finishGuestEditing(app);
  app.canEdit = false; // the tab is a viewer now
  assert.equal(unsent, 1, 'the open text became an unconfirmed batch');
  assert.equal(app.editor, null, 'the editor closed');
  assert.ok(recovery && recovery.elements.some((e) => e.text === 'typed while connected'), 'the text is in the recovery file');
  assert.equal(state.pending.length, 0, 'nothing is left to resend');
  assert.equal(state.get(ID).text, 'start', 'the board shows the server text again');
  ed.ta.dispatch('blur');
  assert.equal(state.pending.length, 0, 'a later blur changes nothing');
  // Without an editor or pending edits there is nothing to recover.
  recovery = null;
  assert.equal(finishGuestEditing(app), 0);
  assert.equal(recovery, null);
});

// A recording 2D context: enough for Painter.sticky.
function recorder() {
  const calls = [];
  const ctx = new Proxy({ calls, font: '', measureText: (s) => ({ width: String(s).length * 6 }) }, {
    get(target, k) {
      if (k in target) return target[k];
      return (...args) => { calls.push([k, ...args]); };
    },
    set(target, k, v) { target[k] = v; return true; },
  });
  return ctx;
}

test('a member sees a guest author line with a separately drawn guest badge', async () => {
  const { Painter } = await import('../render.js');
  const { authorOf } = await import('../schema.js');
  const names = new Map([['guest:0123456789abcdef', 'Pat']]);
  assert.deepEqual(authorOf({ by: 'guest:0123456789abcdef' }, names), { name: 'Pat', guest: true });
  // The line names the creator: a later text writer never lends its name.
  assert.deepEqual(authorOf({ by: 'guest:ffff', ta: { text: { k: 'guest', n: 'Kai' } } }, names), { name: 'Guest', guest: true }, 'a creator who left is just Guest');
  assert.deepEqual(authorOf({ by: 'guest:0123456789abcdef', ta: { text: { k: 'guest', n: 'Kai' } } }, names), { name: 'Pat', guest: true }, 'creator Pat, text rewritten by guest Kai');
  assert.deepEqual(authorOf({ by: 'guest:0123456789abcdef', ta: { text: { k: 'member' } }, lk: 'member' }, names), { name: 'Pat', guest: true }, 'a later member edit does not change the creator');
  assert.deepEqual(authorOf({ by: 'guest:ffff' }, names), { name: 'Guest', guest: true });
  assert.deepEqual(authorOf({ by: 'ts:pat@example.com' }, names), { name: 'pat', guest: false });
  const sticky = (by) => ({ id: 'e_s', type: 'sticky', x: 0, y: 0, w: 200, h: 160, rot: 0, z: 'V', text: '', by });
  const painter = new Painter({ theme: THEMES.light, names, showAuthors: true, get: () => null });
  const texts = (by) => {
    const ctx = recorder();
    painter.sticky(ctx, sticky(by), 1, 0);
    return ctx.calls.filter((c) => c[0] === 'fillText').map((c) => c[1]);
  };
  assert.deepEqual(texts('guest:0123456789abcdef'), ['Pat', 'guest'], 'the badge is its own drawn text after the name');
  // A member typing the same name gets no badge.
  names.set('browser:pat', 'Pat');
  assert.deepEqual(texts('browser:pat'), ['Pat']);
  // A guest named "Pat guest" still gets the real badge after the name.
  names.set('guest:0123456789abcdef', 'Pat guest');
  assert.deepEqual(texts('guest:0123456789abcdef'), ['Pat guest', 'guest']);
});

test('the element list tells members a guest created the element', async () => {
  installDOM();
  const { A11yTree } = await import('../a11y.js');
  const el = { id: 'e_s', type: 'sticky', x: 0, y: 0, w: 200, h: 160, rot: 0, z: 'V', text: 'Hi', by: 'guest:0123456789abcdef' };
  const app = { isMember: true, names: new Map([['guest:0123456789abcdef', 'Pat']]), doc: { get: () => null, index: { connectorsOf: () => [] } } };
  const label = (a) => A11yTree.prototype.label.call({ app: a }, el);
  assert.match(label(app), /Created by Pat \(guest\)$/);
  assert.match(label({ ...app, names: new Map() }), /Created by Guest \(guest\)$/, 'a creator who left is just Guest');
  // A later guest text writer (Kai) never lends its name to the creator line.
  const rewritten = { ...el, ta: { text: { k: 'guest', n: 'Kai' } }, lk: 'guest' };
  assert.match(A11yTree.prototype.label.call({ app: { ...app, names: new Map() } }, rewritten), /Created by Guest \(guest\)$/, 'a creator who left is not named after a later writer');
  assert.match(A11yTree.prototype.label.call({ app }, rewritten), /Created by Pat \(guest\)$/, 'creator Pat stays named after Kai rewrote the text');
  assert.doesNotMatch(A11yTree.prototype.label.call({ app: { ...app, names: new Map() } }, rewritten), /Kai/);
  assert.doesNotMatch(label({ ...app, isMember: false }), /guest/, 'guests and viewers get no author names');
  assert.doesNotMatch(A11yTree.prototype.label.call({ app }, { ...el, by: 'browser:pat' }), /Created by/);
});
