// Fill (rules 21 and 21a, TECH 19). Dragging the handle at the selection's
// corner fills the cells it passes with mode auto: the server continues a
// series where rule 21a finds one and repeats the source elsewhere. Ctrl+D
// and Ctrl+R copy the first row or column over the rest. The client names
// the source and the target by ids; the server reads the cells.
'use strict';

import { announce } from './a11y.js';
import { h, icon } from './dom.js';
import { ops, packIds } from './protocol.js';

// dragTarget is the range a fill drag adds when the pointer is over cell
// (r, c): the cells beyond the selection along the axis the pointer left
// it by more. It is null while the pointer is inside the selection.
export function dragTarget(sel, r, c) {
  const dr = r < sel.r1 ? r - sel.r1 : r > sel.r2 ? r - sel.r2 : 0;
  const dc = c < sel.c1 ? c - sel.c1 : c > sel.c2 ? c - sel.c2 : 0;
  if (!dr && !dc) return null;
  if (Math.abs(dr) >= Math.abs(dc)) {
    return dr > 0 ? { r1: sel.r2 + 1, r2: r, c1: sel.c1, c2: sel.c2 } : { r1: r, r2: sel.r1 - 1, c1: sel.c1, c2: sel.c2 };
  }
  return dc > 0 ? { r1: sel.r1, r2: sel.r2, c1: sel.c2 + 1, c2: c } : { r1: sel.r1, r2: sel.r2, c1: c, c2: sel.c1 - 1 };
}

// keyFill is the source and the target of Ctrl+D (down) and Ctrl+R (right):
// the first row or column of the selection fills the rest of it. A
// selection one line deep is filled from the line before it.
export function keyFill(sel, dir) {
  if (dir === 'down') {
    if (sel.r2 > sel.r1) return { source: { ...sel, r2: sel.r1 }, target: { ...sel, r1: sel.r1 + 1 } };
    return sel.r1 > 0 ? { source: { ...sel, r1: sel.r1 - 1, r2: sel.r1 - 1 }, target: sel } : null;
  }
  if (sel.c2 > sel.c1) return { source: { ...sel, c2: sel.c1 }, target: { ...sel, c1: sel.c1 + 1 } };
  return sel.c1 > 0 ? { source: { ...sel, c1: sel.c1 - 1, c2: sel.c1 - 1 }, target: sel } : null;
}

const idsOf = (t, b) => ({ rows: packIds(t.rows.slice(b.r1, b.r2 + 1)), cols: packIds(t.cols.slice(b.c1, b.c2 + 1)) });

// fillOp names both ranges by ids. It names no mode, which is a copy.
export const fillOp = (tab, source, target) => ops.fill(tab.id, idsOf(tab, source), idsOf(tab, target));

// union is the selection after a fill: the source and the target together.
export const union = (a, b) => ({ r1: Math.min(a.r1, b.r1), c1: Math.min(a.c1, b.c1), r2: Math.max(a.r2, b.r2), c2: Math.max(a.c2, b.c2) });

// FillChoice is the button a fill drag leaves by the filled cells once its
// ack shows that it applied. It offers the same fill again, by the same
// ids, as a copy or as a series. The button closes on the next edit,
// selection change or scroll, and on Escape; the app tells it of each.
// cb: choose(button) offers the two choices, closed(focused) runs when a
// shown button closes, with whether it held focus.
export class FillChoice {
  constructor(cb) {
    this.cb = cb;
    // The drag fill whose ack is awaited, then the one the button offers:
    // {n, fill: {tab, source, target}, moved, top, left}, with the
    // selection's move count and the grid's scroll position after it.
    this.wait = null;
    this.cur = null;
    // An ack can arrive while its action is sent (App #act), before the
    // page expects it: n of such a fill that applied.
    this.early = null;
    this.btn = h('button', { type: 'button', class: 'btn icon', 'aria-haspopup': 'menu', 'aria-label': 'Fill options: copy cells or fill series', title: 'Fill options', onclick: () => this.cb.choose(this.btn) }, icon('more'));
    this.el = h('div', { class: 'fillopts', role: 'group', 'aria-label': 'Fill options', hidden: true }, this.btn);
    this.el.addEventListener('keydown', (e) => {
      if (e.isComposing || e.keyCode === 229 || e.key !== 'Escape') return;
      e.preventDefault();
      e.stopPropagation();
      this.close();
    });
  }

  get open() { return !!this.cur; }

  // fill is the fill the button is for, while its ack is awaited or it
  // shows.
  get fill() { return (this.cur || this.wait)?.fill ?? null; }

  // expect waits for the ack of drag fill n. mark is the selection's move
  // count and the grid's scroll position once the fill selected its cells.
  expect(n, fill, mark) {
    const early = this.early === n;
    this.close();
    this.wait = { n, fill, ...mark };
    if (early) this.#show();
  }

  // acked takes the ack of drag fill n.
  acked(n, applied) {
    if (this.wait?.n !== n) { this.early = applied ? n : null; return; }
    if (applied) this.#show(); else this.close();
  }

  #show() {
    this.cur = this.wait;
    this.wait = null;
    this.el.hidden = false;
    announce('Filled. F6 reaches the fill options: copy cells or fill series.');
  }

  // check closes the button, or forgets the fill it waits for, when the
  // selection moved or the grid scrolled since the fill.
  check(sel, top, left) {
    const w = this.cur || this.wait;
    if (w && (sel.moved !== w.moved || top !== w.top || left !== w.left)) this.close();
  }

  close() {
    this.wait = null;
    this.early = null;
    if (!this.cur) return;
    this.cur = null;
    const focused = this.el.contains(document.activeElement);
    this.el.hidden = true;
    this.cb.closed(focused);
  }

  // place puts the button at (x, y) of its positioned parent.
  place(x, y) {
    this.el.style.left = `${Math.round(x)}px`;
    this.el.style.top = `${Math.round(y)}px`;
  }
}
