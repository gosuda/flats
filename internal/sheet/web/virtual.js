// Virtualization math (TECH 19.3). Row heights live in a Fenwick tree over
// row positions, so the offset of any of 100,000 rows and the row at any
// scroll offset cost a logarithm even when wrapped rows are taller than a
// line. Columns are few enough for a plain prefix array. Nothing here
// touches the DOM.
'use strict';

export const ROW_HEIGHT = 28;  // one line
export const HEAD_HEIGHT = 28; // column letters
export const HEAD_WIDTH = 46;  // row numbers
export const OVERSCAN_ROWS = 10;
export const OVERSCAN_COLS = 3;

// Heights keeps one height per row position.
export class Heights {
  constructor(n = 0, height = ROW_HEIGHT) { this.reset(n, height); }

  // reset makes n rows of the default height, then applies at(i) where it
  // returns a height.
  reset(n, height = ROW_HEIGHT, at = null) {
    this.n = n;
    this.base = height;
    this.h = new Float64Array(n).fill(height);
    if (at) for (let i = 0; i < n; i++) { const v = at(i); if (v) this.h[i] = v; }
    this.tree = new Float64Array(n + 1);
    for (let i = 1; i <= n; i++) {
      this.tree[i] += this.h[i - 1];
      const up = i + (i & -i);
      if (up <= n) this.tree[up] += this.tree[i];
    }
    this.log = 1;
    while (this.log * 2 <= n) this.log *= 2;
  }

  get(i) { return this.h[i]; }

  set(i, height) {
    const d = height - this.h[i];
    if (!d) return false;
    this.h[i] = height;
    for (let j = i + 1; j <= this.n; j += j & -j) this.tree[j] += d;
    return true;
  }

  // offset is the top of row i: the sum of the heights before it.
  offset(i) {
    let s = 0;
    for (let j = Math.min(i, this.n); j > 0; j -= j & -j) s += this.tree[j];
    return s;
  }

  get total() { return this.offset(this.n); }

  // indexAt is the row that covers offset y, clamped to the last row.
  indexAt(y) {
    let pos = 0;
    let rest = y;
    for (let step = this.log; step > 0; step >>= 1) {
      const next = pos + step;
      if (next <= this.n && this.tree[next] <= rest) { pos = next; rest -= this.tree[next]; }
    }
    return Math.min(pos, Math.max(0, this.n - 1));
  }
}

// Offsets keeps the left edge of every column for a list of widths.
export class Offsets {
  constructor(widths = []) { this.set(widths); }

  set(widths) {
    this.n = widths.length;
    this.at = new Float64Array(this.n + 1);
    for (let i = 0; i < this.n; i++) this.at[i + 1] = this.at[i] + widths[i];
  }

  offset(i) { return this.at[Math.min(i, this.n)]; }
  size(i) { return this.at[i + 1] - this.at[i]; }
  get total() { return this.at[this.n]; }

  indexAt(x) {
    let lo = 0;
    let hi = this.n - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (this.at[mid] <= x) lo = mid;
      else hi = mid - 1;
    }
    return Math.max(0, lo);
  }
}

// windowOf returns the half-open range [start, end) of rows or columns to
// draw on one axis. axis has offset(i) and indexAt(x); frozen items lie
// before `frozen` and are always drawn by the caller, so the window starts
// at or after them. scroll is the scroll offset and view the size of the
// scrolling part of the viewport, under or beside the frozen pane.
export function windowOf(axis, count, frozen, scroll, view, overscan) {
  if (count <= frozen) return { start: frozen, end: frozen };
  const origin = axis.offset(frozen);
  const first = Math.max(frozen, axis.indexAt(origin + scroll));
  const last = Math.max(first, axis.indexAt(origin + scroll + Math.max(0, view - 1)));
  return { start: Math.max(frozen, first - overscan), end: Math.min(count, last + 1 + overscan) };
}

// revealScroll returns the scroll offset that brings item i into view on
// one axis, changing it only as far as needed.
export function revealScroll(axis, i, frozen, scroll, view) {
  if (i < frozen) return scroll;
  const origin = axis.offset(frozen);
  const start = axis.offset(i) - origin;
  const end = axis.offset(i + 1) - origin;
  if (start < scroll) return start;
  if (end > scroll + view) return Math.max(start - Math.max(0, view - (end - start)), 0);
  return scroll;
}
