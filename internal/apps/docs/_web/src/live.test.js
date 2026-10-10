import { test } from "node:test";
import assert from "node:assert/strict";
import { EditorState, EditorSelection } from "@codemirror/state";
import { ensureSyntaxTree } from "@codemirror/language";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import {
  activeLines,
  linkAt,
  liveDecorations,
  referenceDefinitions,
  tableBlocks,
} from "./live.js";
import { createMarkdown, linkRules } from "./markdown.js";

const links = linkRules(createMarkdown());

function decorate(text, cursor) {
  const state = EditorState.create({
    doc: text,
    selection:
      cursor === undefined ? undefined : EditorSelection.cursor(cursor),
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(state, state.doc.length, 5000);
  const active = activeLines(state, cursor !== undefined);
  return liveDecorations(
    state,
    [{ from: 0, to: state.doc.length }],
    active,
    links,
    referenceDefinitions(state.doc, links),
  )
    .map((d) => ({ ...d, text: text.slice(d.from, d.to) }))
    .sort((a, b) => a.from - b.from || a.kind.localeCompare(b.kind));
}
const hidden = (ds) => ds.filter((d) => d.kind === "hide").map((d) => d.text);

test("headings are styled and their marks hidden off the cursor line", () => {
  const ds = decorate("# Title\n\ntext");
  assert.ok(ds.some((d) => d.kind === "line" && d.cls === "cm-h1"));
  assert.deepEqual(hidden(ds), ["# "]);
  assert.deepEqual(hidden(decorate("# Title\n\ntext", 3)), []);
});

test("inline syntax is hidden only on inactive lines", () => {
  const text = "a **b** _c_ `d` ~~e~~\n\nnext";
  assert.deepEqual(hidden(decorate(text)), [
    "**",
    "**",
    "_",
    "_",
    "`",
    "`",
    "~~",
    "~~",
  ]);
  assert.deepEqual(hidden(decorate(text, 0)), []);
  const kinds = decorate(text)
    .filter((d) => d.kind === "mark")
    .map((d) => d.cls);
  assert.deepEqual(kinds, ["cm-strong", "cm-em", "cm-code", "cm-strike"]);
});

test("links show their text and keep the destination for opening", () => {
  const ds = decorate("see [docs](guide/setup.md) now\n\nx");
  const link = ds.find((d) => d.cls === "cm-link");
  assert.equal(link.text, "docs");
  assert.equal(link.href, "guide/setup.md");
  assert.deepEqual(hidden(ds), ["[", "](guide/setup.md)"]);
});

test("images, rules and bullets become widgets", () => {
  const ds = decorate("![alt](a.png)\n\n---\n\n- item\n\n1. one");
  const image = ds.find((d) => d.kind === "image");
  assert.equal(image.src, "a.png");
  assert.equal(image.alt, "alt");
  assert.equal(ds.filter((d) => d.kind === "rule").length, 1);
  assert.deepEqual(
    ds.filter((d) => d.kind === "bullet").map((d) => d.text),
    ["-"],
  );
});

test("fenced code keeps its lines and hides the fences", () => {
  const text = "```js\nlet a = 1;\n```\n\nafter";
  const ds = decorate(text);
  assert.equal(ds.filter((d) => d.cls === "cm-codeblock").length, 3);
  assert.deepEqual(hidden(ds), ["```js", "```"]);
  // A cursor inside the code shows neither fence; one on a fence shows it.
  assert.deepEqual(hidden(decorate(text, 8)), ["```js", "```"]);
  assert.deepEqual(hidden(decorate(text, 2)), ["```"]);
});

test("quotes hide their marks and nested syntax never overlaps", () => {
  const ds = decorate("> # Quoted\n> **bold**\n\nx");
  assert.deepEqual(hidden(ds), ["> ", "# ", "> ", "**", "**"]);
  const hides = ds.filter((d) => d.kind === "hide");
  for (let i = 1; i < hides.length; i++)
    assert.ok(hides[i].from >= hides[i - 1].to, "hidden ranges overlap");
});

test("no hidden range spans a line break", () => {
  const ds = decorate("Title\n=====\n\n[a\nb](u)\n\n*x\ny*");
  for (const d of ds)
    if (d.kind !== "line")
      assert.ok(d.kind === "mark" || !d.text.includes("\n"));
});

test("an unfocused editor shows every line rendered", () => {
  const state = EditorState.create({ doc: "a\nb" });
  assert.deepEqual(activeLines(state, false), []);
  assert.deepEqual(activeLines(state, true), [[1, 1]]);
});

test("tables are found as whole lines and reveal under the selection", () => {
  const text = "x\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\ny";
  const state = (cursor) => {
    const s = EditorState.create({
      doc: text,
      selection: EditorSelection.cursor(cursor),
      extensions: [markdown({ base: markdownLanguage })],
    });
    ensureSyntaxTree(s, s.doc.length, 5000);
    return s;
  };
  const [table] = tableBlocks(state(0), true);
  assert.equal(table.source, "| a | b |\n| - | - |\n| 1 | 2 |");
  assert.equal(table.active, false);
  const inside = state(text.indexOf("1 |"));
  assert.equal(tableBlocks(inside, true).at(0).active, true);
  // Without focus the table renders even with the cursor inside it.
  assert.equal(tableBlocks(inside, false).at(0).active, false);
});

test("an empty link label keeps its source and adds no empty mark", () => {
  const ds = decorate("see [](guide.md) and [](<>)\n\nx");
  assert.deepEqual(hidden(ds), []);
  for (const d of ds) if (d.kind === "mark") assert.ok(d.to > d.from);
});

test("autolinks and bare URLs become links as the renderer links them", () => {
  const ds = decorate(
    "<https://a.example> and https://b.example/x and www.c.example\n\nz",
  );
  const found = ds.filter((d) => d.cls === "cm-link");
  // markdown-it does not linkify a bare www. address, so neither do we.
  assert.deepEqual(
    found.map((d) => [d.text, d.href]),
    [
      ["https://a.example", "https://a.example"],
      ["https://b.example/x", "https://b.example/x"],
    ],
  );
  assert.deepEqual(hidden(ds), ["<", ">"]);
});

test("line decorations stay within the visible range", () => {
  const body = Array.from({ length: 5000 }, (_, i) => "x" + i).join("\n");
  const text = "```\n" + body + "\n```\n";
  const state = EditorState.create({
    doc: text,
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(state, state.doc.length, 5000);
  const visible = {
    from: state.doc.line(100).from,
    to: state.doc.line(120).to,
  };
  const lines = liveDecorations(state, [visible], [], links).filter(
    (d) => d.kind === "line",
  );
  assert.equal(lines.length, 21);
  // A selection over everything is two numbers, not one entry per line.
  const all = EditorState.create({
    doc: text,
    selection: EditorSelection.range(0, text.length),
  });
  assert.deepEqual(activeLines(all, true), [[1, 5003]]);
});

test("setext headings style their text and collapse the underline", () => {
  const ds = decorate("Title\n=====\n\nx");
  assert.deepEqual(
    ds.filter((d) => d.kind === "line").map((d) => [d.from, d.cls]),
    [
      [0, "cm-h1"],
      [6, "cm-collapsed"],
    ],
  );
  assert.deepEqual(hidden(ds), ["====="]);
  const onUnderline = decorate("Title\n=====\n\nx", 8);
  assert.deepEqual(hidden(onUnderline), []);
  assert.ok(!onUnderline.some((d) => d.cls === "cm-collapsed"));
});

test("multiline links reveal only the delimiter on the cursor line", () => {
  const text = "[a\nb](u)\n\nz";
  assert.deepEqual(hidden(decorate(text, 0)), ["](u)"]);
  assert.deepEqual(hidden(decorate(text, 4)), ["["]);
});

test("destinations decode escapes and entities like the renderer", () => {
  const ds = decorate(
    "[x](https://e.test/?a=1&amp;b=\\_2) ![i](a\\_b.png)\n\nz",
  );
  assert.equal(
    ds.find((d) => d.cls === "cm-link").href,
    "https://e.test/?a=1&b=_2",
  );
  assert.equal(ds.find((d) => d.kind === "image").src, "a_b.png");
});

test("links and images the renderer rejects keep their source", () => {
  const ds = decorate(
    "[x](javascript:alert(1)) ![a](data:image/png;base64,AA) <javascript:x>\n\nz",
  );
  assert.deepEqual(hidden(ds), []);
  assert.equal(ds.filter((d) => d.kind === "image").length, 0);
  assert.equal(ds.filter((d) => d.cls === "cm-link").length, 0);
});

test("destinations are the normalized URL the renderer links to", () => {
  const ds = decorate("[x](foo\\bar) [y](a b) <a@b.example>\n\nz");
  assert.deepEqual(
    ds.filter((d) => d.cls === "cm-link").map((d) => d.href),
    ["foo%5Cbar", "mailto:a@b.example"],
  );
});

test("ordered items show the number the renderer gives them", () => {
  const ds = decorate("3. a\n1. b\n1. c\n\nx");
  assert.deepEqual(
    ds.filter((d) => d.kind === "number").map((d) => [d.label, d.from]),
    [
      ["3.", 0],
      ["4.", 5],
      ["5.", 10],
    ],
  );
  // The cursor's line shows the stored marker.
  assert.equal(
    decorate("1. a\n1. b\n\nx", 6).filter((d) => d.kind === "number").length,
    1,
  );
});

test("image alt text is the label as the renderer writes it", () => {
  const ds = decorate("![**Revenue** &amp; costs](chart.png)\n\nx");
  assert.equal(ds.find((d) => d.kind === "image").alt, "Revenue & costs");
});

test("escapes and entities read as the renderer writes them", () => {
  const ds = decorate("\\* &copy; &bogus;\n\nx");
  assert.deepEqual(hidden(ds), ["\\"]);
  assert.deepEqual(
    ds.filter((d) => d.kind === "text").map((d) => [d.text, d.label]),
    [["&copy;", "©"]],
  );
  assert.deepEqual(hidden(decorate("\\* &copy;\n\nx", 0)), []);
});

test("fenced code in a quote hides its quote marks and both fences", () => {
  const text = "> ```js\n> hi\n> ```\n\nx";
  const ds = decorate(text);
  assert.deepEqual(hidden(ds), ["> ", "```js", "> ", "> ", "```"]);
  const hides = ds.filter((d) => d.kind === "hide");
  for (let i = 1; i < hides.length; i++)
    assert.ok(hides[i].from >= hides[i - 1].to, "hidden ranges overlap");
  // The cursor on the code line shows only that line's quote mark.
  assert.deepEqual(hidden(decorate(text, 11)), ["> ", "```js", "> ", "```"]);
});

test("tables carry the document's reference definitions", () => {
  const text = "| a |\n| - |\n| [docs][id] |\n\n[id]: https://e.example\n";
  const s = EditorState.create({
    doc: text,
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(s, s.doc.length, 5000);
  const [table] = tableBlocks(s, false, referenceDefinitions(s.doc, links));
  assert.equal(table.context, "[id]: https://e.example");
  const html = createMarkdown().render(table.source + "\n\n" + table.context);
  assert.match(html, /<a href="https:\/\/e\.example">docs<\/a>/);
});

test("reference definitions collapse away from the cursor", () => {
  const text = "[a][r]\n\n[r]: https://e.example\n";
  const ds = decorate(text);
  // The reference link renders too, hiding its brackets and label.
  assert.deepEqual(hidden(ds), ["[", "][r]", "[r]: https://e.example"]);
  assert.ok(ds.some((d) => d.cls === "cm-collapsed" && d.from === 8));
  // On the definition's line, only the definition shows its source.
  assert.deepEqual(hidden(decorate(text, 10)), ["[", "][r]"]);
});

test("rejected reference definitions stay visible", () => {
  assert.deepEqual(hidden(decorate("[r]: javascript:alert(1)\n\nx")), []);
});

test("a bare URL links as far as the renderer links it", () => {
  const ds = decorate("go https://example.com?x=1 now\n\nz");
  const link = ds.find((d) => d.cls === "cm-link");
  assert.equal(link.text, "https://example.com?x=1");
  assert.equal(link.href, "https://example.com?x=1");
});

test("tables carry only the definitions the renderer accepts", () => {
  const text =
    "| a |\n| - |\n| [x][ok] |\n\n[ok]: https://e.example\n[bad]: javascript:alert(1)\n";
  const s = EditorState.create({
    doc: text,
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(s, s.doc.length, 5000);
  assert.equal(
    tableBlocks(s, false, referenceDefinitions(s.doc, links))[0].context,
    "[ok]: https://e.example",
  );
});

test("linkAt finds the link under the cursor for the keyboard", () => {
  const text = "see [docs](guide.md) and https://e.example\n\nz";
  const s = EditorState.create({
    doc: text,
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(s, s.doc.length, 5000);
  const refs = referenceDefinitions(s.doc, links);
  assert.equal(linkAt(s, text.indexOf("docs") + 1, links, refs), "guide.md");
  assert.equal(
    linkAt(s, text.indexOf("e.example"), links, refs),
    "https://e.example",
  );
  assert.equal(linkAt(s, 1, links, refs), "");
});

test("reference links resolve against the document's definitions", () => {
  const text =
    'a [docs][ID] b [id][] c [Id] d [none] ![pic][id]\n\n> [id]: <https://e.example/x> "T"\n';
  const ds = decorate(text);
  assert.deepEqual(
    ds.filter((d) => d.cls === "cm-link").map((d) => [d.text, d.href]),
    [
      ["docs", "https://e.example/x"],
      ["id", "https://e.example/x"],
      ["Id", "https://e.example/x"],
    ],
  );
  assert.equal(ds.find((d) => d.kind === "image").src, "https://e.example/x");
  // "[none]" has no definition and keeps its brackets.
  const none = text.indexOf("[none]");
  assert.ok(!ds.some((d) => d.kind === "hide" && d.from === none));
});

test("definitions are found beyond the parsed tree", () => {
  const filler = Array.from({ length: 3000 }, (_, i) => "line " + i).join(
    "\n\n",
  );
  const text =
    "| a |\n| - |\n| [x][far] |\n\n" +
    filler +
    "\n\n[far]: https://far.example\n";
  // No syntax tree is forced: the scan reads the text itself.
  const s = EditorState.create({ doc: text });
  const refs = referenceDefinitions(s.doc, links);
  assert.equal(
    refs.get(links.label("far")).source,
    "[far]: https://far.example",
  );
});
