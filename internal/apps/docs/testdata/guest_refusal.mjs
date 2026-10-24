// A guest's refused update in a real browser (docs-public-edit.md rule 22):
// the guest types text A, pastes more than 64 KiB (refused with guest_size)
// and types B behind it. The page stops editing with the reason, never shows
// Saved, keeps A, the paste and B for download, and Rejoin edits again as
// the same guest from the server's text.
//
//	node guest_refusal.mjs <url> <evidence dir>
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, evidence] = process.argv.slice(2);
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const errors = [];
let page;
const shot = (name) =>
  page.screenshot({ path: path.join(evidence, name + ".png") });
const editorText = () =>
  page
    .locator(".cm-content")
    .evaluate((el) => el.cmTile.root.view.state.doc.toString());
const serverText = () =>
  page.evaluate(async () => {
    const r = await fetch("/_docs/api/document?doc=index.md");
    return (await r.json()).markdown;
  });
const status = () => page.locator("#status").innerText();
// The guest's join: the name popup, then a writable editor.
async function join(name) {
  await page.locator("#guest-name").waitFor({ timeout: 20000 });
  await page.locator("#guest-name").fill(name);
  await page.getByRole("button", { name: "Join and edit" }).click();
  await page.waitForFunction(
    (name) =>
      document.querySelector("header")?.textContent.includes("You are " + name),
    name,
    { timeout: 20000 },
  );
}
try {
  const context = await browser.newContext({
    acceptDownloads: true,
    viewport: { width: 1280, height: 900 },
  });
  page = await context.newPage();
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(url + "/index.md");
  await join("Mina");
  const token = await page.evaluate(() =>
    sessionStorage.getItem("flats-docs-guest-token"),
  );
  assert.match(token || "", /^[0-9a-f]{32}$/, "no guest token");
  // Text A, typed and saved.
  await page.locator(".cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("\nGuest text A typed");
  await page.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
  assert.ok((await serverText()).includes("Guest text A typed"));
  await shot("1-typed-a");
  // A paste over 64 KiB, then B typed behind it before the answer.
  const paste = "PASTE-BEGIN " + "p".repeat(66 * 1024) + " PASTE-END";
  await page.evaluate(
    ({ paste }) => {
      const content = document.querySelector(".cm-content"),
        view = content.cmTile.root.view,
        data = new DataTransfer();
      data.setData("text/plain", "\n" + paste);
      content.dispatchEvent(
        new ClipboardEvent("paste", {
          clipboardData: data,
          bubbles: true,
          cancelable: true,
        }),
      );
      const at = view.state.selection.main.head;
      view.dispatch({
        changes: { from: at, insert: "\nGuest text B typed after the paste" },
        selection: { anchor: at + 35 },
        userEvent: "input.type",
      });
    },
    { paste },
  );
  const local = await editorText();
  assert.ok(local.includes("PASTE-END"), "the paste is not in the editor");
  assert.ok(local.includes("typed after the paste"), "B is not in the editor");
  const notice = page.locator("#notice");
  await notice.waitFor({ state: "visible", timeout: 20000 });
  const noticeText = await notice.innerText();
  console.log("notice:", noticeText.replace(/\s+/g, " "));
  assert.ok(
    noticeText.includes(
      "This change is too large for a guest, so editing stopped.",
    ),
    noticeText,
  );
  assert.ok(
    noticeText.includes("It and what you typed after it aren't saved."),
  );
  await page.waitForTimeout(1500);
  assert.notEqual(await status(), "Saved", "the page says Saved");
  console.log("status after the refusal:", await status());
  // Read-only: the editor state, and typing changes nothing.
  assert.equal(
    await page
      .locator(".cm-content")
      .evaluate((el) => el.cmTile.root.view.state.readOnly),
    true,
  );
  const before = await editorText();
  await page.locator(".cm-content").click();
  await page.keyboard.type("ZZZ");
  assert.equal(await editorText(), before, "typing changed a stopped editor");
  await shot("2-refused");
  // Download local text: A, the paste and B.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Download local text" }).click(),
  ]);
  const saved = path.join(
    evidence,
    "downloaded-" + download.suggestedFilename(),
  );
  await download.saveAs(saved);
  const file = fs.readFileSync(saved, "utf8");
  console.log(
    "downloaded",
    download.suggestedFilename(),
    file.length,
    "characters",
  );
  for (const part of [
    "Guest text A typed",
    "PASTE-BEGIN",
    "PASTE-END",
    "Guest text B typed after the paste",
  ])
    assert.ok(file.includes(part), "the download lacks " + part);
  assert.ok(file.includes(paste), "the download lacks the whole paste");
  // Rejoin: the same guest, the server's text, editing again.
  await Promise.all([
    page.waitForEvent("load"),
    page.getByRole("button", { name: "Rejoin" }).click(),
  ]);
  await page.waitForFunction(
    () =>
      document.querySelector("header")?.textContent.includes("You are Mina"),
    null,
    { timeout: 20000 },
  );
  assert.equal(await page.locator("#guest-name").count(), 0, "name popup");
  assert.equal(
    await page.evaluate(() => sessionStorage.getItem("flats-docs-guest-token")),
    token,
    "another guest token",
  );
  await page.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
  const server = await serverText();
  assert.ok(server.includes("Guest text A typed"));
  assert.ok(
    !server.includes("PASTE-BEGIN") && !server.includes("after the paste"),
  );
  assert.equal(
    await editorText(),
    server,
    "the page does not show the server's text",
  );
  assert.equal(await notice.isVisible(), false, "the notice stayed");
  await page.locator(".cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("\nGuest text C typed after rejoining");
  await page.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
  await page.waitForFunction(
    async () =>
      (
        await (await fetch("/_docs/api/document?doc=index.md")).json()
      ).markdown.includes("typed after rejoining"),
    null,
    { timeout: 20000 },
  );
  await shot("3-rejoined");
  assert.deepEqual(errors, []);
  console.log(
    "PASS: refusal notice, not Saved, read-only, download with A, paste and B, Rejoin as the same guest with the server's text, editing again.",
  );
} catch (e) {
  if (page) await shot("failure").catch(() => {});
  throw e;
} finally {
  await browser.close();
}
