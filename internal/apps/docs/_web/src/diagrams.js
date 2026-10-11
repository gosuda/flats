import { createDiagramRuntime } from "./diagram-runtime.js";
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
    theme = dark.matches ? "dark" : "default";
  requests.set(el, token);
  // CodeMirror builds widgets before mounting them. A fresh detached widget
  // still owns its request; once mounted, removal invalidates it. Explicit
  // destroyDiagram also cancels widgets discarded before their first mount.
  let mounted = el.isConnected;
  const current = () => {
    if (el.isConnected) mounted = true;
    return requests.get(el) === token && (!mounted || el.isConnected);
  };
  el.dataset.state = "pending";
  delete el.dataset.error;
  el.setAttribute("aria-busy", "true");
  diagramTools(el, () => renderOne(el));
  status(el, "Rendering diagram…");
  return runtime
    .request(source, theme, current)
    .then(
      (result) => {
        if (!current() || !result) return;
        if (result.error) {
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
  for (const el of root.querySelectorAll(".mermaid-diagram")) {
    destroyDiagram(el);
    delete el.dataset.state;
  }
  return renderDiagrams(root);
}
export function invalidateDiagram(el, source) {
  destroyDiagram(el);
  el.querySelector(".mermaid-source").textContent = source;
}
export function updateDiagram(el, source) {
  invalidateDiagram(el, source);
  return renderOne(el);
}
export function onThemeChange(f) {
  dark.addEventListener("change", f);
}
