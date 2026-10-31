// Menus (rule 20): the slash menu of block kinds filtered as the person
// types, and the block handle's actions menu. Both are listboxes or menus
// driven by the keyboard as well as the pointer.
'use strict';


import { h, icon } from './dom.js';
import { KIND_NAMES } from './blocks.js';

// SLASH_ITEMS: [key, label, description, icon, shortcut]. Text kinds turn
// the current block; the others create a block.
export const SLASH_ITEMS = [
  ['paragraph', 'Text', 'Plain paragraph', 'text'],
  ['heading1', 'Heading 1', 'Large section heading', 'text', '#'],
  ['heading2', 'Heading 2', 'Medium section heading', 'text', '##'],
  ['heading3', 'Heading 3', 'Small section heading', 'text', '###'],
  ['bullet', 'Bulleted list', 'A simple list', 'list', '-'],
  ['numbered', 'Numbered list', 'A list with numbers', 'list', '1.'],
  ['todo', 'To-do', 'Track tasks with a checkbox', 'check', '[]'],
  ['toggle', 'Toggle', 'Hide content inside', 'chev'],
  ['quote', 'Quote', 'Capture a quote', 'quote', '>'],
  ['callout', 'Callout', 'Make text stand out', 'info'],
  ['code', 'Code', 'Code with a language label', 'code', '```'],
  ['divider', 'Divider', 'Separate sections', 'minus', '---'],
  ['image', 'Image', 'Upload PNG, JPEG, GIF or WebP', 'image'],
  ['page_link', 'Page link', 'Link to another page', 'link'],
  ['new_page', 'New page', 'Subpage with a link here', 'plus'],
  ['database', 'Database', 'Inline table or kanban', 'table'],
];

const fold = (s) => s.toLocaleLowerCase('en');

// filterSlash returns the items whose label or key matches query: a prefix
// of any word first, then other substrings.
export function filterSlash(query) {
  const q = fold(query.trim());
  if (!q) return SLASH_ITEMS;
  const words = (it) => fold(it[1]).split(/\s+/).concat(fold(it[0]));
  const first = SLASH_ITEMS.filter((it) => words(it).some((w) => w.startsWith(q)));
  const rest = SLASH_ITEMS.filter((it) => !first.includes(it) && (fold(it[1]).includes(q) || (it[4] && it[4] === query.trim())));
  return [...first, ...rest];
}

let menuSeq = 0;

// SlashMenu is a listbox tied to the editable with aria-controls and
// aria-activedescendant; focus stays in the text while it is open. It sits
// inside the block's element, so it opens right below that block.
export class SlashMenu {
  constructor(onPick) {
    this.onPick = onPick;
    this.el = null;
    this.items = [];
    this.sel = 0;
    this.block = null;
    this.range = null;
    this.editable = null;
  }

  get open() { return !!this.el; }

  show(editable, block, query, range) {
    this.items = filterSlash(query);
    if (this.block !== block.ref.id) this.sel = 0;
    this.sel = Math.min(this.sel, Math.max(0, this.items.length - 1));
    this.block = block.ref.id;
    this.range = range;
    this.editable = editable;
    const id = 'slash-' + (++menuSeq);
    const list = h('div', { class: 'menu slash', role: 'listbox', id, 'aria-label': 'Block kinds' },
      h('div', { class: 'mh' }, query ? `Blocks matching “${query}”` : 'Basic blocks'),
      this.items.length ? this.items.map((it, i) => h('div', {
        class: 'mi', role: 'option', id: `${id}-${it[0]}`, 'data-key': it[0], 'aria-selected': String(i === this.sel),
        onmousedown: (e) => e.preventDefault(),
        onclick: () => { this.sel = i; this.pick(); },
      }, h('span', { class: 'ic' }, icon(it[3])), h('span', { class: 'lbl' }, it[1], h('small', {}, it[2])), it[4] ? h('kbd', {}, it[4]) : null))
        : h('div', { class: 'mh empty' }, 'No block kinds match'));
    if (this.el) this.el.remove();
    (editable.closest('.blk') || editable.parentNode).appendChild(list);
    this.el = list;
    editable.setAttribute('aria-controls', id);
    editable.setAttribute('aria-expanded', 'true');
    this.#active();
  }

  #active() {
    const it = this.items[this.sel];
    if (it) this.editable.setAttribute('aria-activedescendant', `${this.el.id}-${it[0]}`);
    else this.editable.removeAttribute('aria-activedescendant');
    for (const [i, o] of Array.from(this.el.querySelectorAll('.mi')).entries()) o.setAttribute('aria-selected', String(i === this.sel));
  }

  move(d) {
    if (!this.items.length) return;
    this.sel = (this.sel + d + this.items.length) % this.items.length;
    this.#active();
  }

  pick() {
    const it = this.items[this.sel];
    const block = this.block, range = this.range;
    this.close();
    if (it) this.onPick(it[0], block, range);
  }

  // key handles a keydown while open and reports whether it did.
  key(e) {
    if (e.key === 'ArrowDown') { e.preventDefault(); this.move(1); return true; }
    if (e.key === 'ArrowUp') { e.preventDefault(); this.move(-1); return true; }
    if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); this.pick(); return true; }
    if (e.key === 'Escape') { e.preventDefault(); this.close(); return true; }
    return false;
  }

  close() {
    if (this.el) this.el.remove();
    if (this.editable) {
      this.editable.removeAttribute('aria-controls');
      this.editable.removeAttribute('aria-activedescendant');
      this.editable.removeAttribute('aria-expanded');
    }
    this.el = null;
    this.block = null;
    this.editable = null;
  }
}

// actionMenu builds a menu of [label, fn, {disabled, hint}] entries and
// section headings (strings). Arrow keys move, Enter picks, Escape closes.
export function actionMenu(label, entries, onClose) {
  const items = [];
  const m = h('div', { class: 'menu', role: 'menu', 'aria-label': label });
  const close = () => { m.remove(); onClose && onClose(); };
  for (const e of entries) {
    if (typeof e === 'string') { m.appendChild(h('div', { class: 'mh', role: 'presentation' }, e)); continue; }
    const [text, fn, opts = {}] = e;
    const b = h('button', { type: 'button', class: 'mi', role: 'menuitem', tabindex: '-1', disabled: opts.disabled || null, title: opts.hint || null },
      h('span', { class: 'lbl' }, text, opts.hint ? h('small', {}, opts.hint) : null));
    b.addEventListener('click', () => { close(); fn(); });
    items.push(b);
    m.appendChild(b);
  }
  m.addEventListener('keydown', (e) => {
    const live = items.filter((b) => !b.hasAttribute('disabled'));
    const i = live.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); live[(i + 1) % live.length]?.focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); live[(i - 1 + live.length) % live.length]?.focus(); }
    else if (e.key === 'Escape') { e.preventDefault(); close(); }
  });
  m.focusFirst = () => { const f = items.find((b) => !b.hasAttribute('disabled')); if (f) f.focus(); };
  m.close = close;
  return m;
}

export const kindLabel = (k) => KIND_NAMES[k] || k;
