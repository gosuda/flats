import assert from "node:assert/strict";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, screenshots] = process.argv.slice(2);
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const contexts = [],
  errors = [];
let a, b;
async function wait(check, message) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((r) => setTimeout(r, 80));
  }
  throw new Error(message);
}
async function editorText(page) {
  return page.locator(".cm-content").evaluate((el) => {
    const copy = el.cloneNode(true);
    for (const cursor of copy.querySelectorAll(".cm-ySelectionCaret"))
      cursor.remove();
    return [...copy.querySelectorAll(".cm-line")]
      .map((line) => line.textContent)
      .join("\n");
  });
}
async function open(name, options = {}) {
  const context = await browser.newContext({
    extraHTTPHeaders: { "X-Flats-Access": "private" },
    viewport: { width: 1440, height: 960 },
    ...options,
  });
  contexts.push(context);
  await context.addCookies([{ name: "flats-test-private", value: "1", url }]);
  await context.addInitScript(
    (name) => localStorage.setItem("flats-docs-name", name),
    name,
  );
  const page = await context.newPage();
  page.on("pageerror", (e) => {
    errors.push(e.message);
    console.error("pageerror", e.stack);
  });
  page.on("console", (m) => {
    if (m.type() === "error") console.error("browser console:", m.text());
  });
  await page.goto(url);
  try {
    await page.locator(".cm-content").waitFor({ timeout: 12000 });
  } catch (e) {
    await page.screenshot({
      path: path.join(screenshots, "failure.png"),
      fullPage: true,
    });
    console.error(await page.locator("body").innerText());
    throw e;
  }
  await wait(
    async () => (await page.locator("#status").innerText()) === "Saved",
    "initial sync",
  );
  return page;
}
try {
  a = await open("Alice");
  b = await open("Bora");
  await Promise.all([
    a.locator(".cm-content").click(),
    b.locator(".cm-content").click(),
  ]);
  await Promise.all([
    a.keyboard.press("ControlOrMeta+End"),
    b.keyboard.press("ControlOrMeta+End"),
  ]);
  await Promise.all([
    a.keyboard.insertText("\nAlice 한국어 🙂"),
    b.keyboard.insertText("\nBora 🌍"),
  ]);
  await wait(async () => {
    const [x, y] = await Promise.all([editorText(a), editorText(b)]);
    return x === y && x.includes("Alice 한국어 🙂") && x.includes("Bora 🌍");
  }, "concurrent clients did not converge");
  await wait(
    async () =>
      (await a.locator("#status").innerText()) === "Saved" &&
      (await b.locator("#status").innerText()) === "Saved",
    "edits not acknowledged",
  );
  // Composition events exercise CodeMirror's IME path while real Korean text enters through browser input.
  await a
    .locator(".cm-content")
    .dispatchEvent("compositionstart", { data: "" });
  await a.keyboard.insertText(" 한글입력");
  await a
    .locator(".cm-content")
    .dispatchEvent("compositionend", { data: "한글입력" });
  await wait(
    async () => (await editorText(b)).includes("한글입력"),
    "Korean composition did not roundtrip",
  );
  await wait(
    async () =>
      (await a.locator(".cm-ySelectionCaret").count()) > 0 &&
      (await b.locator(".cm-ySelectionCaret").count()) > 0,
    "remote cursors missing",
  );
  // A distinct undo group from Alice, followed by Bora's independent group.
  await new Promise((r) => setTimeout(r, 500));
  await a.locator(".cm-content").click();
  await a.keyboard.press("ControlOrMeta+End");
  await a.keyboard.insertText("\nAlice undo target");
  await wait(
    async () => (await editorText(b)).includes("Alice undo target"),
    "Alice edit missing",
  );
  await b.locator(".cm-content").click();
  await b.keyboard.press("ControlOrMeta+End");
  await b.keyboard.insertText("\nBora keep this");
  await wait(
    async () => (await editorText(a)).includes("Bora keep this"),
    "Bora edit missing",
  );
  await a.locator(".cm-content").focus();
  await a.keyboard.press("ControlOrMeta+z");
  await wait(async () => {
    const x = await editorText(a),
      y = await editorText(b);
    return (
      x === y &&
      !x.includes("Alice undo target") &&
      x.includes("Bora keep this")
    );
  }, "undo affected remote user or did not converge");
  await wait(
    async () => (await a.locator("#status").innerText()) === "Saved",
    "undo not saved",
  );
  const persisted = await editorText(a);
  await a.reload();
  await a.locator(".cm-content").waitFor();
  await wait(
    async () => (await editorText(a)) === persisted,
    "reload lost acknowledged changes",
  );
  // Reconnect keeps unacknowledged updates with their ids.
  await contexts[0].setOffline(true);
  await wait(
    async () => (await a.locator("#status").innerText()).startsWith("Offline"),
    "offline state missing",
  );
  await a.locator(".cm-content").focus();
  await a.keyboard.press("ControlOrMeta+End");
  await a.keyboard.insertText("\nOffline kept");
  await contexts[0].setOffline(false);
  await wait(
    async () =>
      (await editorText(b)).includes("Offline kept") &&
      (await a.locator("#status").innerText()) === "Saved",
    "offline changes not synchronized",
  );
  await a.screenshot({
    path: path.join(screenshots, "desktop.png"),
    fullPage: true,
  });
  const mobile = await open("Mobile", {
    viewport: { width: 390, height: 844 },
  });
  await mobile.locator('[data-mode="preview"]').click();
  await mobile.screenshot({
    path: path.join(screenshots, "mobile.png"),
    fullPage: true,
  });
  assert.equal(
    await mobile.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    true,
    "mobile horizontal overflow",
  );
  const publicContext = await browser.newContext({
    viewport: { width: 1000, height: 800 },
  });
  contexts.push(publicContext);
  const publicPage = await publicContext.newPage();
  await publicPage.goto(url);
  await wait(
    async () =>
      (await publicPage.locator("#preview").innerText()).includes(
        "Offline kept",
      ),
    "public live view missing",
  );
  assert.equal(
    await publicPage.locator(".cm-content").count(),
    0,
    "public viewer has editor",
  );
  assert.equal(await publicPage.locator("#status").innerText(), "Read-only");
  assert.equal(
    await publicPage.locator("dialog").count(),
    0,
    "name prompt still rendered",
  );
  assert.equal(
    await publicPage.locator(".brand, #path").count(),
    0,
    "brand or file name still rendered",
  );
  assert.equal(
    await publicPage.locator(".toolbar").isVisible(),
    false,
    "public viewer shows an empty toolbar",
  );
  assert.equal(
    await publicPage
      .locator("#preview footer.powered a")
      .getAttribute("href"),
    "https://github.com/gosuda/flats",
  );
  await wait(
    async () =>
      /^[A-Z][a-z]+ [A-Z][a-z]+$/m.test(
        await publicPage.locator("#presence").innerText(),
      ),
    "random name was not assigned",
  );
  await b.locator(".cm-content").focus();
  await b.keyboard.press("ControlOrMeta+End");
  await b.keyboard.insertText("\nPublic live change");
  await wait(
    async () =>
      (await publicPage.locator("#preview").innerText()).includes(
        "Public live change",
      ),
    "public viewer did not receive new edit",
  );
  const dark = await open("Dark reader", { colorScheme: "dark" });
  await dark.screenshot({
    path: path.join(screenshots, "dark.png"),
    fullPage: true,
  });
  await mobile.locator('[data-mode="split"]').click();
  await mobile.screenshot({
    path: path.join(screenshots, "mobile-split.png"),
    fullPage: true,
  });
  assert.equal(
    await mobile.evaluate(
      () => document.documentElement.scrollHeight <= innerHeight,
    ),
    true,
    "mobile panels overrun viewport",
  );
  await publicPage.screenshot({
    path: path.join(screenshots, "readonly.png"),
    fullPage: true,
  });
  const beforeConflict = await editorText(a);
  assert.equal(await a.evaluate(async () => (await fetch("/test-activate")).status), 204);
  await a.locator("#conflict-notice").waitFor({state: "visible", timeout: 30000});
  assert.equal(await a.locator("#status").innerText(), "Saved");
  const recovery = await a.locator("#conflict-notice a").getAttribute("href");
  const recovered = await a.evaluate(async href => (await fetch(href)).text(), recovery);
  assert.ok(recovered.includes("Public live change"));
  assert.ok(beforeConflict.includes("Public live change"));
  assert.equal(await publicPage.locator("#conflict-notice").isVisible(), false);
  assert.equal(await publicPage.evaluate(async href => (await fetch(href)).status, recovery), 403);
  await a.screenshot({path: path.join(screenshots, "conflict-notice.png"),fullPage: true});
  assert.deepEqual(errors, []);
  console.log(
    "PASS: independent contexts, concurrent convergence, remote cursors, Korean composition/emoji, local undo, persistence, offline reconnect, mobile, public read-only.",
  );
} catch (error) {
  for (const [label, page] of [
    ["alice", a],
    ["bora", b],
  ])
    if (page) {
      console.error(
        label,
        await editorText(page),
        await page.locator("#status").innerText(),
      );
      await page.screenshot({
        path: path.join(screenshots, label + "-failure.png"),
        fullPage: true,
      });
    }
  throw error;
} finally {
  for (const c of contexts) await c.close();
  await browser.close();
}
