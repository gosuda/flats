// Display latency between two member pages (docs/spec/docs-public-edit-TECH.md
// 8, row "Limits"): page A inserts a marked line every 250 ms through its
// CodeMirror view, and page B records when each marker first appears in its
// .cm-content DOM. Both times come from the same machine clock.
//
//	node display_latency.mjs <url> <evidence dir> <label> <seconds> <document> <mode>
//
// mode "top" inserts each marker on its own line at the start, so guest text
// written below is out of page B's viewport; "visible" puts all markers on
// the first line above a blank line and gives page B a tall viewport, so the
// guests' growing text stays in view while B times the markers.
//
// After the edits it prints "EDITS-DONE" and waits for a line on stdin (the
// caller stops the guests and lets their updates settle), then waits until
// both pages' editor text equals the server's committed Markdown.
//
// Prints one line "RESULT <json>". Every 2 s during the edits it also times
// a round trip into page B's main thread ("probe"). With
// FLATS_DOCS_DISPLAY_PROFILE=1 it records a CPU profile of page B during the
// edits into the evidence directory and prints its top functions.
import fs from "node:fs";
import readline from "node:readline";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, evidence, label, seconds, doc, mode = "top"] =
  process.argv.slice(2);
const input = readline.createInterface({ input: process.stdin }),
  lines = input[Symbol.asyncIterator]();
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const contexts = [],
  errors = [];
let a, b;
async function open(name, height = 900) {
  const context = await browser.newContext({
    viewport: { width: 1280, height },
  });
  contexts.push(context);
  await context.addCookies([{ name: "flats-test-private", value: "1", url }]);
  await context.addInitScript(
    (name) => localStorage.setItem("flats-docs-name", name),
    name,
  );
  const page = await context.newPage();
  page.on("pageerror", (e) => errors.push(name + ": " + e.message));
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(name + " console: " + m.text());
  });
  await page.goto(url + "/" + doc);
  await page.locator(".cm-content").waitFor({ timeout: 20000 });
  await page.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
  return page;
}
// The editor's own document (as in browser_test.mjs).
async function editorText(page) {
  return page
    .locator(".cm-content")
    .evaluate((el) => el.cmTile.root.view.state.doc.toString());
}
const started = Date.now();
const phase = (name) =>
  console.log(
    `phase ${name} at ${((Date.now() - started) / 1000).toFixed(1)}s`,
  );
function pct(sorted, q) {
  if (!sorted.length) return null;
  return sorted[Math.max(0, Math.ceil(q * sorted.length) - 1)];
}
try {
  phase("open");
  a = await open("Member A");
  b = await open("Member B", mode === "visible" ? 2400 : 900);
  for (const page of [a, b])
    if (!(await editorText(page)).includes("Agent line"))
      throw new Error("the page did not open " + doc);
  // B: the first time each marker is in its editor's DOM. A's markers are
  // inserted at the start, so the newest is in the first lines, which stay
  // rendered while B is scrolled to the top.
  // Both pages also record their main thread's responsiveness: the gaps
  // between animation frames and the time in long tasks.
  for (const page of [a, b])
    await page.evaluate(() => {
      window.__gaps = [];
      window.__longTasks = 0;
      let last = performance.now();
      const frame = (now) => {
        window.__gaps.push(now - last);
        last = now;
        if (!window.__stopFrames) requestAnimationFrame(frame);
      };
      requestAnimationFrame(frame);
      new PerformanceObserver((list) => {
        for (const e of list.getEntries()) window.__longTasks += e.duration;
      }).observe({ type: "longtask" });
    });
  await b.evaluate(() => {
    window.__seen = {};
    const content = document.querySelector(".cm-content"),
      re = /Mk\d{5}/g;
    const scan = () => {
      const now = performance.timeOrigin + performance.now();
      let i = 0;
      for (const line of content.children) {
        if (i++ >= 3) break;
        for (const m of line.textContent.matchAll(re))
          if (!(m[0] in window.__seen)) window.__seen[m[0]] = now;
      }
    };
    new MutationObserver(scan).observe(content, {
      childList: true,
      subtree: true,
      characterData: true,
    });
  });
  // In visible mode the markers' line is kept apart from the guests' text
  // by a blank line.
  if (mode === "visible") {
    await a
      .locator(".cm-content")
      .evaluate((el) =>
        el.cmTile.root.view.dispatch({ changes: { from: 0, insert: "\n\n" } }),
      );
    await b.waitForFunction(
      () =>
        document
          .querySelector(".cm-content")
          .cmTile.root.view.state.doc.toString()
          .startsWith("\n\n"),
      null,
      { timeout: 20000 },
    );
  }
  phase("edits");
  let profiler;
  if (process.env.FLATS_DOCS_DISPLAY_PROFILE === "1") {
    profiler = await b.context().newCDPSession(b);
    await profiler.send("Profiler.enable");
    await profiler.send("Profiler.setSamplingInterval", { interval: 1000 });
    await profiler.send("Profiler.start");
  }
  // Round trips into B's main thread while A edits.
  const probes = [];
  let probing = true;
  const probe = (async () => {
    while (probing) {
      const at = Date.now();
      await b.evaluate(() => 0);
      probes.push([Math.round((at - started) / 1000), Date.now() - at]);
      await new Promise((r) => setTimeout(r, 2000));
    }
  })();
  // A: one marked line every 250 ms through its CodeMirror view, which the
  // Yjs binding sends like typed text.
  await a.evaluate(
    ({ seconds, separator }) =>
      new Promise((resolve) => {
        const view = document.querySelector(".cm-content").cmTile.root.view;
        window.__sent = {};
        let i = 0;
        const end = performance.now() + seconds * 1000;
        const timer = setInterval(() => {
          if (performance.now() >= end) {
            clearInterval(timer);
            resolve();
            return;
          }
          const marker = "Mk" + String(i++).padStart(5, "0");
          window.__sent[marker] = performance.timeOrigin + performance.now();
          view.dispatch({ changes: { from: 0, insert: marker + separator } });
        }, 250);
      }),
    { seconds: Number(seconds), separator: mode === "visible" ? " " : "\n" },
  );
  phase("edits done");
  // Guest text in B's viewport now, by its words.
  const guestInView = await b.evaluate(() => {
    let n = 0;
    for (const line of document.querySelectorAll(".cm-content .cm-line")) {
      const r = line.getBoundingClientRect();
      if (r.bottom < 0 || r.top > innerHeight) continue;
      n += (line.textContent.match(/g\d\dw\d{5}|\[g\d\d-\d{5}\]/g) || [])
        .length;
    }
    return n;
  });
  console.log("guest words in page B's viewport:", guestInView);
  probing = false;
  await probe;
  console.log("probe [seconds since start, ms]: " + JSON.stringify(probes));
  if (profiler) {
    const { profile } = await profiler.send("Profiler.stop");
    fs.writeFileSync(
      path.join(evidence, `${label}-b.cpuprofile`),
      JSON.stringify(profile),
    );
    const self = new Map(),
      total = profile.samples.length;
    const byId = new Map(profile.nodes.map((n) => [n.id, n]));
    for (const id of profile.samples) {
      const f = byId.get(id).callFrame,
        key = `${f.functionName || "(anonymous)"} ${path.basename(f.url)}:${f.lineNumber + 1}`;
      self.set(key, (self.get(key) || 0) + 1);
    }
    const top = [...self].sort((x, y) => y[1] - x[1]).slice(0, 25);
    console.log(`profile of page B: ${total} samples`);
    for (const [key, n] of top)
      console.log(`  ${((100 * n) / total).toFixed(1)}% ${key}`);
  }
  await new Promise((r) => setTimeout(r, 3000));
  const sent = await a.evaluate(() => window.__sent),
    seen = await b.evaluate(() => window.__seen);
  phase("collected");
  const responsiveness = {};
  for (const [name, page] of [
    ["a", a],
    ["b", b],
  ])
    responsiveness[name] = await page.evaluate(() => {
      window.__stopFrames = true;
      const g = window.__gaps.slice().sort((x, y) => x - y),
        at = (q) =>
          Math.round(g[Math.max(0, Math.ceil(q * g.length) - 1)] || 0);
      return {
        frames: g.length,
        gapP50: at(0.5),
        gapP95: at(0.95),
        gapMax: at(1),
        longTaskMs: Math.round(window.__longTasks),
        lines:
          document.querySelector(".cm-content").cmTile.root.view.state.doc
            .lines,
      };
    });
  const latencies = [];
  let missing = 0;
  for (const [marker, at] of Object.entries(sent)) {
    if (marker in seen) latencies.push(seen[marker] - at);
    else missing++;
  }
  latencies.sort((x, y) => x - y);
  const round = (x) => (x === null ? null : Math.round(x * 10) / 10);
  // The caller stops the guests and lets their updates settle; then both
  // pages must show exactly the server's committed text.
  console.log("EDITS-DONE");
  await lines.next();
  phase("settled");
  const server = () =>
    a.evaluate(
      async (doc) =>
        (
          await (
            await fetch("/_docs/api/document?doc=" + encodeURIComponent(doc))
          ).json()
        ).markdown,
      doc,
    );
  let convergence;
  const deadline = Date.now() + 20000;
  for (;;) {
    const [s, x, y] = await Promise.all([
      server(),
      editorText(a),
      editorText(b),
    ]);
    const diff = (u, v) => {
      let i = 0;
      while (i < u.length && u[i] === v[i]) i++;
      return i === u.length && u.length === v.length ? -1 : i;
    };
    convergence = {
      converged: s === x && s === y,
      server: s.length,
      a: x.length,
      b: y.length,
      aFirstDifference: diff(s, x),
      bFirstDifference: diff(s, y),
    };
    if (convergence.converged || Date.now() > deadline) break;
    await new Promise((r) => setTimeout(r, 250));
  }
  phase("compared");
  console.log("convergence: " + JSON.stringify(convergence));
  const status = await Promise.all(
    [a, b].map((p) => p.locator("#status").innerText()),
  );
  console.log(
    "RESULT " +
      JSON.stringify({
        sent: Object.keys(sent).length,
        count: latencies.length,
        missing,
        p50: round(pct(latencies, 0.5)),
        p95: round(pct(latencies, 0.95)),
        p99: round(pct(latencies, 0.99)),
        max: round(pct(latencies, 1)),
        status,
        errors,
        responsiveness,
        probeMax: Math.max(0, ...probes.map((p) => p[1])),
        guestInView,
        ...convergence,
      }),
  );
  // Screenshots last: a busy page may not take one in time.
  for (const [name, page] of [
    ["a", a],
    ["b", b],
  ])
    await page
      .screenshot({
        path: path.join(evidence, `${label}-${name}.png`),
        timeout: 60000,
      })
      .then(
        () => phase("screenshot " + name),
        (e) =>
          console.log(`screenshot ${name} failed: ${e.message.split("\n")[0]}`),
      );
} catch (e) {
  for (const [name, page] of [
    ["a", a],
    ["b", b],
  ])
    if (page)
      await page
        .screenshot({
          path: path.join(evidence, `${label}-${name}-failure.png`),
        })
        .catch(() => {});
  throw e;
} finally {
  input.close();
  for (const c of contexts) await c.close().catch(() => {});
  await browser.close();
}
