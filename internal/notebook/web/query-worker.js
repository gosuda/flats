// Database query evaluation (TECH 6.1), shared by the table and kanban
// renderers and run in a module worker when one is available. It must order
// rows exactly as Go's OrderRows does; testdata/db-query.test.mjs checks the
// shared vectors.

// Unicode 15.0.0 simple case folding (CaseFolding.txt statuses C and S), as runs
// of [first code point, count, step, delta].
'use strict';

const FOLD_RUNS = [
  65,26,1,32,181,1,1,775,192,23,1,32,216,7,1,32,256,24,2,1,306,3,2,1,313,8,2,
  1,330,23,2,1,376,1,1,-121,377,3,2,1,383,1,1,-268,385,1,1,210,386,2,2,1,390,
  1,1,206,391,1,1,1,393,2,1,205,395,1,1,1,398,1,1,79,399,1,1,202,400,1,1,203,
  401,1,1,1,403,1,1,205,404,1,1,207,406,1,1,211,407,1,1,209,408,1,1,1,412,1,1,
  211,413,1,1,213,415,1,1,214,416,3,2,1,422,1,1,218,423,1,1,1,425,1,1,218,428,
  1,1,1,430,1,1,218,431,1,1,1,433,2,1,217,435,2,2,1,439,1,1,219,440,1,1,1,444,
  1,1,1,452,1,1,2,453,1,1,1,455,1,1,2,456,1,1,1,458,1,1,2,459,9,2,1,478,9,2,1,
  497,1,1,2,498,2,2,1,502,1,1,-97,503,1,1,-56,504,20,2,1,544,1,1,-130,546,9,2,
  1,570,1,1,10795,571,1,1,1,573,1,1,-163,574,1,1,10792,577,1,1,1,579,1,1,-195,
  580,1,1,69,581,1,1,71,582,5,2,1,837,1,1,116,880,2,2,1,886,1,1,1,895,1,1,116,
  902,1,1,38,904,3,1,37,908,1,1,64,910,2,1,63,913,17,1,32,931,9,1,32,962,1,1,
  1,975,1,1,8,976,1,1,-30,977,1,1,-25,981,1,1,-15,982,1,1,-22,984,12,2,1,1008,
  1,1,-54,1009,1,1,-48,1012,1,1,-60,1013,1,1,-64,1015,1,1,1,1017,1,1,-7,1018,
  1,1,1,1021,3,1,-130,1024,16,1,80,1040,32,1,32,1120,17,2,1,1162,27,2,1,1216,
  1,1,15,1217,7,2,1,1232,48,2,1,1329,38,1,48,4256,38,1,7264,4295,1,1,7264,
  4301,1,1,7264,5112,6,1,-8,7296,1,1,-6222,7297,1,1,-6221,7298,1,1,-6212,7299,
  2,1,-6210,7301,1,1,-6211,7302,1,1,-6204,7303,1,1,-6180,7304,1,1,35267,
  7312,43,1,-3008,7357,3,1,-3008,7680,75,2,1,7835,1,1,-58,7838,1,1,
  -7615,7840,48,2,1,7944,8,1,-8,7960,6,1,-8,7976,8,1,-8,7992,8,1,-8,8008,6,1,
  -8,8025,4,2,-8,8040,8,1,-8,8072,8,1,-8,8088,8,1,-8,8104,8,1,-8,8120,2,1,-8,
  8122,2,1,-74,8124,1,1,-9,8126,1,1,-7173,8136,4,1,-86,8140,1,1,-9,
  8152,2,1,-8,8154,2,1,-100,8168,2,1,-8,8170,2,1,-112,
  8172,1,1,-7,8184,2,1,-128,8186,2,1,-126,8188,1,1,-9,8486,1,1,-7517,8490,1,1,
  -8383,8491,1,1,-8262,8498,1,1,28,8544,16,1,16,8579,1,1,1,9398,26,1,26,11264,
  48,1,48,11360,1,1,1,11362,1,1,-10743,11363,1,1,-3814,11364,1,1,-10727,11367,
  3,2,1,11373,1,1,-10780,11374,1,1,-10749,11375,1,1,-10783,11376,1,1,-10782,
  11378,1,1,1,11381,1,1,1,11390,2,1,-10815,11392,50,2,1,11499,2,2,1,11506,1,1,
  1,42560,23,2,1,42624,14,2,1,42786,7,2,1,42802,31,2,1,42873,2,2,1,42877,1,1,
  -35332,42878,5,2,1,42891,1,1,1,42893,1,1,-42280,42896,2,2,1,42902,10,2,1,
  42922,1,1,-42308,42923,1,1,-42319,42924,1,1,-42315,42925,1,1,-42305,42926,1,
  1,-42308,42928,1,1,-42258,42929,1,1,-42282,42930,1,1,-42261,42931,1,1,928,
  42932,8,2,1,42948,1,1,-48,42949,1,1,-42307,42950,1,1,-35384,42951,2,2,1,
  42960,2,6,1,42968,1,1,1,42997,1,1,1,43888,80,1,-38864,
  65313,26,1,32,66560,40,1,40,66736,36,1,40,66928,11,1,39,66940,
  15,1,39,66956,7,1,39,66964,2,1,39,68736,51,1,64,71840,32,1,32,
  93760,32,1,32,125184,34,1,34,
];

const FOLD = new Map();
for (let i = 0; i < FOLD_RUNS.length; i += 4) {
  const [first, count, step, delta] = FOLD_RUNS.slice(i, i + 4);
  for (let k = 0; k < count; k++) FOLD.set(first + k * step, first + k * step + delta);
}

export function fold(s) {
  let out = '';
  for (const ch of s) {
    const c = ch.codePointAt(0);
    out += FOLD.has(c) ? String.fromCodePoint(FOLD.get(c)) : ch;
  }
  return out;
}

// JavaScript compares UTF-16 code units; the contract compares code points.
export function compareCodePoints(a, b) {
  const x = [...a];
  const y = [...b];
  for (let i = 0; i < x.length && i < y.length; i++) {
    const d = x[i].codePointAt(0) - y[i].codePointAt(0);
    if (d) return d < 0 ? -1 : 1;
  }
  return Math.sign(x.length - y.length);
}

const DECIMAL = /^([+-]?)([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE]([+-]?[0-9]+))?$/;
const MAX_CELL = 8000;

// parseDecimal ports Go's ParseDecimal: it returns the canonical decimal
// string (no exponent, no redundant zeros, no negative zero) or null.
export function parseDecimal(input) {
  const m = DECIMAL.exec(input);
  if (!m) return null;
  const mantissa = m[2];
  const dot = mantissa.indexOf('.');
  const point = dot < 0 ? mantissa.length : dot;
  let digits = mantissa.replace('.', '');
  const leading = digits.length - digits.replace(/^0+/, '').length;
  digits = digits.slice(leading);
  if (!digits) return '0';
  const exponent = m[3] ? Number(m[3]) : 0;
  const limit = 2 * Math.max(input.length, MAX_CELL);
  if (!Number.isSafeInteger(exponent) || Math.abs(exponent) > 4 * limit) return null;
  const at = point - leading + exponent;
  digits = digits.replace(/0+$/, '');
  if (at < -limit || at > limit) return null;
  let length = digits.length;
  if (at <= 0) length += 2 - at;
  else if (at >= length) length = at;
  else length++;
  if (m[1] === '-') length++;
  if (length > limit) return null;
  let out;
  if (at <= 0) out = '0.' + '0'.repeat(-at) + digits;
  else if (at >= digits.length) out = digits + '0'.repeat(at - digits.length);
  else out = digits.slice(0, at) + '.' + digits.slice(at);
  return m[1] === '-' ? '-' + out : out;
}

function splitDecimal(s) {
  let t = parseDecimal(String(s)) ?? String(s);
  const neg = t[0] === '-';
  if (neg) t = t.slice(1);
  const [int = '', frac = ''] = t.split('.');
  const i = int.replace(/^0+/, '');
  const zero = i === '' && frac === '';
  return { sign: zero ? 0 : neg ? -1 : 1, int: i, frac };
}

export function compareDecimal(a, b) {
  const x = splitDecimal(a);
  const y = splitDecimal(b);
  if (x.sign !== y.sign) return x.sign < y.sign ? -1 : 1;
  if (x.sign === 0) return 0;
  let mag = Math.sign(x.int.length - y.int.length);
  if (!mag) mag = x.int < y.int ? -1 : x.int > y.int ? 1 : 0;
  if (!mag) {
    const n = Math.max(x.frac.length, y.frac.length);
    const fx = x.frac.padEnd(n, '0');
    const fy = y.frac.padEnd(n, '0');
    mag = fx < fy ? -1 : fx > fy ? 1 : 0;
  }
  return x.sign * mag;
}

// A date-only value sorts before timed values on the same date.
export function compareDate(a, b) {
  const da = a.slice(0, 10);
  const db = b.slice(0, 10);
  if (da !== db) return da < db ? -1 : 1;
  const ta = a.slice(11);
  const tb = b.slice(11);
  if (ta === tb) return 0;
  if (!ta) return -1;
  if (!tb) return 1;
  return ta < tb ? -1 : 1;
}

export function isEmpty(type, v) {
  switch (type) {
    case 'checkbox': return false;
    case 'multi_select': return !Array.isArray(v) || v.length === 0;
    case 'title': case 'text': case 'url': return v == null || v === '';
    default: return v == null;
  }
}

// The operator table of rule 10, in menu order.
export const OPERATORS = {
  title: ['contains', 'does not contain', 'is', 'is not', 'is empty', 'is not empty'],
  text: ['contains', 'does not contain', 'is', 'is not', 'is empty', 'is not empty'],
  url: ['contains', 'does not contain', 'is', 'is not', 'is empty', 'is not empty'],
  number: ['=', '≠', '<', '≤', '>', '≥', 'is empty', 'is not empty'],
  select: ['is', 'is not', 'is any of', 'is empty', 'is not empty'],
  multi_select: ['contains', 'does not contain', 'is empty', 'is not empty'],
  date: ['is', 'before', 'after', 'on or before', 'on or after', 'is empty', 'is not empty'],
  checkbox: ['is checked', 'is not checked'],
};

export function needsValue(op) {
  return !['is empty', 'is not empty', 'is checked', 'is not checked'].includes(op);
}

export function matchesCondition(type, v, f) {
  if (f.op === 'is empty') return isEmpty(type, v);
  if (f.op === 'is not empty') return !isEmpty(type, v);
  if (f.op === 'is checked') return v === true;
  if (f.op === 'is not checked') return v !== true;
  const want = f.value;
  switch (type) {
    case 'title': case 'text': case 'url': {
      const have = fold(v ?? '');
      const q = fold(String(want));
      const present = type !== 'url' || !isEmpty(type, v);
      if (f.op === 'contains') return present && have.includes(q);
      if (f.op === 'does not contain') return !(present && have.includes(q));
      if (f.op === 'is') return present && have === q;
      if (f.op === 'is not') return !(present && have === q);
      break;
    }
    case 'number': {
      if (f.op === '≠') return v == null || compareDecimal(v, want) !== 0;
      if (v == null) return false;
      const c = compareDecimal(v, want);
      return { '=': c === 0, '<': c < 0, '≤': c <= 0, '>': c > 0, '≥': c >= 0 }[f.op] ?? unknown(f.op);
    }
    case 'select':
      if (f.op === 'is') return v === want;
      if (f.op === 'is not') return v !== want;
      if (f.op === 'is any of') return v != null && want.includes(v);
      break;
    case 'multi_select': {
      const has = Array.isArray(v) && v.includes(want);
      if (f.op === 'contains') return has;
      if (f.op === 'does not contain') return !has;
      break;
    }
    case 'date': {
      if (v == null) return false;
      const c = compareDate(v, want);
      return { is: c === 0, before: c < 0, after: c > 0, 'on or before': c <= 0, 'on or after': c >= 0 }[f.op] ?? unknown(f.op);
    }
  }
  return unknown(f.op);
}

function unknown(op) { throw new Error(`unknown filter operator ${op}`); }

export function matchesView(schema, view, row) {
  return view.filter.every((f) => {
    const p = schema.byId.get(f.property);
    return p ? matchesCondition(p.type, row.cells[f.property], f) : true;
  });
}

function sortKey(schema, prop, v) {
  if (isEmpty(prop.type, v)) return null;
  switch (prop.type) {
    case 'title': case 'text': case 'url': return fold(v);
    case 'select': {
      const rank = schema.rank.get(v);
      return rank === undefined ? null : rank;
    }
    case 'multi_select': {
      const ranks = v.map((id) => schema.rank.get(id)).filter((r) => r !== undefined).sort((a, b) => a - b);
      return ranks.length ? ranks : null;
    }
    case 'checkbox': return v === true;
    default: return v;
  }
}

function compareKeys(type, a, b) {
  switch (type) {
    case 'title': case 'text': case 'url': return compareCodePoints(a, b);
    case 'number': return compareDecimal(a, b);
    case 'date': return compareDate(a, b);
    case 'checkbox': return a === b ? 0 : a ? 1 : -1;
    case 'select': return Math.sign(a - b);
    case 'multi_select': {
      for (let i = 0; i < a.length && i < b.length; i++) if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
      return Math.sign(a.length - b.length);
    }
  }
  return 0;
}

export function compareManual(a, b) {
  if (a.position !== b.position) return a.position < b.position ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

// orderRows applies the view's filter then sort and returns row ids. Rows
// named in keep stay even when the filter excludes them (rule 11).
export function orderRows(schema, view, rows, keep = []) {
  const s = indexSchema(schema);
  const kept = new Set(keep);
  const sorts = view.sort.map((k) => ({ prop: s.byId.get(k.property), desc: k.direction === 'desc' })).filter((k) => k.prop);
  const items = rows
    .filter((r) => kept.has(r.id) || matchesView(s, view, r))
    .map((r) => ({ row: r, keys: sorts.map((k) => sortKey(s, k.prop, r.cells[k.prop.id])) }));
  items.sort((x, y) => {
    for (let i = 0; i < sorts.length; i++) {
      const a = x.keys[i];
      const b = y.keys[i];
      if (a === null || b === null) {
        if (a === b) continue;
        return a === null ? 1 : -1;
      }
      const c = compareKeys(sorts[i].prop.type, a, b);
      if (c) return sorts[i].desc ? -c : c;
    }
    return compareManual(x.row, y.row);
  });
  return items.map((x) => x.row.id);
}

export function indexSchema(schema) {
  if (schema.byId) return schema;
  const byId = new Map(schema.properties.map((p) => [p.id, p]));
  const rank = new Map();
  for (const list of Object.values(schema.options)) list.forEach((o, i) => { rank.set(o.id, i); });
  return { ...schema, byId, rank };
}

// ---- Normalization of the shapes that reach the client and the vectors ----

const idOf = (x) => {
  if (x == null) return null;
  if (typeof x === 'string') return x;
  return x.id ?? x.ID ?? x.ref?.id ?? x.Ref?.ID ?? x.option ?? x.Option ?? null;
};
const incOf = (x) => (x && typeof x === 'object' ? String(x.inc ?? x.ref?.inc ?? x.Inc ?? '0') : '0');
const pick = (o, ...keys) => { for (const k of keys) if (o && o[k] !== undefined) return o[k]; return undefined; };

export function normalizeSchema(raw) {
  const props = pick(raw, 'properties', 'Properties') ?? [];
  const propertyIDs = pick(raw, 'property_ids', 'PropertyIDs') ?? [];
  const options = {};
  const rawOptions = pick(raw, 'options', 'Options') ?? {};
  const properties = props.map((p, i) => {
    const def = pick(p, 'definition', 'Definition') ?? p;
    const id = idOf(p.ref ?? p.Ref ?? p) ?? propertyIDs[i];
    const prop = { id, inc: incOf(p), name: pick(def, 'name', 'Name') ?? '', type: pick(def, 'type', 'Type'), format: pick(def, 'format', 'Format') || 'plain' };
    const list = pick(p, 'options', 'Options') ?? rawOptions[id] ?? [];
    const order = pick(p, 'option_order', 'OptionOrder');
    const normalized = list.map((o, j) => ({ id: idOf(o.ref ?? o) ?? order?.[j], inc: incOf(o), name: pick(o, 'name', 'Name') ?? '', color: pick(o, 'color', 'Color') ?? 'gray' }));
    const byId = new Map(normalized.map((o) => [o.id, o]));
    options[id] = order ? order.map((id) => byId.get(id)) : normalized;
    return prop;
  });
  const db = pick(raw, 'database', 'Database');
  return { database: db == null ? null : { id: idOf(db), inc: incOf(db) }, title: raw.title ?? '', properties, options };
}

function normalizeFilterValue(v) {
  if (Array.isArray(v)) return v.map(idOf);
  if (v && typeof v === 'object') return idOf(v);
  return v;
}

export function normalizeView(raw) {
  if (raw && raw.def) return raw;
  const def = pick(raw, 'definition', 'Definition') ?? raw;
  const d = pick(def, 'display', 'Display') ?? {};
  const widths = {};
  for (const w of pick(d, 'widths', 'Widths') ?? []) widths[idOf(pick(w, 'property', 'Property'))] = pick(w, 'px', 'PX');
  const group = pick(d, 'group_by', 'GroupBy');
  const view = {
    name: pick(def, 'name', 'Name') ?? '',
    layout: pick(d, 'layout', 'Layout') ?? 'table',
    visible: (pick(d, 'visible', 'Visible') ?? []).map(idOf),
    widths,
    groupBy: group == null ? null : idOf(group),
    showEmpty: !!pick(d, 'show_empty', 'ShowEmpty'),
    sort: (pick(def, 'sort', 'Sort') ?? []).map((s) => ({ property: idOf(pick(s, 'property', 'Property')), direction: pick(s, 'direction', 'Direction') })),
    filter: (pick(def, 'filter', 'Filter') ?? []).map((f) => {
      const out = { property: idOf(pick(f, 'property', 'Property')), op: pick(f, 'op', 'Op') };
      const value = pick(f, 'value', 'Value');
      if (value !== undefined && value !== null) out.value = normalizeFilterValue(value);
      return out;
    }),
  };
  return raw && (raw.ref || raw.id) && raw !== def ? { id: idOf(raw.ref ?? raw), inc: incOf(raw), def: view } : view;
}

function cellValue(c) {
  if (c && typeof c === 'object' && !Array.isArray(c) && 'type' in c && 'value' in c) return c.value;
  return c;
}

export function normalizeRows(raw) {
  return raw.map((r) => {
    const cells = {};
    for (const [k, v] of Object.entries(pick(r, 'cells', 'Cells') ?? {})) cells[k] = cellValue(v);
    return { id: idOf(pick(r, 'row', 'Row') ?? r.ref ?? r), inc: incOf(r.ref ?? r), position: pick(r, 'position', 'Position') ?? '', cells };
  });
}

// ---- Worker entry and main-thread client ----

if (typeof WorkerGlobalScope !== 'undefined' && globalThis instanceof WorkerGlobalScope) {
  globalThis.onmessage = (e) => {
    const { id, schema, view, rows, keep } = e.data;
    try {
      globalThis.postMessage({ id, order: orderRows(schema, view, rows, keep) });
    } catch (err) {
      globalThis.postMessage({ id, error: String(err?.message ?? err) });
    }
  };
}

// createQuery evaluates in a module worker when the platform has one and on
// the calling thread otherwise. Only the latest request's result is reported.
export function createQuery() {
  let worker = null;
  let latest = 0;
  const waiting = new Map();
  if (typeof Worker === 'function' && typeof document !== 'undefined') {
    try {
      worker = new Worker(new URL('./query-worker.js', import.meta.url), { type: 'module' });
      worker.onmessage = (e) => {
        const cb = waiting.get(e.data.id);
        waiting.delete(e.data.id);
        if (cb && e.data.id === latest && e.data.order) cb(e.data.order);
      };
    } catch {
      worker = null;
    }
  }
  return {
    run(schema, view, rows, keep, cb) {
      const id = ++latest;
      if (!worker) { cb(orderRows(schema, view, rows, keep)); return; }
      waiting.set(id, cb);
      const plain = { database: schema.database, properties: schema.properties, options: schema.options };
      worker.postMessage({ id, schema: plain, view, rows, keep });
    },
    stop() { worker?.terminate(); worker = null; waiting.clear(); },
  };
}
