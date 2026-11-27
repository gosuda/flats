// Keyboard movement in the grid (rule 21, TECH 19.5). gridKey maps a key
// press outside an edit to a command; the app carries it out. edgeJump
// finds where Ctrl or Cmd with an arrow lands: the edge of the data.
'use strict';

// gridKey returns the command for a key event, or null when the key belongs
// to the browser or starts typing.
export function gridKey(e) {
  if (e.isComposing || e.keyCode === 229) return null;
  const mod = e.metaKey || e.ctrlKey;
  const k = e.key;
  const lk = k.length === 1 ? k.toLowerCase() : k;
  const extend = !!e.shiftKey;
  switch (k) {
    case 'ArrowDown': return { type: 'move', dr: 1, dc: 0, extend, jump: mod };
    case 'ArrowUp': return { type: 'move', dr: -1, dc: 0, extend, jump: mod };
    case 'ArrowLeft': return { type: 'move', dr: 0, dc: -1, extend, jump: mod };
    case 'ArrowRight': return { type: 'move', dr: 0, dc: 1, extend, jump: mod };
    case 'Tab': return mod || e.altKey ? null : { type: 'move', dr: 0, dc: extend ? -1 : 1, extend: false, jump: false };
    case 'PageDown': return { type: 'page', dir: 1, extend };
    case 'PageUp': return { type: 'page', dir: -1, extend };
    case 'Home': return { type: 'home', all: mod, extend };
    case 'End': return { type: 'end', all: mod, extend };
    case 'Enter': return e.altKey ? null : { type: 'edit' };
    case 'F2': return { type: 'edit' };
    case 'Delete': case 'Backspace': return { type: 'clear' };
    case 'ContextMenu': return { type: 'menu' };
    case 'F10': return e.shiftKey ? { type: 'menu' } : null;
  }
  if (k === ' ' && e.ctrlKey) return { type: 'select', what: 'cols' };
  if (k === ' ' && e.shiftKey) return { type: 'select', what: 'rows' };
  if (!mod) return null;
  switch (lk) {
    case 'z': return { type: e.shiftKey ? 'redo' : 'undo' };
    case 'y': return { type: 'redo' };
    case 'b': return { type: 'mark', mark: 'bold' };
    case 'i': return { type: 'mark', mark: 'italic' };
    case 'u': return { type: 'mark', mark: 'underline' };
    case 'd': return { type: 'fill', dir: 'down' };
    case 'r': return { type: 'fill', dir: 'right' };
    case 'a': return { type: 'select', what: 'all' };
    case 'v': return e.shiftKey ? { type: 'pasteValues' } : null;
  }
  return null;
}

// edgeJump moves from (r, c) in direction (dr, dc) to the edge of the data:
// inside a filled run it goes to the run's last cell, otherwise to the next
// filled cell, and to the edge of the tab when there is none.
export function edgeJump(filled, rows, cols, r, c, dr, dc) {
  const inside = (y, x) => y >= 0 && y < rows && x >= 0 && x < cols;
  let y = r + dr;
  let x = c + dc;
  if (!inside(y, x)) return { r, c };
  if (filled(r, c) && filled(y, x)) {
    while (inside(y + dr, x + dc) && filled(y + dr, x + dc)) { y += dr; x += dc; }
    return { r: y, c: x };
  }
  while (inside(y, x) && !filled(y, x)) { y += dr; x += dc; }
  if (!inside(y, x)) { y -= dr; x -= dc; }
  return { r: y, c: x };
}
