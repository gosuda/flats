// Cell editing (rule 21, TECH 19.5 and 8.4): the cell's text area, the
// formula bar that mirrors it, composition input, reference insertion by
// click, function hints, and the exchange that keeps a formula in the editor
// bound to one screen state while structural changes arrive. The editor
// never reads a formula: it sends typed text, and the server adjusts
// references through `rebase`.
'use strict';

import { announce } from './a11y.js';
import { h, plural } from './dom.js';
import { HOLD_MAX, address, isId, isStructural, rebasedError, screenPosition, wirePosition } from './protocol.js';

const LINE = 19;
const HINT_MAX = 6;
const REBASE_TRIES = 3;
const REBASE_WAIT = 500;

// tokens splits text into pieces a caret can be counted in: quoted text,
// quoted tab names, runs of name characters, runs of spaces and single
// signs. It is a caret aid, not a formula reader.
export function tokens(text) {
  return text.match(/"(?:[^"]|"")*"?|'(?:[^']|'')*'?|[A-Za-z0-9_.$#?]+|\s+|[^]/g) || [];
}

// caretByToken is where a caret in before lies in after, the text an
// exchange made of it. With as many pieces in both texts the caret keeps
// its piece. At the end of one it stays at its end, as after a reference
// that grew, and inside one it keeps its distance from the start as far as
// the piece goes. Texts of another structure put the caret at the end.
export function caretByToken(before, caret, after) {
  const a = tokens(before);
  const b = tokens(after);
  if (a.length !== b.length) return after.length;
  let from = 0;
  let to = 0;
  for (let i = 0; i < a.length; i++) {
    const end = from + a[i].length;
    if (caret <= end) return to + (caret === end ? b[i].length : Math.min(caret - from, b[i].length));
    from = end;
    to += b[i].length;
  }
  return after.length;
}

// barText is what the formula bar shows for a cell while no edit is open:
// {row, col, text, content, peek}. content says the text is the cell's
// content as it would be typed, which an edit started in the bar goes on
// from. A cell that has arrived shows it even while the rest of its row is
// on its way, because a cell is never in two frames (TECH 12.2). The bar
// shows nothing for content that has not arrived, and for pending text
// typed on another screen, which an edit adjusts first. peek says the
// formula text is to be asked for.
export function barText(m, row, col) {
  const shown = (text, content, peek = false) => ({ row, col, text, content, peek });
  if (m.isPending(row, col)) return m.movedPending(row, col) ? shown('', false) : shown(m.shown(row, col), true);
  const cell = m.cell(row, col);
  if (cell?.formula) {
    const p = m.peeked(row, col);
    return p ? shown(p.edit, true) : shown('', false, true);
  }
  return cell ? shown(cell.edit, true) : shown('', m.known(row, col));
}

// Editor owns the open edit. cb: canWrite(), commit({row, col, text, at,
// move, focus}), closed({move, row, col, focus}), peek(row, col),
// reopen(), changed(), composed(), refused(text, why) for the text of a bar
// edit made of text that is no longer the cell's ('changed') or that is of
// the cell the selection left ('moved').
export class Editor {
  constructor({ grid, model, selection, bar, host, functions = [], cb }) {
    this.grid = grid;
    this.model = model;
    this.selection = selection;
    this.bar = bar;
    this.host = host;           // element the function hints are added to
    this.functions = functions; // [{name, group, args}]
    this.cb = cb;
    // waiting is true until the text arrives: the cell's row of a snapshot
    // still arriving (rows), the formula text by a peek, or the pending
    // text in reopen as a rebase adjusts it to the screen.
    // from is the text the formula bar showed when the edit started there,
    // or null: the text in hand then holds it, so it is not added again
    // when the content arrives. pointed is the reference the last click
    // inserted and the place right after it, {ref, end}, or null.
    this.ed = null;             // {row, col, orig, source, pointed, hint, waiting, rows, reopen, from}
    // idle is what the idle formula bar holds, from barText, or null
    // when that is not known.
    this.idle = null;
    this.composing = false;
    this.hintEl = null;
    for (const ta of [grid.input, bar]) {
      ta.addEventListener('input', () => this.#input(ta));
      // A composition in the idle formula bar joins the content it shows,
      // as typed keys there do.
      ta.addEventListener('compositionstart', () => {
        this.composing = true;
        if (this.ed) return;
        if (ta === bar) this.fromBar(); else this.start({ replace: true, text: '', source: 'cell' });
      });
      ta.addEventListener('compositionend', () => { this.composing = false; this.#input(ta); this.cb.composed(); });
      ta.addEventListener('click', () => { if (this.ed) this.#hint(); });
    }
    bar.addEventListener('focus', () => { if (this.ed) this.ed.source = 'bar'; });
    grid.input.addEventListener('focus', () => { if (this.ed && !this.ed.waiting) this.ed.source = 'cell'; });
  }

  get open() { return !!this.ed && !this.ed.waiting; }
  get area() { return this.ed?.source === 'bar' ? this.bar : this.grid.input; }
  get text() { return this.area.value; }
  get isFormula() { return this.open && this.text.startsWith('='); }
  // bound says whether the editor holds text read against the screen as it
  // is, so the screen must not change under it. That is a formula, or the
  // keys typed while the formula text is on its way, which follow it.
  get bound() { return !!this.ed && (!!this.ed.waiting || this.text.startsWith('=')); }
  get cell() { return this.ed && { row: this.ed.row, col: this.ed.col }; }
  // untyped says whether a waiting edit holds nothing the person typed: no
  // keys, or the text the formula bar showed as it was.
  get untyped() { return this.text === (this.ed.from ?? ''); }

  // position is where the edited cell lies on screen now, or null when its
  // row or column is gone.
  position() {
    const r = this.model.rowIndex(this.ed.row);
    const c = this.model.colIndex(this.ed.col);
    return r < 0 || c < 0 ? null : { r, c };
  }

  // start opens the editor on the active cell. With replace the typed text
  // replaces the content; otherwise the content is edited in place as the
  // formula bar shows it: the text of an edit still pending, the cell's
  // edit text, or its formula text once the peek has arrived. Keys typed
  // while the peek is on its way, in the cell or in the formula bar, are
  // kept and follow the formula. A cell whose row of a snapshot has not
  // arrived whole may not be blank, so the editor waits for that row in
  // the same way (TECH 12.2). Pending formula text typed on a screen a
  // structural change has left since names the cells of that screen, so
  // it is adjusted to the screen first (TECH 8.4), and keys typed
  // meanwhile follow the adjusted text. typed is what was typed before the
  // editor opened in place, kept while it waits. from is the text typed
  // was made from, when typed already holds the content (fromBar).
  start({ replace = false, text = '', source = 'cell', keepCaret = false, typed = '', from = null } = {}) {
    if (this.ed) return true;
    if (!this.cb.canWrite()) return false;
    const a = this.selection.active();
    const row = this.model.rowAt(a.r);
    const col = this.model.colAt(a.c);
    this.ed = { row, col, orig: null, source, pointed: null, hint: null, waiting: false, rows: false, reopen: null, from };
    if (!replace && this.model.isPending(row, col)) {
      const p = this.model.movedPending(row, col);
      if (p) {
        this.#wait(typed);
        this.ed.reopen = { text: p.text, base: p.base, at: p.at };
        this.cb.reopen();
        return true;
      }
      text = this.model.shown(row, col);
    } else if (!replace && !this.model.known(row, col)) {
      this.#wait(typed);
      this.ed.rows = true;
      return true;
    } else if (!replace) {
      text = this.#content(row, col);
      if (text == null) {
        this.#wait(typed);
        this.cb.peek(row, col);
        return true;
      }
    }
    if (replace) {
      this.#show(text, null, keepCaret);
      return true;
    }
    const open = this.#resume(text, typed);
    if (open == null) {
      this.#refuse(row, col, typed, 'changed');
      return false;
    }
    this.#show(open, text, keepCaret);
    return true;
  }

  // fromBar opens an edit from the idle formula bar on what the bar holds:
  // the text it showed, as the person may have changed it already. Text it
  // showed as the cell's content is the edit's text as it stands, also
  // while the rest of the cell's row is on its way, so that content is in
  // it once. Keys typed into a bar that showed nothing, because the
  // content had not arrived, join that content when it does.
  // The bar may still show another cell, as when the selection moved and
  // the screen has not been drawn since. Text typed there was typed into
  // the other cell's text and is refused. With nothing typed the edit
  // starts on the active cell's text.
  fromBar({ keepCaret = true } = {}) {
    let shown = this.idle;
    let typed = this.bar.value;
    if (shown && this.model.tab) {
      const a = this.selection.active();
      const row = this.model.rowAt(a.r);
      const col = this.model.colAt(a.c);
      if (shown.row !== row || shown.col !== col) {
        if (typed !== shown.text) {
          if (this.cb.canWrite()) this.#refuse(row, col, typed, 'moved');
          return false;
        }
        shown = barText(this.model, row, col);
        typed = shown.text;
      }
    }
    return this.start({ source: 'bar', keepCaret, typed, from: shown?.content ? shown.text : null });
  }

  // #resume is the text an edit in place opens with once text, the cell's
  // content as the formula bar would show it now, is in hand, or null for
  // an edit that is refused. typed is what was typed so far. Keys typed
  // before any content was shown follow it. Text made from what the bar
  // showed stands when that is still the cell's text. When a change
  // applied before the bar was drawn again, nothing typed opens the edit
  // on the text as it is, and anything typed was typed into text the cell
  // no longer holds, so no key of it goes into the new text.
  #resume(text, typed) {
    const from = this.ed.from;
    if (from == null) return text + typed;
    if (from === text) return typed;
    return typed === from ? text : null;
  }

  // #refuse ends a bar edit of the cell (row, col), or does not start one,
  // and offers its text back. The bar shows the cell as it is once the
  // deltas the edit held have applied (closed).
  #refuse(row, col, kept, why) {
    if (this.ed) this.#close();
    this.cb.closed({ move: null, focus: false });
    this.idle = barText(this.model, row, col);
    if (this.bar.value !== this.idle.text) this.bar.value = this.idle.text;
    this.cb.refused(kept, why);
  }

  // #content is the text of a known cell as the formula bar shows it, or
  // null for a formula whose text has not arrived.
  #content(row, col) {
    const cell = this.model.cell(row, col);
    if (!cell?.formula) return cell ? cell.edit : '';
    return this.model.peeked(row, col)?.edit ?? null;
  }

  // #wait leaves the editor closed until its text arrives. Both surfaces
  // hold only what is typed meanwhile.
  #wait(typed) {
    this.ed.waiting = true;
    // An equal value is left alone, as in #show.
    if (this.grid.input.value !== typed) this.grid.input.value = typed;
    if (this.bar.value !== typed) this.bar.value = typed;
  }

  // peekArrived opens an editor that waited for its formula text, and
  // says whether it did.
  peekArrived(row, col) {
    const ed = this.ed;
    if (!ed?.waiting || ed.rows || ed.reopen || ed.row !== row || ed.col !== col) return false;
    ed.waiting = false;
    this.#opened(this.model.peeked(row, col).edit);
    return true;
  }

  // received opens an editor that waited for its cell's row once the grid
  // holds the cell, and says whether it did. A formula goes on to wait for
  // its text.
  received() {
    const ed = this.ed;
    if (!ed?.rows || !this.model.known(ed.row, ed.col)) return false;
    ed.rows = false;
    const text = this.#content(ed.row, ed.col);
    if (text == null) {
      this.cb.peek(ed.row, ed.col);
      return false;
    }
    ed.waiting = false;
    this.#opened(text);
    return true;
  }

  // waitsForPeek says whether the editor waits for the formula text of
  // the cell.
  waitsForPeek(row, col) { const ed = this.ed; return !!ed?.waiting && !ed.rows && !ed.reopen && ed.row === row && ed.col === col; }

  // reopened opens an editor on its pending text as adjusted to the screen.
  reopened(text) {
    const ed = this.ed;
    if (!ed?.reopen) return;
    ed.waiting = false;
    ed.reopen = null;
    this.#opened(text);
  }

  // #opened shows the text a waiting editor waited for, with the keys typed
  // meanwhile after it, unless what is typed was made from the text the
  // formula bar showed and holds it already (#resume).
  #opened(text) {
    const open = this.#resume(text, this.text);
    if (open == null) this.#refuse(this.ed.row, this.ed.col, this.text, 'changed');
    else this.#show(open, text);
  }

  #show(text, orig, keepCaret = false) {
    const ed = this.ed;
    ed.orig = orig;
    this.grid.editing = true;
    // An equal value is left alone: writing it would end a composition
    // that has just started, and move the bar's caret.
    if (this.grid.input.value !== text) this.grid.input.value = text;
    if (this.bar.value !== text) this.bar.value = text;
    this.grid.input.classList.add('on');
    this.grid.reveal(this.selection.active().r, this.selection.active().c);
    this.grid.paint();
    const ta = this.area;
    if (document.activeElement !== ta) ta.focus({ preventScroll: true });
    if (!keepCaret && !this.composing) ta.setSelectionRange(text.length, text.length);
    this.#size();
    this.#hint();
    this.cb.changed();
  }

  #input(from) {
    if (this.composing && !this.ed) return;
    if (!this.ed) {
      // Typing in the idle text area starts an edit that replaces the content.
      // In the formula bar the typed characters join the content it shows.
      if (from === this.bar) {
        if (!this.fromBar()) this.cb.changed();
        return;
      }
      const typed = from.value;
      from.value = '';
      if (typed === '') return;
      if (!this.start({ replace: true, text: typed, source: 'cell' })) this.cb.changed();
      return;
    }
    const other = from === this.bar ? this.grid.input : this.bar;
    const was = other.value;
    if (other.value !== from.value) other.value = from.value;
    if (this.ed.waiting) return;
    this.ed.source = from === this.bar ? 'bar' : 'cell';
    // The reference the last click inserted is the one a click replaces
    // while the caret is right after it and the edit left the text up to
    // there as it was. The same reference typed or pasted after it is the
    // person's text.
    const pt = this.ed.pointed;
    this.ed.pointed = pt && from.selectionStart === pt.end && was.slice(0, pt.end) === from.value.slice(0, pt.end) ? pt : null;
    this.#size();
    if (!this.composing) this.#hint();
  }

  // #size grows the cell's text area with its text: wider for a long line,
  // taller for more lines, never smaller than the cell.
  #size() {
    const ta = this.grid.input;
    const v = ta.value;
    const cell = this.grid.activeEl;
    const wrapped = cell.classList.contains('wrap');
    let w = 0;
    if (!wrapped) for (const line of v.split('\n')) w = Math.max(w, this.grid.measure.text(line, false) + 22);
    const max = Math.max(120, this.grid.el.clientWidth - 80);
    ta.style.width = w ? `${Math.min(w, max)}px` : '';
    const lines = v.split('\n').length;
    ta.style.height = '';
    const need = Math.max(lines * LINE + 8, wrapped ? (ta.scrollHeight || 0) + 4 : 0);
    ta.style.height = lines > 1 || wrapped ? `${need}px` : '';
    this.bar.style.height = '';
    this.bar.style.height = `${Math.min(92, Math.max(32, this.bar.scrollHeight || 32))}px`;
  }

  // key handles a key press while the editor is open. It returns true when
  // the key was used.
  key(e) {
    const ed = this.ed;
    if (!ed) return false;
    // Keys of a composition belong to it: they never commit or cancel.
    if (e.isComposing || this.composing || e.keyCode === 229) return true;
    if (ed.waiting) {
      if (e.key === 'Escape') { this.close(); e.preventDefault(); }
      return true;
    }
    const list = ed.hint?.items.length ? ed.hint : null;
    if (e.key === 'Enter' && e.altKey) this.insert('\n');
    else if (list && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      list.at = (list.at + (e.key === 'ArrowDown' ? 1 : -1) + list.items.length) % list.items.length;
      list.nav = true;
      this.#paintHint();
    } else if (list && (e.key === 'Tab' || (e.key === 'Enter' && list.nav))) this.#accept(list.at);
    else if (e.key === 'Enter') this.commit(e.shiftKey ? [-1, 0] : [1, 0]);
    else if (e.key === 'Tab') this.commit(e.shiftKey ? [0, -1] : [0, 1]);
    else if (e.key === 'Escape') this.cancel();
    else return true; // every other key edits the text
    e.preventDefault();
    e.stopPropagation();
    return true;
  }

  insert(s) {
    const ta = this.area;
    if (ta.readOnly) return;
    ta.setRangeText(s, ta.selectionStart, ta.selectionEnd, 'end');
    this.#input(ta);
  }

  // commit sends the typed text for the server to read and closes the
  // editor. Text that was opened in place and not changed writes nothing.
  // A commit the page cannot send now, as while another tab loads, leaves
  // the editor open with its text, so nothing typed is lost. The snapshot
  // that follows offers the text back. With focus false the grid does not
  // take focus, as for a commit before Find moves to a match.
  commit(move = null, { focus = true } = {}) {
    const ed = this.ed;
    if (!ed || ed.waiting || this.area.readOnly) return;
    const text = this.text;
    const at = this.position();
    const write = at && text !== ed.orig;
    if (write && !this.cb.canWrite()) return;
    this.#close();
    if (write) this.cb.commit({ row: ed.row, col: ed.col, text, at, move, focus });
    else this.cb.closed({ move, row: ed.row, col: ed.col, focus });
  }

  cancel() {
    if (!this.ed) return;
    this.#close();
    this.cb.closed({ move: null });
  }

  // close ends the edit without committing and returns the typed text, for
  // the notice that offers it back. An edit that waits for its formula
  // returns the keys typed meanwhile, after the pending text it reopened.
  close() {
    if (!this.ed) return null;
    const text = this.ed.from != null ? this.text : (this.ed.reopen?.text ?? '') + this.text;
    this.#close();
    this.cb.closed({ move: null });
    return text;
  }

  #close() {
    // The bar keeps the edit's text until it shows the active cell again.
    // An open edit's text follows the grid on show. A waiting edit left only
    // the keys typed there, or text made from what the bar showed.
    const ed = this.ed;
    this.idle = { row: ed.row, col: ed.col, text: this.bar.value, content: !ed.waiting || ed.from != null, peek: false };
    this.ed = null;
    this.composing = false;
    this.grid.editing = false;
    this.setReadOnly(false);
    const ta = this.grid.input;
    ta.value = '';
    ta.classList.remove('on');
    ta.style.width = '';
    ta.style.height = '';
    this.#closeHint();
  }

  setReadOnly(on) {
    for (const ta of [this.grid.input, this.bar]) {
      if (on) ta.setAttribute('readonly', ''); else ta.removeAttribute('readonly');
      ta.readOnly = on;
    }
  }

  // replaceText puts adjusted text into the editor and keeps the caret
  // where the text around it maps onto the adjusted text (TECH 8.4, step 3).
  replaceText(text) {
    const ta = this.area;
    const caret = caretByToken(ta.value, ta.selectionStart, text);
    this.grid.input.value = text;
    this.bar.value = text;
    ta.setSelectionRange(caret, caret);
    this.ed.pointed = null;
    this.#size();
  }

  // ---- reference insertion by click (rule 21) ----

  // pointable says whether a click on the grid inserts a reference: the
  // text is a formula and the caret is where a reference can stand, or
  // right after the reference the last click inserted.
  pointable() {
    if (!this.open || this.area.readOnly) return false;
    const ta = this.area;
    const v = ta.value;
    if (!v.startsWith('=')) return false;
    let before = v.slice(0, ta.selectionStart);
    const pt = this.ed.pointed;
    if (pt?.end === ta.selectionStart) before = before.slice(0, -pt.ref.length);
    return /[=+\-*/^&(,<>:]\s*$/.test(before);
  }

  // point inserts the address of a cell or range at the caret and replaces
  // the reference it has just inserted. A read-only editor keeps the text
  // it sent for the last exchange (TECH 8.4, step 4).
  point(r1, c1, r2 = r1, c2 = c1) {
    const ed = this.ed;
    const ta = this.area;
    if (!this.open || ta.readOnly) return;
    const pos = ta.selectionStart;
    const a = address(Math.min(r1, r2), Math.min(c1, c2));
    const ref = r1 === r2 && c1 === c2 ? a : `${a}:${address(Math.max(r1, r2), Math.max(c1, c2))}`;
    let before = ta.value.slice(0, pos);
    if (ed.pointed?.end === pos) before = before.slice(0, -ed.pointed.ref.length);
    const next = before + ref + ta.value.slice(pos);
    this.grid.input.value = next;
    this.bar.value = next;
    const at = before.length + ref.length;
    ta.setSelectionRange(at, at);
    ed.pointed = { ref, end: at };
    if (document.activeElement !== ta) ta.focus({ preventScroll: true });
    this.#size();
    this.#closeHint();
  }

  // ---- function hints (rule 21) ----

  #closeHint() {
    if (this.hintEl) { this.hintEl.remove(); this.hintEl = null; }
    if (this.ed) this.ed.hint = null;
    for (const ta of [this.grid.input, this.bar]) { ta.removeAttribute('aria-activedescendant'); ta.removeAttribute('aria-controls'); }
  }

  // #hint offers the functions that match the name being typed, or names
  // the arguments of the call the caret is in. Up and Down choose, Tab
  // accepts, and Enter accepts once a suggestion was chosen with the arrows.
  #hint() {
    const ed = this.ed;
    this.#closeHint();
    if (!ed || ed.waiting || !this.functions.length) return;
    const ta = this.area;
    const v = ta.value;
    if (!v.startsWith('=')) return;
    const before = v.slice(0, ta.selectionStart);
    if ((before.split('"').length - 1) % 2) return; // inside a text literal
    const m = /(?:^|[^A-Za-z0-9_.$!'])([A-Za-z][A-Za-z0-9]*)$/.exec(before);
    const items = m ? this.functions.filter((f) => f.name.startsWith(m[1].toUpperCase())).slice(0, HINT_MAX) : [];
    let body;
    if (items.length) {
      ed.hint = { items, at: 0, nav: false, prefix: m[1] };
      body = [
        items.map((f, i) => h('div', { class: 'fn', role: 'option', id: `fn-opt-${i}`, 'aria-selected': String(i === 0), onmousedown: (ev) => { ev.preventDefault(); this.#accept(i); } },
          h('b', { text: f.name }), h('span', { text: `(${f.args.join(', ')})` }))),
        h('div', { class: 'keys', text: 'Up and Down choose · Tab accepts · Enter accepts a chosen one' }),
      ];
    } else {
      const call = enclosingCall(before);
      const f = call && this.functions.find((x) => x.name === call.name);
      if (!f) return;
      ed.hint = { items: [], at: 0 };
      const last = f.args.length - 1;
      body = [h('div', { class: 'sig' }, `${f.name}(`, f.args.flatMap((p, i) => [i ? ', ' : '', i === Math.min(call.arg, last) ? h('em', { text: p }) : p]), ')')];
    }
    this.hintEl = h('div', { class: 'hint', role: items.length ? 'listbox' : 'note', id: 'fn-list', 'aria-label': items.length ? 'Matching functions' : 'Arguments' }, body);
    this.host.appendChild(this.hintEl);
    ta.setAttribute('aria-controls', 'fn-list');
    this.placeHint();
    if (items.length) announce(`${plural(items.length, 'function')} ${items.length === 1 ? 'matches' : 'match'}. ${items[0].name} is first.`);
  }

  placeHint() {
    const el = this.hintEl;
    if (!el) return;
    const a = this.area.getBoundingClientRect();
    const w = this.host.getBoundingClientRect();
    el.style.left = `${Math.max(8, Math.min(a.left - w.left, w.width - (el.offsetWidth || 0) - 8))}px`;
    el.style.top = `${Math.min(a.bottom - w.top + 4, w.height - (el.offsetHeight || 0) - 8)}px`;
  }

  #paintHint() {
    const hint = this.ed.hint;
    this.hintEl.querySelectorAll('.fn').forEach((el, i) => el.setAttribute('aria-selected', String(i === hint.at)));
    this.area.setAttribute('aria-activedescendant', `fn-opt-${hint.at}`);
    const f = hint.items[hint.at];
    announce(`${f.name}, ${f.args.join(', ')}`);
  }

  #accept(i) {
    const hint = this.ed.hint;
    const ta = this.area;
    if (ta.readOnly) return;
    const pos = ta.selectionStart;
    ta.setRangeText(`${hint.items[i].name}(`, pos - hint.prefix.length, pos, 'end');
    if (document.activeElement !== ta) ta.focus({ preventScroll: true });
    this.#input(ta);
  }
}

// enclosingCall finds the function call the end of `before` lies in and the
// index of its argument, by counting brackets and commas outside text.
function enclosingCall(before) {
  let depth = 0;
  let arg = 0;
  let quoted = false;
  for (let i = before.length - 1; i >= 0; i--) {
    const ch = before[i];
    if (ch === '"') quoted = !quoted;
    if (quoted) continue;
    if (ch === ')') depth++;
    else if (ch === '(') {
      if (depth === 0) {
        const m = /([A-Za-z][A-Za-z0-9]*)$/.exec(before.slice(0, i));
        return m ? { name: m[1].toUpperCase(), arg } : null;
      }
      depth--;
    } else if (ch === ',' && depth === 0) arg++;
  }
  return null;
}

// Rebaser holds structural deltas back while the editor holds a formula and
// runs the `rebase` exchange (TECH 8.4). The screen changes only together
// with the adjusted text, so no text ever mixes two screen states. An
// editor that waits for its text holds the screen too, because the keys
// typed meanwhile are read against it. Pending text an editor reopens is
// adjusted from the base of its action to the screen by the same exchange
// before the editor opens. cb:
// apply(delta) installs a delta on the screen, offer(text, why, reason)
// shows the notice that offers the typed text back, moved(position) tells
// where the edited cell lies after the exchange.
export class Rebaser {
  constructor({ editor, model, net, cb, now = () => Date.now() }) {
    this.editor = editor;
    this.model = model;
    this.net = net;
    this.cb = cb;
    this.now = now;
    this.held = [];
    this.sizes = [];      // encoded bytes of each held delta
    this.bytes = 0;
    this.inflight = null; // {id, text, through, reopen}
    this.tries = 0;
    this.since = 0;
    this.waitComposition = false;
    this.aborting = false;
  }

  get holding() { return this.held.length > 0; }

  // take is offered every delta before it reaches the screen and returns
  // true when it held the delta back. size() is the encoded bytes of the
  // delta as it arrived, in one frame or in parts. The limit on held deltas
  // is in those bytes, and size is asked only for a delta that is held: a
  // delta that goes straight to the screen costs nothing here.
  take(d, size) {
    if (!this.holding) {
      if (!this.editor.bound || !isStructural(d, this.model.tabs)) return false;
      this.tries = 0;
      this.since = this.now();
    }
    const bytes = size();
    this.held.push(d);
    this.sizes.push(bytes);
    this.bytes += bytes;
    if (this.bytes > HOLD_MAX) { this.abort('overflow'); return true; }
    if (!this.inflight) this.#exchange();
    return true;
  }

  // #exchange asks for the editor text to be adjusted through the newest
  // held delta. Pending text being reopened is adjusted from its action's
  // base to the screen instead. It needs no wait for a composition, as what
  // is composed is not in that text. An editor that waits for its formula
  // has no text to send until the peek arrives (opened).
  #exchange() {
    const ed = this.editor.ed;
    if (!ed || (ed.waiting && !ed.reopen)) return;
    const r = ed.reopen;
    if (!r && this.editor.composing) { this.waitComposition = true; return; }
    const at = this.editor.position();
    if (!at) { this.abort('gone'); return; }
    const text = r ? r.text : this.editor.text;
    const through = r ? this.model.seq : this.held[this.held.length - 1].seq;
    const fields = { tab: this.model.tab.id, row: ed.row, col: ed.col, text, at: r ? r.at : wirePosition(at.r, at.c), base: r ? r.base : this.model.seq, through };
    // Text that no frame can carry is no formula the host could adjust.
    // Sent, it would end the connection each time it was asked again.
    if (!this.net.rebaseFits(fields)) { this.abort('long'); return; }
    const id = this.net.requestId();
    this.inflight = { id, text, through, reopen: !!r };
    this.tries++;
    // Offline the request waits: reconnected() sends it again.
    if (!this.net.rebase(id, fields)) this.inflight = null;
  }

  // reopen adjusts the pending text an editor reopens.
  reopen() { if (!this.inflight) this.#exchange(); }

  // opened runs when an editor that waited opens on its text. The deltas
  // held meanwhile are exchanged now, or applied when the text is no
  // formula or none of them is structural.
  opened() {
    if (!this.holding || this.inflight) return;
    if (!this.editor.isFormula || !this.held.some((d) => isStructural(d, this.model.tabs))) { this.release(); return; }
    this.tries = 0;
    this.since = this.now();
    this.#exchange();
  }

  // composed runs the exchange that waited for the composition to end.
  composed() {
    if (!this.waitComposition || !this.holding) return;
    this.waitComposition = false;
    if (!this.inflight) this.#exchange();
  }

  // reconnected asks again: the answer to a rebase sent before the
  // connection dropped never arrives.
  reconnected() {
    this.inflight = null;
    if (this.holding || this.editor.ed?.reopen) this.#exchange();
  }

  answer(m) {
    const sent = this.inflight;
    if (!sent) return;
    // An answer that does not fit says nothing of the text: the editor
    // closes as for a refusal and offers it back. It is read before it is
    // matched. One whose id does not fit could be the answer to the
    // exchange in flight, which would otherwise wait for good.
    let wrong = rebasedError(m);
    if (m.id !== sent.id && (!wrong || isId(m.id))) return;
    // The host adjusts the text through the sequence it was asked for and
    // names it as the new base. Another base would apply held deltas the
    // text was not adjusted for, or leave some out.
    if (!wrong && m.refused === undefined && m.base !== undefined && m.base !== sent.through) wrong = `a rebase answer through ${m.base} for an exchange through ${sent.through}`;
    this.inflight = null;
    if (wrong) {
      this.abort('refused');
      throw new TypeError(wrong);
    }
    if (m.refused !== undefined) { this.abort('refused', m.refused); return; }
    if (sent.reopen) {
      // The adjusted text is of the screen on show, and no held delta is in it.
      if (!this.editor.position()) { this.abort('gone'); return; }
      this.editor.reopened(m.text ?? sent.text);
      if (m.at) this.cb.moved(screenPosition(m.at));
      this.opened();
      return;
    }
    if (this.editor.composing) { this.waitComposition = true; return; }
    if (this.editor.text !== sent.text) {
      // The person typed or clicked meanwhile, against the screen they
      // still see. Ask again with the new text and the same base.
      if (this.tries >= REBASE_TRIES || this.now() - this.since >= REBASE_WAIT) this.editor.setReadOnly(true);
      this.#exchange();
      return;
    }
    const through = m.base ?? sent.through;
    this.releaseThrough(through);
    this.editor.setReadOnly(false);
    if (!this.editor.open) return;
    if (!this.editor.position()) { this.abort('gone'); return; }
    if (m.text != null) this.editor.replaceText(m.text);
    if (m.at) this.cb.moved(screenPosition(m.at));
    if (!this.holding) return;
    if (this.held.some((d) => isStructural(d, this.model.tabs))) {
      this.tries = 0;
      this.since = this.now();
      this.#exchange();
    } else this.release();
  }

  #applyNext() {
    const d = this.held.shift();
    this.bytes -= this.sizes.shift();
    this.cb.apply(d);
  }

  // releaseThrough applies the held deltas up to sequence seq, in order.
  releaseThrough(seq) {
    while (this.held.length && BigInt(this.held[0].seq) <= BigInt(seq)) this.#applyNext();
  }

  // closed runs after every close of the editor. The deltas it held apply,
  // and an exchange still on its way was asked for that editor's text, so
  // its answer is read by no other editor. An abort applies them itself.
  closed() {
    if (!this.aborting) this.release();
  }

  // release applies everything held, in order: after a commit, which the
  // server adjusts from the old base, and after a cancel.
  release() {
    this.inflight = null;
    this.waitComposition = false;
    while (this.held.length) this.#applyNext();
    this.bytes = 0;
  }

  // drop forgets the held deltas: a new snapshot replaces them.
  drop() {
    this.held = [];
    this.sizes = [];
    this.bytes = 0;
    this.inflight = null;
    this.waitComposition = false;
  }

  // abort closes the editor without committing and offers the typed text
  // back before the grid changes; only then do the held deltas apply.
  abort(why, reason) {
    this.aborting = true;
    const text = this.editor.close();
    this.cb.offer(text, why, reason);
    this.aborting = false;
    this.release();
  }
}
