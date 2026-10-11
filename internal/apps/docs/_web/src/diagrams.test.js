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
      'import { createDiagramRuntime } from "./diagram-runtime.js";',
      "const { createDiagramRuntime } = globalThis.__diagramTest;",
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
