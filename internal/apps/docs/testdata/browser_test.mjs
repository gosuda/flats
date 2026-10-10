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
// The live preview hides Markdown syntax in the DOM, so read the editor's
// own document (EditorView.findFromDOM, without importing the bundle).
async function editorText(page) {
  return page
    .locator(".cm-content")
    .evaluate((el) => el.cmTile.root.view.state.doc.toString());
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
  // Private links open in Edit, the live preview: the heading renders in
  // place. Its "# " is hidden away from the cursor and shown on the
  // cursor's line.
  const pressed = (page, mode) =>
    page.locator(`#modes [data-mode="${mode}"]`).getAttribute("aria-pressed");
  assert.equal(await a.locator("#modes button").count(), 2);
  assert.equal(await pressed(a, "edit"), "true", "private link not in Edit");
  assert.equal(await pressed(a, "view"), "false");
  assert.equal(await a.locator("#editor").isVisible(), true);
  assert.equal(await a.locator("#content").isVisible(), false);
  assert.equal(
    await a.locator(".toolbar, #preview").count(),
    0,
    "split-era panes still rendered",
  );
  const heading = a.locator(".cm-line.cm-h1");
  await a.locator(".cm-content").blur();
  await wait(
    async () => (await heading.innerText()) === "Shared",
    "heading syntax not hidden",
  );
  await heading.click();
  await wait(
    async () => (await heading.innerText()) === "# Shared",
    "heading syntax not revealed on the cursor line",
  );
  assert.equal(
    await a.locator("#page footer.powered a").getAttribute("href"),
    "https://github.com/gosuda/flats",
  );
  await a.locator(".cm-content").blur();
  await a.screenshot({
    path: path.join(screenshots, "desktop.png"),
    fullPage: true,
  });
  // View shows the rendered document Public visitors get, live, and Edit
  // returns to the same editor without reconnecting.
  await a.locator('#modes [data-mode="view"]').click();
  assert.equal(await pressed(a, "view"), "true");
  assert.equal(await pressed(a, "edit"), "false");
  assert.equal(
    await a.locator("#editor").isVisible(),
    false,
    "View shows the editor",
  );
  assert.equal(await a.locator("#content").isVisible(), true);
  assert.ok(
    (await a.locator("#content").innerText()).includes("Offline kept"),
    "View did not show the rendered document at once",
  );
  await b.locator(".cm-content").focus();
  await b.keyboard.press("ControlOrMeta+End");
  await b.keyboard.insertText("\nWhile viewing");
  await wait(
    async () =>
      (await a.locator("#content").innerText()).includes("While viewing"),
    "View did not render a live edit",
  );
  await a.screenshot({
    path: path.join(screenshots, "private-view.png"),
    fullPage: true,
  });
  await a.locator('#modes [data-mode="edit"]').click();
  assert.equal(await pressed(a, "edit"), "true");
  assert.equal(await a.locator("#editor").isVisible(), true);
  assert.equal(await a.locator("#content").isVisible(), false);
  assert.ok((await editorText(a)).includes("While viewing"));
  await a.locator(".cm-content").click();
  await a.keyboard.press("ControlOrMeta+End");
  await a.keyboard.insertText("\nAfter toggle");
  await wait(
    async () =>
      (await editorText(b)).includes("After toggle") &&
      (await a.locator("#status").innerText()) === "Saved",
    "edit after View toggle not synchronized",
  );
  await a.locator(".cm-content").blur();
  const mobile = await open("Mobile", {
    viewport: { width: 390, height: 844 },
  });
  assert.equal(await mobile.locator("#modes").isVisible(), true);
  assert.equal(await pressed(mobile, "edit"), "true");
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
      (await publicPage.locator("#content").innerText()).includes(
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
    await publicPage.locator("[data-mode], #modes, .toolbar").count(),
    0,
    "public viewer shows mode controls",
  );
  assert.equal(await publicPage.locator("#editor").isVisible(), false);
  assert.equal(
    await publicPage.locator("#page footer.powered a").getAttribute("href"),
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
      (await publicPage.locator("#content").innerText()).includes(
        "Public live change",
      ),
    "public viewer did not receive new edit",
  );
  // Access follows each welcome: a reconnect that is read-only hides the
  // controls and shows the rendered document; regaining write access
  // restores Edit and editing without a reload.
  const flip = await open("Flip");
  const reconnect = async (page, privateAccess) => {
    const value = privateAccess ? "1" : "0";
    await page
      .context()
      .addCookies([{ name: "flats-test-private", value, url }]);
    await page.context().setOffline(true);
    await wait(
      async () => !(await page.evaluate(() => navigator.onLine)),
      "page did not go offline",
    );
    await page.context().setOffline(false);
  };
  await reconnect(flip, false);
  await wait(
    async () => (await flip.locator("#status").innerText()) === "Read-only",
    "read-only reconnect not applied",
  );
  assert.equal(await flip.locator("#modes").isVisible(), false);
  assert.equal(await flip.locator("#editor").isVisible(), false);
  assert.equal(await flip.locator("#content").isVisible(), true);
  const statusBox = await flip.locator("#status").boundingBox();
  assert.ok(statusBox.x > 1440 / 2, "status lost its right alignment");
  await reconnect(flip, true);
  await wait(
    async () => (await flip.locator("#status").innerText()) === "Saved",
    "writable reconnect not applied",
  );
  assert.equal(await flip.locator("#modes").isVisible(), true);
  assert.equal(await pressed(flip, "edit"), "true");
  assert.equal(await flip.locator("#editor").isVisible(), true);
  await flip.locator(".cm-content").click();
  await flip.keyboard.press("ControlOrMeta+End");
  await flip.keyboard.insertText("\nWrite access back");
  await wait(
    async () =>
      (await editorText(b)).includes("Write access back") &&
      (await flip.locator("#status").innerText()) === "Saved",
    "edit after regaining write access not synchronized",
  );
  // A page first served read-only gets the controls once it is writable.
  const upgradeContext = await browser.newContext({
    viewport: { width: 1000, height: 800 },
  });
  contexts.push(upgradeContext);
  const upgrade = await upgradeContext.newPage();
  await upgrade.goto(url);
  await wait(
    async () => (await upgrade.locator("#status").innerText()) === "Read-only",
    "upgrade page not read-only",
  );
  assert.equal(await upgrade.locator("#modes").count(), 0);
  await reconnect(upgrade, true);
  await wait(
    async () => (await upgrade.locator("#status").innerText()) === "Saved",
    "upgrade page did not become writable",
  );
  assert.equal(await upgrade.locator("#modes").isVisible(), true);
  assert.equal(await pressed(upgrade, "edit"), "true");
  assert.equal(await upgrade.locator("#editor").isVisible(), true);
  await upgrade.locator('#modes [data-mode="view"]').click();
  assert.equal(await upgrade.locator("#editor").isVisible(), false);
  assert.equal(await upgrade.locator("#content").isVisible(), true);
  const dark = await open("Dark reader", { colorScheme: "dark" });
  await dark.screenshot({
    path: path.join(screenshots, "dark.png"),
    fullPage: true,
  });
  assert.equal(
    await mobile.evaluate(
      () => document.documentElement.scrollHeight <= innerHeight,
    ),
    true,
    "mobile page overruns viewport",
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
    "PASS: independent contexts, concurrent convergence, remote cursors, Korean composition/emoji, local undo, persistence, offline reconnect, live-preview Edit and View toggle, mobile, public read-only.",
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
