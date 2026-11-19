// The tab bar (rules 2, 17 and 21): the tabs in order with the open one
// marked, add, rename in place, reorder by dragging or from the menu, and
// delete with an inline confirmation.
'use strict';

import { button, h, icon, replace } from './dom.js';
import { MAX_TAB_NAME, MAX_TABS } from './protocol.js';

const DRAG_START = 6; // pixels a press travels before it is a drag

// tabNameError says why a name cannot be a tab's name (rule 2), or returns
// null. The server checks again; this only spares a round trip.
export function tabNameError(name, tabs, self) {
  const n = [...name].length;
  if (n < 1 || n > MAX_TAB_NAME) return `A tab name has 1 to ${MAX_TAB_NAME} characters.`;
  const key = name.toLowerCase();
  if (key === 'history') return 'A tab cannot be named History, in any capitals.';
  // biome-ignore lint/suspicious/noControlCharactersInRegex: rule 2 excludes control characters
  if (/[[\]:*?/\\\u0000-\u001f\u007f]/.test(name)) return 'A tab name cannot hold [ ] : * ? / \\ or a control character.';
  if (/^[ ']|[ ']$/.test(name)) return 'A tab name cannot start or end with a space or an apostrophe.';
  if (tabs.some((t) => t.id !== self && t.name.toLowerCase() === key)) return 'Another tab already has this name.';
  return null;
}

// freeName picks "SheetN" with the lowest N no tab uses.
export function freeName(tabs) {
  const used = new Set(tabs.map((t) => t.name.toLowerCase()));
  for (let i = 1; ; i++) if (!used.has(`sheet${i}`)) return `Sheet${i}`;
}

// Tabs draws the bar. cb: open(id), add(name), rename(id, name, base),
// move(id, before, base), remove(id, base), seq(), needsConfirm(id),
// menu(label, items, at), say(text, kept), focusGrid(). A rename field, a
// delete confirmation, a menu and a drag pass the sequence of the screen
// they started on, seq() then, as the base of what they send.
export class Tabs {
  constructor(cb, extra) {
    this.cb = cb;
    this.list = h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'Tabs' });
    this.addBtn = button('Add a tab', () => cb.add(freeName(this.state.tabs)), { class: 'tb', 'aria-label': 'Add a tab', title: 'Add a tab' }, icon('plus'));
    this.confirmEl = h('div', { class: 'confirm', role: 'alertdialog', 'aria-label': 'Delete tab', hidden: true });
    this.closeConfirm = null;
    this.confirmEl.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || !this.closeConfirm) return;
      e.preventDefault();
      e.stopPropagation();
      this.closeConfirm(false);
    });
    this.el = h('footer', { class: 'tabbar' }, this.addBtn, this.list, this.confirmEl, h('span', { class: 'grow' }), extra);
    this.state = { tabs: [], open: null, readOnly: true, people: new Set() };
    this.renaming = null;
    this.drag = null;
    this.sig = '';
  }

  render(state = this.state) {
    this.state = state;
    const { tabs, open, readOnly, people } = state;
    // A rename field whose tab is gone ends without writing.
    if (this.renaming != null && !tabs.some((t) => t.id === this.renaming)) { this.endRename?.(); this.renaming = null; }
    // Drawing again only when something changed keeps focus, a drag and a
    // rename field alive.
    const sig = JSON.stringify([tabs.map((t) => [t.id, t.name]), open, readOnly, [...people], this.renaming]);
    if (sig === this.sig) return;
    this.sig = sig;
    this.addBtn.hidden = readOnly;
    this.addBtn.disabled = tabs.length >= MAX_TABS;
    // Each tab keeps its element while its id, its name and what can be
    // done with it stay the same. The open mark and the dot change in
    // place. Replacing the element would drop a click that is under way
    // (the second click of a double click never arrives on a new element)
    // and would empty a rename field the person is typing in, which
    // happens whenever a tab loads or someone else opens one meanwhile.
    const old = new Map();
    for (const el of this.list.children) old.set(el._key, el);
    const next = tabs.map((t) => {
      const renaming = this.renaming === t.id;
      const key = renaming ? `rename ${t.id}` : `${t.id} ${readOnly} ${t.name}`;
      let el = old.get(key);
      if (!el) {
        el = renaming ? this.#renameField(t) : this.#tab(t);
        el._key = key;
      }
      if (!renaming) {
        const on = t.id === open;
        if (el.getAttribute('aria-selected') !== String(on)) el.setAttribute('aria-selected', String(on));
        if (el.getAttribute('tabindex') !== (on ? '0' : '-1')) el.setAttribute('tabindex', on ? '0' : '-1');
        const dot = people.has(t.id);
        if (dot && !el._dot) { el._dot = h('span', { class: 'pdot', title: 'Someone else has this tab open' }); el.insertBefore(el._dot, el.firstChild); }
        if (!dot && el._dot) { el._dot.remove(); el._dot = null; }
      }
      return el;
    });
    const keep = new Set(next);
    for (const el of [...this.list.children]) if (!keep.has(el)) el.remove();
    // Only elements that are out of place move: a moved element loses focus.
    let cursor = this.list.firstChild;
    for (const el of next) {
      if (el === cursor) { cursor = cursor.nextSibling; continue; }
      this.list.insertBefore(el, cursor);
    }
  }

  // #tab makes the button of one tab. Its handlers read the tab's place and
  // the bar's state when they run, because the button outlives a redraw.
  #tab(t) {
    return h('button', {
      type: 'button', class: 'tab', role: 'tab', id: `tab-${t.id}`, 'data-tab': String(t.id),
      // Opening a tab puts focus in its grid, ready for typing.
      onclick: () => { this.cb.open(t.id); this.cb.focusGrid(); },
      ondblclick: () => { if (!this.state.readOnly) this.rename(t.id); },
      onkeydown: (e) => this.#key(e, t, this.state.tabs.findIndex((x) => x.id === t.id)),
      onmousedown: (e) => this.#press(e, t),
      oncontextmenu: (e) => { if (this.state.readOnly) return; e.preventDefault(); this.menu(t, { x: e.clientX, y: e.clientY - 8 }); },
    }, t.name);
  }

  focus() { (this.list.querySelector('[aria-selected="true"]') || this.addBtn).focus(); }

  #key(e, t, i) {
    const { tabs, readOnly } = this.state;
    const go = (j) => { const el = this.list.children[(j + tabs.length) % tabs.length]; if (el) el.focus(); };
    if (e.key === 'ArrowRight') go(i + 1);
    else if (e.key === 'ArrowLeft') go(i - 1);
    else if (e.key === 'Home') go(0);
    else if (e.key === 'End') go(tabs.length - 1);
    else if (readOnly) return;
    else if (e.key === 'F2') this.rename(t.id);
    else if (e.key === 'Delete') this.askDelete(t.id);
    else if (e.key === 'ContextMenu' || (e.key === 'F10' && e.shiftKey)) { const r = e.currentTarget.getBoundingClientRect(); this.menu(t, { x: r.left, y: r.top - 8 }); }
    else return;
    e.preventDefault();
  }

  // menu lists what can be done with a tab. Moving left and right is the
  // keyboard path for reordering.
  menu(t, at) {
    const { tabs } = this.state;
    const i = tabs.findIndex((x) => x.id === t.id);
    const base = this.cb.seq();
    this.cb.menu(`Tab ${t.name}`, [
      { label: 'Rename…', kbd: 'F2', fn: () => this.rename(t.id, base) },
      { label: 'Move left', fn: () => this.cb.move(t.id, tabs[i - 1].id, base), disabled: i === 0 },
      { label: 'Move right', fn: () => this.cb.move(t.id, tabs[i + 2] ? tabs[i + 2].id : null, base), disabled: i === tabs.length - 1 },
      'hr',
      { label: 'Delete…', kbd: 'Del', danger: true, fn: () => this.askDelete(t.id, base), disabled: tabs.length < 2 },
    ], at);
  }

  // ---- rename in place ----

  rename(id, base = this.cb.seq()) {
    this.renaming = id;
    this.renameBase = base;
    this.render();
    const input = this.list.querySelector('input');
    input.focus();
    input.select();
  }

  #renameField(t) {
    // maxlength counts UTF-16 units, and a code point takes one or two, so
    // the field holds every name of MAX_TAB_NAME code points; a longer one
    // gets the message of tabNameError.
    const input = h('input', { type: 'text', 'aria-label': `New name for tab ${t.name}`, maxlength: String(2 * MAX_TAB_NAME) });
    input.value = t.name;
    let done = false;
    this.endRename = () => { done = true; };
    const finish = (save) => {
      if (done) return;
      done = true;
      this.renaming = null;
      const name = input.value;
      if (save && name !== t.name) {
        const bad = tabNameError(name, this.state.tabs, t.id);
        if (bad) this.cb.say(`The name was not changed. ${bad}`, name);
        else this.cb.rename(t.id, name, this.renameBase);
      }
      this.render();
      this.cb.focusGrid();
    };
    input.addEventListener('keydown', (e) => {
      if (e.isComposing || e.keyCode === 229) return;
      e.stopPropagation();
      if (e.key === 'Enter') { e.preventDefault(); finish(true); }
      else if (e.key === 'Escape') { e.preventDefault(); finish(false); }
    });
    input.addEventListener('blur', () => finish(true));
    return h('span', { class: 'tab', role: 'none' }, input);
  }

  // ---- delete with confirmation (rule 2) ----

  askDelete(id, base = this.cb.seq()) {
    const { tabs } = this.state;
    const t = tabs.find((x) => x.id === id);
    if (tabs.length < 2) { this.cb.say('A sheet keeps at least one tab, so the last one cannot be deleted.'); return; }
    if (!this.cb.needsConfirm(id)) { this.cb.remove(id, base); return; }
    const close = (go) => {
      this.closeConfirm = null;
      this.confirmEl.hidden = true;
      replace(this.confirmEl);
      if (go) this.cb.remove(id, base);
      this.cb.focusGrid();
    };
    const cancel = button('Cancel', () => close(false), { class: 'btn' });
    replace(this.confirmEl, h('span', { text: `Delete tab “${t.name}” with its cells?` }), button('Delete', () => close(true), { class: 'btn danger' }), cancel);
    this.confirmEl.hidden = false;
    this.closeConfirm = close;
    cancel.focus();
  }

  // ---- reorder by dragging ----

  #press(e, t) {
    if (e.button || this.state.readOnly) return;
    const x0 = e.clientX;
    const base = this.cb.seq();
    const move = (ev) => {
      if (!this.drag && Math.abs(ev.clientX - x0) < DRAG_START) return;
      this.drag = { id: t.id, before: this.#dropBefore(ev.clientX) };
      this.#markDrop();
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      const d = this.drag;
      this.drag = null;
      this.#markDrop();
      if (!d) return;
      const tabs = this.state.tabs;
      const i = tabs.findIndex((x) => x.id === d.id);
      const next = tabs[i + 1] ? tabs[i + 1].id : null;
      if (d.before !== d.id && d.before !== next) this.cb.move(d.id, d.before, base);
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  }

  // #dropBefore is the id of the tab a drop at x lands before, or null for
  // the end.
  #dropBefore(x) {
    for (const el of this.list.children) {
      // A tab that is being renamed is no button and names no place.
      if (!el.dataset?.tab) continue;
      const r = el.getBoundingClientRect();
      if (x < r.left + r.width / 2) return Number(el.dataset.tab);
    }
    return null;
  }

  #markDrop() {
    const d = this.drag;
    const kids = [...this.list.children];
    kids.forEach((el, i) => {
      const id = Number(el.dataset.tab);
      el.classList.toggle('dragging', !!d && id === d.id);
      el.classList.toggle('drop-before', !!d && d.before === id);
      el.classList.toggle('drop-after', !!d && d.before == null && i === kids.length - 1);
    });
  }
}
