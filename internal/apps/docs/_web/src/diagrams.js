import { createDiagramRuntime, tooLarge } from "./diagram-runtime.js";
import { diagramTools, uniqueSVG } from "./diagram-tools.js";
let loading,
  loadAttempt = 0,
  sequence = 0;
const dark = matchMedia("(prefers-color-scheme: dark)"),
  requests = new WeakMap();
const freshID = () =>
  "flats-mermaid-" + ++sequence + "-" + Math.random().toString(36).slice(2, 10);
const runtime = createDiagramRuntime(() => {
  const url = loadAttempt
    ? FLATS_MERMAID_URL + "?retry=" + loadAttempt
    : FLATS_MERMAID_URL;
  loading ||= import(url).then(
    (m) => m.default,
    (error) => {
      loading = undefined;
      loadAttempt++;
      throw error;
    },
  );
  return loading;
}, freshID);
const observed = new Set();
const observer =
  typeof IntersectionObserver === "undefined"
    ? null
    : new IntersectionObserver(
        (entries) => {
          for (const entry of entries)
            if (entry.isIntersecting) {
              observer.unobserve(entry.target);
              observed.delete(entry.target);
              renderOne(entry.target);
            }
        },
        { rootMargin: "400px" },
      );

// View mode replaces whole document subtrees; release unseen placeholders too.
if (observer && typeof MutationObserver !== "undefined") {
  new MutationObserver(() => {
    for (const el of observed) if (!el.isConnected) destroyDiagram(el);
  }).observe(document.documentElement, { childList: true, subtree: true });
}
export function destroyDiagram(el) {
  requests.delete(el);
  observer?.unobserve(el);
  observed.delete(el);
}

// Once the page may show text anyone could have written (a guest wrote in
// the document, or the flat is open to anyone for editing), diagrams draw
// only when the reader asks, within size caps (docs-public-edit-TECH
// 3.10). The page never switches back before a reload.
let onRequest = false;
export function drawOnRequest() {
  if (onRequest) return;
  onRequest = true;
  for (const el of [...observed]) ask(el);
}
// ask shows a diagram's source with Show diagram instead of drawing it.
function ask(el) {
  destroyDiagram(el);
  el.querySelector(".mermaid-svg")?.remove();
  el.dataset.state = "request";
  delete el.dataset.error;
  el.removeAttribute("aria-busy");
  diagramTools(el, () => renderOne(el));
  status(el, "");
}

function status(el, message, error = false) {
  let note = el.querySelector(".mermaid-status");
  if (!note) {
    note = document.createElement("p");
    note.setAttribute("role", "status");
    el.append(note);
  }
  note.className = "mermaid-status" + (error ? " mermaid-error" : "");
  note.textContent = message;
}
function renderOne(el) {
  observer?.unobserve(el);
  observed.delete(el);
  const source = el.querySelector(".mermaid-source")?.textContent ?? "";
  const token = {},
    theme = dark.matches ? "dark" : "default",
    capped = onRequest;
  requests.set(el, token);
  // CodeMirror builds widgets before mounting them. A fresh detached widget
  // still owns its request; once mounted, removal invalidates it. Explicit
  // destroyDiagram also cancels widgets discarded before their first mount.
  let mounted = el.isConnected;
  const current = () => {
    if (el.isConnected) mounted = true;
    return requests.get(el) === token && (!mounted || el.isConnected);
  };
  delete el.dataset.error;
  const large = capped && tooLarge(source);
  if (large) {
    el.dataset.state = "error";
    el.dataset.error = "size";
    diagramTools(el, () => renderOne(el));
    status(el, large, true);
    el.dispatchEvent(new Event("diagram-rendered", { bubbles: true }));
    return Promise.resolve();
  }
  el.dataset.state = "pending";
  el.setAttribute("aria-busy", "true");
  diagramTools(el, () => renderOne(el));
  status(el, "Rendering diagram…");
  return runtime
    .request(source, theme, current, capped)
    .then(
      (result) => {
        if (!current() || !result) return;
        const large = capped && result.error && tooLarge(source, result.error);
        if (large) {
          el.dataset.state = "error";
          el.dataset.error = "size";
          status(el, large, true);
        } else if (result.error) {
          el.dataset.state = "error";
          el.dataset.error = "syntax";
          status(
            el,
            (el.querySelector(".mermaid-svg")
              ? "Preview kept from the last valid diagram. "
              : "") +
              "Diagram syntax error: " +
              result.error,
            true,
          );
        } else {
          el.querySelector(".mermaid-svg")?.remove();
          const figure = document.createElement("div");
          figure.className = "mermaid-svg";
          // Mermaid strict mode sanitizes the markup. Each instance has unique IDs.
          figure.innerHTML = result.svg;
          uniqueSVG(figure);
          el.querySelector(".mermaid-tools").after(figure);
          el.dataset.state = "drawn";
          status(el, "");
        }
        diagramTools(el, () => renderOne(el));
      },
      () => {
        if (!current()) return;
        el.dataset.state = "error";
        el.dataset.error = "load";
        status(
          el,
          "Diagram renderer could not load. Check your connection and retry.",
          true,
        );
        diagramTools(el, () => renderOne(el));
      },
    )
    .finally(() => {
      if (current()) {
        el.removeAttribute("aria-busy");
        el.dispatchEvent(new Event("diagram-rendered", { bubbles: true }));
      }
    });
}

// Resolves for visible work; offscreen placeholders render as they approach.
export function renderDiagrams(root) {
  if (onRequest) {
    for (const el of root.querySelectorAll(
      ".mermaid-diagram:not([data-state])",
    ))
      ask(el);
    return Promise.resolve();
  }
  const pending = [];
  // Measure the whole batch before toolbar/status writes invalidate layout.
  const placeholders = [
    ...root.querySelectorAll(".mermaid-diagram:not([data-state])"),
  ].map((el) => ({
    el,
    offscreen: observer && el.getBoundingClientRect().top > innerHeight + 400,
  }));
  for (const { el, offscreen } of placeholders) {
    if (offscreen) {
      el.dataset.state = "waiting";
      diagramTools(el, () => renderOne(el));
      status(el, "Diagram will render when scrolled into view.");
      observed.add(el);
      observer.observe(el);
    } else pending.push(renderOne(el));
  }
  return Promise.all(pending);
}
export function redrawDiagrams(root) {
  const asked = [];
  for (const el of root.querySelectorAll(".mermaid-diagram")) {
    // On request, only the diagrams the reader asked for draw again.
    if (onRequest) {
      if (!el.dataset.state) ask(el);
      else if (el.dataset.state !== "request") asked.push(el);
      continue;
    }
    destroyDiagram(el);
    delete el.dataset.state;
  }
  if (onRequest)
    return Promise.all(
      asked.map((el) => {
        destroyDiagram(el);
        return renderOne(el);
      }),
    );
  return renderDiagrams(root);
}
export function invalidateDiagram(el, source) {
  destroyDiagram(el);
  el.querySelector(".mermaid-source").textContent = source;
}
// updateDiagram draws a diagram's new source, or asks again on request: a
// click covers only the source the reader saw.
export function updateDiagram(el, source) {
  invalidateDiagram(el, source);
  if (!onRequest) return renderOne(el);
  ask(el);
  return Promise.resolve();
}
export function onThemeChange(f) {
  dark.addEventListener("change", f);
}
