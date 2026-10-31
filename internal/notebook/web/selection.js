// Model selection (TECH 18). Inside one block the native selection is the
// source; it is read as code point offsets. Across blocks the editor keeps
// {block, offset} endpoints, never retained DOM ranges, and move, delete,
// duplicate, convert and copy act on the blocks those endpoints cover.
'use strict';


import { domToCP, cpToDom, sliceRuns, plainText, textLength } from './richtext.js';
import { isText } from './state.js';

export function readEndpoint(el, node, offset) {
  return node && el.contains(node) ? domToCP(el, node, offset) : null;
}

// readCaret returns the native selection inside el as {start, end} code
// point offsets in document order, or null when it is elsewhere.
export function readCaret(el) {
  const s = globalThis.getSelection && getSelection();
  if (!s) return null;
  const a = readEndpoint(el, s.anchorNode, s.anchorOffset), f = readEndpoint(el, s.focusNode, s.focusOffset);
  if (a == null || f == null) return null;
  return { start: Math.min(a, f), end: Math.max(a, f), anchor: a, focus: f, collapsed: a === f };
}

// placeCaret sets the native selection inside el from code point offsets.
export function placeCaret(el, { start, end = start }) {
  const s = globalThis.getSelection && getSelection();
  if (!s) return;
  const a = cpToDom(el, start), b = cpToDom(el, end);
  s.setBaseAndExtent(a.node, a.offset, b.node, b.offset);
}

// ModelSelection is the editor's cross-block selection: anchor and focus
// endpoints {block, offset}. A block selection (after Escape, or Backspace
// onto a block without text) covers whole blocks with offset null.
export class ModelSelection {
  constructor() { this.anchor = null; this.focus = null; }

  get active() { return !!this.anchor; }
  get crossBlock() { return this.active && (this.anchor.block !== this.focus.block || this.anchor.offset == null); }

  clear() { this.anchor = null; this.focus = null; }

  set(anchor, focus = anchor) { this.anchor = { ...anchor }; this.focus = { ...focus }; }

  selectBlocks(ids) {
    if (!ids.length) return this.clear();
    this.anchor = { block: ids[0], offset: null };
    this.focus = { block: ids.at(-1), offset: null };
  }

  // ordered returns [start, end] in document order of the page.
  ordered(view, page) {
    if (!this.active) return null;
    const order = view.order(page).map((b) => b.ref.id);
    const ia = order.indexOf(this.anchor.block), ifo = order.indexOf(this.focus.block);
    if (ia < 0 || ifo < 0) return null;
    if (ia < ifo || (ia === ifo && (this.anchor.offset ?? 0) <= (this.focus.offset ?? 0))) return [this.anchor, this.focus, ia, ifo];
    return [this.focus, this.anchor, ifo, ia];
  }

  // blocks lists the ids the selection covers in document order.
  blocks(view, page) {
    const o = this.ordered(view, page);
    if (!o) return [];
    return view.order(page).slice(o[2], o[3] + 1).map((b) => b.ref.id);
  }

  // extend moves the focus to the previous or next block in document
  // order, keeping the anchor (Shift+arrow across a block boundary).
  extend(view, page, dir) {
    if (!this.active) return false;
    const order = view.order(page);
    const i = order.findIndex((b) => b.ref.id === this.focus.block);
    const next = order[i + dir];
    if (!next) return false;
    const len = isText(next.content.kind) ? textLength(next.content.text) : 0;
    this.focus = { block: next.ref.id, offset: this.anchor.offset == null ? null : dir < 0 ? 0 : len };
    return true;
  }
}

// selectionText serializes the covered text from the model, not from the
// DOM: partial first and last blocks, whole blocks between, one line each.
export function selectionText(view, page, sel) {
  const o = sel.ordered(view, page);
  if (!o) return '';
  const [start, end, i, j] = o;
  const lines = [];
  view.order(page).slice(i, j + 1).forEach((b, k, all) => {
    const c = b.content;
    let text;
    const from = k === 0 && start.offset != null ? start.offset : 0;
    const to = k === all.length - 1 && end.offset != null ? end.offset : Infinity;
    if (isText(c.kind)) text = plainText(sliceRuns(c.text, from, to));
    else if (c.kind === 'code') text = [...c.text].slice(from, to).join('');
    else if (c.kind === 'image') text = c.alt;
    else if (c.kind === 'page_link') { const p = view.get(c.target); text = p ? p.title : ''; }
    else text = '';
    lines.push(text);
  });
  return lines.join('\n');
}
