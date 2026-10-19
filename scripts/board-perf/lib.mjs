// Shared helpers of the board performance harness: launching Chrome with a
// throwaway profile, a minimal Chrome DevTools Protocol client over the
// global WebSocket (Node 22+), statistics and environment records.

import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir, cpus, totalmem, release, platform } from 'node:os';
import { join } from 'node:path';

// NativeWebSocket is captured before any script replaces the global.
const NativeWebSocket = globalThis.WebSocket;

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// watchdog ends the process when a run takes longer than ms in total, so a
// lost tab or browser never leaves a run waiting forever.
// launched holds the Chromes this process started; any exit, including a
// crash or the watchdog, kills them and removes their profiles.
const launched = new Set();
process.on('exit', () => {
  for (const c of launched) {
    try { c.proc.kill('SIGKILL'); } catch { /* gone */ }
    try { rmSync(c.profile, { recursive: true, force: true }); } catch { /* gone */ }
  }
});

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

export const DEFAULT_CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

// launchChrome starts Chrome with its own temporary profile and returns
// {cdp, proc, profile, close}. Options: chrome (path), headless, width,
// height (window size), scale (device scale factor; omit for the display's).
export async function launchChrome(opt = {}) {
  const profile = mkdtempSync(join(tmpdir(), 'board-perf-chrome-'));
  const argv = [
    `--user-data-dir=${profile}`, '--remote-debugging-port=0', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-sync', '--password-store=basic',
    '--use-mock-keychain', '--disable-backgrounding-occluded-windows', '--disable-renderer-backgrounding',
    '--disable-background-timer-throttling', '--disable-features=Translate,MediaRouter,OptimizationHints',
    `--window-size=${opt.width || 1440},${opt.height || 900}`, '--window-position=40,40',
  ];
  if (opt.headless) argv.push('--headless=new');
  if (opt.scale) argv.push(`--force-device-scale-factor=${opt.scale}`);
  argv.push('about:blank');
  const proc = spawn(opt.chrome || DEFAULT_CHROME, argv, { stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  proc.stderr.on('data', (d) => { stderr = (stderr + d).slice(-4000); });
  const portFile = join(profile, 'DevToolsActivePort');
  for (let i = 0; i < 300 && !existsSync(portFile); i++) await sleep(100);
  if (!existsSync(portFile)) { proc.kill(); rmSync(profile, { recursive: true, force: true }); throw new Error('Chrome did not start: ' + stderr); }
  let lines = [];
  for (let i = 0; i < 50 && lines.length < 2; i++) { lines = readFileSync(portFile, 'utf8').trim().split('\n'); if (lines.length < 2) await sleep(100); }
  const entry = { proc, profile };
  launched.add(entry);
  const cdp = await CDP.connect(`ws://127.0.0.1:${lines[0]}${lines[1]}`);
  const close = async () => {
    // Chrome may close the socket before it answers Browser.close.
    await Promise.race([cdp.send('Browser.close').catch(() => {}), sleep(3000)]);
    for (let i = 0; i < 50 && proc.exitCode == null; i++) await sleep(100);
    if (proc.exitCode == null) proc.kill('SIGKILL');
    cdp.close();
    rmSync(profile, { recursive: true, force: true });
    launched.delete(entry);
  };
  return { cdp, proc, profile, close };
}

// CDP is a flat-session Chrome DevTools Protocol client.
export class CDP {
  static connect(url) {
    return new Promise((resolve, reject) => {
      const ws = new NativeWebSocket(url);
      ws.onopen = () => resolve(new CDP(ws));
      ws.onerror = (e) => reject(new Error('CDP connect failed: ' + (e.message || url)));
    });
  }

  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.calls = new Map();
    this.listeners = [];
    ws.onclose = () => {
      for (const c of this.calls.values()) c.reject(new Error(`${c.method}: DevTools connection closed`));
      this.calls.clear();
    };
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id != null) {
        const c = this.calls.get(m.id);
        if (!c) return;
        this.calls.delete(m.id);
        if (m.error) c.reject(new Error(`${c.method}: ${m.error.message}`));
        else c.resolve(m.result);
      } else {
        // A closed tab never answers: fail its pending calls.
        if (m.method === 'Target.detachedFromTarget') {
          for (const [id, c] of this.calls) if (c.sessionId === m.params.sessionId) { this.calls.delete(id); c.reject(new Error(`${c.method}: the tab was closed`)); }
        }
        for (const l of this.listeners) l(m);
      }
    };
  }

  send(method, params = {}, sessionId) {
    const id = ++this.id;
    const msg = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      this.calls.set(id, { resolve, reject, method, sessionId });
      this.ws.send(JSON.stringify(msg));
    });
  }

  on(fn) { this.listeners.push(fn); return () => { this.listeners = this.listeners.filter((l) => l !== fn); }; }

  waitFor(pred, timeoutMs = 60000) {
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => { off(); reject(new Error('CDP wait timed out')); }, timeoutMs);
      const off = this.on((m) => { if (pred(m)) { clearTimeout(t); off(); resolve(m); } });
    });
  }

  close() { try { this.ws.close(); } catch { /* closed */ } }

  // page opens a new tab and returns a Page bound to its session.
  async page(width, height) {
    const { targetId } = await this.send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await this.send('Target.attachToTarget', { targetId, flatten: true });
    const p = new Page(this, sessionId, targetId);
    await p.send('Page.enable');
    await p.send('Runtime.enable');
    if (width && height) {
      const { windowId } = await this.send('Browser.getWindowForTarget', { targetId });
      await this.send('Browser.setWindowBounds', { windowId, bounds: { windowState: 'normal' } });
      await this.send('Browser.setWindowBounds', { windowId, bounds: { width, height } });
    }
    return p;
  }

  // firstPage attaches to the tab Chrome opened at start.
  async firstPage() {
    let t = null;
    for (let i = 0; i < 50 && !t; i++) {
      const { targetInfos } = await this.send('Target.getTargets');
      t = targetInfos.find((x) => x.type === 'page');
      if (!t) await sleep(100);
    }
    if (!t) t = { targetId: (await this.send('Target.createTarget', { url: 'about:blank' })).targetId };
    const { sessionId } = await this.send('Target.attachToTarget', { targetId: t.targetId, flatten: true });
    const p = new Page(this, sessionId, t.targetId);
    await p.send('Page.enable');
    await p.send('Runtime.enable');
    return p;
  }
}

export class Page {
  constructor(cdp, sessionId, targetId) { this.cdp = cdp; this.sessionId = sessionId; this.targetId = targetId; }
  send(method, params) { return this.cdp.send(method, params, this.sessionId); }

  async eval(expr, { awaitPromise = true, timeoutMs } = {}) {
    const p = this.send('Runtime.evaluate', { expression: expr, awaitPromise, returnByValue: true });
    let timer;
    const r = timeoutMs
      ? await Promise.race([p, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('evaluate timed out')), timeoutMs); })]).finally(() => clearTimeout(timer))
      : await p;
    if (r.exceptionDetails) throw new Error('page: ' + (r.exceptionDetails.exception?.description || r.exceptionDetails.text));
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

export function loadAvg() {
  const m = /load averages?: ([\d.]+),? ([\d.]+),? ([\d.]+)/.exec(uptime());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
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
