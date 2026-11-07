'use strict';

import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ImageQueue,
  imageMarkdown,
  imageURL,
  pastedImages,
  uploadImage,
} from "./image-paste.js";

const file = (name, type) => ({ name, type });
const clipboard = (files, text = "") => ({
  files,
  getData: (kind) => (kind === "text/plain" ? text : ""),
});

test("a clipboard with only an image adds it", () => {
  const png = file("image.png", "image/png");
  assert.deepEqual(pastedImages(clipboard([png])), [png]);
});

test("blank text does not count as text", () => {
  const png = file("image.png", "image/png");
  assert.deepEqual(pastedImages(clipboard([png], " \n\t")), [png]);
});

test("text wins over the picture a spreadsheet adds", () => {
  const png = file("image.png", "image/png");
  assert.deepEqual(pastedImages(clipboard([png], "a\tb\n1\t2")), []);
});

test("a file manager's file name does not count as text", () => {
  const png = file("chart.png", "image/png");
  assert.deepEqual(pastedImages(clipboard([png], "chart.png")), [png]);
  assert.deepEqual(pastedImages(clipboard([png], "other.png")), []);
});

test("the file name exception is for a single image", () => {
  const a = file("a.png", "image/png"),
    b = file("b.png", "image/png");
  assert.deepEqual(pastedImages(clipboard([a, b], "a.png")), []);
});

test("other files are ignored and several images keep their order", () => {
  const a = file("a.png", "image/png"),
    pdf = file("x.pdf", "application/pdf"),
    b = file("b.jpg", "image/jpeg");
  assert.deepEqual(pastedImages(clipboard([a, pdf, b])), [a, b]);
  assert.deepEqual(pastedImages(clipboard([pdf])), []);
});

test("a clipboard with nothing adds nothing", () => {
  assert.deepEqual(pastedImages(clipboard([], "text")), []);
  assert.deepEqual(pastedImages(null), []);
});

test("an image alone on its line gets a line break", () => {
  assert.equal(imageMarkdown("images/a.png", "", ""), "![](images/a.png)\n");
  assert.equal(imageMarkdown("images/a.png", "  ", "\t"), "![](images/a.png)\n");
});

test("an image next to text stays inline", () => {
  assert.equal(imageMarkdown("images/a.png", "see ", ""), "![](images/a.png)");
  assert.equal(imageMarkdown("images/a.png", "", " here"), "![](images/a.png)");
});

test("the document and name are encoded", () => {
  assert.equal(
    imageURL("새 문서/a b.md", "Kim & Lee"),
    "/_docs/api/images?doc=%EC%83%88+%EB%AC%B8%EC%84%9C%2Fa+b.md&name=Kim+%26+Lee",
  );
});

test("an upload sends the page's header and returns the reference", async () => {
  let seen;
  const body = {};
  const reference = await uploadImage(body, "a.md", "Kim", async (url, init) => {
    seen = { url, init };
    return { ok: true, json: async () => ({ reference: "images/x.png" }) };
  });
  assert.equal(reference, "images/x.png");
  assert.equal(seen.init.method, "POST");
  assert.equal(seen.init.headers["X-Flats-Docs"], "1");
  assert.equal(seen.init.body, body);
});

test("a refusal gives the host's reason", async () => {
  await assert.rejects(
    uploadImage({}, "a.md", "Kim", async () => ({
      ok: false,
      json: async () => ({ code: "too_large", message: "Too big." }),
    })),
    { message: "Too big." },
  );
});

test("a failure without a reason or connection still says something", async () => {
  await assert.rejects(
    uploadImage({}, "a.md", "Kim", async () => ({
      ok: false,
      json: async () => {
        throw new Error("not JSON");
      },
    })),
    { message: "The image could not be added." },
  );
  await assert.rejects(
    uploadImage({}, "a.md", "Kim", async () => {
      throw new Error("offline");
    }),
    /could not be sent/,
  );
});

// queue runs an ImageQueue whose uploads finish when the test says so.
function queue() {
  const uploads = [],
    inserted = [],
    errors = [],
    active = [];
  const q = new ImageQueue({
    upload: (file) =>
      new Promise((resolve, reject) =>
        uploads.push({ file, resolve, reject }),
      ),
    insert: (reference, place) => inserted.push([reference, place]),
    onActive: (on) => active.push(on),
    onError: (message) => errors.push(message),
  });
  return { q, uploads, inserted, errors, active };
}
const settle = () => new Promise((resolve) => setImmediate(resolve));

test("images go one at a time, and insert in order", async () => {
  const { q, uploads, inserted, active } = queue();
  q.add(["a", "b"], "first");
  q.add(["c"], "second");
  assert.equal(q.active, true);
  await settle();
  assert.equal(uploads.length, 1);
  uploads[0].resolve("ra");
  await settle();
  assert.equal(uploads.length, 2);
  uploads[1].resolve("rb");
  await settle();
  uploads[2].resolve("rc");
  await settle();
  assert.deepEqual(inserted, [
    ["ra", "first"],
    ["rb", "first"],
    ["rc", "second"],
  ]);
  assert.deepEqual(active, [true, false]);
  assert.equal(q.active, false);
});

test("a refused image reports and the next one continues", async () => {
  const { q, uploads, inserted, errors } = queue();
  q.add(["a", "b"], "here");
  await settle();
  uploads[0].reject(new Error("Too big."));
  await settle();
  uploads[1].resolve("rb");
  await settle();
  assert.deepEqual(errors, ["Too big."]);
  assert.deepEqual(inserted, [["rb", "here"]]);
});

test("an insert that throws reports like a refusal", async () => {
  const errors = [];
  const q = new ImageQueue({
    upload: async () => "r",
    insert: () => {
      throw new Error("The page stopped syncing.");
    },
    onActive: () => {},
    onError: (m) => errors.push(m),
  });
  q.add(["a"], "here");
  await settle();
  assert.deepEqual(errors, ["The page stopped syncing."]);
  assert.equal(q.active, false);
});

test("after the page goes away nothing inserts and nothing more is sent", async () => {
  const { q, uploads, inserted, errors, active } = queue();
  q.add(["a", "b"], "here");
  await settle();
  q.close();
  uploads[0].resolve("ra");
  await settle();
  assert.equal(uploads.length, 1);
  assert.deepEqual(inserted, []);
  assert.deepEqual(active, [true, false]);
  q.add(["c"], "here");
  await settle();
  assert.equal(uploads.length, 1);
  assert.deepEqual(errors, []);
});

test("a refusal after the page goes away reports nothing", async () => {
  const { q, uploads, errors } = queue();
  q.add(["a"], "here");
  await settle();
  q.close();
  uploads[0].reject(new Error("Too big."));
  await settle();
  assert.deepEqual(errors, []);
});
