'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM, textNodes, elements, htmlWrites } from './dom.mjs';
import {
  cpLength, cpToUnits, unitsToCP, canonicalize, sliceRuns, splitRuns, insertText, toggleMark, diffRuns,
  inlineShortcut, blockShortcut, renderRuns, domToCP, cpToDom, domText, mapOffset, plainText,
} from '../richtext.js';

const r = (text, ...marks) => ({ text, marks });

test('offsets count code points, not UTF-16 units', () => {
  const s = 'a😀한b';
  assert.equal(s.length, 5);
  assert.equal(cpLength(s), 4);
  assert.equal(cpToUnits(s, 2), 3);
  assert.equal(unitsToCP(s, 3), 2);
  assert.equal(unitsToCP(s, 2), 2, 'an index inside a surrogate pair counts the pair');
  assert.equal(cpToUnits(s, 99), 5);
});

test('canonicalize orders marks, drops empty runs and coalesces neighbors', () => {
  assert.deepEqual(canonicalize([r('a', 'italic', 'bold'), r(''), r('b', 'bold', 'italic'), r('c')]),
    [r('ab', 'bold', 'italic'), r('c')]);
  const link = { url: 'https://example.com' };
  assert.deepEqual(canonicalize([{ text: 'x', marks: [], link }, { text: 'y', marks: [], link: { url: 'https://example.com' } }]),
    [{ text: 'xy', marks: [], link }]);
});

test('split and slice keep styles across a surrogate pair boundary', () => {
  const runs = [r('a😀', 'bold'), r('b🎉c')];
  assert.deepEqual(splitRuns(runs, 2), [[r('a😀', 'bold')], [r('b🎉c')]]);
  assert.deepEqual(sliceRuns(runs, 1, 4), [r('😀', 'bold'), r('b🎉')]);
});

test('typing takes the style of the character before the caret and does not extend a link', () => {
  const link = { page: 'p_' + 'a'.repeat(26) };
  const runs = [r('bo', 'bold'), { text: 'ln', marks: [], link }];
  assert.deepEqual(insertText(runs, 2, 'X'), [r('boX', 'bold'), { text: 'ln', marks: [], link }]);
  assert.deepEqual(insertText(runs, 4, 'Y'), [r('bo', 'bold'), { text: 'ln', marks: [], link }, r('Y')]);
  assert.deepEqual(insertText(runs, 3, 'Z'), [r('bo', 'bold'), { text: 'lZn', marks: [], link }]);
});

test('toggleMark adds a mark unless the whole range has it', () => {
  const runs = [r('ab', 'bold'), r('cd')];
  assert.deepEqual(toggleMark(runs, 1, 3, 'bold'), [r('abc', 'bold'), r('d')]);
  assert.deepEqual(toggleMark([r('abcd', 'bold')], 1, 3, 'bold'), [r('a', 'bold'), r('bc'), r('d', 'bold')]);
});

test('diffRuns keeps the styles of an unchanged prefix and suffix', () => {
  const runs = [r('Hello ', 'bold'), r('world')];
  assert.deepEqual(diffRuns(runs, 'Hello big world'), [r('Hello big ', 'bold'), r('world')]);
  assert.deepEqual(diffRuns(runs, '한글 world'), [r('한글 ', 'bold'), r('world')]);
});

test('mapOffset carries a caret through a remote replacement of the same group', () => {
  assert.equal(mapOffset('hello world', 'hello brave world', 8), 14, 'after the change shifts');
  assert.equal(mapOffset('hello world', 'hello brave world', 3), 3, 'before the change stays');
  assert.equal(mapOffset('abc', '', 2), 0, 'clamps');
});

test('Markdown shortcuts: block prefixes and closed inline pairs', () => {
  assert.equal(blockShortcut('# '), 'heading1');
  assert.equal(blockShortcut('[] '), 'todo');
  assert.equal(blockShortcut('```'), 'code');
  assert.equal(blockShortcut('---'), 'divider');
  assert.equal(blockShortcut('#'), null);
  assert.deepEqual(inlineShortcut([r('say **hi**')], 10), { runs: [r('say '), r('hi', 'bold')], caret: 6 });
  assert.deepEqual(inlineShortcut([r('x *it*')], 6), { runs: [r('x '), r('it', 'italic')], caret: 4 });
  assert.deepEqual(inlineShortcut([r('`c`')], 3), { runs: [r('c', 'code')], caret: 1 });
  assert.deepEqual(inlineShortcut([r('~~s~~ tail')], 5), { runs: [r('s', 'strike'), r(' tail')], caret: 1 });
  assert.equal(inlineShortcut([r('a ** b**')], 8), null, 'no mark with a space inside the opener');
  assert.equal(inlineShortcut([r('**')], 2), null);
});

test('rendering maps runs to text nodes under mark elements, never markup', () => {
  installDOM();
  const root = document.createElement('div');
  const evil = '<img src=x onerror=alert(1)>';
  root.append(...renderRuns([r(evil, 'bold', 'code'), { text: 'site', marks: ['italic'], link: { url: 'https://example.com/?a=1' } },
    { text: 'page', marks: [], link: { page: 'p_' + 'b'.repeat(26) } }, { text: 'bad', marks: [], link: { url: 'javascript:alert(1)' } }]));
  assert.deepEqual(textNodes(root).map((t) => t.data), [evil, 'site', 'page', 'bad']);
  assert.deepEqual(elements(root).map((e) => e.tagName), ['STRONG', 'CODE', 'A', 'EM', 'A']);
  const [ext, internal] = elements(root, 'a');
  assert.equal(ext.getAttribute('target'), '_blank');
  assert.equal(ext.getAttribute('rel'), 'noopener noreferrer');
  assert.equal(ext.getAttribute('referrerpolicy'), 'no-referrer');
  assert.equal(internal.getAttribute('href'), '#/p_' + 'b'.repeat(26));
  assert.equal(internal.getAttribute('target'), null);
  assert.equal(elements(root, 'img').length, 0);
  assert.deepEqual(htmlWrites, []);
});

test('DOM points and code point offsets map both ways across runs', () => {
  installDOM();
  const root = document.createElement('div');
  root.append(...renderRuns([r('a😀', 'bold'), r('한글'), r('z', 'italic')]));
  const [t1, t2, t3] = textNodes(root);
  assert.equal(domText(root), 'a😀한글z');
  assert.equal(domToCP(root, t1, 3), 2, 'after the emoji (3 UTF-16 units)');
  assert.equal(domToCP(root, t2, 1), 3);
  assert.equal(domToCP(root, t3, 1), 5);
  assert.equal(domToCP(root, root, 1), 2, 'an element offset counts child nodes');
  assert.deepEqual(cpToDom(root, 2), { node: t1, offset: 3 }, 'a run boundary prefers the earlier run');
  assert.deepEqual(cpToDom(root, 4), { node: t2, offset: 2 });
  assert.deepEqual(cpToDom(root, 99), { node: t3, offset: 1 });
  for (let cp = 0; cp <= 5; cp++) { const p = cpToDom(root, cp); assert.equal(domToCP(root, p.node, p.offset), cp); }
});

test('labels marked contenteditable=false are not text', () => {
  installDOM();
  const root = document.createElement('div');
  const label = document.createElement('span');
  label.setAttribute('contenteditable', 'false');
  label.textContent = 'Note. ';
  root.append(label, ...renderRuns([r('body')]));
  assert.equal(domText(root), 'body');
  assert.equal(plainText(diffRuns([r('body')], domText(root))), 'body');
});
