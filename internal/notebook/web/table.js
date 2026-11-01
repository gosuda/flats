// Inline database views: the view bar, sort and filter chips, the table
// layout and the shared row state that the kanban layout also uses.
//
// The page editor calls renderDatabase(block, el, ctx) with
//   ctx.schema   database schema: {database, properties, options}
//   ctx.views    saved views in view order
//   ctx.rows()   current rows (captured windows plus overlays)
//   ctx.onOps    submits browser operations as one action
//   ctx.presence other editors: [{label, edit: {database, row, property}}]
// and optionally ctx.loading (rows still streaming), ctx.readOnly,
// ctx.notices (view id -> removal notice) and ctx.preview (conversion
// preview request). It returns {update(ctx), stop()}.
'use strict';


import {
  h, ops, propertyOf, titleProperty, cellDisplay, parseInput, optionPicker, filterForm, sortForm,
  filterLabel, propertyMenu, viewMenu, addViewMenu, addProperty, openPopover, closePopover, defaultPreview,
  filterDefaults, defaultWidth, clampWidth, MIN_WIDTH, MAX_WIDTH, TYPE_NAMES,
} from './dbschema.js';
import { createQuery, indexSchema, matchesView, normalizeRows, normalizeSchema, normalizeView } from './query-worker.js';
import { createVirtualRows } from './virtual.js';
import { renderKanban } from './kanban.js';

export const SORTED_TABLE_NOTE = 'Rows follow this view’s sort, so dragging to reorder is off. Remove the sort to reorder.';
export const SORTED_KANBAN_NOTE = 'Cards follow this view’s sort, so reordering is off. Moving a card to another column still changes its group.';
export const HIDDEN_NOTE = 'Hidden by this view’s filter. It stays here until you leave the view.';

const EDITABLE = new Set(['title', 'text', 'number', 'date', 'url']);

export function renderDatabase(block, el, ctx) {
  const db = createDatabaseView(block, el, ctx);
  db.update(ctx);
  return db;
}

function createDatabaseView(block, el, initial) {
  let ctx = initial;
  let schema = null;
  let views = [];
  let rows = new Map();
  let order = [];
  let viewId = null;
  // Rows this tab created or edited in the open view stay visible even when
  // they stop matching (rule 11). Leaving the view forgets them.
  let kept = new Set();
  let focus = { row: null, col: 0 };
  let editing = null;
  let virtual = null;
  let tableBody = null;
  let laidOut = null;
  const kanbanLimits = new Map();
  const query = createQuery();
  const live = h('p', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
  const root = h('section', { class: 'db', 'data-block': block?.id ?? '' });
  el.replaceChildren(root);

  const readOnly = () => !!ctx.readOnly;
  const view = () => views.find((v) => v.id === viewId) || views[0];
  const announce = (text) => { live.textContent = ''; live.textContent = text; };
  const submit = (list) => { if (list.length && !readOnly()) ctx.onOps(list); };
  // Open controls may outlive a view update; apply only their change at commit.
  const writeView = (target, change) => {
    const current = views.find((v) => v.id === target.id && v.inc === target.inc);
    if (!current) return;
    const next = change(current.def);
    if (next) submit([ops.setView(schema, current, next)]);
  };
  const touched = (id) => { kept.add(id); };
  const columns = () => {
    const v = view().def;
    const list = v.visible.map((id) => propertyOf(schema, id)).filter(Boolean);
    const title = titleProperty(schema);
    if (!list.includes(title)) list.unshift(title);
    return list;
  };

  // Row signatures let an update rebuild only rows whose content changed,
  // so a remote edit elsewhere never replaces the row someone is in.
  let signatures = new Map();
  let shared = '';

  function update(next) {
    ctx = next ?? ctx;
    schema = indexSchema(normalizeSchema(ctx.schema));
    views = (ctx.views || []).map((v) => normalizeView(v));
    if (!views.some((v) => v.id === viewId)) {
      viewId = views[0]?.id ?? null;
      kept = new Set();
    }
    const fresh = normalizeRows(ctx.rows());
    rows = new Map(fresh.map((r) => [r.id, r]));
    const remaining = order.filter((id) => rows.has(id));
    if (remaining.length !== order.length) {
      order = remaining;
      virtual?.setRows(order);
    }
    for (const id of kept) if (!rows.has(id)) kept.delete(id);
    if (!views.length) {
      for (const slot of Object.values(slots)) slot.replaceChildren();
      slots.body.append(h('p', { class: 'note' }, 'This database has no views.'));
      return;
    }
    const changed = new Set(kept);
    const nextSignatures = new Map();
    for (const r of fresh) {
      const sig = r.inc + JSON.stringify(r.cells);
      nextSignatures.set(r.id, sig);
      if (signatures.get(r.id) !== sig) changed.add(r.id);
    }
    signatures = nextSignatures;
    const nextShared = JSON.stringify([ctx.presence || [], view().def, schema.options, !!ctx.readOnly]);
    const all = nextShared !== shared;
    shared = nextShared;
    query.run(schema, view().def, fresh, [...kept], (ids) => {
      order = ids.filter((id) => rows.has(id));
      paint(all ? null : changed);
    });
  }

  // ---- Chrome ----

  function selectView(id) {
    if (id === viewId) return;
    viewId = id;
    kept = new Set();
    focus = { row: null, col: 0 };
    laidOut = null;
    kanbanLimits.clear();
    update();
  }

  function viewBar() {
    const v = view();
    const tabs = h('div', { class: 'views', role: 'tablist', 'aria-label': 'Views' },
      views.map((x) => h('button', {
        type: 'button', class: 'vtab', role: 'tab', 'aria-selected': String(x === v), tabindex: x === v ? '0' : '-1',
        onclick: () => selectView(x.id),
        onkeydown: (e) => {
          const i = views.indexOf(x);
          const n = e.key === 'ArrowRight' ? views[i + 1] : e.key === 'ArrowLeft' ? views[i - 1] : null;
          if (n) { e.preventDefault(); selectView(n.id); root.querySelector('[role=tab][aria-selected=true]')?.focus(); }
        },
      }, x.def.name)));
    const tools = readOnly() ? null : h('div', { class: 'db-tools' },
      h('button', { type: 'button', class: 'vtab', 'aria-haspopup': 'dialog', onclick: (e) => openPopover(root, e.currentTarget, 'View settings', viewMenu(ctx, schema, views, v, { write: (change) => writeView(v, change), close: () => closePopover(root, true) })) }, 'View settings'),
      h('button', { type: 'button', class: 'vtab', 'aria-label': 'Add view', 'aria-haspopup': 'menu', onclick: (e) => openPopover(root, e.currentTarget, 'New view', addViewMenu(ctx, schema, views, (id) => { closePopover(root); viewId = id; kept = new Set(); })) }, '+'));
    return h('div', { class: 'db-head' }, h('span', { class: 'db-title' }, ctx.schema.title ?? ''), tabs, tools);
  }

  function chips() {
    const v = view();
    const def = v.def;
    const write = (change) => { closePopover(root); writeView(v, change); };
    const bar = h('div', { class: 'chips' });
    def.sort.forEach((s, i) => {
      const p = propertyOf(schema, s.property);
      if (!p) return;
      bar.append(h('button', {
        type: 'button', class: 'chip on', disabled: readOnly(), 'aria-haspopup': 'dialog',
        'aria-label': `Sorted by ${p.name}, ${s.direction === 'asc' ? 'ascending' : 'descending'}${readOnly() ? '' : '. Edit'}`,
        onclick: (e) => openPopover(root, e.currentTarget, 'Sort', sortForm(schema, def, i, write)),
      }, `${p.name} ${s.direction === 'asc' ? '↑' : '↓'}`));
    });
    def.filter.forEach((f) => {
      const label = filterLabel(schema, f);
      bar.append(h('button', {
        type: 'button', class: 'chip on', disabled: readOnly(), 'aria-label': `Filter ${label}${readOnly() ? '' : ', remove'}`,
        onclick: () => write((current) => {
          const at = current.filter.findIndex((x) => JSON.stringify(x) === JSON.stringify(f));
          if (at < 0) return null;
          return { ...current, filter: current.filter.filter((_, j) => j !== at) };
        }),
      }, label));
    });
    if (!readOnly()) {
      bar.append(
        h('button', { type: 'button', class: 'chip', 'aria-haspopup': 'dialog', onclick: (e) => openPopover(root, e.currentTarget, 'Add filter', filterForm(schema, write)) }, 'Add filter'),
        h('button', { type: 'button', class: 'chip', 'aria-haspopup': 'dialog', onclick: (e) => openPopover(root, e.currentTarget, 'Sort', sortForm(schema, def, -1, write)) }, def.sort.length ? 'Add sort' : 'Sort'));
    }
    return bar;
  }

  // The root keeps fixed slots so a repaint never detaches the table, which
  // would drop focus from a cell or an open editor.
  const slots = {
    head: h('div', {}), notice: h('div', {}), chips: h('div', {}), loading: h('div', {}),
    body: h('div', { class: 'db-body' }), foot: h('div', {}),
  };
  root.append(slots.head, slots.notice, slots.chips, slots.loading, slots.body, slots.foot, live);

  // The view bar, chips and footer are rebuilt only when what they show
  // changed, so a focused tab, chip or button survives a row change.
  let chrome = '';

  function paint(changed = null) {
    if (!views.length) return;
    const v = view();
    const sig = JSON.stringify([v.id, v.inc, views.map((x) => [x.id, x.def.name]), v.def, readOnly(), ctx.schema.title ?? '', schema.properties, schema.options]);
    const rebuild = sig !== chrome;
    chrome = sig;
    if (rebuild) slots.head.replaceChildren(viewBar());
    const notice = ctx.notices?.get?.(v.id);
    slots.notice.replaceChildren(...(notice ? [h('div', { class: 'banner info', role: 'status' }, notice)] : []));
    if (rebuild) slots.chips.replaceChildren(chips());
    slots.loading.replaceChildren(...(ctx.loading ? [h('p', { class: 'note db-loading', role: 'status' }, 'Loading rows. The order may change until every row has arrived.')] : []));
    const foot = [];
    if (!readOnly()) {
      if (v.def.sort.length) foot.push(h('p', { class: 'note db-sorted' }, v.def.layout === 'kanban' ? SORTED_KANBAN_NOTE : SORTED_TABLE_NOTE));
      else foot.push(h('p', { class: 'note' }, `All views share one row order. ${v.def.layout === 'kanban' ? 'Drag a card, or press Alt and an arrow key, to reorder.' : 'Drag a row by its handle, or press Alt and an arrow key, to reorder.'}`));
    }
    if (v.def.layout === 'kanban' && propertyOf(schema, v.def.groupBy)) {
      if (rebuild) slots.foot.replaceChildren(...foot);
      virtual = null;
      tableBody = null;
      laidOut = null;
      const hadFocus = slots.body.contains(document.activeElement);
      renderKanban(slots.body, {
        schema, view: v, rows, order, kept, readOnly: readOnly(), onOps: submit, announce, touched,
        limits: kanbanLimits, focus: focus.row, setFocus: (id) => { focus.row = id; },
        focusCard: (id) => { focus.row = id; paint(); slots.body.querySelector(`.card[data-row=${id}]`)?.focus(); },
        repaint: () => paint(), createRow,
      });
      if (hadFocus && focus.row) slots.body.querySelector(`.card[data-row=${focus.row}]`)?.focus();
      return;
    }
    if (!readOnly()) foot.push(h('button', { type: 'button', class: 'addrow', onclick: () => createRow({}) }, 'New row'));
    if (rebuild) slots.foot.replaceChildren(...foot);
    const hadFocus = slots.body.contains(document.activeElement);
    const shape = tableShape(v);
    if (!laidOut || laidOut.shape !== shape) {
      slots.body.replaceChildren(tableView(v, shape));
    }
    virtual.setRows(order, changed, editing != null);
    if (hadFocus && !slots.body.contains(document.activeElement)) restoreFocus();
  }

  function tableShape(v) {
    return [v.id, v.def.sort.length > 0, readOnly(), ...columns().map((p) => `${p.id}:${p.name}:${p.type}:${p.format}:${v.def.widths[p.id] ?? ''}`)].join('|');
  }

  // ---- Table ----

  function tableView(v, shape) {
    const cols = columns();
    const sorted = v.def.sort.length > 0;
    const headRow = h('tr', { 'aria-rowindex': '1' },
      h('th', { scope: 'col', class: 'handle-col' }, h('span', { class: 'sr-only' }, 'Row actions')),
      cols.map((p, ci) => headerCell(v, p, ci)),
      readOnly() ? null : h('th', { scope: 'col', class: 'add-col' }, h('button', { type: 'button', 'aria-label': 'Add property', onclick: () => addProperty(ctx, schema, view()) }, '+')));
    tableBody = h('tbody');
    const table = h('table', { class: 'dbt', 'aria-label': `${ctx.schema.title || 'Database'}, ${v.def.name}`, 'aria-rowcount': String(order.length + 1), 'aria-colcount': String(cols.length + 1) },
      h('colgroup', {}, h('col', { class: 'handle-col' }), cols.map((p) => {
        const col = h('col', { 'data-prop': p.id });
        col.style.width = `${v.def.widths[p.id] ?? defaultWidth(p)}px`;
        return col;
      })),
      h('thead', {}, headRow), tableBody);
    const wrap = h('div', { class: 'tablewrap', tabindex: '-1' }, table);
    virtual = createVirtualRows({
      scroller: wrap,
      body: tableBody,
      renderRow: (id) => rowElement(v, cols, rows.get(id), sorted),
      spacer: () => h('tr', { class: 'spacer' }, h('td', { colspan: String(cols.length + 2) })),
      onCount: (n) => table.setAttribute('aria-rowcount', String(n + 1)),
    });
    laidOut = { shape, wrap };
    return wrap;
  }

  function headerCell(v, p, ci) {
    const s = v.def.sort.find((x) => x.property === p.id);
    const width = v.def.widths[p.id] ?? defaultWidth(p);
    const th = h('th', { scope: 'col', 'data-prop': p.id, 'aria-colindex': String(ci + 2), 'aria-sort': s ? (s.direction === 'asc' ? 'ascending' : 'descending') : null },
      readOnly()
        ? h('span', {}, p.name)
        : h('button', { type: 'button', class: 'th-name', 'aria-haspopup': 'dialog', 'aria-label': `${p.name}, ${TYPE_NAMES[p.type]}. Property settings`, onclick: (e) => openPopover(root, e.currentTarget, `${p.name} settings`, propertyMenu({ ...ctx, preview: ctx.preview || defaultPreview }, schema, p, { close: () => closePopover(root, true) })) }, p.name),
      readOnly() ? null : resizeHandle(v, p, width));
    return th;
  }

  // Resizing changes the column locally while the pointer moves and writes
  // the whole view definition once, on release or on Enter.
  function resizeHandle(v, p, start) {
    let px = start;
    let committed = start;
    const grip = h('span', {
      class: 'col-resize', role: 'separator', tabindex: '0', 'aria-orientation': 'vertical',
      'aria-label': `Resize ${p.name}`, 'aria-valuemin': String(MIN_WIDTH), 'aria-valuemax': String(MAX_WIDTH), 'aria-valuenow': String(start),
    });
    const show = (w) => {
      px = clampWidth(w);
      grip.setAttribute('aria-valuenow', String(px));
      const col = laidOut?.wrap.querySelector(`col[data-prop=${p.id}]`);
      if (col) col.style.width = `${px}px`;
    };
    const commit = () => {
      if (px === committed) return;
      committed = px;
      writeView(v, (current) => current.layout === 'table'
        ? { ...current, widths: { ...current.widths, [p.id]: px } }
        : null);
    };
    grip.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      const x0 = e.clientX;
      const w0 = px;
      const moveH = (m) => show(w0 + m.clientX - x0);
      const up = () => {
        document.removeEventListener('pointermove', moveH);
        document.removeEventListener('pointerup', up);
        commit();
      };
      document.addEventListener('pointermove', moveH);
      document.addEventListener('pointerup', up);
    });
    grip.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') { e.preventDefault(); show(px + (e.key === 'ArrowRight' ? 10 : -10)); }
      else if (e.key === 'Enter') { e.preventDefault(); commit(); }
      else if (e.key === 'Escape') { e.preventDefault(); show(committed); }
    });
    grip.addEventListener('blur', commit);
    return grip;
  }

  function rowElement(v, cols, r, sorted) {
    const hidden = kept.has(r.id) && !matchesView(schema, v.def, r);
    const tr = h('tr', { 'data-row': r.id, class: hidden ? 'hidden-by-filter' : null });
    const handle = h('button', {
      type: 'button', class: 'row-handle', tabindex: '-1', 'aria-haspopup': 'menu',
      'aria-label': `Row ${r.cells[titleProperty(schema).id] || 'Untitled'} actions${sorted ? '' : '. Drag to reorder'}`,
      draggable: !readOnly() && !sorted ? 'true' : null,
      title: sorted ? SORTED_TABLE_NOTE : null,
      onclick: (e) => openPopover(root, e.currentTarget, 'Row actions', rowMenu(r)),
    }, '⋮');
    tr.append(h('td', { class: 'handle-col', 'aria-colindex': '1' }, readOnly() ? null : handle));
    if (!readOnly()) {
      handle.addEventListener('dragstart', (e) => {
        if (sorted) { e.preventDefault(); announce(SORTED_TABLE_NOTE); return; }
        e.dataTransfer?.setData('text/x-notebook-row', r.id);
      });
      tr.addEventListener('dragover', (e) => { if (!sorted) e.preventDefault(); });
      tr.addEventListener('drop', (e) => {
        e.preventDefault();
        if (sorted) { announce(SORTED_TABLE_NOTE); return; }
        const moved = rows.get(e.dataTransfer?.getData('text/x-notebook-row'));
        if (moved && moved.id !== r.id) moveRow(moved, r);
      });
    }
    cols.forEach((p, ci) => { tr.append(cellElement(v, r, p, ci, hidden)); });
    return tr;
  }

  function rowMenu(r) {
    return h('div', { class: 'db-menu', role: 'menu' },
      h('button', { type: 'button', role: 'menuitem', class: 'mi', onclick: () => { closePopover(root); createRow({}, r); } }, 'Insert row above'),
      h('button', { type: 'button', role: 'menuitem', class: 'mi danger', onclick: () => { closePopover(root); submit([ops.deleteRows([r])]); announce('Row deleted.'); } }, 'Delete row'));
  }

  function presenceFor(r, p) {
    return (ctx.presence || []).filter((e) => e.edit && e.edit.row?.id === r.id && e.edit.property?.id === p.id).map((e) => e.label);
  }

  function cellElement(v, r, p, ci, hidden) {
    const value = r.cells[p.id];
    const others = presenceFor(r, p);
    const td = h('td', {
      'data-prop': p.id, 'aria-colindex': String(ci + 2), tabindex: tabStop(r.id, ci) ? '0' : '-1',
      class: [p.type === 'number' ? 'num' : '', p.type === 'title' ? 'title' : '', others.length ? 'has-presence' : ''].filter(Boolean).join(' ') || null,
    });
    if (p.type === 'checkbox') {
      td.append(h('input', {
        type: 'checkbox', 'aria-label': p.name, checked: value === true, disabled: readOnly(), tabindex: '-1',
        onchange: (e) => setCell(r, p, e.target.checked),
      }));
    } else {
      const shown = cellDisplay(schema, p, value);
      td.append(shown == null ? h('span', { class: 'empty' }, 'Empty') : shown);
    }
    if (p.type === 'title' && hidden) td.append(h('span', { class: 'note' }, HIDDEN_NOTE));
    if (others.length) td.append(h('span', { class: 'presence' }, `${others.join(', ')} editing`));
    td.addEventListener('focus', () => { focus = { row: r.id, col: ci }; if (virtual) virtual.focused = r.id; });
    td.addEventListener('keydown', (e) => cellKey(e, r, p, ci, td));
    td.addEventListener('dblclick', () => startEdit(r, p, td));
    td.addEventListener('click', () => {
      if (p.type === 'select' || p.type === 'multi_select') startEdit(r, p, td);
    });
    return td;
  }

  // One cell is the table's tab stop: the focused one, else the first.
  function tabStop(id, ci) {
    const current = focus.row != null && virtual?.indexOf(focus.row) >= 0;
    return current ? focus.row === id && focus.col === ci : ci === 0 && virtual?.ids[0] === id;
  }

  function setCell(r, p, value) {
    touched(r.id);
    submit([ops.setCell(schema, r, p, value)]);
  }

  function startEdit(r, p, td) {
    if (readOnly()) return;
    // Updates rebuild the schema, so p may be an older copy: match by id.
    focus = { row: r.id, col: columns().findIndex((c) => c.id === p.id) };
    if (virtual) virtual.focused = r.id;
    if (p.type === 'checkbox') { setCell(r, p, r.cells[p.id] !== true); return; }
    if (p.type === 'select' || p.type === 'multi_select') {
      openPopover(root, td, p.name, optionPicker({ ...ctx, onOps: (list) => { touched(r.id); submit(list); } }, schema, p, r, r.cells[p.id], () => closePopover(root, true)));
      return;
    }
    if (!EDITABLE.has(p.type)) return;
    const value = r.cells[p.id];
    const input = h('input', { type: 'text', class: 'cell-input', 'aria-label': p.name, value: value == null ? '' : String(value), inputmode: p.type === 'number' ? 'decimal' : null });
    const error = h('span', { class: 'db-error', role: 'alert' });
    editing = { row: r.id, prop: p.id };
    let done = false;
    const finish = (save) => {
      if (done) return true;
      if (save) {
        const got = parseInput(p.type, input.value);
        if (got.error) {
          input.setAttribute('aria-invalid', 'true');
          error.textContent = got.error;
          return false;
        }
        const same = (got.value ?? null) === (value ?? (p.type === 'title' || p.type === 'text' ? '' : null));
        if (!same) setCell(r, p, got.value);
      }
      done = true;
      editing = null;
      virtual?.rerender(new Set([r.id]), false);
      restoreFocus();
      return true;
    };
    input.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') { e.preventDefault(); finish(true); }
      else if (e.key === 'Escape') { e.preventDefault(); finish(false); }
    });
    input.addEventListener('blur', () => { if (!finish(true)) finish(false); });
    td.replaceChildren(input, error);
    input.focus();
  }

  function cellKey(e, r, p, ci, td) {
    const cols = columns();
    const i = virtual.indexOf(r.id);
    const go = (row, col) => {
      if (row < 0 || row >= virtual.ids.length) return;
      focus = { row: virtual.ids[row], col: Math.max(0, Math.min(cols.length - 1, col)) };
      virtual.reveal(row);
      restoreFocus();
    };
    if (e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
      e.preventDefault();
      if (readOnly()) return;
      if (view().def.sort.length) { announce(SORTED_TABLE_NOTE); return; }
      const ids = virtual.ids;
      if (e.key === 'ArrowUp' && i > 0) moveRow(r, rows.get(ids[i - 1]));
      if (e.key === 'ArrowDown' && i < ids.length - 1) moveRow(r, ids[i + 2] ? rows.get(ids[i + 2]) : null);
      return;
    }
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); go(i + 1, ci); break;
      case 'ArrowUp': e.preventDefault(); go(i - 1, ci); break;
      case 'ArrowRight': e.preventDefault(); go(i, ci + 1); break;
      case 'ArrowLeft': e.preventDefault(); go(i, ci - 1); break;
      case 'Home': e.preventDefault(); go(e.ctrlKey ? 0 : i, e.ctrlKey ? ci : 0); break;
      case 'End': e.preventDefault(); go(e.ctrlKey ? virtual.ids.length - 1 : i, e.ctrlKey ? ci : cols.length - 1); break;
      case 'PageDown': e.preventDefault(); go(Math.min(virtual.ids.length - 1, i + 10), ci); break;
      case 'PageUp': e.preventDefault(); go(Math.max(0, i - 10), ci); break;
      case 'Enter': case 'F2': e.preventDefault(); startEdit(r, p, td); break;
      case ' ': if (p.type === 'checkbox') { e.preventDefault(); startEdit(r, p, td); } break;
      case 'Delete':
        if (e.shiftKey && !readOnly()) { e.preventDefault(); submit([ops.deleteRows([r])]); announce('Row deleted.'); }
        break;
    }
  }

  function moveRow(r, before) {
    touched(r.id);
    focus.row = r.id;
    if (virtual) virtual.focused = r.id;
    submit([ops.moveRows([r], before)]);
  }

  function restoreFocus() {
    if (!virtual || focus.row == null) return;
    const i = virtual.indexOf(focus.row);
    if (i < 0) return;
    const tr = virtual.element(focus.row) || virtual.reveal(i);
    const td = tr?.querySelectorAll('td')[focus.col + 1];
    if (!td) return;
    for (const other of tableBody.querySelectorAll('td[tabindex="0"]')) if (other !== td) other.setAttribute('tabindex', '-1');
    td.setAttribute('tabindex', '0');
    td.focus();
  }

  // createRow adds a row with the view's matching defaults; the row stays
  // visible to its creator even if those defaults cannot make it match.
  function createRow(cells, before = null) {
    if (readOnly()) return null;
    const v = view();
    const values = { ...filterDefaults(schema, v.def), ...cells };
    const { op, id } = ops.createRow(schema, v, values, before);
    kept.add(id);
    focus = { row: id, col: 0 };
    if (virtual) virtual.focused = id;
    submit([op]);
    return id;
  }

  return {
    update,
    stop() { query.stop(); closePopover(root); },
    get view() { return view(); },
    get order() { return order; },
    get kept() { return new Set(kept); },
    get editing() { return editing; },
    selectView,
    createRow,
    focusRow(id, col = 0) { focus = { row: id, col }; virtual?.reveal(virtual.indexOf(id)); restoreFocus(); },
    root,
  };
}

