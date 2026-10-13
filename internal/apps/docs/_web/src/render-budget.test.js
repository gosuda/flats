import { test } from "node:test";
import assert from "node:assert/strict";
import { coalescedRefresh } from "./render-budget.js";

test("continuous remote edits produce bounded, non-starving refreshes", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  const start = Date.now();
  const times = [];
  const refresh = coalescedRefresh(() => times.push(Date.now() - start));
  for (let i = 0; i < 160; i++) {
    refresh.request();
    t.mock.timers.tick(25);
  }
  // The first edit renders at once (observed at the 25 ms tick); the burst
  // then renders once a second.
  assert.deepEqual(times, [25, 1025, 2025, 3025]);
  refresh.cancel();
});

test("an edit after an idle period renders on the next task", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
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

test("quiet edits coalesce and successive renders have a minimum gap", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
  refresh.request();
  t.mock.timers.tick(0);
  assert.equal(calls, 1);
  t.mock.timers.tick(400);
  refresh.request();
  t.mock.timers.tick(100);
  refresh.request();
  // Debounced 250 ms after the last edit (t=750), past the 500 ms gap.
  t.mock.timers.tick(249);
  assert.equal(calls, 1);
  t.mock.timers.tick(1);
  assert.equal(calls, 2);
  // An edit right after a render waits for the 500 ms gap, not 250 ms.
  refresh.request();
  t.mock.timers.tick(499);
  assert.equal(calls, 2);
  t.mock.timers.tick(1);
  assert.equal(calls, 3);
});

test("an immediate refresh supersedes pending work and cancellation is final", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
  refresh.request();
  refresh.run();
  t.mock.timers.tick(100);
  assert.equal(calls, 1);
  refresh.request();
  t.mock.timers.tick(10);
  refresh.run();
  t.mock.timers.tick(1000);
  assert.equal(calls, 2);
  refresh.request();
  refresh.cancel();
  t.mock.timers.tick(1000);
  assert.equal(calls, 2);
});
