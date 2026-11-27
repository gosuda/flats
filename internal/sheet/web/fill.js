// Fill (rule 21): dragging the handle at the selection's corner, or Ctrl+D
// and Ctrl+R, copies the first cells over the rest. The client names the
// source and the target; the server repeats the source and shifts formulas.
'use strict';

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

// fillOp names both ranges by ids.
export const fillOp = (tab, source, target) => ops.fill(tab.id, idsOf(tab, source), idsOf(tab, target));

// union is the selection after a fill: the source and the target together.
export const union = (a, b) => ({ r1: Math.min(a.r1, b.r1), c1: Math.min(a.c1, b.c1), r2: Math.max(a.r2, b.r2), c2: Math.max(a.c2, b.c2) });
