import { test } from "node:test";
import assert from "node:assert/strict";
import { markdownLanguage } from "@codemirror/lang-markdown";
import { linearLinks } from "./markdown-links.js";

// The stock editor parser and the one the editor uses.
const stock = markdownLanguage.parser,
  linear = stock.configure(linearLinks);

// nodes lists every node of a parse with its range, so two parses compare
// node for node.
function nodes(parser, text) {
  const out = [];
  parser.parse(text).iterate({
    enter(n) {
      out.push(`${n.name} ${n.from}-${n.to}`);
    },
  });
  return out;
}

function same(text) {
  assert.deepEqual(
    nodes(linear, text),
    nodes(stock, text),
    JSON.stringify(text),
  );
}

test("ordinary Markdown parses exactly as before", () => {
  for (const text of [
    "[a](https://example.com) and [b](<a b> \"title\") and [c](d 'e') [f](g (h))",
    "[ref][label] and [collapsed][] and [shortcut]\n\n[label]: https://x\n[shortcut]: /y",
    '![image](a.png "t") ![ref image][label] ![shortcut]\n\n[label]: b.png',
    "`[not a link](x)` and ``[a]`` then [real](x)",
    "\\[escaped\\] [a\\]b](c) [a](b\\)c)",
    "[outer [inner](a)](b) and ![outer [inner](a)](b) and [a ![i](b)](c)",
    "[a [b] c](d) [[a]](b) [a]] [[b] [] [](x) [ ](y) []z",
    "*[a*](b) [a *b](c)* **[x](y)** _[a_](b) ~~[s](t)~~",
    "[a](b [c](d) [e](f)\n[g\nh](i) [j](k\nl)",
    "www.example.com [x www.a.com/[y] z](w) [www.b.com] https://c.d/[e]",
    "<https://auto.link> [a](<b>) [c](<d) [e]: not a definition",
    "- [x] task [link](a)\n- [ ] [ref]\n\n> [quoted](q) ![im](g)\n\n| [a](b) | [c] |\n| - | - |\n| [d][e] | x |",
    "# [heading](h) [ref]\n\nSetext [x](y)\n===",
    "[a](b)[c](d)[e][f][g]\n\n[f]: x",
    "[" + "x".repeat(1200) + "][" + "y".repeat(1200) + "]",
    "!![a](b) ![](c) ![[d]](e) ![a] [b]: c",
  ])
    same(text);
});

test("random bracket soup parses exactly as before", () => {
  // A deterministic generator, so a failure names a reproducible input.
  let seed = 7;
  const random = () => {
    seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
    return seed / 4294967296;
  };
  const pieces = [
    "[",
    "]",
    "![",
    "(",
    ")",
    "a",
    " ",
    "*",
    "_",
    "~~",
    "`",
    "\\",
    "<",
    ">",
    '"',
    "\n",
    "www.a.io",
    "http://b.c/",
    ":",
    "x",
    "[]",
    "](u)",
    "][r]",
    "\n\n[r]: /d\n\n",
    "\n> ",
    "\n- ",
    "&amp;",
  ];
  for (let n = 0; n < 4000; n++) {
    let text = "";
    const length = 1 + Math.floor(random() * 60);
    for (let i = 0; i < length; i++)
      text += pieces[Math.floor(random() * pieces.length)];
    same(text);
  }
});

// best returns the fastest of several parses, to keep scheduler noise out of
// the growth comparison.
function best(text) {
  linear.parse(text);
  let fastest = Infinity;
  for (let r = 0; r < 5; r++) {
    const t = performance.now();
    linear.parse(text);
    fastest = Math.min(fastest, performance.now() - t);
  }
  return fastest;
}

// One paragraph of guest-typed items with a newline every ten, as in the
// browser display test.
const paragraph = (n, item) => {
  let text = "";
  for (let i = 0; i < n; i++) text += item(i) + (i % 10 == 9 ? "\n" : " ");
  return text;
};

for (const [name, item] of [
  ["bracketed words", (i) => `[g${i % 20}-${i}]`],
  ["links", (i) => `[a${i}](https://x/${i})`],
  ["stray closing brackets", () => "*a* ]"],
  ["autolinks", (i) => `www.a${i}.io`],
  ["unclosed openers", () => "[a"],
]) {
  test(`a paragraph of ${name} parses in linear time`, () => {
    const small = paragraph(2000, item),
      large = paragraph(8000, item);
    const ts = best(small),
      tl = best(large);
    // Lezer's own LinkEnd took ~40-280 ms for 8000 items on a laptop and
    // grew ~4x per doubling; linear parsing takes a few ms and doubles.
    assert.ok(tl < 1000, `${name}: 8000 items took ${tl.toFixed(1)} ms`);
    assert.ok(
      tl / Math.max(ts, 0.1) < 8,
      `${name}: 2000 items ${ts.toFixed(1)} ms, 8000 items ${tl.toFixed(1)} ms`,
    );
  });
}
