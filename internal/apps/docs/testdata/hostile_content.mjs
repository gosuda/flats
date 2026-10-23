// Hostile guest Markdown on member, viewer and guest pages
// (docs-public-edit-TECH.md 3.10): no script the text carries runs, no
// request leaves the flat's origin, links carry rel="noopener noreferrer"
// and unsafe destinations are not links, also after the member asks to see
// the guest's diagram.
//
//	node hostile_content.mjs <url> <evidence dir>
import assert from "node:assert/strict";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href
);
const [url, evidence] = process.argv.slice(2);
const origin = new URL(url).origin;
const browser = await chromium.launch({ headless: true });
console.log("Chromium", browser.version());
const errors = [],
  dialogs = [],
  // Requests to another origin: sent to the network (the route saw them),
  // and only attempted (the page asked for them; the browser may block
  // them before they leave, for example by the page's CSP).
  sent = [],
  attempted = [],
  failed = [],
  problems = [],
  csp = [],
  pages = {};
const HOSTILE = [
  "",
  "HOSTILE-BEGIN",
  "",
  "<script>window.__pwned = 'script'</script>",
  "",
  "<img src=x onerror=\"window.__pwned = 'img'\">",
  "",
  "[javascript link](javascript:window.__pwned='link')",
  "",
  "[data link](data:text/html,<script>parent.__pwned='data'</script>)",
  "",
  "![remote image](https://example.invalid/a.png)",
  "",
  '<iframe src="https://example.invalid/frame"></iframe>',
  "",
  '<a href="https://example.invalid/raw" onclick="window.__pwned=\'raw\'">raw link</a>',
  "",
  "[safe link](https://example.invalid/page)",
  "",
  "```mermaid",
  "graph TD",
  "  A[\"<script>window.__pwned='label'</script><img src=x onerror=window.__pwned='label-img'>\"] --> B",
  '  click A call window.__pwnedCallback("x")',
  "  click B \"javascript:window.__pwned='click'\"",
  "```",
  "",
  "HOSTILE-END",
  "",
].join("\n");
async function open(name, { member = false } = {}) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
  });
  if (member)
    await context.addCookies([{ name: "flats-test-private", value: "1", url }]);
  // Every request outside the flat's origin is recorded and refused.
  await context.route("**/*", (route) => {
    const u = route.request().url();
    if (new URL(u).origin === origin) return route.continue();
    sent.push(name + " " + u);
    return route.abort();
  });
  const page = await context.newPage();
  page.on("request", (r) => {
    const u = r.url();
    if (!/^(data|blob):/.test(u) && new URL(u).origin !== origin)
      attempted.push(name + " " + r.resourceType() + " " + u);
  });
  page.on("requestfailed", (r) => {
    if (new URL(r.url()).origin !== origin)
      failed.push(name + " " + r.url() + " " + r.failure()?.errorText);
  });
  page.on("pageerror", (e) => errors.push(name + ": " + e.message));
  page.on("dialog", (d) => {
    dialogs.push(name + ": " + d.message());
    d.dismiss();
  });
  page.on("console", (m) => {
    if (/Content Security Policy/i.test(m.text()))
      csp.push(name + ": " + m.text());
  });
  const response = await page.goto(url + "/index.md");
  // Spec rule 21a: resources load only from the flat itself or as data:
  // images. The page's policy enforces it for every renderer.
  const policy = response.headers()["content-security-policy"] || "";
  if (
    !/img-src 'self' data:/.test(policy) ||
    !/default-src 'self'/.test(policy)
  )
    problems.push(name + ": page policy does not confine resources: " + policy);
  pages[name] = page;
  return page;
}
const has = (page, needle) =>
  page.waitForFunction(
    (needle) => document.body.textContent.includes(needle),
    needle,
    { timeout: 20000 },
  );
try {
  const member = await open("member", { member: true });
  await member.locator(".cm-content").waitFor({ timeout: 20000 });
  const viewer = await open("viewer");
  await viewer.locator("#guest-name").waitFor({ timeout: 20000 });
  await viewer.getByRole("button", { name: "View only" }).click();
  const guest = await open("guest");
  await guest.locator("#guest-name").waitFor({ timeout: 20000 });
  await guest.locator("#guest-name").fill("Mallory");
  await guest.getByRole("button", { name: "Join and edit" }).click();
  await guest.waitForFunction(
    () => document.querySelector("header")?.textContent.includes("You are"),
    null,
    { timeout: 20000 },
  );
  // The guest types the Markdown into its editor.
  await guest.locator(".cm-content").click();
  await guest.keyboard.press("ControlOrMeta+End");
  await guest.keyboard.insertText(HOSTILE);
  await guest.waitForFunction(
    () => document.querySelector("#status")?.textContent === "Saved",
    null,
    { timeout: 20000 },
  );
  for (const page of [member, viewer, guest]) await has(page, "HOSTILE-END");
  // The member also views the rendered document, and asks for the guest's
  // diagram in the editor.
  await member.waitForTimeout(1000);
  await member.screenshot({ path: path.join(evidence, "member-edit.png") });
  const show = member.getByRole("button", { name: "Show diagram" });
  await show.first().waitFor({ timeout: 20000 });
  await show.first().click();
  await member.waitForFunction(
    () =>
      [...document.querySelectorAll(".mermaid-diagram")].some((d) =>
        ["drawn", "error"].includes(d.dataset.state),
      ),
    null,
    { timeout: 30000 },
  );
  console.log(
    "member diagram after Show diagram:",
    await member.evaluate(() =>
      [...document.querySelectorAll(".mermaid-diagram")].map(
        (d) => d.dataset.state + (d.dataset.error ? "/" + d.dataset.error : ""),
      ),
    ),
  );
  // Clicking the drawn nodes runs nothing either.
  for (const node of await member.locator(".mermaid-svg .node").all())
    await node.click({ timeout: 2000 }).catch(() => {});
  await member.screenshot({ path: path.join(evidence, "member-diagram.png") });
  await member.locator('#modes [data-mode="view"]').click();
  await has(member, "HOSTILE-END");
  await member.waitForTimeout(1000);
  await member.screenshot({
    path: path.join(evidence, "member-view.png"),
    fullPage: true,
  });
  await viewer.screenshot({
    path: path.join(evidence, "viewer.png"),
    fullPage: true,
  });
  await guest.screenshot({ path: path.join(evidence, "guest.png") });
  // Links: every link is safe, and unsafe destinations are not links.
  for (const [name, page] of Object.entries(pages)) {
    const found = await page.evaluate(() => {
      const out = { links: [], scripts: 0, iframes: 0, handlers: [] };
      for (const root of document.querySelectorAll("#content, .cm-content")) {
        for (const a of root.querySelectorAll("a[href]"))
          out.links.push({
            text: a.textContent,
            href: a.getAttribute("href"),
            rel: a.getAttribute("rel") || "",
          });
        out.scripts += root.querySelectorAll("script").length;
        out.iframes += root.querySelectorAll("iframe, object, embed").length;
        for (const el of root.querySelectorAll("*"))
          for (const attr of el.getAttributeNames())
            if (/^on/i.test(attr)) out.handlers.push(el.tagName + " " + attr);
      }
      out.pwned = window.__pwned ?? null;
      return out;
    });
    console.log(name, JSON.stringify(found));
    const check = (ok, message) => ok || problems.push(message);
    check(found.pwned === null, name + ": a script ran: " + found.pwned);
    check(found.scripts === 0, name + ": a script element");
    check(found.iframes === 0, name + ": an embedded frame");
    check(
      !found.handlers.length,
      name + ": event handler attributes " + found.handlers,
    );
    const images = await page.evaluate(() =>
      [...document.querySelectorAll("#content img, .cm-content img")].map((i) =>
        i.getAttribute("src"),
      ),
    );
    console.log(name, "images:", JSON.stringify(images));
    for (const link of found.links) {
      check(
        /^https?:/i.test(new URL(link.href, url).href),
        name + ": unsafe link " + link.href,
      );
      check(
        /\bnoopener\b/.test(link.rel) && /\bnoreferrer\b/.test(link.rel),
        name + ": link without noopener noreferrer: " + JSON.stringify(link),
      );
    }
  }
  console.log("CSP reports:", csp.length);
  for (const line of csp) console.log("  " + line);
  console.log("requests sent to another origin:", JSON.stringify(sent));
  console.log(
    "requests attempted to another origin:",
    JSON.stringify(attempted),
  );
  console.log("failed requests to another origin:", JSON.stringify(failed));
  if (dialogs.length) problems.push("dialogs opened: " + dialogs);
  if (sent.length) problems.push("requests left the flat's origin: " + sent);
  // A renderer may still ask for an outside image; the page's policy must
  // refuse every such attempt before it is sent (rule 21a is about what
  // loads). Each attempt must have failed with the policy as the reason.
  for (const a of attempted) {
    const u = a.split(" ").at(-1);
    if (!failed.some((f) => f.includes(u) && /csp/i.test(f)))
      problems.push(
        "an outside request was not refused by the page policy: " + a,
      );
  }
  if (errors.length) problems.push("page errors: " + errors);
  assert.deepEqual(problems, []);
  console.log(
    "PASS: no script ran, no request left the origin (" +
      attempted.length +
      " outside attempts refused by the page policy), links safe on member (edit and view), viewer and guest pages.",
  );
} catch (e) {
  for (const [name, page] of Object.entries(pages))
    await page
      .screenshot({ path: path.join(evidence, name + "-failure.png") })
      .catch(() => {});
  console.log("sent:", sent, "attempted:", attempted, "errors:", errors);
  throw e;
} finally {
  await browser.close();
}
