// Database schema and view controls, typed value parsing and the browser
// operations they emit. Every operation names existing items with {id, inc}
// and every create proposes a fresh id (TECH 4.1, Appendix A).
'use strict';


import { OPERATORS, needsValue, parseDecimal, isEmpty } from './query-worker.js';

export const TYPES = ['text', 'number', 'select', 'multi_select', 'date', 'checkbox', 'url'];
export const TYPE_NAMES = {
  title: 'Title', text: 'Text', number: 'Number', select: 'Select', multi_select: 'Multi-select',
  date: 'Date', checkbox: 'Checkbox', url: 'URL',
};
export const COLORS = ['gray', 'brown', 'orange', 'yellow', 'green', 'blue', 'purple', 'pink', 'red'];
export const FORMATS = { plain: 'Plain', comma: 'Comma-grouped', percent: 'Percent' };
export const MIN_WIDTH = 80;
export const MAX_WIDTH = 1200;
export const MAX_TEXT = 8000;
export const MAX_NAME = 200;
export const MAX_URL_BYTES = 2048;

// ---- DOM ----

// h builds an element. Text always goes through text nodes, never HTML.
export function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'selected') { el[k] = true; el.setAttribute(k, ''); }
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid);
  }
  return el;
}

// ---- Identity ----

const BASE32 = 'abcdefghijklmnopqrstuvwxyz234567';

// newId returns a canonical 128-bit id: 26 base32 characters whose last
// character carries two zero padding bits.
export function newId(prefix) {
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  let bits = 0;
  let value = 0;
  let out = '';
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      out += BASE32[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
    value &= (1 << bits) - 1;
  }
  out += BASE32[(value << (5 - bits)) & 31];
  return prefix + out;
}

export function newUnit() {
  return 'u' + newId('').slice(0, 20);
}

export const ref = (x) => ({ id: x.id, inc: String(x.inc ?? '0') });

// ---- Values ----

const codePoints = (s) => [...s].length;

function validDate(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}):(\d{2}):(\d{2}))?$/.exec(s);
  if (!m) return false;
  const [y, mo, d] = [+m[1], +m[2], +m[3]];
  if (y < 1 || mo < 1 || mo > 12 || d < 1) return false;
  const leap = (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][mo - 1];
  if (d > days) return false;
  return m[4] === undefined || (+m[4] < 24 && +m[5] < 60 && +m[6] < 60);
}

function validURL(s) {
  if (/[\s\\]/u.test(s) || /\p{Cc}/u.test(s)) return false;
  if (new TextEncoder().encode(s).length > MAX_URL_BYTES) return false;
  try {
    const u = new URL(s);
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.hostname !== '';
  } catch {
    return false;
  }
}

// parseInput turns what a person typed into a typed cell value. It returns
// {value} or {error} with a sentence to show next to the field.
export function parseInput(type, text) {
  const s = String(text ?? '');
  switch (type) {
    case 'title': case 'text':
      return codePoints(s) > MAX_TEXT ? { error: `Use at most ${MAX_TEXT} characters.` } : { value: s };
    case 'number': {
      if (s.trim() === '') return { value: null };
      const d = parseDecimal(s.trim());
      return d === null ? { error: 'Enter a number such as 12, -3.5 or 1e3.' } : { value: d };
    }
    case 'date':
      if (s.trim() === '') return { value: null };
      return validDate(s.trim()) ? { value: s.trim() } : { error: 'Enter a date as YYYY-MM-DD, optionally with THH:MM:SS.' };
    case 'url':
      if (s.trim() === '') return { value: null };
      return validURL(s.trim()) ? { value: s.trim() } : { error: 'Enter an http or https address.' };
    case 'checkbox':
      return { value: s === 'true' };
  }
  return { error: 'This value cannot be typed.' };
}

export function formatNumber(v, format) {
  if (v == null) return '';
  const neg = v.startsWith('-');
  let [int, frac = ''] = (neg ? v.slice(1) : v).split('.');
  let suffix = '';
  if (format === 'percent') {
    frac = frac.padEnd(2, '0');
    int = (int + frac.slice(0, 2)).replace(/^0+(?=\d)/, '');
    frac = frac.slice(2).replace(/0+$/, '');
    suffix = '%';
  }
  if (format === 'comma') int = int.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return (neg ? '-' : '') + int + (frac ? '.' + frac : '') + suffix;
}

export function optionsOf(schema, prop) {
  return schema.options[prop.id] || [];
}

function chip(o) {
  return h('span', { class: 'opt ' + (o?.color || 'gray') }, o ? o.name : 'Deleted option');
}

// cellDisplay returns the nodes showing a value, or null when it is empty.
// Option colors always come with the option name (rule 18).
export function cellDisplay(schema, prop, value) {
  if (prop.type !== 'checkbox' && isEmpty(prop.type, value)) return null;
  const opts = optionsOf(schema, prop);
  switch (prop.type) {
    case 'select': return chip(opts.find((o) => o.id === value));
    case 'multi_select': {
      const set = new Set(value);
      return h('span', { class: 'opts' }, opts.filter((o) => set.has(o.id)).map(chip));
    }
    case 'number': return formatNumber(value, prop.format);
    case 'checkbox': return value === true ? 'Checked' : 'Not checked';
    case 'url': return h('a', { href: value, target: '_blank', rel: 'noopener noreferrer' }, value);
    default: return String(value);
  }
}

// optionPicker edits a select or multi-select cell. A new option and the
// cell that uses it are one unit.
export function optionPicker(ctx, schema, prop, row, value, done) {
  const opts = optionsOf(schema, prop);
  const multi = prop.type === 'multi_select';
  const chosen = new Set(multi ? value || [] : value == null ? [] : [value]);
  const set = (next) => ctx.onOps([ops.setCell(schema, row, prop, next)]);
  const list = h('div', { class: 'db-menu', role: multi ? 'group' : 'menu', 'aria-label': prop.name },
    opts.map((o) => (multi
      ? h('label', { class: 'mi' }, h('input', {
        type: 'checkbox', checked: chosen.has(o.id),
        onchange: (e) => {
          if (e.target.checked) chosen.add(o.id); else chosen.delete(o.id);
          set(opts.filter((x) => chosen.has(x.id)).map((x) => x.id));
        },
      }), chip(o))
      : h('button', { type: 'button', class: 'mi', role: 'menuitemradio', 'aria-checked': String(chosen.has(o.id)), onclick: () => { set(o.id); done?.(); } }, chip(o)))),
    multi ? null : h('button', { type: 'button', class: 'mi', role: 'menuitemradio', 'aria-checked': String(value == null), onclick: () => { set(null); done?.(); } }, 'No value'));
  const input = h('input', { type: 'text', 'aria-label': `New ${prop.name} option`, placeholder: 'New option' });
  const error = h('p', { class: 'db-error', role: 'alert' });
  input.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    e.preventDefault();
    const name = input.value;
    if (name.trim() === '' || [...name].length > MAX_NAME) { error.textContent = `Use 1 to ${MAX_NAME} characters.`; return; }
    if (opts.some((o) => o.name === name)) { error.textContent = 'Another option already has this name.'; return; }
    const { op } = ops.createOption(prop, name, COLORS[opts.length % COLORS.length]);
    const unit = newUnit();
    // The new option is named by its temporary name within the same action.
    const fresh = { tmp: op.ref };
    const cellValue = multi ? [...[...chosen].map((x) => optionRef(schema, x)).filter(Boolean), fresh] : fresh;
    ctx.onOps([{ ...op, unit_id: unit }, { op: 'set_cell', row: ref(row), cell: { property: ref(prop), value: cellValue }, unit_id: unit }]);
    done?.();
  });
  return h('div', {}, list, h('div', { class: 'db-field' }, input, error));
}

// ---- Wire conversion ----

export function optionRef(schema, id) {
  for (const list of Object.values(schema.options)) {
    const o = list.find((x) => x.id === id);
    if (o) return ref(o);
  }
  return null;
}

export function propertyOf(schema, id) {
  return schema.properties.find((p) => p.id === id) || null;
}

export function titleProperty(schema) {
  return schema.properties.find((p) => p.type === 'title') || schema.properties[0];
}

export function cellToWire(schema, prop, value) {
  if (prop.type === 'select') return value == null ? null : optionRef(schema, value);
  if (prop.type === 'multi_select') return (value || []).map((id) => optionRef(schema, id)).filter(Boolean);
  return value;
}

function filterValueToWire(schema, prop, f) {
  if (!needsValue(f.op)) return undefined;
  if (prop.type === 'select' && f.op === 'is any of') return f.value.map((id) => optionRef(schema, id)).filter(Boolean);
  if (prop.type === 'select' || prop.type === 'multi_select') return optionRef(schema, f.value);
  return f.value;
}

export function defaultWidth(prop) {
  return prop.type === 'title' ? 240 : 180;
}

export function clampWidth(px) {
  return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, Math.round(px)));
}

// viewToWire encodes a view for set_view or create_view. Parts naming gone
// properties or options are dropped here; the server repairs the rest.
export function viewToWire(schema, view) {
  const prop = (id) => propertyOf(schema, id);
  const visible = view.visible.map(prop).filter(Boolean);
  const display = view.layout === 'kanban' && prop(view.groupBy)
    ? { layout: 'kanban', group_by: ref(prop(view.groupBy)), visible: visible.map(ref), show_empty: !!view.showEmpty }
    : {
      layout: 'table',
      visible: visible.map(ref),
      widths: visible.filter((p) => view.widths?.[p.id] != null)
        .map((p) => ({ property: ref(p), px: clampWidth(view.widths[p.id]) })),
    };
  if (display.layout === 'table' && !visible.some((p) => p.type === 'title')) display.visible.unshift(ref(titleProperty(schema)));
  return {
    name: view.name,
    display,
    sort: view.sort.filter((s) => prop(s.property)).map((s) => ({ property: ref(prop(s.property)), direction: s.direction })),
    filter: view.filter.filter((f) => prop(f.property)).map((f) => {
      const p = prop(f.property);
      const out = { property: ref(p), op: f.op };
      const value = filterValueToWire(schema, p, f);
      if (value !== undefined) out.value = value;
      return out;
    }),
  };
}

// ---- Operations ----

export const ops = {
  setCell: (schema, row, prop, value) => ({ op: 'set_cell', row: ref(row), cell: { property: ref(prop), value: cellToWire(schema, prop, value) } }),
  createRow(schema, view, cells = {}, before = null) {
    const id = newId('r_');
    return {
      op: {
        op: 'create_row', ref: id, proposed_id: id, database: ref(schema.database),
        cells: Object.entries(cells).map(([p, v]) => ({ property: ref(propertyOf(schema, p)), value: cellToWire(schema, propertyOf(schema, p), v) })),
        before: before ? ref(before) : null, view: ref(view),
      },
      id,
    };
  },
  moveRows: (rows, before) => ({ op: 'move_rows', rows: rows.map(ref), before: before ? ref(before) : null }),
  deleteRows: (rows) => ({ op: 'delete_rows', rows: rows.map(ref) }),
  createProperty(schema, definition) {
    const id = newId('pr_');
    return { op: { op: 'create_property', ref: id, proposed_id: id, database: ref(schema.database), definition }, id };
  },
  setProperty: (prop, definition, preview) => ({ op: 'set_property', property: ref(prop), definition, ...(preview ? { confirmed_preview: preview } : {}) }),
  deleteProperty: (prop) => ({ op: 'delete_property', property: ref(prop) }),
  createOption(prop, name, color) {
    const id = newId('o_');
    return { op: { op: 'create_option', ref: id, proposed_id: id, property: ref(prop), name, color }, id };
  },
  setOption: (o, name, color) => ({ op: 'set_option', option: ref(o), name, color }),
  deleteOption: (o) => ({ op: 'delete_option', option: ref(o) }),
  createView(schema, definition) {
    const id = newId('v_');
    return { op: { op: 'create_view', ref: id, proposed_id: id, database: ref(schema.database), definition }, id };
  },
  setView: (schema, view, def) => ({ op: 'set_view', view: ref(view), definition: viewToWire(schema, def) }),
  deleteView: (view) => ({ op: 'delete_view', view: ref(view) }),
};

export function definitionOf(prop, changes = {}) {
  const def = { name: changes.name ?? prop.name, type: changes.type ?? prop.type };
  if (def.type === 'number') def.format = changes.format ?? (prop.type === 'number' ? prop.format : 'plain') ?? 'plain';
  return def;
}

// filterDefaults gives a new row the values that make it match the view's
// `is` and `is checked` conditions (rule 11). The server applies the same
// defaults from create_row's view; the client uses them to know whether the
// new row will need retaining.
export function filterDefaults(schema, view) {
  const cells = {};
  for (const f of view.filter) {
    const p = propertyOf(schema, f.property);
    if (!p || p.type === 'title') continue;
    if (f.op === 'is checked') cells[p.id] = true;
    if (f.op === 'is' && p.type !== 'multi_select') cells[p.id] = f.value;
  }
  return cells;
}

// ---- Popovers ----

// openPopover shows content below anchor inside host; Escape and a press
// outside close it, Escape returning focus to the anchor.
export function openPopover(host, anchor, label, content) {
  closePopover(host);
  const pop = h('div', { class: 'db-pop', role: 'dialog', 'aria-label': label }, content);
  const close = () => closePopover(host, true);
  pop.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); close(); } });
  host.append(pop);
  placePopover(pop, host, anchor);
  const outside = (e) => { if (!pop.contains(e.target) && !anchor.contains?.(e.target)) closePopover(host); };
  document.addEventListener('pointerdown', outside, true);
  host._dbPop = { pop, anchor, outside };
  const first = pop.querySelector('input, select, button');
  if (first) first.focus();
  return pop;
}

export function closePopover(host, refocus = false) {
  const cur = host._dbPop;
  if (!cur) return;
  cur.pop.remove();
  document.removeEventListener('pointerdown', cur.outside, true);
  host._dbPop = null;
  if (refocus && cur.anchor?.isConnected) cur.anchor.focus();
}

// placePopover puts pop under its anchor, kept inside host's width.
function placePopover(pop, host, anchor) {
  if (!anchor.getBoundingClientRect || !host.getBoundingClientRect) return;
  const a = anchor.getBoundingClientRect();
  const r = host.getBoundingClientRect();
  pop.style.left = `${Math.max(0, Math.min(a.left - r.left, r.width - (pop.offsetWidth || 0)))}px`;
  pop.style.top = `${a.bottom - r.top + 4}px`;
}

function fieldError(field, message) {
  field.setAttribute('aria-invalid', 'true');
  const box = field.parentNode?.querySelector('.db-error');
  if (box) box.textContent = message;
  field.focus();
}

// ---- Filter and sort forms ----

function valueField(schema, prop, op) {
  if (!needsValue(op)) return null;
  const options = schema.options[prop.id] || [];
  if (prop.type === 'select' && op === 'is any of') {
    return h('fieldset', { class: 'db-value', 'aria-label': 'Values' },
      options.map((o) => h('label', {}, h('input', { type: 'checkbox', value: o.id }), h('span', { class: 'opt ' + o.color }, o.name))));
  }
  if (prop.type === 'select' || prop.type === 'multi_select') {
    return h('select', { class: 'db-value', 'aria-label': 'Value' },
      h('option', { value: '' }, 'Choose an option'),
      options.map((o) => h('option', { value: o.id }, o.name)));
  }
  const hint = { number: 'Number', date: 'YYYY-MM-DD' }[prop.type] || 'Text';
  return h('input', { class: 'db-value', type: 'text', 'aria-label': 'Value', placeholder: hint, inputmode: prop.type === 'number' ? 'decimal' : null });
}

// readFilterValue validates the value field. Empty values are refused for
// every operator that needs one.
export function readFilterValue(prop, op, field) {
  if (!needsValue(op)) return { ok: true };
  if (field.tagName === 'FIELDSET') {
    const ids = [...field.querySelectorAll('input')].filter((i) => i.checked).map((i) => i.value);
    return ids.length ? { ok: true, value: ids } : { error: 'Choose at least one option.' };
  }
  const raw = field.value;
  if (raw === '' || (prop.type !== 'select' && prop.type !== 'multi_select' && raw.trim() === '')) {
    return { error: 'Enter a value, or choose “is empty”.' };
  }
  if (prop.type === 'select' || prop.type === 'multi_select') return { ok: true, value: raw };
  if (prop.type === 'title' || prop.type === 'text' || prop.type === 'url') {
    return codePoints(raw) > MAX_TEXT ? { error: `Use at most ${MAX_TEXT} characters.` } : { ok: true, value: raw };
  }
  const parsed = parseInput(prop.type, raw);
  return parsed.error ? { error: parsed.error } : { ok: true, value: parsed.value };
}

export function filterForm(schema, onSubmit) {
  const usable = schema.properties.filter((p) => OPERATORS[p.type]);
  const propSel = h('select', { 'aria-label': 'Property' }, usable.map((p) => h('option', { value: p.id }, p.name)));
  const opSel = h('select', { 'aria-label': 'Condition' });
  const slot = h('div', { class: 'db-slot' });
  const error = h('p', { class: 'db-error', role: 'alert' });
  let field = null;
  const syncValue = () => {
    slot.replaceChildren();
    error.textContent = '';
    field = valueField(schema, propertyOf(schema, propSel.value), opSel.value);
    if (field) slot.append(field);
  };
  const syncOps = () => {
    const p = propertyOf(schema, propSel.value);
    opSel.replaceChildren(...OPERATORS[p.type].map((o) => h('option', { value: o }, o)));
    opSel.value = OPERATORS[p.type][0];
    syncValue();
  };
  propSel.addEventListener('change', syncOps);
  opSel.addEventListener('change', syncValue);
  const form = h('form', { class: 'db-form', 'aria-label': 'Add filter' },
    h('p', { class: 'db-form-head' }, 'Show rows where'), propSel, opSel, slot, error,
    h('p', { class: 'note' }, 'All conditions must hold. Everyone sees this view’s filter.'),
    h('button', { type: 'submit', class: 'btn primary' }, 'Add filter'));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const p = propertyOf(schema, propSel.value);
    const op = opSel.value;
    const got = field ? readFilterValue(p, op, field) : { ok: true };
    if (got.error) {
      error.textContent = got.error;
      if (field) fieldError(field, got.error);
      return;
    }
    const cond = { property: p.id, op };
    if (needsValue(op)) cond.value = got.value;
    onSubmit((current) => ({ ...current, filter: [...current.filter, cond] }));
  });
  if (usable.length) {
    propSel.value = usable[0].id;
    syncOps();
  }
  return form;
}

export function sortForm(schema, view, index, onSubmit) {
  const cur = view.sort[index] || { property: schema.properties.find((p) => !view.sort.some((s) => s.property === p.id))?.id, direction: 'asc' };
  const propSel = h('select', { 'aria-label': 'Sort by' }, schema.properties.map((p) => h('option', { value: p.id, selected: p.id === cur.property }, p.name)));
  const dirSel = h('select', { 'aria-label': 'Direction' },
    h('option', { value: 'asc', selected: cur.direction === 'asc' }, 'Ascending'),
    h('option', { value: 'desc', selected: cur.direction === 'desc' }, 'Descending'));
  propSel.value = cur.property;
  dirSel.value = cur.direction;
  const replace = (replacement) => onSubmit((current) => {
    const at = index < 0 ? -1 : current.sort.findIndex((s) => s.property === cur.property);
    if (index >= 0 && at < 0) return null;
    const sort = current.sort.filter((s, i) => i !== at && s.property !== replacement?.property);
    if (replacement) {
      if (at >= 0) sort.splice(Math.min(at, sort.length), 0, replacement);
      else sort.push(replacement);
    }
    return { ...current, sort };
  });
  const form = h('form', { class: 'db-form', 'aria-label': 'Sort' },
    h('p', { class: 'db-form-head' }, index >= 0 ? `Sort ${index + 1} of ${view.sort.length}` : 'Add a sort'), propSel, dirSel,
    h('p', { class: 'note' }, 'Empty values sort last. Ties keep the shared row order. Everyone sees this view’s sort.'),
    h('button', { type: 'submit', class: 'btn primary' }, index >= 0 ? 'Apply' : 'Add sort'),
    index >= 0 ? h('button', { type: 'button', class: 'btn', onclick: () => replace(null) }, 'Remove sort') : null);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const s = { property: propSel.value, direction: dirSel.value };
    replace(s);
  });
  return form;
}

export function filterLabel(schema, f) {
  const p = propertyOf(schema, f.property);
  if (!p) return f.op;
  const name = (id) => (schema.options[p.id] || []).find((o) => o.id === id)?.name ?? 'deleted option';
  let value = '';
  if (needsValue(f.op)) value = ' ' + (Array.isArray(f.value) ? f.value.map(name).join(', ') : p.type === 'select' || p.type === 'multi_select' ? name(f.value) : f.value);
  return `${p.name} ${f.op}${value}`;
}

// ---- Property controls ----

// Conversion asks the server what a type change would empty, shows that,
// and asks again on confirmation. If the answer changed, the person must
// confirm the new numbers; nothing is written in between (TECH 6).
export async function changeType(ctx, schema, prop, definition, ui) {
  let shown = await ctx.preview(ref(prop), definition);
  for (;;) {
    ui.show(conversionText(prop, definition, shown));
    if (!(await ui.confirm())) return false;
    const now = await ctx.preview(ref(prop), definition);
    if (now.preview === shown.preview) {
      ctx.onOps([ops.setProperty(prop, definition, shown.preview)]);
      return true;
    }
    shown = now;
    ui.changed?.();
  }
}

export function conversionText(prop, def, p) {
  const parts = [`Change ${prop.name} from ${TYPE_NAMES[prop.type]} to ${TYPE_NAMES[def.type]}.`];
  parts.push(p.emptied ? `${p.emptied} ${p.emptied === 1 ? 'value' : 'values'} will be emptied.` : 'No values will be emptied.');
  if (p.options_created) parts.push(`${p.options_created} ${p.options_created === 1 ? 'option' : 'options'} will be created.`);
  if (prop.type === 'select' || prop.type === 'multi_select') {
    if (def.type !== 'select' && def.type !== 'multi_select') parts.push('Its options will be deleted.');
  }
  parts.push('The previous values stay in the revision closed just before the change.');
  return parts.join(' ');
}

export async function defaultPreview(property, definition) {
  const res = await fetch('/_notebook/api/conversion-preview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Flats-Notebook': '1' },
    body: JSON.stringify({ property, definition }),
  });
  if (!res.ok) throw new Error(`preview failed: ${res.status}`);
  return res.json();
}

function uniqueName(names, base) {
  if (!names.includes(base)) return base;
  for (let i = 2; ; i++) if (!names.includes(`${base} ${i}`)) return `${base} ${i}`;
}

export function addProperty(ctx, schema, view) {
  const name = uniqueName(schema.properties.map((p) => p.name), 'Property');
  const { op, id } = ops.createProperty(schema, { name, type: 'text' });
  const definition = viewToWire(schema, view.def);
  definition.display.visible.push({ tmp: op.ref });
  const unit = newUnit();
  ctx.onOps([{ ...op, unit_id: unit }, { op: 'set_view', view: ref(view), definition, unit_id: unit }]);
  return id;
}

function nameInput(label, value, commit) {
  const input = h('input', { type: 'text', 'aria-label': label, value });
  const error = h('p', { class: 'db-error', role: 'alert' });
  const save = () => {
    const v = input.value;
    if (v === value) return;
    if (v.trim() === '') { fieldError(input, 'A name cannot be empty.'); error.textContent = 'A name cannot be empty.'; return; }
    if (codePoints(v) > MAX_NAME) { error.textContent = `Use at most ${MAX_NAME} characters.`; fieldError(input, error.textContent); return; }
    const refused = commit(v);
    if (refused) { error.textContent = refused; fieldError(input, refused); return; }
    value = v;
    input.removeAttribute('aria-invalid');
    error.textContent = '';
  };
  input.addEventListener('change', save);
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); save(); } });
  return h('div', { class: 'db-field' }, input, error);
}

function optionsEditor(ctx, schema, prop) {
  const list = schema.options[prop.id] || [];
  const names = list.map((o) => o.name);
  return h('div', { class: 'db-options', role: 'group', 'aria-label': `${prop.name} options` },
    list.map((o) => h('div', { class: 'db-option' },
      h('span', { class: 'opt ' + o.color }, o.name),
      nameInput(`Rename option ${o.name}`, o.name, (v) => (names.includes(v) ? 'Another option already has this name.' : (ctx.onOps([ops.setOption(o, v, o.color)]), null))),
      h('select', { 'aria-label': `Color of ${o.name}`, onchange: (e) => ctx.onOps([ops.setOption(o, o.name, e.target.value)]) },
        COLORS.map((c) => h('option', { value: c, selected: c === o.color }, c))),
      h('button', { type: 'button', class: 'btn', 'aria-label': `Delete option ${o.name}`, onclick: () => ctx.onOps([ops.deleteOption(o)]) }, 'Delete'))),
    h('button', {
      type: 'button', class: 'btn',
      onclick: () => {
        const name = uniqueName(names, 'Option');
        ctx.onOps([ops.createOption(prop, name, COLORS[list.length % COLORS.length]).op]);
      },
    }, 'Add option'));
}

// propertyMenu holds rename, type and format, options and deletion.
export function propertyMenu(ctx, schema, prop, ui) {
  const isTitle = prop.type === 'title';
  const names = schema.properties.filter((p) => p !== prop).map((p) => p.name);
  const status = h('p', { class: 'db-status', role: 'status' });
  const confirmBox = h('div', { class: 'db-confirm' });
  const typeSel = isTitle ? null : h('select', { 'aria-label': 'Type' }, TYPES.map((t) => h('option', { value: t, selected: t === prop.type }, TYPE_NAMES[t])));
  if (typeSel) typeSel.value = prop.type;
  if (typeSel) {
    typeSel.addEventListener('change', () => {
      const def = definitionOf(prop, { type: typeSel.value });
      let answer = null;
      const confirmUI = {
        show(text) {
          confirmBox.replaceChildren(h('p', { role: 'alert' }, text),
            h('button', { type: 'button', class: 'btn primary', onclick: () => answer?.(true) }, 'Change type'),
            h('button', { type: 'button', class: 'btn', onclick: () => answer?.(false) }, 'Cancel'));
          confirmBox.querySelector('button').focus();
        },
        confirm: () => new Promise((resolve) => { answer = resolve; }),
        changed() { status.textContent = 'The values changed while you were deciding. Check the new numbers.'; },
      };
      changeType(ctx, schema, prop, def, confirmUI).then((done) => {
        confirmBox.replaceChildren();
        if (!done) typeSel.value = prop.type;
        else ui?.close?.();
      }).catch(() => {
        confirmBox.replaceChildren();
        typeSel.value = prop.type;
        status.textContent = 'The change could not be previewed. Try again.';
      });
    });
  }
  const formatSel = prop.type === 'number'
    ? h('select', { 'aria-label': 'Number format', onchange: (e) => ctx.onOps([ops.setProperty(prop, definitionOf(prop, { format: e.target.value }))]) },
      Object.entries(FORMATS).map(([k, v]) => h('option', { value: k, selected: k === (prop.format || 'plain') }, v)))
    : null;
  return h('div', { class: 'db-menu' },
    h('p', { class: 'db-form-head' }, prop.name),
    nameInput('Property name', prop.name, (v) => (names.includes(v) ? 'Another property already has this name.' : (ctx.onOps([ops.setProperty(prop, definitionOf(prop, { name: v }))]), null))),
    isTitle ? h('p', { class: 'note' }, 'The title property is always first. Its type cannot change and it cannot be deleted.') : typeSel,
    confirmBox, formatSel,
    prop.type === 'select' || prop.type === 'multi_select' ? optionsEditor(ctx, schema, prop) : null,
    isTitle ? null : h('button', { type: 'button', class: 'btn danger', onclick: () => { ctx.onOps([ops.deleteProperty(prop)]); ui?.close?.(); } }, 'Delete property'),
    status);
}

// ---- View controls ----

export function newTableDefinition(schema, name) {
  return { name, layout: 'table', visible: schema.properties.map((p) => p.id), widths: {}, groupBy: null, showEmpty: false, sort: [], filter: [] };
}

export function viewMenu(ctx, schema, views, view, ui) {
  const def = view.def;
  const write = ui.write;
  const selects = schema.properties.filter((p) => p.type === 'select');
  const names = views.filter((v) => v !== view).map((v) => v.def.name);
  const visibleBox = h('fieldset', { class: 'db-visible' }, h('legend', {}, 'Shown properties'),
    schema.properties.map((p) => {
      const locked = p.type === 'title' && def.layout === 'table';
      return h('label', {}, h('input', {
        type: 'checkbox', checked: def.visible.includes(p.id), disabled: locked,
        onchange: (e) => write((current) => ({ ...current, visible: e.target.checked ? [...new Set([...current.visible, p.id])] : current.visible.filter((id) => id !== p.id) })),
      }), p.name, locked ? ' (always shown)' : '');
    }));
  const kanban = def.layout === 'kanban'
    ? [h('label', {}, 'Group by ', h('select', { 'aria-label': 'Group by', onchange: (e) => write((current) => ({ ...current, groupBy: e.target.value })) },
      selects.map((p) => h('option', { value: p.id, selected: p.id === def.groupBy }, p.name)))),
    h('label', {}, h('input', { type: 'checkbox', checked: def.showEmpty, onchange: (e) => write((current) => ({ ...current, showEmpty: e.target.checked })) }), 'Show empty columns')]
    : [];
  return h('div', { class: 'db-menu' },
    h('p', { class: 'db-form-head' }, 'View'),
    nameInput('View name', def.name, (v) => (names.includes(v) ? 'Another view already has this name.' : (write((current) => ({ ...current, name: v })), null))),
    kanban, visibleBox,
    h('button', {
      type: 'button', class: 'btn danger', disabled: views.length <= 1,
      title: views.length <= 1 ? 'A database keeps at least one view.' : null,
      onclick: () => { if (views.length > 1) { ctx.onOps([ops.deleteView(view)]); ui?.close?.(); } },
    }, 'Delete view'),
    views.length <= 1 ? h('p', { class: 'note' }, 'A database keeps at least one view.') : null);
}

export function addViewMenu(ctx, schema, views, onCreated) {
  const names = views.map((v) => v.def.name);
  const group = schema.properties.find((p) => p.type === 'select');
  const create = (def) => {
    const { op, id } = ops.createView(schema, viewToWire(schema, def));
    ctx.onOps([op]);
    onCreated?.(id);
  };
  return h('div', { class: 'db-menu', role: 'menu' },
    h('p', { class: 'db-form-head' }, 'New view'),
    h('button', { type: 'button', role: 'menuitem', class: 'mi', onclick: () => create(newTableDefinition(schema, uniqueName(names, 'Table'))) }, 'Table'),
    group
      ? h('button', {
        type: 'button', role: 'menuitem', class: 'mi',
        onclick: () => create({ ...newTableDefinition(schema, uniqueName(names, `By ${group.name}`)), layout: 'kanban', groupBy: group.id, visible: schema.properties.filter((p) => p.type !== 'title').map((p) => p.id), showEmpty: true }),
      }, 'Kanban')
      : h('p', { class: 'note' }, 'A kanban needs a select property.'));
}
