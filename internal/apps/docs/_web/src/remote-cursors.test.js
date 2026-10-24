import { test } from "node:test";
import assert from "node:assert/strict";
import { caretLabel } from "./remote-cursors.js";

test("a remote caret shows the name as text and the badge only for guests", () => {
  assert.deepEqual(
    caretLabel({ name: "Mina", guest: true, color: "#9c36b5" }),
    { name: "Mina", guest: true, color: "#9c36b5" },
  );
  assert.equal(caretLabel({ name: "Jun", guest: "yes" }).guest, false);
  // Only a plain color reaches the style attribute.
  assert.equal(
    caretLabel({ name: "x", color: "red; background: url(x)" }).color,
    "#30bced",
  );
  assert.equal(caretLabel(undefined).name, "Anonymous");
});
