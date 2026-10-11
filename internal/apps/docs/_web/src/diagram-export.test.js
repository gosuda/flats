import { test } from "node:test";
import assert from "node:assert/strict";
import { pngSize, pngBlob, snapshotSVG } from "./diagram-export.js";

test("PNG uses double resolution within axis and pixel budgets", () => {
  assert.deepEqual(pngSize(300, 200), { width: 600, height: 400 });
  for (const [w, h] of [
    [100000, 10],
    [10000, 10000],
    [10, 100000],
  ]) {
    const size = pngSize(w, h);
    assert.ok(size.width <= 8192 && size.height <= 8192);
    assert.ok(size.width * size.height <= 16 * 1024 * 1024);
  }
  for (const w of [0, -1, NaN, Infinity])
    assert.throws(() => pngSize(w, 10), /size/);
});

test("PNG snapshots resolved SVG styles and encodes canvas with explicit dimensions, background and failures", async () => {
  const names = ["document", "Image", "getComputedStyle", "XMLSerializer"];
  const original = names.map((name) => [
    name,
    Object.getOwnPropertyDescriptor(globalThis, name),
  ]);
  let failure = "",
    drawn,
    encodedType,
    fill,
    xmlSource;
  const values = new Map([
    ["fill", "rgb(1, 2, 3)"],
    ["font-family", "Arial"],
    ["marker-end", 'url("https://flats.test/doc#arrow")'],
    ["--paper", "#1c211e"],
  ]);
  function node(tag = "svg") {
    const props = new Map();
    return {
      localName: tag,
      style: { setProperty: (key, value) => props.set(key, value) },
      props,
      attrs: {},
      setAttribute(name, value) {
        this.attrs[name] = value;
      },
      getAttribute(name) {
        return this.attrs[name];
      },
      getBoundingClientRect: () => ({ width: 150, height: 100 }),
      viewBox: { baseVal: { width: 300, height: 200 } },
      querySelectorAll: () => [],
      cloneNode: () => node(tag),
    };
  }
  const context = {
    fillRect(x, y, w, h) {
      fill = { w, h, color: this.fillStyle };
    },
    drawImage(image, x, y, w, h) {
      drawn = { w, h, src: image.src };
      if (failure === "security") throw Error("Canvas security restriction");
    },
  };
  const canvas = {
    getContext: () => (failure === "context" ? null : context),
    toBlob(done, type) {
      if (failure === "taint")
        throw Object.assign(Error("tainted"), { name: "SecurityError" });
      encodedType = type;
      done(failure === "encode" ? null : new Blob(["PNG"], { type }));
    },
  };
  const mocks = {
    document: {
      baseURI: "https://flats.test/doc",
      fonts: { ready: Promise.resolve() },
      createElement: () => canvas,
    },
    Image: class {
      set src(value) {
        this._src = value;
        if (value)
          queueMicrotask(() =>
            failure === "decode" ? this.onerror?.() : this.onload?.(),
          );
      }
      get src() {
        return this._src;
      }
    },
    getComputedStyle: () => ({
      [Symbol.iterator]: () => values.keys(),
      getPropertyValue: (key) => values.get(key) || "",
    }),
    XMLSerializer: class {
      serializeToString(clone) {
        xmlSource = clone;
        return '<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div xmlns="http://www.w3.org/1999/xhtml">한글</div></foreignObject></svg>';
      }
    },
  };
  for (const [name, value] of Object.entries(mocks))
    Object.defineProperty(globalThis, name, { value, configurable: true });
  try {
    const svg = node();
    const blob = await pngBlob(svg);
    assert.equal(blob.type, "image/png");
    assert.equal(encodedType, "image/png");
    assert.equal(drawn.w, 600);
    assert.equal(drawn.h, 400);
    assert.match(drawn.src, /^data:image\/svg\+xml/);
    assert.ok(decodeURIComponent(drawn.src).includes("한글"));
    assert.deepEqual(fill, { w: 600, h: 400, color: "#1c211e" });
    assert.equal(xmlSource.props.get("marker-end"), 'url("#arrow")');
    assert.equal(xmlSource.props.get("font-family"), "Arial");
    assert.equal(xmlSource.attrs.width, "300");
    assert.equal(canvas.width, 0);
    for (const [kind, match] of [
      ["decode", /could not render/],
      ["context", /unavailable/],
      ["encode", /encoding failed/],
      ["security", /security restriction/],
      ["taint", /Save SVG instead/],
    ]) {
      failure = kind;
      await assert.rejects(pngBlob(svg), match);
    }
    values.set("cursor", 'url("https://other.test/cursor.png")');
    assert.doesNotThrow(
      () => snapshotSVG(svg),
      "nonpaint cursor URL is ignored",
    );
    assert.equal(xmlSource.props.has("cursor"), false);
    values.set("fill", 'url("https://other.test/image.svg#gradient")');
    assert.throws(() => snapshotSVG(svg), /external/);
    values.set("fill", "red");
    const image = node("image");
    image.attrs.href = "https://other.test/img.png";
    svg.querySelectorAll = () => [image];
    svg.cloneNode = () => {
      const clone = node();
      clone.querySelectorAll = () => [node("image")];
      return clone;
    };
    assert.throws(() => snapshotSVG(svg), /external image/);
    const html = node("div");
    html.namespaceURI = "http://www.w3.org/1999/xhtml";
    const htmlClone = node("div");
    svg.querySelectorAll = () => [html];
    svg.cloneNode = () => {
      const clone = node();
      clone.querySelectorAll = (selector) =>
        selector === "*" ? [htmlClone] : [];
      return clone;
    };
    values.set("padding-left", "12px");
    values.set("background-image", "none");
    snapshotSVG(svg);
    assert.equal(htmlClone.props.get("padding-left"), "12px");
    assert.equal(htmlClone.props.get("font-family"), "Arial");
    assert.equal(htmlClone.props.has("marker-end"), false);
    values.set("background-image", 'url("https://other.test/image.png")');
    assert.throws(() => snapshotSVG(svg), /external/);
  } finally {
    for (const [name, descriptor] of original) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
});
