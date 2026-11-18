// Wire schema of the live protocol (TECH Appendix A, sections 7.1 and 12).
// Everything here is a pure function over frames: id runs, cell and format
// decoding, operation builders and action framing. Each detail the appendix
// leaves open has one named function or constant in this file, so the
// server and the client agree in one place.
'use strict';

export const VERSION = 1;
export const FRAME_MAX = 256 * 1024; // client frame, encoded UTF-8 bytes
export const SERVER_FRAME_MAX = 512 * 1024; // server frame, encoded UTF-8 bytes
export const ACTION_MAX = 8 * 1024 * 1024;
export const PARTS_MAX = 32;
export const HOLD_MAX = 8 * 1024 * 1024; // held deltas while a formula is in the editor (TECH 8.4)

export const DEFAULT_WIDTH = 104;
export const MIN_WIDTH = 40;
export const MAX_WIDTH = 1000;
export const MAX_FROZEN_ROWS = 20;
export const MAX_FROZEN_COLS = 10;
export const MAX_TABS = 50;

// POSITION_BASE is the index of the first row or column wherever the wire
// carries a position instead of an id: `at` of `set`, `rebase` and
// `rebased`, and `positions` of a sort.
export const POSITION_BASE = 0;

// The frame envelope: every frame is one JSON object that names its kind in
// `type`, as the notebook's protocol does. A JSON value that is no object,
// and an object without a text `type`, is no frame. A frame of a type this
// client does not know is left alone.
export const frame = (type, fields = {}) => ({ type, ...fields });
export function frameType(m) {
  if (m === null || typeof m !== 'object' || Array.isArray(m)) throw new TypeError(`a frame that is ${m === null ? 'null' : Array.isArray(m) ? 'a list' : `a ${typeof m}`}, not an object`);
  if (typeof m.type !== 'string') throw new TypeError(`a frame without a text type (${m.type === null ? 'null' : typeof m.type})`);
  return m.type;
}

// ---- sequences ----

// Sequences are decimal strings; compare them as integers of any size.
export function seqCmp(a, b) {
  const x = BigInt(a);
  const y = BigInt(b);
  return x < y ? -1 : x > y ? 1 : 0;
}
export const seqNext = (a) => String(BigInt(a) + 1n);

// ---- id runs ----

// expandIds turns runs [[first, count], ...] into the list of ids.
export function expandIds(runs) {
  const out = [];
  for (const [first, count] of runs) for (let i = 0; i < count; i++) out.push(first + i);
  return out;
}

// joinHead adds a `tab_more` frame to the `tab_head` it continues: more row
// runs and more styles, in order (TECH 12.2). The head is whole when the
// last frame added carries no `more`. The lists grow in place, so a head of
// many frames is not copied once for each.
export function joinHead(head, more) {
  if (more.rows) append(head.rows ??= [], more.rows);
  if (more.styles) append(head.styles ??= [], more.styles);
  if (more.more) head.more = true; else delete head.more;
  return head;
}

// joinDeltaPart adds a delta_part frame to the delta its earlier parts
// built (TECH 12, Appendix A). A part holds some of the delta's fields; a
// list field that an earlier part had goes on with the part's items. A
// row of cells goes on under the same row id, and an edit that ends with
// `more` goes on in the part's first edit, whose lists continue its own.
// Items are added one at a time: a list can hold more than a call takes
// as arguments. A part that does not fit the delta throws.
export function joinDeltaPart(d, part) {
  for (const [k, v] of Object.entries(part)) {
    if (k === 'type' || k === 'seq' || k === 'part' || k === 'parts') continue;
    if (!(k in d)) { d[k] = v; continue; }
    const list = d[k];
    if (!Array.isArray(list) || !Array.isArray(v)) throw new TypeError(`a delta part repeats ${k}`);
    let i = 0;
    const last = list[list.length - 1];
    if (k === 'cells' && last && v.length && last[0] === v[0][0]) {
      append(last[1], v[0][1]);
      i = 1;
    } else if (k === 'struct' && last?.more && v.length) {
      const next = v[0];
      if (next.op !== last.op || next.tab !== last.tab) throw new TypeError('a delta part goes on with another edit');
      delete last.more;
      for (const [f, x] of Object.entries(next)) {
        if (f === 'op' || f === 'tab') continue;
        if (!(f in last)) last[f] = x;
        else if (Array.isArray(last[f]) && Array.isArray(x)) append(last[f], x);
        else throw new TypeError(`a delta part repeats ${f} of an edit`);
      }
      i = 1;
    }
    for (; i < v.length; i++) list.push(v[i]);
  }
  return d;
}

// deltaWhole reports whether a joined delta has no edit that goes on.
export const deltaWhole = (d) => !d.struct?.some((e) => e.more);

function append(list, items) { for (const x of items) list.push(x); }

// packIds turns a list of ids into runs, merging consecutive ids.
export function packIds(ids) {
  const runs = [];
  for (const id of ids) {
    const last = runs[runs.length - 1];
    if (last && last[0] + last[1] === id) last[1]++;
    else runs.push([id, 1]);
  }
  return runs;
}

// ---- addresses ----

// colName names a zero-based column: A to Z, then AA to ZZ.
export function colName(c) {
  return c < 26 ? String.fromCharCode(65 + c) : String.fromCharCode(64 + Math.floor(c / 26)) + String.fromCharCode(65 + (c % 26));
}
export const address = (r, c) => colName(c) + (r + 1);
export function rangeName(b) {
  const a = address(b.r1, b.c1);
  return b.r1 === b.r2 && b.c1 === b.c2 ? a : `${a}:${address(b.r2, b.c2)}`;
}
// wirePosition is a [row, column] pair of zero-based screen positions as
// the wire carries it.
export const wirePosition = (r, c) => [r + POSITION_BASE, c + POSITION_BASE];
export const screenPosition = (at) => ({ r: at[0] - POSITION_BASE, c: at[1] - POSITION_BASE });

// ---- cells ----

// valueText is "the value as text": what a cell shows when its frame leaves
// `shown` out. A number reads as JavaScript prints it, a boolean as TRUE or
// FALSE, text and errors as they are.
export function valueText(value) {
  if (value == null) return '';
  if (typeof value === 'boolean') return value ? 'TRUE' : 'FALSE';
  return String(value);
}

// decodeCell reads CellOut = [col, kind, value?, shown?, edit?, style?].
// Trailing defaults are left out and a skipped middle field is null. A
// frame with no kind, or the kind "", is a blank cell: it carries a style
// when the cell keeps a format, and nothing when the cell is gone.
export function decodeCell(a) {
  const kind = a[1] || '';
  const value = a[2] ?? null;
  const shown = a[3] ?? valueText(value);
  return {
    col: a[0],
    kind: kind.replace('f', ''), // n, t, b, e or '' for blank
    formula: kind.endsWith('f'),
    value,
    shown,
    edit: a[4] ?? shown,
    style: a[5] ?? 0,
  };
}
export const isBlankCell = (c) => !c.kind && !c.formula && !c.style;

// ---- formats ----

// A Format carries the properties of rule 6 under the names of the agent
// interface (TECH 18.2): bold, italic, underline, strikethrough, text_color,
// fill, align, wrap and number. A property left out has its default. The
// style list of a tab is indexed from 0, and index 0 is the default format.
export const NUMBER_KINDS = [
  ['general', 'General'], ['number', 'Number'], ['percent', 'Percent'], ['currency', 'Currency'],
  ['date', 'Date'], ['time', 'Time'], ['datetime', 'Date and time'], ['text', 'Text'],
];
export const DATE_FORMS = ['yyyy-mm-dd', 'm/d/yyyy', 'mmm d, yyyy'];
export const TIME_FORMS = ['hh:mm', 'hh:mm:ss', 'h:mm AM/PM'];
export const HUES = ['gray', 'red', 'orange', 'yellow', 'green', 'teal', 'blue', 'purple', 'magenta'];
export const SHADES = [100, 200, 300, 400, 600, 800];

// numberFormat builds the `number` property for a kind with its settings.
// A date and a time carry their form as `pattern`; a date and time carries
// one form of each.
export function numberFormat(kind, s = {}) {
  switch (kind) {
    case 'number': return { kind, decimals: s.decimals ?? 2, thousands: s.thousands ?? true };
    case 'percent': return { kind, decimals: s.decimals ?? 2 };
    case 'currency': return { kind, symbol: s.symbol || '$', decimals: s.decimals ?? 2 };
    case 'date': return { kind, pattern: s.date || DATE_FORMS[0] };
    case 'time': return { kind, pattern: s.time || TIME_FORMS[0] };
    case 'datetime': return { kind, date: s.date || DATE_FORMS[0], time: s.time || TIME_FORMS[0] };
    default: return { kind };
  }
}
export const numberKind = (format) => format?.number?.kind || 'general';
// dateForm and timeForm read the forms of a number format as the server
// sends it.
export const dateForm = (n) => (n.kind === 'date' ? n.pattern : n.date) || DATE_FORMS[0];
export const timeForm = (n) => (n.kind === 'time' ? n.pattern : n.time) || TIME_FORMS[0];

// ---- operations of TECH 7.1 ----

// Ranges are id runs. Targets of a paste list ids one by one and may end
// with {new: k} for the k-th row or column the write adds.
export const ops = {
  set: (tab, row, col, text, at) => ({ op: 'set', tab, row, col, text, at }),
  write: (tab, rows, cols, values) => ({ op: 'write', tab, rows, cols, values }),
  clear: (tab, rows, cols) => ({ op: 'clear', tab, rows, cols }),
  format: (tab, rows, cols, set) => ({ op: 'format', tab, rows, cols, set }),
  width: (tab, cols, width) => ({ op: 'width', tab, cols, width }),
  copy: (from, to, mode, cut) => ({ op: 'copy', from, to, mode, cut }),
  fill: (tab, source, target) => ({ op: 'fill', tab, source, target }),
  insertRows: (tab, beside, side, count) => ({ op: 'insert_rows', tab, beside, side, count }),
  insertCols: (tab, beside, side, count) => ({ op: 'insert_cols', tab, beside, side, count }),
  appendRows: (tab, count) => ({ op: 'append_rows', tab, count }),
  appendCols: (tab, count) => ({ op: 'append_cols', tab, count }),
  deleteRows: (tab, ids) => ({ op: 'delete_rows', tab, ids }),
  deleteCols: (tab, ids) => ({ op: 'delete_cols', tab, ids }),
  sort: (tab, rows, by, desc) => ({ op: 'sort', tab, rows, by, desc }),
  settings: (tab, frozenRows, frozenCols) => ({ op: 'settings', tab, frozen_rows: frozenRows, frozen_cols: frozenCols }),
  addTab: (name) => ({ op: 'add_tab', name }),
  renameTab: (tab, name) => ({ op: 'rename_tab', tab, name }),
  moveTab: (tab, before) => ({ op: 'move_tab', tab, before }),
  deleteTab: (tab) => ({ op: 'delete_tab', tab }),
  undo: () => ({ op: 'undo' }),
  redo: () => ({ op: 'redo' }),
};

// ---- structural deltas ----

// A delta lists what changed the structure of the sheet in `struct`, in
// operation order: rows and columns inserted, deleted or sorted, and tabs
// renamed, deleted, added or moved. Every entry names its tab in `tab`
// (Appendix A, StructEdit), and a delta with an entry that does not is
// refused before it is read (deltaError). No entry carries cells: for a
// tab that is not watched the client needs it only to know that
// references into that tab moved (TECH 8.4).
const AXIS_OPS = new Set(['insert_rows', 'insert_cols', 'delete_rows', 'delete_cols', 'sort']);
const STRUCT_OPS = new Set([...AXIS_OPS, 'rename_tab', 'add_tab', 'move_tab', 'delete_tab']);

// structEdits reads the structural operations of delta d as a list of
// {tab, op, ...}. This is the one place that knows where a delta keeps
// them.
export const structEdits = (d) => d.struct ?? [];

// isAxisEdit says whether a structural operation changes rows or columns.
export const isAxisEdit = (e) => AXIS_OPS.has(e.op);

// isStructural says whether delta d changes a grid a formula editor is
// bound to: rows or columns of any tab inserted, deleted or sorted, or a
// tab renamed or deleted (TECH 8.4). A formula can name a cell of any tab,
// so a change on a tab that is not watched counts as much as one on the
// watched tab. An added or a moved tab changes no reference. tabs is the
// tab list before d. The tab list is read as well as `struct`: a changed
// name, a missing tab, or a changed row or column count is structural even
// when no operation names it.
export function isStructural(d, tabs) {
  if (structEdits(d).some((e) => e.op !== 'add_tab' && e.op !== 'move_tab')) return true;
  if (!d.tabs) return false;
  const next = new Map(d.tabs.map((t) => [t.id, t]));
  return tabs.some((t) => {
    const n = next.get(t.id);
    return !n || n.name !== t.name || n.rows !== t.rows || n.cols !== t.cols;
  });
}

// ---- the shape of an action ----

// An action holds content and format operations, or exactly one operation
// of another kind: an insert, an append, a delete or a sort of rows or
// columns, a tab operation, undo or redo (TECH 7.1). The host refuses a mix
// whole, as invalid, before any of it runs.
const TOGETHER = new Set(['set', 'write', 'clear', 'format', 'width', 'copy', 'fill', 'settings']);

// actionShapeError says why a list of operations is no action, or returns
// null.
export function actionShapeError(list) {
  if (!Array.isArray(list) || list.length < 1) return 'an action has at least one operation';
  if (list.length === 1) return null;
  const alone = list.find((op) => !TOGETHER.has(op.op));
  return alone ? `${alone.op} is an action of its own and cannot share one with ${list.length - 1} other ${list.length === 2 ? 'operation' : 'operations'}` : null;
}

// ---- fields that hold null ----

// nullPath names the place of a null in server frame m that Appendix A
// does not allow, such as `struct.0.tab` or `me.label`, or returns null. A
// server frame holds null in two places only: the `after` of a
// structural edit that inserts at the start of a tab, and the value, the
// shown text and the edit text a CellOut skips before a later item. A
// null never stands for a field left out, so a frame with one anywhere
// else does not fit, at whatever depth it stands and however its reader
// would take the field.
export function nullPath(m) {
  const type = m.type;
  const delta = type === 'delta' || type === 'delta_part';
  const path = [];
  const allowed = () => (path.length === 3 && delta && path[0] === 'struct' && path[2] === 'after' && /^insert_/.test(m.struct[path[1]].op))
    || (path.length === 5 && path[2] === 1 && path[4] >= 2 && path[4] <= 4
      && ((delta && path[0] === 'cells') || (type === 'rows' && path[0] === 'rows')));
  const walk = (v) => {
    if (v === null) return allowed() ? null : path.join('.');
    if (typeof v !== 'object') return null;
    if (Array.isArray(v)) {
      for (let i = 0; i < v.length; i++) {
        path.push(i);
        const at = walk(v[i]);
        path.pop();
        if (at != null) return at;
      }
      return null;
    }
    for (const k of Object.keys(v)) {
      path.push(k);
      const at = walk(v[k]);
      path.pop();
      if (at != null) return at;
    }
    return null;
  };
  return walk(m);
}
const nullText = (what, m) => {
  const at = nullPath(m);
  return at == null ? null : `${what} whose ${at} is null`;
};
// nullError says where frame m holds a null it may not hold, or returns
// null.
export const nullError = (m) => nullText(`a ${m.type} frame`, m);

// ---- the fields of the grid's frames ----

// isId says whether v fits an id of Appendix A: a tab, a row, a column or a
// request.
export const isId = (v) => Number.isSafeInteger(v) && v > 0;
const isSize = (v) => Number.isSafeInteger(v) && v >= 0;

// tabListError says why a tab list does not fit Appendix A, TabInfo[], or
// returns null. The page installs it whole, so every entry is read here.
// The page finds a tab by its id, so an id named twice could be either.
export function tabListError(list) {
  if (!Array.isArray(list)) return `a tab list that is a ${typeof list}`;
  const ids = new Set();
  for (const t of list) {
    if (t === null || typeof t !== 'object' || Array.isArray(t)) return 'a tab list with an entry that is no object';
    if (!isId(t.id) || typeof t.name !== 'string' || !isSize(t.rows) || !isSize(t.cols) || !isSize(t.frozen_rows) || !isSize(t.frozen_cols)) {
      return `a tab list whose entry ${JSON.stringify(t.id)} does not fit`;
    }
    if (ids.has(t.id)) return `a tab list that names tab ${t.id} twice`;
    ids.add(t.id);
  }
  return null;
}

// deltaError says why a delta, or one part of it, does not fit Appendix A
// in the fields the page reads beyond their type, or returns null. The
// sequence is decimal text, as the page compares it and sends it back as
// the base of an action and as its watermark. Each structural edit names
// a known operation and its tab, the tab list is whole, and the author is
// a session and an action number.
export function deltaError(m) {
  const nulled = nullText(`a ${m.type}`, m);
  if (nulled) return nulled;
  if (!isDecimal(m.seq)) return `a ${m.type} whose sequence is ${JSON.stringify(m.seq)}, not decimal text`;
  if (m.struct !== undefined) {
    if (!Array.isArray(m.struct)) return `a ${m.type} whose struct is a ${typeof m.struct}`;
    for (const e of m.struct) {
      if (e === null || typeof e !== 'object' || Array.isArray(e) || !STRUCT_OPS.has(e.op) || !isId(e.tab)) {
        return `a ${m.type} with a structural edit that names no operation or no tab`;
      }
    }
  }
  if (m.tabs !== undefined) {
    const wrong = tabListError(m.tabs);
    if (wrong) return `a ${m.type} with ${wrong}`;
  }
  if (m.date !== undefined && typeof m.date !== 'string') return `a ${m.type} whose date is a ${typeof m.date}, not text`;
  const f = m.from;
  if (f !== undefined && (typeof f !== 'object' || Array.isArray(f) || typeof f.session_id !== 'string' || !isDecimal(f.n))) {
    return `a ${m.type} whose author does not fit`;
  }
  return null;
}

// snapshotFrameError says why a frame of a snapshot does not fit Appendix
// A in the fields that place it, or returns null. They are read before the
// frame is matched to its snapshot: a generation that is no whole number
// could be that of the snapshot under way, so such a frame is not passed
// over as one of an older snapshot. A head carries the tab list and the
// date as of its sequence, which the page installs with the grid.
export function snapshotFrameError(m) {
  if (!isSize(m.generation)) return `a ${m.type} whose generation is a ${typeof m.generation}, not a whole number`;
  if (!isId(m.tab)) return `a ${m.type} whose tab is a ${typeof m.tab}, not an id`;
  if (m.more !== undefined && m.more !== true) return `a ${m.type} whose more is ${typeof m.more}`;
  if (m.type === 'tab_head') {
    if (!isDecimal(m.seq)) return `a tab_head whose sequence is ${JSON.stringify(m.seq)}, not decimal text`;
    if (typeof m.date !== 'string') return `a tab_head whose date is a ${typeof m.date}, not text`;
    const wrong = tabListError(m.tabs);
    if (wrong) return `a tab_head with ${wrong}`;
    if (!m.tabs.some((t) => t.id === m.tab)) return `a tab_head of tab ${m.tab}, which its tab list does not name`;
  }
  if (m.type === 'rows' && !isSize(m.part)) return `rows whose part is a ${typeof m.part}, not a whole number`;
  if (m.type === 'tab_done' && !isSize(m.parts)) return `a tab_done whose count is a ${typeof m.parts}, not a whole number`;
  return null;
}

// ---- acks ----

const STATUSES = new Set(['applied', 'ignored', 'refused']);
const DECIMAL = /^(0|[1-9][0-9]*)$/;
const DECIMAL_MAX = '18446744073709551615'; // 2^64 - 1, the largest the host reads
// isDecimal says whether v is decimal text as the host writes and reads
// it: without a sign or leading zeros, and at most DECIMAL_MAX.
const isDecimal = (v) => typeof v === 'string' && DECIMAL.test(v) && (v.length < DECIMAL_MAX.length || (v.length === DECIMAL_MAX.length && v <= DECIMAL_MAX));
const count = (v) => v === undefined || (Number.isSafeInteger(v) && v >= 0);
const reason = (v) => v === undefined || typeof v === 'string';

// ackError says why an ack is no answer to an action of ops operations
// (Appendix A), or returns null. An ack carries the number of its action,
// one result for each operation, the undo outcome when there is one, and
// the first row and column ids the action added when it added any.
// With ops null the ack answers no action in flight, and everything but
// the count of its results is checked.
export function ackError(m, ops) {
  const nulled = nullText('an ack', m);
  if (nulled) return nulled;
  if (!isDecimal(m.n)) return 'an ack without an action number';
  if (!isDecimal(m.seq)) return 'an ack without a sequence';
  if (!Array.isArray(m.results)) return `an ack whose results are a ${typeof m.results}, not a list`;
  if (ops != null && m.results.length !== ops) return `an ack with ${m.results.length} results for ${ops} operations`;
  for (const r of m.results) {
    if (r === null || typeof r !== 'object' || Array.isArray(r)) return 'an ack with a result that is no object';
    if (!STATUSES.has(r.status)) return `an ack with a result of status ${JSON.stringify(r.status)}`;
    if (!count(r.applied) || !count(r.ignored) || !reason(r.reason)) return 'an ack with a result whose counts or reason do not fit';
  }
  const u = m.undo;
  if (u !== undefined && (typeof u !== 'object' || Array.isArray(u) || !Number.isSafeInteger(u.applied) || u.applied < 0 || !Number.isSafeInteger(u.skipped) || u.skipped < 0 || !reason(u.reason))) {
    return 'an ack whose undo outcome does not fit';
  }
  const added = m.added;
  if (added !== undefined && (typeof added !== 'object' || Array.isArray(added) || (added.rows === undefined && added.cols === undefined) || (added.rows !== undefined && !isId(added.rows)) || (added.cols !== undefined && !isId(added.cols)))) {
    return 'an ack whose added ids do not fit';
  }
  return null;
}

// welcomeError says why a welcome does not fit Appendix A, or returns
// null. The session token and id, and the label, are present or absent,
// never empty: the page reads an empty one as none.
export function welcomeError(m) {
  const nulled = nullText('a welcome', m);
  if (nulled) return nulled;
  if (m.access !== 'private' && m.access !== 'public') return `a welcome whose access is ${JSON.stringify(m.access)}`;
  const me = m.me;
  if (me !== undefined && (typeof me !== 'object' || Array.isArray(me) || typeof me.verified !== 'boolean' || (me.label !== undefined && (typeof me.label !== 'string' || me.label === '')))) {
    return 'a welcome whose identity does not fit';
  }
  if (!isDecimal(m.seq)) return `a welcome whose sequence is a ${typeof m.seq}`;
  if (m.date !== undefined && typeof m.date !== 'string') return `a welcome whose date is a ${typeof m.date}, not text`;
  if (m.tabs !== undefined) {
    const wrong = tabListError(m.tabs);
    if (wrong) return `a welcome with ${wrong}`;
  }
  if ((m.tabs === undefined) !== (m.date === undefined)) return 'a welcome with one of its tab list and its date';
  for (const k of ['session', 'session_id']) {
    if (m[k] !== undefined && (typeof m[k] !== 'string' || m[k] === '')) return `a welcome whose ${k} is no text`;
  }
  return null;
}

// resyncError says why a resync does not fit Appendix A, or returns null.
// Every resync carries the tab list and the date as of its sequence: the
// page drops the deltas it held for its editor at a resync, and with them
// what they changed of both.
export function resyncError(m) {
  const nulled = nullText('a resync', m);
  if (nulled) return nulled;
  if (!Number.isSafeInteger(m.generation) || !isDecimal(m.seq)) {
    return `a resync without a generation or a sequence: generation ${typeof m.generation}, seq ${typeof m.seq}`;
  }
  if (m.tabs === undefined || m.date === undefined) return 'a resync without its tab list or its date';
  const wrong = tabListError(m.tabs);
  if (wrong) return `a resync with ${wrong}`;
  if (typeof m.date !== 'string') return `a resync whose date is a ${typeof m.date}, not text`;
  return null;
}

// overwrittenError says why an overwrite notice does not fit Appendix A,
// or returns null. The page finds the text it offers by the cell and the
// replaced sequence, so a sequence that is no decimal text names no commit.
export function overwrittenError(m) {
  const nulled = nullText('an overwrite notice', m);
  if (nulled) return nulled;
  if (!isId(m.tab) || !isId(m.row) || !isId(m.col)) return 'an overwrite notice whose cell does not fit';
  if (!isDecimal(m.replaced)) return `an overwrite notice whose replaced sequence is a ${typeof m.replaced}`;
  return null;
}

// peekedError says why a peeked frame does not fit Appendix A, or returns
// null.
export function peekedError(m) {
  const nulled = nullText('a peek answer', m);
  if (nulled) return nulled;
  if (!isId(m.id)) return 'a peek answer without a request id';
  if (!isDecimal(m.seq)) return 'a peek answer without a sequence';
  if (typeof m.edit !== 'string' || typeof m.formula !== 'boolean') return 'a peek answer whose text or formula mark does not fit';
  return null;
}

// rebasedError says why a rebased frame does not fit Appendix A, or
// returns null. A refusal names its reason and carries nothing else, so
// the client takes every refusal that fits as a refusal.
export function rebasedError(m) {
  const nulled = nullText('a rebase answer', m);
  if (nulled) return nulled;
  if (!isId(m.id)) return 'a rebase answer without a request id';
  if (m.refused !== undefined) {
    if (typeof m.refused !== 'string' || m.refused === '') return 'a rebase refusal without a text reason';
    if (m.text !== undefined || m.at !== undefined || m.base !== undefined) return 'a rebase refusal that also carries an answer';
    return null;
  }
  if (m.text !== undefined && typeof m.text !== 'string') return 'a rebase answer whose text is no text';
  if (m.at !== undefined && !(Array.isArray(m.at) && m.at.length === 2 && m.at.every((v) => Number.isSafeInteger(v) && v >= POSITION_BASE))) return 'a rebase answer whose position does not fit';
  if (m.base !== undefined && !isDecimal(m.base)) return 'a rebase answer whose base is no sequence';
  return null;
}

// ---- action framing ----

export class TooLargeError extends Error {}

export function utf8Length(s) {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) { n += 4; i++; }
    else n += 3;
  }
  return n;
}

// jsonCost is the encoded size of the code unit(s) at i inside a JSON
// string literal, and how many code units they take.
function jsonCost(s, i) {
  const c = s.charCodeAt(i);
  if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) return [2, 4];
  if (c >= 0xd800 && c <= 0xdfff) return [1, 6];
  if (c === 0x22 || c === 0x5c || c === 0x08 || c === 0x09 || c === 0x0a || c === 0x0c || c === 0x0d) return [1, 2];
  if (c < 0x20) return [1, 6];
  return [1, c < 0x80 ? 1 : c < 0x800 ? 2 : 3];
}

// actionFrames encodes an action as one frame, or as action_part frames
// that carry slices of its JSON text when it passes the frame limit. Parts
// are numbered from 0.
export function actionFrames(action) {
  const whole = JSON.stringify(frame('action', { n: action.n, base: action.base, ops: action.ops }));
  if (utf8Length(whole) <= FRAME_MAX) return [whole];
  if (utf8Length(whole) > ACTION_MAX) throw new TooLargeError('action over 8 MiB');
  const budget = FRAME_MAX - utf8Length(JSON.stringify(frame('action_part', { n: action.n, part: PARTS_MAX, parts: PARTS_MAX, data: '' })));
  const slices = [];
  let start = 0;
  let enc = 0;
  for (let i = 0; i < whole.length;) {
    const [units, e] = jsonCost(whole, i);
    if (enc + e > budget) { slices.push(whole.slice(start, i)); start = i; enc = 0; }
    enc += e;
    i += units;
  }
  slices.push(whole.slice(start));
  if (slices.length > PARTS_MAX) throw new TooLargeError('action needs more than 32 parts');
  return slices.map((s, i) => JSON.stringify(frame('action_part', { n: action.n, part: i, parts: slices.length, data: s })));
}

// ---- refusal reasons ----

const LIMIT_TEXT = {
  tabs: 'a sheet holds at most 50 tabs',
  rows: 'a tab holds at most 100,000 rows',
  columns: 'a tab holds at most 702 columns',
  cells: 'a sheet holds at most 500,000 cells with content or a format',
  formulas: 'a sheet holds at most 100,000 formulas',
  text_length: 'a text cell holds at most 32,767 characters',
  formula_length: 'a formula holds at most 8,192 characters',
  formula_depth: 'a formula nests brackets and functions at most 64 levels deep',
  formula_reads: 'one formula reads at most 1,000,000 cells',
  sheet_reads: 'the formulas of a sheet read at most 20,000,000 cells together',
  sheet_text: 'the cells of a sheet hold at most 50,000,000 characters of typed text',
  recalculation_time: 'it would take too long to carry out',
  action_changes: 'it changes too much at once; try it in smaller steps',
  disk_quota: 'the sheet is over its disk quota, so no edit is accepted until space is freed or the quota is raised',
  stale_formula: 'the grid changed since the formula was typed and the host can no longer adjust its references',
  gone: 'its row, column or tab was deleted before the host received it',
  unavailable: 'the host is busy; try again in a moment',
  invalid: 'the host could not read the edit',
};

// UNDO_SKIPS says, for each reason an undo or a redo gives for a change it
// skipped (rule 23), why the change stays as it is. "it" is the undo.
const UNDO_SKIPS = {
  changed: 'someone else changed the same value after you. Their value stays',
  written: 'someone else has written in the rows, columns or tab it would remove. They stay, with their work',
  sorted: 'someone else sorted these rows after you. Their order stays',
  last_tab: 'it would delete the only tab of the sheet. A sheet keeps at least one tab',
  empty_tab: 'it would delete the only rows or the only columns of a tab. A tab keeps at least one row and one column',
  tab_name_taken: 'another tab now has the name it would give back. Both tabs keep their names',
  recovered: 'someone has recovered that tab from an earlier revision since. The recovered tab stays as it is',
  // The host lost a scratch file, its record of cleared cells, or its log
  // of recent edits: the words fit all of them.
  lost: 'the host no longer has what it had saved for it',
};

// undoSkipText words the notice for an undo or redo that skipped changes.
// u is the ack's {applied, skipped, reason}. The ack counts every skipped
// change and names one reason, that of the first one it skipped, so with
// more than one skipped change the notice does not put them all down to
// it. A change is one value group of one cell, or one structural step, so
// the count can be larger than the number of cells. A reason this client
// does not know gets words that blame no one.
export function undoSkipText(redo, u) {
  const done = redo ? 'redone' : 'undone';
  // A step that is lost as a whole reads better in one sentence.
  if (u.reason === 'lost' && !u.applied && u.skipped === 1) return `This step could not be ${done}: ${UNDO_SKIPS.lost}. Earlier steps are still available.`;
  const why = UNDO_SKIPS[u.reason] || `it no longer fits the sheet as it is now${u.reason ? ` (the host says: ${u.reason})` : ''}`;
  const count = `${u.skipped} ${u.skipped === 1 ? 'change' : 'changes'}`;
  let text = `Your ${redo ? 'redo' : 'undo'} skipped ${count}: ${why}.`;
  if (u.skipped === 2) text += ' The other skipped change can have another reason.';
  if (u.skipped > 2) text += ' The other skipped changes can have other reasons.';
  return `${text} ${u.applied ? `The rest was ${done}.` : `Nothing was ${done}.`}`;
}

// reasonText turns a Result reason into the words of a notice.
export const reasonText = (reason) => LIMIT_TEXT[reason] || (reason ? `the host answered "${reason}"` : 'the host gave no reason');
