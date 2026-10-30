// Shared helpers of the board performance harness: the measured Page, the
// board-ready probe, statistics and environment records, on top of the
// shared CDP module.

import { execFileSync } from 'node:child_process';
import { cpus, totalmem, release, platform } from 'node:os';
import { sleep } from '../lib/cdp.mjs';

// watchdog ends the process when a run takes longer than ms in total, so a
// lost tab or browser never leaves a run waiting forever.
export function watchdog(ms) {
  const t = setTimeout(() => { console.error(`watchdog: no result after ${Math.round(ms / 1000)} s`); process.exit(3); }, ms);
  t.unref();
}

// args parses --key value and --flag arguments.
export function args(defaults) {
  const out = { ...defaults };
  const a = process.argv.slice(2);
  for (let i = 0; i < a.length; i++) {
    if (!a[i].startsWith('--')) continue;
    const k = a[i].slice(2);
    const next = a[i + 1];
    if (next === undefined || next.startsWith('--')) out[k] = true;
    else { out[k] = next; i++; }
  }
  for (const [k, v] of Object.entries(defaults)) {
    if (typeof v === 'number' && typeof out[k] === 'string') out[k] = Number(out[k]);
  }
  return out;
}

// Page is one tab attached through a flattened session.
export class Page {
  constructor(cdp, sessionId, targetId) { this.cdp = cdp; this.sessionId = sessionId; this.targetId = targetId; }
  send(method, params) { return this.cdp.send(method, params, this.sessionId); }

  // first attaches to the tab Chrome opened at start.
  static async first(cdp) {
    let t = null;
    for (let i = 0; i < 50 && !t; i++) {
      const { targetInfos } = await cdp.send('Target.getTargets');
      t = targetInfos.find((x) => x.type === 'page');
      if (!t) await sleep(100);
    }
    if (!t) t = { targetId: (await cdp.send('Target.createTarget', { url: 'about:blank' })).targetId };
    const { sessionId } = await cdp.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
    const p = new Page(cdp, sessionId, t.targetId);
    await p.send('Page.enable');
    await p.send('Runtime.enable');
    return p;
  }

  async eval(expr, { awaitPromise = true, timeoutMs } = {}) {
    const p = this.send('Runtime.evaluate', { expression: expr, awaitPromise, returnByValue: true });
    let timer;
    const r = timeoutMs
      ? await Promise.race([p, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('evaluate timed out')), timeoutMs); })]).finally(() => clearTimeout(timer))
      : await p;
    if (r.exceptionDetails) throw new Error(`page: ${r.exceptionDetails.exception?.description || r.exceptionDetails.text}`);
    return r.result.value;
  }

  // evalLarge returns a large JSON value in slices: one DevTools message
  // with several megabytes closes the connection.
  async evalLarge(expr) {
    const n = await this.eval(`(window.__large = JSON.stringify(${expr})).length`);
    let out = '';
    for (let i = 0; i < n; i += 500000) out += await this.eval(`window.__large.slice(${i}, ${i + 500000})`);
    await this.eval('delete window.__large');
    return JSON.parse(out);
  }

  async navigate(url) {
    const loaded = this.cdp.waitFor((m) => m.sessionId === this.sessionId && m.method === 'Page.loadEventFired', 120000);
    await this.send('Page.navigate', { url });
    await loaded;
  }

  async setWindow(width, height) {
    const { windowId } = await this.cdp.send('Browser.getWindowForTarget', { targetId: this.targetId });
    await this.cdp.send('Browser.setWindowBounds', { windowId, bounds: { windowState: 'normal' } });
    await this.cdp.send('Browser.setWindowBounds', { windowId, bounds: { left: 40, top: 40, width, height } });
    return this.eval('({inner: [innerWidth, innerHeight], outer: [outerWidth, outerHeight], dpr: devicePixelRatio})');
  }
}

// readyExpr waits in the page until the board shows all of its expected
// elements: snapshot loaded, first fit applied, nothing left to draw.
export const READY_FN = `(expected) => {
  const b = window.board;
  if (!b || !b.welcomed || !b.firstFit || b.fitPending) return false;
  if (b.state.server.size < expected) return false;
  const r = b.renderer;
  return r.raf === 0 && !r.full && !!r.drawn;
}`;

export async function waitReady(page, expected, timeoutMs = 120000) {
  return page.eval(`new Promise((resolve, reject) => {
    const ready = ${READY_FN};
    const t0 = performance.now();
    const tick = () => {
      if (ready(${expected})) return resolve(performance.now());
      if (performance.now() - t0 > ${timeoutMs}) return reject(new Error('board not ready'));
      requestAnimationFrame(tick);
    };
    tick();
  })`);
}

// storageSetup is a script run before the page's own scripts: it gives the
// tab a stable editor name so the name prompt does not open.
export const nameScript = (name) => `try { localStorage.setItem('flats-board-name', ${JSON.stringify(name)}); } catch {}`;

export function stats(xs) {
  if (!xs.length) return { n: 0 };
  const s = [...xs].sort((a, b) => a - b);
  const q = (p) => s[Math.min(s.length - 1, Math.max(0, Math.ceil(p * s.length) - 1))];
  const mean = s.reduce((a, b) => a + b, 0) / s.length;
  const r = (v) => Math.round(v * 100) / 100;
  return { n: s.length, mean: r(mean), p50: r(q(0.5)), p95: r(q(0.95)), p99: r(q(0.99)), max: r(s[s.length - 1]), min: r(s[0]) };
}

export function uptime() {
  try { return execFileSync('uptime', { encoding: 'utf8' }).trim(); } catch { return 'unavailable'; }
}

export async function environment(cdp) {
  let browser = null;
  if (cdp) { try { browser = await cdp.send('Browser.getVersion'); } catch { /* none */ } }
  let os = `${platform()} ${release()}`;
  try { os = execFileSync('sw_vers', { encoding: 'utf8' }).replace(/\s+/g, ' ').trim(); } catch { /* not macOS */ }
  return {
    os, cpu: cpus()[0]?.model, cpus: cpus().length, memGiB: Math.round(totalmem() / 2 ** 30),
    node: process.version, browser: browser && browser.product, userAgent: browser && browser.userAgent, uptime: uptime(),
  };
}

// throttle applies Chrome network emulation to a page: rtt is added
// round-trip latency in ms, mbps the throughput in both directions.
export async function throttle(page, rtt, mbps) {
  await page.send('Network.enable');
  const bps = mbps ? (mbps * 1e6) / 8 : -1;
  await page.send('Network.emulateNetworkConditions', { offline: false, latency: rtt || 0, downloadThroughput: bps, uploadThroughput: bps });
}
