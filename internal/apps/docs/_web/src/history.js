// The history panel of a Private document page: the flat's revisions,
// viewing one (rendered, or as changes against the live text), pinning and
// restoring. The routes are served by the host (core/docsroutes.go).
import { createMarkdown } from "./markdown.js";
import {
  ago,
  who,
  changed,
  changeLines,
  isMarkdown,
  lineChanges,
  markChanges,
} from "./history-format.js";
import { renderDiagrams, destroyDiagram } from "./diagrams.js";
import { replaceRendered } from "./diagram-preview.js";

const PAGE = 50,
  REFRESH = 10000,
  narrow = matchMedia("(max-width: 899px)");
const $ = (s) => document.querySelector(s);
const el = (tag, props = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false)
      node.setAttribute(k, v === true ? "" : v);
  }
  node.append(...children.filter((c) => c !== null && c !== undefined));
  return node;
};

async function api(path, options = {}) {
  const res = await fetch("/_docs/api/revisions" + path, {
    ...options,
    headers: options.body
      ? { "content-type": "application/json", "x-flats-docs": "1" }
      : undefined,
  });
  let body = null;
  try {
    body = await res.json();
  } catch {}
  if (!res.ok)
    throw Object.assign(
      new Error(body?.message || "The history is not available right now."),
      { status: res.status, code: body?.code },
    );
  return body;
}

export function initHistory({ path, name, liveText, onView, onLive, status }) {
  const toggle = $("#history-toggle"),
    panel = $("#history"),
    list = $("#history-list"),
    more = $("#history-more"),
    note = $("#history-note"),
    view = $("#revision-view"),
    banner = $("#revision-banner"),
    body = $("#revision-body");
  if (!toggle || !panel) return { enable() {} };
  let revisions = [],
    next = 0,
    timer = 0,
    viewing = null,
    showChanges = false,
    // Highlight what the viewed revision changed since the one before it.
    highlight = true,
    confirming = false,
    // Bumped by every selection, return to live and close, so a response
    // that arrives late cannot bring back what the person left.
    selection = 0,
    titles = new Map();
  const md = createMarkdown();
  // Blocks a revision changed carry rev-changed; a block after removed text
  // carries rev-removed. Fences render through markdown.js's own rule, so
  // they are wrapped instead.
  const removedText = (n) => `${n} line${n === 1 ? "" : "s"} removed here`;
  for (const type of ["fence", "code_block"]) {
    const base =
      md.renderer.rules[type] ||
      ((tokens, i, options, env, self) => self.renderToken(tokens, i, options));
    md.renderer.rules[type] = (tokens, i, options, env, self) => {
      const html = base(tokens, i, options, env, self),
        meta = tokens[i].meta || {};
      if (!meta.revChanged && !meta.revRemoved) return html;
      const removed = meta.revRemoved
        ? ` data-removed="${removedText(meta.revRemoved)}"`
        : "";
      return `<div class="${meta.revChanged ? "rev-changed " : ""}${meta.revRemoved ? "rev-removed " : ""}rev-block"${removed}>${html}</div>`;
    };
  }
  function renderChanges(markdown, before) {
    const env = {},
      tokens = md.parse(markdown, env),
      out = markChanges(tokens, lineChanges(before, markdown));
    for (const t of tokens) {
      if (!t.meta || t.type === "fence" || t.type === "code_block") continue;
      if (t.meta.revChanged) t.attrJoin("class", "rev-changed");
      if (t.meta.revRemoved) {
        t.attrJoin("class", "rev-removed");
        t.attrSet("data-removed", removedText(t.meta.revRemoved));
      }
    }
    let html = md.renderer.render(tokens, md.options, env);
    if (out.tail)
      html += `<p class="rev-removed rev-tail" data-removed="${removedText(out.tail)}"></p>`;
    return { html, ...out };
  }

  fetch("/_docs/api/documents")
    .then((r) => r.json())
    .then((d) => {
      for (const doc of d.documents) titles.set(doc.path, doc.title);
    })
    .catch(() => {});

  function say(message) {
    note.textContent = message || "";
    note.hidden = !message;
  }

  function render() {
    list.replaceChildren();
    for (const r of revisions) {
      const date = new Date(r.created_at),
        selected = viewing?.revision.number === r.number;
      const open = el(
        "button",
        {
          type: "button",
          class: "revision-open",
          "aria-current": selected ? "true" : null,
          onclick: () => show(r.number),
        },
        el("span", { class: "revision-who", text: who(r) }),
        el(
          "span",
          { class: "revision-meta" },
          el("time", {
            datetime: date.toISOString(),
            title: date.toLocaleString(),
            text: ago(date),
          }),
          el("span", { text: changed(r, titles, path) }),
        ),
        r.message && r.kind !== "created" && r.kind !== "rollback"
          ? el("span", { class: "revision-message", text: r.message })
          : null,
      );
      const pin = el("button", {
        type: "button",
        class: "revision-pin",
        "aria-pressed": String(!!r.pinned),
        "aria-label": `${r.pinned ? "Unpin" : "Pin"} revision ${r.number}`,
        title: r.pinned
          ? "Pinned: kept until you unpin it"
          : "Pin: keep this revision",
        text: r.pinned ? "Pinned" : "Pin",
        onclick: () => setPin(r),
      });
      list.append(
        el(
          "li",
          { class: "revision" + (selected ? " selected" : "") },
          el("span", { class: "revision-number", text: "#" + r.number }),
          open,
          pin,
        ),
      );
    }
    if (!revisions.length)
      list.append(
        el("li", { class: "revision-empty", text: "No revisions yet." }),
      );
    more.hidden = !next;
  }

  // A refresh reloads as many entries as are shown, up to the host's
  // page limit of 100; "Load more" appends the next page.
  async function load(reset = true) {
    const limit = reset
      ? Math.min(100, Math.max(PAGE, revisions.length))
      : PAGE;
    try {
      const page = await api(
        `?limit=${limit}` + (!reset && next ? `&before=${next}` : ""),
      );
      revisions = reset ? page.revisions : revisions.concat(page.revisions);
      next = page.next || 0;
      if (note.dataset.kind !== "action") say("");
      render();
    } catch (e) {
      say(e.message);
    }
  }

  function open() {
    panel.hidden = false;
    toggle.setAttribute("aria-expanded", "true");
    document.body.classList.add("history-open");
    load();
    clearInterval(timer);
    timer = setInterval(() => !document.hidden && load(), REFRESH);
  }
  // hide closes the panel and keeps a viewed revision on screen.
  function hide() {
    panel.hidden = true;
    toggle.setAttribute("aria-expanded", "false");
    document.body.classList.remove("history-open");
    clearInterval(timer);
  }
  function close() {
    hide();
    live();
    toggle.focus();
  }

  async function setPin(r) {
    note.dataset.kind = "action";
    try {
      await api(`/${r.number}/pin`, {
        method: "POST",
        body: JSON.stringify({ pinned: !r.pinned }),
      });
      say("");
    } catch (e) {
      say(e.message);
    }
    delete note.dataset.kind;
    await load();
  }

  // Changed text is highlighted run by run, as in the line it sits on, not
  // across the block's whole width: the inline content of each changed block
  // goes into a span.
  function markText(container) {
    const blocks = /^(UL|OL|P|PRE|DIV|TABLE|BLOCKQUOTE|H[1-6])$/;
    for (const block of container.querySelectorAll(
      "p.rev-changed, li.rev-changed, h1.rev-changed, h2.rev-changed, h3.rev-changed, h4.rev-changed, h5.rev-changed, h6.rev-changed",
    )) {
      let span = null;
      for (const node of [...block.childNodes]) {
        if (node.nodeType === 1 && blocks.test(node.tagName)) {
          span = null;
          continue;
        }
        if (!span) {
          span = el("span", { class: "rev-mark" });
          block.insertBefore(span, node);
        }
        span.append(node);
      }
    }
  }

  // A revision's links and images to the flat's files resolve to that
  // revision's copy; the host answers 404 for a file it does not hold.
  // With changed, images whose file the revision changed are marked too.
  function rewrite(container, number, changed) {
    const base = new URL("/" + path, location.origin);
    for (const node of container.querySelectorAll("img[src], a[href]")) {
      const attr = node.tagName === "IMG" ? "src" : "href";
      let url, p;
      try {
        url = new URL(node.getAttribute(attr), base);
        p = decodeURIComponent(url.pathname.slice(1));
      } catch {
        continue; // a malformed link stays as written
      }
      if (url.origin !== location.origin) continue;
      if (changed?.has(p) && node.tagName === "IMG")
        node.classList.add("rev-changed");
      if (p && !p.startsWith("_docs/") && !isMarkdown(p))
        node.setAttribute(
          attr,
          `/_docs/api/revisions/${number}/files/` +
            p.split("/").map(encodeURIComponent).join("/") +
            url.search +
            url.hash,
        );
    }
  }

  function drawView() {
    const { revision, markdown, documents } = viewing,
      date = new Date(revision.created_at);
    // replaceChildren would print a null; leave it out.
    banner.replaceChildren(
      ...[
        el(
          "div",
          { class: "revision-title" },
          el("strong", { text: `Revision ${revision.number}` }),
          el("span", { text: ` · ${who(revision)} · ` }),
          el("time", {
            datetime: date.toISOString(),
            title: date.toLocaleString(),
            text: ago(date),
          }),
        ),
        el(
          "div",
          { class: "revision-actions" },
          markdown === null || viewing.before === null
            ? null
            : el("button", {
                type: "button",
                "aria-pressed": String(highlight && !showChanges),
                text: "Highlight changes",
                title: `What revision ${revision.number} changed since revision ${viewing.previous}`,
                onclick: () => {
                  highlight = showChanges || !highlight;
                  showChanges = false;
                  drawView();
                },
              }),
          markdown === null
            ? null
            : el("button", {
                type: "button",
                "aria-pressed": String(showChanges),
                text: "Compare with live",
                onclick: () => {
                  showChanges = !showChanges;
                  drawView();
                },
              }),
          el("button", {
            type: "button",
            text: "Restore this revision",
            onclick: () => {
              confirming = true;
              drawView();
              banner.querySelector(".revision-confirm button")?.focus();
            },
          }),
          el("button", { type: "button", text: "Back to live", onclick: live }),
        ),
        confirming
          ? el(
              "div",
              {
                class: "revision-confirm",
                role: "group",
                "aria-label": "Confirm restore",
              },
              el("span", {
                text: `Restore every document to revision ${revision.number}? The current text stays in the history.`,
              }),
              el("button", {
                type: "button",
                class: "primary",
                text: "Restore",
                onclick: restore,
              }),
              el("button", {
                type: "button",
                text: "Cancel",
                onclick: () => {
                  confirming = false;
                  drawView();
                },
              }),
            )
          : null,
      ].filter(Boolean),
    );
    if (markdown === null) {
      body.replaceChildren(
        el("p", {
          text: "This document did not exist in this revision. Its documents:",
        }),
        el("ul", {}, ...documents.map((d) => el("li", { text: d.title }))),
      );
      return;
    }
    if (showChanges) {
      const lines = changeLines(markdown, liveText());
      const hasChanges = lines.some((l) => l.kind !== "same");
      body.replaceChildren(
        el("p", {
          class: "diff-caption",
          text: hasChanges
            ? `Changes from revision ${revision.number} to the live text: − removed since, + added since.`
            : `The live text is the same as revision ${revision.number}.`,
        }),
        el(
          "pre",
          { class: "diff" },
          ...lines.map((l) =>
            el(
              "span",
              { class: "diff-" + l.kind },
              el("span", {
                class: "diff-mark",
                "aria-hidden": "true",
                text:
                  l.kind === "added" ? "+" : l.kind === "removed" ? "−" : " ",
              }),
              el("span", {
                class: "sr-only",
                text:
                  l.kind === "added"
                    ? "Added: "
                    : l.kind === "removed"
                      ? "Removed: "
                      : "",
              }),
              l.line + "\n",
            ),
          ),
        ),
      );
      return;
    }
    const article = el("div", { class: "revision-rendered" });
    let caption = null;
    if (highlight && viewing.before !== null) {
      const shown = renderChanges(markdown, viewing.before);
      replaceRendered(article, shown.html, destroyDiagram);
      markText(article);
      const files = revision.changes || {};
      rewrite(
        article,
        revision.number,
        new Set([...(files.added || []), ...(files.changed || [])]),
      );
      const any =
        shown.marked ||
        shown.tail ||
        article.querySelector(".rev-removed, img.rev-changed");
      caption = el("p", {
        class: "diff-caption",
        text:
          viewing.before === ""
            ? `This document is new in revision ${revision.number}.`
            : any
              ? `Highlighted: what revision ${revision.number} changed in this document since revision ${viewing.previous}.`
              : `Revision ${revision.number} did not change this document.`,
      });
    } else {
      replaceRendered(article, md.render(markdown), destroyDiagram);
      rewrite(article, revision.number);
    }
    body.replaceChildren(...[caption, article].filter(Boolean));
    renderDiagrams(article);
  }

  async function show(number) {
    const mine = ++selection;
    try {
      const info = await api(`/${number}`);
      if (mine !== selection) return;
      const documents = info.documents || [];
      let markdown = null,
        before = null;
      if (documents.some((d) => d.path === path)) {
        const doc = await api(
          `/${number}/document?doc=${encodeURIComponent(path)}`,
        );
        markdown = doc.markdown;
        // The same document one revision earlier; "" when it is new there.
        if (info.previous) {
          try {
            before = (
              await api(
                `/${info.previous}/document?doc=${encodeURIComponent(path)}`,
              )
            ).markdown;
          } catch (e) {
            if (e.status !== 404) throw e;
            before = "";
          }
        }
      }
      if (mine !== selection) return;
      viewing = {
        revision: info.revision,
        markdown,
        documents,
        before,
        previous: info.previous,
      };
      showChanges = false;
      confirming = false;
      view.hidden = false;
      onView();
      drawView();
      render();
      // On a phone the panel covers the page: show the revision instead.
      if (narrow.matches) hide();
      view.focus();
    } catch (e) {
      say(e.message);
    }
  }

  function live() {
    selection++;
    if (!viewing) return;
    viewing = null;
    view.hidden = true;
    body.replaceChildren();
    onLive();
    render();
  }

  async function restore() {
    const n = viewing.revision.number;
    confirming = false;
    for (const b of banner.querySelectorAll("button")) b.disabled = true;
    note.dataset.kind = "action";
    try {
      await api(`/${n}/rollback`, {
        method: "POST",
        body: JSON.stringify({ name: name() }),
      });
      live();
      status(`Restored revision ${n}`);
      say("");
    } catch (e) {
      say(e.message);
      if (viewing) drawView();
    }
    delete note.dataset.kind;
    await load();
  }

  toggle.addEventListener("click", () => (panel.hidden ? open() : close()));
  $("#history-close").addEventListener("click", close);
  more.addEventListener("click", () => load(false));
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || (panel.hidden && !viewing)) return;
    if (document.querySelector(".mermaid-dialog[open], dialog[open]")) return;
    if (viewing) live();
    else close();
  });
  return {
    // A writable connection shows the control; read-only hides it.
    enable(writable) {
      toggle.hidden = !writable;
      if (!writable && !panel.hidden) close();
    },
  };
}
