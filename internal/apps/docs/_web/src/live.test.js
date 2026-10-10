import { test } from "node:test";
import assert from "node:assert/strict";
import { EditorState, EditorSelection } from "@codemirror/state";
import { ensureSyntaxTree } from "@codemirror/language";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { activeLines, liveDecorations, tableBlocks } from "./live.js";

function decorate(text, cursor) {
  const state = EditorState.create({
    doc: text,
    selection:
      cursor === undefined ? undefined : EditorSelection.cursor(cursor),
    extensions: [markdown({ base: markdownLanguage })],
  });
  ensureSyntaxTree(state, state.doc.length, 5000);
  const active = activeLines(state, cursor !== undefined);
  return liveDecorations(state, [{ from: 0, to: state.doc.length }], active)
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
  assert.deepEqual(hidden(decorate(text, 8)), []);
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
  assert.equal(activeLines(state, false).size, 0);
  assert.deepEqual([...activeLines(state, true)], [1]);
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
  const [table] = tableBlocks(state(0));
  assert.equal(table.source, "| a | b |\n| - | - |\n| 1 | 2 |");
  assert.equal(table.active, false);
  assert.equal(tableBlocks(state(text.indexOf("1 |"))).at(0).active, true);
});
