// Keep the first nonzero Shift sample's mapping until a 200 ms idle gap.
// Choosing the dominant axis on every sample reverses diagonal trackpad pans
// when quantization changes which axis is larger. Event creation times keep
// delayed handling from splitting a gesture; coalesced samples keep its mapping.
export function createWheelPan() {
  let swapped = null, last = null;
  const reset = () => { swapped = null; last = null; };
  const pan = (e) => {
    const dx = e.deltaX, dy = e.deltaY;
    if (!e.shiftKey) { reset(); return { x: dx, y: dy }; }
    if (dx === 0 && dy === 0) return { x: dx, y: dy };
    const time = e.timeStamp;
    if (Number.isFinite(time) && last !== null && time - last > 200) reset();
    if (swapped === null) swapped = Math.abs(dy) > Math.abs(dx);
    // Missing or backwards timestamps must not force a new mapping.
    if (Number.isFinite(time) && (last === null || time >= last)) last = time;
    return swapped ? { x: dy, y: dx } : { x: dx, y: dy };
  };
  return { pan, reset };
}
