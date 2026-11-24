// Find (rule 20a, TECH 19). It runs in the client over what the cells of
// the open tab show, capitals ignored by toLowerCase on both sides, so it
// works the same on a Public route. It counts the matches, which are in
// reading order. A change of the text selects the first match at or after
// the anchor, and Next and Previous step from the active cell.
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
  return out.sort(order);
}

// order compares two positions in reading order.
const order = (a, b) => a.r - b.r || a.c - b.c;

// Find owns the find field. cb: model() and selection() on screen,
// settle() ends an open edit and returns false when the selection cannot
// move now, go(r, c), marks(set), closed().
export class Find {
  constructor(cb) {
    this.cb = cb;
    this.matches = [];
    // The anchor is the active cell when the field opened, by ids and by
    // the position it had. It follows the moves of the selection that the
    // person makes: Find keeps the selection's move count from its own
    // last move, so any other move shows as a change of that count.
    this.anchor = null; // {row, col, r, c}
    this.seen = null;   // {sel: a WeakRef to the selection, moved}
    // spoken is the count's text when it was last spoken, and spokenModel and
    // spokenTab are the model and the tab it was spoken of. A paint or a
    // refresh speaks the count only when that text has changed for the same
    // tab of the same sheet. spokenModel and seen.sel are WeakRefs: Find only
    // compares them, so a closed revision's model is not kept by Find.
    this.spoken = null;
    this.spokenModel = null;
    this.spokenTab = null;
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

  // show opens the field, or focuses it when it is open. Opening sets the
  // anchor and moves nothing: the text the field still holds is counted,
  // marked and spoken.
  show() {
    const opening = !this.open;
    this.el.hidden = false;
    this.input.focus();
    this.input.select();
    if (opening) this.#anchorHere();
    if (this.input.value) { this.#find(); this.#say(true); }
  }

  close() {
    if (!this.open) return;
    this.el.hidden = true;
    this.matches = [];
    this.anchor = null;
    this.seen = null;
    this.cb.marks(null);
    this.cb.closed();
  }

  // search finds the matches again: after the tab's cells changed, and
  // with jump after a change of the text, which selects the first match at
  // or after the anchor, or else the first match of the tab. With no match
  // the selection stays. A change of the cells speaks the count only when
  // its text changed.
  search(jump = false) {
    if (!this.open) return;
    if (!jump) { this.#find(); this.#say(false); return; }
    this.#move(() => {
      const p = this.#anchorAt();
      return this.matches.find((m) => order(m, p) >= 0) ?? this.matches[0];
    });
  }

  // step goes to the first match after the active cell, or with dir -1 to
  // the last match before it, round the ends.
  step(dir) {
    this.#move(() => {
      const a = this.cb.selection().active();
      return dir > 0
        ? this.matches.find((m) => order(m, a) > 0) ?? this.matches[0]
        : this.matches.findLast((m) => order(m, a) < 0) ?? this.matches.at(-1);
    });
  }

  // #move selects the match pick chooses. Settling an open edit can apply
  // changes it held back, which move the cells, so the anchor, the active
  // cell and the matches are read after it. A count names only a match the
  // selection is on.
  #move(pick) {
    const moved = this.cb.settle() !== false;
    this.#follow();
    this.#find();
    const m = moved && this.matches.length ? pick() : null;
    if (m) {
      this.cb.go(m.r, m.c);
      this.#see(this.cb.selection());
    }
    this.#say(true);
  }

  // #follow moves the anchor to the active cell when the selection moved
  // since Find last saw it, which only the person or another tab does.
  #follow() {
    const sel = this.cb.selection();
    if (!this.seen || this.seen.sel.deref() !== sel || this.seen.moved !== sel.moved) this.#anchorHere();
  }

  // #see keeps the selection Find has seen as a WeakRef. Find only compares it
  // with the selection on screen, which the app keeps; a selection of a closed
  // revision is then not kept by Find.
  #see(sel) {
    this.seen = { sel: new WeakRef(sel), moved: sel.moved };
  }

  #anchorHere() {
    const sel = this.cb.selection();
    const model = this.cb.model();
    const a = sel.active();
    this.anchor = model.tab ? { row: model.rowAt(a.r), col: model.colAt(a.c), r: a.r, c: a.c } : null;
    this.#see(sel);
  }

  // #anchorAt is where the anchor is now: where its row and column are, or
  // where they were when one was deleted.
  #anchorAt() {
    const p = this.anchor;
    if (!p) return { r: 0, c: 0 };
    const model = this.cb.model();
    const r = model.rowIndex(p.row);
    const c = model.colIndex(p.col);
    return { r: r < 0 ? p.r : r, c: c < 0 ? p.c : c };
  }

  // #find lists the matches and marks them by ids.
  #find() {
    const model = this.cb.model();
    this.matches = findMatches(model, this.input.value);
    const marks = new Set(this.matches.map((m) => `${model.rowAt(m.r)}:${model.colAt(m.c)}`));
    this.cb.marks(marks.size ? marks : null);
  }

  // #at is the index of the match the active cell is on, or -1.
  #at() {
    if (!this.matches.length) return -1;
    const a = this.cb.selection().active();
    return this.matches.findIndex((m) => m.r === a.r && m.c === a.c);
  }

  // sync shows the count for where the selection is now. The grid calls it as
  // it draws, so a click or a key in the grid that moves the selection, which
  // Find does not make, is counted at once and spoken when its text changed.
  // A paint that changes nothing speaks nothing.
  sync() {
    if (this.open) this.#say(false);
  }

  // #say shows the count of the tab on screen and speaks it in the find
  // region. While that tab loads its matches are not the tab's, so the count
  // shows nothing and nothing is spoken; the count is spoken when the tab
  // ends. Otherwise the text is spoken at once after one of Find's own
  // actions (force), and else only when it has changed since it was last
  // spoken of the same tab of the same sheet. A view of a revision is
  // another model, so its count is spoken as a change of tab.
  #say(force) {
    const model = this.cb.model();
    const tab = model.tab;
    if (model !== this.spokenModel?.deref() || tab?.id !== this.spokenTab) this.spoken = null;
    if (!tab || tab.loading) {
      // An action of Find during a load is spoken when the load ends, even if
      // its text is the text spoken last.
      if (tab && force) this.spoken = null;
      this.count.textContent = '';
      return;
    }
    const n = this.matches.length;
    const at = this.#at();
    const all = plural(n, 'match', 'matches');
    const text = !this.input.value ? '' : !n ? 'No matches' : at < 0 ? all : `${at + 1} of ${n}`;
    this.count.textContent = text;
    if (!text) { this.spoken = null; return; }
    if (!force && text === this.spoken) return;
    this.spoken = text;
    this.spokenModel = new WeakRef(model);
    this.spokenTab = tab.id;
    announce(!n ? 'No matches' : at < 0 ? all : `Match ${at + 1} of ${all}`, '#announce-find');
  }
}
