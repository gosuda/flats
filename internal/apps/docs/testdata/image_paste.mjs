// Pasting images in a real browser against a real host
// (docs/spec/docs-image-paste.md). The first cases use the operating system's
// clipboard path: a clipboard write and the paste key.
//
//	node image_paste.mjs <private url> <public url> <evidence dir>
'use strict';

import assert from "node:assert/strict";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [privateURL, publicURL, evidence] = process.argv.slice(2);
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const reference = /!\[\]\(images\/[0-9a-f]{16}\.png\)/;
const allReferences = new RegExp(reference.source, "g");

const editor = (page) =>
  page
    .locator(".cm-content")
    .evaluate((el) => {
      const { state } = el.cmTile.root.view;
      return { text: state.doc.toString(), head: state.selection.main.head };
    });
const server = (page) =>
  page.evaluate(
    async () =>
      (await (await fetch("/_docs/api/document?doc=index.md")).json()).markdown,
  );
const saved = (page) =>
  page.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
// settled waits for the add to finish, its text to appear and to sync.
async function settled(page, count) {
  await page.waitForFunction(
    ({ source, count }) => {
      const { state } = document.querySelector(".cm-content").cmTile.root.view;
      const found = state.doc.toString().match(new RegExp(source, "g")) ?? [];
      return found.length === count;
    },
    { source: reference.source, count },
    { timeout: 20000 },
  );
  await saved(page);
}
async function openEditor(context, url) {
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (err) => errors.push(err.message));
  await page.goto(url);
  await page.locator(".cm-content").waitFor({ timeout: 20000 });
  await saved(page);
  page.errors = errors;
  return page;
}
const typeAtEnd = async (page, text) => {
  await page.locator(".cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type(text);
};
// copyImage puts a generated picture on the real clipboard.
const copyImage = (page, width) =>
  page.evaluate(async (width) => {
    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = 20;
    const c = canvas.getContext("2d");
    c.fillStyle = "#356649";
    c.fillRect(0, 0, width, 20);
    const blob = await new Promise((done) => canvas.toBlob(done, "image/png"));
    await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
  }, width);
const pasteKey = (page) => page.keyboard.press("ControlOrMeta+V");
// pasteEvent delivers a paste with contents the real clipboard cannot hold
// here: several files, or a file beside text.
const pasteEvent = (page, { widths = [], text, type = "image/png", bytes }) =>
  page.evaluate(
    async ({ widths, text, type, bytes }) => {
      const data = new DataTransfer();
      if (bytes) data.items.add(new File([bytes], "x", { type }));
      for (const width of widths) {
        const canvas = document.createElement("canvas");
        canvas.width = width;
        canvas.height = 20;
        canvas.getContext("2d").fillRect(0, 0, width, 20);
        const blob = await new Promise((d) => canvas.toBlob(d, "image/png"));
        data.items.add(new File([blob], `w${width}.png`, { type: "image/png" }));
      }
      if (text) data.setData("text/plain", text);
      document.querySelector(".cm-content").dispatchEvent(
        new ClipboardEvent("paste", {
          clipboardData: data,
          bubbles: true,
          cancelable: true,
        }),
      );
    },
    { widths, text, type, bytes },
  );

// holdAnswer lets the host do its work but keeps its answer from the page until
// the returned function is called, so a test can act while an add is pending.
async function holdAnswer(page) {
  let release;
  const released = new Promise((done) => (release = done));
  let hostDone;
  const done = new Promise((resolve) => (hostDone = resolve));
  await page.route("**/_docs/api/images*", async (route) => {
    const response = await route.fetch();
    hostDone();
    await released;
    await route.fulfill({ response });
    await page.unroute("**/_docs/api/images*");
  });
  return { done, release };
}
const textMatches = (page, source) =>
  page.waitForFunction(
    (source) =>
      new RegExp(source).test(
        document.querySelector(".cm-content").cmTile.root.view.state.doc.toString(),
      ),
    source,
    { timeout: 15000 },
  );
const image = reference.source;

async function test(name, run) {
  await run();
  console.log("ok", name);
}

try {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  const page = await openEditor(context, privateURL);
  let count = 0;

  await test("on an empty line the picture shows and the caret moves on", async () => {
    await typeAtEnd(page, "\n\n");
    const before = (await editor(page)).text;
    await copyImage(page, 320);
    const statuses = await page.evaluateHandle(() => {
      const seen = [];
      const status = document.querySelector("#status");
      new MutationObserver(() => seen.push(status.textContent)).observe(status, {
        childList: true,
        characterData: true,
        subtree: true,
      });
      return seen;
    });
    await pasteKey(page);
    await page.waitForFunction(
      () => document.querySelector("#status").textContent === "Adding image…",
      null,
      { timeout: 5000 },
    );
    assert.equal((await editor(page)).text, before, "no placeholder in the text");
    await settled(page, ++count);
    const { text, head } = await editor(page);
    assert.match(text.slice(before.length), /^!\[\]\(images\/[0-9a-f]{16}\.png\)\n$/);
    assert.equal(head, text.length);
    await page.waitForFunction(
      () => document.querySelector(".cm-image")?.naturalWidth === 320,
      null,
      { timeout: 10000 },
    );
    const seen = await statuses.jsonValue();
    assert.ok(!seen.some((s) => s.startsWith("Offline")), seen.join());
    assert.equal(await server(page), text);
    await page.screenshot({ path: path.join(evidence, "pasted.png") });
    // View mode, the rendered document readers get, shows it too.
    await page.locator('#modes [data-mode="view"]').click();
    await page.waitForFunction(
      () => document.querySelector("#content img")?.naturalWidth === 320,
      null,
      { timeout: 10000 },
    );
    await page.locator('#modes [data-mode="edit"]').click();
    await page.locator(".cm-content").click();
    await page.keyboard.press("ControlOrMeta+End");
  });

  await test("next to text it stays inline", async () => {
    await typeAtEnd(page, "see ");
    await copyImage(page, 21);
    await pasteKey(page);
    await settled(page, ++count);
    const { text, head } = await editor(page);
    assert.match(text, /\nsee !\[\]\(images\/[0-9a-f]{16}\.png\)$/);
    assert.equal(head, text.length);
  });

  await test("Undo removes it, and the same picture again adds no file", async () => {
    const before = await server(page);
    await page.keyboard.press("ControlOrMeta+Z");
    await page.waitForFunction(
      () =>
        !/see !\[\]/.test(
          document.querySelector(".cm-content").cmTile.root.view.state.doc.toString(),
        ),
      null,
      { timeout: 5000 },
    );
    await saved(page);
    assert.notEqual(await server(page), before);
    count--;
    await pasteKey(page);
    await settled(page, ++count);
    assert.equal(await server(page), before);
  });

  await test("Undo takes the image alone, not the typing around it", async () => {
    await typeAtEnd(page, "\n\n");
    await copyImage(page, 23);
    // The host's answer is held so the typing lands right up to the insert,
    // inside the window in which Undo would merge them.
    const held = await holdAnswer(page);
    await pasteKey(page);
    await held.done;
    await page.keyboard.type("BC");
    held.release();
    await page.waitForFunction(
      () =>
        /BC!\[\]/.test(
          document.querySelector(".cm-content").cmTile.root.view.state.doc.toString(),
        ),
      null,
      { timeout: 10000 },
    );
    await page.keyboard.type("DE");
    await settled(page, ++count);
    assert.match((await editor(page)).text, /BC!\[\]\(images\/[0-9a-f]{16}\.png\)DE$/);
    await page.keyboard.press("ControlOrMeta+Z");
    assert.match((await editor(page)).text, /BC!\[\]\(images\/[0-9a-f]{16}\.png\)$/);
    await page.keyboard.press("ControlOrMeta+Z");
    assert.match((await editor(page)).text, /BC$/);
    count--;
    await page.keyboard.press("ControlOrMeta+Shift+Z");
    assert.match((await editor(page)).text, /BC!\[\]\(images\/[0-9a-f]{16}\.png\)$/);
    count++;
    await saved(page);
  });

  await test("a selection is replaced", async () => {
    await page.keyboard.press("Enter");
    await page.keyboard.press("Enter");
    await page.keyboard.type("REPLACE ME");
    await page.keyboard.press("Shift+Home");
    await copyImage(page, 22);
    await pasteKey(page);
    await settled(page, ++count);
    const { text } = await editor(page);
    assert.ok(!text.includes("REPLACE ME"), text);
  });

  await test("several images keep their order", async () => {
    await typeAtEnd(page, "\n\n");
    const start = (await editor(page)).text.length;
    await pasteEvent(page, { widths: [31, 32] });
    count += 2;
    await settled(page, count);
    const added = (await editor(page)).text.slice(start).match(allReferences);
    assert.equal(added.length, 2);
    const widths = await page.evaluate(async (added) => {
      const out = [];
      for (const markdown of added) {
        const image = new Image();
        image.src = markdown.slice(4, -1);
        await image.decode();
        out.push(image.naturalWidth);
      }
      return out;
    }, added);
    assert.deepEqual(widths, [31, 32]);
  });

  await test("a caret moved elsewhere stays there and the image keeps its place", async () => {
    await typeAtEnd(page, "\n\nFIRST\n\nSECOND");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("End");
    await copyImage(page, 24);
    const held = await holdAnswer(page);
    await pasteKey(page);
    await held.done;
    await page.keyboard.press("ControlOrMeta+End");
    held.release();
    await settled(page, ++count);
    const { text, head } = await editor(page);
    assert.match(text, new RegExp(`\\nFIRST${image}\\n\\nSECOND$`));
    assert.equal(head, text.length, "the caret stayed at the end");
  });

  await test("a selection changed during the add keeps its text and the image follows it", async () => {
    const other = await openEditor(context, privateURL);
    await page.bringToFront();
    await typeAtEnd(page, "\n\nKEEP THIS");
    await textMatches(other, "KEEP THIS$");
    await page.keyboard.press("Shift+Home");
    await copyImage(page, 25);
    const held = await holdAnswer(page);
    await pasteKey(page);
    await held.done;
    await other.locator(".cm-content").evaluate((el) => {
      const { view } = el.cmTile.root;
      const at = view.state.doc.toString().lastIndexOf("THIS");
      view.dispatch({ changes: { from: at, insert: "ALL OF " } });
    });
    await textMatches(page, "KEEP ALL OF THIS$");
    held.release();
    await settled(page, ++count);
    await settled(other, count);
    const { text } = await editor(page);
    assert.match(text, new RegExp(`\\nKEEP ALL OF THIS${image}$`));
    assert.deepEqual(other.errors, []);
    await other.close();
  });

  await test("another editor typing through the add and its reconnect loses nothing", async () => {
    await typeAtEnd(page, "\n\nHERE\n\nTHEIRS ");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("End");
    const other = await openEditor(context, privateURL);
    await page.bringToFront();
    await textMatches(other, "THEIRS $");
    let sockets = 0;
    other.on("websocket", () => sockets++);
    await other.bringToFront();
    await other.locator(".cm-content").click();
    await other.keyboard.press("ControlOrMeta+End");
    // Tokens go in one at a time from before the paste until after the page
    // has its answer and the other editor has reconnected.
    let typing = true,
      tokens = 0,
      atRequest = -1,
      atResponse = -1;
    const typed = (async () => {
      while (typing) {
        // A fast typist's pace. Faster than the server's rate limit for one
        // connection (60 frames a second) stops sync with or without an add.
        await other.keyboard.type(`t${++tokens} `, { delay: 50 });
      }
    })();
    page.on("request", (r) => {
      if (r.url().includes("/_docs/api/images")) atRequest = tokens;
    });
    page.on("response", (r) => {
      if (r.url().includes("/_docs/api/images")) atResponse = tokens;
    });
    await page.locator(".cm-content").evaluate((el) => el.cmTile.root.view.focus());
    await pasteEvent(page, { widths: [41] });
    await settled(page, ++count);
    for (const deadline = Date.now() + 15000; sockets === 0 && Date.now() < deadline; )
      await other.waitForTimeout(50);
    await other.waitForTimeout(300);
    typing = false;
    await typed;
    assert.ok(sockets > 0, "the other editor reconnected during the add");
    assert.ok(atRequest > 0 && atResponse > atRequest, `typing overlapped the add: ${atRequest} to ${atResponse} of ${tokens}`);
    assert.ok(tokens > atResponse, "typing continued after the answer");
    console.log(`  typed ${tokens} tokens, ${atRequest} to ${atResponse} during the add, ${sockets} new socket(s)`);
    // Both editors and the server reach the same text, with all of it.
    let mine, theirs, stored;
    for (const deadline = Date.now() + 20000; Date.now() < deadline; ) {
      mine = (await editor(page)).text;
      theirs = (await editor(other)).text;
      stored = await server(page);
      if (mine === theirs && mine === stored) break;
      await page.waitForTimeout(100);
    }
    assert.equal(theirs, mine);
    assert.equal(stored, mine);
    const all = Array.from({ length: tokens }, (_, i) => `t${i + 1}`).join(" ");
    assert.ok(mine.endsWith(`THEIRS ${all} `), mine.slice(-200));
    assert.match(mine, new RegExp(`\\nHERE${image}\\n\\nTHEIRS `));
    assert.deepEqual(other.errors, []);
    await other.close();
    await page.bringToFront();
  });

  await test("text beside an image wins", async () => {
    await typeAtEnd(page, "\n\n");
    await pasteEvent(page, { widths: [51], text: "cell one\tcell two" });
    await page.waitForFunction(
      () =>
        document
          .querySelector(".cm-content")
          .cmTile.root.view.state.doc.toString()
          .includes("cell one"),
      null,
      { timeout: 5000 },
    );
    assert.equal((await editor(page)).text.match(allReferences).length, count);
    await saved(page);
  });

  await test("a refused image reports, and the selection and text stay", async () => {
    await typeAtEnd(page, "\n\nKEEP ME");
    await page.keyboard.press("Shift+Home");
    const before = await editor(page);
    await pasteEvent(page, {
      type: "image/svg+xml",
      bytes: '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
    });
    const notice = page.locator("#image-notice");
    await notice.waitFor({ timeout: 10000 });
    assert.match(await notice.innerText(), /cannot be accepted/);
    assert.equal((await editor(page)).text, before.text);
    await page.screenshot({ path: path.join(evidence, "refused.png") });
    await page.getByRole("button", { name: "Dismiss" }).click();
    assert.equal(await notice.isVisible(), false);
    await saved(page);
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(" still editing");
    await page.waitForFunction(
      async () =>
        (
          await (await fetch("/_docs/api/document?doc=index.md")).json()
        ).markdown.includes("KEEP ME still editing"),
      null,
      { timeout: 10000 },
    );
  });

  assert.deepEqual(page.errors, []);

  await test("if syncing stops during an add, nothing is inserted and the interruption stays", async () => {
    let server;
    const stopping = await context.newPage();
    await stopping.routeWebSocket(/\/_docs\/ws/, (ws) => {
      server = ws;
      ws.connectToServer();
    });
    let release;
    const held = new Promise((done) => (release = done));
    await stopping.route("**/_docs/api/images*", async (route) => {
      await held;
      await route.continue();
    });
    await stopping.goto(privateURL);
    await stopping.locator(".cm-content").waitFor({ timeout: 20000 });
    await saved(stopping);
    await stopping.locator(".cm-content").click();
    const before = (await editor(stopping)).text;
    await pasteEvent(stopping, { widths: [71] });
    await stopping.waitForFunction(
      () => document.querySelector("#status").textContent === "Adding image…",
    );
    // An invalid frame stops the page's sync for good.
    server.send("not a sync message");
    await stopping.locator("#notice button").first().waitFor({ timeout: 5000 });
    release();
    const notice = stopping.locator("#image-notice");
    await notice.waitFor({ timeout: 10000 });
    assert.match(await notice.innerText(), /stopped syncing/);
    assert.equal(await stopping.locator("#notice").isVisible(), true);
    assert.equal((await editor(stopping)).text, before);
    // A stopped editor is read-only: another paste sends nothing.
    await stopping.getByRole("button", { name: "Dismiss" }).click();
    const posts = [];
    stopping.on("request", (r) => {
      if (r.method() === "POST") posts.push(r.url());
    });
    await pasteEvent(stopping, { widths: [72] });
    await stopping.waitForTimeout(300);
    assert.deepEqual(posts, []);
    assert.equal(await notice.isVisible(), false);
    await stopping.close();
  });
  await context.close();

  await test("a guest sees a notice and nothing is sent", async () => {
    const guests = await browser.newContext({
      permissions: ["clipboard-read", "clipboard-write"],
    });
    const guest = await guests.newPage();
    const posts = [];
    guest.on("request", (r) => {
      if (r.method() === "POST") posts.push(r.url());
    });
    await guest.goto(publicURL);
    await guest.locator("#guest-name").waitFor({ timeout: 20000 });
    await guest.locator("#guest-name").fill("Sam");
    await guest.getByRole("button", { name: "Join and edit" }).click();
    await guest.locator(".cm-content").waitFor({ timeout: 20000 });
    await saved(guest);
    const before = (await editor(guest)).text;
    await guest.locator(".cm-content").click();
    await pasteEvent(guest, { widths: [61] });
    const notice = guest.locator("#image-notice");
    await notice.waitFor({ timeout: 5000 });
    assert.match(await notice.innerText(), /Guests can't add images/);
    assert.equal((await editor(guest)).text, before);
    assert.deepEqual(posts, []);
    await guests.close();
  });
} finally {
  await browser.close();
}
console.log("image paste passed");
