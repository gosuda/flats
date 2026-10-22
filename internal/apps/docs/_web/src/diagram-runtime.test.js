import { test } from "node:test";
import assert from "node:assert/strict";
import {
  DIAGRAM_CAPS,
  createDiagramRuntime,
  tooLarge,
} from "./diagram-runtime.js";
function harness(options) {
  const calls = [],
    themes = [];
  const runtime = createDiagramRuntime(
    async () => ({
      initialize: (config) => themes.push(config),
      render: async (id, source) => {
        calls.push(source);
        return { svg: `<svg id="${id}">${source}</svg>` };
      },
    }),
    () => "id",
    options,
  );
  return { runtime, calls, themes };
}
test("identical in-flight requests share one render and strict configuration", async () => {
  const { runtime, calls, themes } = harness();
  const [a, b] = await Promise.all([
    runtime.request("A", "dark"),
    runtime.request("A", "dark"),
  ]);
  assert.equal(a, b);
  assert.deepEqual(calls, ["A"]);
  assert.equal(themes[0].securityLevel, "strict");
});
test("obsolete queued work is skipped, but a shared live consumer keeps work", async () => {
  const { runtime, calls } = harness();
  await Promise.all([
    runtime.request("skip", "dark", () => false),
    runtime.request("keep", "dark", () => false),
    runtime.request("keep", "dark", () => true),
  ]);
  assert.deepEqual(calls, ["keep"]);
});
test("theme is part of identity and configuration follows serialized order", async () => {
  const { runtime, themes } = harness();
  await Promise.all([
    runtime.request("A", "default"),
    runtime.request("A", "dark"),
    runtime.request("B", "default"),
  ]);
  assert.deepEqual(
    themes.map((t) => t.theme),
    ["default", "dark", "default"],
  );
});
test("entry and byte limits evict retained results", async () => {
  const { runtime, calls } = harness({ maxEntries: 1, maxBytes: 100 });
  await runtime.request("A", "dark");
  await runtime.request("B", "dark");
  await runtime.request("A", "dark");
  assert.deepEqual(calls, ["A", "B", "A"]);
  const large = "x".repeat(100);
  await runtime.request(large, "dark");
  await runtime.request(large, "dark");
  assert.equal(calls.filter((s) => s === large).length, 2);
});
test("load failure releases queue and can be retried", async () => {
  let attempts = 0;
  const runtime = createDiagramRuntime(
    async () => {
      if (++attempts === 1) throw Error("offline");
      return { initialize() {}, render: async () => ({ svg: "ok" }) };
    },
    () => "id",
  );
  await assert.rejects(runtime.request("A", "dark"), /offline/);
  assert.equal((await runtime.request("A", "dark")).svg, "ok");
});

test("a capped request runs with the size caps and is cached apart", async () => {
  const { runtime, calls, themes } = harness();
  await runtime.request("A", "dark");
  await runtime.request("A", "dark", () => true, true);
  await runtime.request("A", "dark", () => true, true);
  assert.deepEqual(calls, ["A", "A"]);
  assert.equal(themes[0].maxTextSize, undefined);
  assert.equal(themes[1].maxTextSize, 20000);
  assert.equal(themes[1].maxEdges, 500);
  assert.equal(themes[1].securityLevel, "strict");
  await runtime.request("B", "dark");
  assert.equal(themes[2].maxTextSize, undefined);
});
test("diagrams over the caps are named too large", () => {
  assert.equal(tooLarge("x".repeat(DIAGRAM_CAPS.maxTextSize)), "");
  assert.match(
    tooLarge("x".repeat(DIAGRAM_CAPS.maxTextSize + 1)),
    /over 20,000 characters/,
  );
  assert.match(
    tooLarge(
      "graph TD",
      "Edge limit exceeded. 501 edges found, but the limit is 500.",
    ),
    /over 500 connections/,
  );
  assert.equal(tooLarge("graph TD", "Parse error on line 1"), "");
});
