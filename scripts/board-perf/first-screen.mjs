// First interactive screen (board.md success criteria, "First screen"),
// with a cold and a warm browser cache.
//
//   node first-screen.mjs --url http://<slug>.localhost:<port> --expected 5000 [--runs 5] [--headless]
//        [--rtt 50 --mbps 50] [--out result.json]
//
// --rtt and --mbps emulate the reference network with Chrome's network
// conditions (added round-trip latency and throughput in both directions).
//
// Each cold run starts Chrome with a new, empty profile (no HTTP cache, no
// storage) and opens the board once. Each warm run opens the board again in
// the browser of the previous cold run, after a first visit filled its cache.
//
// Times count from the navigation start of the board page:
// - ready: the first animation frame after which the whole snapshot is
//   loaded, the first fit is applied and the renderer has nothing left to
//   draw (every element of the view drawn; images may still be decoding).
// - interactive: at the ready frame the page dispatches a pan (a wheel
//   event); interactive is the frame that shows it. This is the reported
//   "first interactive screen" time.
// Also recorded: response end of the HTML, the WebSocket open, welcome and
// last snapshot part, and when every image in view finished decoding.

import { writeFileSync } from 'node:fs';
import { args, watchdog, launchChrome, nameScript, stats, environment, uptime, throttle, READY_FN } from './lib.mjs';

const opt = args({ url: '', expected: 5000, runs: 5, rtt: 0, mbps: 0, width: 1440, height: 900, scale: 0, chrome: '', out: '' });
if (!opt.url) { console.error('usage: node first-screen.mjs --url <board url> --expected <elements>'); process.exit(2); }
watchdog(opt.runs * 300000);

// probe runs in the page from document start.
const probe = (expected) => `(() => {
  const P = window.__perf = { ws: {}, parts: 0 };
  const Native = window.WebSocket;
  window.WebSocket = class extends Native {
    constructor(...a) {
      super(...a);
      if (P.ws.created == null) P.ws.created = performance.now();
      this.addEventListener('open', () => { if (P.ws.open == null) P.ws.open = performance.now(); });
      this.addEventListener('message', (e) => {
        const s = typeof e.data === 'string' ? e.data : '';
        if (P.ws.welcome == null && /"t":\s*"welcome"/.test(s)) P.ws.welcome = performance.now();
        if (/"t":\s*"snapshot"/.test(s)) { P.parts++; P.ws.lastSnapshotPart = performance.now(); }
      });
    }
  };
  const ready = ${READY_FN};
  const step = () => {
    const b = window.board;
    if (P.ready == null && ready(${expected})) {
      P.ready = performance.now();
      const host = document.querySelector('.board');
      const r = host.getBoundingClientRect();
      const cam = { ...b.cam };
      host.dispatchEvent(new WheelEvent('wheel', { deltaX: 40, deltaY: 0, clientX: r.left + r.width / 2, clientY: r.top + r.height / 2, bubbles: true, cancelable: true }));
      const wait = (ts) => {
        if (b.cam.x !== cam.x && b.renderer.raf === 0) { P.interactive = performance.now(); P.interactiveFrame = ts; return; }
        requestAnimationFrame(wait);
      };
      requestAnimationFrame(wait);
    }
    if (P.ready != null && P.imagesDone == null && b) {
      const ims = b.renderer.images;
      if (ims.loading.size === 0 && ims.entries.size > 0) P.imagesDone = performance.now();
    }
    if (P.interactive == null || P.imagesDone == null) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
})();`;

async function load(page) {
  await page.navigate(opt.url);
  const p = await page.eval(`new Promise((resolve, reject) => {
    const t0 = performance.now();
    const check = () => {
      const P = window.__perf;
      if (P && P.interactive != null && (P.imagesDone != null || performance.now() - t0 > 20000)) {
        const nav = performance.getEntriesByType('navigation')[0];
        return resolve({ ...P, responseEnd: nav && nav.responseEnd, domContentLoaded: nav && nav.domContentLoadedEventEnd,
          transfer: performance.getEntriesByType('resource').reduce((a, r) => a + (r.transferSize || 0), 0) + (nav ? nav.transferSize : 0),
          cachedResources: performance.getEntriesByType('resource').filter((r) => r.transferSize === 0 && r.decodedBodySize > 0).length,
          resources: performance.getEntriesByType('resource').length, elements: window.board.state.server.size, zoom: window.board.cam.z });
      }
      if (performance.now() - t0 > 120000) return reject(new Error('not interactive within 120 s'));
      setTimeout(check, 100);
    };
    check();
  })`);
  const r = (v) => (v == null ? null : Math.round(v));
  return { ready: r(p.ready), interactive: r(p.interactive), responseEnd: r(p.responseEnd), domContentLoaded: r(p.domContentLoaded),
    wsCreated: r(p.ws.created), wsOpen: r(p.ws.open), welcome: r(p.ws.welcome), lastSnapshotPart: r(p.ws.lastSnapshotPart),
    snapshotParts: p.parts, imagesDone: r(p.imagesDone), transferBytes: p.transfer, cachedResources: p.cachedResources,
    resources: p.resources, elements: p.elements, zoom: p.zoom };
}

const cold = [], warm = [];
let env = null, viewport = null;
const before = uptime();
for (let i = 0; i < opt.runs; i++) {
  const chrome = await launchChrome({ headless: !!opt.headless, width: opt.width, height: opt.height, scale: opt.scale || 0, chrome: opt.chrome || undefined });
  try {
    const page = await chrome.cdp.firstPage();
    await page.send('Page.addScriptToEvaluateOnNewDocument', { source: nameScript('perf-viewer') + '\n' + probe(opt.expected) });
    viewport = await page.setWindow(opt.width, opt.height);
    if (opt.rtt || opt.mbps) await throttle(page, opt.rtt, opt.mbps);
    env ||= await environment(chrome.cdp);
    const c = await load(page);
    cold.push(c);
    await page.navigate('about:blank');
    const w = await load(page);
    warm.push(w);
    console.error(`run ${i + 1}: cold interactive ${c.interactive} ms (ready ${c.ready}), warm interactive ${w.interactive} ms (ready ${w.ready})`);
  } finally {
    await chrome.close();
  }
}
const pick = (xs, k) => stats(xs.map((x) => x[k]).filter((v) => v != null));
const result = {
  kind: 'first-screen', url: opt.url, throttle: { rttMs: opt.rtt, mbps: opt.mbps }, expected: opt.expected, runs: opt.runs, headless: !!opt.headless, env, viewport,
  uptimeBefore: before, uptimeAfter: uptime(),
  cold: { interactive: pick(cold, 'interactive'), ready: pick(cold, 'ready'), lastSnapshotPart: pick(cold, 'lastSnapshotPart'), imagesDone: pick(cold, 'imagesDone') },
  warm: { interactive: pick(warm, 'interactive'), ready: pick(warm, 'ready'), lastSnapshotPart: pick(warm, 'lastSnapshotPart'), imagesDone: pick(warm, 'imagesDone') },
  runsCold: cold, runsWarm: warm,
};
console.log(JSON.stringify(result, null, 1));
if (opt.out) writeFileSync(opt.out, JSON.stringify(result, null, 1));

// Leftover sockets or timers must not keep the process alive.
process.exit(process.exitCode || 0);
