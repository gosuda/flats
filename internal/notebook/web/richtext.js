// Rich text runs (TECH 4.2, rule 4): an ordered list of {text, marks, link?}.
// Offsets in this module count Unicode code points, the unit of the server's
// limits; DOM selection offsets count UTF-16 code units and are converted
// at the boundary (cpToUnits, unitsToCP, domToCP, cpToDom).
'use strict';


export const MARKS = ['bold', 'italic', 'strike', 'code'];

export function cpLength(s) {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      const d = s.charCodeAt(i + 1);
      if (d >= 0xdc00 && d <= 0xdfff) i++;
    }
    n++;
  }
  return n;
}

// cpToUnits returns the UTF-16 index of code point offset cp in s, clamped.
export function cpToUnits(s, cp) {
  let i = 0;
  for (let n = 0; n < cp && i < s.length; n++) {
    const c = s.charCodeAt(i);
    i += c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00 ? 2 : 1;
  }
  return i;
}

// unitsToCP returns the code point offset of UTF-16 index u in s. An index
// inside a surrogate pair counts the whole pair as before it.
export function unitsToCP(s, u) {
  let n = 0;
  for (let i = 0; i < u && i < s.length; n++) {
    const c = s.charCodeAt(i);
    i += c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00 ? 2 : 1;
  }
  return n;
}

const cpSlice = (s, from, to = Infinity) => s.slice(cpToUnits(s, from), to === Infinity ? s.length : cpToUnits(s, to));

const sameLink = (a, b) => (a == null && b == null) || (a != null && b != null && a.url === b.url && a.page === b.page);
const sameMarks = (a, b) => a.length === b.length && a.every((m, i) => m === b[i]);
const orderMarks = (marks) => MARKS.filter((m) => marks.includes(m));

function run(text, marks = [], link) {
  const r = { text, marks: orderMarks(marks) };
  if (link) r.link = { ...link };
  return r;
}

// canonicalize mirrors the server: marks in canonical order, empty runs
// removed, neighbors with equal marks and link coalesced.
export function canonicalize(runs) {
  const out = [];
  for (const r of runs || []) {
    if (!r.text) continue;
    const next = run(r.text, r.marks || [], r.link);
    const last = out.at(-1);
    if (last && sameMarks(last.marks, next.marks) && sameLink(last.link, next.link)) last.text += next.text;
    else out.push(next);
  }
  return out;
}

export const plainText = (runs) => (runs || []).map((r) => r.text).join('');
export const textLength = (runs) => (runs || []).reduce((n, r) => n + cpLength(r.text), 0);

export function sameRuns(a, b) {
  const x = canonicalize(a), y = canonicalize(b);
  return x.length === y.length && x.every((r, i) => r.text === y[i].text && sameMarks(r.marks, y[i].marks) && sameLink(r.link, y[i].link));
}

export function sliceRuns(runs, from, to = Infinity) {
  const out = [];
  let n = 0;
  for (const r of runs || []) {
    const len = cpLength(r.text);
    const a = Math.max(from, n), b = Math.min(to, n + len);
    if (b > a) out.push(run(cpSlice(r.text, a - n, b - n), r.marks, r.link));
    n += len;
  }
  return canonicalize(out);
}

export const splitRuns = (runs, at) => [sliceRuns(runs, 0, at), sliceRuns(runs, at)];
export const concatRuns = (a, b) => canonicalize([...(a || []), ...(b || [])]);
export const deleteRange = (runs, from, to) => concatRuns(sliceRuns(runs, 0, from), sliceRuns(runs, to));

// styleAt is the style new text typed at offset at receives: the marks of
// the character before it (after it at the start). A link continues only
// strictly inside it, so typing after a link does not extend the link.
export function styleAt(runs, at) {
  const total = textLength(runs);
  if (!total) return { marks: [] };
  const before = at > 0 ? sliceRuns(runs, at - 1, at)[0] : null;
  const after = at < total ? sliceRuns(runs, at, at + 1)[0] : null;
  const src = before || after;
  const style = { marks: [...src.marks] };
  if (before && after && before.link && sameLink(before.link, after.link)) style.link = before.link;
  return style;
}

export function insertText(runs, at, text, style = styleAt(runs, at)) {
  const [a, b] = splitRuns(runs, at);
  return canonicalize([...a, run(text, style.marks, style.link), ...b]);
}

export function replaceRange(runs, from, to, text, style) {
  const st = style || styleAt(deleteRange(runs, from, to), from);
  return insertText(deleteRange(runs, from, to), from, text, st);
}

// toggleMark removes mark from [from, to) when every character there has
// it, and adds it otherwise.
export function toggleMark(runs, from, to, mark) {
  if (to <= from) return canonicalize(runs);
  const mid = sliceRuns(runs, from, to);
  const all = mid.every((r) => r.marks.includes(mark));
  const changed = mid.map((r) => run(r.text, all ? r.marks.filter((m) => m !== mark) : [...r.marks, mark], r.link));
  return canonicalize([...sliceRuns(runs, 0, from), ...changed, ...sliceRuns(runs, to)]);
}

// diffRuns maps edited plain text back onto runs: the common prefix and
// suffix keep their styles, and the replaced middle takes the style at the
// prefix end. Composition and the input fallback reconcile through it.
export function diffRuns(runs, text) {
  const old = plainText(runs);
  if (old === text) return canonicalize(runs);
  const a = [...old], b = [...text];
  let p = 0;
  while (p < a.length && p < b.length && a[p] === b[p]) p++;
  let s = 0;
  while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;
  return replaceRange(runs, p, a.length - s, b.slice(p, b.length - s).join(''));
}

// mapOffset carries a caret through a same-group replacement with a
// bounded common-prefix and common-suffix mapping, then clamps (TECH 18).
export function mapOffset(oldText, newText, at) {
  const a = [...oldText], b = [...newText];
  let p = 0;
  while (p < a.length && p < b.length && a[p] === b[p]) p++;
  if (at <= p) return Math.min(at, b.length);
  let s = 0;
  while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;
  if (at >= a.length - s) return Math.max(0, Math.min(b.length, b.length - (a.length - at)));
  return Math.min(at, b.length - s);
}

// ---- Markdown shortcuts (rule 19) ----

const BLOCK_SHORTCUTS = [
  ['# ', 'heading1'], ['## ', 'heading2'], ['### ', 'heading3'], ['- ', 'bullet'], ['* ', 'bullet'],
  ['1. ', 'numbered'], ['[] ', 'todo'], ['> ', 'quote'], ['```', 'code'], ['---', 'divider'],
];

// blockShortcut returns the kind a block whose whole text is now text turns
// into, or null. Shortcuts apply only to an otherwise empty block.
export function blockShortcut(text) {
  for (const [prefix, kind] of BLOCK_SHORTCUTS) if (text === prefix) return kind;
  return null;
}

const INLINE = [['**', 'bold'], ['~~', 'strike'], ['`', 'code'], ['*', 'italic']];

// setMark adds mark to every character in [from, to).
export function setMark(runs, from, to, mark) {
  const mid = sliceRuns(runs, from, to).map((r) => run(r.text, [...r.marks, mark], r.link));
  return canonicalize([...sliceRuns(runs, 0, from), ...mid, ...sliceRuns(runs, to)]);
}

const at = (chars, i, delim) => [...delim].every((c, j) => chars[i + j] === c);

// inlineShortcut applies a mark when the text before the caret ends with a
// closed pair such as **text**. It returns {runs, caret} or null.
export function inlineShortcut(runs, caret) {
  const before = [...plainText(sliceRuns(runs, 0, caret))];
  for (const [delim, mark] of INLINE) {
    const d = delim.length, end = before.length - d;
    if (end < d + 1 || !at(before, end, delim)) continue;
    let start = -1;
    for (let i = end - d - 1; i >= 0; i--) if (at(before, i, delim)) { start = i; break; }
    if (start < 0) continue;
    const inner = before.slice(start + d, end);
    if (/\s/.test(inner[0]) || /\s/.test(inner.at(-1))) continue;
    if (delim === '*' && (before[start - 1] === '*' || inner[0] === '*' || inner.at(-1) === '*')) continue;
    let out = deleteRange(runs, end, end + d);
    out = deleteRange(out, start, start + d);
    return { runs: setMark(out, start, start + inner.length, mark), caret: caret - 2 * d };
  }
  return null;
}

// ---- DOM rendering and offsets ----

export const SAFE_URL = /^(https?:|mailto:)/i;

// renderRuns builds DOM nodes for runs. Text always goes through text
// nodes; a URL link opens in a new tab without a referrer, a page link
// stays in the fragment router (TECH 15, rule 60).
export function renderRuns(runs, doc = document) {
  return canonicalize(runs).map((r) => {
    let el = doc.createTextNode(r.text);
    const wrap = (tag) => { const w = doc.createElement(tag); w.appendChild(el); el = w; };
    if (r.marks.includes('code')) wrap('code');
    if (r.marks.includes('strike')) wrap('s');
    if (r.marks.includes('italic')) wrap('em');
    if (r.marks.includes('bold')) wrap('strong');
    if (r.link && r.link.page) {
      wrap('a');
      el.setAttribute('href', '#/' + r.link.page);
      el.setAttribute('data-page', r.link.page);
    } else if (r.link && r.link.url && SAFE_URL.test(r.link.url)) {
      wrap('a');
      el.setAttribute('href', r.link.url);
      el.setAttribute('target', '_blank');
      el.setAttribute('rel', 'noopener noreferrer');
      el.setAttribute('referrerpolicy', 'no-referrer');
      el.className = 'ext';
    }
    return el;
  });
}

const TEXT = 3, ELEMENT = 1;
const skipped = (n) => n.nodeType === ELEMENT && n.getAttribute('contenteditable') === 'false';
const kids = (n) => Array.from(n.childNodes || []);
const nodeText = (n) => (n.nodeType === TEXT ? n.data : '\n');

// pieces lists the editable text of root in document order: text nodes,
// and <br> line breaks that browser-edited DOM may contain. Children marked
// contenteditable=false (labels inside a block) are not text.
function pieces(root) {
  const out = [];
  const walk = (n) => {
    for (const c of kids(n)) {
      if (c.nodeType === TEXT) out.push(c);
      else if (c.nodeType === ELEMENT && !skipped(c)) {
        if (c.tagName === 'BR') out.push(c);
        else walk(c);
      }
    }
  };
  walk(root);
  return out;
}

export const domText = (root) => pieces(root).map(nodeText).join('');

// domToCP converts the DOM point (node, offset) inside root to a code point
// offset: a text node offset counts UTF-16 units, an element offset counts
// child nodes.
export function domToCP(root, node, offset) {
  let n = 0;
  let found = null;
  const walk = (el) => {
    const list = kids(el);
    for (let i = 0; i <= list.length; i++) {
      if (el === node && i === offset) { found = n; return true; }
      const c = list[i];
      if (!c) break;
      if (c.nodeType === TEXT) {
        if (c === node) { found = n + unitsToCP(c.data, offset); return true; }
        n += cpLength(c.data);
      } else if (c.nodeType === ELEMENT && !skipped(c)) {
        if (c.tagName === 'BR') n += 1;
        else if (walk(c)) return true;
      }
    }
    return false;
  };
  walk(root);
  return found ?? n;
}

// cpToDom returns the DOM point for code point offset cp, at the end of the
// earlier text node on a run boundary so typing continues that run.
export function cpToDom(root, cp) {
  let left = cp;
  let last = null;
  for (const t of pieces(root)) {
    if (t.nodeType !== TEXT) {
      if (left === 0) return { node: t.parentNode, offset: kids(t.parentNode).indexOf(t) };
      left -= 1;
      continue;
    }
    const len = cpLength(t.data);
    if (left <= len) return { node: t, offset: cpToUnits(t.data, left) };
    left -= len;
    last = t;
  }
  if (last) return { node: last, offset: last.data.length };
  return { node: root, offset: kids(root).length };
}
