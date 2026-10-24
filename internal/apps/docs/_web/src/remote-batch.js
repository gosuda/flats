// Remote changes in batches (spec rule 21b, TECH 3.11).
//
// The editor reworks the whole edited paragraph after every change it
// applies, so a page that applied each remote update as it arrived could
// spend all its time applying guests' changes and fall behind for good.
// RemoteBatcher applies queued remote updates together instead: when the
// page is idle an update is applied at once; after each batch is applied and
// rendered, the next waits at least as long as that batch took (and at most
// maxWait), so the main thread is idle at least half the time. Local typing
// does not pass through here and is applied at once.
export const MAX_WAIT = 1000;

// A pause shorter than this is not measurable with timers: the next batch is
// applied at once.
const MIN_WAIT = 1;

export class RemoteBatcher {
  // apply(updates) applies the queued updates, oldest first, together.
  // afterRender(done, now) calls done(ms) once the page has rendered the
  // applied batch, with the time rendering took.
  constructor(
    apply,
    {
      now = () => performance.now(),
      setTimer = (f, ms) => setTimeout(f, ms),
      clearTimer = (t) => clearTimeout(t),
      afterRender = renderedFrame,
      maxWait = MAX_WAIT,
    } = {},
  ) {
    this.apply = apply;
    this.now = now;
    this.setTimer = setTimer;
    this.clearTimer = clearTimer;
    this.afterRender = afterRender;
    this.maxWait = maxWait;
    this.queue = [];
    // idle, rendering (a batch was applied and the page renders it) or
    // waiting (the pause after a batch).
    this.state = "idle";
    this.timer = 0;
    // Advanced by flush and destroy, so a stale render or timer callback
    // does nothing.
    this.generation = 0;
    // The cost of the last batch, for tests and diagnostics.
    this.lastCost = 0;
  }
  get pending() {
    return this.queue.length;
  }
  push(update) {
    if (this.destroyed) return;
    this.queue.push(update);
    if (this.state === "idle") this.run();
  }
  // flush applies everything queued now, for example before a welcome or a
  // stop, then waits as after any batch.
  flush() {
    if (this.destroyed || !this.queue.length) return;
    this.clearTimer(this.timer);
    this.timer = 0;
    this.generation++;
    this.run();
  }
  destroy() {
    this.destroyed = true;
    this.clearTimer(this.timer);
    this.timer = 0;
    this.generation++;
    this.queue = [];
  }
  run() {
    if (!this.queue.length) {
      this.state = "idle";
      return;
    }
    const updates = this.queue,
      generation = ++this.generation,
      started = this.now();
    this.queue = [];
    this.state = "rendering";
    try {
      this.apply(updates);
    } catch (e) {
      this.state = "idle";
      throw e;
    }
    const applied = this.now() - started;
    this.afterRender((rendered) => {
      if (generation !== this.generation) return;
      const cost = applied + Math.max(0, rendered);
      this.lastCost = cost;
      const wait = Math.min(this.maxWait, cost);
      if (wait < MIN_WAIT) {
        this.run();
        return;
      }
      this.state = "waiting";
      this.timer = this.setTimer(() => {
        this.timer = 0;
        if (generation === this.generation) this.run();
      }, wait);
    }, this.now);
  }
}

// renderedFrame calls done with the time from the start of the next
// animation frame until the page has rendered it (CodeMirror's measuring,
// style, layout and paint run in between). A page whose frames are paused,
// such as a hidden tab, does not wait for one: done(0) after 100 ms.
export function renderedFrame(done, now) {
  if (typeof requestAnimationFrame !== "function") {
    done(0);
    return;
  }
  let finished = false;
  const fallback = setTimeout(() => {
    if (finished) return;
    finished = true;
    done(0);
  }, 100);
  requestAnimationFrame((frameStart) => {
    if (finished) return;
    finished = true;
    clearTimeout(fallback);
    setTimeout(() => done(now() - frameStart), 0);
  });
}
