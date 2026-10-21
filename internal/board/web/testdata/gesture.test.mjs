import test from 'node:test';
import assert from 'node:assert/strict';
import { drawToolOverlays } from '../gesture.js';
import { PenTool } from '../tools/pen.js';
import { THEMES } from '../theme.js';
import { BoardState } from '../state.js';

// recorder is a 2D context stand-in that logs every call and property set.
function recorder() {
  const log = [];
  return new Proxy({ log }, {
    get(t, k) { return k in t ? t[k] : (...args) => log.push([k, ...args]); },
    set(t, k, v) { log.push(['set', k, v]); return true; },
  });
}

function setup() {
  const app = { cam: { x: 0, y: 0, z: 1 }, renderer: { dynamic() {} }, styleBar: { update() {} }, state: new BoardState(), commits: [] };
  app.commit = (batch) => app.commits.push(batch);
  const pen = new PenTool(app);
  // The stand-in changes canvas state, which must not reach the stroke.
  const select = { draw(ctx) { ctx.log.push(['select']); ctx.globalAlpha = 0.08; ctx.translate(5, 5); } };
  const r = { theme: THEMES.light, toScreen: (x, y) => [x, y] };
  return { app, pen, select, r };
}

const calls = (ctx) => ctx.log.map((c) => c[0]);

test('a stylus stroke under the select tool draws its preview over the select overlay', () => {
  const { pen, select, r } = setup();
  pen.down({}, [0, 0]);
  pen.move({}, [10, 5]);
  pen.move({}, [20, 0]);
  const ctx = recorder();
  drawToolOverlays(ctx, r, select, pen);
  assert.deepEqual(calls(ctx), ['save', 'select', 'set', 'translate', 'restore', 'save', 'set', 'set', 'set', 'set', 'beginPath', 'moveTo', 'quadraticCurveTo', 'lineTo', 'stroke', 'restore']);
  assert.deepEqual(ctx.log.find((c) => c[0] === 'moveTo'), ['moveTo', 0, 0]);
});

test('a stylus stroke under the hand tool, which draws nothing itself, still draws its preview', () => {
  const { pen, r } = setup();
  const hand = {};
  pen.down({}, [0, 0]);
  pen.move({}, [10, 5]);
  const ctx = recorder();
  drawToolOverlays(ctx, r, hand, pen);
  assert.equal(calls(ctx).filter((c) => c === 'stroke').length, 1);
  assert.deepEqual(calls(ctx).slice(0, 1), ['save'], 'only the stroke draws');
});

test('the current tool draws once when no other tool runs the gesture', () => {
  const { pen, select, r } = setup();
  for (const gesture of [null, select]) {
    const ctx = recorder();
    drawToolOverlays(ctx, r, select, gesture);
    assert.deepEqual(calls(ctx), ['save', 'select', 'set', 'translate', 'restore']);
  }
  pen.down({}, [0, 0]);
  pen.move({}, [4, 4]);
  const ctx = recorder();
  drawToolOverlays(ctx, r, pen, pen);
  assert.equal(calls(ctx).filter((c) => c === 'stroke').length, 1, 'the pen tool draws its own stroke once');
});

test('a finished or cancelled stroke leaves no preview', () => {
  const { app, pen, select, r } = setup();
  for (const end of ['up', 'cancel']) {
    pen.down({}, [0, 0]);
    pen.move({}, [10, 10]);
    pen[end]();
    const ctx = recorder();
    drawToolOverlays(ctx, r, select, pen);
    assert.ok(!calls(ctx).includes('stroke'), end);
  }
  assert.equal(app.commits.length, 1, 'only the finished stroke is committed');
});
