'use strict';

import { cellDisplay } from './dbschema.js';

const API = '/_notebook/api';
const PAGE_SIZE = 50;
const POST_HEADERS = { 'Content-Type': 'application/json', 'X-Flats-Notebook': '1' };
const KIND_LABELS = { created: 'Created', person: 'Edits', agent: 'Agent', rollback: 'Rollback' };

function el(tag, props = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'disabled') n.disabled = true;
    else if (/^on[A-Z]/.test(k)) n.addEventListener(k.slice(2).toLowerCase(), v);
    else n.setAttribute(k, v === true ? '' : String(v));
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    n.appendChild(typeof kid === 'object' ? kid : document.createTextNode(String(kid)));
  }
  return n;
}

function button(text, onClick, props = {}) {
  return el('button', { type: 'button', ...props, onClick }, text);
}

function cmpVal(a, b) {
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  const x = String(a ?? '');
  const y = String(b ?? '');
  return x < y ? -1 : x > y ? 1 : 0;
}

function byPosition(a, b) {
  return cmpVal(a.position, b.position) || cmpVal(a.id, b.id);
}

// childrenOf groups records by parent; a parent that is missing from the set
// puts the record at the top level so nothing is silently dropped.
function childrenOf(records, parentOf) {
  const ids = new Set(records.map((r) => r.id));
  const map = new Map();
  for (const r of records) {
    const p = parentOf(r);
    const key = p != null && ids.has(p) && p !== r.id ? p : null;
    if (!map.has(key)) map.set(key, []);
    map.get(key).push(r);
  }
  for (const list of map.values()) list.sort(byPosition);
  return map;
}

function titleOf(title) {
  return typeof title === 'string' && title.trim() !== '' ? title : 'Untitled';
}

function relativeTime(created, now) {
  const t = Date.parse(created);
  if (Number.isNaN(t)) return String(created ?? '');
  const s = Math.floor((now - t) / 1000);
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return d === 1 ? '1 day ago' : `${d} days ago`;
  return new Date(t).toISOString().slice(0, 10);
}

function warningText(w) {
  if (typeof w === 'string') return w;
  return String(w?.message ?? w?.code ?? 'Warning');
}

function cssToken(s) {
  return String(s ?? '').toLowerCase().replace(/[^a-z0-9_-]/g, '');
}

function errorText(data, status) {
  let text;
  if (data?.reason === 'session_expired' || data?.reason === 'session') {
    text = 'This notebook session has expired. Reload the notebook, preserving any pending edits first.';
  } else if (data?.code === 'notebook_capacity') {
    text = data.reason === 'disk_quota'
      ? 'Notebook storage quota exceeded. Free storage through retention settings or raise the flat’s quota.'
      : 'The notebook item limit would be exceeded. Remove items or raise the applicable limit.';
  } else if (data?.code === 'not_found') {
    text = data.reason === 'revision'
      ? 'That revision is no longer retained. Refresh History and choose a retained revision.'
      : 'The selected historical content is no longer available. Refresh History and choose a retained revision.';
  } else if (data?.code === 'forbidden') {
    text = 'History requires a Private notebook route and a request from the notebook’s origin.';
  } else if (data?.code === 'conflict' && data.reason === 'read_expired') {
    text = 'This historical read has expired. Open the page or database again.';
  } else if (data?.code === 'conflict') {
    text = `The notebook changed (${data.reason || 'conflict'}). Refresh History before continuing.`;
  } else if (data?.code === 'unavailable') {
    text = 'History is temporarily unavailable.';
  } else if (data?.code === 'no_revisions') {
    text = 'This notebook has no retained revisions.';
  } else if (data?.code === 'invalid') {
    text = `The notebook refused the History request (${data.reason || 'invalid'}). Reload to refresh its state.`;
  } else {
    text = `Request failed (${data?.code || status}${data?.reason ? `: ${data.reason}` : ''}).`;
  }
  if (typeof data?.detail === 'string' && data.detail) text += ` ${data.detail}`;
  if (data?.retry === true) text += ' Try again shortly.';
  return text;
}

export class History {
  constructor(host, { onClose, fetch = globalThis.fetch, session = () => null, currentPages = () => new Set(), onRecovered, onRolledBack, now = () => Date.now() } = {}) {
    this.host = host;
    this.open = false;
    this._onClose = onClose;
    // Called unbound: window.fetch throws "Illegal invocation" as a method.
    this._fetch = (...args) => fetch(...args);
    this._session = session;
    this._currentPages = currentPages;
    this._onRecovered = onRecovered;
    this._onRolledBack = onRolledBack;
    this._now = now;
    this._gen = 0;
    this._reset();
    host.hidden = true;
    host.addEventListener('keydown', (e) => this._keydown(e));
  }

  _reset() {
    this._revisions = [];
    this._next = null;
    this._loading = false;
    this._error = '';
    this._status = '';
    this._view = null;
    this._confirm = null;
    this._busy = false;
    this._focusKey = null;
  }

  toggle(open = !this.open) {
    open = Boolean(open);
    if (open === this.open) {
      this.render();
      return;
    }
    this.open = open;
    this._gen++;
    this._reset();
    if (!open) {
      this.render();
      return;
    }
    this._focusKey = 'close';
    this._loadRevisions(false);
  }

  _close() {
    this.toggle(false);
    this._onClose?.();
  }

  _keydown(e) {
    if (!this.open || e.key !== 'Escape') return;
    e.preventDefault?.();
    e.stopPropagation?.();
    if (this._confirm && !this._busy) {
      this._cancelConfirm();
      return;
    }
    this._close();
  }

  async _request(method, path, body) {
    const init = { method, credentials: 'same-origin' };
    if (body !== undefined) {
      init.headers = { ...POST_HEADERS };
      init.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await this._fetch(path, init);
    } catch {
      throw new Error('Could not reach the notebook. Check the connection and try again.');
    }
    let data = null;
    try {
      data = await res.json();
    } catch {
      data = null;
    }
    const ok = res.ok ?? (res.status >= 200 && res.status < 300);
    if (!ok) {
      const err = new Error(errorText(data, res.status));
      err.code = data?.code;
      err.reason = data?.reason;
      err.retry = data?.retry === true;
      err.detail = data?.detail;
      throw err;
    }
    if (!data || typeof data !== 'object' || Array.isArray(data)) {
      throw new Error('The notebook returned an invalid response. Reload to check its current state.');
    }
    return data;
  }

  async _getAll(base, field, isCurrent) {
    const records = [];
    const warnings = [];
    const assets = new Map();
    const cursors = new Set();
    let continuation = null;
    let url = base;
    while (isCurrent()) {
      const data = await this._request('GET', url);
      if (!isCurrent()) return null;
      for (const [hash, url] of Object.entries(data.assets ?? {})) assets.set(hash, url);
      const lists = field === 'database' ? [data.schema, data.rows]
        : [field === 'page' && !Array.isArray(data.page) ? data.page?.items : data[field]];
      for (const items of lists) {
        if (items !== null && !Array.isArray(items)) {
          throw new Error('The notebook returned invalid historical content.');
        }
        for (const r of items ?? []) {
          records.push({ ...r, id: r.ref?.id ?? r.id });
        }
      }
      if (data.continuation) {
        const part = data.continuation;
        if (part.part === 0) continuation = { record: part.record, next: 0, parts: part.parts, data: '' };
        if (!continuation || continuation.record !== part.record || continuation.next !== part.part || continuation.parts !== part.parts || typeof part.data !== 'string') {
          throw new Error('The notebook returned an invalid historical continuation.');
        }
        continuation.data += part.data;
        continuation.next++;
        if (continuation.next === continuation.parts) {
          const r = JSON.parse(continuation.data);
          if ((r.ref?.id ?? r.id) !== part.record) throw new Error('The historical continuation changed identity.');
          records.push({ ...r, id: r.ref?.id ?? r.id });
          continuation = null;
        }
      }
      for (const w of Array.isArray(data.warnings) ? data.warnings : []) warnings.push(w);
      if (data.complete !== false) {
        if (continuation) throw new Error('The historical continuation ended early.');
        for (const record of records) {
          if (record.content?.kind === 'image') record.assetURL = assets.get(record.content.asset);
        }
        return { records, warnings };
      }
      if (!data.next || cursors.has(data.next)) {
        throw new Error('This revision could not be read to the end. Open it again to retry.');
      }
      cursors.add(data.next);
      // A content cursor already binds its selectors; repeating them is invalid.
      url = `${base.split('?')[0]}?cursor=${encodeURIComponent(data.next)}`;
    }
    return null;
  }

  async _loadRevisions(more) {
    if (this._loading) return;
    const gen = this._gen;
    this._loading = more ? 'more' : 'list';
    this._error = '';
    if (!more) {
      this._revisions = [];
      this._next = null;
    }
    this.render();
    const before = more && this._next != null ? `&before=${encodeURIComponent(this._next)}` : '';
    try {
      const data = await this._request('GET', `${API}/revisions?limit=${PAGE_SIZE}${before}`);
      if (gen !== this._gen) return;
      if (data.revisions !== null && !Array.isArray(data.revisions)) {
        throw new Error('The notebook returned an invalid revision list.');
      }
      const list = data.revisions ?? [];
      this._revisions = more ? this._revisions.concat(list) : list;
      this._next = data.next > 0 ? data.next : null;
    } catch (e) {
      if (gen !== this._gen) return;
      this._error = e.message;
    }
    this._loading = false;
    if (more) this._focusKey = this._next != null ? 'more' : null;
    this.render();
  }

  async _openRevision(n) {
    const view = { revision: n, tree: [], warnings: [], loading: true, preview: null };
    this._view = view;
    this._confirm = null;
    this._error = '';
    this._status = '';
    this._focusKey = 'back';
    this.render();
    try {
      const result = await this._getAll(`${API}/revisions/${encodeURIComponent(n)}`, 'tree', () => this._view === view);
      if (this._view !== view) return;
      const { records, warnings } = result;
      view.tree = records.filter((r) => r && r.kind === 'page');
      view.warnings = warnings;
    } catch (e) {
      if (this._view !== view) return;
      this._error = e.message;
    }
    view.loading = false;
    this.render();
  }

  _backToList() {
    const n = this._view?.revision;
    this._view = null;
    this._confirm = null;
    this._error = '';
    this._focusKey = n != null ? `open-${n}` : null;
    this.render();
  }

  async _openPage(page) {
    const view = this._view;
    if (!view) return;
    const preview = { page: page.id, title: page.title, records: [], warnings: [], loading: true, error: false };
    view.preview = preview;
    this._error = '';
    this._focusKey = 'preview';
    this.render();
    try {
      const url = `${API}/revisions/${encodeURIComponent(view.revision)}?page=${encodeURIComponent(page.id)}`;
      const result = await this._getAll(url, 'page', () => this._view === view && view.preview === preview);
      if (this._view !== view || view.preview !== preview) return;
      const { records, warnings } = result;
      preview.records = records.filter(Boolean);
      preview.warnings = warnings;
    } catch (e) {
      if (this._view !== view || view.preview !== preview) return;
      preview.error = true;
      this._error = e.message;
    }
    preview.loading = false;
    this.render();
  }

  async _openDatabase(database) {
    const view = this._view;
    const preview = view?.preview;
    if (!preview) return;
    const db = { id: database.id, title: database.title, records: [], warnings: [], loading: true, error: false };
    preview.database = db;
    this._error = '';
    this._focusKey = 'database-preview';
    this.render();
    const isCurrent = () => this._view === view && view.preview === preview && preview.database === db;
    try {
      const url = `${API}/revisions/${encodeURIComponent(view.revision)}?database=${encodeURIComponent(db.id)}`;
      const result = await this._getAll(url, 'database', isCurrent);
      if (!isCurrent()) return;
      db.records = result.records;
      db.warnings = result.warnings;
    } catch (e) {
      if (!isCurrent()) return;
      db.error = true;
      this._error = e.message;
    }
    db.loading = false;
    this.render();
  }

  _existsNow(id) {
    let cur;
    try {
      cur = this._currentPages();
    } catch {
      return false;
    }
    if (cur && typeof cur.has === 'function') return cur.has(id);
    return Array.isArray(cur) && cur.includes(id);
  }

  _ask(confirm) {
    if (this._busy) return;
    this._confirm = confirm;
    this._error = '';
    this._status = '';
    this._focusKey = 'confirm';
    this.render();
  }

  _cancelConfirm() {
    const c = this._confirm;
    this._confirm = null;
    this._focusKey = c?.trigger ?? null;
    this.render();
  }

  async _confirmRestore() {
    const c = this._confirm;
    const view = this._view;
    if (this._busy || !c || c.type !== 'restore' || !view) return;
    const gen = this._gen;
    const token = this._session();
    if (typeof token !== 'string' || token === '') {
      this._confirm = null;
      this._error = 'This tab has no notebook session, so the page cannot be restored. Reload the notebook and try again.';
      this._focusKey = c.trigger;
      this.render();
      return;
    }
    this._busy = true;
    this.render();
    const req = { revision: view.revision, page: c.page, session: token };
    try {
      const result = await this._request('POST', `${API}/recover`, req);
      this._onRecovered?.({ revision: req.revision, page: req.page, root: result?.root ?? result?.refs?.root?.ref ?? null, result });
      if (gen !== this._gen) return;
      const title = titleOf(c.title);
      this._status = c.copy ? `Restored a copy of “${title}” as “${title} (restored)”.` : `Restored “${title}”.`;
    } catch (e) {
      if (gen !== this._gen) return;
      this._error = e.message;
    }
    this._busy = false;
    this._confirm = null;
    this._focusKey = c.trigger;
    this.render();
  }

  async _confirmRollback() {
    const c = this._confirm;
    if (this._busy || !c || c.type !== 'rollback') return;
    const gen = this._gen;
    this._busy = true;
    this.render();
    let done = false;
    try {
      const body = await this._request('POST', `${API}/rollback`, { revision: c.revision });
      this._onRolledBack?.(body);
      if (gen !== this._gen) return;
      const made = !body.revision_pending && body.revision > 0 ? ` as revision ${body.revision}` : '';
      this._status = `Restored the notebook to revision ${c.revision}${made}.${body.revision_pending ? ' The new revision is still being saved.' : ''}`;
      done = true;
    } catch (e) {
      if (gen !== this._gen) return;
      this._error = e.message;
    }
    this._busy = false;
    this._confirm = null;
    if (!done) {
      this._focusKey = c.trigger;
      this.render();
      return;
    }
    this._view = null;
    this._focusKey = 'close';
    await this._loadRevisions(false);
  }

  async _pin(rev) {
    if (this._busy) return;
    const gen = this._gen;
    const pinned = !rev.pinned;
    this._busy = true;
    this._error = '';
    this._status = '';
    this._focusKey = `pin-${rev.number}`;
    this.render();
    try {
      const body = await this._request('POST', `${API}/pin`, { revision: rev.number, pinned });
      if (gen !== this._gen) return;
      rev.pinned = typeof body?.pinned === 'boolean' ? body.pinned : pinned;
      this._status = `${rev.pinned ? 'Pinned' : 'Unpinned'} revision ${rev.number}.`;
    } catch (e) {
      if (gen !== this._gen) return;
      this._error = e.message;
    }
    this._busy = false;
    this.render();
  }

  render() {
    const host = this.host;
    if (!this.open) {
      host.hidden = true;
      host.replaceChildren();
      return;
    }
    const active = document.activeElement;
    const keep = active && host.contains(active) ? active.getAttribute('data-key') : null;
    host.hidden = false;
    const panel = el('section', { class: 'history', 'aria-label': 'History', 'aria-busy': this._busy || this._loading ? 'true' : 'false' },
      el('div', { class: 'history-head' },
        el('h2', {}, 'History'),
        button('×', () => this._close(), { class: 'history-close', 'aria-label': 'Close history', 'data-key': 'close' })),
      this._error ? el('p', { class: 'history-error', role: 'alert' }, this._error) : null,
      el('p', { class: 'history-status', role: 'status' }, this._status),
      this._view ? this._renderRevision(this._view) : this._renderList());
    host.replaceChildren(panel);
    const key = this._focusKey ?? keep;
    this._focusKey = null;
    if (key != null) {
      const target = [...host.querySelectorAll('[data-key]')].find((n) => n.getAttribute('data-key') === key);
      target?.focus?.();
    }
  }

  _renderConfirm(text, onConfirm) {
    return el('div', { class: 'history-confirm', role: 'group', 'aria-label': 'Confirm' },
      el('p', {}, text),
      button('Confirm', onConfirm, { class: 'history-confirm-yes', 'data-key': 'confirm', disabled: this._busy }),
      button('Cancel', () => this._cancelConfirm(), { class: 'history-confirm-no', disabled: this._busy }));
  }

  _rollbackConfirm(n) {
    const c = this._confirm;
    if (c?.type !== 'rollback' || c.revision !== n) return null;
    return this._renderConfirm(
      `Roll the whole notebook back to revision ${n}? Every page returns to how it was then, saved as a new revision, and undo history is cleared. Later revisions stay in History.`,
      () => this._confirmRollback());
  }

  _renderList() {
    const wrap = el('div', { class: 'history-list' });
    if (this._loading === 'list') {
      wrap.appendChild(el('p', { class: 'history-loading' }, 'Loading…'));
      return wrap;
    }
    if (this._revisions.length === 0) {
      if (this._error) wrap.appendChild(button('Retry', () => this._loadRevisions(false)));
      else wrap.appendChild(el('p', { class: 'history-empty' }, 'No revisions yet.'));
      return wrap;
    }
    const list = el('ol', { class: 'history-revisions' });
    for (const rev of this._revisions) list.appendChild(this._renderRevisionRow(rev));
    wrap.appendChild(list);
    if (this._loading === 'more') wrap.appendChild(el('p', { class: 'history-loading' }, 'Loading…'));
    else if (this._next != null) wrap.appendChild(button('Load more', () => this._loadRevisions(true), { class: 'history-more', 'data-key': 'more', disabled: this._busy }));
    return wrap;
  }

  _renderRevisionRow(rev) {
    const n = rev.number;
    const disabled = this._busy || Boolean(this._loading);
    const kind = KIND_LABELS[rev.kind] ?? String(rev.kind ?? '');
    const authors = Array.isArray(rev.authors) ? rev.authors.filter((a) => typeof a === 'string' && a !== '') : [];
    const pages = Array.isArray(rev.pages) ? rev.pages : [];
    return el('li', { class: `history-revision${rev.pinned ? ' pinned' : ''}`, 'data-revision': n },
      el('div', { class: 'history-revision-head' },
        el('span', { class: `history-kind kind-${cssToken(rev.kind)}` }, kind),
        el('span', { class: 'history-number' }, `Revision ${n}`),
        el('time', { datetime: rev.created, title: rev.created }, relativeTime(rev.created, this._now())),
        rev.pinned ? el('span', { class: 'history-pinned' }, 'Pinned') : null),
      authors.length ? el('p', { class: 'history-authors' }, authors.join(', ')) : null,
      typeof rev.message === 'string' && rev.message !== '' ? el('p', { class: 'history-message' }, rev.message) : null,
      pages.length ? el('ul', { class: 'history-changed', 'aria-label': 'Changed pages' },
        pages.map((p) => el('li', { class: `change-${cssToken(p.change)}` },
          el('span', { class: 'history-page-title' }, titleOf(p.title)),
          p.change ? el('span', { class: 'history-change' }, ` (${p.change})`) : null))) : null,
      el('div', { class: 'history-actions' },
        button('Open', () => this._openRevision(n), { 'aria-label': `Open revision ${n}`, 'data-key': `open-${n}`, disabled }),
        button(rev.pinned ? 'Unpin' : 'Pin', () => this._pin(rev), { 'aria-label': `${rev.pinned ? 'Unpin' : 'Pin'} revision ${n}`, 'aria-pressed': rev.pinned ? 'true' : 'false', 'data-key': `pin-${n}`, disabled }),
        button('Roll back', () => this._ask({ type: 'rollback', revision: n, trigger: `rollback-${n}` }), { 'aria-label': `Roll back to revision ${n}`, 'data-key': `rollback-${n}`, disabled })),
      this._rollbackConfirm(n));
  }

  _renderRevision(view) {
    const n = view.revision;
    const wrap = el('div', { class: 'history-revision-view', 'data-revision': n },
      el('div', { class: 'history-revision-bar' },
        button('Back to revisions', () => this._backToList(), { 'data-key': 'back', disabled: this._busy }),
        el('h3', {}, `Revision ${n}`),
        button('Roll back', () => this._ask({ type: 'rollback', revision: n, trigger: 'rollback-view' }), { 'aria-label': `Roll back to revision ${n}`, 'data-key': 'rollback-view', disabled: this._busy })),
      this._rollbackConfirm(n),
      el('p', { class: 'history-readonly' }, 'Read-only. Open a page to preview it, or restore it into the current notebook.'));
    if (view.loading) {
      wrap.appendChild(el('p', { class: 'history-loading' }, 'Loading…'));
      return wrap;
    }
    if (view.warnings.length) wrap.appendChild(el('ul', { class: 'history-warnings' }, view.warnings.map((w) => el('li', {}, warningText(w)))));
    if (view.tree.length === 0) {
      if (!this._error) wrap.appendChild(el('p', { class: 'history-empty' }, 'This revision has no pages.'));
    } else {
      const kids = childrenOf(view.tree, (p) => p.parent);
      const seen = new Set();
      const branch = (parent) => {
        const list = (kids.get(parent) ?? []).filter((p) => !seen.has(p.id));
        if (!list.length) return null;
        return el('ul', { class: 'history-tree' }, list.map((p) => {
          seen.add(p.id);
          return el('li', { class: 'history-tree-page', 'data-page': p.id }, this._renderPageRow(p), branch(p.id));
        }));
      };
      wrap.appendChild(branch(null));
    }
    if (view.preview) wrap.appendChild(this._renderPreview(view));
    return wrap;
  }

  _renderPageRow(p) {
    const title = titleOf(p.title);
    const copy = this._existsNow(p.id);
    const label = copy ? 'Restore as copy' : 'Restore page';
    const trigger = `restore-${p.id}`;
    const c = this._confirm;
    const confirming = c?.type === 'restore' && c.page === p.id;
    const text = copy
      ? `Restore “${title}” from revision ${this._view.revision} as a new page “${title} (restored)” next to the current page? The current page is not changed.`
      : `Restore “${title}” from revision ${this._view.revision} with its subpages, under its original parent if it still exists?`;
    return el('div', { class: 'history-page-row' },
      el('span', { class: 'history-page-title' }, title),
      button('Open', () => this._openPage(p), { 'aria-label': `Open page ${title}`, 'data-key': `page-${p.id}`, disabled: this._busy }),
      button(label, () => this._ask({ type: 'restore', page: p.id, title: p.title, copy, trigger }), { 'aria-label': `${label}: ${title}`, 'data-key': trigger, disabled: this._busy }),
      confirming ? this._renderConfirm(text, () => this._confirmRestore()) : null);
  }

  _renderPreview(view) {
    const pv = view.preview;
    const pageRec = pv.records.find((r) => r.kind === 'page' && r.id === pv.page);
    const title = titleOf(pageRec?.title ?? pv.title);
    const box = el('section', { class: 'history-preview', 'aria-label': `Preview of ${title}` },
      el('h3', { tabindex: '-1', 'data-key': 'preview' }, title),
      el('p', { class: 'history-readonly' }, `As of revision ${view.revision}, read-only.`));
    if (pv.loading) {
      box.appendChild(el('p', { class: 'history-loading' }, 'Loading…'));
      return box;
    }
    if (pv.error) return box;
    if (pv.warnings.length) box.appendChild(el('ul', { class: 'history-warnings' }, pv.warnings.map((w) => el('li', {}, warningText(w)))));
    const blocks = pv.records.filter((r) => r.kind === 'block' && r.page === pv.page);
    if (!blocks.length) {
      box.appendChild(el('p', { class: 'history-empty' }, 'This page is empty.'));
      return box;
    }
    const databases = pv.records.filter((r) => r.kind === 'database');
    const pageTitles = new Map(view.tree.map((p) => [p.id, p.title]));
    const kids = childrenOf(blocks, (b) => b.parent);
    const seen = new Set();
    const branch = (parent) => {
      const list = (kids.get(parent) ?? []).filter((b) => !seen.has(b.id));
      if (!list.length) return null;
      return el('ul', { class: 'history-blocks' }, list.map((b) => {
        seen.add(b.id);
        return el('li', { class: `history-block block-${cssToken(b.content?.kind)}` }, this._renderBlock(b, databases, pageTitles), branch(b.id));
      }));
    };
    box.appendChild(branch(null));
    if (pv.database) box.appendChild(this._renderDatabase(view, pv.database));
    return box;
  }

  _renderDatabase(view, db) {
    const database = db.records.find((r) => r.kind === 'database' && r.id === db.id);
    const title = titleOf(database?.title ?? db.title);
    const box = el('section', { class: 'history-database', 'aria-label': `Historical database ${title}`, 'aria-busy': String(db.loading) },
      el('h4', { tabindex: '-1', 'data-key': 'database-preview' }, title),
      el('p', { class: 'history-readonly' }, `As of revision ${view.revision}, read-only. All rows and properties; saved view filters are not applied.`));
    if (db.loading) {
      box.appendChild(el('p', { class: 'history-loading' }, 'Loading…'));
      return box;
    }
    if (db.error) return box;
    if (db.warnings.length) box.appendChild(el('ul', { class: 'history-warnings' }, db.warnings.map((w) => el('li', {}, warningText(w)))));
    const properties = db.records.filter((r) => r.kind === 'property' && r.database === db.id);
    const ordered = (database?.property_order ?? []).map((id) => properties.find((p) => p.id === id)).filter(Boolean);
    const columns = [...ordered, ...properties.filter((p) => !ordered.includes(p))].map((p) => ({ ...p, ...p.definition }));
    const schema = { options: Object.fromEntries(columns.map((p) => [p.id, db.records.filter((r) => r.kind === 'option' && r.property === p.id)])) };
    const views = db.records.filter((r) => r.kind === 'view' && r.database === db.id);
    if (views.length) box.appendChild(el('p', {}, `Saved views: ${views.map((v) => v.definition.name).join(', ')}`));
    const rows = db.records.filter((r) => r.kind === 'row' && r.database === db.id);
    box.appendChild(el('table', { 'aria-label': `${title} at revision ${view.revision}` },
      el('thead', {}, el('tr', {}, columns.map((p) => el('th', { scope: 'col' }, p.name)))),
      el('tbody', {}, rows.map((r) => el('tr', {}, columns.map((p) => el('td', {}, cellDisplay(schema, p, r.cells[p.id]?.value))))))));
    if (!rows.length) box.appendChild(el('p', { class: 'history-empty' }, 'This database has no rows.'));
    return box;
  }

  _renderBlock(b, databases, pageTitles) {
    const c = b.content ?? {};
    const runs = Array.isArray(c.text) ? c.text.map((r) => r.text).join('') : '';
    switch (c.kind) {
      case 'divider':
        return el('hr');
      case 'code':
        return el('pre', { class: 'history-code' }, el('code', {}, c.text));
      case 'image': {
        const alt = typeof c.alt === 'string' && c.alt !== '' ? c.alt : 'no description';
        const caption = (c.caption ?? []).map((r) => r.text).join('');
        const image = typeof b.assetURL === 'string' && /^\/_notebook\/a\/[0-9a-f]{64}\.(png|jpg|gif|webp)$/.test(b.assetURL) ? el('img', { src: b.assetURL, alt, loading: 'lazy' }) : el('p', {}, `Image: ${alt}`);
        return el('figure', {}, image, caption ? el('figcaption', {}, caption) : null);
      }
    }
    if (c.kind === 'database' || c.database != null) {
      const db = databases.find((d) => d.id === c.database) ?? databases.find((d) => d.block === b.id);
      const title = titleOf(db?.title);
      return el('div', {}, el('p', {}, `Database: ${title}`),
        button('Open database', () => this._openDatabase(db ?? { id: c.database, title }), {
          'aria-label': `Open database ${title}`, 'data-key': `database-${b.id}`, disabled: this._busy,
        }));
    }
    let text = runs;
    if (text === '' && typeof c.target === 'string' && c.target !== '') {
      text = pageTitles.has(c.target) ? `Page: ${titleOf(pageTitles.get(c.target))}` : `Link: ${c.target}`;
    }
    if (typeof c.checked === 'boolean') text = `${c.checked ? '[x]' : '[ ]'} ${text}`;
    return el('p', {}, text);
  }
}
