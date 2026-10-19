// wheelPan returns how far a wheel event pans the canvas, to subtract from the
// view offset. Windows Chrome reports Shift+wheel as a vertical delta with
// shiftKey set, and the canvas cancels the browser's own sideways scroll, so
// Shift swaps the axes when the gesture is mostly vertical. An event that is
// already mostly horizontal, which is the shape macOS is expected to send for
// Shift+wheel, is used as it is.
export function wheelPan(e) {
  const dx = e.deltaX, dy = e.deltaY;
  if (e.shiftKey && Math.abs(dy) > Math.abs(dx)) return { x: dy, y: dx };
  return { x: dx, y: dy };
}
