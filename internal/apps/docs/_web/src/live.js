// Live preview for the single-view editor. The document stays plain Markdown
// in one Y.Text; this only decorates it. Formatting is shown rendered, and the
// Markdown syntax reappears on the lines the cursor or selection touches, so
// it can be edited where it is written.
import { syntaxTree } from "@codemirror/language";
import MarkdownIt from "markdown-it";

// Link and image destinations decode escapes and entities as markdown-it
// does, so the editor opens the same URL the rendered view links to.
const { unescapeAll } = new MarkdownIt().utils;

// Kinds of decoration. `line` styles a whole line, `mark` styles a range,
// `hide` removes syntax from view, and `bullet`, `rule` and `image` replace
// syntax with a widget.
const headings = {
  ATXHeading1: "h1",
  ATXHeading2: "h2",
  ATXHeading3: "h3",
  ATXHeading4: "h4",
  ATXHeading5: "h5",
  ATXHeading6: "h6",
  SetextHeading1: "h1",
  SetextHeading2: "h2",
};
const marks = {
  Emphasis: "em",
  StrongEmphasis: "strong",
  Strikethrough: "strike",
  InlineCode: "code",
};
const syntax = new Set([
  "EmphasisMark",
  "CodeMark",
  "StrikethroughMark",
  "QuoteMark",
]);

// Nodes whose URL child is not a bare address.
const linkParents = new Set(["Link", "Image", "Autolink", "LinkReference"]);

// lineNumbers returns the 1-based numbers of every line from..to touches.
function lineNumbers(doc, from, to) {
  const first = doc.lineAt(from).number,
    last = doc.lineAt(to).number,
    out = [];
  for (let n = first; n <= last; n++) out.push(n);
  return out;
}

// activeLines lists the [first, last] line-number spans whose syntax is
// shown: the lines each selection range touches while the editor has focus,
// and none otherwise. Spans keep a whole-document selection cheap.
export function activeLines(state, focused) {
  if (!focused) return [];
  return state.selection.ranges.map((r) => [
    state.doc.lineAt(r.from).number,
    state.doc.lineAt(r.to).number,
  ]);
}

// liveDecorations returns unsorted decoration descriptors for the given
// visible ranges. It is pure so it can be tested without a DOM. links holds
// the renderer's link rules (markdown.js): syntax the renderer leaves as text
// keeps its source here too, and a link's href is the URL the renderer uses.
export function liveDecorations(state, ranges, active, links) {
  const doc = state.doc,
    out = [],
    lined = new Set();
  const isActive = (from, to) => {
    const first = doc.lineAt(from).number,
      last = doc.lineAt(to).number;
    return active.some(([a, b]) => a <= last && b >= first);
  };
  // Only lines inside the visible range being walked are decorated, so a
  // long block costs no more than the part on screen.
  let visible;
  const line = (from, to, cls) => {
    from = Math.max(from, visible.from);
    to = Math.min(to, visible.to);
    if (to < from) return;
    for (const n of lineNumbers(doc, from, to)) {
      const key = n + " " + cls;
      if (lined.has(key)) continue;
      lined.add(key);
      out.push({ kind: "line", from: doc.line(n).from, cls });
    }
  };
  const linkMark = (from, to, href) => ({
    kind: "mark",
    from,
    to,
    cls: "cm-link",
    href,
  });
  // Replacements must stay on one line: a plugin may not hide line breaks.
  const hide = (from, to) => {
    if (to > from && !doc.sliceString(from, to).includes("\n"))
      out.push({ kind: "hide", from, to });
  };
  for (const { from, to } of ranges) {
    visible = { from, to };
    syntaxTree(state).iterate({
      from,
      to,
      enter(node) {
        const name = node.name;
        if (headings[name]) {
          const cls = "cm-" + headings[name];
          if (name.startsWith("ATXHeading")) line(node.from, node.to, cls);
          const c = node.node.cursor();
          if (c.firstChild())
            do {
              if (c.name !== "HeaderMark") continue;
              if (name.startsWith("SetextHeading")) {
                // The text lines are the heading; the underline row
                // collapses unless the cursor is on it.
                line(node.from, c.from - 1, cls);
                if (!isActive(c.from, c.to)) {
                  line(c.from, c.to, "cm-collapsed");
                  hide(doc.lineAt(c.from).from, c.to);
                }
                continue;
              }
              if (isActive(c.from, c.to)) continue;
              // Hide "# " with its space, or a closing "#" run.
              let end = c.to;
              if (doc.sliceString(end, end + 1) === " ") end++;
              let start = c.from;
              if (
                start > node.from &&
                doc.sliceString(start - 1, start) === " "
              )
                start--;
              hide(start, end);
            } while (c.nextSibling());
          return;
        }
        if (marks[name]) {
          out.push({
            kind: "mark",
            from: node.from,
            to: node.to,
            cls: "cm-" + marks[name],
          });
          return;
        }
        if (syntax.has(name)) {
          if (!isActive(node.from, node.to)) {
            // A quote mark takes the space after it with it.
            let end = node.to;
            if (name === "QuoteMark" && doc.sliceString(end, end + 1) === " ")
              end++;
            hide(node.from, end);
          }
          return;
        }
        switch (name) {
          case "Blockquote":
            line(node.from, node.to, "cm-quote");
            return;
          case "FencedCode":
          case "CodeBlock":
            line(node.from, node.to, "cm-codeblock");
            // Fence lines stay as empty padding rows of the code block, and
            // container marks (a quote's ">") inside it are hidden; each
            // shows only while the cursor is on its line.
            const marks = [];
            const c = node.node.cursor();
            if (c.firstChild())
              do {
                if (c.name === "QuoteMark") {
                  if (isActive(c.from, c.to)) continue;
                  let end = c.to;
                  if (doc.sliceString(end, end + 1) === " ") end++;
                  hide(c.from, end);
                } else if (c.name === "CodeMark") marks.push([c.from, c.to]);
              } while (c.nextSibling());
            const fences = marks.length > 1 ? [marks[0], marks.at(-1)] : marks;
            for (const [from] of fences) {
              const end = doc.lineAt(from).to;
              if (!isActive(from, end)) hide(from, end);
            }
            return false;
          case "Table":
            line(node.from, node.to, "cm-table");
            return false;
          case "LinkReference": {
            // A reference definition renders nothing, so its lines collapse
            // away from the cursor.
            // One the renderer rejects stays visible as text, as it reads.
            const url = node.node.getChild("URL");
            if (
              isActive(node.from, node.to) ||
              !url ||
              !links.destination(
                unescapeAll(
                  doc.sliceString(url.from, url.to).replace(/^<(.*)>$/, "$1"),
                ),
              )
            )
              return false;
            line(node.from, node.to, "cm-collapsed");
            for (const n of lineNumbers(
              doc,
              Math.max(node.from, visible.from),
              Math.min(node.to, visible.to),
            )) {
              const l = doc.line(n);
              hide(Math.max(l.from, node.from), Math.min(l.to, node.to));
            }
            return false;
          }
          case "Escape":
            // "\*" reads as "*".
            if (!isActive(node.from, node.to)) hide(node.from, node.from + 1);
            return;
          case "Entity": {
            // "&copy;" reads as "©"; an unknown entity stays as written.
            const source = doc.sliceString(node.from, node.to),
              decoded = unescapeAll(source);
            if (decoded !== source && !isActive(node.from, node.to))
              out.push({
                kind: "text",
                from: node.from,
                to: node.to,
                label: decoded,
              });
            return;
          }
          case "HorizontalRule":
            if (!isActive(node.from, node.to))
              out.push({ kind: "rule", from: node.from, to: node.to });
            return;
          case "ListMark": {
            if (isActive(node.from, node.to)) return;
            const text = doc.sliceString(node.from, node.to);
            if (/^[-*+]$/.test(text)) {
              out.push({ kind: "bullet", from: node.from, to: node.to });
              return;
            }
            // An ordered item shows the number the renderer gives it: the
            // list's first number plus the item's position, so lazy
            // "1. 1. 1." numbering reads 1, 2, 3.
            const item = node.node.parent,
              list = item?.parent;
            if (item?.name !== "ListItem" || list?.name !== "OrderedList")
              return;
            let start = null,
              index = 0;
            for (let c = list.firstChild; c; c = c.nextSibling) {
              if (c.name !== "ListItem") continue;
              if (start === null) {
                const mark = c.getChild("ListMark");
                start = mark
                  ? parseInt(doc.sliceString(mark.from, mark.to), 10)
                  : 1;
              }
              if (c.from === item.from) break;
              index++;
            }
            out.push({
              kind: "number",
              from: node.from,
              to: node.to,
              label: start + index + ".",
            });
            return;
          }
          case "Autolink": {
            // <https://example.com>: the URL between its angle brackets.
            const url = node.node.getChild("URL");
            if (!url) return false;
            const raw = doc.sliceString(url.from, url.to),
              href = links.destination(
                /^[a-z][a-z0-9+.-]*:/i.test(raw) ? raw : "mailto:" + raw,
              );
            if (!href) return false;
            out.push(linkMark(url.from, url.to, href));
            if (!isActive(node.from, node.to)) {
              hide(node.from, url.from);
              hide(url.to, node.to);
            }
            return false;
          }
          case "URL":
            // A bare URL; one inside a link or image is handled there.
            if (!linkParents.has(node.node.parent?.name)) {
              const found = links.bare(
                doc.sliceString(node.from, doc.lineAt(node.from).to),
                node.to - node.from,
              );
              if (found)
                out.push(
                  linkMark(node.from, node.from + found.length, found.href),
                );
            }
            return;
          case "Link":
          case "Image": {
            const c = node.node.cursor(),
              parts = [];
            if (c.firstChild())
              do parts.push({ name: c.name, from: c.from, to: c.to });
              while (c.nextSibling());
            const url = parts.find((p) => p.name === "URL"),
              href = url
                ? links.destination(
                    unescapeAll(
                      doc
                        .sliceString(url.from, url.to)
                        .replace(/^<(.*)>$/, "$1"),
                    ),
                  )
                : "";
            const lm = parts.filter((p) => p.name === "LinkMark");
            // Only inline links and images with a destination the renderer
            // accepts are rendered; reference links and rejected ones keep
            // their source.
            if (!href || lm.length < 2) return;
            const textFrom = lm[0].to,
              textTo = lm[1].from;
            if (name === "Image") {
              if (isActive(node.from, node.to)) return false;
              if (doc.sliceString(node.from, node.to).includes("\n"))
                return false;
              out.push({
                kind: "image",
                from: node.from,
                to: node.to,
                src: href,
                alt: links.plainText(doc.sliceString(textFrom, textTo)),
              });
              return false;
            }
            // An empty label has nothing to render, so it keeps its source;
            // CodeMirror also rejects an empty mark.
            if (textTo <= textFrom) return;
            out.push({
              kind: "mark",
              from: textFrom,
              to: textTo,
              cls: "cm-link",
              href,
            });
            // Each delimiter shows only while the cursor is on its line.
            if (!isActive(node.from, textFrom)) hide(node.from, textFrom);
            if (!isActive(textTo, node.to)) hide(textTo, node.to);
            return;
          }
        }
      },
    });
  }
  // A node that spans two visible ranges is visited twice. CodeMirror
  // rejects empty marks, which would disable the whole plugin.
  const seen = new Set();
  return out.filter((d) => {
    if (d.kind === "mark" && d.to <= d.from) return false;
    const key = `${d.kind} ${d.from} ${d.to} ${d.cls}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

// Blocks that contain no tables; tables are never inside these.
const leaves = new Set([
  "Paragraph",
  "FencedCode",
  "CodeBlock",
  "HTMLBlock",
  "HorizontalRule",
  "LinkReference",
  ...Object.keys(headings),
]);

// tableBlocks returns every table as whole lines, with whether a selection
// touches it while the editor has focus. Such a table shows its Markdown
// source; every other table is rendered. context holds the document's link
// reference definitions, which a table's reference links need to render.
export function tableBlocks(state, focused) {
  const doc = state.doc,
    out = [],
    definitions = [];
  syntaxTree(state).iterate({
    enter(node) {
      if (node.name === "LinkReference") {
        definitions.push(doc.sliceString(node.from, node.to));
        return false;
      }
      if (leaves.has(node.name)) return false;
      if (node.name !== "Table") return;
      const from = doc.lineAt(node.from).from,
        to = doc.lineAt(node.to).to,
        active =
          focused &&
          state.selection.ranges.some((r) => r.from <= to && r.to >= from);
      out.push({ from, to, active, source: doc.sliceString(from, to) });
      return false;
    },
  });
  const context = definitions.join("\n");
  for (const t of out) t.context = context;
  return out;
}
