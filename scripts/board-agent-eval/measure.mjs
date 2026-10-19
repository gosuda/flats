// measure.mjs computes the font size the board client draws for a text box,
// using the client's own fitText/wrap from internal/board/web/render.js.
//
// "chrome" runs that code in headless Chrome (real font metrics through
// OffscreenCanvas, over the DevTools protocol). "approx" runs the same code
// in Node with an estimated glyph width; use it only when Chrome is missing,
// and treat results near 12 px as uncertain.

import { readFileSync, mkdtempSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

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

const CHROME = [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
].filter(Boolean);

export function findChrome() {
  return CHROME.find((p) => existsSync(p)) || null;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function inChrome(chrome, expr) {
  const profile = mkdtempSync(join(tmpdir(), 'board-eval-chrome-'));
  const proc = spawn(chrome, ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`,
    '--no-first-run', '--no-default-browser-check', '--disable-extensions', 'about:blank'], { stdio: 'ignore' });
  try {
    let port = null;
    for (let i = 0; i < 300 && !port; i++) {
      await sleep(100);
      try { port = readFileSync(join(profile, 'DevToolsActivePort'), 'utf8').split('\n')[0]; } catch { /* not yet */ }
    }
    if (!port) throw new Error('headless Chrome did not start');
    let page = null;
    for (let i = 0; i < 100 && !page; i++) {
      const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
      page = list.find((t) => t.type === 'page');
      if (!page) await sleep(100);
    }
    if (!page) throw new Error('no Chrome page target');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    const reply = new Promise((res) => {
      ws.onmessage = (m) => { const d = JSON.parse(m.data); if (d.id === 1) res(d); };
    });
    ws.send(JSON.stringify({ id: 1, method: 'Runtime.evaluate', params: { expression: expr, returnByValue: true } }));
    const d = await reply;
    ws.close();
    if (d.result.exceptionDetails) throw new Error('measure failed in Chrome: ' + JSON.stringify(d.result.exceptionDetails).slice(0, 400));
    const ua = (await (await fetch(`http://127.0.0.1:${port}/json/version`)).json()).Browser;
    return { values: d.result.result.value, engine: `chrome (${ua})` };
  } finally {
    proc.kill();
    await sleep(200);
    rmSync(profile, { recursive: true, force: true });
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
