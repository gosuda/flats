// Anonymous public editing (board-public-edit.md, TECH 8.1): guest names,
// viewer labels, the guest badge and the messages for guest refusals and
// fatal codes. Guest names are only ever drawn as text.

import { h } from './ui.js';
import { utf8Length } from './net.js';

export const GUEST_NAME_MAX = 40;
export const GUEST_BATCH_OPS = 100;
export const GUEST_FRAME_MAX = 64 * 1024;
export const VIEWERS_LISTED = 20;

const NAME_KEY = 'flats-board-guest-name';
const TOKEN_KEY = 'flats-board-guest-token';
const VIEWER_KEY = 'flats-board-viewer-token';
const NICK_KEY = 'flats-board-nick';
// VIEW_ONLY_KEY holds the access_epoch at which this tab chose View only;
// JOINED_KEY marks that this tab chose to join as a guest.
const VIEW_ONLY_KEY = 'flats-board-view-only';
const JOINED_KEY = 'flats-board-joined';

// Control (C0, DEL, C1) and Unicode format characters are removed from a
// name, as the server does (TECH 3.1, cleanGuestName).
const UNWANTED = /[\u0000-\u001f\u007f-\u009f\p{Cf}]/gu;

export const runeCount = (s) => [...s].length;

export function cleanGuestName(s) {
  return String(s || '').replace(UNWANTED, '').trim();
}

// guestNameError is '' for a name the server accepts after cleaning, or the
// reason it would refuse it.
export function guestNameError(s) {
  const n = runeCount(cleanGuestName(s));
  if (!n) return 'Type a name to join.';
  if (n > GUEST_NAME_MAX) return `Use at most ${GUEST_NAME_MAX} characters.`;
  return '';
}

// Browser storage can be unavailable (private windows, blocked storage);
// then the name lasts for this page only and tokens are issued again.
function read(store, key) {
  try { return globalThis[store].getItem(key) || ''; } catch { return ''; }
}
function write(store, key, value) {
  try {
    if (value) globalThis[store].setItem(key, value);
    else globalThis[store].removeItem(key);
  } catch { /* storage blocked */ }
}

export const stored = {
  name: () => read('localStorage', NAME_KEY),
  setName: (v) => write('localStorage', NAME_KEY, v),
  guestToken: () => read('sessionStorage', TOKEN_KEY),
  setGuestToken: (v) => write('sessionStorage', TOKEN_KEY, v),
  viewerToken: () => read('sessionStorage', VIEWER_KEY),
  setViewerToken: (v) => write('sessionStorage', VIEWER_KEY, v),
  nick: () => read('localStorage', NICK_KEY),
  setNick: (v) => write('localStorage', NICK_KEY, v),
  viewOnly: () => read('sessionStorage', VIEW_ONLY_KEY),
  setViewOnly: (epoch) => write('sessionStorage', VIEW_ONLY_KEY, epoch == null ? '' : String(epoch)),
  joined: () => read('sessionStorage', JOINED_KEY) === '1',
  setJoined: (v) => write('sessionStorage', JOINED_KEY, v ? '1' : ''),
};

// The generated nickname of a page whose person typed no name: the same
// words as the document app's collaborator names.
const ADJECTIVES = ['Amber', 'Brave', 'Calm', 'Clever', 'Gentle', 'Happy', 'Kind', 'Lucky', 'Merry', 'Quiet', 'Swift', 'Witty'];
const ANIMALS = ['Badger', 'Crane', 'Dolphin', 'Falcon', 'Fox', 'Heron', 'Koala', 'Lynx', 'Otter', 'Owl', 'Panda', 'Robin'];

export function randomNick(pick = (list) => list[Math.floor(Math.random() * list.length)]) {
  return `${pick(ADJECTIVES)} ${pick(ANIMALS)}`;
}

// pageNick is the nickname guests see for this page's person (hello.nick,
// docs-public-edit-TECH 5.2): the typed name when there is one, cut to a
// guest name's length, else the generated name this browser keeps, made
// once.
export function pageNick(typed, nick = randomNick) {
  const clean = [...cleanGuestName(typed)].slice(0, GUEST_NAME_MAX).join('').trim();
  if (clean) return clean;
  const saved = stored.nick();
  if (saved && !guestNameError(saved)) return cleanGuestName(saved);
  const made = nick();
  stored.setNick(made);
  return made;
}

// accessEpochOf reads access_epoch from a welcome or level frame.
export function accessEpochOf(m) {
  return m && Number.isSafeInteger(m.access_epoch) && m.access_epoch >= 0 ? m.access_epoch : 0;
}

// viewOnlyHolds reports whether this tab's View only choice, saved at an
// access_epoch, still holds at epoch: until the level becomes edit again,
// which raises the epoch (spec rule 11).
export function viewOnlyHolds(saved, epoch) {
  return /^\d+$/.test(saved || '') && Number(saved) >= epoch;
}

// offersJoin decides whether a viewer gets the name popup: at level edit,
// unless editing stopped or the tab chose View only in this edit period.
export function offersJoin({ level, epoch, viewOnly, stopped }) {
  return level === 'edit' && !stopped && !viewOnlyHolds(viewOnly, epoch);
}

// startsJoined: a page asks to join at once only for a live guest session
// of this tab, which chose to join and holds the token it was issued. A
// remembered name alone only fills the popup.
export function startsJoined({ joined, guestToken, name }) {
  return !!(joined && guestToken && name);
}

// helloFields adds the guest and viewer parts of a hello (wire contract):
// guest only when the visitor chose to join, the tokens whenever the tab
// holds them, and the page's nickname always.
export function helloFields({ join, name, guestToken, viewerToken, nick }) {
  const out = {};
  if (nick) out.nick = nick;
  if (join && name) {
    out.guest = { name };
    if (guestToken) out.guest.token = guestToken;
  }
  if (viewerToken) out.viewer_token = viewerToken;
  return out;
}

export function viewerLabel(me) {
  if (!me) return 'Viewer';
  return me.label || (me.viewer ? `Viewer ${me.viewer}` : 'Viewer');
}

// viewersOf reads a viewers frame: numbered entries up to 20, a count above.
export function viewersOf(m) {
  if (Array.isArray(m.n)) {
    const nums = m.n.filter((n) => Number.isInteger(n) && n > 0).sort((a, b) => a - b);
    return { count: nums.length, labels: nums.length <= VIEWERS_LISTED ? nums.map((n) => `Viewer ${n}`) : [] };
  }
  const count = Number.isInteger(m.count) && m.count > 0 ? m.count : 0;
  return { count, labels: [] };
}

// byKind is the kind of an author: a guest socket receives kinds only
// ("member", "guest", "agent", "system"), a member socket an object.
export function byKind(by) {
  if (!by) return '';
  return typeof by === 'string' ? by : by.kind || '';
}

const KIND_WHO = { member: 'A member', guest: 'A guest', agent: 'The agent', system: 'Flats' };

// who is the text and guest flag naming the author of a change in a notice.
// A member socket gets a guest author as {kind: "person", label, guest: true}.
export function who(by) {
  const label = by && typeof by === 'object' ? by.label : '';
  const kind = byKind(by);
  const guest = !!label && (kind === 'guest' || by.guest === true);
  return { text: label || KIND_WHO[kind] || 'Someone', guest };
}

// overwriteNotice tells the writer that someone replaced the text they typed.
export function overwriteNotice(by, what) {
  const w = who(by);
  return h('span', {}, nameWithBadge(w.text, w.guest), ` just changed ${what ? `"${what}"` : 'this element'}, so the text you wrote was overwritten.`);
}

// activityNotice reports what an author did to the board.
export function activityNotice(by, said) {
  const w = who(by);
  return h('span', {}, nameWithBadge(by && by.label ? w.text : 'The agent', w.guest), ` ${said}.`);
}

// finishGuestEditing ends the editing of a guest tab while it can still
// commit: the open text editor's text becomes a pending batch, every pending
// batch goes into the recovery file, then they are dropped from the board.
// It returns how many batches were unconfirmed.
export function finishGuestEditing(app) {
  if (app.editor) app.editor.commit();
  const unsent = app.state.pending.length;
  if (unsent) app.exporter.prepareRecovery();
  app.changed(dropPending(app.state));
  return unsent;
}

// badge is the mark drawn next to a name, outside the name text, so no name
// can imitate it.
export function badge(kind) {
  return h('span', { class: 'badge ' + kind, text: kind });
}

// nameWithBadge is a name followed by the guest badge when it is a guest's.
export function nameWithBadge(text, guest) {
  return guest ? [text, ' ', badge('guest')] : [text];
}

const REJECTS = {
  guest_rate: "You're sending changes too fast. Slow down and try again.",
  guest_size: 'This change is too large for a guest. Try a smaller selection.',
  guest_room: 'The board has no room left for guest changes. Ask the owner to make room.',
  guest_asset: 'Guests can only copy images that are on the board now.',
  guest_revive: 'You can only bring back elements you deleted yourself.',
  permission: 'This connection can no longer edit the board.',
};

// rejectMessage is the toast for a guest refusal, or null for other codes.
export function rejectMessage(code) {
  return REJECTS[code] || null;
}

export const GUEST_FATALS = new Set(['editing_ended', 'full', 'name_invalid']);

export const NAME_INVALID = `This name can't be used. Use 1 to ${GUEST_NAME_MAX} characters, without control or invisible characters.`;

// dropPending removes every unconfirmed batch from the live state and
// returns the ids whose display changed. Callers build the recovery file
// first.
export function dropPending(state) {
  const changed = new Set();
  for (const b of [...state.pending]) for (const id of state.reject(b.n, false).changed) changed.add(id);
  return changed;
}

// guestBatchFits reports whether a guest may send ops as batch n: guests
// send one ops frame of at most 64 KiB and 100 operations (TECH 4.1).
export function guestBatchFits(n, ops) {
  return ops.length <= GUEST_BATCH_OPS && utf8Length(JSON.stringify({ t: 'ops', n, ops })) <= GUEST_FRAME_MAX;
}
