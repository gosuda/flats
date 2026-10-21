// Tool overlays on the dynamic layer. A pointer gesture can run under a tool
// other than the current one: a stylus draws with the pen while select is
// active (rule 20), so that gesture's preview is drawn as well.

// drawToolOverlays draws the current tool's overlay and then, when gesture is
// a different tool, the gesture's preview on top. Each draw gets its own
// canvas state.
export function drawToolOverlays(ctx, r, tool, gesture) {
  for (const t of gesture && gesture !== tool ? [tool, gesture] : [tool]) {
    if (!t.draw) continue;
    ctx.save();
    t.draw(ctx, r);
    ctx.restore();
  }
}
