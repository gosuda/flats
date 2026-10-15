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
    // How a viewed revision shows: "highlight" (what it changed since the
    // revision before it), "source" (that as a line diff), "live" (a line
    // diff against the live text) or "plain".
    mode = "highlight",
    confirming = false,
    // Bumped by every selection, return to live and close, so a response
    // that arrives late cannot bring back what the person left.
    selection = 0,
    titles = new Map();
  const md = createMarkdown();
  // Blocks a revision changed carry rev-changed; a block after removed text
  // carries data-removed. Fences render through markdown.js's own rule, so
  // they are wrapped instead.
  for (const type of ["fence", "code_block"]) {
    const base =
      md.renderer.rules[type] ||
      ((tokens, i, options, env, self) => self.renderToken(tokens, i, options));
    md.renderer.rules[type] = (tokens, i, options, env, self) => {
      const html = base(tokens, i, options, env, self),
        meta = tokens[i].meta || {};
      if (!meta.revChanged && !meta.revRemoved) return html;
      const removed = meta.revRemoved
        ? ` data-removed="${Number(meta.revRemoved)}"`
        : "";
      return `<div class="${meta.revChanged ? "rev-changed " : ""}rev-block"${removed}>${html}</div>`;
    };
  }
  // renderChanges renders a revision with what it changed since before
  // marked, or returns null when the texts differ too much to compare.
  function renderChanges(markdown, before) {
    const changes = lineChanges(before, markdown);
    if (!changes) return null;
    const env = {},
      tokens = md.parse(markdown, env),
      // Reference definitions whose target changed: the links using them
      // render differently although their own lines stayed.
      old = {};
    md.parse(before, old);
    const labels = new Set(),
      now = env.references || {},
      was = old.references || {};
    for (const label of new Set([...Object.keys(now), ...Object.keys(was)]))
      if (
        now[label]?.href !== was[label]?.href ||
        now[label]?.title !== was[label]?.title
      )
        labels.add(label);
    // The links and images that use a changed definition: parsed again
    // with those definitions pointing at a marker, they are the ones whose
    // destination is the marker. The parser fills only missing definitions,
    // so the ones set here win.
    const consumers = new Set();
    if (labels.size) {
      const marker = "#flats-revision-reference",
        probe = { references: {} };
      for (const label of labels)
        probe.references[label] = { href: marker, title: "" };
      md.parse(markdown, probe).forEach((t, i) => {
        if (
          t.type === "inline" &&
          t.children?.some(
            (c) =>
              (c.type === "link_open" && c.attrGet("href") === marker) ||
              (c.type === "image" && c.attrGet("src") === marker),
          )
        )
          consumers.add(i);
      });
    }
    const out = markChanges(tokens, changes, consumers);
    for (const t of tokens) {
      if (!t.meta || t.type === "fence" || t.type === "code_block") continue;
      if (t.meta.revChanged) t.attrJoin("class", "rev-changed");
      if (t.meta.revRemoved)
        t.attrSet("data-removed", String(t.meta.revRemoved));
    }
    let html = md.renderer.render(tokens, md.options, env);
    if (out.tail) html += `<div data-removed="${out.tail}"></div>`;
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

  const removedText = (n) => `${n} line${n === 1 ? "" : "s"} removed here`;
  const srLabel = (text) => el("span", { class: "sr-only", text });

  // decorate turns the marks of a rendered revision into what people see
  // and hear. Changed text is highlighted run by run, as in the line it sits
  // on, not across the block's width: the inline content of each changed
  // block goes into a span that starts with a label for screen readers.
  // Other changed blocks get the label before them, and removed text gets a
  // note of its own, a table row in a table.
  function decorate(container) {
    // Only phrasing content goes into the span; rules, lists and other
    // blocks inside a list item stay outside it.
    const phrasing =
      /^(A|ABBR|B|BDI|BDO|BR|CITE|CODE|DATA|DEL|DFN|EM|I|IMG|INPUT|INS|KBD|MARK|Q|S|SAMP|SMALL|SPAN|STRONG|SUB|SUP|TIME|U|VAR|WBR)$/;
    for (const block of container.querySelectorAll(
      "p.rev-changed, li.rev-changed, h1.rev-changed, h2.rev-changed, h3.rev-changed, h4.rev-changed, h5.rev-changed, h6.rev-changed",
    )) {
      let span = null;
      for (const node of [...block.childNodes]) {
        if (node.nodeType === 1 && !phrasing.test(node.tagName)) {
          span = null;
          continue;
        }
        if (!span) {
          span = el("span", { class: "rev-mark" }, srLabel("Changed: "));
          block.insertBefore(span, node);
        }
        span.append(node);
      }
    }
    for (const node of container.querySelectorAll(
      "table.rev-changed, tr.rev-changed, hr.rev-changed, .rev-block.rev-changed, blockquote.rev-changed, ul.rev-changed, ol.rev-changed",
    )) {
      if (node.tagName === "TR")
        node.firstElementChild?.prepend(srLabel("Changed row: "));
      else if (node.tagName === "BLOCKQUOTE" || node.tagName === "DIV")
        node.prepend(srLabel("Changed: "));
      // A table, list or rule holds no free text: the label goes before it.
      else
        node.before(
          el("p", {
            class: "sr-only",
            text: node.tagName === "HR" ? "Changed: a rule." : "Changed:",
          }),
        );
    }
    for (const node of container.querySelectorAll("[data-removed]")) {
      const text = "− " + removedText(Number(node.dataset.removed));
      let note;
      if (node.tagName === "TR")
        note = el(
          "tr",
          { class: "rev-removed-note" },
          el("td", { colspan: String(node.children.length || 1), text }),
        );
      else note = el("div", { class: "rev-removed-note", text });
      if (node.tagName === "DIV" && !node.className) node.replaceWith(note);
      // Inside a list, a note of its own would take an item's number.
      else if (node.tagName === "LI") node.prepend(note);
      else node.before(note);
      node.removeAttribute("data-removed");
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
      if (changed?.has(p) && node.tagName === "IMG") {
        node.classList.add("rev-changed");
        node.before(srLabel("Changed image: "));
      }
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

  // A mode button: pressing the pressed one returns to the plain view.
  const modeButton = (value, text, title) =>
    el("button", {
      type: "button",
      "aria-pressed": String(mode === value),
      text,
      title,
      onclick: () => {
        mode = mode === value ? "plain" : value;
        drawView();
      },
    });

  function diffView(lines, caption) {
    return [
      el("p", { class: "diff-caption", text: caption }),
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
              text: l.kind === "added" ? "+" : l.kind === "removed" ? "−" : " ",
            }),
            srLabel(
              l.kind === "added"
                ? "Added: "
                : l.kind === "removed"
                  ? "Removed: "
                  : "",
            ),
            l.line + "\n",
          ),
        ),
      ),
    ];
  }

  function drawView() {
    const { revision, markdown, documents, before, previous } = viewing,
      date = new Date(revision.created_at),
      comparable = markdown !== null && previous > 0;
    // A mode the revision cannot show falls back to the plain view.
    if (!comparable && (mode === "highlight" || mode === "source"))
      mode = "plain";
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
          comparable
            ? modeButton(
                "highlight",
                "Highlight changes",
                `What revision ${revision.number} changed since revision ${previous}`,
              )
            : null,
          comparable
            ? modeButton(
                "source",
                "Source changes",
                `The Markdown lines revision ${revision.number} changed since revision ${previous}`,
              )
            : null,
          markdown === null
            ? null
            : modeButton(
                "live",
                "Compare with live",
                "Line changes from this revision to the live text",
              ),
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
    const tooMany = "Too many changes to compare here.";
    if (mode === "live" || mode === "source") {
      const [from, to] =
        mode === "live" ? [markdown, liveText()] : [before, markdown];
      const lines = changeLines(from, to);
      if (!lines) {
        body.replaceChildren(el("p", { class: "diff-caption", text: tooMany }));
        return;
      }
      const same = lines.every((l) => l.kind === "same");
      body.replaceChildren(
        ...diffView(
          lines,
          mode === "live"
            ? same
              ? `The live text is the same as revision ${revision.number}.`
              : `Changes from revision ${revision.number} to the live text: − removed since, + added since.`
            : same
              ? `Revision ${revision.number} did not change this document's text.`
              : `What revision ${revision.number} changed since revision ${previous}: − removed, + added.`,
        ),
      );
      return;
    }
    const article = el("div", { class: "revision-rendered" });
    let caption = null;
    const shown = mode === "highlight" ? renderChanges(markdown, before) : null;
    if (shown) {
      replaceRendered(article, shown.html, destroyDiagram);
      decorate(article);
      rewrite(article, revision.number, viewing.changedFiles);
      const any =
        shown.marked ||
        shown.tail ||
        article.querySelector(".rev-removed-note, img.rev-changed");
      let text = !viewing.existed
        ? `This document is new in revision ${revision.number}.`
        : any || shown.unshown
          ? `Highlighted: what revision ${revision.number} changed in this document since revision ${previous}.`
          : `Revision ${revision.number} did not change this document.`;
      if (shown.unshown)
        text +=
          " Some changes have no visible text here, such as link targets: see Source changes.";
      caption = el("p", { class: "diff-caption", text });
    } else {
      if (mode === "highlight")
        caption = el("p", {
          class: "diff-caption",
          text: tooMany + " Showing the revision as it was.",
        });
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
        before = "",
        existed = false,
        changedFiles = new Set();
      if (documents.some((d) => d.path === path)) {
        const doc = await api(
          `/${number}/document?doc=${encodeURIComponent(path)}`,
        );
        markdown = doc.markdown;
        if (info.previous) {
          // The revision before it: this document's text there, and its
          // files, so images that changed between the two are marked.
          const prev = await api(`/${info.previous}`);
          existed = (prev.documents || []).some((d) => d.path === path);
          if (existed)
            before = (
              await api(
                `/${info.previous}/document?doc=${encodeURIComponent(path)}`,
              )
            ).markdown;
          const old = new Map((prev.files || []).map((f) => [f.path, f.hash])),
            cur = new Map((info.files || []).map((f) => [f.path, f.hash]));
          for (const p of new Set([...old.keys(), ...cur.keys()]))
            if (old.get(p) !== cur.get(p)) changedFiles.add(p);
        }
      }
      if (mine !== selection) return;
      viewing = {
        revision: info.revision,
        markdown,
        documents,
        before,
        existed,
        changedFiles,
        previous: info.previous,
      };
      if (mode === "plain") mode = "highlight";
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
