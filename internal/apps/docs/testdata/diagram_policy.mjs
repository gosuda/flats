// When diagrams render (docs-public-edit-TECH.md 3.10): a member page of a
// flat that is Public view only draws a diagram automatically; after the
// level becomes edit, a diagram written afterwards shows its source with
// Show diagram and does not draw, before any guest wrote and for a guest's
// diagram too, until the member clicks.
//
//	node diagram_policy.mjs <url> <evidence dir>
import assert from "node:assert/strict";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, evidence] = process.argv.slice(2);
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const errors = [];
let member, guest;
// states lists each diagram widget in the member's editor: its source's
// first node and its data-state.
const states = () =>
  member.evaluate(() =>
    [...document.querySelectorAll(".cm-content .mermaid-diagram")].map((d) => ({
      source: d.querySelector(".mermaid-source")?.textContent.trim(),
      state: d.dataset.state || "",
      show: [...d.querySelectorAll("button")].some(
        (b) => b.textContent === "Show diagram",
      ),
      svg: !!d.querySelector(".mermaid-svg svg"),
    })),
  );
const diagram = async (needle) =>
  (await states()).find((d) => d.source?.includes(needle));
async function waitFor(check, message, timeout = 20000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(message + ": " + JSON.stringify(await states()));
}
// append adds text at the end of a page's document without moving its
// selection, so the new block shows as a widget.
const append = (page, insert) =>
  page.locator(".cm-content").evaluate((el, insert) => {
    const view = el.cmTile.root.view;
    view.dispatch({ changes: { from: view.state.doc.length, insert } });
  }, insert);
try {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 1400 },
  });
  await context.addCookies([{ name: "flats-test-private", value: "1", url }]);
  member = await context.newPage();
  member.on("pageerror", (e) => errors.push("member: " + e.message));
  await member.goto(url + "/index.md");
  await member.locator(".cm-content").waitFor({ timeout: 20000 });
  // Public view only: the diagram draws without a click.
  await waitFor(
    async () => (await diagram("Start"))?.state === "drawn",
    "the diagram did not draw automatically at level view",
    30000,
  );
  assert.equal((await diagram("Start")).svg, true);
  await member.screenshot({ path: path.join(evidence, "1-view-drawn.png") });
  // The level becomes edit through the host access operation.
  assert.equal(
    await member.evaluate(
      async () =>
        (await fetch("/test-access?level=edit&epoch=2", { method: "POST" }))
          .status,
    ),
    204,
  );
  // The member writes a new diagram before any guest wrote.
  await member.waitForTimeout(1000);
  await append(member, "\n```mermaid\ngraph TD\n  Member --> Written\n```\n");
  await waitFor(
    async () => (await diagram("Member"))?.state === "request",
    "the member's new diagram is not on request",
  );
  await member.waitForTimeout(3000);
  let d = await diagram("Member");
  console.log("member diagram after the level change:", JSON.stringify(d));
  assert.equal(d.state, "request");
  assert.equal(d.show, true, "no Show diagram");
  assert.equal(d.svg, false, "the diagram drew");
  await member.screenshot({
    path: path.join(evidence, "2-edit-on-request.png"),
  });
  // A guest writes a diagram.
  const guestContext = await browser.newContext({
    viewport: { width: 1280, height: 900 },
  });
  guest = await guestContext.newPage();
  guest.on("pageerror", (e) => errors.push("guest: " + e.message));
  await guest.goto(url + "/index.md");
  await guest.locator("#guest-name").waitFor({ timeout: 20000 });
  await guest.locator("#guest-name").fill("Mina");
  await guest.getByRole("button", { name: "Join and edit" }).click();
  await guest.waitForFunction(
    () =>
      document.querySelector("header")?.textContent.includes("You are Mina"),
    null,
    { timeout: 20000 },
  );
  await append(guest, "\n```mermaid\ngraph TD\n  Guest --> Drawn\n```\n");
  await waitFor(
    async () => (await diagram("Guest"))?.state === "request",
    "the guest's diagram is not on request in the member page",
  );
  await member.waitForTimeout(3000);
  d = await diagram("Guest");
  console.log("guest diagram in the member page:", JSON.stringify(d));
  assert.equal(d.show, true, "no Show diagram");
  assert.equal(d.svg, false, "the guest's diagram drew without a click");
  await member.screenshot({
    path: path.join(evidence, "3-guest-on-request.png"),
  });
  // Clicking Show diagram draws it.
  await member
    .locator(".cm-content .mermaid-diagram", { hasText: "Guest" })
    .getByRole("button", { name: "Show diagram" })
    .click();
  await waitFor(
    async () => (await diagram("Guest"))?.svg === true,
    "Show diagram did not draw the guest's diagram",
    30000,
  );
  assert.equal(
    (await diagram("Member")).svg,
    false,
    "the member's diagram drew too",
  );
  await member.screenshot({ path: path.join(evidence, "4-guest-shown.png") });
  // The diagram drawn before the level changed stays drawn while unchanged;
  // a guest's change to it puts it on request too.
  console.log(
    "earlier diagram before the guest changes it:",
    JSON.stringify(await diagram("Start")),
  );
  await guest.locator(".cm-content").evaluate((el) => {
    const view = el.cmTile.root.view,
      at = view.state.doc.toString().indexOf("Start --> Finish") + 16;
    view.dispatch({ changes: { from: at, insert: "\n  Finish --> Changed" } });
  });
  await waitFor(
    async () => (await diagram("Start"))?.source.includes("Changed"),
    "the guest's change to the earlier diagram did not arrive",
  );
  await member.waitForTimeout(3000);
  d = await diagram("Start");
  console.log("earlier diagram after the guest changed it:", JSON.stringify(d));
  assert.equal(d.svg, false, "the changed diagram drew without a click");
  assert.equal(d.show, true, "no Show diagram on the changed diagram");
  await member.screenshot({
    path: path.join(evidence, "5-changed-on-request.png"),
  });
  assert.deepEqual(errors, []);
  console.log(
    "PASS: drawn automatically at view; after edit, the member's and the guest's new diagrams wait for Show diagram, which draws only the one clicked; a guest's change to an earlier diagram puts it on request.",
  );
} catch (e) {
  for (const [name, page] of [
    ["member", member],
    ["guest", guest],
  ])
    if (page)
      await page
        .screenshot({ path: path.join(evidence, name + "-failure.png") })
        .catch(() => {});
  throw e;
} finally {
  await browser.close();
}
