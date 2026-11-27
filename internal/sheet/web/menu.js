// Menus and dialogs (rules 20b and 21). Each one is named, takes focus when
// it opens and gives it back to the grid when it closes. Only one is open at
// a time. This file also builds the rows and columns menu, the freeze menu,
// the sort dialog and the column width dialog.
'use strict';

import { button, h, plural } from './dom.js';
import { MAX_FROZEN_COLS, MAX_FROZEN_ROWS, MAX_WIDTH, MIN_WIDTH, colName } from './protocol.js';

export class Popups {
  // host is the positioned element popups are added to; focusBack gives
  // focus to the grid.
  constructor(host, focusBack) {
    this.host = host;
    this.focusBack = focusBack;
    this.cur = null; // {el, modal, onClose}
    document.addEventListener('mousedown', (e) => {
      if (this.cur && !this.cur.modal && !this.cur.el.contains(e.target)) this.close(false);
    }, true);
  }

  get open() { return !!this.cur; }
  get modal() { return !!this.cur?.modal; }

  // show opens el near the point `at` of the viewport, or centered behind a
  // scrim when modal. focus names the element inside that takes focus.
  show(el, at, { modal = false, focus = null, onClose = null } = {}) {
    this.close(false);
    const root = modal ? h('div', { class: 'backdrop' }, el) : el;
    this.cur = { el: root, modal, onClose };
    this.host.appendChild(root);
    if (!modal && at) {
      const box = this.host.getBoundingClientRect();
      const w = el.offsetWidth || 0;
      const hgt = el.offsetHeight || 0;
      el.style.left = `${Math.max(8, Math.min(at.x - box.left, box.width - w - 8))}px`;
      el.style.top = `${Math.max(8, Math.min(at.y - box.top, box.height - hgt - 8))}px`;
    }
    el.addEventListener('keydown', (e) => this.#key(e, el));
    const first = focus || el.querySelector('button:not([disabled]), input, select, textarea');
    if (first) first.focus();
    return el;
  }

  // close removes the open popup. Focus returns to the grid unless the
  // caller moves it elsewhere.
  close(back = true) {
    const cur = this.cur;
    if (!cur) return;
    this.cur = null;
    cur.el.remove();
    cur.onClose?.();
    if (back) this.focusBack();
  }

  #key(e, el) {
    // Keys pressed inside a popup stay with it: none reaches the grid or
    // the shortcuts of the page.
    e.stopPropagation();
    if (e.isComposing || e.keyCode === 229) return;
    const items = [...el.querySelectorAll('button:not([disabled]), input, select, textarea')];
    const i = items.indexOf(document.activeElement);
    if (e.key === 'Escape') { e.preventDefault(); this.close(); return; }
    if (e.key === 'F6') { e.preventDefault(); return; }
    if (e.key === 'Tab') {
      // Focus stays inside until the popup closes.
      e.preventDefault();
      if (items.length) items[(i + (e.shiftKey ? -1 : 1) + items.length) % items.length].focus();
      return;
    }
    if (el.getAttribute('role') === 'menu' && (e.key === 'ArrowDown' || e.key === 'ArrowUp') && items.length) {
      e.preventDefault();
      items[(i + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length].focus();
    }
  }

  // menu opens a list of commands. items: {label, kbd, fn, disabled,
  // danger}, {head} for a caption, or 'hr'.
  menu(label, items, at) {
    const el = h('div', { class: 'menu', role: 'menu', 'aria-label': label },
      items.map((it) => {
        if (it === 'hr') return h('hr', { role: 'separator' });
        if (it.head) return h('div', { class: 'mhead', text: it.head });
        return h('button', { type: 'button', role: 'menuitem', class: it.danger ? 'danger' : null, disabled: it.disabled, onclick: () => { this.close(); it.fn(); } },
          it.label, it.kbd ? h('kbd', { text: it.kbd }) : null);
      }));
    return this.show(el, at);
  }

  // dialog opens a named dialog. body is its content; a modal dialog covers
  // the sheet.
  dialog(label, body, at, opts = {}) {
    const el = h('div', { class: opts.modal ? 'dialog' : `pop ${opts.class || ''}`, role: 'dialog', 'aria-label': label, 'aria-modal': opts.modal ? 'true' : null }, body);
    return this.show(el, at, opts);
  }
}

// rowsMenu lists what can be done with the selected rows and columns. act
// carries the commands out.
export function rowsMenu(model, selection, act) {
  const t = model.tab;
  const b = selection.box();
  const nr = b.r2 - b.r1 + 1;
  const nc = b.c2 - b.c1 + 1;
  const rl = plural(nr, 'row');
  const cl = plural(nc, 'column');
  const wholeRows = selection.kind === 'rows';
  const wholeCols = selection.kind === 'cols';
  const out = [];
  if (!wholeCols) {
    out.push({ head: `Rows ${b.r1 + 1}${nr > 1 ? ` to ${b.r2 + 1}` : ''}` },
      { label: `Insert ${rl} above`, fn: () => act.insertRows('before') },
      { label: `Insert ${rl} below`, fn: () => act.insertRows('after') },
      { label: `Delete ${rl}`, danger: true, fn: act.deleteRows, disabled: nr >= t.rows.length }, 'hr');
  }
  if (!wholeRows) {
    out.push({ head: `Columns ${colName(b.c1)}${nc > 1 ? ` to ${colName(b.c2)}` : ''}` },
      { label: `Insert ${cl} left`, fn: () => act.insertCols('before') },
      { label: `Insert ${cl} right`, fn: () => act.insertCols('after') },
      { label: `Delete ${cl}`, danger: true, fn: act.deleteCols, disabled: nc >= t.cols.length },
      { label: 'Column width…', fn: act.widthDialog },
      { label: 'Fit column to its content', fn: act.fit }, 'hr');
  }
  out.push(
    { label: 'Fill down', kbd: 'Ctrl+D', fn: () => act.fill('down') },
    { label: 'Fill right', kbd: 'Ctrl+R', fn: () => act.fill('right') },
    { label: 'Paste values only', kbd: 'Ctrl+Shift+V', fn: act.pasteValues }, 'hr',
    { label: 'Sort rows…', fn: act.sortDialog },
    ...freezeItems(t, b, act), 'hr',
    { label: 'Add 100 rows at the end', fn: () => act.appendRows(100) },
    { label: 'Add a column at the end', fn: () => act.appendCols(1) });
  return out;
}

export function freezeItems(t, b, act) {
  return [
    { label: `Freeze rows down to row ${b.r2 + 1}`, fn: () => act.freeze(b.r2 + 1, null), disabled: b.r2 + 1 > MAX_FROZEN_ROWS || b.r2 + 1 >= t.rows.length },
    { label: `Freeze columns up to column ${colName(b.c2)}`, fn: () => act.freeze(null, b.c2 + 1), disabled: b.c2 + 1 > MAX_FROZEN_COLS || b.c2 + 1 >= t.cols.length },
    { label: 'Unfreeze rows', fn: () => act.freeze(0, null), disabled: !t.frozenRows },
    { label: 'Unfreeze columns', fn: () => act.freeze(null, 0), disabled: !t.frozenCols },
  ];
}

// sortScope says which rows a sort from the selection reorders: the rows of
// the selection, or every row under the frozen rows when one cell (or one
// row) is selected (rule 22).
export function sortScope(model, selection) {
  const b = selection.box();
  const t = model.tab;
  if (b.r1 === b.r2) return { rows: 'all', text: `every row under ${t.frozenRows ? `the ${plural(t.frozenRows, 'frozen row')}` : 'the top'}` };
  const r1 = Math.max(b.r1, t.frozenRows);
  return { rows: { r1, r2: b.r2 }, text: `rows ${r1 + 1} to ${b.r2 + 1}` };
}

// sortDialog asks for the column and the direction. It names the rows and
// the column by id as they are when it opens: onSort(by, desc, rows) gets
// the column id and "all" or the row ids, and returns whether the sort was
// sent.
export function sortDialog(popups, model, selection, at, onSort) {
  const b = selection.box();
  const scope = sortScope(model, selection);
  const t = model.tab;
  const rows = scope.rows === 'all' ? 'all' : t.rows.slice(scope.rows.r1, scope.rows.r2 + 1);
  const first = selection.kind === 'cells' || selection.kind === 'cols' ? b.c1 : 0;
  const last = selection.kind === 'cells' && b.c2 > b.c1 ? b.c2 : model.colCount - 1;
  const end = Math.min(last, first + 701);
  const cols = t.cols.slice(first, end + 1);
  const col = h('select', { id: 'sort-col' });
  for (let c = first; c <= end; c++) {
    const o = h('option', { value: String(c), text: `Column ${colName(c)}` });
    if (c === selection.active().c) o.setAttribute('selected', '');
    col.appendChild(o);
  }
  col.value = String(Math.min(Math.max(selection.active().c, first), last));
  const dir = h('select', { id: 'sort-dir' }, h('option', { value: 'asc', text: 'Ascending: 1 to 9, then A to Z' }), h('option', { value: 'desc', text: 'Descending: Z to A, then 9 to 1' }));
  dir.value = 'asc';
  const err = h('div', { class: 'err', role: 'alert' });
  // A sort the page cannot send keeps the dialog and its choices.
  const go = () => {
    const by = cols[Number(col.value) - first];
    if (by !== undefined && onSort(by, dir.value === 'desc', rows)) { popups.close(); return; }
    err.textContent = 'The rows were not sorted. Your choices are kept here.';
  };
  popups.dialog('Sort rows', [
    h('h2', { text: 'Sort rows' }),
    h('p', { text: `Reorders ${scope.text}. Whole rows move; frozen rows stay. Blank cells come last.` }),
    h('label', { for: 'sort-col' }, 'Sort by', col),
    h('label', { for: 'sort-dir' }, 'Order', dir), err,
    h('div', { class: 'rowb' }, button('Cancel', () => popups.close(), { class: 'btn' }), button('Sort', go, { class: 'btn primary' })),
  ], at, { focus: col });
}

// widthDialog asks for a column width in pixels. onSet returns whether
// the width was sent; one the page cannot send keeps the dialog open with
// the typed number (rule 24).
export function widthDialog(popups, current, at, onSet) {
  const input = h('input', { type: 'number', id: 'col-width', min: String(MIN_WIDTH), max: String(MAX_WIDTH), step: '1', value: String(current) });
  input.value = String(current);
  const err = h('div', { class: 'err', role: 'alert' });
  const go = () => {
    const w = Math.round(Number(input.value));
    if (!(w >= MIN_WIDTH && w <= MAX_WIDTH)) { err.textContent = `Enter a width from ${MIN_WIDTH} to ${MAX_WIDTH} pixels.`; input.focus(); return; }
    if (onSet(w)) { popups.close(); return; }
    err.textContent = 'The width was not set. Your number is kept here.';
    input.focus();
  };
  const el = popups.dialog('Column width', [
    h('h2', { text: 'Column width' }),
    h('label', { for: 'col-width' }, `Width in pixels, ${MIN_WIDTH} to ${MAX_WIDTH}`, input), err,
    h('div', { class: 'rowb' }, button('Cancel', () => popups.close(), { class: 'btn' }), button('Set width', go, { class: 'btn primary' })),
  ], at, { focus: input });
  el.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target === input) { e.preventDefault(); go(); } });
}
