// Linear-time inline parsing for the editor's Markdown parser.
//
// CodeMirror reparses the edited paragraph after every change, so the cost of
// parsing one paragraph is paid on every keystroke, local or remote. In
// @lezer/markdown 1.7.2 several inline steps cost time proportional to
// everything before (or after) them in the paragraph, so a paragraph of n
// such items costs O(n^2): a guest typing one froze every editor showing the
// document. This module replaces those steps with versions that give the
// same syntax tree in linear time (markdown-inline.test.js compares trees
// with the stock parser):
//
//   - Closing a link. LinkEnd scans back over the paragraph's inline parts
//     for the nearest "[" or "![" and, after making a link, walks every
//     earlier part to mark older "[" as unusable (a link may not contain a
//     link); a "]" with no "[" scans the whole paragraph, and GFM's Autolink
//     asks hasOpenLink, which does the same. Here the open brackets live in
//     a stack, and "older [ are unusable" is a position watermark.
//   - Matching emphasis. resolveMarkers looks back from every closing "*",
//     "_" or "~~" over all earlier parts; a closer that matches nothing (as
//     in "a* a* a*") scans to the paragraph start each time, and a leftover
//     closer splices into the parts array. Here delimiters live in linked
//     lists, and a closer that finds no opener records, per delimiter kind,
//     that nothing before it can open for that kind (CommonMark's "openers
//     bottom"), lowered again when an opener before it shrinks.
//   - Link destinations and titles. After each "](" lezer scans on for the
//     ")" that ends the destination or title; when there is none (as in
//     "[x](" repeated without spaces) every scan runs to the end of the
//     space-free run or paragraph. Here a run's parenthesis depths are
//     computed once, and title searches remember their answer.
//   - Unclosed HTML. An inline "<?", "<!X" or "<![CDATA[" searches the rest
//     of the paragraph for its terminator; here the search result is
//     remembered, since every later search starts further on.
//
// Even linear, inline parsing a huge paragraph after every change is too
// much work, so a paragraph longer than PLAIN_PARAGRAPH characters is not
// inline parsed at all: it shows as its Markdown source (spec rule 21b,
// TECH 3.11). Paragraphs up to that length parse exactly as before.
//
// The private lezer helpers these need (finishLink and the link parsers)
// are copied from lezer; node_modules is not modified.
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { Element, InlineContext, parser } from "@lezer/markdown";

const LinkStart = InlineContext.linkStart,
  ImageStart = InlineContext.imageStart,
  state = Symbol("linkStack");

const isOpener = (part) =>
  part != null && (part.type === LinkStart || part.type === ImageStart);

// stack returns the context's opener stack, adding the parts appended since
// the last call. Parts only grow at the end between calls (inline parsers
// append; only LinkEnd removes or replaces parts), so the scan is amortized
// over the paragraph. If the parts changed some other way the stack is
// rebuilt, which keeps it exact at the cost of one scan.
function stack(cx) {
  let s = cx[state];
  if (!s) {
    s = cx[state] = { opens: [], scanned: 0, invalidBefore: -1 };
    // GFM's Autolink reads hasOpenLink; answer it from the stack.
    Object.defineProperty(cx, "hasOpenLink", {
      get: () => stack(cx).opens.length > 0,
    });
  }
  const parts = cx.parts,
    top = s.opens[s.opens.length - 1];
  if (
    s.scanned > parts.length ||
    (top !== undefined && !isOpener(parts[top]))
  ) {
    s.opens = [];
    s.scanned = 0;
  }
  for (let i = s.scanned; i < parts.length; i++)
    if (isOpener(parts[i])) s.opens.push(i);
  s.scanned = parts.length;
  return s;
}

function linkEnd(cx, next, start) {
  if (next != 93 /* ']' */) return -1;
  const s = stack(cx);
  if (!s.opens.length) return -1;
  const i = s.opens[s.opens.length - 1],
    part = cx.parts[i];
  // A "[" before an earlier link is unusable (no links inside links), as is
  // one lezer marked so; an empty "[]" not followed by "(" or "[" is text.
  // Either way both brackets stay text.
  if (
    !part.side ||
    (part.type === LinkStart && part.from < s.invalidBefore) ||
    (cx.skipSpace(part.to) == start &&
      !/[(\[]/.test(cx.slice(start + 1, start + 2)))
  ) {
    cx.parts[i] = null;
    s.opens.pop();
    return -1;
  }
  const content = cx.takeContent(i),
    link = (cx.parts[i] = finishLink(
      cx,
      content,
      part.type === LinkStart ? "Link" : "Image",
      part.from,
      start + 1,
    ));
  s.opens.pop();
  s.scanned = cx.parts.length;
  if (part.type === LinkStart)
    s.invalidBefore = Math.max(s.invalidBefore, part.from);
  return link.to;
}

function finishLink(cx, content, type, start, startPos) {
  const text = cx.text,
    offset = cx.offset,
    next = cx.char(startPos);
  let endPos = startPos;
  content.unshift(cx.elt("LinkMark", start, start + (type == "Image" ? 2 : 1)));
  content.push(cx.elt("LinkMark", startPos - 1, startPos));
  if (next == 40 /* '(' */) {
    let pos = cx.skipSpace(startPos + 1),
      title;
    const dest = parseURL(cx, text, pos - offset, offset);
    if (dest) {
      pos = cx.skipSpace(dest.to);
      // The destination and title must be separated by whitespace.
      if (pos != dest.to) {
        title = parseLinkTitle(cx, text, pos - offset, offset);
        if (title) pos = cx.skipSpace(title.to);
      }
    }
    if (cx.char(pos) == 41 /* ')' */) {
      content.push(cx.elt("LinkMark", startPos, startPos + 1));
      endPos = pos + 1;
      if (dest) content.push(dest);
      if (title) content.push(title);
      content.push(cx.elt("LinkMark", pos, endPos));
    }
  } else if (next == 91 /* '[' */) {
    const label = parseLinkLabel(cx, text, startPos - offset, offset);
    if (label) {
      content.push(label);
      endPos = label.to;
    }
  }
  return cx.elt(type, start, endPos, content);
}

const space = (ch) => ch == 32 || ch == 9 || ch == 10 || ch == 13;

function parseURL(cx, text, start, offset) {
  const next = text.charCodeAt(start);
  if (next == 60 /* '<' */) {
    for (let pos = start + 1; pos < text.length; pos++) {
      const ch = text.charCodeAt(pos);
      if (ch == 62 /* '>' */)
        return cx.elt("URL", start + offset, pos + 1 + offset);
      if (ch == 60 || ch == 10 /* '<\n' */) return false;
    }
    return null;
  }
  const pos = urlEnd(cx, start + offset) - offset;
  return pos > start
    ? cx.elt("URL", start + offset, pos + offset)
    : pos == text.length
      ? null
      : false;
}

// urlEnd returns where lezer's scan of a bare link destination starting at
// document position pos stops: the first space, the first ")" that would
// close more than the destination opened, or the end of the paragraph.
// Scanning on from every "](" of a paragraph such as "[x](" repeated would
// cost O(n^2), so the levels of one space-free run are computed once: the
// scan from pos stops at the ")" just before the first later point whose
// parenthesis depth is lower than at pos (the next smaller value).
// Destinations start after "(" or a space, never after a backslash, so a
// scan from any of them sees the same escapes as one from the run's start.
function urlEnd(cx, pos) {
  let run = cx[urlRun];
  if (
    !run ||
    pos < run.from ||
    pos >= run.to ||
    (pos > run.from && cx.char(pos - 1) != 40) /* '(' */
  ) {
    const text = cx.text,
      from = pos - cx.offset;
    let to = from;
    while (to < text.length && !space(text.charCodeAt(to))) to++;
    // level[k]: unescaped "(" minus ")" before from + k.
    const length = to - from,
      level = new Int32Array(length + 1),
      lower = new Int32Array(length + 1).fill(-1),
      stack = [];
    for (let k = 0, depth = 0, escaped = false; k <= length; k++) {
      level[k] = depth;
      while (stack.length && depth < level[stack[stack.length - 1]])
        lower[stack.pop()] = k;
      stack.push(k);
      if (k == length) break;
      const ch = text.charCodeAt(from + k);
      if (escaped) escaped = false;
      else if (ch == 40 /* '(' */) depth++;
      else if (ch == 41 /* ')' */) depth--;
      else if (ch == 92 /* '\\' */) escaped = true;
    }
    run = cx[urlRun] = {
      from: pos,
      to: to + cx.offset,
      lower,
    };
  }
  const k = run.lower[pos - run.from];
  return k < 0 ? run.to : run.from + k - 1;
}
const urlRun = Symbol("urlRun");

function parseLinkTitle(cx, text, start, offset) {
  const next = text.charCodeAt(start);
  if (next != 39 && next != 34 && next != 40 /* '"\'(' */) return false;
  const at = unescaped(cx, next == 40 ? 41 : next, start + offset + 1);
  return at < 0 ? null : cx.elt("LinkTitle", start + offset, at + 1);
}

// unescaped returns the document position of the first ch at or after pos
// not escaped by a backslash, or -1, remembering the answer as find does.
// Whether a character is escaped depends only on the backslashes right
// before it, and a title search starts after its opening quote, so the
// answer does not depend on where the search started.
function unescaped(cx, ch, pos) {
  const memo = (cx[found] ??= new Map()),
    m = memo.get(ch);
  if (m && m.pos <= pos && (m.at < 0 || m.at >= pos)) return m.at;
  const text = cx.text,
    from = pos - cx.offset,
    needle = String.fromCharCode(ch);
  let at = -1;
  for (let i = from; ; ) {
    const j = text.indexOf(needle, i);
    if (j < 0) break;
    let backslashes = 0;
    while (j - backslashes > from && text.charCodeAt(j - backslashes - 1) == 92)
      backslashes++;
    if (backslashes % 2 == 0) {
      at = j + cx.offset;
      break;
    }
    i = j + 1;
  }
  memo.set(ch, { pos, at });
  return at;
}

function parseLinkLabel(cx, text, start, offset) {
  for (
    let escaped = false,
      pos = start + 1,
      end = Math.min(text.length, pos + 999);
    pos < end;
    pos++
  ) {
    const ch = text.charCodeAt(pos);
    if (escaped) escaped = false;
    else if (ch == 93 /* ']' */)
      return cx.elt("LinkLabel", start + offset, pos + 1 + offset);
    else if (ch == 91 /* '[' */) return false;
    else if (ch == 92 /* '\\' */) escaped = true;
  }
  return null;
}

// The delimiter kinds lezer's emphasis rules apply to ("*" and "_"). They
// are private; take them from delimiters the stock Emphasis parser makes.
const [asterisk, underscore] = (() => {
  const cx = new InlineContext(parser, "*a_", 0),
    emphasis = parser.inlineParsers[parser.inlineNames.indexOf("Emphasis")];
  emphasis(cx, 42, 0);
  emphasis(cx, 95, 2);
  return cx.parts.map((p) => p.type);
})();
if (!asterisk || !underscore || asterisk === underscore)
  throw new Error("unexpected @lezer/markdown emphasis delimiters");

// resolveMarkers replaces InlineContext.resolveMarkers on the editor's inline
// contexts. Like lezer's, it pairs each closing delimiter, left to right,
// with the nearest earlier opener of its kind that the emphasis rules allow,
// wraps what lies between, and returns the remaining elements from index
// from on. It reads this.parts from there on but does not rewrite it: its
// callers (takeContent and the end of parseInline) discard those parts.
function resolveMarkers(from) {
  const parts = this.parts;
  // items: elements and pairable delimiters in order (next/prev);
  // delimiters also form their own list (dnext/dprev) for the look-back.
  let first = null,
    last = null,
    dfirst = null,
    dlast = null;
  for (let i = from; i < parts.length; i++) {
    const p = parts[i];
    let item;
    if (p instanceof Element) item = { elt: p };
    else if (
      p != null &&
      typeof p.type == "object" &&
      p.type.resolve &&
      p.side & 3
    ) {
      item = { type: p.type, from: p.from, to: p.to, side: p.side };
      item.dprev = dlast;
      if (dlast) dlast.dnext = item;
      else dfirst = item;
      dlast = item;
    } else continue;
    item.prev = last;
    if (last) last.next = item;
    else first = item;
    last = item;
  }
  const unlink = (item) => {
    if (item.prev) item.prev.next = item.next;
    else first = item.next;
    if (item.next) item.next.prev = item.prev;
    else last = item.prev;
    if (item.dprev) item.dprev.dnext = item.dnext;
    else dfirst = item.dnext;
    if (item.dnext) item.dnext.dprev = item.dprev;
    else dlast = item.dprev;
  };
  // floors.get(kind)[key]: delimiters starting before it cannot open for a
  // closer of that kind and key (whether it can also open, its size mod 3).
  // Only a failed look-back raises a floor; an opener that shrinks lowers
  // every floor to itself, since its size enters the rule of three.
  const floors = new Map();
  for (let close = dfirst; close; ) {
    if (!((close.side & 2) /* close */)) {
      close = close.dnext;
      continue;
    }
    const emp = close.type === asterisk || close.type === underscore,
      closeSize = close.to - close.from,
      key = (close.side & 1 ? 3 : 0) + (closeSize % 3);
    let floor = floors.get(close.type);
    if (!floor) floors.set(close.type, (floor = [-1, -1, -1, -1, -1, -1]));
    let open = close.dprev;
    for (; open && open.from >= floor[key]; open = open.dprev) {
      const openSize = open.to - open.from;
      if (
        open.side & 1 /* open */ &&
        open.type === close.type &&
        !(
          emp &&
          (close.side & 1 || open.side & 2) &&
          (openSize + closeSize) % 3 == 0 &&
          (openSize % 3 || closeSize % 3)
        )
      )
        break;
    }
    if (!open || open.from < floor[key]) {
      floor[key] = close.from;
      const next = close.dnext;
      // A closer that cannot open is never matched again.
      if (!(close.side & 1)) unlink(close);
      close = next;
      continue;
    }
    let type = close.type.resolve,
      start = open.from,
      end = close.to;
    // Emphasis consumes at most two marker characters from each side.
    if (emp) {
      const size = Math.min(2, open.to - open.from, closeSize);
      start = open.to - size;
      end = close.from + size;
      type = size == 1 ? "Emphasis" : "StrongEmphasis";
    }
    const content = [];
    if (open.type.mark) content.push(this.elt(open.type.mark, start, open.to));
    for (let item = open.next; item !== close; item = item.next)
      if (item.elt) content.push(item.elt);
      else unlink(item);
    if (close.type.mark)
      content.push(this.elt(close.type.mark, close.from, end));
    const element = { elt: this.elt(type, start, end, content) };
    // The element replaces everything between the delimiters.
    element.prev = open;
    element.next = close;
    open.next = close.prev = element;
    if (emp && open.from != start) {
      open.to = start;
      for (const f of floors.values())
        for (let k = 0; k < 6; k++) f[k] = Math.min(f[k], open.from);
    } else unlink(open);
    if (emp && close.to != end) {
      // The leftover closer is matched again.
      close.from = end;
    } else {
      const next = close.dnext;
      unlink(close);
      close = next;
    }
  }
  const result = [];
  for (let item = first; item; item = item.next)
    if (item.elt) result.push(item.elt);
  return result;
}

// find returns the document position of the first needle at or after pos in
// cx, or -1. Inline parsing moves forward, so a result found earlier still
// answers a later search that starts at or before it.
function find(cx, needle, pos) {
  const memo = (cx[found] ??= new Map()),
    m = memo.get(needle);
  if (m && m.pos <= pos && (m.at < 0 || m.at >= pos)) return m.at;
  const i = cx.text.indexOf(needle, pos - cx.offset),
    at = i < 0 ? -1 : i + cx.offset;
  memo.set(needle, { pos, at });
  return at;
}
const found = Symbol("found");

const urlTag =
    /^(?:[a-z][-\w+.]+:[^\s>]+|[a-z\d.!#$%&'*+/=?^_`{|}~-]+@[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?(?:\.[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?)*)>/i,
  commentTag = /^!--[^>](?:-[^-]|[^-])*?-->/i,
  // lezer's tag pattern without its first two alternatives ("<!X...>" and
  // "<![CDATA[...]]>"), which htmlTag finds with find.
  otherTag =
    /^(?:\/\s*[a-zA-Z][\w-]*\s*>|\s*[a-zA-Z][\w-]*(\s+[a-zA-Z:_][\w-.:]*(?:\s*=\s*(?:[^\s"'=<>`]+|'[^']*'|"[^"]*"))?)*\s*(\/\s*)?>)/;

// htmlTag is lezer's HTMLTag parser with the unbounded lazy searches
// ("<?...?>", "<!X...>", "<![CDATA[...]]>") answered by find.
function htmlTag(cx, next, start) {
  if (next != 60 /* '<' */ || start == cx.end - 1) return -1;
  const after = cx.slice(start + 1, cx.end);
  const url = urlTag.exec(after);
  if (url)
    return cx.append(
      cx.elt("Autolink", start, start + 1 + url[0].length, [
        cx.elt("LinkMark", start, start + 1),
        cx.elt("URL", start + 1, start + url[0].length),
        cx.elt("LinkMark", start + url[0].length, start + 1 + url[0].length),
      ]),
    );
  const comment = commentTag.exec(after);
  if (comment)
    return cx.append(cx.elt("Comment", start, start + 1 + comment[0].length));
  const c1 = after.charCodeAt(0),
    c2 = after.charCodeAt(1);
  if (c1 == 63 /* '?' */) {
    const at = find(cx, "?>", start + 2);
    // Nothing else lezer accepts starts with "?".
    return at < 0
      ? -1
      : cx.append(cx.elt("ProcessingInstruction", start, at + 2));
  }
  let end = -1;
  if (c1 == 33 /* '!' */ && c2 >= 65 && c2 <= 90 /* A-Z */) {
    const at = find(cx, ">", start + 3);
    end = at < 0 ? -1 : at + 1;
  } else if (after.startsWith("![CDATA[")) {
    const at = find(cx, "]]>", start + 9);
    end = at < 0 ? -1 : at + 3;
  } else {
    const m = otherTag.exec(after);
    end = m ? start + 1 + m[0].length : -1;
  }
  return end < 0 ? -1 : cx.append(cx.elt("HTMLTag", start, end));
}

// install runs first at every inline position and readies a new inline
// context: the bracket stack answers hasOpenLink, and emphasis resolves in
// linear time.
function install(cx) {
  if (!cx[state]) {
    stack(cx);
    cx.resolveMarkers = resolveMarkers;
  }
  return -1;
}

// PLAIN_PARAGRAPH is the longest paragraph, in characters of its text
// (without container markers such as "> "), that is inline parsed.
export const PLAIN_PARAGRAPH = 20000;

// plainLong finishes a paragraph longer than PLAIN_PARAGRAPH without its
// inline content: only container marks (as in a quote) remain inside. A
// task list item keeps its Task node and marker. Leaf parsers before it
// (reference definitions, tables, setext headings) still decide first.
const plainLong = {
  nextLine: () => false,
  finish(cx, leaf) {
    if (leaf.content.length <= PLAIN_PARAGRAPH) return false;
    const end = leaf.start + leaf.content.length;
    cx.addLeafElement(
      leaf,
      /^\[[ xX]\][ \t]/.test(leaf.content) && cx.parentType().name == "ListItem"
        ? cx.elt("Task", leaf.start, end, [
            cx.elt("TaskMarker", leaf.start, leaf.start + 3),
          ])
        : cx.elt("Paragraph", leaf.start, end),
    );
    return true;
  },
};

export const plainLongParagraphs = {
  parseBlock: [{ name: "PlainLongParagraph", leaf: () => plainLong }],
};

export const linearInline = {
  parseInline: [
    { name: "LinearInline", parse: install, before: "Escape" },
    { name: "HTMLTag", parse: htmlTag },
    { name: "LinkEnd", parse: linkEnd },
  ],
};

// editorMarkdown is the editor's Markdown language: GFM with linear-time
// inline parsing and plain long paragraphs.
export const editorMarkdown = () =>
  markdown({
    base: markdownLanguage,
    extensions: [linearInline, plainLongParagraphs],
  });
