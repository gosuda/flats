// What the client holds (TECH 19.2): the tab list and one open tab, with its
// row ids and column ids in order, each row's cells by column id, the style
// list and the widths. Deltas change it; the client's own unconfirmed edits
// lie over it as pending text and never replace the server's values. The
// model calculates nothing: value, shown text and edit text all come from
// the server.
'use strict';

import { DEFAULT_WIDTH, POSITION_BASE, decodeCell, expandIds, isAxisEdit, isBlankCell, isStructural, seqCmp, structEdits } from './protocol.js';

const key = (row, col) => `${row}:${col}`;
const NO_FORMAT = Object.freeze({});

export class Model {
  constructor() {
    this.tabs = [];      // TabInfo[] in tab order
    this.date = '';      // calculation date of the state on screen
    // seq is the sequence of the grid: the open tab shows every change up
    // to it and none after it. It is the base of an action. It is not the
    // place of the stream, which can be ahead: the connection may have
    // received more than the grid shows.
    this.seq = '0';
    // stale says that the stream went on without the grid: changes between
    // seq and the stream's place were lost or skipped, so no delta can bring
    // the grid up to date. Only a new snapshot does (openHead).
    this.stale = false;
    // structSeq is the newest sequence at which the grid may have taken a
    // structural change (TECH 8.4). That is a delta that moved rows,
    // columns or tabs, or a snapshot, whose path from the earlier grid is
    // unknown.
    this.structSeq = '0';
    this.tab = null;     // the open tab, or null while none is loaded
    // "row:col" -> {n, text, base, at} is typed text not yet confirmed,
    // with the sequence and the [row, col] of the screen it was typed on.
    this.pending = new Map();
    this.pendingWidths = new Map(); // col -> {n, width}
    this.peeks = new Map();         // "row:col" -> {edit, formula}
  }

  // ---- tab list ----

  setTabs(list) {
    this.tabs = list || [];
    const t = this.tab;
    if (!t) return;
    const info = this.tabs.find((x) => x.id === t.id);
    if (info) { t.frozenRows = info.frozen_rows; t.frozenCols = info.frozen_cols; t.name = info.name; }
  }

  tabInfo(id) { return this.tabs.find((t) => t.id === id) || null; }

  // ---- snapshot of a tab (TECH 12.2) ----

  openHead(m) {
    const info = this.tabInfo(m.tab);
    const widths = new Map();
    for (const [col, w] of Object.entries(m.widths || {})) widths.set(Number(col), w);
    this.tab = {
      id: m.tab, name: info ? info.name : '', generation: m.generation, loading: true,
      rows: expandIds(m.rows), cols: expandIds(m.cols), rowPos: null, colPos: null,
      widths, frozenRows: m.frozen_rows || 0, frozenCols: m.frozen_cols || 0,
      styles: m.styles?.length ? m.styles.slice() : [NO_FORMAT],
      // While the snapshot arrives, a cell the grid has no record of may
      // hold content that has not come yet. Rows come top first, and a row
      // may go on in the next frame, so the rows above the last row of the
      // latest frame are whole, and through counts them. started says that
      // the first rows frame arrived.
      cells: new Map(), fresh: new Set(), through: 0, started: false,
    };
    this.peeks.clear();
    // The grid is the snapshot: it is at the snapshot's sequence, whatever
    // sequence the grid before it had.
    this.seq = m.seq;
    this.structSeq = m.seq;
    this.stale = false;
  }

  // lag marks the grid as behind the stream for good.
  lag() { if (this.tab) this.stale = true; }

  // addRows installs a `rows` frame. A cell a delta changed since the
  // snapshot's sequence keeps the newer value.
  addRows(m) {
    const t = this.tab;
    for (const [row, cells] of m.rows) {
      for (const raw of cells) {
        const c = decodeCell(raw);
        if (t.fresh?.has(key(row, c.col))) continue;
        setCell(t, row, c);
      }
    }
    t.started = true;
    if (m.rows.length) t.through = Math.max(t.through, this.rowIndex(m.rows[m.rows.length - 1][0]));
  }

  // known says whether the grid holds what a cell has: its snapshot rows
  // have arrived, a delta changed it since, or it shows the person's own
  // pending text. A cell that is not known is not blank, because its
  // content may still be on its way.
  known(row, col) {
    const t = this.tab;
    if (!t) return false;
    if (!t.loading) return true;
    const k = key(row, col);
    if (t.fresh.has(k) || this.pending.has(k)) return true;
    const r = this.rowIndex(row);
    return r >= 0 && r < t.through;
  }

  // knownBox says whether every cell of a box of positions is known. A
  // whole line selection is not while the snapshot arrives, because where
  // its filled part ends is not known yet.
  knownBox(b, lines = false) {
    const t = this.tab;
    if (!t) return false;
    if (!t.loading) return true;
    if (lines) return false;
    const r1 = Math.max(b.r1, t.through);
    if (r1 > b.r2) return true;
    // Only cells a delta changed or that are pending are known below the
    // rows that arrived, so a box with more cells there cannot be.
    if ((b.r2 - r1 + 1) * (b.c2 - b.c1 + 1) > t.fresh.size + this.pending.size) return false;
    for (let r = r1; r <= b.r2; r++) {
      for (let c = b.c1; c <= b.c2; c++) if (!this.known(t.rows[r], t.cols[c])) return false;
    }
    return true;
  }

  done() {
    this.tab.loading = false;
    this.tab.fresh = null;
  }

  close() { this.tab = null; this.stale = false; this.pending.clear(); this.pendingWidths.clear(); this.peeks.clear(); }

  // ---- deltas (TECH 12.3) ----

  // apply installs one delta in the grid and reports what changed: {tabs, struct, rows,
  // cells, widths, text, gone, rewatch}. text means the formula text of any
  // cell may differ now: a tab or an axis of any tab changed, so the peek
  // cache was dropped. rewatch means a structural edit met a snapshot that
  // is still arriving, so the tab must be watched again. gone means the
  // open tab is no longer in the tab list.
  //
  // seq moves to the delta's sequence only when the grid holds the whole
  // delta. A delta the grid did not take leaves the grid stale at the
  // sequence it had: one that met a snapshot still arriving (rewatch), and
  // one that could not be installed, whose error goes on to the caller.
  // What such a delta changed before it failed stays in a grid that only a
  // snapshot replaces.
  apply(d) {
    const out = { tabs: false, struct: false, widths: false, text: false, rows: new Set(), cells: [], gone: false, rewatch: false };
    if (d.date !== undefined) this.date = d.date;
    if (isStructural(d, this.tabs)) this.structSeq = d.seq;
    if (this.stale) {
      // A stale grid takes no delta and keeps its sequence: the snapshot
      // that replaces it holds this change. The tab list is not the grid
      // and stays current.
      const t = this.tab;
      if (d.tabs) {
        this.setTabs(d.tabs);
        this.peeks.clear();
        out.tabs = true;
        out.gone = !!t && !this.tabInfo(t.id);
      }
      return out;
    }
    try {
      this.#install(d, out);
    } catch (err) {
      this.lag();
      throw err;
    }
    if (out.rewatch) this.lag(); else this.seq = d.seq;
    return out;
  }

  #install(d, out) {
    const t = this.tab;
    const edits = structEdits(d);
    if (d.tabs || edits.length) {
      // A formula's A1 text follows the structure of every tab it names.
      this.peeks.clear();
      out.text = true;
    }
    if (d.tabs) {
      this.setTabs(d.tabs);
      out.tabs = true;
      out.gone = !!t && !this.tabInfo(t.id);
    }
    if (!t || out.gone) return;
    const own = edits.filter((e) => e.tab === t.id && isAxisEdit(e));
    if (own.length && t.loading) { out.rewatch = true; return; }
    // A delta may add as many formats as a sheet has cells, more than a
    // call takes as arguments, so they are added one at a time.
    if (d.styles) for (const f of d.styles) t.styles.push(f);
    if (own.length) {
      for (const e of own) applyStruct(t, e);
      out.struct = true;
    }
    for (const [col, w] of d.widths || []) { t.widths.set(col, w); out.widths = true; }
    for (const [row, cells] of d.cells || []) {
      out.rows.add(row);
      for (const raw of cells) {
        const c = decodeCell(raw);
        setCell(t, row, c);
        t.fresh?.add(key(row, c.col));
        this.peeks.delete(key(row, c.col));
        out.cells.push([row, c.col]);
      }
    }
  }

  // ---- reading the open tab ----

  get rowCount() { return this.tab ? this.tab.rows.length : 0; }
  get colCount() { return this.tab ? this.tab.cols.length : 0; }
  rowAt(r) { return this.tab.rows[r]; }
  colAt(c) { return this.tab.cols[c]; }

  rowIndex(id) {
    const t = this.tab;
    if (!t.rowPos) { t.rowPos = new Map(); t.rows.forEach((x, i) => t.rowPos.set(x, i)); }
    return t.rowPos.get(id) ?? -1;
  }

  colIndex(id) {
    const t = this.tab;
    if (!t.colPos) { t.colPos = new Map(); t.cols.forEach((x, i) => t.colPos.set(x, i)); }
    return t.colPos.get(id) ?? -1;
  }

  cell(row, col) { return this.tab?.cells.get(row)?.get(col) || null; }
  cellAt(r, c) { return this.cell(this.tab.rows[r], this.tab.cols[c]); }
  rowCells(row) { return this.tab.cells.get(row) || null; }
  format(cell) { return (cell && this.tab.styles[cell.style]) || NO_FORMAT; }
  filledAt(r, c) { const x = this.cellAt(r, c); return !!x && !!x.kind; }

  widthOf(col) { return this.pendingWidths.get(col)?.width ?? this.tab.widths.get(col) ?? DEFAULT_WIDTH; }

  // shown is the text a cell shows: the person's unconfirmed text while an
  // edit is pending, else the server's shown text.
  shown(row, col) {
    const p = this.pending.get(key(row, col));
    if (p) return p.text;
    return this.cell(row, col)?.shown ?? '';
  }

  isPending(row, col) { return this.pending.has(key(row, col)); }

  // movedPending returns the pending entry of a cell when its text is a
  // formula typed on a screen that a structural change has left since.
  // Its references name the cells of that screen, not of this one.
  movedPending(row, col) {
    const p = this.pending.get(key(row, col));
    return p && p.text.startsWith('=') && p.base != null && seqCmp(this.structSeq, p.base) > 0 ? p : null;
  }

  // usedRange is the last row and column that hold content, or -1.
  usedRange() {
    let r = -1;
    let c = -1;
    for (const [row, cells] of this.tab.cells) {
      let any = false;
      for (const [col, cell] of cells) {
        if (!cell.kind) continue;
        any = true;
        c = Math.max(c, this.colIndex(col));
      }
      if (any) r = Math.max(r, this.rowIndex(row));
    }
    return { r, c };
  }

  // eachCell calls fn(r, c, cell) for the cells with content inside box, in
  // no fixed order. It walks the rows that hold cells, so a whole column
  // costs the filled rows and not the tab's height.
  eachCell(box, fn) {
    for (const [row, cells] of this.tab.cells) {
      const r = this.rowIndex(row);
      if (r < box.r1 || r > box.r2) continue;
      for (const [col, cell] of cells) {
        if (!cell.kind) continue;
        const c = this.colIndex(col);
        if (c >= box.c1 && c <= box.c2) fn(r, c, cell);
      }
    }
  }

  // ---- pending edits ----

  addPending(n, entries) { for (const e of entries) this.pending.set(key(e.row, e.col), { n, text: e.text, base: e.base, at: e.at }); }
  addPendingWidth(n, cols, width) { for (const col of cols) this.pendingWidths.set(col, { n, width }); }

  // dropPending removes the pending display of action n. A later action on
  // the same cell keeps its own. seen, when given, collects what the screen
  // must draw again: the rows whose text changed, and whether a width did.
  dropPending(n, seen = null) {
    let any = false;
    for (const [k, p] of this.pending) {
      if (p.n !== n) continue;
      this.pending.delete(k);
      any = true;
      if (seen) seen.rows.add(Number(k.slice(0, k.indexOf(':'))));
    }
    for (const [k, p] of this.pendingWidths) {
      if (p.n !== n) continue;
      this.pendingWidths.delete(k);
      any = true;
      if (seen) seen.widths = true;
    }
    return any;
  }

  // ---- formula text on demand (TECH 12.4) ----

  peeked(row, col) { return this.peeks.get(key(row, col)) || null; }
  setPeek(row, col, p) { this.peeks.set(key(row, col), p); }
}

function setCell(t, row, c) {
  let cells = t.cells.get(row);
  if (isBlankCell(c)) {
    if (cells) { cells.delete(c.col); if (!cells.size) t.cells.delete(row); }
    return;
  }
  if (!cells) { cells = new Map(); t.cells.set(row, cells); }
  cells.set(c.col, c);
}

// applyStruct applies one StructEdit to the open tab's axes.
function applyStruct(t, e) {
  switch (e.op) {
    case 'insert_rows': t.rows = inserted(t.rows, expandIds(e.ids), e.after); t.rowPos = null; break;
    case 'insert_cols': t.cols = inserted(t.cols, expandIds(e.ids), e.after); t.colPos = null; break;
    case 'delete_rows': {
      const gone = new Set(expandIds(e.ids));
      t.rows = t.rows.filter((id) => !gone.has(id));
      for (const id of gone) t.cells.delete(id);
      t.rowPos = null;
      break;
    }
    case 'delete_cols': {
      const gone = new Set(expandIds(e.ids));
      t.cols = t.cols.filter((id) => !gone.has(id));
      for (const [row, cells] of t.cells) {
        for (const id of gone) cells.delete(id);
        if (!cells.size) t.cells.delete(row);
      }
      for (const id of gone) t.widths.delete(id);
      t.colPos = null;
      break;
    }
    case 'sort': {
      // A new list, as for an insert or a delete, so a paste asked for
      // earlier can keep the list of its screen to know where its cells were.
      const at = expandIds(e.positions);
      t.rows = t.rows.slice();
      at.forEach((p, i) => { t.rows[p - POSITION_BASE] = e.ids[i]; });
      t.rowPos = null;
      break;
    }
  }
}

// inserted puts ids after the id `after`, or first when after is null. An
// unknown anchor puts them at the end.
function inserted(list, ids, after) {
  let at = 0;
  if (after != null) {
    const i = list.indexOf(after);
    at = i < 0 ? list.length : i + 1;
  }
  return [...list.slice(0, at), ...ids, ...list.slice(at)];
}
