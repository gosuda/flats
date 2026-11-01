// Block editing (TECH 14 and 18, rule 19). Each text block is its own
// contenteditable. beforeinput turns browser edits into model edits on code
// point offsets, so the DOM never decides content; an input event without a
// handled beforeinput (Safari gaps, word deletion without target ranges) is
// reconciled from the DOM text once. Enter, Backspace at a block start and
// Tab become semantic actions. Input method composition is left to the
// browser and submitted whole at compositionend.
'use strict';


import * as act from './actions.js';
import { readCaret, placeCaret } from './selection.js';
import {
  replaceRange, deleteRange, toggleMark, diffRuns, inlineShortcut, blockShortcut, plainText, textLength,
  cpLength, domText, domToCP, sliceRuns, canonicalize,
} from './richtext.js';
import { isText, groupVersion } from './state.js';

// fieldRuns is the rich text an editable shows: a text block's text, a
// code block's text as one unmarked run, or an image caption.
export function fieldRuns(block, field) {
  const c = block.content;
  if (field === 'caption') return c.caption;
  if (c.kind === 'code') return c.text ? [{ text: c.text, marks: [] }] : [];
  return c.text;
}

// withField returns the block's content with field set to runs.
export function withField(block, field, runs) {
  const c = block.content;
  if (field === 'caption') return { ...c, caption: canonicalize(runs) };
  if (c.kind === 'code') return { ...c, text: plainText(runs) };
  return { ...c, text: canonicalize(runs) };
}

// Composition captures what TECH 14 requires at compositionstart and
// decides the write at compositionend.
export class Composition {
  constructor(block, field, caret) {
    this.block = block.ref.id;
    this.inc = block.ref.inc;
    this.field = field;
    this.content = block.content;
    this.baseV = groupVersion(block, 'content');
    this.runs = fieldRuns(block, field);
    this.caret = caret;
  }

  // end returns {op, runs} to submit the composed text whole with the
  // captured kind, or {orphan} when the block was deleted, revived or reset
  // meanwhile, in which case nothing is written to it.
  end(view, text) {
    const runs = diffRuns(this.runs, text);
    const now = view.get(this.block);
    const content = this.field === 'caption' ? { ...this.content, caption: runs }
      : this.content.kind === 'code' ? { ...this.content, text: plainText(runs) } : { ...this.content, text: runs };
    if (!now || now.ref.inc !== this.inc) return { orphan: { block: this.block, content } };
    if (plainText(fieldRuns({ content: this.content }, this.field)) === text) return { unchanged: true };
    return { op: { op: 'set_block', block: { id: this.block, inc: this.inc }, content, base_v: this.baseV }, runs };
  }
}

const MOD = (e) => e.metaKey || e.ctrlKey;
const MARK_KEYS = { b: 'bold', i: 'italic', e: 'code' };

export class Editor {
  // app: {store, view(), page, writable(), typing(block, content, opts),
  //       queue(ops, opts), render(), focusBlock(id, offset), menu, selection,
  //       notice(text, content, original), undo(redo), paste(e, block, caret),
  //       openSlash(el, block, query), closeSlash(), selectBlocks(ids)}
  constructor(app) {
    this.app = app;
    this.composition = null;
    this.suppress = null; // {el, text}: the duplicate input after compositionend
  }

  attach(root) {
    root.addEventListener('beforeinput', (e) => this.beforeinput(e));
    root.addEventListener('input', (e) => this.input(e));
    root.addEventListener('keydown', (e) => this.keydown(e));
    root.addEventListener('compositionstart', (e) => this.compositionstart(e));
    root.addEventListener('compositionend', (e) => this.compositionend(e));
    root.addEventListener('paste', (e) => this.paste(e));
  }

  // target finds the editable, its block and field for an event.
  target(e) {
    const el = e.target && e.target.closest && e.target.closest('[data-field]');
    if (!el) return null;
    const blk = el.closest('.blk');
    const block = blk && this.app.view().get(blk.dataset.id);
    if (!block) return null;
    return { el, block, field: el.dataset.field };
  }

  caretOf(el, runs) {
    return readCaret(el) || { start: textLength(runs), end: textLength(runs), collapsed: true };
  }

  // rangeOf converts the event's first target range to code point offsets.
  rangeOf(e, el) {
    const r = e.getTargetRanges && e.getTargetRanges()[0];
    if (!r) return null;
    return { start: domToCP(el, r.startContainer, r.startOffset), end: domToCP(el, r.endContainer, r.endOffset) };
  }

  beforeinput(e) {
    if (this.composition || e.isComposing) return;
    const t = this.target(e);
    if (!t) return;
    if (!this.app.writable()) { e.preventDefault(); return; }
    const { el, block, field } = t;
    const runs = fieldRuns(block, field);
    const caret = this.caretOf(el, runs);
    const kind = block.content.kind;
    switch (e.inputType) {
      case 'insertText':
      case 'insertReplacementText': {
        const text = e.data ?? (e.dataTransfer ? e.dataTransfer.getData('text/plain') : '');
        const range = e.inputType === 'insertReplacementText' ? this.rangeOf(e, el) || caret : caret;
        e.preventDefault();
        this.typeText(t, runs, range, text);
        return;
      }
      case 'insertLineBreak':
        e.preventDefault();
        this.write(t, replaceRange(runs, caret.start, caret.end, '\n'), caret.start + 1);
        return;
      case 'insertParagraph':
        e.preventDefault();
        this.enter(t, runs, caret);
        return;
      case 'deleteContentBackward':
      case 'deleteContentForward':
      case 'deleteWordBackward':
      case 'deleteWordForward':
      case 'deleteSoftLineBackward':
      case 'deleteHardLineBackward':
      case 'deleteByCut': {
        if (e.inputType === 'deleteContentBackward' && caret.collapsed && caret.start === 0 && field === 'text') {
          e.preventDefault();
          this.backspaceAtStart(t, runs);
          return;
        }
        let range = caret.collapsed ? this.rangeOf(e, el) : caret;
        if (!range && e.inputType.startsWith('deleteContent')) {
          range = e.inputType === 'deleteContentBackward' ? { start: Math.max(0, caret.start - 1), end: caret.start } : { start: caret.start, end: Math.min(textLength(runs), caret.start + 1) };
        }
        if (!range) return; // word or line deletion without target ranges: the input fallback reconciles
        e.preventDefault();
        if (range.end > range.start) this.write(t, deleteRange(runs, range.start, range.end), range.start);
        return;
      }
      case 'formatBold': e.preventDefault(); this.mark(t, runs, caret, 'bold'); return;
      case 'formatItalic': e.preventDefault(); this.mark(t, runs, caret, 'italic'); return;
      case 'formatStrikeThrough': e.preventDefault(); this.mark(t, runs, caret, 'strike'); return;
      case 'historyUndo': e.preventDefault(); this.app.undo(false); return;
      case 'historyRedo': e.preventDefault(); this.app.undo(true); return;
      case 'insertFromPaste':
      case 'insertFromDrop':
      case 'insertFromYank':
      case 'formatUnderline':
      case 'formatFontColor':
      case 'formatBackColor':
        e.preventDefault(); // paste has its own handler; other formats have no model
        return;
      default:
        if (kind === 'code' || isText(kind) || field === 'caption') return; // reconciled in input
    }
  }

  // input is the fallback for edits beforeinput did not handle.
  input(e) {
    if (this.composition || e.isComposing) return;
    const t = this.target(e);
    if (!t) return;
    const text = domText(t.el);
    if (this.suppress && this.suppress.el === t.el) {
      const same = this.suppress.text === text;
      this.suppress = null;
      if (same) return;
    }
    const runs = fieldRuns(t.block, t.field);
    if (plainText(runs) === text) return;
    const caret = readCaret(t.el);
    this.write(t, diffRuns(runs, text), caret ? caret.start : cpLength(text));
  }

  typeText(t, runs, range, text) {
    const { block, field } = t;
    let next = replaceRange(runs, range.start, range.end, text);
    let caret = range.start + cpLength(text);
    if (field === 'text' && isText(block.content.kind) && caret === textLength(next)) {
      const kind = blockShortcut(plainText(next));
      if (kind && kind !== block.content.kind) {
        this.app.closeSlash();
        this.app.apply(act.shortcut(this.app.view(), block, kind));
        return;
      }
    }
    if (isText(block.content.kind) || field === 'caption') {
      const s = inlineShortcut(next, caret);
      if (s) { next = s.runs; caret = s.caret; }
    }
    this.write(t, next, caret);
    if (field === 'text' && isText(block.content.kind)) this.slash(t, next, caret);
  }

  // slash opens or filters the block menu while the text before the caret
  // is a slash query that started at the block start or after a space.
  slash(t, runs, caret) {
    const before = plainText(sliceRuns(runs, 0, caret));
    const m = before.match(/(^|\s)\/([^\s/]{0,24})$/);
    if (m) this.app.openSlash(t.el, t.block, m[2], { start: caret - cpLength(m[2]) - 1, end: caret });
    else this.app.closeSlash();
  }

  write(t, runs, caret) {
    const block = this.app.view().get(t.block.ref.id);
    if (!block) return;
    this.app.typing(block, withField(block, t.field, runs));
    this.app.render();
    const el = this.app.editableOf(block.ref.id, t.field);
    if (el) { el.focus(); placeCaret(el, { start: caret }); }
  }

  mark(t, runs, caret, mark) {
    if (t.block.content.kind === 'code') return;
    if (caret.collapsed) return;
    const block = this.app.view().get(t.block.ref.id);
    this.app.queue([act.setBlock(block, withField(block, t.field, toggleMark(runs, caret.start, caret.end, mark)))]);
    const el = this.app.editableOf(block.ref.id, t.field);
    if (el) placeCaret(el, caret);
  }

  enter(t, runs, caret) {
    const { block, field } = t;
    if (block.content.kind === 'code' || field !== 'text') {
      this.write(t, replaceRange(runs, caret.start, caret.end, '\n'), caret.start + 1);
      return;
    }
    if (this.app.slashOpen()) { this.app.pickSlash(); return; }
    const trimmed = caret.collapsed ? runs : deleteRange(runs, caret.start, caret.end);
    this.app.apply(act.enter(this.app.view(), block, trimmed, caret.start));
  }

  backspaceAtStart(t, runs) {
    const { block } = t;
    if (!isText(block.content.kind)) {
      const prev = act.previousInOrder(this.app.view(), block);
      if (prev) this.app.selectBlocks([prev.ref.id]);
      return;
    }
    const plan = act.backspace(this.app.view(), block, runs);
    if (!plan) return;
    if (plan.select) this.app.selectBlocks(plan.select);
    else this.app.apply(plan);
  }

  keydown(e) {
    if (e.isComposing || e.keyCode === 229) return;
    if (this.app.slashOpen() && this.app.menuKey(e)) return;
    const t = this.target(e);
    if (!t) return;
    const { el, block, field } = t;
    const runs = fieldRuns(block, field);
    const caret = this.caretOf(el, runs);
    const len = textLength(runs);
    if (e.key === 'Enter' && !e.shiftKey && !MOD(e)) {
      e.preventDefault();
      if (this.app.writable()) this.enter(t, runs, caret);
      return;
    }
    if (e.key === 'Backspace' && !MOD(e) && caret.collapsed && caret.start === 0 && field === 'text') {
      e.preventDefault();
      if (this.app.writable()) this.backspaceAtStart(t, runs);
      return;
    }
    if (e.key === 'Tab' && !MOD(e) && block.content.kind !== 'code') {
      e.preventDefault();
      if (!this.app.writable()) return;
      const view = this.app.view();
      const plan = e.shiftKey ? act.outdent(view, [block]) : act.indent(view, [block]);
      if (plan && plan.refused) this.app.announce('This block cannot be nested deeper.');
      else if (plan) this.app.apply({ ...plan, focus: { block: block.ref.id, offset: caret.start } });
      return;
    }
    if (MOD(e) && !e.altKey) {
      const k = e.key.toLowerCase();
      if (k === 'z') { e.preventDefault(); this.app.undo(e.shiftKey); return; }
      if (k === 'y' && e.ctrlKey) { e.preventDefault(); this.app.undo(true); return; }
      if (MARK_KEYS[k] && !e.shiftKey) { e.preventDefault(); if (this.app.writable()) this.mark(t, runs, caret, MARK_KEYS[k]); return; }
      if (k === 's' && e.shiftKey) { e.preventDefault(); if (this.app.writable()) this.mark(t, runs, caret, 'strike'); return; }
      if (k === 'd') { e.preventDefault(); if (this.app.writable()) this.app.queue(act.duplicate([block])); return; }
      if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') && e.shiftKey) {
        e.preventDefault();
        const ops = this.app.writable() && act.moveBy(this.app.view(), [block], e.key === 'ArrowUp' ? -1 : 1);
        if (ops) this.app.queue(ops, { focus: { block: block.ref.id, offset: caret.start } });
        return;
      }
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      this.app.closeSlash();
      this.app.selectBlocks([block.ref.id]);
      return;
    }
    const atStart = caret.collapsed && caret.start === 0, atEnd = caret.collapsed && caret.end === len;
    if ((e.key === 'ArrowUp' || e.key === 'ArrowLeft') && atStart && !e.altKey) {
      if (e.shiftKey && e.key === 'ArrowUp') { e.preventDefault(); this.app.extendSelection(block, caret, -1); return; }
      if (e.shiftKey) return;
      const prev = act.previousInOrder(this.app.view(), block);
      if (prev) { e.preventDefault(); this.app.focusBlock(prev.ref.id, e.key === 'ArrowLeft' ? Infinity : 0); }
      return;
    }
    if ((e.key === 'ArrowDown' || e.key === 'ArrowRight') && atEnd && !e.altKey) {
      if (e.shiftKey && e.key === 'ArrowDown') { e.preventDefault(); this.app.extendSelection(block, caret, 1); return; }
      if (e.shiftKey) return;
      const next = act.nextInOrder(this.app.view(), block);
      if (next) { e.preventDefault(); this.app.focusBlock(next.ref.id, 0); }
    }
  }

  confirmCreation(ref) {
    const c = this.composition;
    if (!c || c.block !== ref.id || c.inc !== '0') return;
    c.inc = ref.inc;
    if (c.baseV === '0') c.baseV = ref.inc;
  }

  compositionstart(e) {
    const t = this.target(e);
    if (!t) return;
    this.app.closeRun();
    this.composition = new Composition(t.block, t.field, this.caretOf(t.el, fieldRuns(t.block, t.field)));
    this.app.store.composing = { block: t.block.ref.id };
  }

  compositionend(e) {
    const c = this.composition;
    if (!c) return;
    this.composition = null;
    this.app.store.composing = null;
    const el = this.app.editableOf(c.block, c.field) || (e.target && e.target.closest && e.target.closest('[data-field]'));
    const text = el ? domText(el) : '';
    const caret = el ? readCaret(el) : null;
    const r = c.end(this.app.view(), text);
    if (r.orphan) {
      this.app.notice('The block you were writing in was removed, so your text was not saved there.', r.orphan.content, null);
      this.app.render();
      return;
    }
    if (r.op) {
      this.suppress = el ? { el, text } : null;
      this.app.typing(this.app.view().get(c.block), r.op.content, { baseV: c.baseV, composed: true });
    }
    this.app.render();
    const current = this.app.editableOf(c.block, c.field);
    if (current && caret) { current.focus(); placeCaret(current, caret); }
    if (r.runs && c.field === 'text' && isText(c.content.kind) && caret) this.slash({ el: current, block: this.app.view().get(c.block) }, r.runs, caret.start);
  }

  paste(e) {
    const t = this.target(e);
    if (!t || !this.app.writable()) return;
    e.preventDefault();
    const runs = fieldRuns(t.block, t.field);
    this.app.paste(e.clipboardData, t, this.caretOf(t.el, runs));
  }

  // insertPlain inserts pasted plain text at the caret.
  insertPlain(t, caret, text) {
    const block = this.app.view().get(t.block.ref.id);
    if (!block) return;
    const runs = fieldRuns(block, t.field);
    const clean = block.content.kind === 'code' ? text : text.replace(/\r\n?/g, '\n');
    this.app.closeRun();
    const next = replaceRange(runs, caret.start, caret.end, clean);
    this.app.queue([act.setBlock(block, withField(block, t.field, next))], { focus: { block: block.ref.id, offset: caret.start + cpLength(clean), field: t.field } });
  }
}

