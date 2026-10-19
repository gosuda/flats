// Pan and zoom frame times (board.md success criteria, "Pan and zoom"):
// opens a board in Chrome, waits until every element is loaded and drawn,
// then runs the fixed 60-second interaction script of driver() below,
// reports frame times, and checks that the view stayed over content.
//
//   node pan-zoom.mjs --url http://<slug>.localhost:<port> --expected 5000 [--headless] [--scale 2]
//        [--seconds 60] [--floor 0.2] [--min-ref 5] [--out result.json]
//
// Frame time is the interval between consecutive animation frames
// (requestAnimationFrame timestamps) while the script runs: the time the
// screen takes to show the next picture. On a 60 Hz display it sits near
// 16.7 ms with about a millisecond of timestamp jitter, so the result also
// counts missed frames (an interval over 1.5 display periods) and the share
// of frames shown on time. "work" is the script time spent in each frame
// before the driver's own callback (the board's renderer runs first in a
// frame, so this is the renderer's drawing time; raster and compositing are
// not included). Pan deltas follow the elapsed time and are usually
// fractional; the renderer shifts its bitmap only for whole device-pixel
// moves, so such frames redraw the whole view (the harder case).
//
// Content guard. A frame time measured over empty canvas says nothing about
// drawing the board, so the script keeps the view over content and checks
// it:
// - Every phase starts at a known place: the first at the board's own
//   "fit all", every later one centred on the anchor (in the central half
//   of the content, the sticky with the most elements in a 100% view around
//   it), keeping the current zoom. The jump is one frame and is counted in
//   that phase's frame times.
// - Pans keep their speed but steer: when the next step would take the
//   view centre out of the content bounds (inset by half a view, or by a
//   quarter of the content when the view is wider than half of it), or the
//   estimated elements in view would fall under half of the phase start's
//   estimate, the pan turns to whichever of its four directions (each
//   axis kept or reversed) has the most elements ahead. The estimate is a summed-area table of
//   element centres on a 64-unit grid, O(1) per frame.
// - Zooms are ctrl+wheel at the centre of the view, which is over content.
// - Each frame records the camera (x, y, zoom) it shows. After the script,
//   the page counts the elements in view for every frame with the
//   renderer's own spatial-index query over the recorded view rectangle
//   (frames, which are containers, are not counted). Counting afterwards
//   keeps the measured frames free of extra work; the viewer edits nothing,
//   and the run fails if the board's state changed while it ran.
// - Each phase has a reference count: the elements in view at its start
//   position. A frame passes when it shows at least --floor (default 20%)
//   of the reference, scaled down by the area ratio (z_ref / z)^2 when the
//   frame is zoomed in further than the reference. A reference under
//   --min-ref elements fails the phase. Any failing frame fails the run:
//   the summary has "guard": {"pass": false, ...} and the exit status is 1.

import { writeFileSync } from 'node:fs';
import { args, watchdog, launchChrome, waitReady, nameScript, stats, environment, uptime } from './lib.mjs';

const opt = args({ url: '', expected: 5000, seconds: 60, floor: 0.2, 'min-ref': 5, width: 1440, height: 900, scale: 0, chrome: '', out: '' });
if (!opt.url) { console.error('usage: node pan-zoom.mjs --url <board url> --expected <elements>'); process.exit(2); }
watchdog((opt.seconds + 300) * 1000);

// PHASES is the interaction script: [start s, end s, kind, a, b, name] on a
// 60-second scale (--seconds scales it). pan: a, b = view speed in screen
// px/s in x, y (the sign is the initial direction; steering turns it).
// to: a = target zoom, reached by ctrl+wheel at most e^3 per second
// (per scaled second when --seconds is not 60).
// zoom: a = zoom speed (ln per second), b = pause in ms at the end of each
// 2 s burst; it turns around at 200% and 25%, at the centre of the view.
const PHASES = [
  [0, 8, 'pan', 300, 200, 'fit all: slow pan'],
  [8, 10, 'to', 1, 0, 'zoom to 100%'],
  [10, 20, 'pan', 900, 0, '100%: pan across'],
  [20, 30, 'pan', -400, 700, '100%: diagonal pan'],
  [30, 32, 'to', 0.5, 0, 'zoom to 50%'],
  [32, 40, 'pan', 1200, -500, '50%: pan'],
  [40, 50, 'zoom', 1.4, 300, 'zoom 25%..200%'],
  [50, 52, 'to', 0.25, 0, 'zoom to 25%'],
  [52, 60, 'pan', -1600, 400, '25%: fast pan'],
];

// driver runs in the page. It dispatches one wheel event per frame on the
// board, moving the view at a speed set by the phase table (so a slow frame
// moves the view further, as a hand does), and records every frame. After
// the script it counts the elements in view per frame (see the guard above).
function driver(seconds, table, floorShare, minRef) {
  const b = window.board;
  const host = document.querySelector('.board');
  const rect = host.getBoundingClientRect();
  const cx = rect.left + rect.width / 2, cy = rect.top + rect.height / 2;
  const wheel = (dx, dy, ctrl) => host.dispatchEvent(new WheelEvent('wheel', {
    deltaX: dx, deltaY: dy, ctrlKey: !!ctrl, clientX: cx, clientY: cy, deltaMode: 0, bubbles: true, cancelable: true,
  }));
  const T = seconds / 60;
  const phases = table.map(([s, e, k, a, bb, name]) => ({ s: s * T, e: e * T, kind: k, a, b: bb, name }));
  const W = b.renderer.w, H = b.renderer.h;
  const R = b.contentBounds();
  const content = (id) => { const el = b.doc.get(id); return !!el && el.type !== 'frame'; };
  const countAt = (x, y, z) => { let n = 0; for (const id of b.doc.index.query({ x, y, w: W / z, h: H / z })) if (content(id)) n++; return n; };
  // est estimates the elements in a view from a summed-area table of
  // element centres on a 64-unit grid: O(1) per call, so the pans can steer
  // by it every frame without adding measurable work.
  const C = 64, gx = Math.ceil(R.w / C) + 1, gy = Math.ceil(R.h / C) + 1;
  const sat = new Float64Array((gx + 1) * (gy + 1));
  for (const el of b.doc.all()) {
    if (el.type === 'frame') continue;
    const r = b.doc.index.bounds(el.id);
    if (!r) continue;
    const i = Math.min(gx - 1, Math.max(0, Math.floor((r.x + r.w / 2 - R.x) / C)));
    const j = Math.min(gy - 1, Math.max(0, Math.floor((r.y + r.h / 2 - R.y) / C)));
    sat[(j + 1) * (gx + 1) + (i + 1)] += 1;
  }
  for (let j = 1; j <= gy; j++) for (let i = 1; i <= gx; i++) sat[j * (gx + 1) + i] += sat[(j - 1) * (gx + 1) + i] + sat[j * (gx + 1) + i - 1] - sat[(j - 1) * (gx + 1) + i - 1];
  const cell = (v, n) => Math.min(n, Math.max(0, Math.floor(v / C)));
  const est = (x, y, z) => {
    const i0 = cell(x - R.x, gx), i1 = cell(x + W / z - R.x, gx), j0 = cell(y - R.y, gy), j1 = cell(y + H / z - R.y, gy);
    const at = (i, j) => sat[j * (gx + 1) + i];
    return at(i1, j1) - at(i0, j1) - at(i1, j0) + at(i0, j0);
  };
  // The anchor: in the central half of the content, the sticky with the
  // most elements in a 100% view around it.
  const mid = [R.x + R.w / 2, R.y + R.h / 2];
  let anchor = null;
  for (const central of [true, false]) {
    for (const el of b.doc.all()) {
      if (el.type !== 'sticky') continue;
      const ax = el.x + el.w / 2, ay = el.y + el.h / 2;
      if (central && (Math.abs(ax - mid[0]) > R.w / 4 || Math.abs(ay - mid[1]) > R.h / 4)) continue;
      const n = est(ax - W / 2, ay - H / 2, 1), d = Math.hypot(ax - mid[0], ay - mid[1]);
      if (!anchor || n > anchor.n || (n === anchor.n && d < anchor.d)) anchor = { x: ax, y: ay, n, d };
    }
    if (anchor) break;
  }
  const total = [...b.doc.all()].filter((el) => el.type !== 'frame').length;
  const stateBefore = { watermark: b.state.watermark, elements: b.state.server.size, pending: b.state.pending.length };

  b.fitAll();
  const frames = [];
  const starts = []; // camera each phase starts at
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
    let t0 = null, last = null, zoomDir = 1, cur = -1, shown = -1, dirX = 1, dirY = 1, jump = false, refEst = 0, turns = 0;
    const tick = (ts) => {
      const work = performance.now() - ts;
      if (t0 == null) t0 = ts;
      const t = (ts - t0) / 1000;
      const pi = phases.findIndex((p) => t >= p.s && t < p.e);
      // The camera of this frame is the one the previous callback set:
      // shown is the phase that set it, jump whether that was a phase start.
      if (last != null) {
        const c = b.cam;
        frames.push({ t, dt: ts - last, work, x: c.x, y: c.y, z: c.z, phase: pi, camPhase: shown, jump, img: b.renderer.images.entries.size, loading: b.renderer.images.loading.size });
      }
      const dt = last == null ? 1 / 60 : Math.min(0.25, (ts - last) / 1000);
      last = ts;
      jump = false;
      if (t >= seconds) {
        removeEventListener('error', onErr);
        removeEventListener('unhandledrejection', onErr);
        if (obs) obs.disconnect();
        finish();
        return;
      }
      if (pi >= 0 && pi !== cur) {
        cur = pi;
        dirX = 1; dirY = 1;
        if (pi === 0) b.fitAll();
        else { const z = b.cam.z; b.setCam(anchor.x - W / 2 / z, anchor.y - H / 2 / z, z); }
        starts[pi] = { ...b.cam };
        refEst = est(b.cam.x, b.cam.y, b.cam.z);
        jump = true;
      }
      shown = pi;
      const ph = phases[pi];
      if (ph) {
        if (ph.kind === 'pan') {
          // Steer: keep the speed, but turn an axis around when the next
          // step would leave the content bounds (inset by half a view, or a
          // quarter of the content when the view is wider than half of it)
          // or when the estimated elements in view would fall under half of
          // the phase start's estimate. Of the four directions, the one with
          // the most elements in view wins.
          const z = b.cam.z, vw = W / z, vh = H / z;
          const mx = Math.min(vw / 2, R.w / 4), my = Math.min(vh / 2, R.h / 4);
          const sx = (ph.a * dt) / z, sy = (ph.b * dt) / z;
          const score = (ddx, ddy) => {
            const nx = b.cam.x + ddx * sx, ny = b.cam.y + ddy * sy;
            const ccx = nx + vw / 2, ccy = ny + vh / 2;
            if (ccx < R.x + mx || ccx > R.x + R.w - mx || ccy < R.y + my || ccy > R.y + R.h - my) return -1;
            return est(nx, ny, z);
          };
          const ahead = score(dirX, dirY);
          if (ahead < 0.5 * refEst) {
            let best = [dirX, dirY, ahead];
            for (const [ddx, ddy] of [[-dirX, dirY], [dirX, -dirY], [-dirX, -dirY]]) {
              const v = score(ddx, ddy);
              if (v > best[2]) best = [ddx, ddy, v];
            }
            if (best[0] !== dirX || best[1] !== dirY) turns++;
            dirX = best[0]; dirY = best[1];
          }
          // A wheel delta of d screen px moves the camera by d / z.
          wheel(dirX * sx * z, dirY * sy * z, false);
        } else if (ph.kind === 'to') {
          const want = Math.log(ph.a / b.cam.z);
          const step = Math.max(-3 * dt / T, Math.min(3 * dt / T, want)); // at most e^3 per (scaled) second
          if (Math.abs(want) > 0.01) wheel(0, -step * 100, true);
        } else if (ph.kind === 'zoom') {
          const inBurst = ((t - ph.s) % 2) < 2 - ph.b / 1000;
          if (inBurst) {
            if (b.cam.z > 2) zoomDir = -1;
            if (b.cam.z < 0.25) zoomDir = 1;
            wheel(0, -zoomDir * ph.a * dt * 100, true);
          }
        }
      }
      requestAnimationFrame(tick);
    };
    // finish counts the elements in view per frame and applies the guard.
    const finish = () => {
      const refs = phases.map((p, i) => (starts[i] ? { ...starts[i], n: countAt(starts[i].x, starts[i].y, starts[i].z) } : null));
      const guard = phases.map((p, i) => ({ phase: i, name: p.name, ref: refs[i], frames: 0, below: 0, minInView: null, minAt: null, inView: [] }));
      for (const f of frames) {
        f.inView = countAt(f.x, f.y, f.z);
        const g = guard[f.camPhase];
        if (!g || !g.ref) continue;
        const expect = Math.min(total, g.ref.n * (f.z > g.ref.z ? (g.ref.z / f.z) ** 2 : 1));
        f.floor = Math.round(floorShare * expect * 100) / 100;
        g.frames++;
        g.inView.push(f.inView);
        if (f.inView < f.floor) g.below++;
        if (g.minInView == null || f.inView < g.minInView) { g.minInView = f.inView; g.minAt = { t: Math.round(f.t * 100) / 100, x: Math.round(f.x), y: Math.round(f.y), z: Math.round(f.z * 1000) / 1000, floor: f.floor }; }
      }
      const stateAfter = { watermark: b.state.watermark, elements: b.state.server.size, pending: b.state.pending.length };
      const failures = [];
      for (const g of guard) {
        if (!g.ref) failures.push(`phase ${g.phase} (${g.name}) never ran`);
        else if (g.ref.n < minRef) failures.push(`phase ${g.phase} (${g.name}) starts with ${g.ref.n} elements in view, under ${minRef}`);
        else if (!g.frames) failures.push(`phase ${g.phase} (${g.name}) has no frames`);
        else if (g.below) failures.push(`phase ${g.phase} (${g.name}): ${g.below} of ${g.frames} frames under the floor (min ${g.minInView} at ${JSON.stringify(g.minAt)})`);
      }
      if (JSON.stringify(stateBefore) !== JSON.stringify(stateAfter)) failures.push(`board state changed during the run: ${JSON.stringify(stateBefore)} -> ${JSON.stringify(stateAfter)}`);
      resolve({
        frames, errors, loaf, final: { ...b.cam },
        setup: { view: [W, H], contentBounds: R, anchor: { x: Math.round(anchor.x), y: Math.round(anchor.y), estimatedInViewAt100: anchor.n }, contentElements: total, panTurns: turns, stateBefore, stateAfter },
        guard: { pass: failures.length === 0, floorShare, minRef, failures, phases: guard.map(({ inView, ...g }) => ({ ...g, ref: g.ref && { x: Math.round(g.ref.x), y: Math.round(g.ref.y), z: Math.round(g.ref.z * 1000) / 1000, inView: g.ref.n }, inView: inView.length ? (() => { const s = [...inView].sort((a, c) => a - c); return { min: s[0], p50: s[s.length >> 1], max: s[s.length - 1] }; })() : null })) },
      });
    };
    requestAnimationFrame(tick);
  });
}

const env0 = uptime();
const chrome = await launchChrome({ headless: !!opt.headless, width: opt.width, height: opt.height, scale: opt.scale || 0, chrome: opt.chrome || undefined });
const exceptions = [];
let guardPass = false;
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
  const r = await page.eval(`(${driver})(${opt.seconds}, ${JSON.stringify(PHASES)}, ${Number(opt.floor)}, ${Number(opt['min-ref'])})`, { timeoutMs: (opt.seconds + 120) * 1000 });
  const all = r.frames.filter((f) => f.t >= 0.5); // skip the first fit frames
  const byPhase = {};
  for (const f of all) (byPhase[f.phase] ||= []).push(f);
  // The display's frame period is the median interval. A frame "missed" the
  // display when its interval exceeds 1.5 periods (a whole refresh skipped).
  const vsync = stats(all.map((f) => f.dt)).p50;
  const missed = all.filter((f) => f.dt > 1.5 * vsync).length;
  guardPass = r.guard.pass;
  const result = {
    kind: 'pan-zoom', url: opt.url, expected: opt.expected, seconds: opt.seconds, headless: !!opt.headless,
    env: await environment(chrome.cdp), viewport, uptimeBefore: env0, uptimeAfter: uptime(), readyAtMs: Math.round(readyAt), loads,
    setup: r.setup, guard: r.guard,
    frameTime: stats(all.map((f) => f.dt)), work: stats(all.map((f) => f.work)), inView: stats(all.map((f) => f.inView)),
    displayPeriodMs: vsync, missedFrames: missed, onTimeShare: Math.round((1 - missed / all.length) * 10000) / 100,
    over16_7: all.filter((f) => f.dt > 16.7).length, over33: all.filter((f) => f.dt > 33).length, over100: all.filter((f) => f.dt > 100).length,
    phases: Object.fromEntries(Object.entries(byPhase).map(([k, v]) => [k, {
      name: PHASES[k] ? PHASES[k][5] : 'after the script', frameTime: stats(v.map((f) => f.dt)), work: stats(v.map((f) => f.work)),
      inView: stats(v.map((f) => f.inView)), z: stats(v.map((f) => f.z)), jumps: v.filter((f) => f.jump).length,
    }])),
    longAnimationFrames: { n: r.loaf.length, maxMs: Math.max(0, ...r.loaf.map((x) => x.d)), blockingMs: Math.round(r.loaf.reduce((a, x) => a + (x.blocking || 0), 0)) },
    pageErrors: r.errors, exceptions, finalCamera: r.final,
  };
  console.log(JSON.stringify(result, null, 1));
  if (opt.out) writeFileSync(opt.out, JSON.stringify({ ...result, rawFrames: r.frames }, null, 1));
  if (!guardPass) console.error('content guard FAILED:\n  ' + r.guard.failures.join('\n  '));
} finally {
  await chrome.close();
}

// Leftover sockets or timers must not keep the process alive. A failed
// content guard makes the run fail.
process.exit(process.exitCode || (guardPass ? 0 : 1));
