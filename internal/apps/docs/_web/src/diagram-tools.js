import { pngBlob, saveBlob } from "./diagram-export.js";
import { createWheelPan } from "../../../../design/web/wheel.js";
let cloneID = 0;
// Marker, title and description IDs must also be unique across cached copies.
export function uniqueSVG(container) {
  const ids = new Map();
  for (const node of container.querySelectorAll("[id]"))
    ids.set(node.id, `${node.id}-copy-${++cloneID}`);
  for (const node of container.querySelectorAll("*")) {
    for (const attr of [...node.attributes]) {
      let value = attr.value;
      if (attr.name === "id") value = ids.get(value);
      else if (["aria-labelledby", "aria-describedby"].includes(attr.name))
        value = value
          .split(" ")
          .map((id) => ids.get(id) || id)
          .join(" ");
      else {
        for (const [id, replacement] of ids) {
          value = value.replaceAll(`url(#${id})`, `url(#${replacement})`);
          if (
            (attr.name === "href" || attr.name === "xlink:href") &&
            value === `#${id}`
          )
            value = `#${replacement}`;
        }
      }
      if (value !== attr.value) node.setAttribute(attr.name, value);
    }
    if (node.tagName.toLowerCase() === "style") {
      node.textContent = node.textContent.replace(/#([\w-]+)/g, (whole, id) =>
        ids.has(id) ? `#${ids.get(id)}` : whole,
      );
    }
  }
}
function actionStatus(el, message, error = false, clear = true) {
  let note = el.querySelector(".mermaid-export-status");
  if (!note) {
    note = document.createElement("p");
    note.setAttribute("role", "status");
    el.append(note);
  }
  clearTimeout(note.clearTimer);
  note.className =
    "mermaid-export-status mermaid-status" + (error ? " mermaid-error" : "");
  note.textContent = message;
  if (clear)
    note.clearTimer = setTimeout(() => {
      note.textContent = "";
    }, 10000);
}
function button(label, action) {
  const el = document.createElement("button");
  el.type = "button";
  el.textContent = label;
  el.addEventListener("click", action);
  return el;
}
function expand(el, trigger) {
  // Pointer toolbar actions retain contentDOM focus. Remember it before the
  // modal blurs the editor so closing resumes the existing CM selection.
  const active = document.activeElement;
  const priorEditor = active?.matches('.cm-content[contenteditable="true"]')
    ? active
    : null;
  const sourceText = el.querySelector(".mermaid-source")?.textContent;
  const peers = [...document.querySelectorAll(".mermaid-diagram")].filter(
    (node) => node.querySelector(".mermaid-source")?.textContent === sourceText,
  );
  const occurrence = peers.indexOf(el);
  const sourceSVG = el.querySelector(".mermaid-svg svg");
  if (!sourceSVG) return;
  const dialog = document.createElement("dialog");
  dialog.className = "mermaid-dialog";
  dialog.setAttribute("aria-label", "Expanded diagram");
  const bar = document.createElement("div");
  bar.className = "mermaid-tools";
  const viewport = document.createElement("div");
  viewport.className = "mermaid-viewport";
  viewport.tabIndex = 0;
  viewport.setAttribute(
    "aria-label",
    "Diagram. Use arrow keys to pan, plus and minus to zoom, zero to fit.",
  );
  const canvas = document.createElement("div");
  canvas.className = "mermaid-canvas";
  canvas.append(sourceSVG.cloneNode(true));
  uniqueSVG(canvas);
  const svg = canvas.querySelector("svg");
  const box = svg.viewBox?.baseVal;
  const width = box?.width || sourceSVG.getBoundingClientRect().width || 800;
  const height = box?.height || sourceSVG.getBoundingClientRect().height || 600;
  svg.style.width = width + "px";
  svg.style.height = height + "px";
  svg.style.maxWidth = "none";
  let scale = 1,
    x = 0,
    y = 0;
  const label = document.createElement("span");
  label.className = "mermaid-zoom";
  const paint = () => {
    canvas.style.transform = `translate(${x}px, ${y}px) scale(${scale})`;
    label.textContent = `${Math.round(scale * 100)}%`;
  };
  const zoom = (factor) => {
    const next = Math.min(8, Math.max(0.05, scale * factor));
    const cx = viewport.clientWidth / 2,
      cy = viewport.clientHeight / 2;
    x = cx - ((cx - x) * next) / scale;
    y = cy - ((cy - y) * next) / scale;
    scale = next;
    paint();
  };
  const fit = () => {
    scale = Math.min(
      1,
      Math.max(
        0.05,
        Math.min(
          (viewport.clientWidth - 32) / width,
          (viewport.clientHeight - 32) / height,
        ),
      ),
    );
    x = (viewport.clientWidth - width * scale) / 2;
    y = (viewport.clientHeight - height * scale) / 2;
    paint();
  };
  bar.append(
    button("Zoom out", () => zoom(1 / 1.25)),
    label,
    button("Zoom in", () => zoom(1.25)),
    button("Fit", fit),
    button("100%", () => {
      zoom(1 / scale);
    }),
    button("Close", () => dialog.close()),
  );
  const points = new Map();
  viewport.addEventListener("pointerdown", (event) => {
    points.set(event.pointerId, { x: event.clientX, y: event.clientY });
    viewport.setPointerCapture(event.pointerId);
    viewport.focus();
  });
  viewport.addEventListener("pointermove", (event) => {
    const before = points.get(event.pointerId);
    if (!before) return;
    const oldPoints = [...points.values()];
    points.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (points.size === 2) {
      const nextPoints = [...points.values()];
      const distance = (p) => Math.hypot(p[0].x - p[1].x, p[0].y - p[1].y);
      if (distance(oldPoints) > 0)
        zoom(distance(nextPoints) / distance(oldPoints));
    } else {
      x += event.clientX - before.x;
      y += event.clientY - before.y;
      paint();
    }
  });
  for (const type of ["pointerup", "pointercancel", "lostpointercapture"])
    viewport.addEventListener(type, (event) => points.delete(event.pointerId));
  viewport.addEventListener("keydown", (event) => {
    if (event.key === "+" || event.key === "=") zoom(1.25);
    else if (event.key === "-") zoom(1 / 1.25);
    else if (event.key === "0") fit();
    else if (event.key.startsWith("Arrow")) {
      if (event.key === "ArrowLeft") x += 40;
      if (event.key === "ArrowRight") x -= 40;
      if (event.key === "ArrowUp") y += 40;
      if (event.key === "ArrowDown") y -= 40;
      paint();
    } else return;
    event.preventDefault();
  });
  const wheel = createWheelPan();
  viewport.addEventListener(
    "wheel",
    (event) => {
      event.preventDefault();
      if (event.ctrlKey || event.metaKey) {
        wheel.reset();
        zoom(event.deltaY < 0 ? 1.1 : 1 / 1.1);
      } else {
        const pan = wheel.pan(event);
        x -= pan.x;
        y -= pan.y;
        paint();
      }
    },
    { passive: false },
  );
  // Releasing Shift or leaving the window ends a Shift gesture, as in the
  // design canvas. Closing the dialog removes both listeners.
  const gesture = new AbortController();
  window.addEventListener(
    "keyup",
    (event) => {
      if (event.key === "Shift") wheel.reset();
    },
    { signal: gesture.signal },
  );
  window.addEventListener("blur", wheel.reset, { signal: gesture.signal });
  viewport.append(canvas);
  dialog.append(bar, viewport);
  document.body.append(dialog);
  dialog.addEventListener("close", () => {
    gesture.abort();
    dialog.remove();
    if (priorEditor?.isConnected) priorEditor.focus({ preventScroll: true });
    else if (trigger.isConnected) trigger.focus();
    else {
      const replacements = [
        ...document.querySelectorAll(".mermaid-diagram"),
      ].filter(
        (node) =>
          node.querySelector(".mermaid-source")?.textContent === sourceText,
      );
      const replacement = replacements[occurrence]?.querySelector(
        '[data-diagram-action="expand"]',
      );
      const fallback =
        replacement ||
        document.querySelector('.cm-content[contenteditable="true"]') ||
        document.querySelector("#content");
      if (fallback) {
        const hadTabIndex = fallback.hasAttribute("tabindex");
        if (!hadTabIndex) fallback.setAttribute("tabindex", "-1");
        fallback.focus();
        if (!hadTabIndex)
          fallback.addEventListener(
            "blur",
            () => fallback.removeAttribute("tabindex"),
            { once: true },
          );
      }
    }
  });
  dialog.showModal();
  fit();
  viewport.focus();
}
// diagramTools adds a diagram's toolbar once and updates what it offers.
// retry draws the diagram: after a load failure, or as Show diagram while
// it waits for the reader to ask (diagrams.js).
export function diagramTools(el, retry) {
  let bar = el.querySelector(".mermaid-tools");
  if (!bar) {
    bar = document.createElement("div");
    bar.className = "mermaid-tools";
    for (const type of ["pointerdown", "mousedown", "click"])
      bar.addEventListener(type, (event) => event.stopPropagation());
    const source = button("Source", () => {
      const visible = el.dataset.sourceVisible !== "true";
      el.dataset.sourceVisible = String(visible);
      source.textContent = visible ? "Hide source" : "Source";
    });
    const copy = button("Copy source", async () => {
      try {
        const text = el.querySelector(".mermaid-source").textContent;
        if (navigator.clipboard?.writeText)
          await navigator.clipboard.writeText(text);
        else {
          const field = document.createElement("textarea");
          field.value = text;
          field.className = "mermaid-copy-buffer";
          document.body.append(field);
          field.select();
          let copied;
          try {
            copied = document.execCommand("copy");
          } finally {
            field.remove();
            copy.focus();
          }
          if (!copied) throw Error("Clipboard unavailable");
        }
        copy.textContent = "Copied";
      } catch {
        copy.textContent = "Copy unavailable";
        if (el.dataset.editable === "true")
          actionStatus(
            el,
            "Copy unavailable. Use Edit source to select and copy the diagram text.",
            true,
          );
        if (el.dataset.editable !== "true") {
          el.dataset.sourceVisible = "true";
          source.textContent = "Hide source";
        }
      }
      setTimeout(() => {
        copy.textContent = "Copy source";
      }, 2000);
    });
    const download = button("Save SVG", () => {
      const svg = el.querySelector(".mermaid-svg svg");
      if (!svg) return;
      saveBlob(
        new Blob([new XMLSerializer().serializeToString(svg)], {
          type: "image/svg+xml;charset=utf-8",
        }),
        "diagram.svg",
      );
    });
    download.dataset.diagramAction = "svg";
    const png = button("Save PNG", async () => {
      const svg = el.querySelector(".mermaid-svg svg");
      if (!svg || png.dataset.busy === "true") return;
      png.dataset.busy = "true";
      png.setAttribute("aria-disabled", "true");
      png.textContent = "Saving PNG…";
      actionStatus(el, "Preparing PNG…", false, false);
      try {
        saveBlob(await pngBlob(svg), "diagram.png");
        actionStatus(el, "PNG download started.");
      } catch (error) {
        actionStatus(
          el,
          "PNG export failed: " + (error?.message || "Try Save SVG instead."),
          true,
        );
      } finally {
        delete png.dataset.busy;
        png.setAttribute("aria-disabled", "false");
        png.disabled = !el.querySelector(".mermaid-svg svg");
        png.textContent = "Save PNG";
      }
    });
    png.dataset.diagramAction = "png";
    const open = button("Expand", () => expand(el, open));
    open.dataset.diagramAction = "expand";
    const retryButton = button("Retry", () => retry());
    retryButton.className = "mermaid-retry";
    const show = button("Show diagram", () => retry());
    show.className = "mermaid-show";
    bar.append(show);
    if (el.dataset.editable !== "true") bar.append(source);
    bar.append(copy, download, png, open, retryButton);
    el.prepend(bar);
  }
  for (const control of bar.querySelectorAll("[data-diagram-action]"))
    control.disabled = !el.querySelector(".mermaid-svg svg");
  bar.querySelector(".mermaid-retry").hidden = el.dataset.error !== "load";
  bar.querySelector(".mermaid-show").hidden = el.dataset.state !== "request";
}
