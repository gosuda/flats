// Linear-time link parsing for the editor's Markdown parser.
//
// @lezer/markdown 1.7.2 closes a link in time proportional to everything
// before it in the paragraph: its LinkEnd parser scans back over the
// paragraph's inline parts for the nearest "[" or "![" and, after making a
// link, walks every earlier part to mark older "[" as unusable (a link may
// not contain a link). A "]" with no "[" scans the whole paragraph, and
// GFM's Autolink asks hasOpenLink, which does the same. One paragraph of n
// bracketed words, links, stray "]" or www. addresses therefore costs O(n^2)
// to parse, and CodeMirror reparses the edited paragraph after every change:
// a guest typing such a paragraph froze every editor showing the document.
//
// linearLinks replaces LinkEnd and wraps Autolink with versions that keep
// the open "[" and "![" positions in a stack beside the inline context, so
// each "]" and each autolink look only at the stack top. The parse result
// is the same as lezer's: the nearest opener is the stack top, and "older
// [ are unusable" becomes a position watermark instead of a walk. Tests in
// markdown-links.test.js compare trees with the stock parser.
//
// It must be used with a base that already has GFM's Autolink (the editor's
// markdownLanguage): configuring it over plain CommonMark would add
// autolinks. finishLink and its helpers follow lezer's private versions.
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { Autolink, InlineContext } from "@lezer/markdown";

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
  let depth = 0,
    pos = start;
  for (let escaped = false; pos < text.length; pos++) {
    const ch = text.charCodeAt(pos);
    if (space(ch)) break;
    else if (escaped) escaped = false;
    else if (ch == 40 /* '(' */) depth++;
    else if (ch == 41 /* ')' */) {
      if (!depth) break;
      depth--;
    } else if (ch == 92 /* '\\' */) escaped = true;
  }
  return pos > start
    ? cx.elt("URL", start + offset, pos + offset)
    : pos == text.length
      ? null
      : false;
}

function parseLinkTitle(cx, text, start, offset) {
  const next = text.charCodeAt(start);
  if (next != 39 && next != 34 && next != 40 /* '"\'(' */) return false;
  const end = next == 40 ? 41 : next;
  for (let pos = start + 1, escaped = false; pos < text.length; pos++) {
    const ch = text.charCodeAt(pos);
    if (escaped) escaped = false;
    else if (ch == end)
      return cx.elt("LinkTitle", start + offset, pos + 1 + offset);
    else if (ch == 92 /* '\\' */) escaped = true;
  }
  return null;
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

const autolink = Autolink.parseInline[0].parse;

export const linearLinks = {
  parseInline: [
    { name: "LinkEnd", parse: linkEnd },
    {
      name: "Autolink",
      parse(cx, next, pos) {
        // Install the stack-backed hasOpenLink before Autolink reads it.
        if (!cx[state]) stack(cx);
        return autolink(cx, next, pos);
      },
    },
  ],
};

// editorMarkdown is the editor's Markdown language: GFM with linear-time
// links.
export const editorMarkdown = () =>
  markdown({ base: markdownLanguage, extensions: linearLinks });
