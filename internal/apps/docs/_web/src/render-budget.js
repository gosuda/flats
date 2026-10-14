// A remote writer can send many small updates a second, and each rendered
// View refresh parses the whole Markdown document and replaces its DOM. Remote
// edits therefore render at most once per gap: an edit after an idle gap
// renders on the next task (so a loaded document and isolated edits appear at
// once), and edits arriving sooner render together when the gap ends, always
// with the latest text. The gap is measured from the end of the previous
// render, so the main thread stays free for at least that long between
// renders however large the document is. Local interactions call run().
export const REMOTE_RENDER_GAP_MS = 150;

export function coalescedRefresh(refresh, gap = REMOTE_RENDER_GAP_MS) {
  let timer;
  let lastEnd = -Infinity;

  const cancel = () => {
    clearTimeout(timer);
    timer = undefined;
  };
  const run = () => {
    cancel();
    try {
      refresh();
    } finally {
      lastEnd = Date.now();
    }
  };
  return {
    request() {
      // A scheduled refresh reads the text when it runs, so it already
      // includes this edit.
      if (timer !== undefined) return;
      timer = setTimeout(run, Math.max(0, lastEnd + gap - Date.now()));
    },
    run,
    cancel,
  };
}
