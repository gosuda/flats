// Remote edits must not force a full-document render on every transaction.
// The first edit after an idle half second refreshes on the next task, so a
// loaded document and isolated edits appear at once. Later edits refresh
// after a quiet period, at most twice per second, and at least once per
// second while they continue. Local interactions can call run() directly.
export function coalescedRefresh(refresh) {
  let timer;
  let queuedAt = null;
  let leading = false;
  let lastRun = -Infinity;

  const cancel = () => {
    clearTimeout(timer);
    timer = undefined;
    queuedAt = null;
    leading = false;
  };
  const run = () => {
    cancel();
    lastRun = Date.now();
    refresh();
  };
  return {
    request() {
      // A pending immediate refresh already renders the latest state.
      if (leading) return;
      const now = Date.now();
      leading = queuedAt === null && now - lastRun >= 500;
      if (queuedAt === null) queuedAt = now;
      const deadline = leading
        ? now
        : Math.min(Math.max(now + 250, lastRun + 500), queuedAt + 1000);
      clearTimeout(timer);
      timer = setTimeout(run, Math.max(0, deadline - now));
    },
    run,
    cancel,
  };
}
