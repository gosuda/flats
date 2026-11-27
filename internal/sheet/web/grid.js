// The grid (rules 16 and 20b, TECH 19.3 and 19.4). One scroll container
// holds a sized box with the full height and width of the tab. Each drawn
// row is one element placed by offset; it holds its drawn cells. The column
// letters and the frozen rows sit in a block that sticks to the top, and
// each row's number and frozen cells in a block that sticks to the left, so
// a sheet row is always one DOM row. Rows and cells are recycled while
// scrolling. Cell text is always set as text.
'use strict';

import { cellLabel } from './a11y.js';
import { h } from './dom.js';
import { inRuns } from './presence.js';
import { colName } from './protocol.js';
import { HEAD_HEIGHT, HEAD_WIDTH, Heights, OVERSCAN_COLS, OVERSCAN_ROWS, Offsets, ROW_HEIGHT, revealScroll, windowOf } from './virtual.js';

const PAD = 12;       // left and right padding of a cell's text
const END_SPACE = 48; // room after the last row and column
const RESIZE_GRAB = 5;
const PEOPLE_COLORS = 6;
const DRAG_SCROLL_EVERY = 30; // milliseconds between steps while a drag holds the pointer past an edge
const DRAG_SCROLL_MAX = 48;   // pixels one step scrolls at most
const PROBES_MAX = 2000;      // cells outside the drawn columns measured in one paint

// browserMeasure measures text with a canvas and heights from layout. Tests
// pass their own.
export function browserMeasure() {
  let ctx = null;
  let font = '';
  const cache = new Map();
  return {
    text(s, bold) {
      if (!ctx) {
        ctx = document.createElement('canvas').getContext('2d');
        const cs = getComputedStyle(document.body);
        font = `${cs.fontSize} ${cs.fontFamily}`;
      }
      const k = (bold ? 'b' : 'n') + s;
      let w = cache.get(k);
      if (w === undefined) {
        ctx.font = (bold ? '700 ' : '') + font;
        w = ctx.measureText(s).width;
        if (cache.size > 20000) cache.clear();
        cache.set(k, w);
      }
      return w;
    },
    height(el) { return el.offsetHeight; },
  };
}

// nextFrame runs fn on the next frame, or shortly after when the browser
// holds frames back, as it does for a page that is not visible.
function nextFrame(fn) {
  let ran = false;
  const run = () => { if (!ran) { ran = true; fn(); } };
  requestAnimationFrame(run);
  setTimeout(run, 100);
}

const generalAlign = { n: 'num', b: 'ctr', e: 'ctr', t: '', '': '' };
const alignClass = { left: 'lft', center: 'ctr', right: 'num' };

export class Grid {
  // handlers: down(hit, e), drag(hit, e), up(e), dbl(hit, e), context(hit, e),
  // label(text) when the active cell's spoken name changes.
  constructor({ model, selection, measure = browserMeasure(), handlers = {}, raf = nextFrame, timers = globalThis }) {
    this.timers = timers;
    this.dragTimer = null;
    this.model = model;
    this.selection = selection;
    this.measure = measure;
    this.h = handlers;
    this.raf = raf;
    this.heights = new Heights(0);
    this.cols = new Offsets([]);
    this.measured = new Map(); // row id -> height of a row taller than one line
    // A row's height is that of its tallest cell, drawn or not, so it does
    // not depend on which columns the viewport shows (rule 7).
    this.tall = new Map();     // row id -> Map(col id -> height its text needs) for cells that wrap or hold line breaks
    this.scanned = new Set();  // row ids whose cells outside the drawn columns are measured
    this.probes = [];          // hidden cells that measure text outside the drawn columns
    this.rowEls = new Map();   // row id -> row element
    this.pool = [];
    this.overlays = { people: [], hits: null, flash: null, fill: null, handle: true };
    this.editing = false;
    this.scheduled = false;

    this.input = h('textarea', { class: 'cell-in', rows: '1', spellcheck: 'false', autocomplete: 'off', autocapitalize: 'off', 'aria-autocomplete': 'list' });
    this.activeEl = this.#newCell();
    this.activeEl.appendChild(this.input);
    this.corner = h('div', { class: 'rh gcorner', role: 'columnheader', 'aria-colindex': '1', 'aria-label': 'Select every cell' });
    this.headRow = this.#newRow(this.corner);
    this.headRow.classList.add('ghead');
    this.headRow.setAttribute('aria-rowindex', '1');
    this.headRow.style.height = `${HEAD_HEIGHT}px`;
    this.top = h('div', { class: 'gtop', role: 'rowgroup' }, this.headRow);
    this.body = h('div', { class: 'gbody', role: 'rowgroup' });
    this.probeBox = h('div', { class: 'gprobe', role: 'none', 'aria-hidden': 'true' });
    this.size = h('div', { class: 'gsize', role: 'none' }, this.top, this.body, this.probeBox);
    this.el = h('div', { class: 'gridwrap', role: 'grid', 'aria-describedby': 'grid-keys', 'aria-multiselectable': 'true' }, this.size);

    this.el.addEventListener('scroll', () => this.schedule());
    this.el.addEventListener('mousedown', (e) => this.#down(e));
    this.el.addEventListener('dblclick', (e) => { const hit = this.hit(e.clientX, e.clientY); if (hit) this.h.dbl?.(hit, e); });
    this.el.addEventListener('contextmenu', (e) => { const hit = this.hit(e.clientX, e.clientY); if (hit) this.h.context?.(hit, e); });
  }

  // show swaps the tab the grid draws: the live tab, or a revision's.
  show(model, selection) {
    this.model = model;
    this.selection = selection;
    this.measured.clear();
    this.tall.clear();
    this.scanned.clear();
    this.layout();
  }

  // layout rebuilds the axes after the rows, columns or widths changed.
  // Every row is measured again when it is next drawn; until then it keeps
  // the height it had.
  layout() {
    const t = this.model.tab;
    this.scanned.clear();
    if (!t) { this.heights.reset(0); this.cols.set([]); this.measured.clear(); this.tall.clear(); return; }
    for (const kept of [this.measured, this.tall]) for (const id of kept.keys()) if (this.model.rowIndex(id) < 0) kept.delete(id);
    this.heights.reset(t.rows.length, ROW_HEIGHT, (i) => this.measured.get(t.rows[i]));
    this.cols.set(t.cols.map((id) => this.model.widthOf(id)));
  }

  // widthsChanged rebuilds the column offsets from the model's widths. Row
  // heights stay until each row is drawn and measured again at the new
  // widths.
  widthsChanged() {
    const t = this.model.tab;
    if (!t) return;
    this.scanned.clear();
    this.cols.set(t.cols.map((id) => this.model.widthOf(id)));
  }

  // rowsChanged tells the grid that the content or the format of cells in
  // these rows changed, so their heights are measured again when drawn.
  rowsChanged(rows) { for (const id of rows) this.scanned.delete(id); }

  schedule() {
    if (this.scheduled) return;
    this.scheduled = true;
    this.raf(() => { this.scheduled = false; this.paint(); });
  }

  focus() { if (document.activeElement !== this.input) this.input.focus({ preventScroll: true }); }
  get focused() { return document.activeElement === this.input; }

  // ---- geometry ----

  #frozen() {
    const t = this.model.tab;
    const fr = Math.min(t.frozenRows, t.rows.length);
    const fc = Math.min(t.frozenCols, t.cols.length);
    return { fr, fc, fh: this.heights.offset(fr), fw: this.cols.offset(fc) };
  }

  #view() {
    const f = this.#frozen();
    return { ...f, vh: Math.max(0, this.el.clientHeight - HEAD_HEIGHT - f.fh), vw: Math.max(0, this.el.clientWidth - HEAD_WIDTH - f.fw) };
  }

  // windows are the row and column ranges to draw besides the frozen ones.
  windows() {
    const t = this.model.tab;
    const v = this.#view();
    return {
      ...v,
      rows: windowOf(this.heights, t.rows.length, v.fr, this.el.scrollTop, v.vh, OVERSCAN_ROWS),
      cols: windowOf(this.cols, t.cols.length, v.fc, this.el.scrollLeft, v.vw, OVERSCAN_COLS),
    };
  }

  // rowsPerPage is how many rows Page Up and Page Down move.
  rowsPerPage() { return Math.max(1, Math.floor(this.#view().vh / ROW_HEIGHT) - 1); }

  // reveal scrolls just far enough to show cell (r, c).
  reveal(r, c) {
    if (!this.model.tab) return;
    const v = this.#view();
    this.el.scrollTop = revealScroll(this.heights, r, v.fr, this.el.scrollTop, v.vh);
    this.el.scrollLeft = revealScroll(this.cols, c, v.fc, this.el.scrollLeft, v.vw);
  }

  // hit finds what lies under a point of the viewport: {zone, r, c} with
  // zone cell, row, col, corner or resize (the right edge of a letter).
  hit(clientX, clientY) {
    const t = this.model.tab;
    if (!t || !t.rows.length || !t.cols.length) return null;
    const box = this.el.getBoundingClientRect();
    const x = clientX - box.left - HEAD_WIDTH;
    const y = clientY - box.top - HEAD_HEIGHT;
    const f = this.#frozen();
    const cx = x < f.fw ? x : x + this.el.scrollLeft;
    const cy = y < f.fh ? y : y + this.el.scrollTop;
    const c = this.cols.indexAt(Math.max(0, cx));
    const r = this.heights.indexAt(Math.max(0, cy));
    if (x < 0 && y < 0) return { zone: 'corner', r: 0, c: 0 };
    if (y < 0) {
      for (const i of [c, c - 1]) {
        if (i >= 0 && Math.abs(this.cols.offset(i + 1) - cx) <= RESIZE_GRAB) return { zone: 'resize', r: 0, c: i };
      }
      return { zone: 'col', r: 0, c };
    }
    if (x < 0) return { zone: 'row', r, c: 0 };
    return { zone: 'cell', r, c };
  }

  // cellRect is where cell (r, c) lies in the viewport, for menus and hints.
  cellRect(r, c) {
    const box = this.el.getBoundingClientRect();
    const f = this.#frozen();
    const left = box.left + HEAD_WIDTH + this.cols.offset(c) - (c < f.fc ? 0 : this.el.scrollLeft);
    const top = box.top + HEAD_HEIGHT + this.heights.offset(r) - (r < f.fr ? 0 : this.el.scrollTop);
    return { left, top, width: this.cols.size(c), height: this.heights.get(r), right: left + this.cols.size(c), bottom: top + this.heights.get(r) };
  }

  #down(e) {
    if (e.button) return;
    const hit = this.hit(e.clientX, e.clientY);
    if (!hit) return;
    if (e.target.classList.contains('handle')) hit.zone = 'handle';
    if (e.target === this.input && this.editing) return; // a click inside the open editor moves its caret
    this.h.down?.(hit, e);
    // A drag from a cell scrolls both ways, one from a row number up and
    // down, one from a column letter sideways; a resize scrolls nothing.
    const cells = hit.zone === 'cell' || hit.zone === 'handle';
    const axes = { x: cells || hit.zone === 'col', y: cells || hit.zone === 'row' };
    const move = (ev) => {
      const at = this.hit(ev.clientX, ev.clientY);
      if (at) this.h.drag?.(at, ev);
      this.#dragScroll(ev, axes);
    };
    const up = (ev) => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      this.timers.clearTimeout(this.dragTimer);
      this.dragTimer = null;
      this.h.up?.(ev);
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  }

  // #dragScroll scrolls while a drag holds the pointer past an edge of the
  // cells, and extends the drag to the cell that comes under that edge. The
  // further out the pointer is, the faster the grid scrolls. It goes on
  // without the pointer moving, until the grid ends or the button is let go.
  #dragScroll(ev, axes) {
    this.timers.clearTimeout(this.dragTimer);
    this.dragTimer = null;
    const box = this.el.getBoundingClientRect();
    const f = this.#frozen();
    const x0 = box.left + HEAD_WIDTH + f.fw;
    const x1 = box.left + this.el.clientWidth;
    const y0 = box.top + HEAD_HEIGHT + f.fh;
    const y1 = box.top + this.el.clientHeight;
    const past = (v, lo, hi) => (v < lo ? v - lo : v > hi ? v - hi : 0);
    const step = (d) => Math.sign(d) * Math.min(DRAG_SCROLL_MAX, 8 + Math.abs(d) / 2);
    const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
    const dx = axes.x ? past(ev.clientX, x0, x1) : 0;
    const dy = axes.y ? past(ev.clientY, y0, y1) : 0;
    const left = clamp(this.el.scrollLeft + step(dx), 0, Math.max(0, HEAD_WIDTH + this.cols.total + END_SPACE - this.el.clientWidth));
    const top = clamp(this.el.scrollTop + step(dy), 0, Math.max(0, HEAD_HEIGHT + this.heights.total + END_SPACE - this.el.clientHeight));
    if (left === this.el.scrollLeft && top === this.el.scrollTop) return;
    this.el.scrollLeft = left;
    this.el.scrollTop = top;
    const at = this.hit(clamp(ev.clientX, x0, x1 - 1), clamp(ev.clientY, y0, y1 - 1));
    if (at) this.h.drag?.(at, ev);
    this.dragTimer = this.timers.setTimeout(() => this.#dragScroll(ev, axes), DRAG_SCROLL_EVERY);
  }

  // ---- drawing ----

  #newCell() {
    const tx = h('span', { class: 'tx' });
    const el = h('div', { class: 'gc', role: 'gridcell' }, tx);
    el._tx = tx;
    return el;
  }

  #newRow(head = h('div', { class: 'rh', role: 'rowheader' })) {
    const fz = h('div', { class: 'gfz', role: 'none' }, head);
    const el = h('div', { class: 'gr', role: 'row' }, fz);
    el._fz = fz;
    el._rh = head;
    el._f = [];
    el._c = [];
    el._tall = []; // [cell element, column id] of the drawn cells that wrap or hold line breaks
    el._cols = []; // column ids of the drawn cells
    return el;
  }

  paint(pass = 0) {
    const t = this.model.tab;
    if (!t || !t.rows.length || !t.cols.length) {
      for (const el of this.rowEls.values()) el.remove();
      this.rowEls.clear();
      this.headRow.hidden = true;
      return;
    }
    this.headRow.hidden = false;
    const w = this.windows();
    const sel = this.selection.box();
    const act = this.selection.active();
    const hadFocus = this.focused;
    const ctx = { t, w, sel, act, one: sel.r1 === sel.r2 && sel.c1 === sel.c2, people: this.#people(t) };

    this.el.setAttribute('aria-rowcount', String(t.rows.length + 1));
    this.el.setAttribute('aria-colcount', String(t.cols.length + 1));
    this.el.setAttribute('aria-label', `Tab ${t.name}, ${t.rows.length} rows, ${t.cols.length} columns`);
    this.size.style.width = `${HEAD_WIDTH + this.cols.total + END_SPACE}px`;
    this.size.style.height = `${HEAD_HEIGHT + this.heights.total + END_SPACE}px`;
    this.top.style.height = `${HEAD_HEIGHT + w.fh}px`;

    const want = [];
    for (let r = 0; r < w.fr; r++) want.push(r);
    for (let r = w.rows.start; r < w.rows.end; r++) want.push(r);
    if (!want.includes(act.r)) { want.push(act.r); want.sort((a, b) => a - b); }
    const keep = new Set(want.map((r) => t.rows[r]));
    for (const [id, el] of this.rowEls) {
      if (keep.has(id)) continue;
      this.rowEls.delete(id);
      if (el.contains(this.activeEl)) this.activeEl.remove();
      el.remove();
      this.pool.push(el);
    }
    const topRows = [this.headRow];
    const bodyRows = [];
    this.#paintHead(ctx);
    for (const r of want) {
      const id = t.rows[r];
      let el = this.rowEls.get(id);
      if (!el) { el = this.pool.pop() || this.#newRow(); this.rowEls.set(id, el); }
      (r < w.fr ? topRows : bodyRows).push(el);
      this.#paintRow(el, r, id, ctx);
    }
    order(this.top, topRows);
    order(this.body, bodyRows);
    const changed = this.#measureRows(want, t);
    if (hadFocus && !this.focused) this.input.focus({ preventScroll: true });
    if (changed && pass < 2) this.paint(pass + 1);
  }

  #columns(w) {
    const out = [];
    for (let c = w.cols.start; c < w.cols.end; c++) out.push(c);
    return out;
  }

  #paintHead(ctx) {
    const { w, sel } = ctx;
    const row = this.headRow;
    const all = this.selection.kind === 'all';
    setClass(this.corner, `rh gcorner${all ? ' on' : ''}`);
    const paintOne = (el, c) => {
      const on = c >= sel.c1 && c <= sel.c2;
      const sig = `${c}|${this.cols.offset(c)}|${this.cols.size(c)}|${on}|${c === w.fc - 1}`;
      if (el._sig === sig) return;
      el._sig = sig;
      el.style.left = `${HEAD_WIDTH + this.cols.offset(c)}px`;
      el.style.width = `${this.cols.size(c)}px`;
      setClass(el, `gh${on ? ' on' : ''}${c === w.fc - 1 ? ' fc-last' : ''}`);
      el.setAttribute('aria-colindex', String(c + 2));
      el._tx.textContent = colName(c);
    };
    const make = () => {
      const tx = h('span', { class: 'tx' });
      const el = h('div', { class: 'gh', role: 'columnheader' }, tx, h('span', { class: 'rz', 'aria-hidden': 'true' }));
      el._tx = tx;
      return el;
    };
    fit(row._f, w.fc, make, row._fz);
    for (let c = 0; c < w.fc; c++) paintOne(row._f[c], c);
    const cols = this.#columns(w);
    fit(row._c, cols.length, make, row);
    cols.forEach((c, i) => paintOne(row._c[i], c));
    row._fz.style.width = `${HEAD_WIDTH + w.fw}px`;
  }

  #paintRow(el, r, rowId, ctx) {
    const { t, w, sel, act } = ctx;
    const top = HEAD_HEIGHT + this.heights.offset(r);
    const hgt = this.heights.get(r);
    if (el._top !== top) { el._top = top; el.style.top = `${top}px`; }
    if (el._h !== hgt) { el._h = hgt; el.style.height = `${hgt}px`; }
    el.setAttribute('aria-rowindex', String(r + 2));
    setClass(el, `gr${r === w.fr - 1 ? ' fr-last' : ''}`);
    el._fz.style.width = `${HEAD_WIDTH + w.fw}px`;
    const onRow = r >= sel.r1 && r <= sel.r2;
    setClass(el._rh, `rh${onRow ? ' on' : ''}`);
    if (el._rh._n !== r) { el._rh._n = r; el._rh.textContent = String(r + 1); }

    const cells = this.model.rowCells(rowId);
    el._tall.length = 0;
    el._cols.length = 0;
    const cols = this.#columns(w);
    // Text that starts left of the drawn columns can still run into them.
    const lead = cells ? this.#leadCell(cells, w, t) : -1;
    if (lead >= 0) cols.unshift(lead);
    fit(el._f, w.fc, () => this.#newCell(), el._fz);
    fit(el._c, cols.length, () => this.#newCell(), el);
    const spill = { until: -1 };
    for (let c = 0; c < w.fc; c++) this.#paintCell(el._f[c], el, r, c, rowId, cells, ctx, spill);
    spill.until = -1;
    cols.forEach((c, i) => this.#paintCell(el._c[i], el, r, c, rowId, cells, ctx, spill));
    if (r === act.r) {
      const parent = act.c < w.fc ? el._fz : el;
      if (this.activeEl.parentNode !== parent) parent.appendChild(this.activeEl);
      this.#paintCell(this.activeEl, el, r, act.c, rowId, cells, ctx, { until: -1 }, true);
    }
  }

  // #leadCell finds the nearest filled cell left of the drawn columns when
  // it is text that may run on into them.
  #leadCell(cells, w, t) {
    let best = -1;
    for (const [col, cell] of cells) {
      if (!cell.kind) continue;
      const c = this.model.colIndex(col);
      if (c >= w.fc && c < w.cols.start && c > best) best = c;
    }
    if (best < 0) return -1;
    const cell = cells.get(t.cols[best]);
    return cell.kind === 't' ? best : -1;
  }

  #paintCell(el, rowEl, r, c, rowId, cells, ctx, spill, isActive = false) {
    const { t, w, sel, act } = ctx;
    const m = this.model;
    const colId = t.cols[c];
    const cell = cells?.get(colId) || null;
    const f = m.format(cell);
    const pending = m.isPending(rowId, colId);
    const shadow = !isActive && r === act.r && c === act.c; // the active cell has its own element
    let text = pending ? m.shown(rowId, colId) : cell ? cell.shown : '';
    const kind = pending ? 't' : cell ? cell.kind : '';
    const width = this.cols.size(c);
    if (!shadow) rowEl._cols.push(colId);
    let cls = isActive ? 'gc act' : 'gc';
    const align = (!pending && alignClass[f.align]) || generalAlign[kind];
    if (align) cls += ` ${align}`;
    if (kind === 'e') cls += ' err';
    if (f.bold) cls += ' b';
    if (f.italic) cls += ' it';
    if (f.underline) cls += ' u';
    if (f.strikethrough) cls += ' s';
    const multi = text.includes('\n');
    if (f.wrap) cls += ' wrap';
    if (multi) cls += ' ml';
    if (pending) cls += ' pend';
    if (c === w.fc - 1) cls += ' fc-last';

    if (this.editing && isActive) cls += ' editing';

    const said = text;
    let spillW = 0;
    if (kind === 'n' && text) {
      if (this.measure.text(text, !!f.bold) > width - PAD) text = '####';
    } else if (kind === 't' && text && !f.wrap && !multi && (align === '' || align === 'lft') && !(this.editing && isActive)) {
      const need = this.measure.text(text, !!f.bold) + PAD;
      if (need > width) {
        let room = width;
        let last = c;
        const stop = c < w.fc ? w.fc : t.cols.length;
        while (room < need && last + 1 < stop && this.#blank(rowId, t.cols[last + 1], cells)) { last++; room += this.cols.size(last); }
        if (last > c) { spillW = Math.min(room, need); spill.until = last; cls += ' spill'; }
      }
    }
    if (!spillW && c < spill.until && !text) cls += ' cov';
    if ((f.wrap || multi) && text && !shadow) rowEl._tall.push([el, colId]);

    const selected = !ctx.one && r >= sel.r1 && r <= sel.r2 && c >= sel.c1 && c <= sel.c2;
    if (selected) {
      cls += ' sel';
      if (r === sel.r1) cls += ' s-t';
      if (r === sel.r2) cls += ' s-b';
      if (c === sel.c1) cls += ' s-l';
      if (c === sel.c2) cls += ' s-r';
    }
    const o = this.overlays;
    const idKey = `${rowId}:${colId}`;
    if (o.hits?.has(idKey)) cls += ' hit';
    if (o.flash?.has(idKey)) cls += ' flash';
    if (o.fill && r >= o.fill.r1 && r <= o.fill.r2 && c >= o.fill.c1 && c <= o.fill.c2) cls += ' fl';
    let who = '';
    let editedBy = '';
    for (const p of ctx.people) {
      if (!p.row(r) || !p.col(c)) continue;
      cls += ` eg pc${(p.color % PEOPLE_COLORS) + 1}`;
      // An edge is drawn where the range ends: beside a row or a column
      // that is not part of it.
      const top = !p.row(r - 1);
      const left = !p.col(c - 1);
      if (top) cls += ' et';
      if (!p.row(r + 1)) cls += ' eb';
      if (left) cls += ' el';
      if (!p.col(c + 1)) cls += ' er';
      if (top && left) who = p.editing ? `${p.label} is editing` : p.label;
      if (p.editing && r === p.ar && c === p.ac) editedBy = p.label;
      break;
    }
    let handle = o.handle && r === sel.r2 && c === sel.c2;
    if (isActive) {
      const label = cellLabel({ r, c, shown: said, formula: !!cell?.formula, error: kind === 'e', pending, selected, editing: editedBy });
      if (this.input.getAttribute('aria-label') !== label) { this.input.setAttribute('aria-label', label); this.h.label?.(label); }
    }
    // The active cell has its own element; the recycled one under it is an
    // empty placeholder that assistive technology skips.
    if (shadow) { cls = `gc ph${selected ? ' sel' : ''}`; text = ''; spillW = 0; who = ''; handle = false; }

    const left = HEAD_WIDTH + this.cols.offset(c);
    const sig = `${left}|${width}|${cls}|${text}|${f.text_color || ''}|${f.fill || ''}|${spillW}|${who}|${handle}|${c}`;
    if (el._sig === sig) return;
    el._sig = sig;
    el.style.left = `${left}px`;
    el.style.width = `${width}px`;
    setClass(el, cls);
    el.style.color = f.text_color && !shadow ? `var(--c-${f.text_color})` : '';
    el.style.backgroundColor = f.fill && !shadow ? `var(--c-${f.fill})` : '';
    el._tx.style.width = spillW ? `${spillW}px` : '';
    if (el._tx.textContent !== text) el._tx.textContent = text;
    el.setAttribute('aria-colindex', String(c + 2));
    if (shadow) el.setAttribute('aria-hidden', 'true'); else el.removeAttribute('aria-hidden');
    if (selected) el.setAttribute('aria-selected', 'true'); else el.removeAttribute('aria-selected');
    extra(el, 'who', who);
    extra(el, 'handle', handle ? ' ' : '');
  }

  // #people gives each person on this tab two questions for one paint: is
  // the row at position r, or the column at position c, in their range.
  // An answer is found by halving the range's spans and kept for the
  // paint, and only the cells that are drawn ask, so drawing a range costs
  // what is on screen.
  #people(t) {
    return this.overlays.people.map((p) => {
      const rows = new Map();
      const cols = new Map();
      const has = (kept, spans, ids, i) => {
        if (i < 0 || i >= ids.length) return false;
        let yes = kept.get(i);
        if (yes === undefined) { yes = inRuns(spans, ids[i]); kept.set(i, yes); }
        return yes;
      };
      return { ...p, row: (r) => has(rows, p.rows, t.rows, r), col: (c) => has(cols, p.cols, t.cols, c) };
    });
  }

  #blank(rowId, colId, cells) {
    if (this.model.isPending(rowId, colId)) return false;
    const x = cells?.get(colId);
    return !x || !x.kind;
  }

  // #measureRows sets the height of each drawn row from every cell of the
  // row that wraps or holds line breaks, and returns whether a height
  // changed. Drawn cells are measured where they are. The cells outside
  // the drawn columns are measured once, in hidden cells of the same
  // width and format, and their heights are kept until the row's content
  // or a width changes: scrolling sideways never changes a row's height.
  #measureRows(want, t) {
    // First lay out the hidden cells of the rows that still need them, so
    // that all heights are read after one layout.
    const asked = [];
    let used = 0;
    for (const r of want) {
      const rowId = t.rows[r];
      if (this.scanned.has(rowId)) continue;
      if (used >= PROBES_MAX) { this.schedule(); break; } // the next frame goes on
      const list = this.#probeRow(this.rowEls.get(rowId), rowId, used);
      used += list.length;
      asked.push([rowId, list]);
    }
    for (const [rowId, list] of asked) {
      const kept = new Map();
      for (const [colId, probe] of list) kept.set(colId, Math.ceil(this.measure.height(probe._tx)) + 1);
      if (kept.size) this.tall.set(rowId, kept); else this.tall.delete(rowId);
      this.scanned.add(rowId);
    }
    for (let i = 0; i < used; i++) this.probes[i].hidden = true;
    let changed = false;
    for (const r of want) {
      const rowId = t.rows[r];
      const el = this.rowEls.get(rowId);
      let kept = this.tall.get(rowId);
      if (kept || el._tall.length) {
        if (!kept) { kept = new Map(); this.tall.set(rowId, kept); }
        // A drawn cell counts as it is drawn now: pending text and an
        // open editor change it without a change of the model.
        if (this.scanned.has(rowId)) for (const colId of el._cols) kept.delete(colId);
        for (const [cell, colId] of el._tall) kept.set(colId, Math.ceil(this.measure.height(cell._tx)) + 1);
        if (!kept.size) this.tall.delete(rowId);
      }
      let hgt = ROW_HEIGHT;
      if (kept) for (const v of kept.values()) if (v > hgt) hgt = v;
      if (hgt === ROW_HEIGHT) this.measured.delete(rowId); else this.measured.set(rowId, hgt);
      if (this.heights.set(r, hgt)) changed = true;
    }
    return changed;
  }

  // #probeRow puts one hidden cell in the probe box for each cell of the
  // row that wraps or holds line breaks and lies outside the drawn columns,
  // and returns [column id, hidden cell] for them. from is the number of
  // hidden cells already in use.
  #probeRow(el, rowId, from) {
    const out = [];
    const cells = this.model.rowCells(rowId);
    if (!cells) return out;
    const m = this.model;
    const drawn = new Set(el._cols);
    for (const [colId, cell] of cells) {
      if (drawn.has(colId)) continue;
      const pending = m.isPending(rowId, colId);
      let text = pending ? m.shown(rowId, colId) : cell.shown;
      const f = m.format(cell);
      const multi = text.includes('\n');
      if (!text || !(f.wrap || multi)) continue;
      const c = m.colIndex(colId);
      if (c < 0) continue;
      const width = this.cols.size(c);
      if (!pending && cell.kind === 'n' && this.measure.text(text, !!f.bold) > width - PAD) text = '####';
      const i = from + out.length;
      let probe = this.probes[i];
      if (!probe) { probe = this.#newCell(); probe.removeAttribute('role'); this.probes.push(probe); this.probeBox.appendChild(probe); }
      probe.hidden = false;
      setClass(probe, `gc${f.bold ? ' b' : ''}${f.italic || pending ? ' it' : ''}${f.wrap ? ' wrap' : ''}${multi ? ' ml' : ''}`);
      probe.style.width = `${width}px`;
      if (probe._tx.textContent !== text) probe._tx.textContent = text;
      out.push([colId, probe]);
    }
    return out;
  }
}

// fit makes list hold n cells, creating and removing elements at its end.
function fit(list, n, make, parent) {
  while (list.length < n) { const el = make(); list.push(el); parent.appendChild(el); }
  while (list.length > n) list.pop().remove();
}

// order puts parent's children in list order, moving only the elements
// that are out of place: a moved element loses focus, and the active row
// never moves while the person only scrolls.
function order(parent, list) {
  let cursor = parent.firstChild;
  for (const el of list) {
    if (el === cursor) { cursor = cursor.nextSibling; continue; }
    parent.insertBefore(el, cursor);
  }
}

function setClass(el, cls) { if (el.className !== cls) el.className = cls; }

// extra keeps one optional child of a cell: another person's name tag or
// the fill handle.
function extra(el, name, text) {
  const key = `_${name}`;
  if (!text) { if (el[key]) { el[key].remove(); el[key] = null; } return; }
  if (!el[key]) { el[key] = h('span', { class: name, 'aria-hidden': name === 'handle' ? 'true' : null }); el.appendChild(el[key]); }
  if (name === 'who' && el[key].textContent !== text) el[key].textContent = text;
}
