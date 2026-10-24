import { test } from "node:test";
import assert from "node:assert/strict";
import { markdownLanguage } from "@codemirror/lang-markdown";
import {
  linearInline,
  plainLongParagraphs,
  PLAIN_PARAGRAPH,
} from "./markdown-inline.js";

// The stock editor parser and the one the editor uses.
// stock is lezer's parser; editor is the editor's (all test paragraphs
// here are shorter than PLAIN_PARAGRAPH, so it must match stock); linear
// has only the inline replacements, for growth tests on long paragraphs.
const stock = markdownLanguage.parser,
  editor = stock.configure([linearInline, plainLongParagraphs]),
  linear = stock.configure(linearInline);

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
    nodes(editor, text),
    nodes(stock, text),
    JSON.stringify(text.length > 200 ? text.slice(0, 200) + "…" : text),
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

test("emphasis, strikethrough and inline HTML parse exactly as before", () => {
  for (const text of [
    "*a* _b_ **c** __d__ ***e*** ___f___ ~~g~~ ~h~ ^i^",
    "snake_case_word and a_b_c but _a_b_ and __a__b__ and *a*b*",
    "*a **b** c* and **a *b* c** and ***a* b** and ***a** b* and *a **b***",
    "*a _b* c_ and _a *b_ c* and ~~a *b~~ c* and *a ~~b* c~~",
    "**a* and *a** and ****a**** and *****a** and **a*****",
    // An opener that shrinks can become usable for a closer kind that
    // found nothing before.
    "*****_****_***_",
    "a* b* c* d_ e_ f_ g~~ h~~ and a*b c*d e_f g_h",
    "*foo**bar**baz* and *foo**bar* and **foo*bar*baz** and foo***bar***baz",
    '*(*foo*)* and _(_foo_)_ and **foo "*bar*" foo** and *foo [bar](/url)*',
    "*a `*` b* and _a <span>_</span> b_ and *a [*](b) c* and \\*a\\* *b\\*",
    "* not a list *\n\n*a\nb* and _a\n_ and **a\n\n**b",
    "x <?php echo 1; ?> y <!DOCTYPE html> <![CDATA[ a ]] b ]]> <!-- c --> <a href='x' b=\"y\" c=d e>",
    "x <? never closed and <!X never > closed <![CDATA[ never and <a b='c and </a > <b/> <c / >",
    "x <a@b.c> <https://a.b/c?d> <?a@b> <!A@b> <?x?> <??> <?> <!A> <!a> <![CDATA[]]>",
  ])
    same(text);
});

test("random emphasis soup parses exactly as before", () => {
  let seed = 11;
  const random = () => {
    seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
    return seed / 4294967296;
  };
  const pieces = [
    "*",
    "**",
    "***",
    "_",
    "__",
    "~~",
    "~",
    "a",
    "b",
    " ",
    ".",
    "\n",
    "[",
    "](u)",
    "`",
  ];
  for (let n = 0; n < 20000; n++) {
    let text = "";
    const length = 1 + Math.floor(random() * 30);
    for (let i = 0; i < length; i++)
      text += pieces[Math.floor(random() * pieces.length)];
    same(text);
  }
});

test("link destinations and titles parse exactly as before", () => {
  for (const text of [
    "[a](b(c)d) [a](b(c)d)e) [a](b)c) [a](b((c)) [a]((b)) [a](()) [a](b(c(d)e)f)g",
    "[a](b\\(c) [a](b\\)c) [a](b\\\\)c) [a](\\\\) [a](\\() [a](b\\ c) [a](b\\",
    '[a](  b  ) [a]( b "t" ) [a](b\n"t") [a](\nb\n) [a](b "t"x) [a](b"t")',
    '[a](<b c>) [a](<b>c) [a](<b(>) [a](<b\nc>) [a](<b<c>) [a](<> "t") [a](<b',
    '[a](b "c\\"d") [a](b \'c\\\'d\') [a](b (c\\)d)) [a](b (c(d)) [a](b "c\nd")',
    "[a](b \"c [x](y) d\") [a](b (c [x](y) d)) [a](b 'c' 'd') [a](b \"c\")",
    "[x](a[x](b[x](c) [x](a(b[x](c)d)e) [x](\\[x](a)",
    "[x](a (t [x](b (u [x](c (v) w) ([x](d))",
    '[x](a "t\\\\" [x](b "u\\\\\\" [x](c "v")',
    "![i](a(b) ![i](a \"t\" [x](b) ![i](<a> 'b' ![i](a))",
    "[x](a)(b) [x](a) (b) [x] (a) [x][y](z) [x](a)[y]\n\n[y]: /q",
  ])
    same(text);
});

test("random destination soup parses exactly as before", () => {
  let seed = 13;
  const random = () => {
    seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
    return seed / 4294967296;
  };
  const pieces = [
    "[x]",
    "](",
    "(",
    ")",
    "\\",
    "a",
    " ",
    '"',
    "'",
    "<",
    ">",
    "\n",
    "[",
    "]",
    "![",
  ];
  for (let n = 0; n < 20000; n++) {
    let text = "";
    const length = 1 + Math.floor(random() * 30);
    for (let i = 0; i < length; i++)
      text += pieces[Math.floor(random() * pieces.length)];
    same(text);
  }
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
    "**",
    "***",
    "_",
    "<?",
    "?>",
    "<!A",
    "<![CDATA[",
    "]]>",
    "<!--",
    "-->",
    "<a b='c'>",
    "'",
    "](",
    "](a ",
    ' "',
    " (",
  ];
  for (let n = 0; n < 6000; n++) {
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
// A paragraph of items typed without spaces or line breaks.
const joined = (n, item) => {
  let text = "w ";
  for (let i = 0; i < n; i++) text += item(i);
  return text;
};

for (const [name, item, layout = paragraph] of [
  ["bracketed words", (i) => `[g${i % 20}-${i}]`],
  ["links", (i) => `[a${i}](https://x/${i})`],
  ["stray closing brackets", () => "*a* ]"],
  ["autolinks", (i) => `www.a${i}.io`],
  ["unclosed openers", () => "[a"],
  ["closing asterisks", () => "a*"],
  ["closing underscores", () => "a_"],
  ["closing tildes", () => "a~~"],
  ["mixed closers", (i) => ["a*", "a_", "a~~", "a**"][i % 4]],
  ["intraword asterisks", () => "a*b"],
  ["leftover closers", () => "*a**"],
  ["unclosed processing instructions", () => "w <?a"],
  ["unclosed declarations", () => "w <!A"],
  ["unclosed CDATA", () => "w <![CDATA["],
  ["unfinished destinations", () => "[x](", joined],
  ["unfinished word destinations", () => "[x](a", joined],
  ["unbalanced destinations", () => "[x]((", joined],
  ["unfinished parenthesized titles", () => "[x](a (", joined],
  ["unfinished parenthesized titles on lines", () => "[x](a ("],
  ["unfinished quoted titles", () => '[x](a "', joined],
  ["unfinished labels", () => "[x][y", joined],
]) {
  test(`a paragraph of ${name} parses in linear time`, () => {
    const small = layout(2000, item),
      large = layout(8000, item);
    const ts = best(small),
      tl = best(large);
    // Stock lezer took ~40-1200 ms for 8000 of these items on a laptop and
    // grew 11-16x from 2000; linear parsing takes a few ms and grows ~4x.
    assert.ok(tl < 1000, `${name}: 8000 items took ${tl.toFixed(1)} ms`);
    assert.ok(
      tl / Math.max(ts, 0.1) < 8,
      `${name}: 2000 items ${ts.toFixed(1)} ms, 8000 items ${tl.toFixed(1)} ms`,
    );
  });
}

// sized returns Markdown with links, emphasis, inline HTML and references
// whose paragraph text is exactly n characters.
function sized(n) {
  const unit = "a *b* [c](https://d/) <i>e</i> [f][g] `h` ";
  let text = unit.repeat(Math.ceil(n / unit.length)).slice(0, n - 1);
  return text + "x";
}

test("paragraphs up to 20,000 characters are inline parsed as before", () => {
  assert.equal(PLAIN_PARAGRAPH, 20000);
  for (const n of [19999, 20000]) {
    const text = sized(n);
    assert.equal(text.length, n);
    same(text);
    same(text + "\n\n[g]: /ref");
    same("> " + text);
    // A task item's text includes its "[ ] ".
    same("- [ ] " + text.slice(4));
    // Text over several lines counts its line breaks.
    const split = text.indexOf(" a ", 9990);
    same(text.slice(0, split) + "\n" + text.slice(split + 1));
  }
});

test("a paragraph over 20,000 characters is not inline parsed", () => {
  const text = sized(20001),
    tree = (doc) => editor.parse(doc).topNode;
  // Only the paragraph, without inline nodes.
  assert.equal(tree(text).toString(), "Document(Paragraph)");
  // A quote keeps its marks, a task item its marker.
  // (Split before a word: a line starting "* " would begin a list.)
  const split = text.indexOf(" a ", 9990) + 1,
    quoted = "> " + text.slice(0, split) + "\n> " + text.slice(split);
  assert.equal(
    tree(quoted).toString(),
    "Document(Blockquote(QuoteMark,Paragraph(QuoteMark)))",
  );
  assert.equal(
    tree("- [ ] " + text.slice(3)).toString(),
    "Document(BulletList(ListItem(ListMark,Task(TaskMarker))))",
  );
  // Neighbouring blocks, and a reference definition starting the long
  // paragraph, are parsed as before.
  const doc = "# *T*\n\n" + text + "\n\n*after* [x](y)",
    nodesOf = (parser) => {
      const out = [];
      parser.parse(doc).iterate({
        enter(n) {
          if (n.from < 9 || n.from > 9 + text.length)
            out.push(`${n.name} ${n.from}-${n.to}`);
        },
      });
      return out;
    };
  assert.deepEqual(nodesOf(editor), nodesOf(stock));
  assert.equal(
    tree("[g]: /ref\n" + text).toString(),
    "Document(LinkReference(LinkLabel,LinkMark,URL),Paragraph)",
  );
});
