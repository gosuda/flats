import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

test("late theme results and detached placeholders never replace the current diagram", async () => {
  const jobs = [],
    predicates = [];
  const media = { matches: false, addEventListener() {} };
  const original = {
    matchMedia: globalThis.matchMedia,
    document: globalThis.document,
    harness: globalThis.__diagramTest,
  };
  function element() {
    const el = {
      dataset: {},
      isConnected: true,
      source: { textContent: "graph TD; A-->B" },
      figure: null,
      note: null,
      dispatchEvent() {},
      setAttribute() {},
      removeAttribute() {},
      append(note) {
        this.note = note;
      },
      querySelector(selector) {
        if (selector === ".mermaid-source") return this.source;
        if (selector === ".mermaid-svg") return this.figure;
        if (selector === ".mermaid-status") return this.note;
        if (selector === ".mermaid-tools")
          return {
            after: (figure) => {
              this.figure = figure;
              figure.remove = () => {
                this.figure = null;
              };
            },
          };
      },
    };
    return el;
  }
  globalThis.matchMedia = () => media;
  globalThis.document = {
    createElement: () => ({ setAttribute() {}, textContent: "" }),
  };
  globalThis.__diagramTest = {
    tooLarge: () => "",
    createDiagramRuntime: () => ({
      request: (source, theme, current) => {
        predicates.push(current);
        return new Promise((resolve) => jobs.push(resolve));
      },
    }),
  };
  try {
    let code = await readFile(
      new URL("./diagrams.js", import.meta.url),
      "utf8",
    );
    code = code.replace(
      'import { createDiagramRuntime, tooLarge } from "./diagram-runtime.js";',
      "const { createDiagramRuntime, tooLarge } = globalThis.__diagramTest;",
    );
    code = code.replace(
      'import { diagramTools, uniqueSVG } from "./diagram-tools.js";',
      "const diagramTools = () => {}, uniqueSVG = () => {};",
    );
    const { renderDiagrams, redrawDiagrams, destroyDiagram } = await import(
      `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
    );
    const el = element(),
      root = { querySelectorAll: () => [el] };
    const initial = renderDiagrams(root);
    media.matches = true;
    const dark = redrawDiagrams(root);
    media.matches = false;
    const light = redrawDiagrams(root);
    jobs[2]({ svg: "latest light" });
    await light;
    jobs[1]({ svg: "stale dark" });
    await dark;
    jobs[0]({ svg: "initial light" });
    await initial;
    assert.equal(el.figure.innerHTML, "latest light");
    const detached = redrawDiagrams(root);
    el.isConnected = false;
    jobs[3]({ svg: "detached result" });
    await detached;
    assert.equal(el.figure.innerHTML, "latest light");
    const fresh = element();
    fresh.isConnected = false;
    const freshRoot = { querySelectorAll: () => [fresh] };
    const mounting = renderDiagrams(freshRoot);
    assert.equal(
      predicates[4](),
      true,
      "new detached widget can render before mounting",
    );
    fresh.isConnected = true;
    jobs[4]({ svg: "mounted result" });
    await mounting;
    assert.equal(fresh.figure.innerHTML, "mounted result");
    const discarded = element();
    discarded.isConnected = false;
    const discardRender = renderDiagrams({
      querySelectorAll: () => [discarded],
    });
    destroyDiagram(discarded);
    assert.equal(
      predicates[5](),
      false,
      "destroy cancels even before first mount",
    );
    jobs[5]({ svg: "discarded result" });
    await discardRender;
    assert.equal(discarded.figure, null);
  } finally {
    globalThis.matchMedia = original.matchMedia;
    globalThis.document = original.document;
    globalThis.__diagramTest = original.harness;
  }
});

test("on request, diagrams draw only when the reader asks, within the caps", async () => {
  const { tooLarge } = await import("./diagram-runtime.js");
  const requests = [],
    tools = new Map();
  const original = {
    matchMedia: globalThis.matchMedia,
    document: globalThis.document,
    harness: globalThis.__diagramTest,
  };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {} });
  globalThis.document = {
    createElement: () => ({ setAttribute() {}, textContent: "" }),
  };
  globalThis.__diagramTest = {
    tooLarge,
    createDiagramRuntime: () => ({
      request: (source, theme, current, capped) => {
        requests.push({ source, capped });
        return Promise.resolve({ svg: "<svg></svg>" });
      },
    }),
  };
  function element(source) {
    const el = {
      dataset: {},
      isConnected: true,
      source: { textContent: source },
      figure: null,
      note: null,
      dispatchEvent() {},
      setAttribute() {},
      removeAttribute() {},
      append(note) {
        this.note = note;
      },
      querySelector(selector) {
        if (selector === ".mermaid-source") return this.source;
        if (selector === ".mermaid-svg") return this.figure;
        if (selector === ".mermaid-status") return this.note;
        if (selector === ".mermaid-tools")
          return {
            after: (figure) => {
              this.figure = figure;
              figure.remove = () => (this.figure = null);
            },
          };
      },
    };
    return el;
  }
  const rootOf = (...els) => ({
    querySelectorAll: (selector) =>
      selector.includes(":not([data-state])")
        ? els.filter((e) => !e.dataset.state)
        : els,
  });
  try {
    let code = await readFile(
      new URL("./diagrams.js", import.meta.url),
      "utf8",
    );
    code = code
      .replace(
        'import { createDiagramRuntime, tooLarge } from "./diagram-runtime.js";',
        "const { createDiagramRuntime, tooLarge } = globalThis.__diagramTest;",
      )
      .replace(
        'import { diagramTools, uniqueSVG } from "./diagram-tools.js";',
        "const diagramTools = (el, show) => globalThis.__diagramTest.tools.set(el, show), uniqueSVG = () => {};",
      );
    globalThis.__diagramTest.tools = tools;
    // A separate module instance: the mode is per page.
    const { renderDiagrams, redrawDiagrams, updateDiagram, drawOnRequest } =
      await import(
        `data:text/javascript;base64,${Buffer.from(code + "\n// on request").toString("base64")}`
      );
    const before = element("graph TD; A-->B");
    await renderDiagrams(rootOf(before));
    assert.equal(before.dataset.state, "drawn");
    assert.deepEqual(requests, [{ source: "graph TD; A-->B", capped: false }]);

    drawOnRequest();
    const guest = element("graph TD; X-->Y");
    await renderDiagrams(rootOf(before, guest));
    assert.equal(guest.dataset.state, "request");
    assert.equal(requests.length, 1, "nothing renders until asked");
    assert.equal(before.dataset.state, "drawn", "drawn diagrams stay");

    // Show diagram.
    await tools.get(guest)();
    assert.equal(guest.dataset.state, "drawn");
    assert.deepEqual(requests[1], { source: "graph TD; X-->Y", capped: true });

    // A changed source needs another click.
    await updateDiagram(guest, "graph TD; X-->Z");
    assert.equal(guest.dataset.state, "request");
    assert.equal(guest.figure, null);
    assert.equal(requests.length, 2);

    // A theme change draws again only the diagrams the reader asked for.
    const unasked = element("graph TD; P-->Q");
    await renderDiagrams(rootOf(unasked));
    await redrawDiagrams(rootOf(before, guest, unasked));
    assert.deepEqual(
      requests.slice(2).map((r) => r.source),
      ["graph TD; A-->B"],
    );
    assert.equal(unasked.dataset.state, "request");

    // Over the caps the source stays, with a note.
    const big = element("graph TD;" + " A-->B;".repeat(3000));
    await renderDiagrams(rootOf(big));
    await tools.get(big)();
    assert.equal(big.dataset.state, "error");
    assert.equal(big.dataset.error, "size");
    assert.match(big.note.textContent, /Too large to draw/);
    assert.equal(requests.length, 3);
  } finally {
    globalThis.matchMedia = original.matchMedia;
    globalThis.document = original.document;
    globalThis.__diagramTest = original.harness;
  }
});
