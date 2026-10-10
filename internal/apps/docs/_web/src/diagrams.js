// Draws the Mermaid diagram placeholders markdown.js renders. Mermaid is
// large, so it loads on first use from its own content-hashed asset;
// documents without diagrams never fetch it. FLATS_MERMAID_URL is set by
// build.mjs.
let loading,
  initialized = "",
  queue = Promise.resolve();
// Drawn SVG (or an error) by theme and source, so re-rendering the document
// on every edit reuses unchanged diagrams instead of drawing them again.
const cache = new Map(),
  cacheLimit = 64,
  dark = matchMedia("(prefers-color-scheme: dark)");

function load() {
  loading ||= import(FLATS_MERMAID_URL).then(
    (m) => m.default,
    (err) => {
      loading = undefined;
      throw err;
    },
  );
  return loading;
}

// Each drawing gets its own id: the SVG scopes its styles and arrow markers
// by it, and a copy sharing ids with a hidden one can lose its markers.
const freshID = () =>
  "flats-mermaid-" + Math.random().toString(36).slice(2, 12);

// draw runs one Mermaid render at a time; theme changes reinitialize
// between them.
function draw(source, theme) {
  const job = queue.then(async () => {
    const mermaid = await load();
    if (initialized !== theme) {
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        suppressErrorRendering: true,
        theme,
      });
      initialized = theme;
    }
    const id = freshID();
    try {
      const { svg } = await mermaid.render(id, source);
      return { svg, id };
    } catch (err) {
      // Parse errors point at the column on their own line, so the
      // first lines keep their breaks.
      const message = String(err?.message || err || "Invalid diagram")
        .split("\n")
        .slice(0, 4)
        .join("\n");
      return { error: message.slice(0, 500) };
    }
  });
  queue = job.catch(() => {});
  return job;
}

function cached(key) {
  const value = cache.get(key);
  if (value) {
    cache.delete(key);
    cache.set(key, value);
  }
  return value;
}

function remember(key, value) {
  cache.set(key, value);
  if (cache.size > cacheLimit) cache.delete(cache.keys().next().value);
}

// show puts a drawing (or its error) before the source, which CSS hides
// once drawn; the source stays so a theme change can draw it again.
function show(el, result) {
  for (const old of el.querySelectorAll(".mermaid-svg, .mermaid-error"))
    old.remove();
  if (result.error) {
    el.dataset.state = "error";
    const note = document.createElement("p");
    note.className = "mermaid-error";
    note.textContent = "Diagram error: " + result.error;
    el.prepend(note);
    return;
  }
  el.dataset.state = "drawn";
  const figure = document.createElement("div");
  figure.className = "mermaid-svg";
  // Mermaid's strict security level sanitizes the SVG it returns.
  figure.innerHTML = result.svg.replaceAll(result.id, freshID());
  el.prepend(figure);
}

// renderDiagrams draws every placeholder under root that is not drawn yet.
// It resolves once all of them are drawn or have failed; drawings already in
// the cache are shown synchronously.
export function renderDiagrams(root) {
  const theme = dark.matches ? "dark" : "default",
    pending = [];
  for (const el of root.querySelectorAll(
    ".mermaid-diagram:not([data-state])",
  )) {
    const source = el.querySelector(".mermaid-source")?.textContent ?? "",
      key = theme + "\n" + source,
      hit = cached(key);
    if (hit) {
      show(el, hit);
      continue;
    }
    el.dataset.state = "pending";
    pending.push(
      draw(source, theme).then(
        (result) => {
          remember(key, result);
          show(el, result);
        },
        () => {
          // Mermaid could not load: the source stays as a code block, and
          // the next render tries again.
          delete el.dataset.state;
        },
      ),
    );
  }
  return Promise.all(pending);
}

// redrawDiagrams draws every placeholder under root again, as after a
// theme change.
export function redrawDiagrams(root) {
  for (const el of root.querySelectorAll(".mermaid-diagram[data-state]"))
    delete el.dataset.state;
  return renderDiagrams(root);
}

// onThemeChange calls f when the color scheme changes.
export function onThemeChange(f) {
  dark.addEventListener("change", f);
}
