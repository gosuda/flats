import { test } from "node:test";
import assert from "node:assert/strict";
import { coalescedRefresh } from "./render-budget.js";

test("continuous remote edits produce bounded, non-starving refreshes", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  const start = Date.now();
  const times = [];
  const refresh = coalescedRefresh(() => times.push(Date.now() - start));
  for (let i = 0; i < 80; i++) {
    refresh.request();
    t.mock.timers.tick(25);
  }
  assert.deepEqual(times, [1000, 2000]);
  refresh.cancel();
});

test("quiet edits coalesce and successive renders have a minimum gap", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
  refresh.request();
  t.mock.timers.tick(100);
  refresh.request();
  t.mock.timers.tick(249);
  assert.equal(calls, 0);
  t.mock.timers.tick(1);
  assert.equal(calls, 1);
  refresh.request();
  t.mock.timers.tick(499);
  assert.equal(calls, 1);
  t.mock.timers.tick(1);
  assert.equal(calls, 2);
});

test("an immediate refresh supersedes pending work and cancellation is final", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  let calls = 0;
  const refresh = coalescedRefresh(() => calls++);
  refresh.request();
  t.mock.timers.tick(10);
  refresh.run();
  t.mock.timers.tick(1000);
  assert.equal(calls, 1);
  refresh.request();
  refresh.cancel();
  t.mock.timers.tick(1000);
  assert.equal(calls, 1);
});
