// Virtualized row lists (TECH 18). Only rows in the viewport plus overscan
// are in the DOM, with spacers for the rest. The focused row stays rendered
// wherever it is, so an open editor or keyboard focus survives scrolling and
// reordering. Row heights are measured once rendered and estimated before.
'use strict';


export const ROW_ESTIMATE = 36;
export const OVERSCAN = 8;

// windowFor returns the half-open index range [start, end) to render.
export function windowFor(offsets, scrollTop, viewport, overscan = OVERSCAN) {
  const n = offsets.length - 1;
  if (n <= 0) return { start: 0, end: 0 };
  let lo = 0;
  let hi = n;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (offsets[mid + 1] <= scrollTop) lo = mid + 1;
    else hi = mid;
  }
  let end = lo;
  while (end < n && offsets[end] < scrollTop + viewport) end++;
  return { start: Math.max(0, lo - overscan), end: Math.min(n, Math.max(end, lo + 1) + overscan) };
}

// createVirtualRows renders rows into body (a tbody or list element) inside
// the scrolling element scroller. renderRow(id, index) returns a fresh row
// element; the list sets its aria-rowindex (header rows come first).
export function createVirtualRows({ scroller, body, renderRow, spacer, headerRows = 1, estimate = ROW_ESTIMATE, overscan = OVERSCAN, onCount }) {
  let ids = [];
  let index = new Map();
  let offsets = [0];
  const heights = new Map();
  const rendered = new Map();
  let focusedId = null;
  const top = spacer();
  const bottom = spacer();
  top.setAttribute('aria-hidden', 'true');
  bottom.setAttribute('aria-hidden', 'true');

  const heightOf = (id) => heights.get(id) ?? estimate;
  const relayout = () => {
    offsets = new Array(ids.length + 1);
    offsets[0] = 0;
    for (let i = 0; i < ids.length; i++) offsets[i + 1] = offsets[i] + heightOf(ids[i]);
  };

  function paint() {
    const viewport = scroller.clientHeight || estimate * 20;
    const { start, end } = windowFor(offsets, scroller.scrollTop || 0, viewport, overscan);
    const want = [];
    for (let i = start; i < end; i++) want.push(i);
    const f = focusedId != null ? index.get(focusedId) : undefined;
    if (f !== undefined && (f < start || f >= end)) want.push(f);
    want.sort((a, b) => a - b);
    const keep = new Set(want.map((i) => ids[i]));
    for (const [id, el] of rendered) {
      if (!keep.has(id)) {
        el.remove();
        rendered.delete(id);
      }
    }
    const kids = [top];
    let prevEnd = 0;
    for (const i of want) {
      const id = ids[i];
      let el = rendered.get(id);
      if (!el) {
        el = renderRow(id, i);
        el.dataset.row = id;
        rendered.set(id, el);
      }
      el.setAttribute('aria-rowindex', String(i + 1 + headerRows));
      // A pinned row far from the window needs its own gap above it.
      if (kids.length > 1 && i > prevEnd) kids.push(gap(offsets[i] - offsets[prevEnd]));
      if (kids.length === 1) top.style.height = `${offsets[i]}px`;
      kids.push(el);
      prevEnd = i + 1;
    }
    if (kids.length === 1) top.style.height = `${offsets[ids.length] ?? 0}px`;
    bottom.style.height = `${Math.max(0, offsets[ids.length] - offsets[prevEnd])}px`;
    kids.push(bottom);
    reconcile(kids);
    measure();
  }

  // reconcile puts body's children in kids order without moving the row
  // that holds focus: moving a focused element blurs it, and a blurred cell
  // editor commits.
  function reconcile(kids) {
    const want = new Set(kids);
    for (const c of [...body.children]) if (!want.has(c)) c.remove();
    const active = document.activeElement;
    const pivot = kids.find((k) => k.parentNode === body && active && k.contains(active));
    if (pivot) {
      const at = kids.indexOf(pivot);
      for (let i = 0; i < at; i++) body.insertBefore(kids[i], pivot);
      let prev = pivot;
      for (let i = at + 1; i < kids.length; i++) {
        if (prev.nextSibling !== kids[i]) body.insertBefore(kids[i], prev.nextSibling);
        prev = kids[i];
      }
      return;
    }
    let cursor = body.firstChild;
    for (const k of kids) {
      if (k === cursor) {
        cursor = cursor.nextSibling;
        continue;
      }
      body.insertBefore(k, cursor);
    }
  }

  function gap(px) {
    const el = spacer();
    el.setAttribute('aria-hidden', 'true');
    el.style.height = `${px}px`;
    return el;
  }

  function measure() {
    let changed = false;
    for (const [id, el] of rendered) {
      const hgt = el.offsetHeight;
      if (hgt > 0 && hgt !== heights.get(id)) {
        heights.set(id, hgt);
        changed = true;
      }
    }
    if (changed) relayout();
  }

  // setRows replaces the order. The first visible row keeps its place on
  // screen, unless the focused row is visible, which then stays put instead.
  function setRows(next, changed, keepFocused = true) {
    const scrollTop = scroller.scrollTop || 0;
    let anchor = focusedId != null && rendered.has(focusedId) ? focusedId : null;
    if (anchor == null && ids.length) anchor = ids[windowFor(offsets, scrollTop, 1, 0).start];
    const delta = anchor != null && index.has(anchor) ? offsets[index.get(anchor)] - scrollTop : 0;
    // Removing a focused row can blur its editor and synchronously repaint.
    ids = [...next];
    index = new Map(ids.map((id, i) => [id, i]));
    relayout();
    for (const [id, el] of [...rendered]) {
      if (index.has(id)) continue;
      rendered.delete(id);
      el.remove();
    }
    if (changed !== undefined) invalidate(changed, keepFocused);
    if (anchor != null && index.has(anchor)) scroller.scrollTop = Math.max(0, offsets[index.get(anchor)] - delta);
    onCount?.(ids.length);
    paint();
  }

  // reveal materializes row i and scrolls just enough to show it.
  function reveal(i) {
    if (i < 0 || i >= ids.length) return null;
    const viewport = scroller.clientHeight || estimate * 20;
    const st = scroller.scrollTop || 0;
    if (offsets[i] < st) scroller.scrollTop = offsets[i];
    else if (offsets[i + 1] > st + viewport) scroller.scrollTop = offsets[i + 1] - viewport;
    focusedId = ids[i];
    paint();
    return rendered.get(ids[i]);
  }

  function invalidate(only, keepFocused) {
    for (const [id, el] of [...rendered]) {
      if (only && !only.has(id)) continue;
      if (keepFocused && id === focusedId && el.contains(document.activeElement)) continue;
      rendered.delete(id);
      el.remove();
    }
  }

  // rerender replaces the elements of the given rows, or of every rendered
  // row, keeping the focused row's element when keepFocused is set.
  function rerender(only, keepFocused = true) {
    invalidate(only, keepFocused);
    paint();
  }

  scroller.addEventListener('scroll', () => paint());

  return {
    setRows,
    reveal,
    rerender,
    paint,
    get ids() { return ids; },
    indexOf: (id) => index.get(id) ?? -1,
    element: (id) => rendered.get(id) || null,
    rendered: () => [...rendered.keys()],
    get focused() { return focusedId; },
    set focused(id) { focusedId = id; },
  };
}
