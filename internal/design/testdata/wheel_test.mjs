// Runs under Node from TestWheelPan, which puts web/wheel.js beside this file.
import { createWheelPan } from './wheel.js';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// [shiftKey, deltaX, deltaY, expected x, expected y]. Numbers compare with ===,
// so -0 equals 0.
const cases = [
  // Shift with a vertical wheel, as Windows Chrome sends it, pans sideways only.
  [true, 0, 100, 100, 0],
  [true, 0, -100, -100, 0],
  [true, -0, 100, 100, 0],
  // Shift with a horizontal event, the shape macOS is expected to send, keeps its horizontal delta.
  [true, -30, 0, -30, 0],
  [true, 100, 5, 100, 0],
  [true, 10, 10, 10, 0],
  [true, 0, 0, 0, 0],
  // A mostly vertical gesture with stray horizontal movement uses its vertical delta horizontally.
  [true, 5, 100, 100, 0],
  [true, -5, 100, 100, 0],
  [true, -5, -100, -100, 0],
  // Both sides of the boundary, with opposite signs and with equal sizes.
  [true, 10, -11, -11, 0],
  [true, 11, -10, 11, 0],
  [true, 10, -10, 10, 0],
  [true, -10, 10, -10, 0],
  [true, 10, 11, 11, 0],
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
  const got = createWheelPan({ horizontalOnly: true }).pan({ shiftKey, deltaX, deltaY, timeStamp: 0 });
  if (got.x !== x || got.y !== y) {
    failed++;
    console.error(`row ${i}: shift=${shiftKey} (${deltaX}, ${deltaY}): got (${got.x}, ${got.y}), want (${x}, ${y})`);
  }
});
if (failed) process.exit(1);
const event = (deltaX, deltaY, timeStamp, shiftKey = true) => ({ deltaX, deltaY, timeStamp, shiftKey });
const pairs = [ [[-1, 2], [-1, 1]], [[1, -2], [1, -1]], [[10, -11], [11, -10]] ];
for (const [first, next] of pairs) {
  const wheel = createWheelPan({ horizontalOnly: true });
  wheel.pan(event(...first, 0));
  for (let i = 1; i <= 20; i++) {
    const [dx, dy] = i % 2 ? next : first;
    assert.deepEqual(wheel.pan(event(dx, dy, i * 16)), { x: dy, y: 0 });
  }
}
{
  const wheel = createWheelPan({ horizontalOnly: true });
  assert.deepEqual(wheel.pan(event(0, 0, 0)), { x: 0, y: 0 });
  assert.deepEqual(wheel.pan(event(5, 1, 1)), { x: 5, y: 0 });
  assert.deepEqual(wheel.pan(event(1, 5, 201)), { x: 1, y: 0 }); // exactly 200 ms
  wheel.pan(event(0, 0, 400)); // cannot prolong the gesture
  assert.deepEqual(wheel.pan(event(1, 5, 402)), { x: 5, y: 0 });
  wheel.pan(event(0, 0, 403, false));
  assert.deepEqual(wheel.pan(event(5, 1, 404)), { x: 5, y: 0 });
  wheel.reset();
  assert.deepEqual(wheel.pan(event(1, 5, 405)), { x: 5, y: 0 });
  for (const time of [undefined, NaN, Infinity, -1, 406]) {
    assert.deepEqual(wheel.pan(event(5, 1, time)), { x: 1, y: 0 });
  }
  assert.deepEqual(createWheelPan({ horizontalOnly: true }).pan(event(5, 1, 406)), { x: 5, y: 0 });
}
// A selected source axis stays selected even through orthogonal-only samples.
for (const first of [[0, -1], [1, 0], [1, -1]]) {
  const wheel = createWheelPan({ horizontalOnly: true });
  const vertical = Math.abs(first[1]) > Math.abs(first[0]);
  wheel.pan(event(...first, 0));
  const orthogonal = vertical ? [12, 0] : [0, -12];
  assert.deepEqual(wheel.pan(event(...orthogonal, 10)), { x: 0, y: 0 });
  assert.deepEqual(wheel.pan(event(...first, 20)), { x: vertical ? first[1] : first[0], y: 0 });
}
// Real direction reversals on the selected axis still reverse the pan.
{
  const wheel = createWheelPan({ horizontalOnly: true });
  assert.deepEqual(wheel.pan(event(.001, -.002, 0)), { x: -.002, y: 0 });
  assert.deepEqual(wheel.pan(event(-.001, .002, 10)), { x: .002, y: 0 });
}
// Raw line/page values and Alt use the same mapping without a startup dead zone.
for (const deltaMode of [0, 1, 2]) for (const altKey of [false, true]) {
  assert.deepEqual(createWheelPan({ horizontalOnly: true }).pan({ ...event(.001, .002, 0), deltaMode, altKey }), { x: .002, y: 0 });
  assert.deepEqual(createWheelPan({ horizontalOnly: true }).pan({ ...event(.001, .002, 0, false), deltaMode, altKey }), { x: .001, y: .002 });
}
// The shared mapper's default keeps the document diagram viewer's two-axis contract.
for (const options of [undefined, {}, { horizontalOnly: false }]) {
  const wheel = createWheelPan(options);
  assert.deepEqual(wheel.pan(event(5, 10, 0)), { x: 10, y: 5 });
  assert.deepEqual(wheel.pan(event(10, 5, 10)), { x: 5, y: 10 });
  wheel.reset();
  assert.deepEqual(wheel.pan(event(10, 0, 20)), { x: 10, y: 0 });
  assert.deepEqual(wheel.pan(event(7, 12, 30, false)), { x: 7, y: 12 });
}
// Exercise the real viewport handler and its reset listeners, with DOM output
// stubbed. This catches wiring omissions that mapper-only tests cannot see.
const app = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
assert.match(app, /import \{ createWheelPan \} from '\.\/wheel\.js';/);
assert.match(app, /const wheel = createWheelPan\(\{ horizontalOnly: true \}\);/);
const mapperSource = fs.readFileSync(new URL('./wheel.js', import.meta.url), 'utf8').replace('export function', 'function');
const setup = app.slice(app.indexOf('function setupViewport()'), app.indexOf('// ---- boards ----'));
const zoom = app.slice(app.indexOf('function zoomAt('), app.indexOf('function zoomBy('));
const listeners = {}, keys = {}, S = { view: { x: 0, y: 0, z: 1 } };
const vp = { addEventListener: (k, f, opts) => { listeners[k] = f; if (k === 'wheel') assert.equal(opts.passive, false); }, getBoundingClientRect: () => ({ left: 20, top: 30 }) };
let handlerClock = 0, applies = 0;
vm.runInNewContext(mapperSource + '\n' + setup + '\n' + zoom + '\nsetupViewport();', {
  S, $: () => vp, window: { addEventListener: (k, f) => { keys[k] = f; } },
  document: { activeElement: null }, performance: { now: () => handlerClock },
  loadVisible: () => {}, applyView: () => { applies++; },
});
const send = (e) => { let prevented = false; listeners.wheel({ ...e, clientX: 120, clientY: 230, preventDefault: () => { prevented = true; } }); assert.ok(prevented); };
const reset = () => { keys.blur(); S.view = { x: 0, y: 0, z: 1 }; };
reset(); send(event(-1, 2, 0));
handlerClock = 1000; // delayed handling, but the input events are 15 ms apart
keys.keydown({ key: 'Shift', code: 'ShiftLeft', shiftKey: true, repeat: true });
send(event(-1, 1, 15));
assert.deepEqual([S.view.x, S.view.y], [-3, 0]);
for (const clear of [() => keys.keyup({ key: 'Shift', code: 'ShiftRight', shiftKey: false }), () => keys.blur()]) {
  reset(); send(event(0, 10, 0)); clear(); send(event(10, 0, 10));
  assert.deepEqual([S.view.x, S.view.y], [-20, 0]);
}
reset(); send(event(7, 12, 0, false));
assert.deepEqual([S.view.x, S.view.y], [-7, -12]);
reset(); send(event(0, 10, 0)); send(event(0, 0, 1, false)); send(event(10, 0, 2));
assert.deepEqual([S.view.x, S.view.y], [-20, 0]);
for (const key of ['ctrlKey', 'metaKey']) for (const shiftKey of [false, true]) {
  reset(); send(event(0, 10, 0));
  send({ ...event(0, 20, 10, shiftKey), [key]: true });
  const z = Math.exp(-0.2);
  assert.equal(S.view.z, z); assert.equal(S.view.x, 100 - 110 * z); assert.equal(S.view.y, 200 - 200 * z);
  const before = S.view.x; send(event(10, 0, 20)); assert.equal(S.view.x, before - 10);
}
assert.ok(applies > 0);
console.log('ok');
