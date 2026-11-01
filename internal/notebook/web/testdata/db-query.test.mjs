'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  orderRows, normalizeSchema, normalizeView, normalizeRows, fold, compareDecimal, compareDate,
  parseDecimal, createQuery, OPERATORS,
} from '../query-worker.js';

const vectors = JSON.parse(readFileSync(new URL('../../testdata/query_vectors.json', import.meta.url), 'utf8'));

test('shared query vectors cover every applicable operator', () => {
  assert.ok(vectors.length > 0);
  const seen = new Map(Object.keys(OPERATORS).map((type) => [type, new Set()]));
  for (const c of vectors) {
    const schema = normalizeSchema(c.schema);
    for (const f of normalizeView(c.view).filter) {
      const type = schema.properties.find((p) => p.id === f.property).type;
      seen.get(type).add(f.op);
    }
  }
  for (const [type, ops] of Object.entries(OPERATORS)) assert.deepEqual([...seen.get(type)].sort(), [...ops].sort(), type);
});

for (const c of vectors) {
  test(`shared query vector: ${c.name}`, () => {
    const schema = normalizeSchema(c.schema);
    const view = normalizeView(c.view);
    const rows = normalizeRows(c.rows);
    const before = structuredClone({ schema, view, rows });
    assert.deepEqual(orderRows(schema, view, rows), c.want);
    assert.deepEqual({ schema, view, rows }, before, 'query must not mutate inputs');
    assert.deepEqual(orderRows(schema, view, [...rows].reverse()), c.want, 'input order must not break ties');
  });
}

test('explicit option identities still follow the stored option order', () => {
  const c = vectors.find((x) => x.name === 'select sort asc');
  const raw = structuredClone(c.schema);
  for (const [i, p] of raw.properties.entries()) {
    const id = raw.property_ids[i];
    p.ref = { id, inc: '1' };
    raw.options[id] = (raw.options[id] ?? []).map((o, j) => ({ ...o, ref: { id: p.option_order[j], inc: '1' } })).reverse();
  }
  const before = structuredClone(raw);
  const schema = normalizeSchema(raw);
  assert.deepEqual(orderRows(schema, normalizeView(c.view), normalizeRows(c.rows)), c.want);
  assert.deepEqual(raw, before);
});

test('simple folding matches the shared Unicode table exactly', () => {
  const { fold_version: version, fold_pairs: raw } = vectors[0];
  assert.equal(version, '15.0.0');
  const pairs = [...raw];
  assert.equal(pairs.length % 2, 0);
  const expected = new Map();
  for (let i = 0; i < pairs.length; i += 2) expected.set(pairs[i], pairs[i + 1]);
  assert.equal(expected.size, 1454);
  for (let code = 0; code <= 0x10ffff; code++) {
    const ch = String.fromCodePoint(code);
    assert.equal(fold(ch), expected.get(ch) ?? ch, `U+${code.toString(16)}`);
  }
});

test('simple case folding uses one code point per mapping', () => {
  assert.equal(fold('ẞ'), 'ß', 'full folding would give "ss"');
  assert.equal(fold('İ'), 'İ', 'İ has only a full folding');
  assert.equal(fold('Σς'), 'σσ');
  assert.equal(fold('ＡＢ'), 'ａｂ');
  assert.equal(fold('𐐀'), '𐐨', 'supplementary planes fold too');
  assert.equal(fold('Ǆ'), 'ǆ');
  assert.equal(fold('\u13F8'), '\u13F0', 'Cherokee folds small letters to capitals');
  assert.equal(fold('\uAB70'), '\u13A0');
  assert.equal(fold('\u13A0'), '\u13A0');
});

test('decimals compare exactly and parse as Go canonicalizes them', () => {
  assert.equal(compareDecimal('0.1', '0.10'), 0);
  assert.equal(compareDecimal('-0', '0'), 0);
  assert.equal(compareDecimal('12345678901234567890.1', '12345678901234567890.01'), 1);
  assert.equal(compareDecimal('-2', '-10'), 1);
  assert.equal(compareDecimal('9007199254740993', '9007199254740992'), 1, 'beyond float precision');
  for (const [input, want] of [['+001.2300e2', '123'], ['-0.0', '0'], ['.5', '0.5'], ['5.', '5'], ['1e-3', '0.001'], ['-12.5E1', '-125'], ['0e99', '0']]) {
    assert.equal(parseDecimal(input), want, input);
  }
  for (const bad of ['', '1e', 'e1', '1.2.3', ' 1', 'NaN', 'Infinity', '0x10', '1e999999']) assert.equal(parseDecimal(bad), null, bad);
});

test('a date-only value sorts before timed values on the same day', () => {
  assert.ok(compareDate('2024-02-29', '2024-02-29T00:00:00') < 0);
  assert.ok(compareDate('2024-02-28T23:59:59', '2024-02-29') < 0);
  assert.equal(compareDate('2024-02-29', '2024-02-29'), 0);
});

test('the operator table is closed and matches rule 10', () => {
  assert.deepEqual(Object.keys(OPERATORS).sort(), ['checkbox', 'date', 'multi_select', 'number', 'select', 'text', 'title', 'url']);
  assert.deepEqual(OPERATORS.select, ['is', 'is not', 'is any of', 'is empty', 'is not empty']);
  assert.deepEqual(OPERATORS.checkbox, ['is checked', 'is not checked']);
  const c = vectors[0];
  const s = normalizeSchema(c.schema);
  assert.throws(() => orderRows(s, { sort: [], filter: [{ property: s.properties[0].id, op: 'starts with', value: 'x' }] }, normalizeRows(c.rows)), /unknown filter operator/);
});

test('rows kept by this tab stay even when the filter excludes them', () => {
  const c = vectors.find((x) => x.name === 'checkbox is checked');
  const s = normalizeSchema(c.schema);
  const v = normalizeView(c.view);
  const rows = normalizeRows(c.rows);
  const outside = rows.find((r) => !c.want.includes(r.id)).id;
  const got = orderRows(s, v, rows, [outside]);
  assert.ok(got.includes(outside));
  assert.deepEqual(got.filter((x) => x !== outside), c.want);
});

test('without a Worker the query runs on the calling thread', () => {
  const q = createQuery();
  const c = vectors[0];
  let got = null;
  q.run(normalizeSchema(c.schema), normalizeView(c.view), normalizeRows(c.rows), [], (ids) => { got = ids; });
  assert.deepEqual(got, c.want);
  q.stop();
});
