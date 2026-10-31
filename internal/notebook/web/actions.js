// Semantic editing actions (TECH 7 and 18, rule 19). Each planner reads the
// display view and returns the operations of one editing step. Compound
// steps put one unit_id on every operation and record their dependencies in
// structure checks on the first operation, so the server applies them whole
// or not at all. Planners never choose physical position keys.
'use strict';


import { ref, isText, PARENT_KINDS, groupVersion, convertContent, withDefaults, TEXT_KINDS } from './state.js';
import { splitRuns, concatRuns, textLength } from './richtext.js';

const B32 = 'abcdefghijklmnopqrstuvwxyz234567';
const PREFIX = { page: 'p_', block: 'b_', database: 'd_', property: 'pr_', option: 'o_', row: 'r_', view: 'v_' };

// newId proposes a fresh id: 128 random bits as 26 unpadded base32
// characters, the last carrying two zero padding bits (TECH 4.1).
export function newId(kind) {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let bits = 0, value = 0, out = '';
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) { out += B32[(value >>> (bits - 5)) & 31]; bits -= 5; }
  }
  out += B32[(value << (5 - bits)) & 31];
  return PREFIX[kind] + out;
}

let unitCounter = 0;
export const newUnitId = () => 'u' + Date.now().toString(36) + '-' + (++unitCounter).toString(36);
export const newActionId = () => 'a' + Date.now().toString(36) + '-' + (++unitCounter).toString(36) + '-' + newId('block').slice(2, 10);

export const MAX_BLOCK_DEPTH = 6;
export const CONTINUES = ['bullet', 'numbered', 'todo'];

const pageRef = (view, id) => ref(view.get(id));
const parentRef = (view, block) => (block.parent ? ref(view.get(block.parent)) : null);
const baseV = (block) => groupVersion(block, 'content');

export function setBlock(block, content) {
  return { op: 'set_block', block: ref(block), content, base_v: baseV(block) };
}

function check(view, block, extra = {}) {
  return { block: ref(block), page: pageRef(view, block.page), parent: parentRef(view, block), ...extra };
}

function unit(ops, structure) {
  const id = newUnitId();
  const out = ops.map((op) => ({ ...op, unit_id: id }));
  if (structure && structure.length) out[0].structure = structure;
  return out;
}

const textContent = (kind, text, from) => {
  const c = withDefaults({ kind, text });
  if (from && from.kind === kind && kind === 'callout') c.tone = from.tone;
  return c;
};

// previousSibling and the document-order neighbors of a block.
export function siblings(view, block) { return view.blocks(block.page, block.parent); }
export function previousSibling(view, block) {
  const s = siblings(view, block);
  const i = s.findIndex((b) => b.ref.id === block.ref.id);
  return i > 0 ? s[i - 1] : null;
}
export function previousInOrder(view, block) {
  const order = view.order(block.page);
  const i = order.findIndex((b) => b.ref.id === block.ref.id);
  return i > 0 ? order[i - 1] : null;
}
export function nextInOrder(view, block) {
  const order = view.order(block.page);
  const i = order.findIndex((b) => b.ref.id === block.ref.id);
  return i >= 0 && i + 1 < order.length ? order[i + 1] : null;
}

// subtreeHeight counts the levels of a block and its descendants.
function subtreeHeight(view, block) {
  const kids = view.blocks(block.page, block.ref.id);
  return 1 + (kids.length ? Math.max(...kids.map((k) => subtreeHeight(view, k))) : 0);
}

// ---- Enter ----

// enter plans Enter at code point offset at in a text block whose current
// rich text is runs. Enter in an empty continuing list item ends the list;
// otherwise the block splits, and the new block continues the list or is a
// paragraph. The new block is placed after the block being split.
export function enter(view, block, runs, at) {
  const kind = block.content.kind;
  if (CONTINUES.includes(kind) && !textLength(runs)) {
    return { ops: [{ op: 'turn_blocks', blocks: [ref(block)], kind: 'paragraph' }], focus: { block: block.ref.id, offset: 0 } };
  }
  const [head, tail] = splitRuns(runs, at);
  const id = newId('block');
  const next = CONTINUES.includes(kind) ? kind : 'paragraph';
  const ops = unit([
    setBlock(block, { ...textContent(kind, head, block.content), ...keepFields(block.content) }),
    { op: 'create_block', ref: 'split', proposed_id: id, place: { page: pageRef(view, block.page), parent: parentRef(view, block), after: ref(block) }, content: textContent(next, tail) },
  ], [check(view, block)]);
  return { ops, focus: { block: id, offset: 0 } };
}

// keepFields retains the kind's own fields of the block being split.
function keepFields(c) {
  const out = {};
  if (c.kind === 'todo') out.checked = c.checked;
  if (c.kind === 'toggle') out.open_default = c.open_default;
  if (c.kind === 'callout') out.tone = c.tone;
  return out;
}

// ---- Backspace ----

// backspace plans Backspace at the start of a text block. It merges into
// the previous block in document order when both hold rich text, moving
// the block's children with it; otherwise it selects the previous block.
export function backspace(view, block, runs) {
  const prev = previousInOrder(view, block);
  if (!prev) return null;
  if (!isText(prev.content.kind)) return { select: [prev.ref.id] };
  const offset = textLength(prev.content.text);
  const merged = { ...prev.content, text: concatRuns(prev.content.text, runs) };
  const ops = [setBlock(prev, merged)];
  const kids = view.blocks(block.page, block.ref.id);
  if (kids.length) {
    const place = PARENT_KINDS.includes(prev.content.kind)
      ? { page: pageRef(view, prev.page), parent: ref(prev), before: null }
      : { page: pageRef(view, prev.page), parent: parentRef(view, prev), after: ref(prev) };
    ops.push({ op: 'move_blocks', blocks: kids.map(ref), place });
  }
  ops.push({ op: 'delete_blocks', blocks: [ref(block)] });
  const before = previousSibling(view, block);
  const structure = [check(view, block, before ? { after: ref(before) } : {})];
  if (!before || before.ref.id !== prev.ref.id) structure.push(check(view, prev));
  return { ops: unit(ops, structure), focus: { block: prev.ref.id, offset } };
}

// ---- Tab and Shift+Tab ----

// indent nests blocks under the preceding sibling when it holds children
// and the result stays within the depth limit.
export function indent(view, blocks) {
  const first = blocks[0];
  const prev = previousSibling(view, first);
  if (!prev || !PARENT_KINDS.includes(prev.content.kind)) return null;
  if (blocks.some((b) => b.page !== first.page || (b.parent || null) !== (first.parent || null))) return null;
  const depth = view.depth(prev) + Math.max(...blocks.map((b) => subtreeHeight(view, b)));
  if (depth > MAX_BLOCK_DEPTH) return { refused: 'depth' };
  const ops = unit([{ op: 'move_blocks', blocks: blocks.map(ref), place: { page: pageRef(view, first.page), parent: ref(prev), before: null } }],
    [check(view, first, { after: ref(prev) })]);
  return { ops, focus: { block: first.ref.id } };
}

// outdent moves blocks out of their parent to just after it.
export function outdent(view, blocks) {
  const first = blocks[0];
  if (!first.parent) return null;
  if (blocks.some((b) => b.page !== first.page || b.parent !== first.parent)) return null;
  const parent = view.get(first.parent);
  const ops = unit([{ op: 'move_blocks', blocks: blocks.map(ref), place: { page: pageRef(view, first.page), parent: parentRef(view, parent), after: ref(parent) } }],
    [check(view, first), check(view, parent)]);
  return { ops, focus: { block: first.ref.id } };
}

// ---- kinds ----

// turnTargets is the intersection of legal targets for the selected blocks
// (rule 3): text kinds convert among themselves and to code, code converts
// to text kinds, and nothing else converts.
export function turnTargets(blocks) {
  const sets = blocks.map((b) => {
    const k = b.content.kind;
    if (isText(k)) return [...TEXT_KINDS, 'code'];
    if (k === 'code') return [...TEXT_KINDS];
    return [];
  });
  if (!sets.length) return [];
  const all = sets.reduce((a, s) => a.filter((k) => s.includes(k)));
  return all.filter((k) => !blocks.every((b) => b.content.kind === k));
}

export function turn(blocks, kind) {
  return [{ op: 'turn_blocks', blocks: blocks.map(ref), kind }];
}

// shortcutKind plans a block-start Markdown shortcut that turned the
// whole block text into the given kind. A divider cannot be reached by
// conversion, so the divider is created before the now empty block.
export function shortcut(view, block, kind) {
  if (kind === 'divider') {
    const ops = unit([
      { op: 'create_block', ref: 'divider', proposed_id: newId('block'), place: { page: pageRef(view, block.page), parent: parentRef(view, block), before: ref(block) }, content: { kind: 'divider' } },
      setBlock(block, textContent(block.content.kind, [], block.content)),
    ], [check(view, block)]);
    return { ops, focus: { block: block.ref.id, offset: 0 } };
  }
  const content = kind === 'code' ? { kind: 'code', text: '', language: '' } : textContent(kind, []);
  return { ops: [setBlock(block, content)], focus: { block: block.ref.id, offset: 0 } };
}

// ---- multi-block actions ----

// roots drops blocks whose ancestor is also selected and keeps document
// order, the selection rule of move, delete and duplicate.
export function roots(view, ids) {
  const set = new Set(ids);
  const out = [];
  for (const id of ids) {
    const b = view.get(id);
    if (!b) continue;
    let covered = false;
    for (let p = b.parent && view.get(b.parent); p; p = p.parent && view.get(p.parent)) if (set.has(p.ref.id)) { covered = true; break; }
    if (!covered) out.push(b);
  }
  const order = new Map(view.order(out[0] ? out[0].page : null).map((b, i) => [b.ref.id, i]));
  return out.sort((a, b) => (order.get(a.ref.id) ?? 0) - (order.get(b.ref.id) ?? 0));
}

export const remove = (blocks) => [{ op: 'delete_blocks', blocks: blocks.map(ref) }];

export const duplicate = (blocks) => [{ op: 'duplicate_blocks', blocks: blocks.map(ref), after: ref(blocks.at(-1)) }];

// moveBy moves the selected sibling run one position up or down among its
// siblings with the keyboard.
export function moveBy(view, blocks, dir) {
  const first = blocks[0], last = blocks.at(-1);
  const sibs = siblings(view, first);
  const i = sibs.findIndex((b) => b.ref.id === first.ref.id), j = sibs.findIndex((b) => b.ref.id === last.ref.id);
  const place = { page: pageRef(view, first.page), parent: parentRef(view, first) };
  if (dir < 0) {
    if (i <= 0) return null;
    place.before = ref(sibs[i - 1]);
  } else {
    if (j < 0 || j + 1 >= sibs.length) return null;
    place.after = ref(sibs[j + 1]);
  }
  return [{ op: 'move_blocks', blocks: blocks.map(ref), place }];
}

// moveToPage moves blocks to the end of another page.
export const moveToPage = (view, blocks, page) => [{ op: 'move_blocks', blocks: blocks.map(ref), place: { page: pageRef(view, page), parent: null, before: null } }];

// moveTo drops blocks before or after a target block, or into it as its
// last child.
export function moveTo(view, blocks, target, where) {
  const place = { page: pageRef(view, target.page) };
  if (where === 'inside') { place.parent = ref(target); place.before = null; }
  else { place.parent = parentRef(view, target); place[where] = ref(target); }
  return [{ op: 'move_blocks', blocks: blocks.map(ref), place }];
}

// ---- creation ----

export function createBlock(view, after, content, { page } = {}) {
  const id = newId('block');
  const place = after
    ? { page: pageRef(view, after.page), parent: parentRef(view, after), after: ref(after) }
    : { page: ref(page), parent: null, before: null };
  return { ops: [{ op: 'create_block', ref: 'new', proposed_id: id, place, content }], id };
}

// replaceWith plans a slash-menu choice that creates a non-convertible
// block: the new block goes after the current one, and the current block
// is deleted when it held nothing but the slash query.
export function replaceWith(view, block, create, empty) {
  const ops = [...create];
  if (empty) ops.push({ op: 'delete_blocks', blocks: [ref(block)] });
  return ops.length > 1 ? unit(ops, [check(view, block)]) : ops;
}

export function newPageLink(view, block, title = '') {
  const page = newId('page'), link = newId('block');
  const parent = view.get(block.page);
  return {
    ops: [
      { op: 'create_page', ref: 'page', proposed_id: page, parent: ref(parent), before: null, title },
      { op: 'create_block', ref: 'link', proposed_id: link, place: { page: pageRef(view, block.page), parent: parentRef(view, block), after: ref(block) }, content: { kind: 'page_link', target: page } },
    ],
    page, link,
  };
}

export function newDatabase(view, block) {
  const ids = { database: newId('database'), block: newId('block'), title: newId('property'), view: newId('view') };
  return {
    ops: [{
      op: 'create_database', ref: 'db', proposed_id: ids.database, block_ref: 'db-block', block_proposed_id: ids.block,
      place: { page: pageRef(view, block.page), parent: parentRef(view, block), after: ref(block) },
      title: '', title_property_proposed_id: ids.title, view_proposed_id: ids.view,
    }],
    ids,
  };
}

export function createPage(view, parent, title = '') {
  const id = newId('page');
  return { ops: [{ op: 'create_page', ref: 'page', proposed_id: id, parent: parent ? ref(parent) : null, before: null, title }], id };
}

// keepMyText plans Keep my text: a new block with the retained content
// right after the original, or at the end of a surviving page when the
// original is gone. It never writes into the contested group (rule 31).
export function keepMyText(view, original, content, fallbackPage) {
  const c = isText(content.kind) || content.kind === 'code' ? content : convertContent(content, 'paragraph');
  if (original && view.get(original)) return createBlock(view, view.get(original), c);
  return createBlock(view, null, c, { page: view.get(fallbackPage) || view.firstPage() });
}
