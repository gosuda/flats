// Selection (rules 17 and 21). The selection names its anchor, its far
// corner and the active cell by row id and column id, so an insert, a
// delete or a sort elsewhere leaves it on the same cells. Positions are
// resolved against the open tab when they are needed. A row or column that
// disappeared falls back to the position it had.
'use strict';

import { packIds } from './protocol.js';

const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

export class Selection {
  constructor(model) {
    this.model = model;
    this.kind = 'cells'; // cells, rows, cols or all: headers select whole lines
    this.anchor = null;  // {row, col} ids
    this.focus = null;
    this.activeId = null;
    this.last = { ar: 0, ac: 0, r1: 0, c1: 0, r2: 0, c2: 0 };
  }

  #ids(r, c) { return { row: this.model.rowAt(r), col: this.model.colAt(c) }; }

  #at(p, lastR, lastC) {
    const m = this.model;
    let r = p ? m.rowIndex(p.row) : -1;
    let c = p ? m.colIndex(p.col) : -1;
    if (r < 0) r = lastR;
    if (c < 0) c = lastC;
    return { r: clamp(r, 0, Math.max(0, m.rowCount - 1)), c: clamp(c, 0, Math.max(0, m.colCount - 1)) };
  }

  // active is the position of the active cell.
  active() {
    const p = this.#at(this.activeId, this.last.ar, this.last.ac);
    this.last.ar = p.r;
    this.last.ac = p.c;
    return p;
  }

  // box is the selected range as positions, r1 <= r2 and c1 <= c2.
  box() {
    const m = this.model;
    const l = this.last;
    const a = this.#at(this.anchor, l.r1, l.c1);
    const f = this.#at(this.focus, l.r2, l.c2);
    l.r1 = a.r; l.c1 = a.c; l.r2 = f.r; l.c2 = f.c;
    const b = { r1: Math.min(a.r, f.r), c1: Math.min(a.c, f.c), r2: Math.max(a.r, f.r), c2: Math.max(a.c, f.c) };
    if (this.kind === 'rows' || this.kind === 'all') { b.c1 = 0; b.c2 = Math.max(0, m.colCount - 1); }
    if (this.kind === 'cols' || this.kind === 'all') { b.r1 = 0; b.r2 = Math.max(0, m.rowCount - 1); }
    return b;
  }

  // far is the corner Shift moves: the end of the range opposite its anchor.
  far() { return this.#at(this.focus, this.last.r2, this.last.c2); }

  get single() { const b = this.box(); return b.r1 === b.r2 && b.c1 === b.c2; }

  contains(r, c) { const b = this.box(); return r >= b.r1 && r <= b.r2 && c >= b.c1 && c <= b.c2; }

  // set selects one cell and makes it active.
  set(r, c) {
    const m = this.model;
    r = clamp(r, 0, Math.max(0, m.rowCount - 1));
    c = clamp(c, 0, Math.max(0, m.colCount - 1));
    this.kind = 'cells';
    this.anchor = this.focus = this.activeId = this.#ids(r, c);
    this.last = { ar: r, ac: c, r1: r, c1: c, r2: r, c2: c };
  }

  // extend moves the far corner and keeps the anchor and the active cell.
  extend(r, c) {
    const m = this.model;
    if (!this.anchor) return this.set(r, c);
    r = clamp(r, 0, Math.max(0, m.rowCount - 1));
    c = clamp(c, 0, Math.max(0, m.colCount - 1));
    this.focus = this.#ids(r, c);
    this.last.r2 = r;
    this.last.c2 = c;
  }

  range(r1, c1, r2, c2, kind = 'cells') {
    this.set(r1, c1);
    this.extend(r2, c2);
    this.kind = kind;
  }

  // rows, cols and all select whole lines; the active cell stays in the
  // first selected line.
  rows(r1, r2) { const c = this.active().c; this.range(r1, c, r2, c, 'rows'); }
  cols(c1, c2) { const r = this.active().r; this.range(r, c1, r, c2, 'cols'); }

  all() {
    const a = this.active();
    this.range(0, 0, this.model.rowCount - 1, this.model.colCount - 1, 'all');
    this.activeId = this.#ids(a.r, a.c);
    this.last.ar = a.r;
    this.last.ac = a.c;
  }

  // ids names the selected range for an operation: id runs of its rows and
  // of its columns, in position order.
  ids(b = this.box()) {
    const t = this.model.tab;
    return { rows: packIds(t.rows.slice(b.r1, b.r2 + 1)), cols: packIds(t.cols.slice(b.c1, b.c2 + 1)) };
  }
}
