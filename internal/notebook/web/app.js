// The notebook page editor: shell, routing, connection status, typing
// flushes, notices and the wiring between the modules (TECH 9, 13, 14, 18).
'use strict';


import { $, h, icon, replace, toast, plural } from './dom.js';
import { Store, ref, isText, groupVersion, PARENT_KINDS, TEXT_KINDS, withDefaults } from './state.js';
import { Net } from './net.js';
import { PageView } from './blocks.js';
import { Editor, fieldRuns } from './editor.js';
import { ModelSelection, readCaret, readEndpoint, placeCaret, selectionText } from './selection.js';
import { Sidebar, resolveRoute, breadcrumb, routeOf } from './sidebar.js';
import { SlashMenu, actionMenu, kindLabel } from './menu.js';
import { Presence, activity } from './presence.js';
import { undoOp, undoMessage } from './undo.js';
import * as paste from './paste.js';
import { announce, nearestSurvivor } from './a11y.js';
import { Theme } from './theme.js';
import { History } from './history.js';
import * as act from './actions.js';
import { deleteRange, textLength, plainText } from './richtext.js';
import { renderDatabase } from './table.js';

export const FLUSH_MS = 200;   // typing reaches the server within 200 ms
export const RUN_IDLE_MS = 750; // a typing run closes after 750 ms idle
export const NOTICES_MAX = 8;
const RETAINED_MAX = 64;

const STOPPED = {
  session_expired: 'The server no longer knows this tab, so your unsaved edits cannot be sent.',
  expired: 'The server no longer has the receipt of an earlier edit, so your unsaved edits cannot be sent safely.',
  conflict: 'An unsaved edit conflicts with what the server recorded, so editing stopped.',
  too_large: 'An edit is too large to send.',
  quota: 'The notebook is over its disk quota, so the server cannot accept your edits.',
};

const pageTitleOf = (view, id) => { const p = view.get(id); return p ? p.title || 'Untitled' : 'another page'; };

export class App {
  constructor({ connect, fetch: fetchImpl = globalThis.fetch && globalThis.fetch.bind(globalThis), timers = globalThis, now } = {}) {
    this.timers = timers;
    this.fetch = fetchImpl;
    this.store = new Store();
    this.presence = new Presence();
    this.selection = new ModelSelection();
    this.notices = [];        // overwritten or refused text this tab remembers
    this.retained = [];       // recently acknowledged own content writes
    this.open = new Map();    // toggle id -> personal open state
    this.page = null;         // shown page id, or null
    this.missing = null;      // unknown page id from the address
    this.run = null;          // {key, action_id, idle}
    this.flushTimer = null;
    this.focusAfter = null;
    this.confirmDelete = false;
    this.peopleOpen = false;
    this.lastOrder = [];
    this.databases = new Map(); // block id -> {el, deps, handle} of mounted database views
    this.viewNotices = new Map(); // view id -> removal notice text (rule 10a)
    this.noticeVersion = 0;
    this.#layout();
    this.theme = new Theme(() => this.#layoutClass());
    this.net = new Net(this.store, {
      welcome: (m) => this.#welcome(m),
      changed: (d) => this.#changed(d),
      confirmCreation: (ref) => { this.pageView.confirmCreation(ref); this.editor.confirmCreation(ref); },
      ack: (a, m) => this.#ack(a, m),
      reject: (a, m) => this.#reject(a, m),
      stopped: () => this.render(),
      presence: (m) => { this.presence.update(m); this.render(); },
      activity: (m) => activity(m, this.pageEl),
      overwrote: (list) => this.#overwrote(list),
      viewAdjustment: (m) => this.#viewAdjustment(m),
      status: () => this.#status(),
    }, { ...(connect ? { connect } : {}), timers, ...(now ? { now } : {}) });
    this.pageView = new PageView({
      writable: () => this.writable(),
      view: () => this.view(),
      composing: () => this.store.composing,
      presenceFor: (id) => this.presence.editing(id),
      assetExt: (hash) => this.store.canonical.assets.get(hash),
      renderDatabase: (block, el) => this.#database(block, el),
      toggleOpen: (id) => this.#toggle(id),
      isOpen: (b) => (this.open.has(b.ref.id) ? this.open.get(b.ref.id) : !!b.content.open_default),
      selected: (id) => this.#selected().has(id),
      onCheck: (b, checked) => b && this.queue([act.setBlock(b, { ...b.content, checked })]),
      onLanguage: (b, language) => b && this.queue([act.setBlock(b, { ...b.content, language })]),
      handle: (b, el) => this.#handle(b, el),
      caret: { read: readCaret, place: placeCaret },
    });
    this.editor = new Editor(this);
    this.editor.attach(this.pageEl);
    this.slashMenu = new SlashMenu((key, block, range) => this.#pickSlash(key, block, range));
    this.history = new History(this.historyEl, {
      fetch: this.fetch,
      session: () => this.net.open && this.writable() ? this.net.session : null,
      currentPages: () => new Set([...this.view().map.values()].filter((item) => item.kind === 'page').map((item) => item.ref.id)),
      onRecovered: ({ root }) => { if (root) this.go(root.id); this.render(); },
      onClose: () => { this.historyBtn.setAttribute('aria-expanded', 'false'); this.render(); },
    });
    this.sidebar = new Sidebar({
      view: () => this.view(),
      current: () => this.page,
      go: (id) => this.go(id),
      writable: () => this.writable(),
      presenceOn: (id) => this.presence.onPage(id),
    }, this.treeHost);
    this.#events();
    this.#route();
    this.net.start();
    this.render();
  }

  // ---- state ----

  view() { return this.store.view(); }
  writable() { return this.net.access === 'private' && !this.net.stopped; }
  slashOpen() { return this.slashMenu.open; }
  menuKey(e) { return this.slashMenu.key(e); }
  pickSlash() { this.slashMenu.pick(); }
  closeSlash() { this.slashMenu.close(); }
  editableOf(id, field = 'text') { return this.pageView.editableOf(id, field); }
  announce(text) { announce(text); }

  // ---- layout ----

  #layout() {
    const root = $('#app');
    this.toggleSide = h('button', { type: 'button', class: 'btn icon menu-btn', 'aria-label': 'Pages', 'aria-expanded': 'false', 'aria-controls': 'side' }, icon('menu'));
    this.crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Breadcrumb' });
    this.statusEl = h('span', { class: 'status', role: 'status' });
    this.peopleEl = h('div', { class: 'people-host' });
    this.historyBtn = h('button', { type: 'button', class: 'btn priv-only', 'aria-expanded': 'false', 'aria-controls': 'history' }, icon('history'), h('span', { class: 'lbl' }, 'History'));
    this.treeHost = h('div', { class: 'tree-host' });
    this.newPageBtn = h('button', { type: 'button', class: 'side-btn priv-only' }, icon('plus'), 'New page');
    this.side = h('nav', { id: 'side', class: 'side', 'aria-label': 'Notebook' },
      h('div', { class: 'side-head' }, h('span', { class: 'nb' }, 'Notebook')),
      h('div', { class: 'side-label', id: 'pages-label' }, 'Pages'),
      this.treeHost,
      h('div', { class: 'side-foot' }, this.newPageBtn));
    this.banners = h('div', { class: 'banners' });
    this.titleEl = h('h1', { class: 'ptitle', 'aria-label': 'Page title', 'data-ph': 'Untitled' });
    this.subpages = h('div', { class: 'subs' });
    this.pageEl = h('div', { class: 'blocks', tabindex: '-1', 'aria-label': 'Page content' });
    this.article = h('article', { class: 'page' }, this.banners, this.titleEl, this.subpages, this.pageEl);
    this.scroll = h('div', { class: 'scroll' }, this.article);
    this.main = h('main', { class: 'main' },
      h('div', { class: 'top' }, this.toggleSide, this.crumbs, this.statusEl, this.peopleEl, this.historyBtn),
      this.scroll);
    this.historyEl = h('aside', { id: 'history', class: 'side hist', 'aria-label': 'History' });
    this.historyEl.hidden = true;
    replace(root, this.side, this.main, this.historyEl);
  }

  #layoutClass() {
    const root = $('#app');
    if (root) root.classList.toggle('phone', this.theme ? this.theme.phone : false);
  }

  #events() {
    addEventListener('hashchange', () => this.#route());
    addEventListener('beforeunload', (e) => {
      if (!this.store.pending.size) return;
      e.preventDefault();
      e.returnValue = '';
    });
    document.addEventListener('click', (e) => {
      const a = e.target.closest && e.target.closest('a[href^="#/"]');
      if (!a || e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      this.go(a.getAttribute('href').slice(2));
    });
    document.addEventListener('copy', (e) => this.#copy(e));
    document.addEventListener('focusin', (e) => this.#focusin(e));
    document.addEventListener('focusout', (e) => {
      if (e.target && e.target.closest && e.target.closest('[data-field]')) this.closeRun();
    });
    document.addEventListener('mouseup', () => this.#pointerSelection());
    this.toggleSide.addEventListener('click', () => this.#side(!this.side.classList.contains('open')));
    this.historyBtn.addEventListener('click', () => { this.history.toggle(); this.historyBtn.setAttribute('aria-expanded', String(this.history.open)); });
    this.newPageBtn.addEventListener('click', () => this.newPage(null));
    this.pageEl.addEventListener('keydown', (e) => this.#selectionKey(e));
    this.titleEl.addEventListener('input', (e) => { if (!e.isComposing) this.#titleInput(); });
    this.titleEl.addEventListener('compositionend', () => this.#titleInput());
    this.titleEl.addEventListener('keydown', (e) => this.#titleKey(e));
    this.titleEl.addEventListener('paste', (e) => {
      e.preventDefault();
      const text = (e.clipboardData && e.clipboardData.getData('text/plain')) || '';
      const s = getSelection();
      if (s && s.rangeCount) { s.deleteFromDocument(); s.getRangeAt(0).insertNode(document.createTextNode(text.replace(/\s*\n\s*/g, ' '))); s.collapseToEnd(); }
      this.#titleInput();
    });
  }

  #side(open) {
    this.side.classList.toggle('open', open);
    this.toggleSide.setAttribute('aria-expanded', String(open));
    if (open) this.side.querySelector('[role="treeitem"][tabindex="0"]')?.focus();
  }

  // ---- routing ----

  #route() {
    const r = resolveRoute(this.view(), location.hash);
    this.missing = null;
    if (r.page) {
      if (r.redirect && history.replaceState) history.replaceState(null, '', routeOf(r.page));
      this.#show(r.page);
      if (r.block) this.focusAfter = { block: r.block, offset: 0 };
    } else if (r.missing) {
      // The page may simply not be loaded yet; the tree decides once it is.
      this.missing = this.store.canonical.hasScope({ kind: 'tree' }) ? r.missing : null;
      this.#show(this.missing ? null : r.missing);
    }
    this.render();
  }

  go(id) {
    this.#side(false);
    if (location.hash !== routeOf(id)) location.hash = routeOf(id);
    else this.#route();
  }

  #show(id) {
    if (this.page === id) return;
    this.closeRun();
    this.flush();
    this.closeSlash();
    this.selection.clear();
    this.confirmDelete = false;
    this.page = id;
    if (id) {
      this.sidebar.reveal(this.view(), id);
      this.watchedDbs = this.#databases().join(',');
      this.net.watch(id, this.#databases());
      this.net.presence({ page: id, edit: null });
    }
  }

  #databases() {
    if (!this.page) return [];
    return this.view().order(this.page).filter((b) => b.content.kind === 'database').map((b) => b.content.database);
  }

  // ---- connection ----

  #welcome(m) {
    document.body.classList.toggle('public', m.access !== 'private');
    if (this.page) this.net.presence({ page: this.page, edit: null });
    this.render();
  }

  #changed(d) {
    if (d && this.run && this.run.block) {
      // A remote content change to the block being typed closes the run.
      const remote = !(d.from && d.from.session_id === this.net.sessionId);
      if (remote && (d.changes || []).some((c) => (c.ref && c.ref.id === this.run.block) || (c.deleted && c.deleted.id === this.run.block))) this.closeRun();
    }
    if (d && d.reset) { this.open.clear(); this.selection.clear(); }
    const tree = this.store.canonical.hasScope({ kind: 'tree' });
    if (!this.store.composing) {
      if (!this.page || (this.missing == null && tree && !this.view().get(this.page))) this.#route();
      else if (tree && this.missing && this.view().get(this.missing)) this.#route();
    }
    const dbs = this.#databases().join(',');
    if (this.page && dbs !== this.watchedDbs) { this.watchedDbs = dbs; this.net.watch(this.page, this.#databases()); }
    this.render();
  }

  #status() {
    const s = this.net.status;
    const pending = this.store.pending.size;
    this.statusEl.dataset.state = s;
    const text = this.net.access === 'public' && s !== 'offline' ? 'Public · read only'
      : s === 'saved' ? 'Saved' : s === 'saving' ? 'Saving…' : s === 'stopped' ? 'Editing stopped'
        : pending ? `Disconnected · ${plural(pending, 'unsaved edit')}` : 'Disconnected';
    replace(this.statusEl, h('span', { class: 'dot', 'aria-hidden': 'true' }), text);
    this.#banners();
  }

  // ---- actions ----

  // queue sends ops as one action now. Typing held for the flush window is
  // released first so actions keep their order.
  queue(ops, { focus, close = true } = {}) {
    if (!this.writable() || !ops.length) return null;
    this.closeRun();
    this.#releaseDraft();
    const a = this.store.pending.push({ action_id: act.newActionId(), ops, close });
    if (focus) this.focusAfter = focus;
    this.render();
    this.net.flush();
    return a;
  }

  apply(plan) {
    if (!plan) return;
    if (plan.select) { this.selectBlocks(plan.select); return; }
    this.closeSlash();
    this.queue(plan.ops, { focus: plan.focus });
  }

  // typing records one content edit of a typing run. Edits within one flush
  // window coalesce into a single held action whose last write wins.
  typing(block, content, { baseV } = {}) {
    if (!this.writable()) return;
    const key = 'block:' + block.ref.id;
    this.#runFor(key, block.ref.id);
    const draft = this.store.pending.draft(key);
    const canonical = this.store.canonical.get(block.ref.id);
    const op = { op: 'set_block', block: ref(block), content, base_v: baseV || (draft ? draft.ops[0].base_v : groupVersion(canonical || block, 'content')) };
    if (draft) this.store.pending.replaceOps(draft, [op]);
    else this.store.pending.push({ action_id: this.run.action_id, typing_block: ref(block), ops: [op], held: true, draftKey: key });
    this.#scheduleFlush();
  }

  #titleInput() {
    const page = this.view().get(this.page);
    if (!page || !this.writable()) return;
    const title = this.titleEl.textContent.replace(/\n/g, ' ');
    const key = 'title:' + page.ref.id;
    this.#runFor(key, null);
    const draft = this.store.pending.draft(key);
    const op = { op: 'set_page_title', page: ref(page), title };
    if (draft) this.store.pending.replaceOps(draft, [op]);
    else this.store.pending.push({ action_id: this.run.action_id, ops: [op], held: true, draftKey: key });
    this.#scheduleFlush();
    this.sidebar.render(this.view());
    replace(this.crumbs, breadcrumb(this.view(), this.page));
  }

  #titleKey(e) {
    if (e.isComposing) return;
    if (e.key === 'Enter' || (e.key === 'ArrowDown' && !e.shiftKey)) {
      e.preventDefault();
      const first = this.view().order(this.page)[0];
      if (first) this.focusBlock(first.ref.id, 0);
      else if (e.key === 'Enter' && this.writable()) {
        const plan = act.createBlock(this.view(), null, withDefaults({ kind: 'paragraph', text: [] }), { page: this.view().get(this.page) });
        this.queue(plan.ops, { focus: { block: plan.id, offset: 0 } });
      }
    }
  }

  #runFor(key, block) {
    if (this.run && this.run.key !== key) this.closeRun();
    if (!this.run) this.run = { key, block, action_id: act.newActionId(), idle: null };
    this.timers.clearTimeout(this.run.idle);
    this.run.idle = this.timers.setTimeout(() => this.closeRun(), RUN_IDLE_MS);
  }

  // closeRun ends the typing run: a caret jump, blur, idle time, a remote
  // change to the block or any other action (TECH 10).
  closeRun() {
    if (!this.run) return;
    this.timers.clearTimeout(this.run.idle);
    const draft = this.store.pending.draft(this.run.key);
    this.run = null;
    if (draft) { this.store.pending.release(draft, true); this.net.flush(); }
  }

  #scheduleFlush() {
    if (this.flushTimer) return;
    this.flushTimer = this.timers.setTimeout(() => { this.flushTimer = null; this.flush(); }, FLUSH_MS);
  }

  // flush releases held typing so it is sent; composition holds it back.
  flush() {
    if (this.store.composing) { this.#scheduleFlush(); return; }
    this.#releaseDraft();
    this.net.flush();
    this.#status();
  }

  #releaseDraft() {
    const last = this.store.pending.actions.at(-1);
    if (last && last.held) this.store.pending.release(last, !this.run || this.run.key !== last.draftKey);
  }

  undo(redo) {
    if (!this.writable()) return;
    this.queue([undoOp(redo)]);
  }

  // ---- results ----

  #ack(a, m) {
    const status = new Map();
    for (const g of m.outcomes || []) for (const i of g.indexes) status.set(i, g);
    const lost = [];
    a.ops.forEach((op, i) => {
      const s = status.get(i);
      if (!s || s.status === 'applied' || s.status === 'adjusted') {
        if (op.op === 'set_block') this.#retain(a.n, op);
        return;
      }
      if (op.op === 'set_block' || op.op === 'create_block') lost.push({ op, s });
    });
    // A lost creation is kept next to the block it was placed after.
    const originalOf = (op) => (op.block ? op.block.id : op.place && (op.place.after || op.place.before) ? (op.place.after || op.place.before).id : null);
    if (lost.length) {
      const first = lost[0].s;
      const message = first.reason === 'kind' ? 'This block was changed into a kind your text cannot go into.'
        : first.status === 'ignored' ? 'The block you edited was deleted, so your edit was not applied.'
          : first.failed_index != null ? 'Your edit could not be applied whole, so none of it was applied.'
            : `Your edit was refused (${first.reason || first.status}).`;
      for (const { op } of lost) if (op.content && (isText(op.content.kind) || op.content.kind === 'code') && plainText(fieldRuns({ content: op.content }, 'text'))) this.notice(message, op.content, originalOf(op));
    }
    if (a.ops.length === 1 && (a.ops[0].op === 'undo' || a.ops[0].op === 'redo')) {
      const msg = undoMessage(m.undo, a.ops[0].op === 'redo');
      if (msg) toast(msg);
    }
    this.render();
  }

  #reject(a, m) {
    for (const op of a.ops) {
      if ((op.op === 'set_block' || op.op === 'create_block') && op.content && (isText(op.content.kind) || op.content.kind === 'code')) {
        this.notice(`Your edit was refused (${m.code}).`, op.content, op.block ? op.block.id : null);
      }
    }
    if (!this.notices.length) toast(`Your edit was refused (${m.code}).`, { error: true });
    this.render();
  }

  // #retain remembers this tab's acknowledged content writes, so an
  // overwrite notice can show the person's own text (TECH 14).
  #retain(n, op) {
    this.retained.push({ session_id: this.net.sessionId, n, block: op.block.id, content: op.content });
    while (this.retained.length > RETAINED_MAX) this.retained.shift();
  }

  // #overwrote handles delta entries naming a replaced content version of
  // one of this tab's receipts.
  #overwrote(list) {
    for (const o of list) {
      if (!o.source) continue;
      const r = this.retained.find((x) => x.session_id === o.source.session_id && x.n === o.source.n && x.block === o.block.id);
      if (!r) continue;
      const block = this.view().get(o.block.id);
      const who = o.by && o.by.label ? o.by.label : 'Someone';
      this.notice(`${who} changed this ${block ? kindLabel(block.content.kind).toLowerCase() : 'block'} after you.`, r.content, o.block.id);
    }
    this.render();
  }

  // notice keeps text the person would otherwise lose, with Keep my text.
  notice(message, content, original) {
    this.notices.push({ id: act.newUnitId(), message, content, original, page: this.page });
    while (this.notices.length > NOTICES_MAX) this.notices.shift();
    announce(message);
  }

  #keep(n) {
    const plan = act.keepMyText(this.view(), n.original, n.content, n.page);
    this.notices = this.notices.filter((x) => x !== n);
    if (plan) this.queue(plan.ops, { focus: { block: plan.id, offset: 0 } });
    toast('Your text was added as a new block. Nothing else changed.');
  }

  // ---- rendering ----

  render() {
    const view = this.view();
    const page = this.page && view.get(this.page);
    const focused = this.#focusedBlock();
    document.body.classList.toggle('editing', this.writable());
    this.sidebar.render(view);
    replace(this.crumbs, page ? breadcrumb(view, this.page) : []);
    this.peopleEl.replaceChildren();
    const faces = this.net.access === 'private' && this.presence.faces(() => { this.peopleOpen = !this.peopleOpen; this.render(); }, this.peopleOpen, (id) => pageTitleOf(view, id));
    if (faces) this.peopleEl.appendChild(faces);
    this.historyBtn.hidden = this.net.access !== 'private';
    this.newPageBtn.hidden = !this.writable();
    if (!page) {
      if (this.store.composing) { this.#status(); return; }
      this.pageView.page = null;
      this.pageEl.replaceChildren();
      this.subpages.replaceChildren();
      this.titleEl.removeAttribute('contenteditable');
      if (this.missing) {
        this.titleEl.textContent = 'This page does not exist';
        const first = view.firstPage();
        this.subpages.append(h('p', {}, first ? h('a', { class: 'ext', href: routeOf(first.ref.id) }, `Go to ${first.title || 'Untitled'}`) : null,
          this.net.access === 'private' ? [' or ', h('button', { type: 'button', class: 'linkish', onclick: () => this.history.toggle(true) }, 'look for it in History')] : null));
      } else {
        this.titleEl.textContent = 'Loading…';
      }
      this.#sweepDatabases();
      this.#status();
      return;
    }
    if (document.activeElement !== this.titleEl || this.titleEl.dataset.page !== page.ref.id) {
      this.titleEl.textContent = page.title;
      this.titleEl.dataset.page = page.ref.id;
    }
    if (this.writable()) this.titleEl.setAttribute('contenteditable', 'true'); else this.titleEl.removeAttribute('contenteditable');
    this.#subpages(view, page);
    if (!this.store.composing && !this.store.canonical.hasScope({ kind: 'page', id: page.ref.id }) && !view.order(page.ref.id).length) {
      this.pageEl.replaceChildren(h('p', { class: 'note loading' }, 'Loading…'));
      this.pageView.page = null;
    } else {
      this.pageView.render(view, page.ref.id, this.pageEl);
    }
    const order = view.order(page.ref.id).map((b) => b.ref.id);
    if (!this.store.composing && focused && !view.get(focused)) {
      const next = nearestSurvivor(this.lastOrder, focused, (id) => !!view.get(id));
      if (next) this.focusBlock(next, 0); else this.titleEl.focus();
      announce('The block you were editing was removed.');
    }
    if (!this.store.composing) this.lastOrder = order;
    if (!this.store.composing && this.focusAfter) {
      const f = this.focusAfter;
      this.focusAfter = null;
      if (view.get(f.block)) this.focusBlock(f.block, f.offset ?? Infinity, f.field);
    }
    this.#sweepDatabases();
    this.#status();
  }

  #subpages(view, page) {
    const kids = view.pages(page.ref.id);
    const items = [];
    if (kids.length) items.push(h('span', { class: 'note' }, icon('page'), `${plural(kids.length, 'subpage')}: `, kids.map((c, i) => [i ? ', ' : '', h('a', { class: 'ext', href: routeOf(c.ref.id) }, c.title || 'Untitled')])));
    if (this.writable()) {
      items.push(h('button', { type: 'button', class: 'btn', 'aria-haspopup': 'menu', onclick: (e) => this.#pageMenu(e.currentTarget) }, icon('grip'), h('span', { class: 'lbl' }, 'Page')));
    }
    replace(this.subpages, items);
  }

  #banners() {
    const out = [];
    const n = this.store.pending.size;
    if (this.net.stopped) {
      const texts = this.store.unsentText();
      const code = this.net.stopped.code;
      out.push(h('div', { class: 'banner bad', role: 'alert' }, icon('bad'),
        h('div', {}, h('strong', {}, 'Editing stopped. '), STOPPED[code] || `The server could not accept your edits (${code}).`, n ? ` ${plural(n, 'unsaved edit')} stay on this screen. Copy them before you reload.` : ''),
        n ? h('div', { class: 'acts' },
          h('button', { type: 'button', class: 'btn primary', onclick: () => this.#copyUnsent(texts) }, icon('copy'), ' Copy unsaved text'),
          h('button', { type: 'button', class: 'btn', onclick: () => this.#downloadUnsent() }, icon('download'), ' Download')) : null,
        h('textarea', { class: 'unsent', 'aria-label': 'Unsaved text', readonly: '', rows: '3', hidden: !texts.length || null }, texts.join('\n\n'))));
    } else if (!this.net.open && n) {
      out.push(h('div', { class: 'banner warn', role: 'status' }, icon('warn'),
        h('div', {}, h('strong', {}, 'Reconnecting. '), `Your ${plural(n, 'unsaved edit')} stay on screen and are sent again when the connection is back. They are not saved yet.`)));
    }
    if (this.confirmDelete) {
      const page = this.view().get(this.page);
      const subs = page ? this.view().subtreeSize(page.ref.id) : 0;
      out.push(h('div', { class: 'banner bad', role: 'alert' }, icon('trash'),
        h('div', {}, h('strong', {}, `Delete “${page ? page.title || 'Untitled' : ''}”${subs ? ` and ${plural(subs, 'subpage')}` : ''}?`), ' You can restore it from History.'),
        h('div', { class: 'acts' },
          h('button', { type: 'button', class: 'btn danger', onclick: () => this.#deletePage() }, 'Delete'),
          h('button', { type: 'button', class: 'btn', onclick: () => { this.confirmDelete = false; this.render(); } }, 'Cancel'))));
    }
    for (const nt of this.notices) {
      const text = nt.content.kind === 'code' ? nt.content.text : plainText(nt.content.text || nt.content.caption || []);
      out.push(h('div', { class: 'banner info', role: 'status', 'data-notice': nt.id }, icon('info'),
        h('div', {}, h('strong', {}, nt.message), ' Your text was: ', h('q', {}, text),
          h('div', { class: 'note' }, 'Only this tab remembers your text. Reloading or closing it drops this notice.')),
        h('div', { class: 'acts' },
          this.writable() ? h('button', { type: 'button', class: 'btn primary', onclick: () => this.#keep(nt) }, 'Keep my text') : null,
          h('button', { type: 'button', class: 'btn', onclick: () => this.#copyUnsent([text]) }, 'Copy'),
          h('button', { type: 'button', class: 'btn', onclick: () => { this.notices = this.notices.filter((x) => x !== nt); this.render(); } }, 'Dismiss'))));
    }
    replace(this.banners, out);
  }

  async #copyUnsent(texts) {
    const text = texts.join('\n\n');
    try {
      await navigator.clipboard.writeText(text);
      toast(`Copied ${plural(texts.length, 'text')}.`);
    } catch {
      const ta = this.banners.querySelector('textarea.unsent');
      if (ta) { ta.hidden = false; ta.focus(); ta.select(); }
      toast('Copying was blocked. Select the text and copy it.');
    }
  }

  #downloadUnsent() {
    const data = JSON.stringify({ notebook: location.pathname, pending: this.store.pending.actions.map((a) => ({ n: a.n, ops: a.ops })), text: this.store.unsentText() }, null, 2);
    const url = URL.createObjectURL(new Blob([data], { type: 'application/json' }));
    const a = h('a', { href: url, download: 'notebook-unsaved.json' });
    document.body.appendChild(a);
    a.click();
    a.remove();
    this.timers.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  // ---- focus and selection ----

  #focusedBlock() {
    const el = document.activeElement;
    const blk = el && el.closest && el.closest('.blk');
    return blk && this.pageEl.contains(blk) ? blk.dataset.id : null;
  }

  focusBlock(id, offset = 0, field = 'text') {
    const el = this.editableOf(id, field);
    const block = this.view().get(id);
    if (!el || !block) { if (block) this.selectBlocks([id]); return; }
    this.selection.clear();
    el.focus();
    const len = textLength(fieldRuns(block, field) || []);
    placeCaret(el, { start: Math.min(offset, len) });
  }

  #selected() {
    if (!this.selection.crossBlock || !this.page) return new Set();
    return new Set(this.selection.blocks(this.view(), this.page));
  }

  selectBlocks(ids) {
    this.closeRun();
    this.selection.selectBlocks(ids);
    this.render();
    this.pageEl.focus();
    announce(`${plural(ids.length, 'block')} selected.`);
  }

  // extendSelection starts or extends a cross-block selection from a caret
  // at a block boundary (Shift+arrow).
  extendSelection(block, caret, dir) {
    if (!this.selection.active) this.selection.set({ block: block.ref.id, offset: caret.anchor ?? caret.start });
    if (this.selection.extend(this.view(), this.page, dir)) {
      this.render();
      this.pageEl.focus();
      announce(`${plural(this.#selected().size, 'block')} selected.`);
    }
  }

  // #pointerSelection materializes a native selection dragged across
  // blocks as model endpoints.
  #pointerSelection() {
    const s = globalThis.getSelection && getSelection();
    if (!s || !s.anchorNode || s.isCollapsed) return;
    const a = s.anchorNode.parentNode && (s.anchorNode.nodeType === 1 ? s.anchorNode : s.anchorNode.parentNode).closest('.blk');
    const f = s.focusNode.parentNode && (s.focusNode.nodeType === 1 ? s.focusNode : s.focusNode.parentNode).closest('.blk');
    if (!a || !f || a === f || !this.pageEl.contains(a) || !this.pageEl.contains(f)) return;
    const ea = this.editableOf(a.dataset.id), ef = this.editableOf(f.dataset.id);
    const ca = ea ? readEndpoint(ea, s.anchorNode, s.anchorOffset) : null;
    const cf = ef ? readEndpoint(ef, s.focusNode, s.focusOffset) : null;
    if (ca == null || cf == null) return;
    this.selection.set({ block: a.dataset.id, offset: ca }, { block: f.dataset.id, offset: cf });
    s.removeAllRanges();
    this.render();
    this.pageEl.focus();
  }

  #selectionKey(e) {
    if (e.target !== this.pageEl || !this.selection.active) return;
    const view = this.view();
    const ids = this.selection.blocks(view, this.page);
    const blocks = act.roots(view, ids);
    const mod = e.metaKey || e.ctrlKey;
    if (e.key === 'Escape') { e.preventDefault(); this.selection.clear(); this.render(); return; }
    if (e.key === 'Enter') { e.preventDefault(); const b = ids[0]; this.selection.clear(); this.focusBlock(b, Infinity); return; }
    if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') && !mod) {
      e.preventDefault();
      const dir = e.key === 'ArrowUp' ? -1 : 1;
      if (e.shiftKey) this.selection.extend(view, this.page, dir);
      else {
        const order = view.order(this.page);
        const i = order.findIndex((b) => b.ref.id === (dir < 0 ? ids[0] : ids.at(-1)));
        const next = order[i + dir];
        if (next) this.selection.selectBlocks([next.ref.id]);
      }
      this.render();
      return;
    }
    if (!this.writable() || !blocks.length) return;
    if (e.key === 'Backspace' || e.key === 'Delete') {
      e.preventDefault();
      const next = nearestSurvivor(view.order(this.page).map((b) => b.ref.id), ids.at(-1), (id) => !ids.includes(id));
      this.selection.clear();
      this.queue(act.remove(blocks), { focus: next ? { block: next, offset: 0 } : null });
      announce(`Deleted ${plural(blocks.length, 'block')}.`);
      return;
    }
    if (mod && e.key.toLowerCase() === 'd') { e.preventDefault(); this.queue(act.duplicate(blocks)); return; }
    if (mod && e.shiftKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
      e.preventDefault();
      const ops = act.moveBy(view, blocks, e.key === 'ArrowUp' ? -1 : 1);
      if (ops) this.queue(ops);
      return;
    }
    if (e.key === 'Tab') {
      e.preventDefault();
      const plan = e.shiftKey ? act.outdent(view, blocks) : act.indent(view, blocks);
      if (plan && !plan.refused) this.queue(plan.ops);
      return;
    }
    if (mod && e.key === '/') { e.preventDefault(); this.#blockMenu(blocks, this.pageEl); }
  }

  #copy(e) {
    if (!this.selection.crossBlock || !this.page) return;
    e.preventDefault();
    e.clipboardData.setData('text/plain', selectionText(this.view(), this.page, this.selection));
  }

  #focusin(e) {
    const blk = e.target && e.target.closest && e.target.closest('.blk');
    if (!this.page || this.net.access !== 'private') return;
    const block = blk && this.view().get(blk.dataset.id);
    if (blk && block && block.content.kind === 'database') { this.net.presence({ page: this.page, edit: this.#cellOf(e.target) }); return; }
    if (blk && !e.target.closest('[data-field]')) return;
    if (this.selection.active && e.target !== this.pageEl) { this.selection.clear(); this.render(); }
    this.net.presence({ page: this.page, edit: block ? { block: ref(block) } : null });
  }

  // ---- block handle, menus, drag ----

  #handle(b, el) {
    const grip = el.querySelector('.grip'), add = el.querySelector('.add');
    grip.addEventListener('click', () => {
      const view = this.view();
      const ids = this.#selected();
      const blocks = ids.has(b.ref.id) ? act.roots(view, [...ids]) : [view.get(b.ref.id)].filter(Boolean);
      this.#blockMenu(blocks, grip);
    });
    add.addEventListener('click', () => {
      const block = this.view().get(b.ref.id);
      if (!block) return;
      const plan = act.createBlock(this.view(), block, withDefaults({ kind: 'paragraph', text: [] }));
      this.queue(plan.ops, { focus: { block: plan.id, offset: 0 } });
      const t = this.editableOf(plan.id);
      if (t) this.slashMenu.show(t, this.view().get(plan.id), '', { start: 0, end: 0 });
    });
    grip.addEventListener('dragstart', (e) => {
      const ids = this.#selected();
      const list = ids.has(b.ref.id) ? [...ids] : [b.ref.id];
      e.dataTransfer.setData('application/x-notebook-blocks', JSON.stringify(list));
      e.dataTransfer.effectAllowed = 'move';
    });
    el.addEventListener('dragover', (e) => {
      if (!e.dataTransfer || !Array.from(e.dataTransfer.types || []).includes('application/x-notebook-blocks')) return;
      e.preventDefault();
      el.dataset.drop = this.#dropWhere(e, el, this.view().get(b.ref.id));
    });
    el.addEventListener('dragleave', () => { delete el.dataset.drop; });
    el.addEventListener('drop', (e) => {
      e.preventDefault();
      e.stopPropagation();
      delete el.dataset.drop;
      const view = this.view();
      const target = view.get(b.ref.id);
      let ids;
      try { ids = JSON.parse(e.dataTransfer.getData('application/x-notebook-blocks')); } catch { return; }
      const blocks = act.roots(view, ids);
      if (!target || !blocks.length || blocks.some((x) => x.ref.id === target.ref.id)) return;
      this.queue(act.moveTo(view, blocks, target, this.#dropWhere(e, el, target)));
    });
  }

  #dropWhere(e, el, target) {
    const r = el.getBoundingClientRect();
    const y = (e.clientY - r.top) / Math.max(1, r.height);
    if (target && PARENT_KINDS.includes(target.content.kind) && y > 0.5 && (e.clientX - r.left) > 48) return 'inside';
    return y < 0.5 ? 'before' : 'after';
  }

  #popup(menu, anchor) {
    this.#closePopup();
    const close = menu.close;
    menu.close = () => { close(); this.popup = null; anchor.focus && anchor.focus(); };
    this.popup = menu;
    this.article.appendChild(menu);
    menu.focusFirst();
    const outside = (e) => { if (!menu.contains(e.target)) { menu.remove(); this.popup = null; document.removeEventListener('pointerdown', outside, true); } };
    document.addEventListener('pointerdown', outside, true);
  }

  #closePopup() { if (this.popup) { this.popup.remove(); this.popup = null; } }

  #blockMenu(blocks, anchor) {
    if (!blocks.length) return;
    const view = this.view();
    const targets = act.turnTargets(blocks);
    const entries = ['Turn into'];
    if (targets.length) for (const k of targets) entries.push([kindLabel(k), () => this.queue(act.turn(blocks, k))]);
    else entries.push([`${kindLabel(blocks[0].content.kind)} blocks cannot be turned into another kind.`, () => {}, { disabled: true }]);
    entries.push(blocks.length > 1 ? `${blocks.length} blocks` : 'Block',
      ['Duplicate', () => this.queue(act.duplicate(blocks))],
      ['Move up', () => { const ops = act.moveBy(view, blocks, -1); if (ops) this.queue(ops); }, { disabled: !act.moveBy(view, blocks, -1) }],
      ['Move down', () => { const ops = act.moveBy(view, blocks, 1); if (ops) this.queue(ops); }, { disabled: !act.moveBy(view, blocks, 1) }],
      ['Move to page…', () => this.#pagePicker(anchor, 'Move to page', (p) => { this.queue(act.moveToPage(view, blocks, p)); toast(`Moved ${plural(blocks.length, 'block')} to ${p.title || 'Untitled'}.`); }, this.page)],
      ['Copy link', () => this.#copyLink(blocks[0])],
      ['Delete', () => { this.selection.clear(); this.queue(act.remove(blocks)); announce(`Deleted ${plural(blocks.length, 'block')}.`); }]);
    this.#popup(actionMenu('Block actions', entries), anchor);
  }

  #pagePicker(anchor, label, onPick, except) {
    const view = this.view();
    const entries = [label];
    const walk = (parent, depth) => {
      for (const p of view.pages(parent)) {
        if (p.ref.id !== except) entries.push([`${'  '.repeat(depth)}${p.title || 'Untitled'}`, () => onPick(p)]);
        walk(p.ref.id, depth + 1);
      }
    };
    walk(null, 0);
    this.#popup(actionMenu(label, entries), anchor);
  }

  async #copyLink(block) {
    const url = location.origin + location.pathname + routeOf(this.page) + '/' + block.ref.id;
    try { await navigator.clipboard.writeText(url); toast('Link copied.'); } catch { toast('Copying was blocked.'); }
  }

  #pageMenu(anchor) {
    const page = this.view().get(this.page);
    if (!page) return;
    this.#popup(actionMenu('Page actions', [
      ['Add subpage', () => this.newPage(page)],
      ['Duplicate page', () => this.queue([{ op: 'duplicate_page', page: ref(page) }])],
      ['Move page…', () => this.#pagePicker(anchor, 'Move under', (p) => this.queue([{ op: 'move_page', page: ref(page), parent: ref(p), before: null }]), page.ref.id)],
      ['Move to top level', () => this.queue([{ op: 'move_page', page: ref(page), parent: null, before: null }]), { disabled: !page.parent }],
      ['Delete page', () => { this.confirmDelete = true; this.render(); }, { disabled: !page.parent && this.view().pages(null).length <= 1, hint: !page.parent && this.view().pages(null).length <= 1 ? 'A notebook keeps at least one page.' : null }],
    ]), anchor);
  }

  newPage(parent) {
    const plan = act.createPage(this.view(), parent);
    this.queue(plan.ops);
    if (parent) this.sidebar.expanded.add(parent.ref.id);
    this.go(plan.id);
    this.titleEl.focus();
  }

  #deletePage() {
    const page = this.view().get(this.page);
    this.confirmDelete = false;
    if (!page) return;
    const sibling = this.view().pages(null).find((p) => p.ref.id !== page.ref.id);
    const next = page.parent || (sibling && sibling.ref.id);
    this.queue([{ op: 'delete_page', page: ref(page) }]);
    toast(`Deleted “${page.title || 'Untitled'}”. Links to it show Missing page.`);
    if (next) this.go(next);
  }

  #toggle(id) {
    const b = this.view().get(id);
    if (!b) return;
    const open = this.open.has(id) ? this.open.get(id) : !!b.content.open_default;
    this.open.set(id, !open);
    this.render();
  }

  // ---- slash menu ----

  openSlash(el, block, query, range) {
    if (!this.writable()) return;
    this.slashMenu.show(el, block, query, range);
  }

  #pickSlash(key, blockId, range) {
    const view = this.view();
    const block = view.get(blockId);
    if (!block) return;
    this.closeRun();
    const rest = deleteRange(block.content.text, range.start, range.end);
    const empty = !textLength(rest);
    if (TEXT_KINDS.includes(key) || key === 'code') {
      const content = key === 'code' ? { kind: 'code', text: plainText(rest), language: '' } : { ...withDefaults({ kind: key, text: rest }), ...(key === block.content.kind ? { ...block.content, text: rest } : {}) };
      this.queue([act.setBlock(block, content)], { focus: { block: blockId, offset: range.start } });
      return;
    }
    const cleaned = empty ? [] : [act.setBlock(block, { ...block.content, text: rest })];
    const finish = (create, focus) => {
      const ops = act.replaceWith(view, block, [...cleaned, ...create], empty);
      this.queue(ops, { focus });
    };
    switch (key) {
      case 'divider': finish([{ op: 'create_block', ref: 'divider', proposed_id: act.newId('block'), place: { page: ref(view.get(block.page)), parent: block.parent ? ref(view.get(block.parent)) : null, after: ref(block) }, content: { kind: 'divider' } }]); return;
      case 'new_page': { const p = act.newPageLink(view, block); finish(p.ops); this.go(p.page); return; }
      case 'database': { const d = act.newDatabase(view, block); finish(d.ops); return; }
      case 'page_link': {
        const anchor = this.editableOf(blockId) || this.pageEl;
        this.#pagePicker(anchor, 'Link to page', (p) => finish([{ op: 'create_block', ref: 'link', proposed_id: act.newId('block'), place: { page: ref(view.get(block.page)), parent: block.parent ? ref(view.get(block.parent)) : null, after: ref(block) }, content: { kind: 'page_link', target: p.ref.id } }]));
        return;
      }
      case 'image': this.#pickImage(block, cleaned, empty); return;
    }
  }

  #pickImage(block, cleaned, empty) {
    const input = h('input', { type: 'file', accept: paste.IMAGE_TYPES.join(','), class: 'sr' });
    input.addEventListener('change', async () => {
      input.remove();
      const file = input.files && input.files[0];
      if (!file) return;
      const created = await this.#uploadImages([file], block);
      if (created.length) this.queue(act.replaceWith(this.view(), block, [...cleaned, ...created], empty));
    });
    document.body.appendChild(input);
    input.click();
  }

  async #uploadImages(files, after) {
    const view = this.view();
    const out = [];
    let prev = after;
    for (const file of files) {
      try {
        const r = await paste.upload(this.fetch, file);
        if (r.ext) this.store.canonical.assets.set(r.hash, r.ext);
        const id = act.newId('block');
        out.push({ op: 'create_block', ref: 'img' + out.length, proposed_id: id, place: { page: ref(view.get(after.page)), parent: after.parent ? ref(view.get(after.parent)) : null, after: out.length ? { tmp: 'img' + (out.length - 1) } : ref(prev) }, content: { kind: 'image', asset: r.hash, alt: file.name ? file.name.replace(/\.[^.]+$/, '') : '', caption: [] } });
        prev = null;
      } catch (err) {
        toast(`The image could not be uploaded: ${err.message}`, { error: true });
      }
    }
    return out;
  }

  // ---- paste ----

  async paste(data, t, caret) {
    const c = paste.classify(data);
    if (c.kind === 'plain' || t.field !== 'text' || t.block.content.kind === 'code') {
      this.editor.insertPlain(t, caret, c.text || (data.getData && data.getData('text/plain')) || '');
      return;
    }
    const block = this.view().get(t.block.ref.id);
    if (c.kind === 'image') {
      const created = await this.#uploadImages(c.files, block);
      if (created.length) this.queue(created);
      return;
    }
    const empty = !textLength(block.content.text);
    try {
      const r = await paste.convert(this.fetch, c.kind === 'html' ? { html: c.html } : { markdown: c.text });
      const view = this.view();
      const live = view.get(block.ref.id);
      if (!live) return;
      const op = {
        op: 'markdown_to_blocks',
        place: { page: ref(view.get(live.page)), parent: live.parent ? ref(view.get(live.parent)) : null, after: ref(live) },
        markdown: r.markdown,
        proposed_ids: (r.refs || []).map((x) => ({ ref: x.ref, proposed_id: act.newId(x.kind) })),
      };
      if (r.warnings && r.warnings.length) toast(`Pasted with ${plural(r.warnings.length, 'change')}: ${r.warnings[0].message}`);
      this.queue(act.replaceWith(view, live, [op], empty));
    } catch {
      this.editor.insertPlain(t, caret, c.text || '');
    }
  }

  // ---- databases (table.js renders them) ----

  // #database mounts a database view once per block element and afterwards
  // only updates it, and only when something it shows changed, so focus and
  // an open cell editor survive remote changes (TECH 18). A view whose block
  // leaves the page is stopped by #sweepDatabases.
  #database(block, el) {
    const id = block.ref.id;
    const mounted = this.databases.get(id);
    // Renders happen on every keystroke; a view needs work only when the
    // display state or what decorates it changed.
    const at = [this.view(), this.presence.editors, this.noticeVersion, this.writable()];
    if (mounted && mounted.el === el && sameItems(mounted.at, at)) return;
    const data = this.#databaseData(block);
    if (!data) {
      if (mounted) { mounted.handle.stop(); this.databases.delete(id); }
      el.replaceChildren(h('p', { class: 'note loading' }, 'Loading…'));
      return;
    }
    if (mounted && mounted.el === el) {
      mounted.at = at;
      if (sameItems(mounted.deps, data.deps)) return;
      mounted.deps = data.deps;
      mounted.handle.update(data.ctx);
      return;
    }
    if (mounted) mounted.handle.stop();
    this.databases.set(id, { el, at, deps: data.deps, handle: renderDatabase(block, el, data.ctx) });
  }

  // #databaseData gathers what table.js reads, with the items it came from
  // as deps: the view caches items, so an unchanged item keeps its identity.
  #databaseData(block) {
    const view = this.view();
    const db = view.get(block.content.database);
    if (!db) return null;
    const id = db.ref.id;
    const props = (db.property_order || []).map((p) => view.get(p)).filter(Boolean);
    const options = props.map((p) => (p.option_order || []).map((o) => view.get(o)).filter(Boolean));
    const views = (db.view_order || []).map((v) => view.get(v)).filter(Boolean);
    const rows = [];
    for (const it of view.map.values()) if (it.kind === 'row' && it.database === id) rows.push(it);
    const loading = this.store.canonical.loading({ kind: 'database', id });
    const readOnly = !this.writable();
    const presence = this.presence.editors.filter((e) => e.edit && e.edit.database && e.edit.database.id === id);
    const deps = [db, ...props, ...options.flat(), ...views, ...rows, loading, readOnly, JSON.stringify(presence), this.noticeVersion];
    const ctx = {
      schema: {
        database: db,
        title: db.title,
        properties: props.map((p, i) => ({ ref: p.ref, definition: p.definition, options: options[i] })),
      },
      views,
      rows: () => rows,
      loading,
      readOnly,
      presence,
      notices: this.viewNotices,
      preview: (property, definition) => this.#conversionPreview(property, definition),
      onOps: (ops) => this.queue(ops),
    };
    return { deps, ctx };
  }

  #sweepDatabases() {
    for (const [id, m] of this.databases) {
      const e = this.pageView.entryOf(id);
      if (e && e.db === m.el && m.el.isConnected) continue;
      m.handle.stop();
      this.databases.delete(id);
    }
  }

  // #cellOf names the database cell an element is in, for presence.
  #cellOf(el) {
    const td = el.closest && el.closest('td[data-prop]');
    const tr = td && td.closest('tr[data-row]');
    const view = this.view();
    const row = tr && view.get(tr.dataset.row);
    const prop = td && view.get(td.dataset.prop);
    const db = row && view.get(row.database);
    return db && prop ? { database: ref(db), row: ref(row), property: ref(prop) } : null;
  }

  // #viewAdjustment keeps a schema change's view repair notice for this
  // tab until reload (rule 10a).
  #viewAdjustment(m) {
    const view = this.view();
    const text = adjustmentText(view, m.removed || []);
    if (!text) return;
    const prev = this.viewNotices.get(m.view);
    this.viewNotices.set(m.view, prev ? `${prev} ${text}` : text);
    this.noticeVersion++;
    this.render();
  }

  async #conversionPreview(property, definition) {
    const res = await this.fetch('/_notebook/api/conversion-preview', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'X-Flats-Notebook': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify({ property, definition }),
    });
    if (!res.ok) throw new Error(`preview failed: ${res.status}`);
    return res.json();
  }
}

const sameItems = (a, b) => a.length === b.length && a.every((x, i) => x === b[i]);

const REMOVED = {
  sort: (n) => `the sort by ${n}`,
  filter: (n) => `the filter on ${n}`,
  'filter-option': (n) => `the filter on ${n}, which named a deleted option`,
  visible: (n) => `${n} from the shown properties`,
  width: (n) => `the column width of ${n}`,
  group_by: (n) => `grouping by ${n}, so the view is now a table`,
  gone: () => 'a reference to a deleted item',
  title_position: () => 'a position other than first for the title column',
  title_visibility: () => 'hiding the title column',
};

// adjustmentText names what a view repair removed, using property names
// that still exist.
function adjustmentText(view, removed) {
  const parts = [];
  for (const r of removed) {
    const at = r.indexOf(':');
    const kind = at < 0 ? r : r.slice(0, at);
    const target = at < 0 ? null : view.get(r.slice(at + 1));
    const name = target && target.definition ? target.definition.name : 'a deleted property';
    const say = REMOVED[kind];
    if (say) parts.push(say(name));
  }
  if (!parts.length) return '';
  return `A schema change removed ${parts.length > 1 ? parts.slice(0, -1).join(', ') + ' and ' + parts.at(-1) : parts[0]} from this view.`;
}

if (!globalThis.notebookTest) globalThis.notebook = new App();
