// measure.mjs computes the font size the board client draws for a text box,
// using the client's own fitText/wrap from internal/board/web/render.js.
//
// "chrome" runs that code in headless Chrome (real font metrics through
// OffscreenCanvas, over the DevTools protocol). "approx" runs the same code
// in Node with an estimated glyph width; use it only when Chrome is missing,
// and treat results near 12 px as uncertain.
'use strict';

import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { findChrome, launchChrome } from '../lib/cdp.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const RENDER = join(here, '../../internal/board/web/render.js');

// clientSource turns render.js into a script body that defines fitText,
// wrap and FONT without its imports (the text functions use none of them).
function clientSource() {
  return readFileSync(RENDER, 'utf8')
    .replace(/^import .*$/gm, '')
    .replace(/^export (?=const|function|class|async)/gm, '');
}

function program(items) {
  return `(() => {
${clientSource()}
const ctx = new OffscreenCanvas(1, 1).getContext('2d');
const items = ${JSON.stringify(items)};
const out = items.map((it) => {
  const r = fitText(ctx, it.text, it.maxW, it.maxH === null ? Infinity : it.maxH, it.size, it.shrink, it.weight || 400);
  return { size: r.size, overflow: r.overflow, lines: r.lines.length };
});
const labelWidths = items.map((it) => it.labelFont ? (ctx.font = it.labelFont, ctx.measureText(it.text).width) : null);
return out.map((r, i) => ({ ...r, width: labelWidths[i] }));
})()`;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// inChrome runs the program once in a headless Chrome the shared launcher
// owns: the page target Chrome opened at start evaluates it, so the result
// carries real font metrics.
async function inChrome(chrome, expr) {
  const browser = await launchChrome({ chrome, headless: true });
  try {
    let target = null;
    for (let i = 0; i < 100 && !target; i++) {
      const { targetInfos } = await browser.cdp.send('Target.getTargets');
      target = targetInfos.find((t) => t.type === 'page');
      if (!target) await sleep(100);
    }
    if (!target) throw new Error('no Chrome page target');
    const { sessionId } = await browser.cdp.send('Target.attachToTarget', { targetId: target.targetId, flatten: true });
    const d = await browser.cdp.send('Runtime.evaluate', { expression: expr, returnByValue: true }, sessionId);
    if (d.exceptionDetails) throw new Error('measure failed in Chrome: ' + JSON.stringify(d.exceptionDetails).slice(0, 400));
    return { values: d.result.value, engine: `chrome (${browser.version.product})` };
  } finally {
    await browser.close();
  }
}

// An estimated measureText: 0.56 em per glyph, 0.3 em per space, 1 em for
// wide (CJK) characters.
class ApproxCtx {
  set font(f) { this._size = Number(/(\d+(?:\.\d+)?)px/.exec(f)[1]); }
  get font() { return `${this._size}px`; }
  measureText(s) {
    let w = 0;
    for (const ch of s) w += /\s/.test(ch) ? 0.3 : ch.codePointAt(0) > 0x2e80 ? 1 : 0.56;
    return { width: w * this._size };
  }
}

function inNode(items) {
  const fn = new Function('OffscreenCanvas', `return ${program(items)};`);
  const values = fn(class { getContext() { return new ApproxCtx(); } });
  return { values, engine: 'approx (0.56 em per glyph; not real font metrics)' };
}

// measure returns, per item, {size, overflow, lines, width} and the engine used.
export async function measure(items, mode) {
  if (mode === 'approx') return inNode(items);
  const chrome = findChrome();
  if (!chrome) {
    if (mode === 'chrome') throw new Error('Chrome not found (set CHROME=/path/to/chrome)');
    return inNode(items);
  }
  return inChrome(chrome, program(items));
}
