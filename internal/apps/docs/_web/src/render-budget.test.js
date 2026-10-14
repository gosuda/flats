import { test } from "node:test";
import assert from "node:assert/strict";
import { REMOTE_RENDER_GAP_MS, coalescedRefresh } from "./render-budget.js";

test("an edit after an idle gap renders on the next task", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++, 150);
  // A burst within one task (such as the initial sync) renders once, at once.
  for (let i = 0; i < 20; i++) refresh.request();
  assert.equal(calls, 0);
  t.mock.timers.tick(0);
  assert.equal(calls, 1);
  t.mock.timers.tick(5000);
  assert.equal(calls, 1);
  refresh.request();
  t.mock.timers.tick(0);
  assert.equal(calls, 2);
});

test("a continuous burst renders at a steady gap and delivers the last edit", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  const start = Date.now();
  const times = [];
  const refresh = coalescedRefresh(() => times.push(Date.now() - start), 150);
  // 40 updates per second for two seconds, on a 1 ms clock.
  for (let ms = 0; ms < 3000; ms++) {
    if (ms < 2000 && ms % 25 === 0) refresh.request();
    t.mock.timers.tick(1);
  }
  // The first update renders at once, then one render per 150 ms gap.
  assert.equal(times[0], 1);
  const gaps = times.slice(1).map((v, i) => v - times[i]);
  assert.deepEqual(new Set(gaps), new Set([150]));
  // 80 updates, 15 renders; the last one (2101 ms) shows the last update.
  assert.equal(times.length, 15);
  assert.ok(times.at(-1) >= 1975, `last render at ${times.at(-1)}`);
});

test("an edit right after a render waits only for the rest of the gap", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++, 150);
  refresh.request();
  t.mock.timers.tick(0);
  assert.equal(calls, 1);
  t.mock.timers.tick(100);
  refresh.request();
  t.mock.timers.tick(49);
  assert.equal(calls, 1);
  t.mock.timers.tick(1);
  assert.equal(calls, 2);
});

test("an immediate refresh supersedes pending work and cancellation is final", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
  refresh.request();
  refresh.run();
  t.mock.timers.tick(1000);
  assert.equal(calls, 1);
  // A request right after a render waits for the gap; run() replaces it.
  refresh.run();
  refresh.request();
  t.mock.timers.tick(10);
  refresh.run();
  t.mock.timers.tick(1000);
  assert.equal(calls, 3);
  refresh.request();
  refresh.cancel();
  t.mock.timers.tick(1000);
  assert.equal(calls, 3);
});

test("a failing render does not block later renders", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => {
    calls++;
    if (calls === 1) throw new Error("render failed");
  });
  refresh.request();
  assert.throws(() => t.mock.timers.tick(0), /render failed/);
  refresh.request();
  t.mock.timers.tick(REMOTE_RENDER_GAP_MS);
  assert.equal(calls, 2);
});
