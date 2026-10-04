import { test } from "node:test";
import assert from "node:assert/strict";
import {
  base64,
  unbase64,
  encodePresence,
  decodePresence,
  parseFrame,
  utf8Bytes,
} from "./protocol.js";
test("base64 binary exact and strict size validation", () => {
  const b = Uint8Array.from({ length: 1000 }, (_, i) => i % 256);
  assert.deepEqual(unbase64(base64(b)), b);
  for (const s of ["", "AA==", "AQID"]) assert.equal(base64(unbase64(s)), s);
  for (const s of ["AA", "A!AA", "AB==", "===="])
    assert.throws(() => unbase64(s));
  assert.throws(() => unbase64(base64(b), 999));
});
test("presence roundtrip and frame validation", () => {
  const p = { id: 12, clock: 3, state: { user: { name: "한국어 🙂" } } };
  assert.deepEqual(decodePresence(encodePresence([p])), p);
  assert.throws(() => decodePresence(encodePresence([p, p])));
  assert.deepEqual(parseFrame('{"t":"ping"}'), { t: "ping" });
  assert.throws(() => parseFrame("[]"));
  assert.throws(() => parseFrame("x".repeat(524289)));
  assert.equal(utf8Bytes("한🙂"), 7);
});
test("base64 handles all padding lengths and large buffers", () => {
  for (const size of [0, 1, 2, 3, 4, 31, 8191, 8192, 8193, 100001, 262144]) {
    const u = Uint8Array.from({ length: size }, (_, i) => (i * 71 + 3) % 256);
    assert.equal(base64(u), Buffer.from(u).toString("base64"));
    assert.deepEqual(unbase64(base64(u)), u);
  }
});
test("UTF8 size has no percent-encoding allocation and handles lone surrogates", () => {
  for (const s of [
    "a".repeat(100000),
    "한".repeat(350000),
    "🙂",
    "\ud800",
    "\udfff",
    "\ud800x",
    "한🙂\u0000",
  ]) {
    assert.equal(utf8Bytes(s), Buffer.byteLength(s));
  }
});
