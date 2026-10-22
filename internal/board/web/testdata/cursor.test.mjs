import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { SelectTool } from '../tools/select.js';

test('select hover shows a handle cursor, but not while Space is held', () => {
  const app = { space: false, renderer: { dyn: { style: { cursor: '' } } } };
  const tool = new SelectTool(app);
  tool.handleAt = () => ({ kind: 'nw' });
  tool.hover(null, [0, 0], [0, 0]);
  assert.equal(app.renderer.dyn.style.cursor, 'nwse-resize');
  app.space = true;
  tool.hover(null, [0, 0], [0, 0]);
  assert.equal(app.renderer.dyn.style.cursor, '', 'the inline cursor would hide the Space grab cursor');
});

test('Space keydown clears an inline handle cursor without waiting for a hover', () => {
  // Given — app.js starts the board when imported, so the test compiles App.key from its source
  const src = readFileSync(new URL('../app.js', import.meta.url), 'utf8');
  const start = src.indexOf('\n  key(e) {');
  const end = src.indexOf('\n  }\n', start);
  assert.ok(start > 0 && end > start, 'App.key not found in app.js');
  const d = { style: { cursor: 'nwse-resize' }, classList: new Set() };
  const $ = (sel) => (sel === '.layer-dynamic' ? d : null);
  const key = new Function('$', 'document', `return {${src.slice(start, end + 4)}}.key;`)($, { querySelector: () => null });
  const app = { space: false, typing: () => false };
  let prevented = false;

  // When — Space goes down with the pointer still over the handle
  key.call(app, { key: ' ', metaKey: false, ctrlKey: false, preventDefault() { prevented = true; } });

  // Then — the Space class can show the grab cursor at once
  assert.equal(d.style.cursor, '', 'the inline handle cursor would hide the Space grab cursor until the pointer moves');
  assert.ok(app.space && d.classList.has('space') && prevented);
});

test('the Space and panning cursors are the last canvas cursor rules in app.css', () => {
  // The rules have equal specificity, so the later rule wins.
  const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  const rules = [...css.matchAll(/([^{}]+)\{([^}]*)\}/g)]
    .map(([, sel, body]) => ({ sel: sel.trim(), body }))
    .filter((r) => r.sel.includes('.layer-dynamic') && /(^|;)\s*cursor\s*:/.test(r.body));
  const last = rules.slice(-2).map((r) => r.sel);
  assert.ok(last[0].includes('.layer-dynamic.space'), `next to last: ${last[0]}`);
  assert.ok(last[1].includes('.layer-dynamic.panning'), `last: ${last[1]}`);
  for (const tool of ['sticky', 'text', 'shape', 'frame', 'connector', 'pen', 'eraser']) {
    assert.ok(rules.some((r) => r.sel.includes(`.layer-dynamic[data-tool=${tool}]`)), `${tool} has a cursor rule`);
  }
});
