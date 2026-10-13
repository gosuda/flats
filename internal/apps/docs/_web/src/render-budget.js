// Remote edits must not force a full-document render on every transaction.
// Refresh after a quiet period, at most twice per second, and at least once
// per second while edits continue. Local interactions can call run() directly.
export function coalescedRefresh(refresh) {
  let timer;
  let queuedAt = null;
  let lastRun = -Infinity;

  const cancel = () => {
    clearTimeout(timer);
    timer = undefined;
    queuedAt = null;
  };
  const run = () => {
    cancel();
    lastRun = Date.now();
    refresh();
  };
  return {
    request() {
      const now = Date.now();
      if (queuedAt === null) queuedAt = now;
      const deadline = Math.min(
        Math.max(now + 250, lastRun + 500),
        queuedAt + 1000,
      );
      clearTimeout(timer);
      timer = setTimeout(run, Math.max(0, deadline - now));
    },
    run,
    cancel,
  };
}
