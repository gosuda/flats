// Pan and zoom frame times (board.md success criteria, "Pan and zoom"):
// opens a board in Chrome, waits until every element is loaded and drawn,
// then runs the fixed 60-second interaction script of driver() below and
// reports frame times.
//
//   node pan-zoom.mjs --url http://<slug>.localhost:<port> --expected 5000 [--headless] [--scale 2] [--out result.json]
//
// Frame time is the interval between consecutive animation frames
// (requestAnimationFrame timestamps) while the script runs: the time the
// screen takes to show the next picture. On a 60 Hz display it sits near
// 16.7 ms with about a millisecond of timestamp jitter, so the result also
// counts missed frames (an interval over 1.5 display periods) and the share
// of frames shown on time. "work" is the script time spent in each frame
// before the driver's own callback (the board's renderer runs first in a
// frame, so this is the renderer's drawing time). Pan deltas follow the
// elapsed time and are usually fractional; the renderer shifts its bitmap
// only for whole device-pixel moves, so such frames redraw the whole view
// (the harder case).

import { writeFileSync } from 'node:fs';
import { args, watchdog, launchChrome, waitReady, nameScript, stats, environment, uptime } from './lib.mjs';

const opt = args({ url: '', expected: 5000, seconds: 60, width: 1440, height: 900, scale: 0, chrome: '', out: '' });
if (!opt.url) { console.error('usage: node pan-zoom.mjs --url <board url> --expected <elements>'); process.exit(2); }
watchdog((opt.seconds + 300) * 1000);

// driver runs in the page. It dispatches one wheel event per frame on the
// board, moving the view at a speed set by the phase table (so a slow frame
// moves the view further, as a hand does), and records every frame.
function driver(seconds) {
  const b = window.board;
  const host = document.querySelector('.board');
  const rect = host.getBoundingClientRect();
  const cx = rect.left + rect.width / 2, cy = rect.top + rect.height / 2;
  const wheel = (dx, dy, ctrl) => host.dispatchEvent(new WheelEvent('wheel', {
    deltaX: dx, deltaY: dy, ctrlKey: !!ctrl, clientX: cx, clientY: cy, deltaMode: 0, bubbles: true, cancelable: true,
  }));
  // Phases: [start s, end s, kind, a, b]. pan: a, b = px/s on screen in x, y.
  // zoom: a = zoom factor per second (ln), b = pause ms between 2 s bursts.
  // to: a = target zoom, reached by ctrl+wheel at the gesture speed.
  const T = seconds / 60;
  const phases = [
    [0, 8, 'pan', 300, 200],          // whole board in view: slow pan
    [8, 10, 'to', 1],                  // zoom in to 100%
    [10, 20, 'pan', 900, 0],           // 100%: pan right across stickies
    [20, 30, 'pan', -400, 700],        // 100%: pan down and left
    [30, 32, 'to', 0.5],
    [32, 40, 'pan', 1200, -500],       // 50%
    [40, 50, 'zoom', 1.4, 300],        // zoom in and out between 25% and 200%
    [50, 52, 'to', 0.25],
    [52, 60, 'pan', -1600, 400],       // 25%: fast pan
  ].map(([s, e, k, a, bb]) => [s * T, e * T, k, a, bb]);
  b.fitAll();
  const frames = [];
  const errors = [];
  const onErr = (e) => errors.push(String(e.message || e.reason || e));
  addEventListener('error', onErr);
  addEventListener('unhandledrejection', onErr);
  const loaf = [];
  let obs = null;
  try {
    obs = new PerformanceObserver((l) => { for (const e of l.getEntries()) loaf.push({ d: e.duration, blocking: e.blockingDuration }); });
    obs.observe({ type: 'long-animation-frame', buffered: false });
  } catch { /* not supported */ }
  return new Promise((resolve) => {
    let t0 = null, last = null, zoomDir = 1;
    const tick = (ts) => {
      const work = performance.now() - ts;
      if (t0 == null) t0 = ts;
      const t = (ts - t0) / 1000;
      if (last != null) {
        const ph = phases.find((p) => t >= p[0] && t < p[1]);
        frames.push({ t, dt: ts - last, work, z: b.cam.z, phase: ph ? phases.indexOf(ph) : -1, img: b.renderer.images.entries.size, loading: b.renderer.images.loading.size });
      }
      const dt = last == null ? 1 / 60 : Math.min(0.25, (ts - last) / 1000);
      last = ts;
      if (t >= seconds) {
        removeEventListener('error', onErr);
        removeEventListener('unhandledrejection', onErr);
        if (obs) obs.disconnect();
        resolve({ frames, errors, loaf, final: { ...b.cam } });
        return;
      }
      const ph = phases.find((p) => t >= p[0] && t < p[1]);
      if (ph) {
        const [s, , kind, a, bb] = ph;
        if (kind === 'pan') wheel(-a * dt, -bb * dt, false);
        else if (kind === 'to') {
          const want = Math.log(a / b.cam.z);
          const step = Math.max(-3 * dt, Math.min(3 * dt, want)); // at most e^3 per second
          if (Math.abs(want) > 0.01) wheel(0, -step * 100, true);
        } else if (kind === 'zoom') {
          const inBurst = ((t - s) % 2) < 2 - bb / 1000;
          if (inBurst) {
            if (b.cam.z > 2) zoomDir = -1;
            if (b.cam.z < 0.25) zoomDir = 1;
            wheel(0, -zoomDir * a * dt * 100, true);
          }
        }
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
}

const env0 = uptime();
const chrome = await launchChrome({ headless: !!opt.headless, width: opt.width, height: opt.height, scale: opt.scale || 0, chrome: opt.chrome || undefined });
const exceptions = [];
try {
  const page = await chrome.cdp.firstPage();
  chrome.cdp.on((m) => {
    if (m.method === 'Runtime.exceptionThrown') exceptions.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
    if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') exceptions.push('console.error: ' + m.params.args.map((a) => a.value ?? a.description).join(' '));
  });
  await page.send('Page.addScriptToEvaluateOnNewDocument', { source: nameScript('perf-viewer') });
  const viewport = await page.setWindow(opt.width, opt.height);
  await page.navigate(opt.url);
  const readyAt = await waitReady(page, opt.expected);
  // Let image decoding and idle work settle before measuring.
  await page.eval('new Promise((r) => setTimeout(r, 3000))');
  const loads = await page.eval('({images: window.board.renderer.images.entries.size, failed: window.board.renderer.images.failed.size, elements: window.board.state.server.size, zoom: window.board.cam.z})');
  const r = await page.eval(`(${driver})(${opt.seconds})`, { timeoutMs: (opt.seconds + 120) * 1000 });
  const all = r.frames.filter((f) => f.t >= 0.5); // skip the first fit frames
  const byPhase = {};
  for (const f of all) (byPhase[f.phase] ||= []).push(f.dt);
  // The display's frame period is the median interval. A frame "missed" the
  // display when its interval exceeds 1.5 periods (a whole refresh skipped).
  const vsync = stats(all.map((f) => f.dt)).p50;
  const missed = all.filter((f) => f.dt > 1.5 * vsync).length;
  const result = {
    kind: 'pan-zoom', url: opt.url, expected: opt.expected, seconds: opt.seconds, headless: !!opt.headless,
    env: await environment(chrome.cdp), viewport, uptimeBefore: env0, uptimeAfter: uptime(), readyAtMs: Math.round(readyAt), loads,
    frameTime: stats(all.map((f) => f.dt)), work: stats(all.map((f) => f.work)),
    displayPeriodMs: vsync, missedFrames: missed, onTimeShare: Math.round((1 - missed / all.length) * 10000) / 100,
    over16_7: all.filter((f) => f.dt > 16.7).length, over33: all.filter((f) => f.dt > 33).length, over100: all.filter((f) => f.dt > 100).length,
    phases: Object.fromEntries(Object.entries(byPhase).map(([k, v]) => [k, stats(v)])),
    longAnimationFrames: { n: r.loaf.length, maxMs: Math.max(0, ...r.loaf.map((x) => x.d)), blockingMs: Math.round(r.loaf.reduce((a, x) => a + (x.blocking || 0), 0)) },
    pageErrors: r.errors, exceptions, finalCamera: r.final,
  };
  console.log(JSON.stringify({ ...result, phases: undefined }, null, 1));
  if (opt.out) writeFileSync(opt.out, JSON.stringify({ ...result, rawFrames: r.frames }, null, 1));
} finally {
  await chrome.close();
}

// Leftover sockets or timers must not keep the process alive.
process.exit(process.exitCode || 0);
