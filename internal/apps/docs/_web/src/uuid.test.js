import { test } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "./uuid.js";

test("UUID uses the native secure-context implementation when available", () => {
  const crypto = {
    randomUUID() {
      assert.equal(this, crypto);
      return "native-id";
    },
    getRandomValues() {
      throw new Error("fallback should not run");
    },
  };
  assert.equal(randomUUID(crypto), "native-id");
});

test("HTTP UUID fallback preserves entropy and sets RFC 4122 version/variant", () => {
  let calls = 0;
  const crypto = {
    getRandomValues(bytes) {
      assert.ok(bytes instanceof Uint8Array);
      assert.equal(bytes.length, 16);
      calls++;
      bytes.set(Array.from({ length: 16 }, (_, i) => 240 + i));
      return bytes;
    },
  };
  assert.equal(randomUUID(crypto), "f0f1f2f3-f4f5-46f7-b8f9-fafbfcfdfeff");
  assert.equal(calls, 1);
});
