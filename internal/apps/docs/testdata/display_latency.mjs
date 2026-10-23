// Display latency between two member pages (docs/spec/docs-public-edit-TECH.md
// 8, row "Limits"): page A inserts a marked line every 250 ms through its
// CodeMirror view, and page B records when each marker first appears in its
// .cm-content DOM. Both times come from the same machine clock.
//
//	node display_latency.mjs <url> <evidence dir> <label> <seconds> <document>
//
// Prints one line "RESULT <json>".
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, evidence, label, seconds, doc] = process.argv.slice(2);
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const contexts = [],
  errors = [];
let a, b;
async function open(name) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
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
function pct(sorted, q) {
  if (!sorted.length) return null;
  return sorted[Math.max(0, Math.ceil(q * sorted.length) - 1)];
}
try {
  a = await open("Member A");
  b = await open("Member B");
  for (const page of [a, b])
    if (!(await editorText(page)).includes("Agent line"))
      throw new Error("the page did not open " + doc);
  // B: the first time each marker is in its editor's DOM. A's markers are
  // inserted at the start, so the newest is in the first lines, which stay
  // rendered while B is scrolled to the top.
  await b.evaluate(() => {
    window.__seen = {};
    const content = document.querySelector(".cm-content"),
      re = /Mk\d{5}/g;
    const scan = () => {
      const now = performance.timeOrigin + performance.now();
      let i = 0;
      for (const line of content.children) {
        if (i++ >= 8) break;
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
  // A: one marked line every 250 ms through its CodeMirror view, which the
  // Yjs binding sends like typed text.
  await a.evaluate(
    (seconds) =>
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
          view.dispatch({ changes: { from: 0, insert: marker + "\n" } });
        }, 250);
      }),
    Number(seconds),
  );
  await new Promise((r) => setTimeout(r, 3000));
  const sent = await a.evaluate(() => window.__sent),
    seen = await b.evaluate(() => window.__seen);
  const latencies = [];
  let missing = 0;
  for (const [marker, at] of Object.entries(sent)) {
    if (marker in seen) latencies.push(seen[marker] - at);
    else missing++;
  }
  latencies.sort((x, y) => x - y);
  const round = (x) => (x === null ? null : Math.round(x * 10) / 10);
  const status = await Promise.all(
    [a, b].map((p) => p.locator("#status").innerText()),
  );
  for (const [name, page] of [
    ["a", a],
    ["b", b],
  ])
    await page.screenshot({
      path: path.join(evidence, `${label}-${name}.png`),
    });
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
      }),
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
  for (const c of contexts) await c.close().catch(() => {});
  await browser.close();
}
