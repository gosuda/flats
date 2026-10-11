import { test } from "node:test";
import assert from "node:assert/strict";
import { uniqueSVG, diagramTools } from "./diagram-tools.js";

class Node {
  constructor(tag = "div", attributes = {}) {
    this.tagName = tag;
    this.attrs = attributes;
    this.children = [];
    this.dataset = {};
    this.events = {};
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
  addEventListener(type, handler) {
    this.events[type] = handler;
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
  const body = new Node("body");
  const create = (tag) => {
    const node = new Node(tag);
    node.clientWidth = 800;
    node.clientHeight = 600;
    node.getBoundingClientRect = () => ({ width: 400, height: 300 });
    node.cloneNode = () => create(tag);
    node.showModal = () => {};
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
  }
});
