// A scripted fake server for the sheet client's Node tests. It speaks the
// wire schema of Appendix A through fake sockets: it reads the text of
// every client frame against the schema, answers hello, watch,
// action, peek and rebase the way the design says, keeps receipts so a
// resent action never applies twice, sends the structural operations of
// every tab to every connection, and lets a test push deltas or take over
// any answer. It reads nothing as a formula: a typed string is stored and shown
// as it is, which is all the client's protocol handling needs.
'use strict';

import { fire, installDOM } from './dom.mjs';
import { FRAME_MAX, SERVER_FRAME_MAX, utf8Length } from '../protocol.js';

// What the server reads (Appendix A and section 7.1). A frame is read
// from its text, not from a parsed copy, which shows neither a field that
// stands twice nor a number written with a fraction.
// readFrame returns the frame, or throws a Refused that says how the
// engine answers: with a protocol error, which ends the connection, or by
// refusing an operation of the action as invalid. The client's tests fail
// on either. web/testdata/frames.json holds frames the engine is checked
// against too, so this reading cannot drift from the engine unnoticed.
export class Refused extends Error {
  constructor(outcome, why) { super(why); this.outcome = outcome; }
}
const protocol = (why) => { throw new Refused('protocol', why); };
const invalid = (why) => { throw new Refused('invalid', why); };

// FRACTION stands for a number that is no integer of at most 18 digits.
// No field takes one.
const FRACTION = Symbol('a number that is no integer');
const NUMBER = /-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/y;
const INTEGER = /^(0|-?[1-9][0-9]{0,17})$/;

// parseStrict reads JSON text the way the engine does. An object has each
// of its names once, compared after their escapes are read.
export function parseStrict(text) {
  let i = 0;
  const space = () => { while (text[i] === ' ' || text[i] === '\t' || text[i] === '\n' || text[i] === '\r') i++; };
  const string = () => {
    let end = i + 1;
    while (end < text.length && text[end] !== '"') end += text[end] === '\\' ? 2 : 1;
    if (end >= text.length) protocol('a text that does not end');
    let s;
    try { s = JSON.parse(text.slice(i, end + 1)); } catch { protocol('a text that is no JSON string'); }
    i = end + 1;
    // Half of a surrogate pair reads as U+FFFD (Appendix A). JSON.parse
    // keeps the half, so the server would store a text the engine never
    // holds.
    return s.toWellFormed();
  };
  const value = () => {
    space();
    const c = text[i];
    if (c === '"') return string();
    if (c === '{') {
      i++;
      const out = {};
      const seen = new Set();
      space();
      if (text[i] === '}') { i++; return out; }
      for (;;) {
        space();
        if (text[i] !== '"') protocol('a field without a name');
        const name = string();
        if (seen.has(name)) protocol(`the field ${name} twice`);
        seen.add(name);
        space();
        if (text[i++] !== ':') protocol('a field without a value');
        // defineProperty makes a field named __proto__ a field like any other.
        Object.defineProperty(out, name, { value: value(), enumerable: true, writable: true, configurable: true });
        space();
        if (text[i] === '}') { i++; return out; }
        if (text[i++] !== ',') protocol('an object that does not end');
      }
    }
    if (c === '[') {
      i++;
      const out = [];
      space();
      if (text[i] === ']') { i++; return out; }
      for (;;) {
        out.push(value());
        space();
        if (text[i] === ']') { i++; return out; }
        if (text[i++] !== ',') protocol('a list that does not end');
      }
    }
    for (const [word, v] of [['true', true], ['false', false], ['null', null]]) {
      if (text.startsWith(word, i)) { i += word.length; return v; }
    }
    NUMBER.lastIndex = i;
    const m = NUMBER.exec(text);
    if (!m) protocol('no JSON value');
    i += m[0].length;
    return INTEGER.test(m[0]) ? Number(m[0]) : FRACTION;
  };
  const v = value();
  space();
  if (i !== text.length) protocol('text after the JSON value');
  return v;
}

// A shape is the JSON type of a place. A value of another type, a null
// where none is named, an unknown field and a list of a fixed length with
// another number of items are protocol errors wherever they stand.
const TEXT = { t: 'text' };
const INT = { t: 'int' };
const FLAG = { t: 'flag' };
const list = (item, count) => ({ t: 'list', item, count });
const object = (fields, required = Object.keys(fields)) => ({ t: 'object', fields, required });
const oneOf = (...alts) => ({ t: 'oneOf', alts });
const orNull = (s) => ({ ...s, nul: true });
// A variant is an object whose fields depend on its kind: `tag` names the
// kind, `kinds` lists per kind [the fields it needs, the ones it may carry].
const variant = (tag, fields, kinds, open = false) => ({ t: 'object', fields, tag, kinds, open });

const typeOf = (v) => (v === null ? 'null' : Array.isArray(v) ? 'list' : v === FRACTION ? 'fraction' : { string: 'text', number: 'int', boolean: 'flag', object: 'object' }[typeof v]);

function shaped(v, s, at) {
  const t = typeOf(v);
  if (t === 'null') { if (!s.nul) protocol(`${at} is null`); return; }
  if (s.t === 'oneOf') {
    const alt = s.alts.find((a) => a.t === t);
    if (!alt) protocol(`${at} is a ${t}`);
    shaped(v, alt, at);
    return;
  }
  if (s.t !== t) protocol(`${at} is a ${t}, not a ${s.t}`);
  if (t === 'list') {
    if (s.count !== undefined && v.length !== s.count) protocol(`${at} has ${v.length} items, not ${s.count}`);
    v.forEach((item, k) => shaped(item, s.item, `${at}[${k}]`));
  } else if (t === 'object') {
    for (const k of Object.keys(v)) {
      if (!Object.hasOwn(s.fields, k)) protocol(`${at} has the unknown field ${k}`);
      shaped(v[k], s.fields[k], `${at}.${k}`);
    }
    let required = s.required;
    if (s.tag) {
      if (!Object.hasOwn(v, s.tag)) protocol(`${at} lacks ${s.tag}`);
      const kind = Object.hasOwn(s.kinds, v[s.tag]) ? s.kinds[v[s.tag]] : null;
      if (!kind && !s.open) protocol(`${at} is of the unknown kind ${v[s.tag]}`);
      if (!kind) return;
      for (const k of Object.keys(v)) if (k !== s.tag && !kind[0].includes(k) && !kind[1].includes(k)) protocol(`${at} of kind ${v[s.tag]} has the field ${k}`);
      required = kind[0];
    }
    for (const k of required) if (!Object.hasOwn(v, k)) protocol(`${at} lacks the field ${k}`);
  }
}

// Ids or Target[]: "all", or a list of ids, [first, count] runs and {new: k}.
const IDS = oneOf(TEXT, list(oneOf(INT, list(INT), object({ new: INT }))));
// The Ids of a presence range: "all", or runs alone.
const RUNS = oneOf(TEXT, list(list(INT)));
const NUMBER_FORMAT = variant('kind', { kind: TEXT, symbol: TEXT, decimals: INT, thousands: FLAG, pattern: TEXT, date: TEXT, time: TEXT }, {
  general: [[], []], text: [[], []], number: [['decimals', 'thousands'], []], percent: [['decimals'], []], currency: [['symbol', 'decimals'], []],
  date: [['pattern'], []], time: [['pattern'], []], datetime: [['date', 'time'], []],
});
const FORMAT = object({
  bold: orNull(FLAG), italic: orNull(FLAG), underline: orNull(FLAG), strikethrough: orNull(FLAG), wrap: orNull(FLAG),
  text_color: orNull(TEXT), fill: orNull(TEXT), align: orNull(TEXT), number: orNull(NUMBER_FORMAT),
}, []);
const RANGE = object({ rows: IDS, cols: IDS });
const TAB_RANGE = object({ tab: INT, rows: IDS, cols: IDS });
const OP = variant('op', {
  op: TEXT, tab: INT, row: INT, col: INT, text: TEXT, at: list(INT, 2), rows: IDS, cols: IDS, ids: IDS, values: list(list(TEXT)),
  set: FORMAT, width: INT, from: TAB_RANGE, to: TAB_RANGE, source: RANGE, target: RANGE, mode: TEXT, cut: FLAG,
  beside: INT, side: TEXT, count: INT, by: INT, desc: FLAG, frozen_rows: INT, frozen_cols: INT, name: TEXT, before: orNull(INT),
}, {
  set: [['tab', 'row', 'col', 'text'], ['at']],
  write: [['tab', 'rows', 'cols', 'values'], []],
  clear: [['tab', 'rows', 'cols'], []],
  format: [['tab', 'rows', 'cols', 'set'], []],
  width: [['tab', 'cols', 'width'], []],
  copy: [['from', 'to', 'mode'], ['cut']],
  fill: [['tab', 'source', 'target'], ['mode']],
  insert_rows: [['tab', 'beside', 'side', 'count'], []],
  insert_cols: [['tab', 'beside', 'side', 'count'], []],
  append_rows: [['tab', 'count'], []],
  append_cols: [['tab', 'count'], []],
  delete_rows: [['tab', 'ids'], []],
  delete_cols: [['tab', 'ids'], []],
  sort: [['tab', 'rows', 'by'], ['desc']],
  settings: [['tab', 'frozen_rows', 'frozen_cols'], []],
  add_tab: [[], ['name', 'before']],
  rename_tab: [['tab', 'name'], []],
  move_tab: [['tab'], ['before']],
  delete_tab: [['tab'], []],
  undo: [[], []],
  redo: [[], []],
}, true);
const FRAME = variant('type', {
  type: TEXT, version: INT, session: TEXT, watermark: TEXT, tab: INT, name: TEXT, pending: INT, generation: INT,
  n: TEXT, base: TEXT, ops: list(OP), part: INT, parts: INT, data: TEXT, id: INT, row: INT, col: INT, text: TEXT,
  at: list(INT, 2), through: TEXT, active: list(INT, 2), range: object({ rows: RUNS, cols: RUNS }), editing: FLAG,
}, {
  hello: [['version', 'pending'], ['session', 'watermark', 'tab', 'name']],
  watch: [['generation', 'tab'], []],
  action: [['n', 'base', 'ops'], []],
  action_part: [['n', 'part', 'parts', 'data'], []],
  peek: [['id', 'tab', 'row', 'col'], []],
  rebase: [['id', 'tab', 'row', 'col', 'text', 'at', 'base', 'through'], []],
  presence: [['tab'], ['active', 'range', 'editing']],
});

// What a field holds beyond its JSON type. In a frame a value outside
// these is a protocol error; in an operation it refuses the operation as
// invalid, and the action is read.
const DECIMAL = /^(0|[1-9][0-9]*)$/;
const U64_MAX = 2n ** 64n - 1n;
const is = {
  whole: (v) => Number.isSafeInteger(v),
  id: (v) => Number.isSafeInteger(v) && v > 0,
  index: (v) => v >= 0 && v < 2 ** 31,
  decimal: (v) => DECIMAL.test(v) && BigInt(v) <= U64_MAX,
};
const PALETTE = /^(gray|red|orange|yellow|green|teal|blue|purple|magenta)-(100|200|300|400|600|800)$/;
const DATE_PATTERNS = ['yyyy-mm-dd', 'm/d/yyyy', 'mmm d, yyyy'];
const TIME_PATTERNS = ['hh:mm', 'hh:mm:ss', 'h:mm AM/PM'];
const SYMBOL = /^[\p{L}\p{Sc}]{1,3}$/u;

// The operations an action can hold together. Any other is alone in its
// action. The server does not read a mix.
const TOGETHER = new Set(['set', 'write', 'clear', 'format', 'width', 'copy', 'fill', 'settings']);

// checkIds holds Ids or Target[] to an axis of at most limit rows or
// columns and returns how many it names, or null for "all", which names
// as many as the tab has.
function checkIds(v, limit, grow) {
  if (typeof v === 'string') { if (v !== 'all') invalid(`ids ${JSON.stringify(v)}`); return null; }
  let total = 0;
  for (const x of v) {
    if (Array.isArray(x)) {
      if (x.length !== 2 || !is.id(x[0]) || x[1] < 1 || x[1] > limit - total) invalid(`the run ${JSON.stringify(x)}`);
      total += x[1];
    } else if (typeof x === 'object') {
      if (x.new < 1 || x.new > limit || !grow) invalid(`the target ${JSON.stringify(x)}`);
      total++;
    } else {
      if (!is.id(x)) invalid(`the id ${x}`);
      total++;
    }
    if (total > limit) invalid('more ids than a tab holds');
  }
  return total;
}

function checkFormat(set) {
  for (const k of ['text_color', 'fill']) if (set[k] != null && !PALETTE.test(set[k])) invalid(`the color ${set[k]}`);
  if (set.align != null && !['general', 'left', 'center', 'right'].includes(set.align)) invalid(`the alignment ${set.align}`);
  const n = set.number;
  if (n == null) return;
  if ('decimals' in n && (n.decimals < 0 || n.decimals > 10)) invalid(`${n.decimals} decimals`);
  if ('symbol' in n && !SYMBOL.test(n.symbol)) invalid(`the symbol ${n.symbol}`);
  if (n.kind === 'date' && !DATE_PATTERNS.includes(n.pattern)) invalid(`the date pattern ${n.pattern}`);
  if (n.kind === 'time' && !TIME_PATTERNS.includes(n.pattern)) invalid(`the time pattern ${n.pattern}`);
  if (n.kind === 'datetime' && !(DATE_PATTERNS.includes(n.date) && TIME_PATTERNS.includes(n.time))) invalid('the date and time patterns');
}

const ROWS_MAX = 100000;
const COLS_MAX = 702;

// checkOp holds one operation to what the engine applies. A value it
// refuses makes the operation invalid.
function checkOp(op) {
  if (!Object.hasOwn(OP.kinds, op.op)) invalid(`unknown operation ${JSON.stringify(op.op)}`);
  for (const k of ['tab', 'row', 'col', 'beside', 'by']) if (k in op && !is.id(op[k])) invalid(`the id ${k}: ${op[k]}`);
  for (const k of ['from', 'to']) if (k in op && !is.id(op[k].tab)) invalid(`the tab of ${k}`);
  if (op.before != null && !is.id(op.before)) invalid(`before ${op.before}`);
  if ('side' in op && op.side !== 'before' && op.side !== 'after') invalid(`the side ${op.side}`);
  if ('at' in op && !op.at.every(is.index)) invalid(`at ${JSON.stringify(op.at)}`);
  switch (op.op) {
    case 'write': {
      // Every id of a run has its own values. The server holds the values
      // to "all" when it applies the operation, where it has the tab.
      const rows = checkIds(op.rows, ROWS_MAX, true);
      const cols = checkIds(op.cols, COLS_MAX, true);
      if ((rows !== null && op.values.length !== rows) || (cols !== null && op.values.some((line) => line.length !== cols))) invalid('values of another size than its rows and columns');
      break;
    }
    case 'clear': case 'format':
      checkIds(op.rows, ROWS_MAX, false);
      checkIds(op.cols, COLS_MAX, false);
      if (op.op === 'format') checkFormat(op.set);
      break;
    case 'width':
      checkIds(op.cols, COLS_MAX, false);
      if (op.width < 40 || op.width > 1000) invalid(`the width ${op.width}`);
      break;
    case 'copy': case 'fill': {
      const [from, to] = op.op === 'copy' ? [op.from, op.to] : [op.source, op.target];
      if (op.op === 'copy' && op.mode !== 'all' && op.mode !== 'values') invalid(`the mode ${op.mode}`);
      // A fill without a mode is a copy.
      if (op.op === 'fill' && 'mode' in op && !['copy', 'auto', 'series'].includes(op.mode)) invalid(`the mode ${op.mode}`);
      checkIds(from.rows, ROWS_MAX, false);
      checkIds(from.cols, COLS_MAX, false);
      checkIds(to.rows, ROWS_MAX, true);
      checkIds(to.cols, COLS_MAX, true);
      break;
    }
    case 'insert_rows': case 'insert_cols': case 'append_rows': case 'append_cols':
      if (op.count < 1) invalid(`the count ${op.count}`);
      break;
    case 'delete_rows': case 'delete_cols':
      checkIds(op.ids, op.op === 'delete_rows' ? ROWS_MAX : COLS_MAX, false);
      if (op.ids === 'all') invalid('a tab keeps at least one row and one column');
      break;
    case 'sort': checkIds(op.rows, ROWS_MAX, false); break;
    case 'settings':
      if (op.frozen_rows < 0 || op.frozen_rows > 20 || op.frozen_cols < 0 || op.frozen_cols > 10) invalid('frozen counts outside a tab\'s');
      break;
  }
}

// checkRange holds the range of a presence frame to what the engine reads
// of it whatever the sheet holds: the rows and the columns are each "all"
// or runs [first, count] that name ids, do not overlap and hold no more
// ids than a tab can have rows or columns, and they are at most 64 runs
// together.
function checkRange(range, bad) {
  let runs = 0;
  for (const [list, limit] of [[range.rows, ROWS_MAX], [range.cols, COLS_MAX]]) {
    if (typeof list === 'string') {
      if (list !== 'all') bad('a range that is neither all nor runs');
      continue;
    }
    let total = 0;
    for (const [i, run] of list.entries()) {
      if (run.length !== 2) bad('a run that is not [first, count]');
      const [first, count] = run;
      if (first < 1 || count < 1) bad('a run that names no ids');
      if ((total += count) > limit) bad('more ids than a tab can hold');
      for (const [a, n] of list.slice(0, i)) if (first < a + n && a < first + count) bad('runs that overlap');
    }
    runs += list.length;
  }
  if (runs > 64) bad('more than 64 runs');
}

// readFrame reads the text of one client frame.
export function readFrame(text) {
  const m = parseStrict(text);
  shaped(m, FRAME, 'the frame');
  const bad = (why) => protocol(`frame ${m.type}: ${why}`);
  for (const k of ['tab', 'row', 'col']) if (k in m && !is.id(m[k])) bad(`${k} is no id`);
  for (const k of ['version', 'pending', 'generation', 'part', 'parts', 'id']) if (k in m && !is.whole(m[k])) bad(`${k} is no number a frame holds`);
  switch (m.type) {
    case 'hello':
      if (m.version !== 1 || m.pending < 0 || ('watermark' in m && !is.decimal(m.watermark))) bad('a hello the server does not take');
      break;
    case 'action':
      if (!is.decimal(m.n) || m.n === '0' || !is.decimal(m.base)) bad('n or base is no sequence');
      if (m.ops.length === 0 || m.ops.length > 1000) bad(`${m.ops.length} operations`);
      // An action of more than one operation holds content and format
      // operations only (7.1).
      if (m.ops.length > 1 && m.ops.some((op) => !TOGETHER.has(op.op))) bad('an operation that is an action of its own, among others');
      for (const op of m.ops) checkOp(op);
      break;
    case 'action_part':
      if (!is.decimal(m.n) || m.parts < 1 || m.parts > 32 || m.part < 0 || m.part >= m.parts || m.data === '') bad(`part ${m.part} of ${m.parts}`);
      break;
    case 'rebase':
      if (!m.at.every(is.index) || !is.decimal(m.base) || !is.decimal(m.through) || BigInt(m.through) < BigInt(m.base)) bad('a rebase the server does not take');
      break;
    case 'presence':
      if ('active' in m && !m.active.every(is.id)) bad('an active cell that names no ids');
      if ('range' in m) checkRange(m.range, bad);
      break;
  }
  return m;
}

// checkFrame reads a frame a test built as an object, through its text.
export const checkFrame = (m) => readFrame(JSON.stringify(m));

// deltaFrames yields the texts a delta goes out in, cut the way the host
// cuts one that passes the server frame limit (splitDelta in
// deltapart.go): delta_part frames that each hold some of its fields, a
// list going on over parts, a row of cells under the same row id and an
// edit marked more in the first edit of the next part. The delta is
// encoded item by item, so no text of its whole size is built, and the
// parts are counted first and then built one at a time.
export function* deltaFrames(d) {
  const fields = Object.entries(d).filter(([k]) => k !== 'type' && k !== 'seq');
  const room = SERVER_FRAME_MAX - 256;
  let whole = 0;
  for (const [k, v] of fields) whole += utf8Length(k) + 4 + encodedSize(v);
  if (whole + 64 <= room) { yield JSON.stringify({ type: 'delta', ...d }); return; }
  const parts = cutDelta(fields, room);
  for (let i = 0; i < parts.length; i++) {
    let text = `{"type":"delta_part","seq":${JSON.stringify(d.seq)},"part":${i},"parts":${parts.length}`;
    for (const p of parts[i]) text += typeof p === 'string' ? p : JSON.stringify(p.v);
    yield text;
  }
}

const encodedSize = (v) => (Array.isArray(v) ? v.reduce((n, x) => n + encodedSize(x) + 1, 1) : utf8Length(JSON.stringify(v)));

// cutDelta lays out the parts as pieces: texts, and values to encode when
// the part is built.
function cutDelta(fields, room) {
  const parts = [];
  let cur = [];
  let size = 0;
  let base = 0;
  const open = []; // {reopen, cut, end, list, n}
  const add = (piece, n) => { cur.push(piece); size += n; };
  const text = (s) => add(s, utf8Length(s));
  const fits = (n) => size + n + 1 + open.reduce((c, l) => c + l.cut.length, 0) <= room;
  const sep = () => (open.at(-1)?.list && open.at(-1).n > 0 ? ',' : '');
  const flush = () => {
    for (let i = open.length - 1; i >= 0; i--) text(open[i].cut);
    text('}');
    parts.push(cur);
    cur = [];
    size = 0;
    open.forEach((l, i) => { text(l.reopen); l.n = 0; if (i > 0) open[i - 1].n = 1; });
    base = size;
  };
  // item writes pieces of n bytes whole, here or in the next part, and
  // reports false when they fit no part of their own.
  const item = (pieces, n) => {
    if (!fits(sep().length + n) && size > base) flush();
    if (!fits(sep().length + n)) return false;
    text(sep());
    for (const p of pieces) add(p, 0);
    size += n;
    if (open.length) open.at(-1).n++;
    return true;
  };
  const value = (v) => ({ v });
  const member = (k, v) => item([`,"${k}":`, value(v)], utf8Length(k) + 4 + encodedSize(v));
  const push = (reopen, cut, end, list) => { text(sep() + reopen); if (open.length) open.at(-1).n++; open.push({ reopen, cut, end, list, n: 0 }); };
  const pop = () => text(open.pop().end);
  const list = (k, items, each) => {
    push(`,"${k}":[`, ']', ']', true);
    for (const x of items) if (!item([value(x)], encodedSize(x)) && !each(x)) throw new Error(`a ${k} item fits no part`);
    pop();
  };
  for (const [k, v] of fields) {
    if (member(k, v)) continue;
    if (!Array.isArray(v)) throw new Error(`the field ${k} fits no part`);
    list(k, v, (x) => {
      if (k === 'cells') {
        push(`[${JSON.stringify(x[0])},[`, ']]', ']]', true);
        for (const c of x[1]) if (!item([value(c)], encodedSize(c))) return false;
        pop();
        return true;
      }
      if (k !== 'struct') return false;
      push(`{"op":${JSON.stringify(x.op)},"tab":${JSON.stringify(x.tab)}`, ',"more":true}', '}', false);
      for (const [f, y] of Object.entries(x)) {
        if (f === 'op' || f === 'tab' || member(f, y)) continue;
        if (!Array.isArray(y)) return false;
        list(f, y, () => false);
      }
      pop();
      return true;
    });
  }
  flush();
  return parts;
}

export class FakeSocket {
  constructor(server) { this.server = server; this.sent = []; this.closed = false; this.parts = ''; this.queue = []; }
  send(s) {
    // The host reads no frame over the limit and ends the connection.
    if (utf8Length(s) > FRAME_MAX) { this.drop(); return; }
    const m = readFrame(s);
    this.sent.push(m);
    if (this.server) this.server.receive(this, m);
  }
  close() { this.closed = true; }
  // open plays the socket's opening and returns the hello it carried.
  open() { const i = this.sent.length; this.onopen(); return this.sent[i]; }
  // recv delivers a frame. A delta larger than a frame arrives in parts,
  // as the host sends it.
  recv(m) {
    if (this.closed) return;
    if (m.type !== 'delta') { this.onmessage({ data: JSON.stringify(m) }); return; }
    for (const data of deltaFrames(m)) if (!this.closed) this.onmessage({ data });
  }
  drop() { this.closed = true; if (this.onclose) this.onclose(); }
  frames(type) { return this.sent.filter((m) => m.type === type); }
}

// Manual timers: tests advance time explicitly.
export class Timers {
  constructor() { this.now = 0; this.queue = []; this.seq = 0; }
  setTimeout(fn, ms = 0) { const id = ++this.seq; this.queue.push({ id, at: this.now + ms, fn }); return id; }
  clearTimeout(id) { this.queue = this.queue.filter((t) => t.id !== id); }
  advance(ms) {
    const end = this.now + ms;
    for (;;) {
      this.queue.sort((a, b) => a.at - b.at || a.id - b.id);
      const t = this.queue[0];
      if (!t || t.at > end) break;
      this.queue.shift();
      this.now = t.at;
      t.fn();
    }
    this.now = end;
  }
}

const runs = (ids) => {
  const out = [];
  for (const id of ids) { const l = out.at(-1); if (l && l[0] + l[1] === id) l[1]++; else out.push([id, 1]); }
  return out;
};
const expand = (r) => r.flatMap(([first, count]) => Array.from({ length: count }, (_, i) => first + i));
// targets lists what Ids or Target[] names on an axis, one by one: every
// id of "all", the ids of each run, each single id, and null for a row or
// column the operation adds.
const targets = (list, axis) => (list === 'all' ? [...axis]
  : list.flatMap((x) => (Array.isArray(x) ? expand([x]) : typeof x === 'object' ? [null] : [x])));

// structField is the `struct` field of a delta as a connection gets it:
// the structural operations of the sequence in operation order, for every
// tab, each with the id of its tab. It is the same for every connection,
// whichever tab the connection watches. This is the one place the tests
// take that shape from (protocol.js structEdits reads it), and
// TestFakeServerConformance compares it with the engine's.
export function structField(edits, watched) {
  void watched;
  return (edits || []).map((e) => ({ ...e }));
}

// cellOut encodes a stored cell as CellOut with trailing defaults left out.
export function cellOut(col, c) {
  if (!c) return [col];
  const out = [col, c.kind, c.value ?? null, c.shown ?? null, c.edit ?? null, c.style ?? null];
  while (out.length > 2 && out.at(-1) == null) out.pop();
  return out;
}

// typed stores a typed string the simplest way: a number when it reads as
// one, a formula marker when it starts with "=", text otherwise.
function typed(text, style) {
  if (text === '') return style ? { kind: '', style } : null;
  if (text.startsWith('=') && text.length > 1) return { kind: 'nf', value: 0, formula: text, style };
  if (/^-?\d+(\.\d+)?$/.test(text)) return { kind: 'n', value: Number(text), style };
  return { kind: 't', value: text, style };
}

// plainFormat is a format as the list holds it: only the properties that
// differ from the default, in one order. A property an operation reset
// with null, a mark that is off, the general alignment and the general
// number format are the default.
function plainFormat(f) {
  const out = {};
  for (const k of Object.keys(f).sort()) {
    const v = f[k];
    if (v == null || v === false || (k === 'align' && v === 'general') || (k === 'number' && v.kind === 'general')) continue;
    out[k] = v;
  }
  return out;
}

export class FakeServer {
  // me is the identity the route verified; null is a route without one,
  // where a session's label is the name its first hello gave.
  // newTab is the size of a tab that an add_tab adds.
  constructor({ access = 'private', me = { label: 'Ann', verified: true }, tabs = [{ id: 1, name: 'Sheet1', rows: 20, cols: 6 }], newTab = { rows: 20, cols: 6 } } = {}) {
    this.access = access;
    this.me = me;
    this.newTab = newTab;
    this.seq = 10;
    this.nextId = 1000;
    this.date = '2026-11-13';
    // One format list for the whole sheet, as the engine keeps it: a cell
    // names its format by its index here, on every tab.
    this.styles = [{}];
    this.tabs = tabs.map((t) => this.#tab(t));
    this.sockets = [];
    this.sessions = new Map(); // token -> {id, receipts: Map(n -> ack)}
    this.sessionCount = 0;
    this.watching = new Map(); // socket -> tab id
    this.log = [];             // every delta sent, for replay
    this.applied = [];         // [session id, n] of each action that changed the sheet
    this.manual = new Set();   // frame types the test answers itself
    this.refuse = null;        // Result to give the next action instead of applying it
    this.unavailable = 0;      // how many actions to come end their connection with fatal "unavailable"
    this.busyActions = [];     // n of each action answered that way
    this.holdAcks = false;     // acks stay in the send queue and never arrive
    this.paced = false;        // snapshot frames wait in the connection's queue for deliver()
    this.slowWelcome = false;  // the answer to a hello waits there too
    this.undoResult = { applied: 1, skipped: 0 };
    this.rebases = [];
    this.rebaseAnswer = null;  // (frame) => fields of `rebased`
    this.resume = 'replay';    // what a hello with a watermark gets: replay or snapshot
    this.headRuns = Infinity;  // row runs a head frame holds; the rest go in tab_more frames
    this.presence = [];
    this.inbox = [];
    this.busy = false;
    // Formulas of the form =Tab!A1: {from: {tab, row, col}, to: {tab, row,
    // col}}. An action that writes `from` recalculates `to` to its value.
    this.dependents = [];
  }

  #tab({ id, name, rows, cols, cells = {}, frozen_rows = 0, frozen_cols = 0 }) {
    // Every tab names the one list: a snapshot of any tab carries all of it.
    this.nextTab = Math.max(this.nextTab || 1, id + 1); // the tab ids given out so far lie below it
    const t = { id, name, rows: [], cols: [], cells: new Map(), widths: {}, frozen_rows, frozen_cols, styles: this.styles };
    for (let i = 0; i < rows; i++) t.rows.push(this.nextId++);
    for (let i = 0; i < cols; i++) t.cols.push(this.nextId++);
    // cells are given by position: {"r,c": text or a cell record}
    for (const [at, v] of Object.entries(cells)) {
      const [r, c] = at.split(',').map(Number);
      t.cells.set(`${t.rows[r]}:${t.cols[c]}`, typeof v === 'string' ? typed(v) : v);
    }
    return t;
  }

  tab(id) { return this.tabs.find((t) => t.id === id); }
  tabInfo() { return this.tabs.map((t) => ({ id: t.id, name: t.name, rows: t.rows.length, cols: t.cols.length, frozen_rows: t.frozen_rows, frozen_cols: t.frozen_cols })); }
  cell(tabId, r, c) { const t = this.tab(tabId); return t.cells.get(`${t.rows[r]}:${t.cols[c]}`) || null; }

  // connect is what the client's `connect` option calls.
  connect() {
    const ws = new FakeSocket(this);
    this.sockets.push(ws);
    return ws;
  }
  get socket() { return this.sockets.at(-1); }

  // receive handles one client frame at a time, in arrival order, as a
  // server reads a socket: a frame the client sends while an answer is
  // being delivered waits until that answer is complete.
  receive(ws, m) {
    // A connection starts with one hello; the server reads nothing before
    // it and ends a connection that sends another.
    if ((m.type === 'hello') === !!ws.greeted) throw new Refused('protocol', ws.greeted ? 'client sent a second hello' : `client sent ${m.type} before hello`);
    ws.greeted = true;
    this.inbox.push([ws, m]);
    if (this.busy) return;
    this.busy = true;
    try {
      while (this.inbox.length) this.#handle(...this.inbox.shift());
    } finally {
      this.busy = false;
    }
  }

  #handle(ws, m) {
    if (m.type === 'action_part') {
      ws.parts += m.data;
      if (m.part < m.parts - 1) return;
      const whole = ws.parts;
      ws.parts = '';
      m = readFrame(whole);
      if (m.type !== 'action') throw new Refused('protocol', `the parts of an action hold a ${m.type}`);
    }
    if (m.type === 'rebase') this.rebases.push(m);
    if (m.type === 'presence') this.#checkPresence(m);
    if (this.manual.has(m.type)) return;
    if (this.access === 'public' && ['action', 'presence', 'rebase'].includes(m.type)) throw new Error(`a Public client sent ${m.type}`);
    switch (m.type) {
      case 'hello': return this.#hello(ws, m);
      case 'watch': this.watching.set(ws, m.tab); return this.snapshot(ws, m.generation, m.tab);
      case 'action': return this.#action(ws, m);
      case 'peek': return this.#peek(ws, m);
      case 'rebase': if (this.rebaseAnswer) ws.recv({ type: 'rebased', id: m.id, ...this.rebaseAnswer(m) }); return;
      case 'presence': this.presence.push(m); return;
    }
  }

  // #checkPresence holds a presence frame to what the engine reads of it
  // against the sheet: the tab, the active cell and the runs of a range
  // name only ids the sheet has given out. readFrame has checked the rest
  // (checkRange). A frame it refuses ends the connection on the engine.
  #checkPresence(m) {
    const bad = (why) => { throw new Refused('protocol', `a presence frame the server refuses: ${why}: ${JSON.stringify(m).slice(0, 200)}`); };
    if (m.tab >= this.nextTab) bad('a tab that was never given out');
    if (m.active && (m.active[0] >= this.nextId || m.active[1] >= this.nextId)) bad('an active cell that was never given out');
    if (!m.range) return;
    for (const list of [m.range.rows, m.range.cols]) {
      if (list === 'all') continue;
      for (const [first, count] of list) if (first >= this.nextId || count > this.nextId - first) bad('ids that were never given out');
    }
  }

  #hello(ws, m) {
    // A Public connection opens no session. A name the browser kept from
    // a Private route is not read. A session, a watermark or pending
    // actions, which only a page that was open on a Private route sends,
    // end the connection, as on the host. An empty session names none, on
    // either route.
    const named = m.session || null;
    if (this.access === 'public' && (named || m.pending !== 0 || m.watermark)) {
      ws.recv({ type: 'fatal', code: 'forbidden' });
      ws.closed = true;
      return;
    }
    const welcome = { type: 'welcome', access: this.access, seq: String(this.seq), date: this.date, tabs: this.tabInfo() };
    if (this.access === 'private') {
      let s = null;
      let token = named;
      if (named) s = this.sessions.get(named);
      // A token the server does not know, and unconfirmed edits without a
      // session, end the connection: the session expired.
      if ((named && !s) || (!named && m.pending > 0)) {
        ws.recv({ type: 'fatal', code: 'session_expired' });
        ws.closed = true;
        return;
      }
      if (!s) {
        const k = ++this.sessionCount;
        token = `tok${k}`;
        // A session keeps the label it was opened with; a later hello's name changes nothing.
        const me = this.me || (m.name ? { label: m.name, verified: false } : { verified: false });
        s = { id: `s${k}`, me, receipts: new Map() };
        this.sessions.set(token, s);
      }
      ws.session = s;
      Object.assign(welcome, { session: token, session_id: s.id, me: s.me });
    }
    // A stream that is replayed goes on from the watermark, and the welcome
    // names it, with no tab list and no date: those of the watermark come
    // from the page and the replayed deltas. Otherwise the welcome names
    // the current sequence with its tab list and date, and the watched tab
    // follows as a snapshot.
    const resumes = m.watermark != null && m.tab != null;
    const replay = resumes && this.resume === 'replay' && this.access === 'private';
    if (replay) {
      welcome.seq = m.watermark;
      delete welcome.tabs;
      delete welcome.date;
    }
    // A slow welcome and what follows it are on their way until deliver().
    const out = this.slowWelcome ? { recv: (f) => ws.queue.push(f) } : ws;
    out.recv(welcome);
    if (!resumes) return;
    this.watching.set(ws, m.tab);
    if (!replay) { this.snapshot(out, 0, m.tab); return; }
    for (const d of this.log) if (BigInt(d.seq) > BigInt(m.watermark)) out.recv(this.#project(ws, d));
  }

  // snapshot sends tab_head, with the tab list and the date, rows and
  // tab_done of a tab at the current sequence, at most `per` rows a frame. A head with more than headRuns row runs
  // goes on in tab_more frames, which carry the styles after the last run.
  snapshot(ws, generation, tabId, per = 2000) {
    if (this.paced && ws.queue) {
      // The frames wait in the connection's queue until the test lets them go.
      const frames = [];
      this.snapshot({ recv: (m) => frames.push(m) }, generation, tabId, per);
      ws.queue.push(...frames);
      return;
    }
    // A tab that is gone gives way to the first tab, as on the host.
    const t = this.tab(tabId) ?? this.tabs[0];
    const head = { type: 'tab_head', generation, tab: t.id, seq: String(this.seq), date: this.date, tabs: this.tabInfo(), rows: runs(t.rows), cols: runs(t.cols), widths: t.widths, frozen_rows: t.frozen_rows, frozen_cols: t.frozen_cols, styles: t.styles };
    if (head.rows.length <= this.headRuns) ws.recv(head);
    else {
      const all = head.rows;
      ws.recv({ ...head, rows: all.slice(0, this.headRuns), styles: [], more: true });
      for (let i = this.headRuns; i < all.length; i += this.headRuns) ws.recv({ type: 'tab_more', generation, tab: t.id, rows: all.slice(i, i + this.headRuns), more: true });
      ws.recv({ type: 'tab_more', generation, tab: t.id, styles: t.styles });
    }
    // A rows frame holds `per` rows and stays inside the server frame
    // limit: a row whose cells do not fit goes on in the next frame under
    // the same row id, as the host sends it.
    const room = SERVER_FRAME_MAX - 256;
    let part = 0;
    let rows = [];
    let size = 64;
    const send = () => { ws.recv({ type: 'rows', generation, tab: t.id, part: part++, rows }); rows = []; size = 64; };
    for (const row of t.rows) {
      let entry = null;
      for (const col of t.cols) {
        if (!t.cells.has(`${row}:${col}`)) continue;
        const c = cellOut(col, t.cells.get(`${row}:${col}`));
        const n = encodedSize(c) + 1;
        if (size + n + 32 > room && rows.length) { send(); entry = null; }
        if (!entry) { entry = [row, []]; rows.push(entry); size += 32; }
        entry[1].push(c);
        size += n;
      }
      if (rows.length >= per) send();
    }
    if (rows.length || part === 0) send();
    ws.recv({ type: 'tab_done', generation, tab: t.id, parts: part });
  }

  // #project is the delta as one connection receives it: cells and widths
  // only for its watched tab, the structural operations of every tab, new
  // formats whatever tab they were made on, and no author or agent note on
  // a Public connection.
  #project(ws, d) {
    const out = { type: 'delta', seq: d.seq };
    const watched = this.watching.get(ws);
    if (d.tabs) out.tabs = d.tabs;
    if (d.date) out.date = d.date;
    const struct = structField(d.struct, watched);
    if (struct.length) out.struct = struct;
    if (d.tab != null && watched === d.tab) {
      for (const k of ['widths', 'cells']) if (d[k]) out[k] = d[k];
    } else if (d.tab != null) {
      out.touched = [d.tab];
      const other = d.recalc?.get(watched);
      if (other) out.cells = other;
    }
    if (d.styles?.length) out.styles = d.styles;
    if (this.access === 'private') {
      if (d.from) out.from = d.from;
      if (d.note) out.note = d.note;
    }
    return out;
  }

  // push sends one delta to every connection. fields: {tab, cells, struct,
  // widths, styles, tabs, from, note, date}.
  push(fields = {}) {
    const d = { ...fields, seq: String(++this.seq) };
    // A structural operation names its tab; a test may leave it to `tab`.
    if (d.struct) d.struct = d.struct.map((e) => ({ tab: e.tab ?? d.tab, ...e }));
    this.log.push(d);
    // A connection gets nothing before its hello, and a delta waits behind
    // the frames of a snapshot that are still in its queue.
    for (const ws of this.sockets) {
      if (ws.closed || !ws.greeted) continue;
      if (ws.queue.length) ws.queue.push(this.#project(ws, d)); else ws.recv(this.#project(ws, d));
    }
    return d;
  }

  // deliver lets the next n frames of a connection's queue go (see paced).
  deliver(n = Infinity, ws = this.socket) {
    while (n-- > 0 && ws.queue.length && !ws.closed) ws.recv(ws.queue.shift());
  }

  // Changes another writer makes, for tests to script.
  setCell(tabId, r, c, text, extra = {}) {
    const t = this.tab(tabId);
    const row = t.rows[r];
    const col = t.cols[c];
    const cell = typed(text);
    if (cell) t.cells.set(`${row}:${col}`, cell); else t.cells.delete(`${row}:${col}`);
    return this.push({ tab: tabId, cells: [[row, [cellOut(col, cell)]]], ...extra });
  }

  insertRows(tabId, at, count = 1, extra = {}) {
    const t = this.tab(tabId);
    const ids = Array.from({ length: count }, () => this.nextId++);
    const after = at > 0 ? t.rows[at - 1] : null;
    t.rows.splice(at, 0, ...ids);
    return this.push({ tab: tabId, struct: [{ op: 'insert_rows', ids: runs(ids), after }], tabs: this.tabInfo(), ...extra });
  }

  deleteRows(tabId, at, count = 1, extra = {}) {
    const t = this.tab(tabId);
    const ids = t.rows.splice(at, count);
    for (const k of [...t.cells.keys()]) if (ids.includes(Number(k.split(':')[0]))) t.cells.delete(k);
    return this.push({ tab: tabId, struct: [{ op: 'delete_rows', ids: runs(ids) }], tabs: this.tabInfo(), ...extra });
  }

  renameTab(tabId, name) {
    this.tab(tabId).name = name;
    return this.push({ tab: tabId, struct: [{ op: 'rename_tab', tab: tabId, name }], tabs: this.tabInfo() });
  }

  deleteTab(tabId) {
    this.tabs = this.tabs.filter((t) => t.id !== tabId);
    return this.push({ struct: [{ op: 'delete_tab', tab: tabId }], tabs: this.tabInfo() });
  }

  sortRows(tabId) {
    const t = this.tab(tabId);
    const ids = [...t.rows].reverse();
    t.rows = ids;
    return this.push({ tab: tabId, struct: [{ op: 'sort', positions: [[0, ids.length]], ids }] });
  }

  #action(ws, m) {
    const s = ws.session;
    // A base the server has not sent yet names no screen.
    if (BigInt(m.base) > BigInt(this.seq)) throw new Refused('protocol', `an action based on sequence ${m.base}, which the server has not sent`);
    if (this.unavailable > 0) {
      // The host cannot take the action now and ends the connection. That
      // says nothing about the action: the answer comes before the receipt
      // is looked up, so an action that was committed earlier gets it too.
      this.unavailable--;
      this.busyActions.push(m.n);
      ws.recv({ type: 'fatal', code: 'unavailable' });
      ws.closed = true;
      return;
    }
    const stored = s.receipts.get(m.n);
    if (stored) { ws.recv(stored); return; } // a resent action returns its receipt and applies nothing
    const from = { session_id: s.id, n: m.n };
    let ack;
    if (this.refuse) {
      const res = this.refuse;
      this.refuse = null;
      // An action that changes nothing takes no sequence, so no delta goes out.
      if (res.status === 'applied') this.push({ from });
      ack = { type: 'ack', n: m.n, seq: String(this.seq), results: m.ops.map(() => res) };
    } else {
      const d = { from };
      const results = m.ops.map((op) => this.#apply(op, d));
      this.#recalc(d);
      // An action of which nothing applied changes nothing and takes no
      // sequence, so no delta goes out.
      if (results.some((r) => r.status === 'applied')) {
        this.applied.push([s.id, m.n]);
        this.push(d);
      }
      ack = { type: 'ack', n: m.n, seq: String(this.seq), results };
      if (d.added) ack.added = d.added;
      if (m.ops[0].op === 'undo' || m.ops[0].op === 'redo') ack.undo = this.undoResult;
    }
    s.receipts.set(m.n, ack);
    // With holdAcks the action is committed and its receipt stored, but the
    // ack waits in the send queue. It is lost when the connection ends
    // first: dropped, or ended by a fatal frame, which is sent ahead of
    // the queue.
    if (this.holdAcks) return;
    ws.recv(ack);
  }

  // #recalc gives each dependent of a cell the action wrote the cell's new
  // value. One on another tab goes to that tab's watchers only.
  #recalc(d) {
    for (const { from, to } of this.dependents) {
      if (from.tab !== d.tab || !d.cells?.some(([row, cells]) => row === from.row && cells.some((c) => c[0] === from.col))) continue;
      const src = this.tab(from.tab).cells.get(`${from.row}:${from.col}`);
      const t = this.tab(to.tab);
      const cell = { ...t.cells.get(`${to.row}:${to.col}`), kind: src?.kind === 'n' ? 'nf' : 'tf', value: src?.value ?? '' };
      t.cells.set(`${to.row}:${to.col}`, cell);
      const out = [[to.row, [cellOut(to.col, cell)]]];
      if (to.tab === d.tab) d.cells.push(...out);
      else (d.recalc ??= new Map()).set(to.tab, out);
    }
  }

  #apply(op, d) {
    if (op.op === 'copy') {
      // The fake moves no cell, but it answers with the engine's counts:
      // the destinations that applied and those in deleted rows or
      // columns. A row or column the copy adds is a destination that
      // applied.
      const to = this.tab(op.to.tab);
      if (!to || !this.tab(op.from.tab)) return { status: 'ignored', reason: 'gone' };
      let applied = 0;
      let ignored = 0;
      for (const row of targets(op.to.rows, to.rows)) {
        for (const col of targets(op.to.cols, to.cols)) {
          if ((row === null || to.rows.includes(row)) && (col === null || to.cols.includes(col))) applied++;
          else ignored++;
        }
      }
      return ignored ? { status: 'applied', applied, ignored } : { status: 'applied', applied };
    }
    const t = op.tab != null ? this.tab(op.tab) : null;
    if (op.tab != null && !t) return { status: 'ignored', reason: 'gone' };
    if (t) d.tab = t.id;
    const edit = (e) => { d.struct = [...(d.struct || []), { tab: t.id, ...e }]; };
    const put = (row, col, cell) => {
      if (cell) t.cells.set(`${row}:${col}`, cell); else t.cells.delete(`${row}:${col}`);
      d.cells = d.cells || [];
      let entry = d.cells.find((e) => e[0] === row);
      if (!entry) { entry = [row, []]; d.cells.push(entry); }
      entry[1].push(cellOut(col, cell));
    };
    switch (op.op) {
      case 'set': {
        if (!t.rows.includes(op.row) || !t.cols.includes(op.col)) return { status: 'ignored', reason: 'gone' };
        put(op.row, op.col, typed(op.text, t.cells.get(`${op.row}:${op.col}`)?.style));
        return { status: 'applied', applied: 1 };
      }
      case 'clear': {
        let n = 0;
        for (const row of targets(op.rows, t.rows)) for (const col of targets(op.cols, t.cols)) if (t.cells.has(`${row}:${col}`)) { put(row, col, null); n++; }
        return { status: 'applied', applied: n };
      }
      case 'write': {
        let applied = 0;
        let ignored = 0;
        const rows = targets(op.rows, t.rows);
        const cols = targets(op.cols, t.cols);
        // checkOp holds the values to a list of ids. "all" is as many as
        // the tab has, so the values are held to it here.
        if (op.values.length !== rows.length || op.values.some((line) => line.length !== cols.length)) invalid('values of another size than the rows and columns of the tab');
        // A {new: k} target is the k-th row or column the write adds at the
        // end of the tab, as many as the largest k. A write that applies
        // to no cell adds none (TECH 7.1).
        const added = (list) => (list === 'all' ? [] : list.flatMap((x) => (Array.isArray(x) ? expand([x]).map(() => 0) : typeof x === 'object' ? [x.new] : [0])));
        const rowK = added(op.rows);
        const colK = added(op.cols);
        const lands = (id, k, axis) => k > 0 || axis.includes(id);
        if (!rows.some((row, i) => lands(row, rowK[i], t.rows)) || !cols.some((col, j) => lands(col, colK[j], t.cols))) return { status: 'ignored', reason: 'gone' };
        const grow = (axis, ks, op) => {
          const n = Math.max(0, ...ks);
          if (!n) return [];
          const ids = Array.from({ length: n }, () => this.nextId++);
          edit({ op, ids: runs(ids), after: axis.at(-1) ?? null });
          // The ack names the first id the action added to the axis.
          d.added = { ...d.added, [op === 'insert_rows' ? 'rows' : 'cols']: ids[0] };
          axis.push(...ids);
          d.tabs = this.tabInfo();
          return ids;
        };
        const newRows = grow(t.rows, rowK, 'insert_rows');
        const newCols = grow(t.cols, colK, 'insert_cols');
        rows.forEach((row, i) => cols.forEach((col, j) => {
          const r = rowK[i] ? newRows[rowK[i] - 1] : row;
          const c = colK[j] ? newCols[colK[j] - 1] : col;
          if (!t.rows.includes(r) || !t.cols.includes(c)) { ignored++; return; }
          put(r, c, typed(op.values[i][j]));
          applied++;
        }));
        return ignored ? { status: 'applied', applied, ignored } : { status: 'applied', applied };
      }
      case 'format': {
        // Named properties change; the others stay. A format the list
        // already holds keeps its index, and a new one goes out once.
        for (const row of targets(op.rows, t.rows)) {
          for (const col of targets(op.cols, t.cols)) {
            const cell = t.cells.get(`${row}:${col}`) || { kind: '' };
            const style = plainFormat({ ...this.styles[cell.style || 0], ...op.set });
            const sig = JSON.stringify(style);
            let index = this.styles.findIndex((f) => JSON.stringify(f) === sig);
            if (index < 0) {
              index = this.styles.push(style) - 1;
              d.styles = [...(d.styles || []), style];
            }
            put(row, col, { ...cell, style: index });
          }
        }
        return { status: 'applied' };
      }
      case 'width': {
        d.widths = targets(op.cols, t.cols).map((col) => { t.widths[col] = op.width; return [col, op.width]; });
        return { status: 'applied' };
      }
      case 'insert_rows': {
        const i = t.rows.indexOf(op.beside);
        const at = op.side === 'before' ? i : i + 1;
        const ids = Array.from({ length: op.count }, () => this.nextId++);
        edit({ op: 'insert_rows', ids: runs(ids), after: at > 0 ? t.rows[at - 1] : null });
        t.rows.splice(at, 0, ...ids);
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'insert_cols': {
        const i = t.cols.indexOf(op.beside);
        const at = op.side === 'before' ? i : i + 1;
        const ids = Array.from({ length: op.count }, () => this.nextId++);
        edit({ op: 'insert_cols', ids: runs(ids), after: at > 0 ? t.cols[at - 1] : null });
        t.cols.splice(at, 0, ...ids);
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'append_rows': case 'append_cols': {
        const axis = op.op === 'append_rows' ? t.rows : t.cols;
        const ids = Array.from({ length: op.count }, () => this.nextId++);
        edit({ op: op.op === 'append_rows' ? 'insert_rows' : 'insert_cols', ids: runs(ids), after: axis.at(-1) });
        axis.push(...ids);
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'delete_rows': {
        const ids = targets(op.ids, t.rows).filter((id) => t.rows.includes(id));
        t.rows = t.rows.filter((id) => !ids.includes(id));
        edit({ op: 'delete_rows', ids: runs(ids) });
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'delete_cols': {
        const ids = targets(op.ids, t.cols).filter((id) => t.cols.includes(id));
        t.cols = t.cols.filter((id) => !ids.includes(id));
        edit({ op: 'delete_cols', ids: runs(ids) });
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'sort': {
        // The fake reverses the rows it is given; the real order is the server's.
        const pos = (op.rows === 'all' ? t.rows.slice(t.frozen_rows) : targets(op.rows, t.rows)).map((id) => t.rows.indexOf(id)).sort((a, b) => a - b);
        const ids = pos.map((p) => t.rows[p]).reverse();
        pos.forEach((p, i) => { t.rows[p] = ids[i]; });
        edit({ op: 'sort', positions: runs(pos), ids });
        return { status: 'applied' };
      }
      case 'settings': t.frozen_rows = op.frozen_rows; t.frozen_cols = op.frozen_cols; d.tabs = this.tabInfo(); return { status: 'applied' };
      case 'add_tab': {
        // A tab without a name gets the first of Sheet1, Sheet2 and so on
        // that no tab has, and a tab goes before the one the operation
        // names, or last.
        let name = op.name;
        for (let i = 1; name == null; i++) if (!this.tabs.some((x) => x.name === `Sheet${i}`)) name = `Sheet${i}`;
        const added = this.#tab({ id: this.nextTab, name, ...this.newTab });
        const before = op.before == null ? -1 : this.tabs.findIndex((x) => x.id === op.before);
        this.tabs.splice(before < 0 ? this.tabs.length : before, 0, added);
        // A new tab and a renamed one are named as touched, like the engine does.
        d.tab = added.id;
        d.struct = [...(d.struct || []), { op: 'add_tab', tab: added.id }];
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      case 'rename_tab': t.name = op.name; edit({ op: 'rename_tab', name: op.name }); d.tabs = this.tabInfo(); return { status: 'applied' };
      case 'delete_tab': this.tabs = this.tabs.filter((x) => x !== t); delete d.tab; edit({ op: 'delete_tab' }); d.tabs = this.tabInfo(); return { status: 'applied' };
      case 'move_tab': {
        this.tabs = this.tabs.filter((x) => x !== t);
        const i = op.before == null ? this.tabs.length : this.tabs.findIndex((x) => x.id === op.before);
        this.tabs.splice(i, 0, t);
        delete d.tab;
        edit({ op: 'move_tab' });
        d.tabs = this.tabInfo();
        return { status: 'applied' };
      }
      default: return { status: 'applied' };
    }
  }

  #peek(ws, m) {
    const c = this.tab(m.tab).cells.get(`${m.row}:${m.col}`);
    ws.recv({ type: 'peeked', id: m.id, seq: String(this.seq), edit: c ? c.formula ?? c.edit ?? String(c.value) : '', formula: !!c?.formula });
  }
}

// startApp loads the app on the fake DOM against a fake server and plays
// the connection up to a loaded tab. The grid gets a viewport and a
// measure that counts 7 pixels a character. An error the app recovers from
// (a frame it could not take) fails the test that did not ask for it:
// report throws it, unless the test passes its own.
export async function startApp({ server = new FakeServer(), hash = '', functions = [], open = true, width = 1000, height = 400, report = (err) => { throw err; }, raf = (fn) => fn(), storage = null } = {}) {
  const doc = installDOM({ hash });
  // storage is what the browser kept for this origin from an earlier page.
  if (storage) globalThis.localStorage = storage;
  globalThis.sheetTest = true;
  const timers = new Timers();
  const { App } = await import('../app.js');
  const measure = { text: (s) => s.length * 7, height: (el) => (el.textContent.split('\n').length * 18) + 10 };
  const fetch = async () => { throw new Error('no network in tests'); };
  const app = new App({ connect: () => server.connect(), timers, now: () => timers.now, measure, raf, functions, fetch, report });
  app.grid.el.clientWidth = width;
  app.grid.el.clientHeight = height;
  app.grid.el.rect = { left: 0, top: 0, width, height };
  if (open) server.socket.open();
  return { app, server, doc, timers, grid: app.grid, model: app.model, net: app.net, editor: app.editor, sel: app.selection };
}

// ---- what a person does and sees, for the app tests ----


// key presses a key in whatever holds focus (the grid's text area unless a
// test moved focus).
export const key = (t, k, props = {}) => fire(t.doc.activeElement, 'keydown', { key: k, ...props });

// type puts text into the grid's text area the way typing does.
export function type(t, text) {
  const ta = t.editor.ed ? t.editor.area : t.grid.input;
  ta.value = t.editor.ed ? ta.value.slice(0, ta.selectionStart) + text + ta.value.slice(ta.selectionEnd) : ta.value + text;
  ta.selectionStart = ta.selectionEnd = t.editor.ed ? ta.selectionStart + text.length : ta.value.length;
  fire(ta, 'input');
}

// drawn finds the drawn element of cell (r, c): the active cell's own
// element when it is the active one.
export function drawn(t, r, c) {
  const { grid } = t;
  const model = grid.model;
  const a = grid.selection.active();
  if (a.r === r && a.c === c) return grid.activeEl;
  const row = grid.rowEls.get(model.rowAt(r));
  if (!row) return null;
  return [...row._f, ...row._c].find((el) => el.getAttribute('aria-colindex') === String(c + 2) && !el.classList.contains('ph')) || null;
}
export const shownAt = (t, r, c) => drawn(t, r, c)?._tx.textContent;

// notices lists the notices on screen as [kind, text, kept].
export const notices = (t) => t.app.toasts.children.map((el) => [
  el.className.replace('toast ', ''), el.querySelector('p').textContent, el.querySelector('.kept')?.textContent ?? null]);

export const find = (root, text) => root.querySelectorAll('button').find((b) => b.textContent === text || b.getAttribute('aria-label') === text) || null;
export const settle = async () => { for (let i = 0; i < 8; i++) await new Promise((r) => setImmediate(r)); };
