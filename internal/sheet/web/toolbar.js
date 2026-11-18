// The format toolbar (rule 21): every format of rule 6 on the selection,
// undo and redo, and the doors to the rows and columns menu, the sort
// dialog and the freeze menu. On a phone-width screen it collapses to a
// menu (rule 20). The toolbar sets format properties by name; the server
// decides what a cell then shows.
'use strict';

import { button, h, icon, iconButton, plural, replace } from './dom.js';
import { DATE_FORMS, HUES, NUMBER_KINDS, SHADES, TIME_FORMS, dateForm, numberFormat, numberKind, timeForm } from './protocol.js';

const MARKS = [
  ['bold', 'Bold (Ctrl or Cmd+B)', 'B', 'mk-b'],
  ['italic', 'Italic (Ctrl or Cmd+I)', 'I', 'mk-i'],
  ['underline', 'Underline (Ctrl or Cmd+U)', 'U', 'mk-u'],
  ['strikethrough', 'Strikethrough', 'S', 'mk-s'],
];
const ALIGNS = [['left', 'Align left'], ['center', 'Align center'], ['right', 'Align right']];

// Toolbar draws the controls. cb: format(set, target, texts), which
// returns whether the format was sent, target(), which names the selected
// cells for a dialog or a menu to act on later, undo(), redo(),
// sort(desc), sortDialog(at), rowsMenu(at), freezeMenu(at), popups.
export class Toolbar {
  constructor(cb) {
    this.cb = cb;
    this.el = h('div', { class: 'tools', role: 'toolbar', 'aria-label': 'Format and structure' });
    this.sig = '';
  }

  // render draws the toolbar for the active cell's format f. unknown says
  // that the active cell has not arrived and its format is not known, so
  // the controls that change the format the cell has are off until it is.
  render({ format: f, disabled, unknown = false, phone, frozenRows, frozenCols }) {
    // Drawing again only when something changed keeps focus on the control
    // the person is using.
    const sig = JSON.stringify([f, disabled, unknown, phone, frozenRows, frozenCols]);
    if (sig === this.sig) return;
    this.sig = sig;
    const cb = this.cb;
    const below = (e) => { const r = e.currentTarget.getBoundingClientRect(); return { x: r.left, y: r.bottom + 4 }; };
    const undo = [
      iconButton('undo', 'Undo (Ctrl or Cmd+Z)', cb.undo, { disabled }),
      iconButton('redo', 'Redo (Shift+Ctrl or Cmd+Z)', cb.redo, { disabled }),
      h('span', { class: 'tsep' }),
    ];
    const from = disabled || unknown;
    if (phone) {
      replace(this.el, undo, h('button', { type: 'button', class: 'tb wide', 'aria-haspopup': 'menu', disabled: from, onclick: (e) => this.#phoneMenu(f, below(e)) }, 'Format', icon('more')));
      return;
    }
    const nf = h('select', { 'aria-label': 'Number format', title: 'Number format', disabled: from, onchange: (e) => cb.format({ number: numberFormat(e.target.value) }) },
      NUMBER_KINDS.map(([v, label]) => h('option', { value: v, text: label })));
    nf.value = numberKind(f);
    const mark = ([key, label, glyph, cls]) => iconButton(h('span', { class: cls, text: glyph }), label, () => cb.format({ [key]: !f[key] }), { 'aria-pressed': String(!!f[key]), disabled: from });
    const align = ([v, label]) => iconButton(v, label, () => cb.format({ align: f.align === v ? null : v }), { 'aria-pressed': String(f.align === v), disabled: from });
    const frozen = frozenRows || frozenCols;
    replace(this.el, undo, nf,
      h('button', { type: 'button', class: 'tb wide', 'aria-haspopup': 'dialog', 'aria-label': 'Format options: decimals, separator, currency symbol, date and time forms', title: 'Format options', disabled: from, onclick: (e) => this.options(f, below(e)) }, 'Options…'),
      iconButton(h('span', { text: '.0' }), 'Fewer decimals', () => this.#decimals(f, -1), { class: 'tb wide', disabled: from }),
      iconButton(h('span', { text: '.00' }), 'More decimals', () => this.#decimals(f, 1), { class: 'tb wide', disabled: from }),
      h('span', { class: 'tsep' }),
      MARKS.map(mark),
      iconButton(swatch('chipA', f.text_color, 'A'), 'Text color', (e) => this.palette('text_color', f.text_color, below(e)), { 'aria-haspopup': 'dialog', disabled }),
      iconButton(swatch('chipF', f.fill), 'Fill color', (e) => this.palette('fill', f.fill, below(e)), { 'aria-haspopup': 'dialog', disabled }),
      h('span', { class: 'tsep' }),
      ALIGNS.map(align),
      iconButton('wrap', 'Wrap text', () => cb.format({ wrap: !f.wrap }), { 'aria-pressed': String(!!f.wrap), disabled: from }),
      h('span', { class: 'tsep' }),
      h('button', { type: 'button', class: 'tb wide', 'aria-haspopup': 'menu', 'aria-label': 'Rows and columns menu', title: 'Rows and columns: insert, delete, width, fill, add at the end (Shift+F10 in the grid)', disabled, onclick: (e) => cb.rowsMenu(below(e)) }, icon('rows'), 'Rows & columns'),
      iconButton('asc', 'Sort ascending by the active column', () => cb.sort(false), { disabled }),
      iconButton('desc', 'Sort descending by the active column', () => cb.sort(true), { disabled }),
      h('button', { type: 'button', class: 'tb wide', 'aria-haspopup': 'dialog', title: 'Sort rows by one column', disabled, onclick: (e) => cb.sortDialog(below(e)) }, 'Sort…'),
      h('button', { type: 'button', class: 'tb wide', 'aria-haspopup': 'menu', 'aria-pressed': String(!!frozen), 'aria-label': 'Freeze rows and columns', title: frozen ? `Frozen: ${plural(frozenRows, 'row')}, ${plural(frozenCols, 'column')}` : 'Freeze rows and columns', disabled, onclick: (e) => cb.freezeMenu(below(e)) }, icon('freeze'), 'Freeze'));
  }

  #decimals(f, d) {
    const n = f.number || { kind: 'general' };
    const kind = ['number', 'percent', 'currency'].includes(n.kind) ? n.kind : 'number';
    const now = n.kind === kind ? n.decimals : 2 - d;
    this.cb.format({ number: numberFormat(kind, { ...n, decimals: Math.max(0, Math.min(10, now + d)) }) });
  }

  #phoneMenu(f, at) {
    const cb = this.cb;
    const on = (v) => (v ? ' (on)' : '');
    // The menu acts on the cells it was opened for.
    const target = cb.target();
    const set = (s) => cb.format(s, target);
    cb.popups.menu('Format', [
      ...MARKS.map(([key, label]) => ({ label: label.replace(/ \(.*/, '') + on(f[key]), fn: () => set({ [key]: !f[key] }) })),
      { label: `Wrap text${on(f.wrap)}`, fn: () => set({ wrap: !f.wrap }) }, 'hr',
      ...ALIGNS.map(([v, label]) => ({ label: label + on(f.align === v), fn: () => set({ align: f.align === v ? null : v }) })), 'hr',
      { head: 'Number format' },
      ...NUMBER_KINDS.map(([v, label]) => ({ label: label + on(numberKind(f) === v), fn: () => set({ number: numberFormat(v) }) })),
    ], at);
  }

  // palette opens the 54 palette colors of rule 6 for the text color or the
  // fill. Each swatch is drawn from the theme's tokens.
  palette(key, current, at) {
    const cb = this.cb;
    const target = cb.target();
    const pick = (v) => { cb.popups.close(); cb.format({ [key]: v }, target); };
    const title = key === 'fill' ? 'Fill color' : 'Text color';
    const grid = h('div', { class: 'palgrid', role: 'group', 'aria-label': title });
    for (const shade of SHADES) {
      for (const hue of HUES) {
        const name = `${hue}-${shade}`;
        const sw = button('', () => pick(name), { class: `sw sw-${name}`, 'aria-label': `${hue} ${shade}`, 'aria-pressed': String(current === name), title: `${hue} ${shade}` });
        grid.appendChild(sw);
      }
    }
    cb.popups.dialog(title, [h('h2', { text: title }), grid,
      button('None', () => pick(null), { class: 'btn none', 'aria-pressed': String(!current) })], at, { class: 'pal' });
  }

  // options sets the settings of the number format: decimals, separator,
  // currency symbol and the date and time forms.
  options(f, at) {
    const cb = this.cb;
    const target = cb.target();
    const cur = f.number || { kind: 'general' };
    const kind = h('select', { id: 'nf-kind' }, NUMBER_KINDS.map(([v, label]) => h('option', { value: v, text: label })));
    kind.value = cur.kind;
    const decimals = h('input', { type: 'number', id: 'nf-dec', min: '0', max: '10', step: '1' });
    decimals.value = String(cur.decimals ?? 2);
    const thousands = h('input', { type: 'checkbox', id: 'nf-sep' });
    thousands.checked = cur.thousands ?? true;
    const symbol = h('input', { type: 'text', id: 'nf-sym', maxlength: '3' });
    symbol.value = cur.symbol || '$';
    const date = h('select', { id: 'nf-date' }, DATE_FORMS.map((v) => h('option', { value: v, text: v })));
    date.value = dateForm(cur);
    const time = h('select', { id: 'nf-time' }, TIME_FORMS.map((v) => h('option', { value: v, text: v })));
    time.value = timeForm(cur);
    const err = h('div', { class: 'err', role: 'alert' });
    const rows = {
      decimals: h('label', { for: 'nf-dec' }, 'Decimals, 0 to 10', decimals),
      thousands: h('label', { class: 'check', for: 'nf-sep' }, thousands, 'Thousands separator'),
      symbol: h('label', { for: 'nf-sym' }, 'Currency symbol, 1 to 3 characters', symbol),
      date: h('label', { for: 'nf-date' }, 'Date form', date),
      time: h('label', { for: 'nf-time' }, 'Time form', time),
    };
    const uses = { number: ['decimals', 'thousands'], percent: ['decimals'], currency: ['symbol', 'decimals'], date: ['date'], time: ['time'], datetime: ['date', 'time'] };
    const show = () => { for (const [k, el] of Object.entries(rows)) el.hidden = !(uses[kind.value] || []).includes(k); };
    kind.addEventListener('change', show);
    show();
    const apply = () => {
      const d = Math.round(Number(decimals.value));
      const sym = symbol.value.trim();
      const needs = uses[kind.value] || [];
      if (needs.includes('decimals') && !(d >= 0 && d <= 10)) { err.textContent = 'Decimals go from 0 to 10.'; decimals.focus(); return; }
      if (needs.includes('symbol') && ([...sym].length < 1 || [...sym].length > 3)) { err.textContent = 'A currency symbol has 1 to 3 characters.'; symbol.focus(); return; }
      // A format the page cannot send keeps the dialog and what was typed in
      // it; one the host refuses offers the settings back.
      const number = numberFormat(kind.value, { decimals: d, thousands: thousands.checked, symbol: sym, date: date.value, time: time.value });
      if (cb.format({ number }, target, [numberText(number)])) cb.popups.close();
      else err.textContent = 'The format was not applied. Your settings are kept here.';
    };
    cb.popups.dialog('Format options', [
      h('h2', { text: 'Number format' }),
      h('label', { for: 'nf-kind' }, 'Kind', kind), Object.values(rows), err,
      h('div', { class: 'rowb' }, button('Cancel', () => cb.popups.close(), { class: 'btn' }), button('Apply', apply, { class: 'btn primary' })),
    ], at, { focus: kind });
  }
}

// numberText words a number format's settings for a person to read again.
function numberText(n) {
  const parts = [NUMBER_KINDS.find(([v]) => v === n.kind)[1]];
  if (n.symbol != null) parts.push(`symbol ${n.symbol}`);
  if (n.decimals != null) parts.push(plural(n.decimals, 'decimal'));
  if (n.thousands != null) parts.push(n.thousands ? 'thousands separator' : 'no thousands separator');
  if (n.pattern != null) parts.push(`form ${n.pattern}`);
  if (n.date != null) parts.push(`date form ${n.date}`);
  if (n.time != null) parts.push(`time form ${n.time}`);
  return `Number format: ${parts.join(', ')}`;
}

function swatch(cls, color, text = '') {
  return h('span', { class: `${cls}${color ? ` sw-${color}` : ''}`, text });
}
