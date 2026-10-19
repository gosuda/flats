import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import { BoardState } from '../state.js';
import { THEMES } from '../theme.js';
import { TextEditor } from '../textedit.js';

const ID = 'e_shape00001';
const shape = (text, v) => ({ id: ID, type: 'shape', x: 0, y: 0, w: 200, h: 100, rot: 0, z: 'V', text, inc: 1, v: { geom: 1, text: v } });

// setup opens a TextEditor on a shape whose text is at version 10, with a
// stub app that records commits.
function setup(t) {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const doc = installDOM([{ cls: 'board-ui' }, { cls: 'toasts' }]);
  const state = new BoardState();
  state.session = 'S';
  state.loadSnapshot({ seq: 10, elements: [shape('start', 10)] });
  const app = {
    state, cam: { z: 1 }, editor: null, commits: [], focused: 0,
    renderer: { theme: THEMES.light, setHidden() {} },
    presence: { send() {} },
    styleBar: { hide() {}, update() {} },
    toScreen: (x, y) => [x, y],
    commit(list) { this.commits.push(list); return {}; },
    focusBoard() { this.focused++; },
  };
  const ed = new TextEditor(app, state.get(ID));
  app.editor = ed;
  // remote delivers a delta the way the app does: apply, then tell the editor.
  const remote = (seq, change) => {
    assert.equal(state.applyDelta({ seq, from: 'T:1', changes: [{ id: ID, ...change }] }).status, 'applied');
    ed.remote();
  };
  return { doc, state, app, ed, remote };
}

const textOps = (app) => app.commits.flat().filter((o) => o.g === 'text');

test('base_v is the text version seen when editing started', (t) => {
  const { app, ed, remote } = setup(t);
  ed.ta.value = 'mine';                                   // the person types
  remote(11, { inc: 1, set: { text: ['theirs', 11] } }); // a concurrent edit
  assert.equal(ed.ta.value, 'mine', 'a touched editor keeps the person\'s text');
  ed.commit();
  assert.deepEqual(textOps(app).map((o) => [o.val, o.base_v]), [['mine', 10]], 'not the version current at commit');
  assert.equal(app.focused, 1, 'focus goes back to the board region');
});

test('an untouched editor adopts remote text and its version', (t) => {
  const { app, ed, remote } = setup(t);
  remote(11, { inc: 1, set: { text: ['theirs', 11] } });
  assert.equal(ed.ta.value, 'theirs');
  ed.ta.value = 'theirs, edited';
  remote(12, { inc: 1, set: { geom: [{ x: 5, y: 0, w: 200, h: 100, rot: 0 }, 12] } });
  ed.commit();
  assert.deepEqual(textOps(app).map((o) => [o.val, o.base_v]), [['theirs, edited', 11]]);
});

test('remote text does not touch the editor during composition', (t) => {
  const { app, ed, remote } = setup(t);
  ed.ta.dispatch('compositionstart');
  remote(11, { inc: 1, set: { text: ['theirs', 11] } });
  assert.equal(ed.ta.value, 'start');
  ed.ta.value = 'start한';
  ed.ta.dispatch('compositionend');
  ed.commit();
  assert.deepEqual(textOps(app).map((o) => [o.val, o.base_v]), [['start한', 10]]);
});

test('an element deleted during composition offers Keep my text when composition ends', (t) => {
  const { doc, app, ed, remote } = setup(t);
  ed.ta.dispatch('compositionstart');
  ed.ta.value = 'my words';
  remote(11, { del: 11 });
  assert.equal(ed.done, false, 'the editor stays until composition ends');
  ed.ta.dispatch('compositionend');
  assert.equal(ed.done, true);
  assert.equal(app.commits.length, 0, 'nothing is committed for a vanished element');
  const notice = doc.querySelector('.toasts').children[0];
  assert.ok(notice && notice.querySelectorAll('button').some((b) => b.children.some((c) => c.textContent === 'Keep my text')));
});

test('an element restored before composition ends is committed, not offered as lost', (t) => {
  const { doc, app, ed, remote } = setup(t);
  ed.ta.dispatch('compositionstart');
  ed.ta.value = 'my words';
  remote(11, { del: 11 });
  assert.equal(ed.vanished, true);
  remote(12, { revive: shape('start', 10) }); // a rollback brings it back
  ed.ta.dispatch('compositionend');
  assert.equal(ed.done, false, 'editing continues');
  assert.equal(doc.querySelector('.toasts').children.length, 0, 'no Keep my text notice');
  ed.commit();
  assert.deepEqual(textOps(app).map((o) => o.val), ['my words']);
});
