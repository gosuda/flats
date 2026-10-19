// Runs under Node from TestWheelPan, which puts web/wheel.js beside this file.
import { wheelPan } from './wheel.js';

// [shiftKey, deltaX, deltaY, expected x, expected y]. Numbers compare with ===,
// so -0 equals 0.
const cases = [
  // Shift with a vertical wheel, as Windows Chrome sends it, pans sideways only.
  [true, 0, 100, 100, 0],
  [true, 0, -100, -100, 0],
  [true, -0, 100, 100, 0],
  // Shift with a horizontal event, the shape macOS is expected to send, is used as it is.
  [true, -30, 0, -30, 0],
  [true, 100, 5, 100, 5],
  [true, 10, 10, 10, 10],
  [true, 0, 0, 0, 0],
  // A mostly vertical gesture with stray horizontal movement swaps the axes.
  [true, 5, 100, 100, 5],
  [true, -5, 100, 100, -5],
  [true, -5, -100, -100, -5],
  // Both sides of the boundary, with opposite signs and with equal sizes.
  [true, 10, -11, -11, 10],
  [true, 11, -10, 11, -10],
  [true, 10, -10, 10, -10],
  [true, -10, 10, -10, 10],
  [true, 10, 11, 11, 10],
  // Without Shift nothing changes.
  [false, 0, 100, 0, 100],
  [false, 7, 0, 7, 0],
  [false, 7, 100, 7, 100],
  [false, 0, -100, 0, -100],
  [false, -7, 0, -7, 0],
  [false, -7, -100, -7, -100],
  [false, 33.333, -12.5, 33.333, -12.5],
];

let failed = 0;
cases.forEach(([shiftKey, deltaX, deltaY, x, y], i) => {
  const got = wheelPan({ shiftKey, deltaX, deltaY });
  if (got.x !== x || got.y !== y) {
    failed++;
    console.error(`row ${i}: shift=${shiftKey} (${deltaX}, ${deltaY}): got (${got.x}, ${got.y}), want (${x}, ${y})`);
  }
});
if (failed) process.exit(1);
console.log('ok');
