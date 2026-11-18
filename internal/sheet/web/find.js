// Find (rule 20a). It runs in the client over what the cells of the open
// tab show, capitals ignored, so it works the same on a Public route. It
// counts the matches and moves the selection to the next or previous one.
'use strict';

import { announce } from './a11y.js';
import { button, h, icon, plural } from './dom.js';

// findMatches lists the cells whose shown text holds query, row by row.
export function findMatches(model, query) {
  const q = query.toLowerCase();
  const out = [];
  if (!q || !model.tab) return out;
  model.eachCell({ r1: 0, c1: 0, r2: model.rowCount - 1, c2: model.colCount - 1 }, (r, c, cell) => {
    if (cell.shown.toLowerCase().includes(q)) out.push({ r, c });
  });
  return out.sort((a, b) => a.r - b.r || a.c - b.c);
}

// Find owns the find field. cb: model(), settle() ends an open edit and
// returns false when the selection cannot move now, go(r, c), marks(set),
// closed().
export class Find {
  constructor(cb) {
    this.cb = cb;
    this.matches = [];
    this.at = -1;
    this.cur = null; // {row, col} ids of the match the selection is on
    this.input = h('input', { type: 'text', 'aria-label': 'Find in this tab', placeholder: 'Find in this tab', autocomplete: 'off', spellcheck: 'false' });
    this.count = h('span', { class: 'find-count', 'aria-hidden': 'true' });
    this.el = h('div', { class: 'pop find', role: 'dialog', 'aria-label': 'Find', hidden: true },
      this.input, this.count,
      button('Previous', () => this.step(-1), { class: 'btn', 'aria-label': 'Previous match (Shift+Enter)' }, icon('up')),
      button('Next', () => this.step(1), { class: 'btn', 'aria-label': 'Next match (Enter)' }, icon('down')),
      button('Close', () => this.close(), { class: 'btn', 'aria-label': 'Close find (Escape)' }, icon('close')));
    this.input.addEventListener('input', () => this.search(true));
    this.el.addEventListener('keydown', (e) => {
      if (e.isComposing || e.keyCode === 229) return;
      if (e.key === 'Enter') { e.preventDefault(); this.step(e.shiftKey ? -1 : 1); }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); this.close(); }
    });
  }

  get open() { return !this.el.hidden; }

  show() {
    this.el.hidden = false;
    this.input.focus();
    this.input.select();
    if (this.input.value) this.search(true);
  }

  close() {
    if (!this.open) return;
    this.el.hidden = true;
    this.matches = [];
    this.at = -1;
    this.cur = null;
    this.cb.marks(null);
    this.cb.closed();
  }

  // search finds the matches again: after typing, and after the tab's
  // cells changed. With jump it moves to the first match.
  search(jump = false) {
    if (!this.open) return;
    if (jump) { this.at = -1; this.cur = null; this.step(1); return; }
    this.#find();
    this.#say();
  }

  step(dir) {
    // Settling an open edit can apply changes it held back, which move the
    // cells, so the matches are found after it.
    const moved = this.cb.settle() !== false;
    this.#find();
    const n = this.matches.length;
    // A count names only a match the selection is on.
    if (!n || !moved) { this.#say(); return; }
    const at = this.at < 0 ? (dir > 0 ? 0 : n - 1) : (this.at + dir + n) % n;
    const m = this.matches[at];
    const model = this.cb.model();
    this.cb.go(m.r, m.c);
    this.at = at;
    this.cur = { row: model.rowAt(m.r), col: model.colAt(m.c) };
    this.#say();
  }

  // #find lists the matches and marks them. The count stays on the match
  // the selection is on, by its ids, wherever the cell moved.
  #find() {
    const model = this.cb.model();
    this.matches = findMatches(model, this.input.value);
    const marks = new Set(this.matches.map((m) => `${model.rowAt(m.r)}:${model.colAt(m.c)}`));
    this.cb.marks(marks.size ? marks : null);
    const cur = this.cur;
    this.at = cur ? this.matches.findIndex((m) => model.rowAt(m.r) === cur.row && model.colAt(m.c) === cur.col) : -1;
    if (this.at < 0) this.cur = null;
  }

  #say() {
    const n = this.matches.length;
    const all = plural(n, 'match', 'matches');
    const text = !this.input.value ? '' : !n ? 'No matches' : this.at < 0 ? all : `${this.at + 1} of ${n}`;
    this.count.textContent = text;
    if (this.input.value) announce(!n ? 'No matches' : this.at < 0 ? all : `Match ${this.at + 1} of ${all}`);
  }
}
