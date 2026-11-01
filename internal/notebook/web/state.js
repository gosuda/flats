// Client state (TECH 9 and 13). Canonical state is a captured base per
// scope plus a live-delta overlay; the person's own unconfirmed actions are
// a separate pending overlay on top. Only complete canonical deltas and
// complete snapshot units change canonical state. Acknowledgements remove
// pending actions and never apply content themselves.
//
// Items use the wire codec of reads: {kind, ref: {id, inc}, ver, v, ...}
// with the kind's fields. Public reads carry {id, kind, ...} instead; state
// adds a ref.id alias for display without inventing private version stamps.
// Delta deletions carry {ref, deleted: true} (Private) or {id, kind,
// deleted: true} (Public). Older fixtures use {deleted: {id, inc}}.
'use strict';


import { canonicalize, plainText } from './richtext.js';

export const TEXT_KINDS = ['paragraph', 'heading1', 'heading2', 'heading3', 'bullet', 'numbered', 'todo', 'toggle', 'quote', 'callout'];
export const PARENT_KINDS = ['bullet', 'numbered', 'todo', 'toggle'];
export const isText = (kind) => TEXT_KINDS.includes(kind);

const dec = (s) => BigInt(s || '0');
export const decGreater = (a, b) => dec(a) > dec(b);

export const ref = (item) => ({ id: item.ref.id, inc: item.ref.inc });

// groupVersion is the last write version of one value group; groups never
// written after creation carry the incarnation (TECH 4.3).
export const groupVersion = (item, group) => (item.v && item.v[group]) || item.ref.inc;

function readItem(item) {
  return item.ref ? item : { ...item, ref: { id: item.id } };
}

// ---- fractional positions (the server's alphabet; local display only) ----

const DIGITS = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz';

function midpoint(a, b) {
  if (b) {
    let n = 0;
    while (n < b.length && (a[n] || '0') === b[n]) n++;
    if (n > 0) return b.slice(0, n) + midpoint(a.slice(n), b.slice(n));
  }
  const da = a ? DIGITS.indexOf(a[0]) : 0;
  const db = b ? DIGITS.indexOf(b[0]) : DIGITS.length;
  if (db - da > 1) return DIGITS[(da + db) >> 1];
  if (b && b.length > 1) return b.slice(0, 1);
  return DIGITS[da] + midpoint(a.slice(1), '');
}

// positionBetween returns a key strictly between a and b ('' is either end).
export const positionBetween = (a, b) => midpoint(a || '', b || '');

export const byPosition = (x, y) => (x.position < y.position ? -1 : x.position > y.position ? 1 : x.ref.id < y.ref.id ? -1 : x.ref.id > y.ref.id ? 1 : 0);

// ---- scopes ----

export const scopeKey = (s) => (s.kind === 'tree' ? 'tree' : s.kind + ':' + s.id);

// inScope reports whether item belongs to scope, given a lookup for owners.
function inScope(scope, item, get) {
  switch (scope.kind) {
    case 'tree': return item.kind === 'page';
    case 'page': return pageOf(item, get) === scope.id && item.kind !== 'page' && item.kind !== 'row';
    case 'database': {
      if (item.kind === 'database') return item.ref.id === scope.id;
      if (item.kind === 'option') { const p = get(item.property); return !!p && p.database === scope.id; }
      return ['property', 'view', 'row'].includes(item.kind) && item.database === scope.id;
    }
  }
  return false;
}

// pageOf is the page an item lives on: a block's page, or the page of the
// block owning its database.
function pageOf(item, get) {
  switch (item.kind) {
    case 'page': return item.ref.id;
    case 'block': return item.page;
    case 'database': { const b = get(item.block); return b && b.page; }
    case 'property': case 'view': case 'row': { const d = get(item.database); return d && pageOf(d, get); }
    case 'option': { const p = get(item.property); return p && pageOf(p, get); }
  }
  return null;
}

// ---- canonical state ----

export class Canonical {
  constructor() {
    this.base = new Map();     // id -> item from installed snapshot units
    this.overlay = new Map();  // id -> {seq, item} or {seq, deleted: ref}
    this.seq = null;           // last applied delta or capture sequence
    this.installed = new Map(); // scope key -> {seq, complete, windows}
    this.stale = new Set();    // invalidated scope keys
    this.assets = new Map();   // asset hash -> file extension
    this.version = 0;
  }

  get(id) {
    const o = this.overlay.get(id);
    if (o) return o.deleted ? null : o.item;
    return this.base.get(id) || null;
  }

  *items() {
    for (const [id, item] of this.base) if (!this.overlay.has(id)) yield item;
    for (const o of this.overlay.values()) if (!o.deleted) yield o.item;
  }

  // applyDelta applies one complete canonical delta at once.
  applyDelta(d) {
    for (const rec of [...(d.tree || []), ...(d.changes || [])]) {
      if (rec.deleted) {
        const deleted = rec.deleted === true ? rec.ref || { id: rec.id } : rec.deleted;
        this.overlay.set(deleted.id, { seq: d.seq, deleted });
      } else {
        const item = readItem(rec);
        this.overlay.set(item.ref.id, { seq: d.seq, item });
      }
    }
    for (const s of d.scopes || []) this.stale.add(scopeKey(s.scope || s));
    for (const [h, ext] of Object.entries(d.assets || {})) this.assets.set(h, ext);
    this.seq = d.seq;
    this.version++;
  }

  // install puts one complete snapshot unit captured at seq into the base.
  // Overlay values newer than the capture keep winning; overlay entries at
  // or below it are subsumed by the capture. A database scope arrives in
  // windows: schema reconciles in window zero, and rows stay until the final
  // window, so a resumed or repeated capture never blanks rows that are still
  // there, and created or moved rows stay visible in the overlay before their
  // window arrives.
  install(scope, seq, items, { window = null, final = true, assets } = {}) {
    const key = scopeKey(scope);
    let st = this.installed.get(key);
    if (window == null || window === 0 || !st || st.seq !== seq) st = { seq, complete: false, seen: new Set(), windows: 0 };
    for (const record of items) {
      const item = readItem(record);
      this.base.set(item.ref.id, item);
      st.seen.add(item.ref.id);
      const o = this.overlay.get(item.ref.id);
      if (o && !decGreater(o.seq, seq)) this.overlay.delete(item.ref.id);
    }
    if (window != null) st.windows++;
    const schema = scope.kind === 'database' && window === 0;
    if (final || schema) {
      const lookup = (id) => this.base.get(id) || this.get(id);
      const inUnit = (item) => (final || item.kind !== 'row') && inScope(scope, item, lookup);
      const baseDeletes = [], overlayDeletes = [];
      // Schema is complete in window zero, even when a receipt omits rows.
      // Resolve shared page/database ownership before removing any owner.
      for (const [id, item] of this.base) if (!st.seen.has(id) && inUnit(item)) baseDeletes.push(id);
      for (const [id, o] of this.overlay) {
        if (decGreater(o.seq, seq) || st.seen.has(id)) continue;
        const item = o.item || this.base.get(id);
        if (item ? inUnit(item) : final) overlayDeletes.push(id);
      }
      for (const id of baseDeletes) this.base.delete(id);
      for (const id of overlayDeletes) this.overlay.delete(id);
      if (final) this.stale.delete(key);
    }
    st.complete = final;
    this.installed.set(key, st);
    for (const [h, ext] of Object.entries(assets || {})) this.assets.set(h, ext);
    if (this.seq == null || decGreater(seq, this.seq)) this.seq = seq;
    this.version++;
  }

  // loading reports a scope whose capture has not completed.
  loading(scope) { const s = this.installed.get(scopeKey(scope)); return !s || !s.complete; }

  // reset drops everything: a rollback or an unknown watermark (rule 35).
  reset() {
    this.base.clear();
    this.overlay.clear();
    this.installed.clear();
    this.stale.clear();
    this.seq = null;
    this.version++;
  }

  hasScope(scope) {
    const s = this.installed.get(scopeKey(scope));
    return !!s && s.complete && !this.stale.has(scopeKey(scope));
  }
}

// ---- pending local actions ----

// Pending holds the tab's unconfirmed actions in order. An action is
// {n, action_id, ops, typing_block?, close?, held, sent, applied}. n is assigned
// when the action is queued and never changes. sent stays true once any
// socket carried the action, so its payload is never changed afterwards and
// a resend is exact. applied means the canonical delta tagged with this
// action arrived, so its effects are in canonical state and it no longer
// overlays.
export class Pending {
  constructor(captureScopes = () => {}) {
    this.captureScopes = captureScopes;
    this.actions = [];
    this.nextN = 1n;
    this.version = 0;
  }

  get size() { return this.actions.length; }
  get(n) { return this.actions.find((a) => a.n === String(n)) || null; }

  push(action) {
    const a = { held: false, ...action, n: String(this.nextN++), sent: false, applied: false };
    this.actions.push(a);
    this.version++;
    this.captureScopes();
    return a;
  }

  // draft returns the last action while it is still held for more typing
  // under key, so one flush window coalesces into one action. A held action
  // has never been sent.
  draft(key) {
    const a = this.actions.at(-1);
    return a && a.held && !a.sent && a.draftKey === key ? a : null;
  }

  replaceOps(a, ops) { a.ops = ops; this.version++; this.captureScopes(); }

  // release lets a held action be sent.
  release(a, close = false) {
    if (!a || !a.held) return;
    a.held = false;
    if (close) a.close = true;
    this.version++;
  }
  markApplied(n) { const a = this.get(n); if (a) { a.applied = true; this.version++; } }

  remove(n) {
    const i = this.actions.findIndex((a) => a.n === String(n));
    if (i < 0) return null;
    const [a] = this.actions.splice(i, 1);
    this.version++;
    return a;
  }

}

// ---- the derived view ----

// View is canonical state with pending actions applied, plus indexes.
export class View {
  constructor(items) {
    this.map = items;
    this.children = new Map(); // parent key -> sorted children
    for (const it of items.values()) {
      const k = parentKey(it);
      if (k == null) continue;
      if (!this.children.has(k)) this.children.set(k, []);
      this.children.get(k).push(it);
    }
    for (const list of this.children.values()) list.sort(byPosition);
  }

  get(id) { return this.map.get(id) || null; }
  kids(key) { return this.children.get(key) || []; }
  pages(parent = null) { return this.kids('p:' + (parent || '')); }
  blocks(page, parent = null) { return this.kids('b:' + page + ':' + (parent || '')); }

  // subtreeSize counts the page's descendant pages.
  subtreeSize(page) { return this.pages(page).reduce((n, p) => n + 1 + this.subtreeSize(p.ref.id), 0); }

  ancestors(pageId) {
    const out = [];
    for (let p = this.get(pageId); p && p.parent; p = this.get(p.parent)) {
      const parent = this.get(p.parent);
      if (!parent) break;
      out.unshift(parent);
    }
    return out;
  }

  firstPage() { return this.pages(null)[0] || null; }

  // order lists the page's blocks in document order (depth first).
  order(page) {
    const out = [];
    const walk = (parent) => { for (const b of this.blocks(page, parent)) { out.push(b); walk(b.ref.id); } };
    walk(null);
    return out;
  }

  depth(block) { let d = 1; for (let b = block; b.parent; b = this.get(b.parent)) d++; return d; }
}

function parentKey(it) {
  if (it.kind === 'page') return 'p:' + (it.parent || '');
  if (it.kind === 'block') return 'b:' + it.page + ':' + (it.parent || '');
  return null;
}

// ---- optimistic application of local operations ----

const clone = (x) => (x == null ? x : JSON.parse(JSON.stringify(x)));

// localCell turns a cell input into the stored cell shape of reads,
// {type, value}, where options are ids rather than references. An option
// this action creates has no id until the server answers, so it is left out.
function localCell(prop, value, tmp) {
  const type = prop && prop.definition && prop.definition.type;
  if (!type) return clone(value);
  if (type === 'select') return { type, value: value == null ? null : resolve(value, tmp) };
  if (type === 'multi_select') return { type, value: (value || []).map((t) => resolve(t, tmp)).filter(Boolean) };
  return { type, value: clone(value) };
}

// resolve maps a target to its item id: a reference, or a temporary name
// created earlier in the same action.
function resolve(t, tmp) {
  if (!t) return null;
  if (t.tmp) return tmp.get(t.tmp) || null;
  return t.id;
}

function placeKey(items, place, tmp, movingIds = new Set()) {
  const page = resolve(place.page, tmp), parent = resolve(place.parent, tmp);
  const sibs = [...items.values()].filter((b) => b.kind === 'block' && b.page === page && (b.parent || null) === (parent || null) && !movingIds.has(b.ref.id)).sort(byPosition);
  let i = sibs.length;
  if ('after' in place && place.after) {
    const a = sibs.findIndex((b) => b.ref.id === resolve(place.after, tmp));
    if (a >= 0) i = a + 1;
  } else if (place.before) {
    const a = sibs.findIndex((b) => b.ref.id === resolve(place.before, tmp));
    if (a >= 0) i = a;
  }
  const lo = i > 0 ? sibs[i - 1].position : '', hi = i < sibs.length ? sibs[i].position : '';
  return { page, parent, position: positionBetween(lo, hi) };
}

function pagePlace(items, parent, before, tmp, moving) {
  const sibs = [...items.values()].filter((p) => p.kind === 'page' && (p.parent || null) === (parent || null) && p.ref.id !== moving).sort(byPosition);
  let i = sibs.length;
  if (before) { const a = sibs.findIndex((p) => p.ref.id === resolve(before, tmp)); if (a >= 0) i = a; }
  return positionBetween(i > 0 ? sibs[i - 1].position : '', i < sibs.length ? sibs[i].position : '');
}

function blockSubtree(items, id) {
  const out = [id];
  for (let i = 0; i < out.length; i++) for (const b of items.values()) if (b.kind === 'block' && b.parent === out[i]) out.push(b.ref.id);
  return out;
}

function pageSubtree(items, id) {
  const out = [id];
  for (let i = 0; i < out.length; i++) for (const p of items.values()) if (p.kind === 'page' && p.parent === out[i]) out.push(p.ref.id);
  return out;
}

// convertContent derives new content for turn_blocks (TECH 6 table).
export function convertContent(content, kind) {
  if (content.kind === kind) return clone(content);
  if (isText(content.kind) && isText(kind)) return withDefaults({ kind, text: clone(content.text) });
  if (isText(content.kind) && kind === 'code') return { kind: 'code', text: plainText(content.text), language: '' };
  if (content.kind === 'code' && isText(kind)) return withDefaults({ kind, text: canonicalize([{ text: content.text, marks: [] }]) });
  return null;
}

export function withDefaults(c) {
  const out = { kind: c.kind, text: c.text || [] };
  if (c.kind === 'todo') out.checked = !!c.checked;
  if (c.kind === 'toggle') out.open_default = !!c.open_default;
  if (c.kind === 'callout') out.tone = c.tone || 'neutral';
  return out;
}

// hoist moves children of a block whose new kind holds none to right after
// it at its parent level, keeping their order.
function hoist(items, block) {
  if (PARENT_KINDS.includes(block.content.kind)) return;
  const kids = [...items.values()].filter((b) => b.kind === 'block' && b.parent === block.ref.id).sort(byPosition);
  let after = block.ref.id;
  for (const k of kids) {
    const p = placeKey(items, { page: { id: block.page }, parent: block.parent ? { id: block.parent } : null, after: { id: after } }, new Map(), new Set([k.ref.id]));
    items.set(k.ref.id, { ...k, parent: block.parent || null, position: p.position });
    after = k.ref.id;
  }
}

// applyLocal applies one browser operation to the items map in place, as
// the person expects to see it before the server answers. It returns false
// when a target is missing, which skips the operation locally; the server
// decides the real outcome.
export function applyLocal(items, op, tmp) {
  const get = (t) => { const id = resolve(t, tmp); const it = id && items.get(id); return it && (!t.inc || t.inc === '0' || it.ref.inc === t.inc || t.tmp) ? it : null; };
  switch (op.op) {
    case 'create_page': {
      const parent = resolve(op.parent, tmp);
      const it = { kind: 'page', ref: { id: op.proposed_id, inc: '0' }, ver: '0', v: {}, title: op.title, parent: parent || null, position: pagePlace(items, parent, op.before, tmp) };
      items.set(it.ref.id, it);
      tmp.set(op.ref, it.ref.id);
      return true;
    }
    case 'set_page_title': { const p = get(op.page); if (!p) return false; items.set(p.ref.id, { ...p, title: op.title }); return true; }
    case 'move_page': {
      const p = get(op.page); if (!p) return false;
      const parent = resolve(op.parent, tmp);
      items.set(p.ref.id, { ...p, parent: parent || null, position: pagePlace(items, parent, op.before, tmp, p.ref.id) });
      return true;
    }
    case 'delete_page': {
      const p = get(op.page); if (!p) return false;
      const pages = new Set(pageSubtree(items, p.ref.id));
      for (const [id, it] of items) if (pages.has(id) || (it.kind === 'block' && pages.has(it.page))) items.delete(id);
      return true;
    }
    case 'create_block': {
      const place = placeKey(items, op.place, tmp);
      const it = { kind: 'block', ref: { id: op.proposed_id, inc: '0' }, ver: '0', v: {}, page: place.page, parent: place.parent || null, position: place.position, content: clone(op.content) };
      items.set(it.ref.id, it);
      tmp.set(op.ref, it.ref.id);
      return true;
    }
    case 'set_block': {
      const b = get(op.block); if (!b) return false;
      if (b.content.kind !== op.content.kind && !convertContent(b.content, op.content.kind)) return false;
      const nb = { ...b, content: clone(op.content) };
      items.set(b.ref.id, nb);
      hoist(items, nb);
      return true;
    }
    case 'turn_blocks': {
      const list = op.blocks.map(get);
      if (list.some((b) => !b || !convertContent(b.content, op.kind))) return false;
      for (const b of list) { const nb = { ...b, content: convertContent(b.content, op.kind) }; items.set(b.ref.id, nb); hoist(items, nb); }
      return true;
    }
    case 'move_blocks': {
      const list = op.blocks.map(get).filter(Boolean);
      if (!list.length) return false;
      const moving = new Set(list.map((b) => b.ref.id));
      let place = op.place;
      for (const b of list) {
        const p = placeKey(items, place, tmp, moving);
        const subtree = blockSubtree(items, b.ref.id);
        items.set(b.ref.id, { ...items.get(b.ref.id), page: p.page, parent: p.parent || null, position: p.position });
        for (const id of subtree.slice(1)) items.set(id, { ...items.get(id), page: p.page });
        moving.delete(b.ref.id);
        place = { page: place.page, parent: place.parent, after: { id: b.ref.id } };
      }
      return true;
    }
    case 'delete_blocks': {
      const list = op.blocks.map(get).filter(Boolean);
      if (!list.length) return false;
      for (const b of list) for (const id of blockSubtree(items, b.ref.id)) items.delete(id);
      return true;
    }
    case 'create_row': {
      const database = get(op.database); if (!database) return false;
      tmp.set(op.ref, op.proposed_id);
      if (items.has(op.proposed_id)) return true;
      const rows = [...items.values()].filter((r) => r.kind === 'row' && r.database === database.ref.id).sort(byPosition);
      const before = get(op.before);
      let i = before ? rows.findIndex((r) => r.ref.id === before.ref.id) : rows.length;
      if (i < 0) i = rows.length;
      const cells = {};
      for (const cell of op.cells) {
        const property = get(cell.property);
        if (property) cells[property.ref.id] = localCell(property, cell.value, tmp);
      }
      items.set(op.proposed_id, {
        kind: 'row', ref: { id: op.proposed_id, inc: '0' }, ver: '0', v: {}, database: database.ref.id,
        position: positionBetween(i > 0 ? rows[i - 1].position : '', i < rows.length ? rows[i].position : ''), cells,
      });
      return true;
    }
    case 'set_view': {
      const view = get(op.view); if (!view) return false;
      items.set(view.ref.id, { ...view, definition: clone(op.definition) });
      return true;
    }
    case 'set_cell': {
      const r = get(op.row); if (!r) return false;
      const prop = resolve(op.cell.property, tmp);
      items.set(r.ref.id, { ...r, cells: { ...r.cells, [prop]: localCell(items.get(prop), op.cell.value, tmp) } });
      return true;
    }
    case 'delete_rows': {
      const list = op.rows.map(get).filter(Boolean);
      for (const r of list) items.delete(r.ref.id);
      return list.length > 0;
    }
  }
  return false; // duplicates, schema and other server-computed results wait for the delta
}

// ---- the store ----

export class Store {
  constructor() {
    this.canonical = new Canonical();
    this.pendingScopesByAction = new WeakMap();
    this.pending = new Pending(() => this.pendingScopes());
    this.composing = null; // {block, inc} while an input method composes
    this.cached = null;
    this.cachedAt = null;
  }

  // view returns the display state; it is cached until either layer changes.
  view() {
    const at = this.canonical.version + ':' + this.pending.version;
    if (this.cached && this.cachedAt === at) return this.cached;
    const items = new Map();
    for (const it of this.canonical.items()) items.set(it.ref.id, it);
    for (const a of this.pending.actions) {
      if (a.applied) continue;
      const tmp = new Map();
      for (const op of a.ops) applyLocal(items, op, tmp);
    }
    this.cached = new View(items);
    this.cachedAt = at;
    return this.cached;
  }

  // pendingScopes lists scopes touched by unconfirmed actions, so reconnect
  // hydrates them before resending (TECH 9).
  pendingScopes() {
    if (!this.pending.size) return [];
    const scopes = new Map();
    const items = new Map([...this.canonical.items()].map((it) => [it.ref.id, it]));
    const get = (id) => items.get(id);
    for (const a of this.pending.actions) {
      // Remember ownership before a delta or reset removes its lookup chain.
      const touched = new Map(this.pendingScopesByAction.get(a));
      const tmp = new Map();
      const add = (kind, id) => { if (id) { const s = { kind, id }; touched.set(scopeKey(s), s); } };
      const note = (id) => {
        if (!id) return;
        const it = get(id);
        if (!it) {
          if (id.startsWith('p_')) add('page', id);
          if (id.startsWith('d_')) add('database', id);
          return;
        }
        add('page', pageOf(it, get));
        let owner = it;
        if (owner.kind === 'option') owner = get(owner.property);
        if (!owner) return;
        if (owner.kind === 'database') add('database', owner.ref.id);
        else if (owner.database) add('database', owner.database);
        else if (owner.kind === 'block' && owner.content.kind === 'database') add('database', owner.content.database);
      };
      const visit = (value) => {
        if (!value || typeof value !== 'object') return;
        if (value.id || value.tmp) note(resolve(value, tmp));
        for (const child of Object.values(value)) visit(child);
      };
      const create = (kind, id, fields, name) => {
        if (!items.has(id)) items.set(id, { kind, ref: { id, inc: '0' }, ...fields });
        tmp.set(name, id);
        note(id);
      };
      for (const op of a.ops) {
        switch (op.op) {
          case 'create_page': create('page', op.proposed_id, {}, op.ref); break;
          case 'create_block': create('block', op.proposed_id, { page: resolve(op.place.page, tmp), content: op.content }, op.ref); break;
          case 'create_database': {
            create('block', op.block_proposed_id, { page: resolve(op.place.page, tmp), content: { kind: 'database', database: op.proposed_id } }, op.block_ref);
            create('database', op.proposed_id, { block: op.block_proposed_id }, op.ref);
            create('property', op.title_property_proposed_id, { database: op.proposed_id }, op.ref + '.title');
            create('view', op.view_proposed_id, { database: op.proposed_id }, op.ref + '.view');
            break;
          }
          case 'create_property': case 'create_row': case 'create_view':
            create(op.op.slice(7), op.proposed_id, { database: resolve(op.database, tmp) }, op.ref);
            break;
          case 'create_option': create('option', op.proposed_id, { property: resolve(op.property, tmp) }, op.ref); break;
        }
        visit(op);
      }
      this.pendingScopesByAction.set(a, touched);
      for (const [key, scope] of touched) scopes.set(key, scope);
    }
    return [...scopes.values()];
  }

  // unsentText lists the text of every unconfirmed block write, for the
  // recovery notice when editing stops (rule 40).
  unsentText() {
    const out = [];
    for (const a of this.pending.actions) {
      for (const op of a.ops) {
        if ((op.op === 'set_block' || op.op === 'create_block') && op.content && isText(op.content.kind)) out.push(plainText(op.content.text));
        else if (op.content && op.content.kind === 'code') out.push(op.content.text);
        else if (op.op === 'set_page_title' || op.op === 'create_page') out.push(op.title);
      }
    }
    return out.filter((t) => t);
  }
}
