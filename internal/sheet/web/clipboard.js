// Copy, cut and paste (rule 21, TECH 19.5). The system clipboard carries
// tab-separated shown text, so a range moves to and from other spreadsheet
// programs. A paste whose text equals what this tab last copied is a paste
// inside the sheet: it sends `copy`, and the server carries formulas and
// formats. Any other paste sends the typed strings for the server to read.
'use strict';

import { ACTION_MAX, ops, packIds, wirePosition } from './protocol.js';

const REPEAT_MAX = 10000; // cells one pasted value may fill
// CELL_MIN is the fewest bytes a cell takes in an action: its two
// quotation marks and the comma or bracket after it.
const CELL_MIN = 3;

// encodeTSV writes rows of fields as tab-separated text. A field that
// holds a tab, a line break or a quotation mark is quoted, with quotation
// marks doubled, as spreadsheet programs write it.
export function encodeTSV(rows) {
  return rows.map((row) => row.map((f) => (/[\t\n\r"]/.test(f) ? `"${f.replaceAll('"', '""')}"` : f)).join('\t')).join('\n');
}

// scan reads tab-separated text once. One trailing line break ends the
// last row and adds none. With rows, a list that holds one empty row, it
// puts each field in its row. It returns the number of rows, the fields of
// the widest row and the code units of all fields.
function scan(text, rows) {
  let height = 1;
  let width = 0;
  let units = 0;
  let count = 0; // fields of this row so far
  let size = 0;  // code units of this field so far
  let field = '';
  let open = true; // a field is due: at the start, after a tab and on a new row
  let i = 0;
  const n = text.length;
  const take = () => { size++; if (rows) field += text[i]; };
  const endField = () => {
    if (rows) { rows[rows.length - 1].push(field); field = ''; }
    units += size;
    size = 0;
    count++;
  };
  const endRow = () => { if (count > width) width = count; count = 0; };
  while (i < n) {
    const ch = text[i];
    if (ch === '"' && size === 0) {
      i++;
      while (i < n) {
        if (text[i] === '"') {
          if (text[i + 1] === '"') { take(); i += 2; continue; }
          i++;
          break;
        }
        take();
        i++;
      }
      continue;
    }
    if (ch === '\t') { endField(); i++; continue; }
    if (ch === '\n' || ch === '\r') {
      endField();
      endRow();
      i += ch === '\r' && text[i + 1] === '\n' ? 2 : 1;
      open = i < n;
      if (open) { height++; if (rows) rows.push([]); }
      continue;
    }
    take();
    i++;
  }
  if (open) { endField(); endRow(); }
  return { height, width, units };
}

// measureTSV says what decodeTSV would return for text without building
// it: {height, width, units}, its rows, the fields of each row and the
// code units of all its fields. A text of a few kilobytes can ask for
// billions of cells, one long row beside many short ones, so a paste is
// measured before it is read.
export const measureTSV = (text) => scan(text, null);

// decodeTSV reads tab-separated text into rows of fields, each row as
// long as the widest.
export function decodeTSV(text) {
  const rows = [[]];
  const { width } = scan(text, rows);
  for (const r of rows) while (r.length < width) r.push('');
  return rows;
}

// targets lists count ids of axis from position start; past the end of the
// tab it names the rows or columns the write adds.
export function targets(axis, start, count) {
  const out = [];
  for (let i = 0; i < count; i++) out.push(start + i < axis.length ? axis[start + i] : { new: start + i - axis.length + 1 });
  return out;
}

// destination names where a paste goes when it is asked for: the open tab,
// the ids of the rows and columns from the selection's top left cell to
// the end of the tab, and the size of the selection. A paste whose text
// arrives later writes it to those rows and columns wherever they are
// then, and a part whose row or column was deleted meanwhile is ignored
// like any edit of a deleted cell (rules 31 and 32). screen holds the row
// and column lists of the screen the paste's base names, which the model
// replaces rather than changes. A cell's position there goes with its
// pending text, so a reopen adjusts that text from the same screen.
export function destination(model, selection, screen = model.tab) {
  const t = model.tab;
  const b = selection.box();
  return { tab: t.id, rows: t.rows.slice(b.r1), cols: t.cols.slice(b.c1), height: b.r2 - b.r1 + 1, width: b.c2 - b.c1 + 1, single: selection.single, screen: { rows: screen.rows, cols: screen.cols } };
}

// indexIn finds ids in list, through the model's own index when list is
// the one on screen.
function indexIn(list, current, index) {
  if (list === current) return index;
  let at = null;
  return (id) => {
    if (!at) { at = new Map(); list.forEach((x, i) => at.set(x, i)); }
    return at.get(id) ?? -1;
  };
}

export class Clipboard {
  constructor(model, selection) {
    this.model = model;
    this.selection = selection;
    this.kept = null; // {text, tab, rows, cols, cut}: the last copy of this tab
    this.copies = 0;  // copies and cuts made, remembered or not
  }

  // copyText returns the shown text of the selection and remembers the
  // source range by ids. Rows and columns after the last filled cell are
  // left out of a whole-line selection. With remember off nothing is kept
  // and an earlier copy is forgotten: the text comes from cells a paste
  // cannot copy from, so pasting it writes the text. It returns null, and
  // keeps the earlier copy, while a cell of the selection has not arrived.
  // A selection of cells can be the whole tab, blank cells and all. For a
  // range of more cells than a paste can carry it returns false, keeps
  // the earlier copy and reads no cell.
  copyText(cut, remember = true) {
    const m = this.model;
    const t = m.tab;
    const b = { ...this.selection.box() };
    if (!m.knownBox(b, this.selection.kind !== 'cells')) return null;
    if (this.selection.kind !== 'cells') {
      const used = m.usedRange();
      b.r2 = Math.max(b.r1, Math.min(b.r2, used.r));
      b.c2 = Math.max(b.c1, Math.min(b.c2, used.c));
    }
    if (CELL_MIN * (b.r2 - b.r1 + 1) * (b.c2 - b.c1 + 1) > ACTION_MAX) return false;
    const rows = [];
    for (let r = b.r1; r <= b.r2; r++) {
      const row = [];
      for (let c = b.c1; c <= b.c2; c++) row.push(m.shown(t.rows[r], t.cols[c]));
      rows.push(row);
    }
    const text = encodeTSV(rows);
    this.copies++;
    this.kept = remember ? { text, tab: t.id, rows: t.rows.slice(b.r1, b.r2 + 1), cols: t.cols.slice(b.c1, b.c2 + 1), cut } : null;
    return text;
  }

  // remembers says whether a paste of text is a paste of the last copy of
  // this tab. Shown text can be empty, as for a formula whose result is
  // empty text or a blank cell with a format, and such a copy is pasted
  // like any other.
  remembers(text) { return this.kept != null && this.kept.text === text; }

  // pasteOps turns clipboard text into the operations of one action, the
  // pending text to show and the text of each cell the host may write:
  // {ops, pending, typed, cut}. typed also holds the cells of the rows and
  // columns the paste adds, which the screen cannot show yet, each named by
  // its {new: k} target until the host gives it an id. d is the
  // destination, the selection's now unless the paste was asked for
  // earlier. cut is the copy a paste moves, kept for the answer: a cut that
  // moved nothing can be pasted again. fits says whether a list of
  // operations can be sent as one action. pasteOps returns null, and
  // changes nothing, for a paste that cannot. For text from elsewhere
  // whose rectangle cannot fit an action it builds nothing at all.
  pasteOps(text, valuesOnly, d = destination(this.model, this.selection), fits = () => true) {
    const m = this.model;
    const k = this.kept;
    if (this.remembers(text)) {
      const from = { tab: k.tab, rows: packIds(k.rows), cols: packIds(k.cols) };
      const cut = k.cut && !valuesOnly;
      // One copied cell goes over the whole selection in the copy itself,
      // which repeats its source over a larger target and moves its
      // formula from the source to each cell (rule 15). Every cell comes
      // from the source, never from another pasted cell, and a source that
      // is gone leaves every cell as it was.
      const repeat = k.rows.length === 1 && k.cols.length === 1 && !d.single && !cut;
      const to = repeat
        ? { tab: d.tab, rows: packIds(d.rows.slice(0, d.height)), cols: packIds(d.cols.slice(0, d.width)) }
        : { tab: d.tab, rows: targets(d.rows, 0, k.rows.length), cols: targets(d.cols, 0, k.cols.length) };
      const list = [ops.copy(from, to, valuesOnly ? 'values' : 'all', cut)];
      if (!fits(list)) return null;
      if (cut) this.kept = null; // a cut moves once
      return { ops: list, pending: [], typed: [], cut: cut ? { kept: k, copies: this.copies } : null };
    }
    let { height, width, units } = measureTSV(text);
    const cells = d.height * d.width;
    const repeat = height === 1 && width === 1 && cells > 1 && cells <= REPEAT_MAX;
    if (repeat) { ({ height, width } = d); units *= cells; }
    // An action with this many cells and this much text passes the size
    // the host takes. Nothing is built for it.
    if (CELL_MIN * height * width + units > ACTION_MAX) return null;
    let values = decodeTSV(text);
    if (repeat) values = Array.from({ length: height }, () => new Array(width).fill(values[0][0]));
    const rows = targets(d.rows, 0, values.length);
    const cols = targets(d.cols, 0, values[0].length);
    // The measure counts the least the cells can take. With the ids of
    // its rows and columns and the bytes of its text the write can still
    // be too large.
    const list = [ops.write(d.tab, rows, cols, values)];
    if (!fits(list)) return null;
    // A cell deleted since the paste was asked for shows no pending text,
    // and neither does one that was not on the screen of its base, which
    // the host ignores. The others keep their position on that screen.
    const rowAt = indexIn(d.screen.rows, m.tab.rows, (id) => m.rowIndex(id));
    const colAt = indexIn(d.screen.cols, m.tab.cols, (id) => m.colIndex(id));
    // A position is null for a row or column the paste adds, and -1 for
    // one the host ignores.
    const rowOf = (row) => (typeof row !== 'number' ? null : m.rowIndex(row) < 0 ? -1 : rowAt(row));
    const colOf = (col) => (typeof col !== 'number' ? null : m.colIndex(col) < 0 ? -1 : colAt(col));
    const pending = [];
    const typed = [];
    rows.forEach((row, i) => {
      const r = rowOf(row);
      if (r === -1) return;
      cols.forEach((col, j) => {
        const c = colOf(col);
        if (c === -1) return;
        const e = { row, col, text: values[i][j] };
        typed.push(e);
        if (r !== null && c !== null) pending.push({ ...e, at: wirePosition(r, c) });
      });
    });
    return { ops: list, pending, typed, cut: null };
  }

  // unmoved remembers a cut again after the host answered that its paste
  // moved no cell, unless something was copied or cut since.
  unmoved(cut) { if (this.copies === cut.copies) this.kept = cut.kept; }
}
