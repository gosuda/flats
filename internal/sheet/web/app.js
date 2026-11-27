// The sheet's browser client (TECH 19). App builds the shell, owns the
// connection and the model, and wires the grid, the editor and the panels
// together. The Public client is the same code with editing, presence, undo
// and History off, by the access value of the welcome; the server enforces
// that boundary.
'use strict';

import { GRID_KEYS, announce, nextRegion } from './a11y.js';
import { Clipboard, destination } from './clipboard.js';
import { $, button, h, icon, plural, replace } from './dom.js';
import { Editor, Rebaser, barText } from './editor.js';
import { FillChoice, dragTarget, fillOp, keyFill, union } from './fill.js';
import { Find } from './find.js';
import { Grid } from './grid.js';
import { History } from './history.js';
import { edgeJump, gridKey } from './keys.js';
import { Popups, freezeItems, rowsMenu, sortDialog, sortScope, widthDialog } from './menu.js';
import { Model } from './model.js';
import { Net } from './net.js';
import { Notices, copyText } from './notices.js';
import { Presence, agentNotice, presenceMessage } from './presence.js';
import { MAX_WIDTH, MIN_WIDTH, address, colName, isId, ops, packIds, peekedError, rangeName, reasonText, seqCmp, structEdits, undoSkipText, wirePosition } from './protocol.js';
import { Selection } from './select.js';
import { Status, aggregate } from './status.js';
import { Tabs } from './tabs.js';
import { Theme } from './theme.js';
import { Toolbar } from './toolbar.js';

const NAME_KEY = 'flats-sheet-name';
const PEEK_DELAY = 60;   // settle before asking for a formula's text (TECH 12.4)
const FLASH_TIME = 5000; // highlight of cells an agent wrote (rule 27)
const COMMITS_MAX = 200; // own commits remembered for overwrite notices
const COMMITS_BYTES = 2 << 20; // and the most memory their text may hold
const HEAD_WAIT = 15000; // how long a snapshot's head may take before the tab is asked for again

function storedName() {
  try { return localStorage.getItem(NAME_KEY) || ''; } catch { return ''; }
}

// reportFailure tells the browser of an error the page recovered from, the
// way it tells of one nothing caught.
function reportFailure(err) {
  if (typeof globalThis.reportError === 'function') globalThis.reportError(err);
  else console.error(err);
}

// commitKey names an own commit of a cell by the sequence it committed at,
// which an overwrite notice names as the one it replaced.
const commitKey = (tab, row, col, seq) => `${tab}:${row}:${col}:${seq}`;

// addedIds reads the ids that action d added to an axis of tab, from the
// structural edits of its delta, which every connection gets whatever tab
// it watches. It takes no more than count ids.
function addedIds(d, tab, op, count) {
  const out = [];
  const e = structEdits(d).find((x) => x.op === op && x.tab === tab);
  if (!Array.isArray(e?.ids)) return out;
  for (const [first, n] of e.ids) {
    for (let i = 0; i < n && out.length < count; i++) out.push(first + i);
    if (out.length === count) break;
  }
  return out;
}

// added is the largest k of the {new: k} rows and columns that cells
// name, 0 for none.
function added(cells) {
  let rows = 0;
  let cols = 0;
  for (const e of cells) {
    if (typeof e.row !== 'number') rows = Math.max(rows, e.row.new);
    if (typeof e.col !== 'number') cols = Math.max(cols, e.col.new);
  }
  return { rows, cols };
}

// placeAdded gives the cells of an own action that name a row or column
// it added ({new: k}) the id that rowId(k) or colId(k) tells. A cell whose
// id is not told is left out, because no notice can be matched to it.
function placeAdded(c, rowId, colId) {
  const id = (x, of) => (typeof x === 'number' ? x : of(x.new));
  const out = [];
  for (const e of c.cells) {
    const row = id(e.row, rowId);
    const col = id(e.col, colId);
    if (row !== undefined && col !== undefined) out.push(row === e.row && col === e.col ? e : { row, col, text: e.text });
  }
  c.cells = out;
}

// fromDelta places the added cells of own action c by the structural
// edits of its delta, which every connection gets whatever tab it watches.
function fromDelta(c, d) {
  const n = added(c.cells);
  if (!n.rows && !n.cols) return;
  const r = addedIds(d, c.tab, 'insert_rows', n.rows);
  const k = addedIds(d, c.tab, 'insert_cols', n.cols);
  placeAdded(c, (i) => r[i - 1], (i) => k[i - 1]);
}

// fromAck places them by the first ids its ack names. The host gives out
// the ids of an axis in operation order and in a run, and the cells of a
// commit are those of the action's first operation, so {new: k} is the
// first id plus k - 1.
function fromAck(c, m) {
  const n = added(c.cells);
  if (!n.rows && !n.cols) return;
  const at = (first, i) => (first ? first + i - 1 : undefined);
  placeAdded(c, (i) => at(m.added?.rows, i), (i) => at(m.added?.cols, i));
}

export class App {
  constructor({ connect, timers = globalThis, now = () => Date.now(), fetch = globalThis.fetch, measure, raf, functions = [], report = reportFailure } = {}) {
    this.timers = timers;
    this.report = report;    // is told an error the page recovered from
    this.model = new Model();
    this.selection = new Selection(this.model);
    this.view = null;        // an open revision: {revision, tabs, tab, model, selection}
    this.calculating = false;
    this.headTimer = null;   // asks again for a snapshot whose head does not arrive
    this.needName = false;
    this.tabWanted = Number(location.hash.replace(/^#\/?/, '')) || null;
    this.memory = new Map();  // tab id -> {r, c, top, left}: where the person was
    // Own commits for overwrite notices (rule 30): typed edits and pasted
    // text. An action waits in queued until its sequence is known, from its
    // delta or its ack, and the text of each cell it names is then
    // remembered in commits under that sequence. A notice names the commit
    // it replaced by its sequence, so it never offers the text of a later
    // edit of the cell that is still pending or was refused. A cell of a
    // row or column the action adds is named by {new: k} until its delta
    // or its ack tells the id.
    this.queued = new Map();  // action n -> {tab, cells: [{row, col, text}]}
    this.commits = new Map(); // commitKey -> typed text
    this.commitBytes = 0;
    // A notice can come before the text it names is filed: the action's
    // delta was skipped across a resync and its ack comes later. Such a
    // notice waits here for that text while actions are queued.
    this.unfiled = new Map(); // commitKey -> {tab, row, col, replaced}
    this.peekIds = new Map(); // peek request id -> {tab, row, col, changed, answer}
    this.peekTimer = null;
    this.flashTimer = null;
    this.valuesNext = false;
    this.drag = null;
    // The base of the menu whose item runs now (#within).
    this.preparedBase = null;
    this.rendering = false;
    this.barPointer = false;
    this.nameAsked = false;
    this.nameSent = false;    // the name was given; its connection has not answered
    this.reloadAsk = false;
    this.stoppedWhy = '';
    this.bannerSig = '';
    this.lastPresence = '';
    this.facesSig = '';
    this.root = $('#app');
    this.popups = new Popups(this.root, () => this.focusGrid());

    const gridOpts = { model: this.model, selection: this.selection, handlers: this.#pointer(), timers };
    if (measure) gridOpts.measure = measure;
    if (raf) gridOpts.raf = raf;
    this.grid = new Grid(gridOpts);
    this.raf = this.grid.raf;
    this.#build();
    this.notices = new Notices(this.toasts, { timers });
    this.clipboard = new Clipboard(this.model, this.selection);
    this.presence = new Presence();
    this.editor = new Editor({
      grid: this.grid, model: this.model, selection: this.selection, bar: this.bar, host: this.root, functions,
      cb: {
        canWrite: () => this.canWrite(true),
        commit: (c) => this.#commit(c),
        closed: (c) => this.#editorClosed(c),
        peek: (row, col) => this.#peek(row, col),
        reopen: () => this.rebaser.reopen(),
        changed: () => this.changed(),
        composed: () => this.rebaser.composed(),
        refused: (text, why) => this.#offerBack(text, why),
      },
    });
    this.net = new Net(this.#handlers(), { connect, timers, now });
    this.net.name = storedName() || null;
    this.rebaser = new Rebaser({
      editor: this.editor, model: this.model, net: this.net, now,
      cb: {
        apply: (d) => this.#applyDelta(d),
        offer: (text, why, reason) => this.#offerBack(text, why, reason),
        moved: () => this.changed(),
      },
    });
    this.history = new History(this.histEl, {
      fetch,
      now,
      onClose: () => { this.changed(); this.focusGrid(); },
      onView: (v) => this.#viewRevision(v),
      onRolledBack: () => this.changed(),
      onRecovered: () => this.changed(),
      onPeek: () => this.changed(),
      liveTabs: () => this.model.tabs,
      session: () => this.net.session,
      timers,
    });
    this.theme = null;
    this.theme = new Theme(() => {
      if (!this.theme) return;
      // The cell being edited stays in view when the layout changes under
      // it, as when the on-screen keyboard opens.
      if (this.editor.ed && this.cur.tab) { const a = this.sel.active(); this.grid.reveal(a.r, a.c); }
      this.changed();
    });
    this.#events();
    this.#chrome();
    this.net.start();
  }

  // cur is the model on screen: the live tab, or the tab of an open revision.
  get cur() { return this.view ? this.view.model : this.model; }
  get sel() { return this.view ? this.view.selection : this.selection; }
  get readOnly() { return this.net.access !== 'private' || !!this.view || !!this.net.stopped || this.calculating || this.model.stale || this.needName; }

  // A name the person just gave travels with a new connection (nameSent).
  // Until that connection answers or fails (#naming), the page asks for no
  // name (#askName) and does not call the connection lost.
  get #naming() { return this.nameSent && !this.net.failures && !this.net.stopped; }
  get #askName() { return this.needName && !this.#naming; }

  // #unstarted says that the live tab's snapshot has sent its head and no
  // rows yet. The grid is interactive after its first rows (TECH 12.2).
  get #unstarted() { const t = this.model.tab; return !!t && t.loading && !t.started; }

  // ---- shell ----

  #build() {
    this.status = new Status();
    // The flat's name is the first label of its host name; an address has none.
    const host = location.hostname;
    this.titleEl = h('h1', { text: /^[a-z]/i.test(host) && host.includes('.') ? host.split('.')[0] : '' });
    this.badge = h('span', { class: 'badge', text: 'Public · read-only', hidden: true });
    this.facesEl = h('span', { class: 'faces-slot' });
    this.findBtn = h('button', { type: 'button', class: 'btn', 'aria-label': 'Find in this tab (Ctrl or Cmd+F)', title: 'Find in this tab (Ctrl or Cmd+F)', onclick: () => this.find.show() }, icon('find'), h('span', { class: 'hide-s', text: 'Find' }));
    this.csvBtn = h('button', { type: 'button', class: 'btn', 'aria-label': 'Download this tab as CSV', title: 'Download this tab as a CSV file', onclick: () => this.downloadCSV() }, icon('download'), h('span', { class: 'hide-s', text: 'CSV' }));
    this.histBtn = h('button', { type: 'button', class: 'btn', 'aria-pressed': 'false', 'aria-label': 'History: revisions, tab recovery, rollback', title: 'History: revisions, tab recovery, rollback', onclick: () => this.toggleHistory() }, icon('history'), h('span', { class: 'hide-s', text: 'History' }));
    this.keysBtn = h('button', { type: 'button', class: 'btn', 'aria-haspopup': 'dialog', 'aria-label': `Keyboard paths. ${GRID_KEYS}`, title: 'Keyboard paths', onclick: (e) => this.#keysDialog(e.currentTarget) }, icon('keys'), h('span', { class: 'hide-s', text: 'F6: next region' }));
    this.top = h('header', { class: 'top' }, h('span', { class: 'kind' }, icon('sheet'), h('span', { text: 'Sheet' })), this.titleEl, this.badge, h('span', { class: 'grow' }),
      this.status.el, this.facesEl, this.findBtn, this.csvBtn, this.histBtn, this.keysBtn);
    this.banners = h('div', { class: 'banners' });
    this.toolbar = new Toolbar({
      popups: this.popups,
      format: (set, target, texts) => this.format(set, target, texts),
      target: () => this.#target(),
      undo: () => this.undo(false),
      redo: () => this.undo(true),
      sort: (desc) => this.sort(this.sel.active().c, desc, sortScope(this.model, this.selection).rows),
      sortDialog: (at) => this.#sortDialog(at),
      rowsMenu: (at) => this.rowsMenu(at),
      freezeMenu: (at) => this.#freezeMenu(at),
    });
    this.nameBox = h('div', { class: 'namebox', title: 'Selected range', 'aria-hidden': 'true' });
    this.bar = h('textarea', { class: 'finput', rows: '1', spellcheck: 'false', autocomplete: 'off', autocapitalize: 'off', 'aria-label': 'Formula bar: content of the active cell' });
    this.fbar = h('div', { class: 'fbar' }, this.nameBox, h('div', { class: 'fx', 'aria-hidden': 'true', text: 'fx' }), this.bar);
    this.find = new Find({
      model: () => this.cur,
      selection: () => this.sel,
      // The find field keeps focus while the person steps through matches.
      settle: () => this.#settle(false),
      go: (r, c) => {
        this.sel.set(r, c);
        this.grid.reveal(r, c);
        this.changed();
      },
      marks: (set) => { this.grid.overlays.hits = set; this.changed(); },
      closed: () => this.focusGrid(),
    });
    this.fillChoice = new FillChoice({
      choose: (btn) => this.#fillMenu(btn),
      closed: (focused) => { if (focused) this.focusGrid(); },
    });
    this.cover = h('div', { class: 'cover', hidden: true });
    this.tabs = new Tabs({
      open: (id) => this.openTab(id),
      add: (name) => this.#act([ops.addTab(name)], { label: `Add tab ${name}` }, false),
      // The tab bar's rename field, delete confirmation, menu and drag act
      // with the base of the screen they started on.
      seq: () => this.model.seq,
      // A rename the page cannot send gives the typed name back.
      rename: (id, name, base) => {
        if (!this.#act([ops.renameTab(id, name)], { label: 'Rename tab', texts: [name], base }, false)) this.notices.show({ kind: 'bad', text: 'The tab was not renamed. Your text:', kept: name });
      },
      move: (id, before, base) => this.#act([ops.moveTab(id, before)], { label: 'Move tab', base }, false),
      remove: (id, base) => this.#act([ops.deleteTab(id)], { label: 'Delete tab', base }, false),
      needsConfirm: (id) => !(this.model.tab && this.model.tab.id === id && !this.model.tab.loading && this.model.tab.cells.size === 0),
      menu: (label, items, at) => this.popups.menu(label, items, at),
      say: (text, kept) => this.notices.show({ kind: 'bad', text, kept }),
      focusGrid: () => this.focusGrid(),
    }, this.status.agg);
    this.main = h('div', { class: 'main' }, this.top, this.banners, this.toolbar.el, this.fbar, this.grid.el, this.cover, this.tabs.el);
    this.histEl = h('aside', { class: 'hist', 'aria-label': 'History', hidden: true });
    this.toasts = h('div', { class: 'toasts', role: 'region', 'aria-label': 'Notices', 'aria-live': 'polite', 'aria-relevant': 'additions' });
    replace(this.root, this.main, this.histEl, this.toasts, this.find.el, this.fillChoice.el, h('div', { class: 'sr', id: 'grid-keys', text: GRID_KEYS }));
  }

  #events() {
    addEventListener('hashchange', () => {
      const id = Number(location.hash.replace(/^#\/?/, ''));
      if (id && this.model.tabInfo(id) && (!this.model.tab || this.model.tab.id !== id)) this.openTab(id);
    });
    addEventListener('beforeunload', (e) => {
      if (!this.net.pending.length) return;
      e.preventDefault();
      e.returnValue = '';
    });
    addEventListener('resize', () => this.changed());
    // A scroll of the grid closes the fill button (rule 21a).
    this.grid.el.addEventListener('scroll', () => this.fillChoice.check(this.selection, this.grid.el.scrollTop, this.grid.el.scrollLeft));
    this.grid.input.addEventListener('keydown', (e) => { if (this.editor.ed) this.editor.key(e); else this.#gridKey(e); });
    this.bar.addEventListener('keydown', (e) => {
      if (this.editor.ed) { this.editor.key(e); return; }
      if (e.isComposing || e.keyCode === 229) return;
      if (e.key === 'Enter' || e.key === 'F2') { e.preventDefault(); this.editor.fromBar({ keepCaret: false }); }
    });
    this.bar.addEventListener('mousedown', () => { this.barPointer = true; });
    this.bar.addEventListener('focus', () => {
      if (this.barPointer && !this.editor.ed) this.editor.fromBar();
      this.barPointer = false;
    });
    document.addEventListener('keydown', (e) => this.#docKey(e));
    document.addEventListener('copy', (e) => this.#copy(e, false));
    document.addEventListener('cut', (e) => this.#copy(e, true));
    document.addEventListener('paste', (e) => this.#paste(e));
    document.addEventListener('mousedown', (e) => {
      // A press outside the grid, the formula bar and the hints commits the edit.
      if (this.editor.open && !e.target.closest?.('.gridwrap, .fbar, .hint')) this.editor.commit();
    });
  }

  focusGrid() { this.grid.focus(); }

  // ---- connection ----

  #handlers() {
    return {
      resumable: () => this.#grid() === 'current' && !this.switching,
      welcome: (m, info) => this.#welcome(m, info),
      tabHead: (m) => this.#take(() => this.#tabHead(m)),
      rows: (m) => this.#take(() => { this.model.addRows(m); if (!this.view) this.grid.rowsChanged(m.rows.map(([row]) => row)); this.#received(); this.changed(); }),
      tabDone: () => this.#take(() => { this.model.done(); if (!this.view) this.find.search(); this.#received(); this.changed(); }),
      delta: (d, size) => {
        if (d.from && d.from.session_id === this.net.sessionId) this.#ownDelta(d.from.n, d);
        if (!this.rebaser.take(d, size)) this.#applyDelta(d);
        else if (this.peekIds.size) this.#settlePeeks();
      },
      resync: (m) => this.#resynced(m),
      behind: () => { this.#lag('The sheet was loaded again while you were typing'); this.changed(); },
      reset: () => this.#rolledBack(),
      ack: (a, m) => this.#ack(a, m),
      overwritten: (m) => this.#overwritten(m),
      peeked: (m) => this.#peeked(m),
      rebased: (m) => this.rebaser.answer(m),
      presence: (m) => { this.presence.update(m, this.net.sessionId); this.changed(); },
      calculating: (on) => { this.calculating = on; if (on && this.editor.ed) this.#offerBack(this.editor.close(), 'calculating'); this.changed(); },
      stopped: (code, detail) => this.#stopped(code, detail),
      status: () => this.#status(),
    };
  }

  #welcome(m, { resumed, replayed }) {
    const model = this.model;
    // A replayed stream's welcome carries no tab list and no date: the page
    // keeps those of its grid, which match the sequence the stream goes on
    // from, and the replayed deltas change them in order (TECH 12.5).
    if (!replayed) {
      model.setTabs(m.tabs);
      model.date = m.date;
    }
    this.needName = m.access === 'private' && !m.me?.label;
    this.nameSent = false;
    // A welcome comes from a host that is not calculating. The end of a
    // calculation is told to the connections that saw it start, and a
    // rollback makes this page leave that connection before it ends.
    this.calculating = false;
    if (m.access !== 'private') { this.presence.clear(); if (this.history.open) this.history.toggle(false); }
    // The name is asked once; after that the banner offers it.
    if (this.needName && !this.nameAsked) this.#nameDialog();
    const want = this.#wanted();
    // The answer to a peek the lost connection carried never arrives.
    this.peekIds.clear();
    // A new connection knows nothing of where this person is.
    this.lastPresence = '';
    if (resumed && replayed) {
      // The hello asked to go on from a grid that was current, and the
      // stream does: the grid stays as it is.
      this.rebaser.reconnected();
      const ed = this.editor.ed;
      if (ed && this.editor.waitsForPeek(ed.row, ed.col)) this.#peek(ed.row, ed.col);
    } else {
      // The stream does not go on from the grid on screen: a snapshot
      // replaces it. One follows unasked when the server could not replay
      // the tab the hello named; otherwise the tab is asked for.
      this.#lag('The sheet was loaded again while you were typing');
      const unasked = resumed && model.tab && model.tab.id === want;
      if (!unasked && want != null) this.#watchLive(want);
    }
    this.#expectHead();
    this.changed();
  }

  // ---- the grid and the stream ----
  //
  // The stream is what the connection has received; its place is the
  // connection's watermark. The grid is what the person sees: the live tab
  // as of model.seq. They are two things, and #grid names how they stand:
  //
  //   none     no live tab is on screen
  //   stale    the stream went on without the grid (model.stale): a
  //            rollback reset the connection, a frame of the grid was not
  //            taken, a resume was not replayed, a resync arrived, a
  //            structural change met a snapshot still arriving, or a
  //            snapshot older than the stream arrived
  //   loading  the grid's snapshot is still arriving
  //   current  the grid shows every change up to model.seq, and what the
  //            stream holds beyond that is held for the editor
  //
  // Everything that depends on it reads it from here. An edit can start on
  // a loading or a current grid, never on a stale one (readOnly). A hello
  // asks to resume only from a current grid (the resumable handler), so
  // after any welcome the grid is either current or a snapshot is on its
  // way on that connection. Only the head of a snapshot that is not older
  // than the stream ends stale (#tabHead). A head that does not arrive is
  // asked for again (#expectHead), whatever was lost on the way.
  #grid() {
    const t = this.model.tab;
    if (!t) return 'none';
    if (this.model.stale) return 'stale';
    return t.loading ? 'loading' : 'current';
  }

  // #wanted is the live tab the page should show: the one the person asked
  // for, else the one on screen, else the first.
  #wanted() {
    const model = this.model;
    return [this.tabWanted, model.tab?.id].find((id) => id != null && model.tabInfo(id)) ?? model.tabs[0]?.id ?? null;
  }

  // awaiting says that the page waits for the head of a snapshot: the grid
  // is stale, the person asked for another tab, or nothing is on screen.
  get awaiting() { return this.#grid() === 'stale' || this.switching || (this.#grid() === 'none' && this.#wanted() != null); }

  // #expectHead asks for the wanted tab again when its head has not arrived
  // after HEAD_WAIT on an open connection, and goes on doing so. No event
  // is the only way out of waiting: a watch the host dropped, or a
  // snapshot that never started, costs one wait.
  #expectHead() {
    this.timers.clearTimeout(this.headTimer);
    this.headTimer = null;
    if (!this.awaiting || this.net.stopped) return;
    this.headTimer = this.timers.setTimeout(() => {
      const want = this.#wanted();
      if (this.awaiting && this.net.open && want != null) { this.tabWanted = want; this.net.watch(want); }
      this.#expectHead();
    }, HEAD_WAIT);
  }

  // openTab is what a person does: it opens a tab of the revision on
  // screen, or watches a tab of the live sheet.
  openTab(id) {
    if (this.view) { if (this.view.tabs.some((t) => t.id === id)) this.history.openTab(id); return; }
    // The tab that is open and loaded needs no new snapshot. Asking for one
    // would close an edit the person starts right after the click.
    const t = this.model.tab;
    if (this.#grid() === 'current' && t.id === id && this.net.tab === id) return;
    this.#watchLive(id);
  }

  // switching says that the person asked for another tab whose snapshot
  // has not started yet. The grid still shows the tab they left.
  get switching() { return !this.view && !!this.model.tab && this.tabWanted != null && this.tabWanted !== this.model.tab.id; }

  // #watchLive watches a tab of the live sheet, also while a revision is on
  // screen: the connection keeps the live sheet current underneath it. The
  // tab on screen stays until the new snapshot's head arrives.
  #watchLive(id) {
    if (this.editor.open) this.editor.commit();
    this.#remember();
    this.tabWanted = id;
    this.net.watch(id);
    this.#expectHead();
  }

  // #remember keeps where the person was in the live tab. While a revision
  // is on screen the grid's scroll position is the revision's.
  #remember() {
    const t = this.model.tab;
    if (!t || this.view) return;
    const a = this.selection.active();
    this.memory.set(t.id, { r: a.r, c: a.c, top: this.grid.el.scrollTop, left: this.grid.el.scrollLeft });
  }

  // #tabHead starts a snapshot of the live sheet. While a revision is on
  // screen the snapshot fills the live model only: the grid keeps showing
  // the revision, and going back shows the live tab as it is then.
  //
  // The deltas the editor held are dropped here, and with them what they
  // changed of the tab list and the date. The head carries both as of its
  // sequence, so they are installed with the grid, before any edit can
  // start on it. A head older than the stream leaves the stream's own, and
  // the tab is asked for again below.
  #tabHead(m) {
    const same = this.model.tab && this.model.tab.id === m.tab;
    const a = same ? this.selection.active() : null;
    const older = this.net.lastSeq != null && seqCmp(m.seq, this.net.lastSeq) < 0;
    this.#snapshotComing('The tab was loaded again while you were typing');
    if (!older) {
      this.model.setTabs(m.tabs);
      this.model.date = m.date;
    }
    this.model.pending.clear();
    this.model.openHead(m);
    for (const p of this.net.pending) if (p.meta.tab === m.tab && p.meta.pending) this.model.addPending(p.n, p.meta.pending);
    // A snapshot replaces the state a waiting peek was asked on.
    for (const p of this.peekIds.values()) if (p.changed == null || seqCmp(m.seq, p.changed) > 0) p.changed = m.seq;
    this.#settlePeeks();
    // A snapshot older than the stream cannot be brought up to date: the
    // deltas between it and the stream's place went by. The grid stays
    // stale and the tab is asked for again.
    if (older) { this.model.lag(); this.net.watch(m.tab); }
    const at = a || this.memory.get(m.tab) || { r: 0, c: 0 };
    this.selection.set(at.r, at.c);
    this.tabWanted = m.tab;
    if (Number(location.hash.replace(/^#\/?/, '')) !== m.tab) history.replaceState(null, '', `#/${m.tab}`);
    this.#expectHead();
    if (this.view) {
      if (!same) this.memory.set(m.tab, { r: at.r, c: at.c, top: at.top || 0, left: at.left || 0 });
      this.changed();
      return;
    }
    this.grid.show(this.model, this.selection);
    if (!same) { this.grid.el.scrollTop = at.top || 0; this.grid.el.scrollLeft = at.left || 0; }
    this.changed();
    // A loaded tab has a focused cell that accepts a key, unless a dialog holds focus.
    if (document.activeElement === document.body) this.focusGrid();
  }

  // #lag marks the grid on screen as behind the stream for good (see
  // #grid). Until the head of a new snapshot, model.seq stays the sequence
  // of what the person sees, no delta is applied to the grid, no edit can
  // start, and an open editor closes without committing: its text was read
  // against a grid that is being replaced (rule 14).
  #lag(why) {
    this.model.lag();
    this.#snapshotComing(why);
    this.#expectHead();
  }

  // #resynced takes a resync. It carries the tab list and the date as of
  // its sequence: the deltas it skipped, and those the editor held, which
  // are dropped here, may have changed them. When the tab on screen is
  // gone, the host's snapshot is of the first tab, and the page waits for
  // it with nothing on screen, as after a deletion in a delta.
  #resynced(m) {
    const model = this.model;
    const tab = model.tab;
    this.#lag('The sheet was loaded again while you were typing');
    model.setTabs(m.tabs);
    model.date = m.date;
    if (tab && !model.tabInfo(tab.id)) {
      this.notices.show({ kind: 'warn', text: `The tab “${tab.name}” was deleted. Showing another tab.` });
      model.close();
      this.peekIds.clear();
    } else if (tab && this.tabWanted != null && !model.tabInfo(this.tabWanted)) this.#watchLive(tab.id);
    this.changed();
  }

  // #snapshotComing closes an open editor before a new snapshot replaces
  // the grid it was bound to, and offers its text back (TECH 8.4). A resize
  // or a fill under way ends too.
  #snapshotComing(why) {
    this.#dragCheck(true);
    this.rebaser.drop();
    if (!this.editor.ed) return;
    const at = this.editor.position();
    const text = this.editor.close();
    this.notices.show({ kind: 'warn', text: `${why}${at ? ` in ${address(at.r, at.c)}` : ''}. Nothing was written. Your text:`, kept: text });
  }

  // #rolledBack takes a reset. The grid on screen is the sheet before the
  // rollback, so it is behind from here, however long the next welcome
  // takes and whatever the page knew of a calculation: no edit starts on
  // it until the snapshot of the restored sheet.
  #rolledBack() {
    this.#lag('The sheet was rolled back while you were typing');
    this.notices.show({ id: 'rollback', text: 'The sheet was rolled back to an earlier revision. Undo history was cleared for everyone.' });
    this.#forgetCommits();
    this.changed();
  }

  // #take runs the taking of a frame of a snapshot or of a delta, which
  // carry the state of the grid. When it fails, the grid may hold a part
  // of the frame and the screen may show less than the grid, and nothing
  // but a new snapshot is known to be whole. The page then treats the
  // frame as one that did not arrive. The grid is stale from here, so no
  // edit starts on it and an open editor gives its text back, and the
  // connection starts over without resuming (Net.lost). The error is
  // reported and stops here, because a delta the editor held is applied in
  // the middle of closing the editor.
  #take(fn) {
    try {
      fn();
    } catch (err) {
      this.#lag('The sheet was loaded again while you were typing');
      this.net.lost();
      this.report(err);
      this.changed();
    }
  }

  // #applyDelta installs a delta in the live model and draws what changed.
  // While a revision is on screen the grid, its geometry and the find
  // marks belong to the revision, so only the live model moves; going back
  // lays the live tab out again.
  #applyDelta(d) { this.#take(() => this.#install(d)); }

  #install(d) {
    const model = this.model;
    const tab = model.tab;
    const own = d.from && d.from.session_id === this.net.sessionId;
    const tabsBefore = model.tabs;
    const out = model.apply(d);
    const seen = { rows: out.rows, widths: false };
    if (own) model.dropPending(d.from.n, seen);
    if (this.peekIds.size) {
      for (const p of this.peekIds.values()) {
        if (out.text || out.cells.some(([row, col]) => row === p.row && col === p.col)) p.changed = d.seq;
      }
      this.#settlePeeks();
    }
    if (out.rewatch) {
      // The snapshot that is still arriving was taken before this change.
      this.#lag('The tab was loaded again while you were typing');
      this.net.watch(tab.id);
      return;
    }
    // Before a deleted tab gives way to another one.
    if (out.struct || out.gone || out.tabs) this.#dragCheck();
    // The tab the person asked for was deleted before it arrived: the page
    // goes back to asking for the tab on screen.
    if (out.tabs && !out.gone && model.tab && this.tabWanted != null && !model.tabInfo(this.tabWanted)) this.#watchLive(model.tab.id);
    if (out.gone) {
      if (this.editor.ed) this.#offerBack(this.editor.close(), 'gone');
      this.notices.show({ kind: 'warn', text: `The tab “${tab.name}” was deleted. Showing another tab.` });
      model.close();
      this.peekIds.clear();
      if (model.tabs.length) this.#watchLive(model.tabs[0].id);
    }
    const live = !this.view;
    if (out.struct) {
      if (live) this.grid.layout();
      if (this.editor.ed && !this.editor.position()) this.#offerBack(this.editor.close(), 'gone');
    } else if (live && model.tab) {
      if (out.widths || seen.widths) this.grid.widthsChanged();
      this.grid.rowsChanged(seen.rows);
    }
    if (d.note?.agent) this.#agentWrote(d, out, structEdits(d), tabsBefore);
    if (live && (out.cells.length || out.struct)) this.find.search();
    if (out.cells.length) this.#received();
    this.changed();
  }

  // #received opens an editor that waited for its cell's row of the
  // snapshot once the grid holds that cell.
  #received() { if (this.editor.received()) this.rebaser.opened(); }

  #agentWrote(d, out, edits, tabsBefore) {
    this.notices.show({ id: 'agent', kind: 'ok', text: agentNotice(d.note, edits, tabsBefore, this.model.tabs) });
    // note.tab is 0 for a call that wrote in several tabs, so the flash goes
    // by the cells this delta brought to the open tab.
    if (!this.model.tab || this.view || !out.cells.length) return;
    this.grid.overlays.flash = new Set(out.cells.map(([row, col]) => `${row}:${col}`));
    this.timers.clearTimeout(this.flashTimer);
    this.flashTimer = this.timers.setTimeout(() => { this.grid.overlays.flash = null; this.changed(); }, FLASH_TIME);
  }

  // #ack reads the result of an own action: what applied, what was ignored
  // because its target was deleted, and what was refused, each with the
  // typed text offered back (rules 24, 32 and 37).
  #ack(a, m) {
    // An edit that applied committed at the ack's sequence; one that did
    // not is no text a later overwrite can name.
    const c = this.queued.get(a.n);
    this.queued.delete(a.n);
    if (c && m.results.some((r) => r.status === 'applied')) {
      if (m.results[0].status === 'applied') fromAck(c, m);
      this.#committed(c, m.seq);
    }
    if (!this.queued.size) this.unfiled.clear();
    // A refused action has no delta: dropping its pending text and width
    // is what returns the screen to the state the host confirmed, so the
    // grid's column offsets and row heights follow at once (rule 24).
    const seen = { rows: new Set(), widths: false };
    this.model.dropPending(a.n, seen);
    if (!this.view && this.model.tab) {
      if (seen.widths) this.grid.widthsChanged();
      this.grid.rowsChanged(seen.rows);
    }
    const texts = a.meta.texts || [];
    const results = m.results;
    const what = a.meta.label || 'edit';
    // A limit, the quota and the recalculation time refuse a whole action:
    // every operation of it carries the same reason. That is one refusal
    // to the person, so it gets one notice, with all the text they typed.
    const whole = results.length > 1 && results.every((res) => res.status === 'refused' && res.reason === results[0].reason);
    if (whole) {
      const kept = texts.length ? texts.join('\n') : null;
      this.notices.show({ kind: 'bad', text: `The host refused this ${what}: ${reasonText(results[0].reason)}. Nothing was changed${kept != null ? '. Your text:' : '.'}`, kept });
    }
    const parts = results.length > 1;
    (whole ? [] : results).forEach((res, i) => {
      const kept = texts[i] ?? null;
      if (res.status === 'refused' && parts) {
        // The other operations of the action stand as their results say.
        this.notices.show({ kind: 'bad', text: `The host refused a part of this ${what}: ${reasonText(res.reason)}. That part was not applied${kept != null ? '. Your text:' : '.'}`, kept });
      } else if (res.status === 'refused') {
        this.notices.show({ kind: 'bad', text: `The host refused this ${what}: ${reasonText(res.reason)}. Nothing was changed${kept != null ? '. Your text:' : '.'}`, kept });
      } else if (res.status === 'ignored') {
        this.notices.show({ kind: 'warn', text: `Your ${what} was ignored: ${reasonText(res.reason || 'gone')}${kept != null ? '. Your text:' : '.'}`, kept });
      } else if (res.ignored) {
        // A cut cell whose destination was deleted stays, and so does one
        // whose destination is such a cell, so the cells of a cut that did
        // not move are not all in the deleted rows or columns. A cut cell
        // whose own row or column was deleted did not move either, and is
        // gone. The ack does not tell the two apart, so the notice names
        // both and offers the text of the cut back.
        const total = plural((res.applied || 0) + res.ignored, 'cell');
        const said = a.meta.cut
          ? `Your ${what} moved ${res.applied || 0} of its ${total}. Rows or columns were deleted before the host received it, so the other ${res.ignored} did not move. A cut cell that did not move keeps its content, unless another edit deleted its own row or column and the cell with it.`
          : `Your ${what} applied to ${res.applied || 0} of its ${total}. The other ${res.ignored} lay in rows or columns that were deleted before the host received it.`;
        this.notices.show({ kind: 'warn', text: `${said}${kept != null ? ' The text:' : ''}`, kept });
      }
    });
    // A cut moves its cells only to destinations that applied. When none
    // did, every cut cell is where it was, and a paste can move it again.
    if (a.meta.cut && results.every((res) => !res.applied)) this.clipboard.unmoved(a.meta.cut);
    if (m.undo) this.#undoResult(a.ops[0].op === 'redo', m.undo);
    // A fill drag that applied to a cell leaves its button (rule 21a). One
    // whose cells were all deleted first applied to none.
    if (a.meta.fillDrag) this.fillChoice.acked(a.n, results[0]?.status === 'applied' && results[0].applied > 0);
    this.changed();
  }

  #undoResult(redo, u) {
    if (!u.applied && !u.skipped) {
      this.notices.show({ id: 'undo', text: redo ? 'Nothing to redo.' : 'No earlier undo is available.' });
      return;
    }
    if (!u.skipped) return;
    this.notices.show({ id: 'undo', kind: 'warn', text: undoSkipText(redo, u) });
  }

  // #ownDelta files the cells of own action n at the sequence of its
  // delta, whose structural edits give the ids of the rows and columns the
  // action added. The delta's cells say nothing of the action's: they are
  // those of the tab the connection watches, written or recalculated.
  #ownDelta(n, d) {
    const c = this.queued.get(n);
    if (!c) return;
    this.queued.delete(n);
    fromDelta(c, d);
    this.#committed(c, d.seq);
    if (!this.queued.size) this.unfiled.clear();
  }

  // #committed files the text of each cell of own action c under the
  // sequence it committed at. Neither the delta nor the ack tells which
  // cells the host ignored, and a cell it ignored is filed too. No notice
  // offers it: a notice names a cell this session wrote at that sequence.
  // A notice that came first offers its text now.
  #committed(c, seq) {
    const waiting = [];
    for (const e of c.cells) {
      if (typeof e.row !== 'number' || typeof e.col !== 'number') continue;
      const k = commitKey(c.tab, e.row, e.col, seq);
      if (this.commits.has(k)) continue;
      this.commits.set(k, e.text);
      if (this.unfiled.has(k)) waiting.push(k);
      // A text is held as UTF-16, two bytes a unit at most.
      this.commitBytes += 2 * e.text.length;
      for (const [old, text] of this.commits) {
        if (this.commits.size <= COMMITS_MAX && this.commitBytes <= COMMITS_BYTES) break;
        this.commits.delete(old);
        this.commitBytes -= 2 * text.length;
      }
    }
    for (const k of waiting) {
      const m = this.unfiled.get(k);
      this.unfiled.delete(k);
      if (this.commits.has(k)) this.#overwritten(m);
    }
  }

  #forgetCommits() {
    this.queued.clear();
    this.commits.clear();
    this.commitBytes = 0;
    this.unfiled.clear();
  }

  // #overwritten shows the notice of a replaced commit, with its text when
  // it is filed. While an action is queued it may file that text later, so
  // the notice waits for it, and showing it again replaces the earlier one.
  #overwritten(m) {
    const k = commitKey(m.tab, m.row, m.col, m.replaced);
    const text = this.commits.get(k);
    let where = 'a cell';
    if (this.model.tab && this.model.tab.id === m.tab) {
      const r = this.model.rowIndex(m.row);
      const c = this.model.colIndex(m.col);
      if (r >= 0 && c >= 0) where = address(r, c);
    }
    this.notices.show({ id: `overwritten:${k}`, kind: 'warn', text: `Someone changed ${where} after you typed in it. The value the host received later stays${text != null ? '. Your text:' : '.'}`, kept: text ?? null });
    if (text != null || !this.queued.size) return;
    this.unfiled.delete(k);
    this.unfiled.set(k, { tab: m.tab, row: m.row, col: m.col, replaced: m.replaced });
    if (this.unfiled.size > COMMITS_MAX) this.unfiled.delete(this.unfiled.keys().next().value);
  }

  // #cover replaces the grid when the sheet cannot be shown at all: a
  // Public sheet that was withdrawn, or a connection the host refused
  // before anything loaded.
  #cover() {
    const net = this.net;
    const closed = !!net.stopped && net.access !== 'private';
    if (closed === !this.cover.hidden) return;
    for (const el of [this.fbar, this.grid.el, this.tabs.el]) el.hidden = closed;
    this.cover.hidden = !closed;
    if (!closed) return;
    this.model.close();
    const text = net.access === 'public'
      ? 'It was open on a public address and is no longer available there. The connection was closed and its content was removed from this screen. Nothing you selected or scrolled was saved.'
      : `${this.stoppedWhy} Nothing was loaded.`;
    replace(this.cover, h('div', { class: 'box', role: 'alert' }, h('h2', { text: 'This sheet is not available here' }), h('p', { text }), button('Try again', () => location.reload(), { class: 'btn' })));
  }

  #stopped(code, detail) {
    if (this.editor.ed) this.#offerBack(this.editor.close(), 'stopped');
    this.stoppedWhy = {
      session_expired: 'The host no longer knows this browser tab’s session, so it cannot accept your unsaved edits.',
      receipt_expired: 'The host no longer keeps the outcome of an unsaved edit, so it cannot tell whether it was applied.',
      too_large: 'One edit was larger than the host accepts.',
      protocol: 'The host could not read what this page sent. Reload to get the current sheet.',
      deleted: 'This sheet was deleted.',
      renamed: 'This sheet was renamed and has a new address.',
    }[code] || detail.message || `The host closed the sheet (${code}).`;
    this.changed();
  }

  #status() {
    const net = this.net;
    // A Public sheet that stays unreachable was withdrawn: its content leaves the screen.
    if (net.access === 'public' && net.failures >= 3 && !net.stopped) { net.stop('closed'); return; }
    this.status.set(net.status, net.pending.length, this.calculating, net.access == null || this.#naming);
    this.#cover();
    this.#banners();
  }

  // ---- writing ----

  // canWrite says whether an edit can start; with notify it tells the
  // person why not, and offers kept back: text already read for the edit.
  canWrite(notify = false, grid = true, kept = null) {
    // After a click on another tab nothing is typed into the tab that is
    // still on screen: the person means the one they asked for. Nothing is
    // done in a grid that has no rows yet either. What is done in the tab
    // bar names its tab and goes on.
    if (!this.readOnly && this.model.tab && !(grid && (this.switching || this.#unstarted))) return true;
    if (!notify) return false;
    const text = this.net.access === 'public' ? 'This sheet is Public here: it can be read, copied and downloaded, not edited.'
      : this.view ? 'A revision is read-only. Go back to the current sheet to edit.'
      : this.net.stopped ? 'Editing has stopped in this browser tab. Copy your unsaved edits from the notice at the top, then reload.'
      : this.calculating ? 'The sheet is calculating. Editing is available again when it ends.'
      : this.#askName ? 'Enter your name to start editing.'
      : this.model.stale ? 'The sheet is loading again. Editing is available in a moment.'
      : this.switching ? 'The tab you opened is still loading. Editing is available when it shows.'
      : this.#unstarted ? 'The tab is still loading. Editing is available when its first rows show.'
      : 'The sheet is still loading.';
    // A notice that offers text back is not replaced by the next one.
    if (kept != null) this.notices.show({ kind: 'warn', text: `${text} Nothing was written. The text:`, kept });
    else this.notices.show({ id: 'readonly', text });
    if (this.#askName && !this.popups.open) this.#nameDialog();
    return false;
  }

  // #act sends one action with the screen's sequence as its base. An
  // action prepared earlier, by a dialog, a menu or a drag, gives the
  // sequence of the screen it was prepared on as meta.base instead, so the
  // host ignores what was deleted or came back since (TECH 7.1). meta
  // keeps what notices need (a label, the typed text of each operation, the
  // cell and text of an edit for overwrite notices) and what the screen
  // shows until the server answers: pending text and widths.
  // grid is false for an action of the tab bar, which does not write into
  // the tab on screen.
  // Every action a person makes is queued here and nowhere else. One that
  // is larger than an action may be is refused before it is queued, with
  // the text the person typed. Once queued it could never be sent, and
  // editing would stop for it and for every action behind it.
  #act(list, { base: prepared, ...meta } = {}, grid = true) {
    if (!this.canWrite(true, grid)) return null;
    // The next edit closes the button of a fill drag (rule 21a).
    this.fillChoice.close();
    const tab = this.model.tab.id;
    const base = prepared ?? this.preparedBase ?? this.model.seq;
    if (!this.net.fits(list, base)) {
      const kept = meta.texts?.length ? meta.texts.join('\n') : null;
      this.notices.show({ kind: 'bad', text: `This ${meta.label || 'edit'} is larger than one edit can carry. Nothing was changed${kept != null ? '. Your text:' : '.'}`, kept });
      return null;
    }
    // Pending text keeps the screen it was typed on, so an edit that
    // reopens a formula of it adjusts the formula from there (TECH 8.4).
    // An action prepared earlier gives each cell's position on the screen
    // of its base; the screen on show may have moved it since.
    if (meta.pending?.length) meta = { ...meta, pending: meta.pending.map((e) => ({ ...e, base, at: e.at ?? wirePosition(this.model.rowIndex(e.row), this.model.colIndex(e.col)) })) };
    const a = this.net.queue(list, base, { tab, ...meta });
    if (!a) return null;
    // Its answer can arrive while it is sent.
    if (meta.commit) this.queued.set(a.n, { tab, cells: meta.commit });
    if (meta.pending?.length) { this.model.addPending(a.n, meta.pending); this.grid.rowsChanged(meta.pending.map((e) => e.row)); }
    if (meta.widths) { this.model.addPendingWidth(a.n, meta.widths.cols, meta.widths.width); this.grid.widthsChanged(); }
    this.net.flush();
    this.changed();
    return a;
  }

  #commit({ row, col, text, at, move, focus }) {
    const op = ops.set(this.model.tab.id, row, col, text, wirePosition(at.r, at.c));
    this.#act([op], { label: `edit of ${address(at.r, at.c)}`, texts: [text], pending: [{ row, col, text }], commit: [{ row, col, text }] });
    this.#editorClosed({ move, row, col, focus });
  }

  // #editorClosed runs after every close: the deltas the editor held now
  // apply and its exchange is forgotten, the selection moves, and focus is
  // back in the grid unless the caller keeps it.
  #editorClosed({ move, row, col, focus = true }) {
    this.rebaser.closed();
    const r = move ? this.model.rowIndex(row) : -1;
    const c = move ? this.model.colIndex(col) : -1;
    if (r >= 0 && c >= 0) {
      this.selection.set(r + move[0], c + move[1]);
      const a = this.selection.active();
      this.grid.reveal(a.r, a.c);
    }
    this.changed();
    if (focus) this.focusGrid();
  }

  // #settle ends an open edit before the active cell moves (rule 21). The
  // text area is drawn at the active cell and the formula bar names it, so
  // the edit commits first. An edit that cannot commit now (its formula has
  // not arrived, it is read-only during an exchange, or the page cannot
  // send it) stays on its cell with its text. The selection must not move
  // then, and #settle returns false.
  #settle(focus = true) {
    if (!this.editor.ed) return true;
    this.editor.commit(null, { focus });
    return !this.editor.ed;
  }

  #offerBack(text, why, reason) {
    if (text == null || text === '') return;
    const say = {
      refused: `The host could not adjust the formula you were typing to the changed grid (${reasonText(reason)}). It was not written. Your text:`,
      overflow: 'Too many changes arrived while you were typing a formula, so the editor closed. Nothing was written. Your text:',
      long: 'The grid changed while you were typing a formula that is too long for the host to adjust, so the editor closed. Nothing was written. Your text:',
      ahead: 'The grid changed before the formula you were editing arrived, so the keys you typed cannot follow it. Nothing was written. Your text:',
      changed: 'The cell changed before your edit in the formula bar opened, so the text you edited is no longer its text. Nothing was written. Your text:',
      moved: 'The selection moved before the formula bar showed the new cell, so the text you edited is of another cell. Nothing was written. Your text:',
      gone: 'The row, column or tab of the cell you were editing was deleted. Nothing was written. Your text:',
      calculating: 'The sheet started calculating while you were typing. Nothing was written. Your text:',
      stopped: 'Editing stopped while you were typing. Nothing was written. Your text:',
      revision: 'A revision opened while you were typing. Nothing was written. Your text:',
    }[why];
    this.notices.show({ kind: 'warn', text: say, kept: text });
  }

  // format sets format properties on the selection, or on the target a
  // dialog was opened for. texts are the settings typed in a dialog, which
  // a host refusal offers back.
  format(set, target, texts) {
    if (target) {
      const to = this.#bound(target);
      if (!to) return null;
      const m = this.model;
      const b = { r1: m.rowIndex(to.rows[0]), c1: m.colIndex(to.cols[0]), r2: m.rowIndex(to.rows.at(-1)), c2: m.colIndex(to.cols.at(-1)) };
      return this.#act([ops.format(to.tab, packIds(to.rows), packIds(to.cols), set)], { label: `format of ${rangeName(b)}`, base: target.base, ...(texts && { texts }) });
    }
    const b = this.selection.box();
    const ids = this.selection.ids(b);
    return this.#act([ops.format(this.model.tab.id, ids.rows, ids.cols, set)], { label: `format of ${rangeName(b)}`, ...(texts && { texts }) });
  }

  // #target names what a dialog or a menu acts on, taken when it opens:
  // the open tab, the ids of the selected rows and columns, and the
  // sequence and the row and column lists of the screen they were taken
  // on. It acts on those rows and
  // columns wherever they have moved since (rule 21), with that sequence as
  // its base, so none of them that was deleted and came back since is
  // changed (rule 34).
  #target() {
    const t = this.model.tab;
    if (!t) return { tab: null, rows: [], cols: [], base: this.model.seq };
    const b = this.selection.box();
    return { tab: t.id, rows: t.rows.slice(b.r1, b.r2 + 1), cols: t.cols.slice(b.c1, b.c2 + 1), base: this.model.seq, screen: { rows: t.rows, cols: t.cols } };
  }

  // #within runs a menu item with the base of the screen its menu was
  // opened on, for the actions it sends.
  #within(target, fn) {
    const was = this.preparedBase;
    this.preparedBase = target.base;
    try {
      return fn();
    } finally {
      this.preparedBase = was;
    }
  }

  // #bound resolves a target against the open tab: the ids of its rows and
  // columns that still exist, in position order. A rows or cols of null is
  // not part of the target. When its tab is no longer the open one, or none
  // of its rows or none of its columns is left, the person is told and
  // nothing changes.
  #bound(target) {
    const m = this.model;
    const live = (ids, at) => ids.filter((id) => at(id) >= 0).sort((a, b) => at(a) - at(b));
    if (m.tab && target.tab === m.tab.id) {
      const rows = target.rows && live(target.rows, (id) => m.rowIndex(id));
      const cols = target.cols && live(target.cols, (id) => m.colIndex(id));
      if ((!rows || rows.length) && (!cols || cols.length)) return { tab: target.tab, rows, cols };
    }
    this.notices.show({ kind: 'bad', text: 'The tab, rows or columns this was opened for were deleted, or another tab is open now. Nothing was changed.' });
    return null;
  }

  // #same says whether a menu still acts on what it was opened for: its
  // tab is open and, with lines, the selection names the same rows and
  // columns. A menu acts on the selection when its item is chosen. kept is
  // text the item has already read, which the notice offers back.
  #same(target, lines = true, kept = null) {
    const m = this.model;
    let same = !!m.tab && m.tab.id === target.tab;
    if (same && lines) {
      const now = this.#target();
      const eq = (a, b) => a.length === b.length && a.every((id, i) => id === b[i]);
      same = eq(now.rows, target.rows) && eq(now.cols, target.cols);
    }
    if (!same) this.notices.show({ kind: 'bad', text: `The tab, rows or columns this menu was opened for changed. Nothing was changed; open the menu again${kept != null ? '. The text:' : '.'}`, kept });
    return same;
  }

  clear() {
    const m = this.model;
    const b = this.selection.box();
    const ids = this.selection.ids(b);
    const pending = [];
    m.eachCell(b, (r, c) => pending.push({ row: m.rowAt(r), col: m.colAt(c), text: '' }));
    this.#act([ops.clear(m.tab.id, ids.rows, ids.cols)], { label: `clearing of ${rangeName(b)}`, pending });
  }

  undo(redo) { this.#act([redo ? ops.redo() : ops.undo()], { label: redo ? 'redo' : 'undo' }); }

  insertRows(side) {
    const b = this.selection.box();
    const beside = this.model.rowAt(side === 'before' ? b.r1 : b.r2);
    this.#act([ops.insertRows(this.model.tab.id, beside, side, b.r2 - b.r1 + 1)], { label: 'insert of rows' });
  }

  insertCols(side) {
    const b = this.selection.box();
    const beside = this.model.colAt(side === 'before' ? b.c1 : b.c2);
    this.#act([ops.insertCols(this.model.tab.id, beside, side, b.c2 - b.c1 + 1)], { label: 'insert of columns' });
  }

  deleteRows() {
    const b = this.selection.box();
    if (b.r2 - b.r1 + 1 >= this.model.rowCount) { this.notices.show({ kind: 'bad', text: 'A tab keeps at least one row.' }); return; }
    this.#act([ops.deleteRows(this.model.tab.id, this.selection.ids(b).rows)], { label: 'deletion of rows' });
  }

  deleteCols() {
    const b = this.selection.box();
    if (b.c2 - b.c1 + 1 >= this.model.colCount) { this.notices.show({ kind: 'bad', text: 'A tab keeps at least one column.' }); return; }
    this.#act([ops.deleteCols(this.model.tab.id, this.selection.ids(b).cols)], { label: 'deletion of columns' });
  }

  // sort reorders whole rows by one column: `rows` is "all" or a range of
  // positions (rule 22).
  sort(c, desc, rows) {
    const t = this.model.tab;
    if (!Number.isInteger(c) || c < 0 || c >= t.cols.length) return null;
    const which = rows === 'all' ? 'all' : packIds(t.rows.slice(rows.r1, rows.r2 + 1));
    return this.#act([ops.sort(t.id, which, t.cols[c], desc)], { label: `sort by column ${colName(c)}` });
  }

  freeze(rows, cols) {
    const t = this.model.tab;
    this.#act([ops.settings(t.id, rows ?? t.frozenRows, cols ?? t.frozenCols)], { label: 'freeze' });
  }

  // setWidth sends a column width. A width typed in the dialog is offered
  // back when the host refuses it.
  setWidth(c1, c2, width, typed = false) {
    return this.#width(this.model.tab.cols.slice(c1, c2 + 1), width, typed);
  }

  #width(cols, width, typed, base) {
    const t = this.model.tab;
    return this.#act([ops.width(t.id, packIds(cols), width)], { label: 'column width', base, widths: { cols, width }, ...(typed && { texts: [String(width)] }) });
  }

  // fitColumn measures the loaded cells of a column and sets its width to
  // the widest line (TECH 19.5). While the tab loads, some of its cells
  // have not arrived and nothing is measured.
  fitColumn(c) {
    const m = this.model;
    if (m.tab.loading) { this.notices.show({ id: 'loading', text: 'The tab is still loading, so the column was not measured. Try again when it has loaded.' }); return; }
    const col = m.colAt(c);
    let w = MIN_WIDTH;
    for (const cells of m.tab.cells.values()) {
      const cell = cells.get(col);
      if (!cell?.kind) continue;
      const bold = !!m.format(cell).bold;
      for (const line of cell.shown.split('\n')) w = Math.max(w, this.grid.measure.text(line, bold) + 16);
    }
    this.setWidth(c, c, Math.min(MAX_WIDTH, Math.ceil(w)));
  }

  fill(dir) {
    const f = keyFill(this.selection.box(), dir);
    if (!f) return;
    this.#act([fillOp(this.model.tab, f.source, f.target)], { label: `fill ${dir}` });
  }

  rowsMenu(at) {
    if (!this.canWrite(true)) return;
    const point = at || this.#cellPoint();
    // An item acts on what the menu was opened for. One worded for the
    // selection acts while the selection names the same rows and columns;
    // freeze and the adding at the end act while the same tab is open. A
    // dialog or a clipboard read an item starts keeps the menu's target
    // and base.
    const target = this.#target();
    const lines = (fn) => (...a) => { if (this.#same(target)) this.#within(target, () => fn(...a)); };
    const tab = (fn) => (...a) => { if (this.#same(target, false)) this.#within(target, () => fn(...a)); };
    this.popups.menu('Rows and columns', rowsMenu(this.model, this.selection, {
      insertRows: lines((side) => this.insertRows(side)),
      insertCols: lines((side) => this.insertCols(side)),
      deleteRows: lines(() => this.deleteRows()),
      deleteCols: lines(() => this.deleteCols()),
      widthDialog: lines(() => this.#widthDialog(point, target)),
      fit: lines(() => { const b = this.selection.box(); for (let c = b.c1; c <= Math.min(b.c2, b.c1 + 25); c++) this.fitColumn(c); }),
      fill: lines((dir) => this.fill(dir)),
      pasteValues: lines(() => this.#pasteValuesCommand(target)),
      sortDialog: lines(() => this.#sortDialog(point, target)),
      freeze: tab((r, c) => this.freeze(r, c)),
      appendRows: tab((n) => this.#act([ops.appendRows(this.model.tab.id, n)], { label: 'adding of rows' })),
      appendCols: tab((n) => this.#act([ops.appendCols(this.model.tab.id, n)], { label: 'adding of columns' })),
    }), point);
  }

  #cellPoint() {
    const a = this.sel.active();
    const r = this.grid.cellRect(a.r, a.c);
    return { x: r.left + 20, y: r.bottom + 2 };
  }

  #freezeMenu(at) {
    if (!this.canWrite(true)) return;
    const t = this.model.tab;
    const target = this.#target();
    this.popups.menu('Freeze', [
      { head: `Frozen now: ${plural(t.frozenRows, 'row')}, ${plural(t.frozenCols, 'column')}` },
      ...freezeItems(t, this.selection.box(), { freeze: (r, c) => { if (this.#same(target, false)) this.#within(target, () => this.freeze(r, c)); } }),
    ], at);
  }

  // The sort and width dialogs act on the tab, rows and columns they were
  // opened for (rule 21), with the base of that screen. One a menu opens
  // takes the menu's target, which has the base of the screen the menu
  // opened on.
  #sortDialog(at, target = this.#target()) {
    if (!this.canWrite(true)) return;
    const { tab, base } = target;
    sortDialog(this.popups, this.model, this.selection, at, (by, desc, rows) => {
      const to = this.#bound({ tab, rows: rows === 'all' ? null : rows, cols: [by] });
      if (!to) return null;
      const name = colName(this.model.colIndex(by));
      return this.#act([ops.sort(tab, rows === 'all' ? 'all' : packIds(to.rows), by, desc)], { label: `sort by column ${name}`, base, texts: [`Sort by column ${name}, ${desc ? 'descending' : 'ascending'}`] });
    });
  }

  #widthDialog(at, opened = this.#target()) {
    const target = { ...opened, rows: null };
    widthDialog(this.popups, this.model.widthOf(target.cols[0]), at, (w) => {
      const to = this.#bound(target);
      return to && this.#width(to.cols, w, true, target.base);
    });
  }

  // ---- clipboard ----

  #copy(e, cut) {
    if (this.editor.ed || !this.grid.focused || !this.cur.tab) return;
    if (cut && !this.canWrite(true)) return;
    this.clipboard.model = this.cur;
    this.clipboard.selection = this.sel;
    // A copy from a revision is text only: its cells are not the live cells
    // of the same ids, so a later paste must not copy from those.
    const text = this.clipboard.copyText(cut, !this.view);
    if (text === false) {
      this.notices.show({ id: 'copy-size', kind: 'bad', text: `This selection is too large to ${cut ? 'cut' : 'copy'}. Select fewer cells.` });
      return;
    }
    // Cells whose rows have not arrived are not blank, so they give no
    // text, and the clipboard keeps what it held.
    if (text == null) {
      this.notices.show({ id: 'loading', text: `This part of the tab is still loading. Nothing was ${cut ? 'cut' : 'copied'}; try again when it has loaded.` });
      return;
    }
    e.clipboardData.setData('text/plain', text);
    e.preventDefault();
    announce(`${cut ? 'Cut' : 'Copied'} ${rangeName(this.sel.box())}.`);
  }

  #paste(e) {
    if (this.editor.ed || !this.grid.focused) return;
    e.preventDefault();
    const values = this.valuesNext;
    this.valuesNext = false;
    this.paste(e.clipboardData.getData('text/plain'), values);
  }

  // paste sends clipboard text as one action: `copy` inside the sheet,
  // which carries formulas and formats, or `write` with the typed strings.
  // A paste asked for earlier gives its destination and base. The text of
  // such a paste was read from the clipboard, which may hold other text
  // by now, so a paste that cannot be sent offers it back.
  paste(text, valuesOnly = false, dest = null, base = null) {
    this.clipboard.model = this.model;
    this.clipboard.selection = this.selection;
    // Empty text from another program has nothing to write.
    if (!this.canWrite(true, true, dest && text !== '' ? text : null) || (text === '' && !this.clipboard.remembers(text))) return;
    // A paste too large for an action is refused here, with the base it
    // would be sent on, before its cells are built or its cut is taken.
    const at = base ?? this.preparedBase ?? this.model.seq;
    const p = this.clipboard.pasteOps(text, valuesOnly, dest ?? destination(this.model, this.selection), (list) => this.net.fits(list, at));
    if (!p) {
      this.notices.show({ kind: 'bad', text: 'This paste holds more cells or text than one edit can carry. Nothing was changed; paste it in smaller parts. The text:', kept: text });
      return;
    }
    this.#act(p.ops, { label: 'paste', texts: [text], pending: p.pending, cut: p.cut, base: at, commit: p.typed });
  }

  // #pasteValuesCommand reads the clipboard, which answers later. The
  // paste goes to the rows and columns under the selection when the item
  // was chosen, wherever they are when the text arrives, and sends with
  // the base of the screen its menu opened on. It needs the same tab open
  // but not the same selection, because the destination is already fixed.
  // Text that arrives when it cannot be pasted is offered back.
  #pasteValuesCommand(target) {
    const cb = globalThis.navigator?.clipboard;
    if (!cb?.readText) {
      this.notices.show({ text: 'Press Ctrl or Cmd+Shift+V in the grid to paste values only.' });
      return;
    }
    const dest = destination(this.model, this.selection, target.screen);
    cb.readText().then((text) => { if (this.#same(target, false, text === '' ? null : text)) this.paste(text, true, dest, target.base); }, () => this.notices.show({ text: 'The clipboard could not be read. Press Ctrl or Cmd+Shift+V in the grid to paste values only.' }));
  }

  downloadCSV() {
    const t = this.model.tab;
    if (!t || this.view) return;
    const a = h('a', { href: `/_sheet/csv?tab=${t.id}`, download: `${t.name}.csv`, hidden: true });
    this.root.appendChild(a);
    a.click();
    a.remove();
    this.notices.show({ id: 'csv', kind: 'ok', text: `Downloading “${t.name}.csv”: a CSV file of this tab, one field per cell, up to the last filled cell.` });
  }

  // ---- keyboard ----

  #gridKey(e) {
    const cmd = gridKey(e);
    if (!cmd) return; // the key types into the text area and starts an edit
    if (cmd.type === 'pasteValues') {
      // The paste event that follows carries the text.
      this.valuesNext = true;
      this.timers.setTimeout(() => { this.valuesNext = false; }, 600);
      return;
    }
    const m = this.cur;
    if (!m.tab) return;
    const s = this.sel;
    const a = s.active();
    const from = cmd.extend ? s.far() : a;
    const go = (r, c) => { if (cmd.extend) s.extend(r, c); else s.set(r, c); const p = cmd.extend ? s.far() : s.active(); this.grid.reveal(p.r, p.c); };
    e.preventDefault();
    switch (cmd.type) {
      case 'move': {
        if (cmd.jump) { const p = edgeJump((r, c) => m.filledAt(r, c), m.rowCount, m.colCount, from.r, from.c, cmd.dr, cmd.dc); go(p.r, p.c); } else go(from.r + cmd.dr, from.c + cmd.dc);
        break;
      }
      case 'page': go(from.r + cmd.dir * this.grid.rowsPerPage(), from.c); break;
      case 'home': go(cmd.all ? 0 : from.r, 0); break;
      case 'end': {
        const used = m.usedRange();
        if (cmd.all) { go(Math.max(0, used.r), Math.max(0, used.c)); break; }
        let c = 0;
        for (let i = m.colCount - 1; i > 0; i--) if (m.filledAt(from.r, i)) { c = i; break; }
        go(from.r, c);
        break;
      }
      case 'select': {
        const b = s.box();
        if (cmd.what === 'all') s.all(); else if (cmd.what === 'rows') s.rows(b.r1, b.r2); else s.cols(b.c1, b.c2);
        break;
      }
      case 'edit': this.editor.start({ source: this.theme.phone ? 'bar' : 'cell' }); break;
      case 'clear': if (this.canWrite(true)) this.clear(); break;
      case 'menu': this.rowsMenu(); break;
      case 'undo': this.undo(false); break;
      case 'redo': this.undo(true); break;
      case 'mark': if (this.canWrite(true) && this.#formatKnown()) this.format({ [cmd.mark]: !this.#activeFormat()[cmd.mark] }); break;
      case 'fill': if (this.canWrite(true)) this.fill(cmd.dir); break;
    }
    this.changed();
  }

  #activeFormat() {
    const a = this.sel.active();
    return this.cur.format(this.cur.cellAt(a.r, a.c));
  }

  // #activeKnown says whether the active cell has arrived, so a format
  // toggled from its format starts from the format it has.
  #activeKnown() {
    const m = this.cur;
    const a = this.sel.active();
    return m.known(m.rowAt(a.r), m.colAt(a.c));
  }

  #formatKnown() {
    if (this.#activeKnown()) return true;
    this.notices.show({ id: 'loading', text: 'This cell is still loading, so its format is not known yet. Try again when it has loaded.' });
    return false;
  }

  #docKey(e) {
    if (e.isComposing || e.keyCode === 229 || this.popups.modal) return;
    const mod = e.metaKey || e.ctrlKey;
    if (e.key === 'F6') {
      e.preventDefault();
      if (this.editor.open) this.editor.commit();
      this.popups.close(false);
      this.#cycle(e.shiftKey ? -1 : 1);
      return;
    }
    if (mod && e.key.toLowerCase() === 'f') { e.preventDefault(); this.find.show(); return; }
    if (e.key === 'Escape' && !this.editor.ed && this.fillChoice.open) { e.preventDefault(); this.fillChoice.close(); return; }
    if (e.key === 'Escape' && !this.editor.ed && !this.grid.focused && !this.popups.open) { e.preventDefault(); this.focusGrid(); }
  }

  // #cycle moves focus along the F6 route: top bar, toolbar, formula bar,
  // grid, the button of a fill drag when it shows, tab bar, then History
  // and the notices when they are shown.
  #cycle(dir) {
    const first = (root, sel) => () => (root.hidden ? null : root.querySelector(sel));
    const regions = [
      { name: 'Top bar', root: this.top, target: first(this.top, 'button:not([disabled]):not([hidden])') },
      { name: 'Toolbar', root: this.toolbar.el, target: first(this.toolbar.el, 'button:not([disabled]), select:not([disabled])') },
      { name: 'Formula bar', root: this.fbar, target: () => this.bar },
      { name: 'Grid', root: this.grid.el, target: () => (this.cur.tab ? this.grid.input : null) },
      { name: 'Fill options', root: this.fillChoice.el, target: () => (this.fillChoice.open ? this.fillChoice.btn : null) },
      { name: 'Tabs', root: this.tabs.el, target: () => this.tabs.list.querySelector('[aria-selected="true"]') || this.tabs.addBtn },
      { name: 'History', root: this.histEl, target: first(this.histEl, 'button') },
      { name: 'Notices', root: this.toasts, target: first(this.toasts, 'button') },
    ];
    const next = nextRegion(regions, document.activeElement, dir);
    if (!next) return;
    next.el.focus();
    announce(next.name);
  }

  // ---- pointer ----

  #pointer() {
    return {
      down: (hit, e) => this.#down(hit, e),
      drag: (hit, e) => this.#dragTo(hit, e),
      up: () => this.#up(),
      dbl: (hit) => {
        if (hit.zone === 'cell') this.editor.start({ source: this.theme.phone ? 'bar' : 'cell' });
        else if (hit.zone === 'resize' && this.canWrite(true)) this.fitColumn(hit.c);
      },
      // A screen reader hears each move: a label that changes on the focused
      // text area is not read again by itself.
      label: (text) => { if (this.grid.focused && !this.editor.ed) announce(text, '#announce-cell'); },
      context: (hit, e) => {
        if (this.readOnly) return;
        e.preventDefault();
        if (this.#selectForMenu(hit)) this.rowsMenu({ x: e.clientX, y: e.clientY });
      },
    };
  }

  #down(hit, e) {
    const s = this.sel;
    if (this.editor.ed) {
      if (hit.zone === 'cell' && this.editor.pointable()) {
        e.preventDefault();
        this.drag = { kind: 'point', r: hit.r, c: hit.c };
        this.editor.point(hit.r, hit.c);
        return;
      }
      const ids = this.#hitIds(hit);
      if (!this.#settle()) { e.preventDefault(); return; }
      hit = this.#hitAfter(hit, ids);
      if (!hit) { e.preventDefault(); this.changed(); return; }
    }
    e.preventDefault();
    this.focusGrid();
    this.drag = null;
    switch (hit.zone) {
      case 'cell':
        if (e.shiftKey) s.extend(hit.r, hit.c); else s.set(hit.r, hit.c);
        this.drag = { kind: 'cells' };
        break;
      case 'row':
        if (e.shiftKey) { s.extend(hit.r, s.far().c); s.kind = 'rows'; } else s.rows(hit.r, hit.r);
        this.drag = { kind: 'rows' };
        break;
      case 'col':
        if (e.shiftKey) { s.extend(s.far().r, hit.c); s.kind = 'cols'; } else s.cols(hit.c, hit.c);
        this.drag = { kind: 'cols' };
        break;
      case 'corner': s.all(); break;
      // A resize and a fill name what they act on by ids when they start,
      // and are sent with the base of the screen they were prepared on
      // (TECH 19.5).
      case 'resize':
        if (this.canWrite(true)) {
          const col = this.model.colAt(hit.c);
          this.drag = { kind: 'resize', tab: this.model.tab.id, col, base: this.model.seq, x: e.clientX, from: this.model.widthOf(col), width: null };
        }
        break;
      case 'handle':
        if (this.canWrite(true)) this.drag = { kind: 'fill', source: this.#target(), target: null };
        break;
    }
    this.changed();
  }

  #dragTo(hit, e) {
    const d = this.drag;
    const s = this.sel;
    if (!d) return;
    const ids = d.kind !== 'point' && d.kind !== 'resize' && this.editor.ed ? this.#hitIds(hit) : null;
    // A reference drag ends with its edit, when the editor turns read-only
    // for the last exchange of a rebase, whose text must stay as it is, and
    // when an exchange replaced the reference it inserted, because its
    // start is a place on the screen before. A key typed during another drag opens an
    // edit of the active cell, which settles before the selection moves;
    // one that stays ends the drag.
    const editor = this.editor;
    if (d.kind === 'point' ? !editor.open || editor.area.readOnly || !editor.ed.pointed : d.kind !== 'resize' && !this.#settle()) {
      this.drag = null;
      this.grid.overlays.fill = null;
      this.changed();
      return;
    }
    if (ids) {
      hit = this.#hitAfter(hit, ids);
      if (!hit) { this.changed(); return; }
    }
    switch (d.kind) {
      case 'point': this.editor.point(d.r, d.c, hit.r, hit.c); return;
      case 'cells': s.extend(hit.r, hit.c); break;
      case 'rows': s.extend(hit.r, s.far().c); break;
      case 'cols': s.extend(s.far().r, hit.c); break;
      case 'fill': {
        // The target lies beyond the source the drag started from.
        const src = this.#box(d.source);
        if (!src) { this.#dragCheck(); return; }
        const b = dragTarget(src, hit.r, hit.c);
        const m = this.model;
        d.target = b && { rows: m.tab.rows.slice(b.r1, b.r2 + 1), cols: m.tab.cols.slice(b.c1, b.c2 + 1), base: m.seq };
        d.hit = { ...this.#hitIds(hit), r: hit.r, c: hit.c };
        this.grid.overlays.fill = b;
        break;
      }
      case 'resize': this.resizeTo(e.clientX); return;
    }
    this.changed();
  }

  // #hitIds names the row and the column a pointer hit lands on by id, so
  // the hit can be found again after settling an edit applies the changes
  // the edit held back and moves them.
  #hitIds(hit) {
    const m = this.model;
    const row = hit.zone === 'cell' || hit.zone === 'row' ? m.rowAt(hit.r) : undefined;
    const col = hit.zone === 'cell' || hit.zone === 'col' || hit.zone === 'resize' ? m.colAt(hit.c) : undefined;
    return { row, col };
  }

  // #hitAfter is the hit at the positions its row and column have now, or
  // null when a change deleted one of them.
  #hitAfter(hit, { row, col }) {
    const r = row === undefined ? hit.r : this.model.rowIndex(row);
    const c = col === undefined ? hit.c : this.model.colIndex(col);
    return r < 0 || c < 0 ? null : { ...hit, r, c };
  }

  // #box is the range that the rows and columns of a target cover on the
  // open tab now, or null when its tab is not open or one of them is gone
  // or they no longer lie side by side.
  #box(target) {
    const m = this.model;
    if (!m.tab || m.tab.id !== target.tab || !m.tabInfo(target.tab)) return null;
    const span = (ids, at) => {
      let lo = Infinity;
      let hi = -1;
      for (const id of ids) {
        const i = at(id);
        if (i < 0) return null;
        lo = Math.min(lo, i);
        hi = Math.max(hi, i);
      }
      return hi - lo + 1 === ids.length ? [lo, hi] : null;
    };
    const r = span(target.rows, (id) => m.rowIndex(id));
    const c = r && span(target.cols, (id) => m.colIndex(id));
    return c && { r1: r[0], r2: r[1], c1: c[0], c2: c[1] };
  }

  // #dragCheck ends a resize or a fill whose column, source or tab a change
  // has taken away, and says so. Every structural delta that reaches the
  // grid and every new snapshot runs it, so a source deleted and brought
  // back between two pointer moves ends the drag too. A snapshot ends it
  // always: it cannot tell such a source from one that never left.
  #dragCheck(snapshot = false) {
    const d = this.drag;
    let why;
    if (d?.kind === 'resize') {
      if (!snapshot && this.model.tab?.id === d.tab && this.model.tabInfo(d.tab) && this.model.colIndex(d.col) >= 0) return;
      this.model.dropPending('drag');
      if (this.model.tab && !this.view) this.grid.widthsChanged();
      why = 'its column was deleted or its tab closed';
    } else if (d?.kind === 'fill') {
      if (!snapshot && this.#box(d.source)) return;
      this.grid.overlays.fill = null;
      why = 'the cells it started from were deleted or sorted apart, or their tab closed';
    } else return;
    if (snapshot) why = 'the tab was loaded again';
    d.lost = true;
    this.drag = null;
    this.notices.show({ kind: 'bad', text: `The ${d.kind === 'fill' ? 'fill' : 'resize'} ended because ${why}. Nothing was changed.` });
    this.changed();
  }

  // resizeTo previews a column width while its edge is dragged; one `width`
  // goes out on release.
  resizeTo(clientX) {
    const d = this.drag;
    if (d?.kind !== 'resize') return;
    d.width = Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, Math.round(d.from + clientX - d.x)));
    this.model.addPendingWidth('drag', [d.col], d.width);
    this.grid.widthsChanged();
    this.changed();
  }

  #up() {
    const d = this.drag;
    if (!d) return;
    if (d.kind === 'fill') {
      this.grid.overlays.fill = null;
      // The drag stays set while the edit settles: a held delta the settle
      // applies can delete the source, which ends it (#dragCheck).
      if (d.target && this.#settle() && !d.lost) this.#fill(d);
    } else if (d.kind === 'resize') {
      // The preview goes whether or not the width can be sent.
      this.model.dropPending('drag');
      if (d.width != null && d.width !== d.from) this.#width([d.col], d.width, false, d.base);
      this.grid.widthsChanged();
    }
    this.drag = null;
    this.changed();
  }

  // #fill sends a fill drag with mode auto (rule 21a): its source and the
  // target the pointer last chose, by ids, with the base of the screen the
  // target was chosen on. A target cell the settle deleted is not filled.
  #fill(d) {
    const src = this.#box(d.source);
    const at = d.hit && this.#hitAfter(d.hit, d.hit);
    if (!src || !at) {
      this.notices.show({ kind: 'bad', text: 'The cells this fill was aimed at were deleted. Nothing was changed.' });
      return;
    }
    const t = d.target;
    const m = this.model;
    const fill = { tab: m.tab.id, source: { rows: packIds(d.source.rows), cols: packIds(d.source.cols) }, target: { rows: packIds(t.rows), cols: packIds(t.cols) } };
    // A fill the page cannot send leaves the selection as it was.
    const a = this.#act([ops.fill(fill.tab, fill.source, fill.target, 'auto')], { label: 'fill', base: t.base, fillDrag: true });
    if (!a) return;
    // The selection covers the source and the target cells that are left.
    const u = { ...src };
    for (const id of t.rows) { const r = m.rowIndex(id); if (r >= 0) { u.r1 = Math.min(u.r1, r); u.r2 = Math.max(u.r2, r); } }
    for (const id of t.cols) { const c = m.colIndex(id); if (c >= 0) { u.c1 = Math.min(u.c1, c); u.c2 = Math.max(u.c2, c); } }
    this.selection.range(u.r1, u.c1, u.r2, u.c2);
    this.fillChoice.expect(a.n, fill, { moved: this.selection.moved, top: this.grid.el.scrollTop, left: this.grid.el.scrollLeft });
  }

  // #fillMenu offers the fill of the button again by the same ids, as a
  // copy or as a series. Each is one more fill with the sequence of the
  // screen on show, so undo takes it back on its own. The fill, as the
  // next edit, closes the button; a choice the sheet cannot take now, as
  // while it calculates, says why and leaves the button.
  #fillMenu(btn) {
    const r = btn.getBoundingClientRect();
    const again = (mode, label) => () => {
      const f = this.fillChoice.open ? this.fillChoice.fill : null;
      if (f && this.model.tab?.id === f.tab) this.#act([ops.fill(f.tab, f.source, f.target, mode)], { label });
    };
    this.popups.menu('Fill options', [
      { label: 'Copy cells', fn: again('copy', 'fill as a copy') },
      { label: 'Fill series', fn: again('series', 'fill as a series') },
    ], { x: r.left, y: r.bottom + 2 });
  }

  // #fillCheck closes the button of a fill drag once an edit starts, the
  // selection moves, the grid scrolls, or another tab or a revision is on
  // screen, and keeps it by the corner of the filled cells. A choice made
  // while the sheet cannot be edited says why, as any edit does.
  #fillCheck() {
    const fc = this.fillChoice;
    const f = fc.fill;
    if (!f) return;
    if (this.editor.ed || this.view || this.model.tab?.id !== f.tab) { fc.close(); return; }
    fc.check(this.selection, this.grid.el.scrollTop, this.grid.el.scrollLeft);
    if (!fc.open) return;
    const b = this.selection.box();
    const cell = this.grid.cellRect(b.r2, b.c2);
    const g = this.grid.el.getBoundingClientRect();
    const host = this.root.getBoundingClientRect();
    const within = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
    fc.place(within(cell.right + 2, g.left, g.right - 32) - host.left, within(cell.bottom + 2, g.top, g.bottom - 32) - host.top);
  }

  // #selectForMenu selects what a right click names when the selection
  // does not hold it yet. It returns false when an open edit keeps the
  // selection where it is, and then no menu opens.
  #selectForMenu(hit) {
    const s = this.selection;
    const m = this.model;
    const b = s.box();
    // Settling the edit can apply changes it held back and move the line or
    // cell clicked, so it is selected by id after the edit settles.
    const row = hit.zone === 'col' ? null : m.rowAt(hit.r);
    const col = hit.zone === 'row' ? null : m.colAt(hit.c);
    const r = () => m.rowIndex(row);
    const c = () => m.colIndex(col);
    let select = null;
    if (hit.zone === 'col') { if (!(s.kind === 'cols' && hit.c >= b.c1 && hit.c <= b.c2)) select = () => s.cols(c(), c()); }
    else if (hit.zone === 'row') { if (!(s.kind === 'rows' && hit.r >= b.r1 && hit.r <= b.r2)) select = () => s.rows(r(), r()); }
    else if (hit.zone === 'cell' && !s.contains(hit.r, hit.c)) select = () => s.set(r(), c());
    if (select && !this.#settle()) {
      // The right button took focus from the edit.
      this.editor.area.focus({ preventScroll: true });
      return false;
    }
    // A line or cell that the changes deleted gets no menu.
    if (select && ((row != null && r() < 0) || (col != null && c() < 0))) { this.changed(); return false; }
    select?.();
    this.changed();
    return true;
  }

  // ---- formula text on demand ----

  // #peek asks for the text of a cell of the live tab. The server answers
  // with the sequence it read the text at, and that answer can arrive
  // before or after deltas of other sequences. The request therefore
  // remembers the newest change to its cell's text that reached the screen
  // while it was out (TECH 12.4).
  #peek(row, col) {
    const id = this.net.requestId();
    const tab = this.model.tab.id;
    this.peekIds.set(id, { tab, row, col, changed: null, answer: null });
    if (!this.net.peek(id, tab, row, col)) this.peekIds.delete(id);
  }

  #peekAsked(row, col) {
    for (const p of this.peekIds.values()) if (p.row === row && p.col === col) return true;
    return false;
  }

  #peeked(m) {
    // An answer that does not fit is not kept, where its sequence would be
    // compared with every later delta, and the text is asked for again. It
    // is read before it is matched. An answer whose id does not fit could
    // be the one of any request out, so each of them is asked again rather
    // than left waiting for an answer that has come.
    const wrong = peekedError(m);
    const p = this.peekIds.get(m.id);
    if (wrong) {
      const lost = p ? [[m.id, p]] : isId(m.id) ? [] : [...this.peekIds];
      for (const [id, q] of lost) {
        this.peekIds.delete(id);
        this.#peekAgain(q.row, q.col);
      }
      throw new TypeError(wrong);
    }
    if (!p) return;
    p.answer = m;
    this.#settlePeeks();
  }

  // #settlePeeks installs the answers that match the screen. An answer
  // read at a later sequence than the screen waits until the screen gets
  // there. An answer read before a change to its cell's text, or to the
  // structure or the name of a tab, is text of a state the screen has
  // left: it is dropped and asked for again. An answer for another tab
  // than the open one is dropped.
  //
  // An editor that waits for its formula holds the screen while it holds
  // deltas back, and an answer read after them is text of a screen the
  // editor is not bound to. The keys typed meanwhile were read against the
  // screen on show and cannot follow that text, so the editor closes and
  // offers them back. With no keys typed it binds nothing, and the held
  // deltas apply up to the answer, which then opens the editor.
  #settlePeeks() {
    const model = this.model;
    let ahead = null;
    for (const [id, p] of this.peekIds) {
      if (!model.tab || model.tab.id !== p.tab) { this.peekIds.delete(id); continue; }
      if (!p.answer) continue;
      if (seqCmp(p.answer.seq, model.seq) > 0) {
        if (this.editor.waitsForPeek(p.row, p.col)) ahead = p.answer.seq;
        continue;
      }
      this.peekIds.delete(id);
      if (p.changed != null && seqCmp(p.changed, p.answer.seq) > 0) { this.#peekAgain(p.row, p.col); continue; }
      model.setPeek(p.row, p.col, { edit: p.answer.edit, formula: p.answer.formula });
      if (this.editor.peekArrived(p.row, p.col)) this.rebaser.opened();
      this.changed();
    }
    const ed = this.editor.ed;
    if (ahead == null || !this.rebaser.holding || !ed || !this.editor.waitsForPeek(ed.row, ed.col)) return;
    if (this.editor.untyped) this.rebaser.releaseThrough(ahead);
    else this.rebaser.abort('ahead');
  }

  // #peekAgain asks once more, after the same pause as a first request,
  // when the text is still needed: an editor waits for it, or its cell is
  // the active one.
  #peekAgain(row, col) {
    this.timers.setTimeout(() => {
      const m = this.model;
      if (!m.tab || m.peeked(row, col) || this.#peekAsked(row, col) || m.rowIndex(row) < 0 || m.colIndex(col) < 0) return;
      const a = this.selection.active();
      const active = !this.view && m.rowAt(a.r) === row && m.colAt(a.c) === col;
      if (this.editor.waitsForPeek(row, col) || active) this.#peek(row, col);
    }, PEEK_DELAY);
  }

  // ---- History ----

  toggleHistory() {
    if (this.net.access !== 'private') return;
    if (this.editor.open) this.editor.commit();
    this.history.toggle();
    this.changed();
    if (!this.history.open) this.focusGrid();
  }

  // #viewRevision puts a tab of a revision on screen, or returns to the
  // live sheet as the connection kept it meanwhile.
  #viewRevision(v) {
    if (v) {
      // An edit still open on the live tab, one that waits for its formula
      // or could not be sent yet, cannot stay over a revision's grid.
      if (this.editor.ed) this.#offerBack(this.editor.close(), 'revision');
      this.#remember();
      v.selection = new Selection(v.model);
      v.selection.set(0, 0);
      this.view = v;
      this.grid.overlays.flash = null;
      this.grid.show(v.model, v.selection);
      this.grid.el.scrollTop = 0;
      this.grid.el.scrollLeft = 0;
    } else {
      this.view = null;
      this.grid.show(this.model, this.selection);
      const at = this.model.tab && this.memory.get(this.model.tab.id);
      this.grid.el.scrollTop = at?.top || 0;
      this.grid.el.scrollLeft = at?.left || 0;
    }
    this.find.search();
    this.changed();
  }

  // ---- identity (rule 26) ----

  #nameDialog() {
    this.nameAsked = true;
    const input = h('input', { type: 'text', id: 'me-name', maxlength: '80', autocomplete: 'nickname', 'aria-describedby': 'me-why' });
    input.value = this.net.name || '';
    const err = h('div', { class: 'err', role: 'alert' });
    const done = () => {
      const name = input.value.trim();
      if (!name) { err.textContent = 'Enter a name of 1 to 80 characters.'; input.focus(); return; }
      try { localStorage.setItem(NAME_KEY, name); } catch { /* private mode: the name lasts for this tab */ }
      this.nameSent = true;
      this.popups.close();
      this.notices.show({ text: `Others see you as “${name}”, marked Unverified: this route does not verify who you are.` });
      this.net.rename(name);
    };
    const el = this.popups.dialog('Your name on this sheet', [
      h('h2', { text: 'Your name on this sheet' }),
      h('p', { id: 'me-why', text: 'This route has no verified identity. Enter the name others will see next to your cell and your edits. It is shown as Unverified. You can read the sheet without a name; editing starts once you have one.' }),
      h('label', { for: 'me-name' }, 'Name', input), err,
      h('div', { class: 'rowb' }, button('Read only', () => this.popups.close(), { class: 'btn' }), button('Continue', done, { class: 'btn primary' })),
    ], null, { modal: true, focus: input });
    el.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target === input && !e.isComposing) { e.preventDefault(); done(); } });
  }

  #keysDialog(opener) {
    const r = opener.getBoundingClientRect();
    const rows = [
      ['F6, Shift+F6', 'Next and previous region: top bar, toolbar, formula bar, grid, tabs'],
      ['Escape', 'Back to the grid'],
      ['Arrows, Tab, Shift+Tab', 'Move between cells; with Shift extend; with Ctrl or Cmd jump to the edge of the data'],
      ['Enter, F2', 'Edit the cell; Enter commits and moves down, Tab commits and moves right'],
      ['Alt or Option+Enter', 'A line break inside a cell'],
      ['Shift+F10', 'Rows and columns menu'],
      ['Ctrl or Cmd+F', 'Find in this tab'],
      ['Ctrl or Cmd+Z, Shift+Ctrl or Cmd+Z', 'Undo and redo'],
      ['Ctrl+D, Ctrl+R', 'Fill down and right'],
      ['Ctrl or Cmd+Shift+V', 'Paste values only'],
    ];
    this.popups.dialog('Keyboard paths', [
      h('h2', { text: 'Keyboard paths' }),
      h('dl', {}, rows.map(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })])),
      h('div', { class: 'rowb' }, button('Close', () => this.popups.close(), { class: 'btn' })),
    ], { x: r.left, y: r.bottom + 4 }, { class: 'keys' });
  }

  // ---- drawing ----

  // changed draws the screen once per frame, however many things changed.
  changed() {
    if (this.rendering) return;
    this.rendering = true;
    this.raf(() => { this.rendering = false; this.#render(); });
  }

  #render() {
    const m = this.cur;
    const o = this.grid.overlays;
    o.people = this.view ? [] : this.presence.outlines(this.model);
    o.handle = !this.readOnly && !this.editor.ed && !this.theme.phone;
    this.grid.paint();
    this.find.sync();
    if (this.editor.ed) this.editor.placeHint();
    this.#fillCheck();
    this.#chrome();
    // A stale grid holds ids of a state the host has left; after a rollback
    // they can be ids the host never gave out. Nothing is said from it.
    if (m.tab && !this.view && this.net.writable && !this.needName && !this.model.stale) {
      const p = presenceMessage(this.model, this.selection, this.editor.open);
      const sig = JSON.stringify(p);
      if (p && sig !== this.lastPresence) { this.lastPresence = sig; this.net.presence(p); }
    }
  }

  #chrome() {
    const m = this.cur;
    const pub = this.net.access === 'public';
    this.root.classList.toggle('with-hist', this.history.open);
    this.root.classList.toggle('ro', this.readOnly);
    this.badge.hidden = !pub;
    this.status.el.hidden = pub;
    this.toolbar.el.hidden = pub;
    this.histBtn.hidden = this.net.access !== 'private';
    this.histBtn.setAttribute('aria-pressed', String(this.history.open));
    this.csvBtn.disabled = !this.model.tab || !!this.view;
    const faces = JSON.stringify([this.net.access, this.net.me, this.presence.people.map((p) => [p.label, p.color, p.verified])]);
    if (faces !== this.facesSig) {
      this.facesSig = faces;
      replace(this.facesEl, this.net.access === 'private' ? this.presence.faces(this.net.me, (e) => this.#people(e.currentTarget)) : null);
    }
    this.#status();
    const t = m.tab;
    const f = t ? this.#activeFormat() : {};
    this.toolbar.render({ format: f, disabled: this.readOnly || !t, unknown: !!t && !this.#activeKnown(), phone: this.theme.phone, frozenRows: t?.frozenRows || 0, frozenCols: t?.frozenCols || 0 });
    this.tabs.render({
      tabs: this.view ? this.view.tabs : this.model.tabs,
      open: this.view ? this.view.tab : (t?.id ?? this.tabWanted),
      readOnly: this.readOnly,
      people: this.view ? new Set() : this.presence.tabs(),
    });
    this.#bar();
    // A sum of cells that have not all arrived is no sum of the selection.
    this.status.totals(t && !this.sel.single && m.knownBox(this.sel.box(), this.sel.kind !== 'cells') ? aggregate(m, this.sel.box()) : null);
  }

  // #bar shows the active cell's address and content in the formula bar:
  // the formula text for a formula, once it has been fetched, and otherwise
  // the content as it would be typed (rule 18). An edit started in the bar
  // goes on from what it shows (barText).
  #bar() {
    const m = this.cur;
    if (!m.tab) { this.nameBox.textContent = ''; return; }
    const b = this.sel.box();
    const a = this.sel.active();
    this.nameBox.textContent = this.sel.single ? address(a.r, a.c) : rangeName(b);
    if (this.editor.ed) return;
    this.bar.readOnly = this.readOnly;
    const row = m.rowAt(a.r);
    const col = m.colAt(a.c);
    // A revision's formula text comes from that revision, never from the
    // live cell of the same ids (rule 51).
    const shown = barText(m, row, col);
    if (shown.peek) this.#peekSoon(row, col);
    this.editor.idle = shown;
    if (this.bar.value !== shown.text) this.bar.value = shown.text;
  }

  // #peekSoon asks for the formula text of the active cell once the
  // selection has settled on it: from the open revision while one is on
  // screen, from the live sheet otherwise.
  #peekSoon(row, col) {
    this.timers.clearTimeout(this.peekTimer);
    this.peekTimer = this.timers.setTimeout(() => {
      const m = this.cur;
      if (!m.tab || m.peeked(row, col)) return;
      const a = this.sel.active();
      if (m.rowAt(a.r) !== row || m.colAt(a.c) !== col) return;
      if (this.view) this.history.peek(row, col);
      else if (!this.#peekAsked(row, col)) this.#peek(row, col);
    }, PEEK_DELAY);
  }

  #people(opener) {
    const r = opener.getBoundingClientRect();
    this.popups.dialog('People here', this.presence.list(this.net.me, (id) => this.model.tabInfo(id)?.name || 'another tab'), { x: r.left - 200, y: r.bottom + 4 }, { class: 'people' });
  }

  #banners() {
    const net = this.net;
    const out = [];
    const n = net.pending.length;
    const edits = () => net.pending.map((a) => (a.meta.texts?.length ? `${a.meta.label}: ${a.meta.texts.join('\n')}` : a.meta.label)).join('\n');
    const list = () => h('ul', { class: 'oplist', 'aria-label': 'Unsaved edits' }, net.pending.slice(0, 20).map((a) => h('li', {}, h('code', { text: a.meta.label || 'edit' }))));
    const rev = this.history.banner();
    if (rev) out.push(rev);
    if (net.stopped && net.access === 'private') {
      const el = h('div', { class: 'banner bad', role: 'alert' },
        h('div', { class: 'grow' }, `Editing stopped. ${this.stoppedWhy} ${n ? `Your ${plural(n, 'unsaved edit')} ${n === 1 ? 'was' : 'were'} not confirmed by the host and may not be saved. Copy ${n === 1 ? 'it' : 'them'} before you reload:` : 'Reload to continue.'}`, n ? list() : null),
        n ? button('Copy edits', (e) => copyText(edits(), e.currentTarget), { class: 'btn' }) : null,
        this.reloadAsk
          ? [h('span', { text: n ? `Reloading discards ${n === 1 ? 'this edit' : `these ${n} edits`}.` : 'Reload now?' }),
            button('Reload and discard', () => location.reload(), { class: 'btn danger' }),
            button('Cancel', () => { this.reloadAsk = false; this.#banners(); }, { class: 'btn' })]
          : button('Reload', () => { this.reloadAsk = true; this.#banners(); }, { class: 'btn primary' }));
      out.push(el);
    } else if (!net.stopped && net.busyLong) {
      // An action the host keeps answering "unavailable" has no known
      // outcome. It is neither saved nor refused, and the page says so for
      // as long as that lasts, also while a connection is open.
      out.push(h('div', { class: 'banner warn', role: 'group', 'aria-label': 'Host busy' },
        h('div', { class: 'grow' }, `The host has been busy for a while, so your ${plural(n, 'unsaved edit')} ${n === 1 ? 'is' : 'are'} not saved yet. ${n === 1 ? 'It stays' : 'They stay'} on this screen, and this page keeps asking the host until it answers. Each applies once.`, list()),
        net.open ? null : button('Ask now', () => net.start(), { class: 'btn' })));
    } else if (!net.stopped && !net.open && net.failures > 0) {
      out.push(h('div', { class: 'banner warn', role: 'group', 'aria-label': net.busy ? 'Host busy' : 'Connection lost' },
        h('div', { class: 'grow' }, net.access == null ? 'Could not reach the sheet. Trying again…'
          : net.busy && n ? `The host is busy. Your ${plural(n, 'unsaved edit')} ${n === 1 ? 'stays' : 'stay'} on this screen and ${n === 1 ? 'is' : 'are'} sent again in a moment. Each applies once.`
          : n ? `Connection lost. Your ${plural(n, 'unsaved edit')} ${n === 1 ? 'stays' : 'stay'} on this screen and ${n === 1 ? 'is' : 'are'} sent again when the connection returns. Each applies once.` : 'Connection lost. Reconnecting…', n ? list() : null),
        button('Retry now', () => net.start(), { class: 'btn' })));
    }
    if (this.calculating) {
      out.push(h('div', { class: 'banner', role: 'group', 'aria-label': 'Calculating' },
        h('span', { class: 'grow', text: 'The sheet is calculating. No value is shown partly calculated; the new values appear together. You can scroll, select, switch tabs and copy. Editing returns when it ends.' })));
    }
    if (this.#askName && !this.popups.open) {
      out.push(h('div', { class: 'banner', role: 'group', 'aria-label': 'Name' },
        h('span', { class: 'grow', text: 'You are reading this sheet. Choose a name to edit it.' }),
        button('Choose a name', () => this.#nameDialog(), { class: 'btn primary' })));
    }
    const sig = JSON.stringify([rev ? this.history.view.revision : 0, net.stopped?.code, net.open, net.failures > 0, n, this.calculating, this.#askName && !this.popups.open, !!this.reloadAsk, net.access, net.busy, net.busyLong]);
    if (sig === this.bannerSig) return;
    this.bannerSig = sig;
    replace(this.banners, out);
  }
}

if (!globalThis.sheetTest) {
  const start = (functions) => { globalThis.sheet = new App({ functions }); };
  fetch(new URL('./functions.json', import.meta.url)).then((r) => r.json()).then(start, () => start([]));
}
