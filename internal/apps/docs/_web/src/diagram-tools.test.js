import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { uniqueSVG, diagramTools } from "./diagram-tools.js";

class Node {
  constructor(tag = "div", attributes = {}) {
    this.tagName = tag;
    this.attrs = attributes;
    this.children = [];
    this.dataset = {};
    this.events = {};
    this.listenerOptions = {};
    this.style = {};
    this.textContent = "";
    this.className = "";
  }
  get id() {
    return this.attrs.id;
  }
  get attributes() {
    return Object.entries(this.attrs).map(([name, value]) => ({ name, value }));
  }
  setAttribute(name, value) {
    this.attrs[name] = value;
  }
  getAttribute(name) {
    return this.attrs[name];
  }
  append(...nodes) {
    for (const n of nodes) {
      this.children.push(n);
      n.parent = this;
    }
  }
  prepend(n) {
    this.children.unshift(n);
    n.parent = this;
  }
  remove() {
    if (this.parent)
      this.parent.children = this.parent.children.filter((n) => n !== this);
  }
  addEventListener(type, handler, options) {
    this.events[type] = handler;
    this.listenerOptions[type] = options;
  }
  click() {
    return this.events.click?.({});
  }
  matches(selector) {
    return (
      selector === '.cm-content[contenteditable="true"]' &&
      this.className.split(" ").includes("cm-content") &&
      this.attrs.contenteditable === "true"
    );
  }
  focus() {
    document.activeElement = this;
  }
  select() {
    document.selection = this.value;
  }
  querySelectorAll(selector) {
    const all = this.children.flatMap((n) => [n, ...n.querySelectorAll("*")]);
    if (selector === "*") return all;
    if (selector === "[id]") return all.filter((n) => n.id);
    if (selector === "[data-diagram-action]")
      return all.filter((n) => n.dataset.diagramAction);
    if (selector === ".mermaid-svg svg")
      return this.querySelector(".mermaid-svg")?.querySelectorAll("svg") || [];
    return all.filter((n) =>
      selector.startsWith(".")
        ? n.className.split(" ").includes(selector.slice(1))
        : n.tagName === selector,
    );
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
}
// A window that keeps its listeners, skips those whose abort signal fired and
// can fire an event at the rest.
function fakeWindow() {
  const listeners = [];
  const live = (type) =>
    listeners.filter((l) => l.type === type && !l.signal?.aborted && !l.spent);
  return {
    addEventListener(type, handler, options = {}) {
      listeners.push({ type, handler, options, signal: options.signal });
    },
    fire(type, event = {}) {
      for (const l of live(type)) {
        l.handler(event);
        if (l.options.once) l.spent = true;
      }
    },
    count: (type) => live(type).length,
    // Element blur events pass through window in the capture phase, so a
    // capture listener would also hear focus moves inside the dialog.
    captures: (type) =>
      live(type).filter((l) => l.options === true || l.options?.capture).length,
  };
}
test("SVG copies rewrite markers, use links, styles and accessible labels without changing label text", () => {
  const container = new Node();
  const svg = new Node("svg", {
    id: "diagram",
    "aria-labelledby": "title desc",
  });
  const style = new Node("style");
  style.textContent = "#diagram .node {marker-end:url(#arrow);fill:#fff}";
  const title = new Node("title", { id: "title" });
  title.textContent = "diagram title arrow";
  const desc = new Node("desc", { id: "desc" });
  const arrow = new Node("marker", { id: "arrow" });
  const path = new Node("path", { "marker-end": "url(#arrow)" });
  const use = new Node("use", { href: "#arrow", "xlink:href": "#arrow" });
  svg.append(style, title, desc, arrow, path, use);
  container.append(svg);
  uniqueSVG(container);
  assert.equal(svg.getAttribute("aria-labelledby"), `${title.id} ${desc.id}`);
  assert.equal(path.getAttribute("marker-end"), `url(#${arrow.id})`);
  assert.equal(use.getAttribute("href"), `#${arrow.id}`);
  assert.equal(use.getAttribute("xlink:href"), `#${arrow.id}`);
  assert.ok(style.textContent.includes(`#${svg.id}`));
  assert.ok(style.textContent.includes(`url(#${arrow.id})`));
  assert.ok(style.textContent.includes("fill:#fff"));
  assert.equal(title.textContent, "diagram title arrow");
  const old = arrow.id;
  uniqueSVG(container);
  assert.notEqual(arrow.id, old);
  assert.equal(path.getAttribute("marker-end"), `url(#${arrow.id})`);
});
test("HTTP copy fallback reports real success/failure and SVG save uses serialized markup", async () => {
  const saved = Object.fromEntries(
    ["document", "navigator", "XMLSerializer", "setTimeout"].map((key) => [
      key,
      Object.getOwnPropertyDescriptor(globalThis, key),
    ]),
  );
  const oldURL = URL.createObjectURL,
    oldRevoke = URL.revokeObjectURL;
  const actionTimers = [];
  let copied = true,
    blob,
    serialized,
    clicked = false;
  const body = new Node("body");
  const document = {
    body,
    createElement(tag) {
      const node = new Node(tag);
      if (tag === "a")
        node.click = () => {
          clicked =
            body.children.includes(node) && node.download === "diagram.svg";
        };
      return node;
    },
    execCommand: () => copied,
  };
  for (const [key, value] of Object.entries({
    document,
    navigator: {},
    XMLSerializer: class {
      serializeToString(svg) {
        serialized = svg;
        return '<svg xmlns="http://www.w3.org/2000/svg"><title>Diagram</title></svg>';
      }
    },
    setTimeout: (callback) => actionTimers.push(callback),
  }))
    Object.defineProperty(globalThis, key, { value, configurable: true });
  URL.createObjectURL = (value) => {
    blob = value;
    return "blob:diagram";
  };
  URL.revokeObjectURL = () => {};
  try {
    const el = new Node();
    el.dataset.error = "syntax";
    const source = new Node("pre");
    source.className = "mermaid-source";
    source.textContent = "graph TD; A-->B";
    const figure = new Node();
    figure.className = "mermaid-svg";
    const svg = new Node("svg");
    figure.append(svg);
    el.append(source, figure);
    diagramTools(el, () => {});
    const controls = el.querySelectorAll("button");
    const toggle = controls.find((n) => n.textContent === "Source");
    toggle.click();
    assert.equal(toggle.textContent, "Hide source");
    assert.equal(toggle.getAttribute("aria-pressed"), undefined);
    assert.equal(el.dataset.sourceVisible, "true");
    toggle.click();
    assert.equal(toggle.textContent, "Source");
    assert.equal(el.dataset.sourceVisible, "false");
    const editable = new Node();
    editable.dataset.editable = "true";
    const editSource = new Node("pre");
    editSource.className = "mermaid-source";
    editSource.textContent = "graph TD";
    editable.append(editSource);
    diagramTools(editable, () => {});
    assert.equal(
      editable
        .querySelectorAll("button")
        .some((n) => n.textContent === "Source"),
      false,
    );
    const copy = controls.find((n) => n.textContent === "Copy source");
    await copy.click();
    assert.equal(document.selection, source.textContent);
    assert.equal(copy.textContent, "Copied");
    assert.equal(body.children.length, 0);
    copied = false;
    await copy.click();
    assert.equal(copy.textContent, "Copy unavailable");
    assert.equal(el.dataset.sourceVisible, "true");
    assert.equal(el.querySelector(".mermaid-retry").hidden, true);
    el.dataset.error = "load";
    diagramTools(el, () => {});
    assert.equal(el.querySelector(".mermaid-retry").hidden, false);
    controls.find((n) => n.textContent === "Save SVG").click();
    assert.equal(serialized, svg);
    assert.equal(clicked, true);
    assert.equal(blob.type, "image/svg+xml;charset=utf-8");
    assert.match(await blob.text(), /^<svg xmlns=/);
    assert.equal(body.children.length, 0);
    const editableCopy = editable
      .querySelectorAll("button")
      .find((n) => n.textContent === "Copy source");
    await editableCopy.click();
    assert.equal(editable.dataset.sourceVisible, undefined);
    assert.match(
      editable.querySelector(".mermaid-export-status").textContent,
      /Use Edit source/,
    );
    const png = controls.find((n) => n.textContent === "Save PNG");
    let fontsReady;
    document.fonts = {
      ready: new Promise((resolve) => {
        fontsReady = resolve;
      }),
    };
    svg.isConnected = false;
    png.focus();
    const exporting = png.click();
    assert.equal(png.disabled, false);
    assert.equal(png.getAttribute("aria-disabled"), "true");
    assert.equal(document.activeElement, png);
    diagramTools(el, () => {});
    assert.equal(
      png.disabled,
      false,
      "refresh does not disable busy focused button",
    );
    await png.click();
    assert.equal(png.dataset.busy, "true");
    fontsReady();
    await exporting;
    assert.match(
      el.querySelector(".mermaid-export-status").textContent,
      /diagram changed/,
    );
    assert.equal(png.textContent, "Save PNG");
    assert.equal(png.disabled, false);
    assert.equal(png.getAttribute("aria-disabled"), "false");
    assert.equal(png.dataset.busy, undefined);
    assert.equal(document.activeElement, png);
    actionTimers.at(-1)();
    assert.equal(el.querySelector(".mermaid-export-status").textContent, "");
  } finally {
    for (const [key, descriptor] of Object.entries(saved)) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete globalThis[key];
    }
    URL.createObjectURL = oldURL;
    URL.revokeObjectURL = oldRevoke;
  }
});

test("closing an expanded editing preview returns focus to the editor, with opener fallback", () => {
  const priorDocument = globalThis.document;
  const priorWindow = globalThis.window;
  globalThis.window = fakeWindow();
  const body = new Node("body");
  const create = (tag) => {
    const node = new Node(tag);
    node.clientWidth = 800;
    node.clientHeight = 600;
    node.getBoundingClientRect = () => ({ width: 400, height: 300 });
    node.cloneNode = () => create(tag);
    node.showModal = () => {};
    node.setPointerCapture = () => {};
    node.close = () => node.events.close();
    Object.defineProperty(node, "isConnected", {
      get: () => {
        let n = node;
        while (n.parent) n = n.parent;
        return n === body;
      },
    });
    return node;
  };
  const doc = {
    body,
    createElement: create,
    querySelectorAll: (s) => body.querySelectorAll(s),
    querySelector: (s) => body.querySelector(s),
  };
  globalThis.document = doc;
  try {
    const editor = create("div");
    editor.className = "cm-content";
    editor.setAttribute("contenteditable", "true");
    const diagram = create("div");
    diagram.className = "mermaid-diagram";
    const source = create("pre");
    source.className = "mermaid-source";
    source.textContent = "graph TD; A-->B";
    const figure = create("div");
    figure.className = "mermaid-svg";
    figure.append(create("svg"));
    diagram.append(source, figure);
    body.append(editor, diagram);
    doc.activeElement = editor;
    diagramTools(diagram, () => {});
    const open = diagram
      .querySelectorAll("button")
      .find((n) => n.textContent === "Expand");
    open.click();
    const dialog = body.querySelector("dialog");
    assert.ok(dialog, "missing viewBox does not prevent opening");
    dialog.close();
    assert.equal(doc.activeElement, editor);
    // A keyboard-focused toolbar button is inside contentDOM, but is not
    // contentDOM itself. Closing must return to that exact native control.
    diagram.remove();
    editor.append(diagram);
    open.closest = () => editor;
    doc.activeElement = open;
    open.click();
    body.querySelector("dialog").close();
    assert.equal(doc.activeElement, open, "keyboard opener retains focus");
    diagram.remove();
    body.append(diagram);
    doc.activeElement = editor;
    open.click();
    const second = body.querySelector("dialog");
    editor.remove();
    editor.parent = null;
    second.close();
    assert.equal(
      doc.activeElement,
      open,
      "removed editor falls back to attached opener",
    );
  } finally {
    globalThis.document = priorDocument;
    globalThis.window = priorWindow;
  }
});

// A fake document holding one diagram, for opening the expanded viewer.
function viewerFixture() {
  const body = new Node("body");
  const create = (tag) => {
    const node = new Node(tag);
    node.clientWidth = 800;
    node.clientHeight = 600;
    node.getBoundingClientRect = () => ({ width: 400, height: 300 });
    node.cloneNode = () => create(tag);
    node.showModal = () => {};
    node.setPointerCapture = () => {};
    node.close = () => node.events.close();
    Object.defineProperty(node, "isConnected", {
      get: () => {
        let n = node;
        while (n.parent) n = n.parent;
        return n === body;
      },
    });
    return node;
  };
  const doc = {
    body,
    createElement: create,
    querySelectorAll: (s) => body.querySelectorAll(s),
    querySelector: (s) => body.querySelector(s),
  };
  const diagram = create("div");
  diagram.className = "mermaid-diagram";
  const source = create("pre");
  source.className = "mermaid-source";
  source.textContent = "graph TD; A-->B";
  const figure = create("div");
  figure.className = "mermaid-svg";
  figure.append(create("svg"));
  diagram.append(source, figure);
  body.append(diagram);
  return { doc, body, diagram };
}

// Opens the expanded viewer of a fresh fake page. The fit view of the fake
// 400 by 300 diagram in the fake 800 by 600 viewport is
// translate(200px, 150px) scale(1).
function openViewer(tools) {
  const { doc, body, diagram } = viewerFixture();
  const win = fakeWindow();
  globalThis.document = doc;
  globalThis.window = win;
  tools(diagram, () => {});
  diagram
    .querySelectorAll("button")
    .find((n) => n.textContent === "Expand")
    .click();
  const dialog = body.querySelector("dialog");
  return {
    win,
    dialog,
    viewport: dialog.querySelector(".mermaid-viewport"),
    canvas: dialog.querySelector(".mermaid-canvas"),
  };
}

async function withViewerGlobals(run) {
  const prior = { document: globalThis.document, window: globalThis.window };
  try {
    await run();
  } finally {
    Object.assign(globalThis, prior);
  }
}

function wheel(viewport, init) {
  let prevented = false;
  viewport.events.wheel({
    deltaX: 0,
    deltaY: 0,
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    timeStamp: 0,
    ...init,
    preventDefault: () => {
      prevented = true;
    },
  });
  assert.equal(prevented, true, "the viewer cancels the wheel event");
}

const transform = (canvas) => {
  const m =
    /^translate\((-?[\d.]+)px, (-?[\d.]+)px\) scale\((-?[\d.]+)\)$/.exec(
      canvas.style.transform,
    );
  assert.ok(m, "unexpected transform " + canvas.style.transform);
  return { x: +m[1], y: +m[2], scale: +m[3] };
};

test("in the expanded diagram, Shift + vertical wheel pans sideways and other wheel input pans as reported", () =>
  withViewerGlobals(() => {
    const { viewport, canvas } = openViewer(diagramTools);
    assert.equal(canvas.style.transform, "translate(200px, 150px) scale(1)");
    assert.equal(
      viewport.listenerOptions.wheel?.passive,
      false,
      "a passive wheel listener cannot cancel the event",
    );
    wheel(viewport, { deltaY: 100 });
    assert.equal(canvas.style.transform, "translate(200px, 50px) scale(1)");
    wheel(viewport, { deltaX: 30 });
    assert.equal(canvas.style.transform, "translate(170px, 50px) scale(1)");
    wheel(viewport, { deltaX: -10, deltaY: 5 });
    assert.equal(canvas.style.transform, "translate(180px, 45px) scale(1)");
    // Trackpads report fractions; they are applied as reported, not rounded.
    wheel(viewport, { deltaX: 0.5, deltaY: 2.25 });
    assert.equal(
      canvas.style.transform,
      "translate(179.5px, 42.75px) scale(1)",
    );
    wheel(viewport, { deltaY: 100, shiftKey: true, timeStamp: 1000 });
    assert.equal(canvas.style.transform, "translate(79.5px, 42.75px) scale(1)");
    // A Shift event that is already horizontal pans as reported, in a new gesture.
    wheel(viewport, { deltaX: -30, shiftKey: true, timeStamp: 2000 });
    assert.equal(
      canvas.style.transform,
      "translate(109.5px, 42.75px) scale(1)",
    );
    wheel(viewport, { deltaY: 2.5, shiftKey: true, timeStamp: 3000 });
    assert.equal(canvas.style.transform, "translate(107px, 42.75px) scale(1)");
  }));

test("in the expanded diagram, a Shift gesture keeps its mapping until a pause, a plain wheel, Shift release or blur", () =>
  withViewerGlobals(() => {
    const { viewport, canvas, win } = openViewer(diagramTools);
    const at = () => canvas.style.transform;
    // A vertical start pans sideways, and a horizontal event 60 ms later still follows that mapping,
    // also after the auto-repeat of a held Shift key in between.
    wheel(viewport, { deltaY: 100, shiftKey: true, timeStamp: 1000 });
    win.fire("keydown", { key: "Shift", repeat: true });
    wheel(viewport, { deltaX: 100, shiftKey: true, timeStamp: 1060 });
    assert.equal(at(), "translate(100px, 50px) scale(1)");
    // More than 200 ms later a horizontal event starts a new gesture and pans as reported.
    wheel(viewport, { deltaX: 100, shiftKey: true, timeStamp: 1400 });
    assert.equal(at(), "translate(0px, 50px) scale(1)");
    // Releasing Shift ends the gesture.
    wheel(viewport, { deltaY: 100, shiftKey: true, timeStamp: 2000 });
    win.fire("keyup", { key: "Shift" });
    wheel(viewport, { deltaX: 100, shiftKey: true, timeStamp: 2060 });
    assert.equal(at(), "translate(-200px, 50px) scale(1)");
    // The window losing focus ends it too.
    wheel(viewport, { deltaY: 100, shiftKey: true, timeStamp: 3000 });
    win.fire("blur");
    wheel(viewport, { deltaX: 100, shiftKey: true, timeStamp: 3060 });
    assert.equal(at(), "translate(-400px, 50px) scale(1)");
    // So does a wheel event without Shift.
    wheel(viewport, { deltaY: 100, shiftKey: true, timeStamp: 4000 });
    wheel(viewport, { deltaY: 10, timeStamp: 4030 });
    wheel(viewport, { deltaX: 100, shiftKey: true, timeStamp: 4060 });
    assert.equal(at(), "translate(-600px, 40px) scale(1)");
  }));

test("in the expanded diagram, Ctrl or Cmd wheel zooms about the center, with or without Shift, and ends a Shift gesture", () =>
  withViewerGlobals(() => {
    const zoomed = [
      { deltaY: 100, ctrlKey: true },
      { deltaY: 100, ctrlKey: true, shiftKey: true },
      { deltaY: 100, metaKey: true },
    ].map((init) => {
      const { viewport, canvas } = openViewer(diagramTools);
      wheel(viewport, init);
      return transform(canvas);
    });
    assert.deepEqual(zoomed[1], zoomed[0]);
    assert.deepEqual(zoomed[2], zoomed[0]);
    assert.ok(Math.abs(zoomed[0].scale - 1 / 1.1) < 1e-9);
    assert.ok(Math.abs(zoomed[0].x - (400 - 200 / 1.1)) < 1e-9);
    assert.ok(Math.abs(zoomed[0].y - (300 - 150 / 1.1)) < 1e-9);
    const { viewport, canvas } = openViewer(diagramTools);
    wheel(viewport, { deltaY: -100, ctrlKey: true });
    assert.ok(Math.abs(transform(canvas).scale - 1.1) < 1e-9);
    // Shift + vertical starts a sideways gesture, Ctrl zooms, and the next Shift event
    // inside 200 ms starts a new gesture: a horizontal event pans horizontally.
    const fresh = openViewer(diagramTools);
    wheel(fresh.viewport, { deltaY: 100, shiftKey: true, timeStamp: 1000 });
    wheel(fresh.viewport, {
      deltaY: 100,
      ctrlKey: true,
      shiftKey: true,
      timeStamp: 1030,
    });
    const before = transform(fresh.canvas);
    wheel(fresh.viewport, { deltaX: 100, shiftKey: true, timeStamp: 1060 });
    const after = transform(fresh.canvas);
    assert.equal(after.x, before.x - 100);
    assert.equal(after.y, before.y);
  }));

test("the expanded diagram pans through one shared mapper per viewer and resets it on Ctrl/Cmd, Shift release and blur", async () => {
  const shared =
    'import { createWheelPan } from "../../../../design/web/wheel.js";';
  const exporter = 'import { pngBlob, saveBlob } from "./diagram-export.js";';
  let code = await readFile(
    new URL("./diagram-tools.js", import.meta.url),
    "utf8",
  );
  assert.ok(
    code.includes(shared),
    "diagram-tools.js must import createWheelPan from the design app's wheel.js",
  );
  assert.ok(code.includes(exporter), "diagram-export import not found");
  // A module loaded from a data URL cannot resolve relative imports, so both
  // are replaced. The shared mapper becomes a stand-in that logs its calls and
  // returns a distinctive pan; a viewer with a rule of its own ignores it.
  code = code
    .replace(
      shared,
      `const createWheelPan = () => {
        const log = globalThis.mapperLog, id = log.made++;
        return {
          pan: (e) => (log.calls.push(["pan", id, e.deltaY, e.shiftKey]), { x: 7, y: 9 }),
          reset: () => log.calls.push(["reset", id]),
        };
      };`,
    )
    .replace(exporter, "const pngBlob = () => {}, saveBlob = () => {};");
  const { diagramTools: tools } = await import(
    "data:text/javascript;base64," + Buffer.from(code).toString("base64")
  );
  await withViewerGlobals(() => {
    const log = (globalThis.mapperLog = { made: 0, calls: [] });
    const first = openViewer(tools);
    assert.equal(log.made, 1, "one mapper for the opened viewer");
    // Plain and Shift events both go through the mapper, and its result is used.
    wheel(first.viewport, { deltaY: 100 });
    wheel(first.viewport, { deltaY: 100, shiftKey: true });
    assert.equal(
      first.canvas.style.transform,
      "translate(186px, 132px) scale(1)",
    );
    // Zoom resets the mapper and does not pan through it.
    wheel(first.viewport, { deltaY: 100, ctrlKey: true });
    wheel(first.viewport, { deltaY: -100, metaKey: true });
    // Other keys, a Shift keydown, pointer drags and arrow keys leave the mapper alone.
    for (const key of ["a", "Control", "Meta", "Alt", "Escape"])
      first.win.fire("keyup", { key });
    first.win.fire("keydown", { key: "Shift" });
    first.viewport.events.pointerdown({ pointerId: 1, clientX: 5, clientY: 5 });
    first.viewport.events.keydown({ key: "ArrowRight", preventDefault() {} });
    // A Shift release and a blur reset it, and again the second time: the
    // listeners are not one-shot.
    for (let i = 0; i < 2; i++) {
      first.win.fire("keyup", { key: "Shift" });
      first.win.fire("blur");
    }
    assert.equal(first.win.captures("keyup"), 0);
    assert.equal(
      first.win.captures("blur"),
      0,
      "blur must not be a capture listener",
    );
    assert.deepEqual(log.calls, [
      ["pan", 0, 100, false],
      ["pan", 0, 100, true],
      ["reset", 0], // Ctrl wheel
      ["reset", 0], // Cmd wheel
      ["reset", 0], // Shift release
      ["reset", 0], // blur
      ["reset", 0], // second Shift release
      ["reset", 0], // second blur
    ]);
    // A second viewer gets a mapper of its own.
    const second = openViewer(tools);
    assert.equal(log.made, 2);
    wheel(second.viewport, { deltaY: 100 });
    assert.deepEqual(log.calls.at(-1), ["pan", 1, 100, false]);
    // Closing a viewer removes its window listeners and only those.
    assert.equal(first.win.count("keyup"), 1);
    assert.equal(first.win.count("blur"), 1);
    first.dialog.close();
    assert.equal(first.win.count("keyup"), 0);
    assert.equal(first.win.count("blur"), 0);
    first.win.fire("keyup", { key: "Shift" });
    first.win.fire("blur");
    assert.equal(log.calls.filter((c) => c[0] === "reset").length, 6);
    assert.equal(second.win.count("keyup"), 1);
    assert.equal(second.win.count("blur"), 1);
  });
});
