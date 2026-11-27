// Announcements, cell labels and the F6 route (rule 20b, TECH 19.4).
'use strict';

import { address } from './protocol.js';

// announce speaks text through a polite live region without moving focus.
// Save status and notices use #announce; each move in the grid uses
// #announce-cell so a status never cuts a cell short, and the find count
// uses #announce-find for the same reason.
export function announce(text, region = '#announce') {
  const live = document.querySelector(region);
  if (!live) return;
  live.textContent = '';
  const set = () => { live.textContent = text; };
  if (globalThis.requestAnimationFrame) requestAnimationFrame(set); else set();
}

// cellLabel is what a screen reader hears on a cell: its address, what it
// shows, "formula" or "error" when they apply, and "selected" when the
// cell is part of a range.
export function cellLabel({ r, c, shown, formula, error, pending, selected, editing }) {
  const parts = [address(r, c), shown === '' ? 'blank' : shown];
  if (formula) parts.push('formula');
  if (error) parts.push('error');
  if (pending) parts.push('not saved yet');
  if (selected) parts.push('selected');
  if (editing) parts.push(`edited by ${editing}`);
  return parts.join(', ');
}

export const GRID_KEYS = 'Arrow keys, Tab and Shift+Tab move between cells. Enter or F2 edits. Shift+F10 opens the menu. F6 and Shift+F6 move between top bar, toolbar, formula bar, grid and tabs; Escape returns to the grid.';

// Regions of the F6 route, in order. Each entry names the region and finds
// the element that takes focus, or null when the region is not shown.
export function nextRegion(regions, active, dir) {
  let i = regions.findIndex((r) => r.root && active && r.root.contains(active));
  if (i < 0) i = dir > 0 ? -1 : 0;
  for (let k = 0; k < regions.length; k++) {
    i = (i + dir + regions.length) % regions.length;
    const el = regions[i].target();
    if (el) return { name: regions[i].name, el };
  }
  return null;
}
