// Image-mode SVG cannot fetch external styles/images. Snapshot resolved styles
// and use a data URL so foreignObject labels remain origin-clean in canvas.
// https://developer.mozilla.org/en-US/docs/Web/SVG/Guides/SVG_as_an_image
const maxSide = 8192,
  maxPixels = 16 * 1024 * 1024;
export function pngSize(width, height) {
  if (![width, height].every((n) => Number.isFinite(n) && n > 0))
    throw Error("The diagram has no exportable size.");
  const scale = Math.min(
    2,
    maxSide / width,
    maxSide / height,
    Math.sqrt(maxPixels / width / height),
  );
  return {
    width: Math.max(1, Math.floor(width * scale)),
    height: Math.max(1, Math.floor(height * scale)),
  };
}
function localResources(value, base) {
  return value.replace(/url\(\s*(["']?)(.*?)\1\s*\)/gi, (_, quote, url) => {
    if (url.startsWith("#") || url.startsWith("data:")) return `url("${url}")`;
    const target = new URL(url, base);
    if (target.hash && target.href.split("#")[0] === base.split("#")[0])
      return `url("${target.hash}")`;
    throw Error(
      "This diagram uses an external image or font. Save SVG instead.",
    );
  });
}
// Paint and label layout only: do not serialize hundreds of browser longhands,
// interaction cursors or app custom properties into every diagram element.
const textProperties =
  "color font-family font-size font-style font-weight font-variant font-kerning letter-spacing word-spacing line-height text-decoration text-transform text-align white-space word-break overflow-wrap direction writing-mode".split(
    " ",
  );
const svgProperties =
  "display visibility opacity fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-dasharray stroke-dashoffset stroke-linecap stroke-linejoin stroke-miterlimit paint-order marker-start marker-mid marker-end clip-path mask filter text-anchor dominant-baseline alignment-baseline baseline-shift vector-effect shape-rendering text-rendering transform transform-origin transform-box".split(
    " ",
  );
const htmlProperties =
  "display visibility opacity box-sizing width height min-width max-width min-height max-height padding-top padding-right padding-bottom padding-left margin-top margin-right margin-bottom margin-left border-top-width border-right-width border-bottom-width border-left-width border-style border-color border-radius background-color background-image vertical-align overflow position top left right bottom flex-direction align-items justify-content gap".split(
    " ",
  );
export function snapshotSVG(svg) {
  const rect = svg.getBoundingClientRect(),
    box = svg.viewBox?.baseVal;
  const width = box?.width || rect.width,
    height = box?.height || rect.height;
  const size = pngSize(width, height),
    clone = svg.cloneNode(true);
  const originals = [svg, ...svg.querySelectorAll("*")],
    copies = [clone, ...clone.querySelectorAll("*")];
  if (originals.length > 10000)
    throw Error(
      "This diagram is too complex for PNG export. Save SVG instead.",
    );
  for (let i = 0; i < originals.length; i++) {
    const source = originals[i],
      target = copies[i];
    const style = getComputedStyle(source);
    const properties =
      source.namespaceURI === "http://www.w3.org/1999/xhtml"
        ? [...textProperties, ...htmlProperties]
        : [...textProperties, ...svgProperties];
    for (const property of properties)
      target.style.setProperty(
        property,
        localResources(style.getPropertyValue(property), document.baseURI),
      );
    // Embedded images are allowed; external image-mode fetches are not.
    if (["image", "img", "use"].includes(source.localName)) {
      const href =
        source.getAttribute("href") ||
        source.getAttribute("xlink:href") ||
        source.getAttribute("src");
      if (href && !href.startsWith("#") && !href.startsWith("data:"))
        throw Error("This diagram uses an external image. Save SVG instead.");
    }
  }
  for (const style of clone.querySelectorAll("style")) style.remove();
  clone.setAttribute("xmlns", "http://www.w3.org/2000/svg");
  clone.setAttribute("width", String(width));
  clone.setAttribute("height", String(height));
  clone.style.width = width + "px";
  clone.style.height = height + "px";
  clone.style.maxWidth = "none";
  clone.style.maxHeight = "none";
  const xml = new XMLSerializer().serializeToString(clone);
  if (xml.length > 16 * 1024 * 1024)
    throw Error("This diagram is too large for PNG export. Save SVG instead.");
  return {
    xml,
    ...size,
    background:
      getComputedStyle(svg).getPropertyValue("--paper").trim() || "#fff",
  };
}
export async function pngBlob(svg) {
  if (document.fonts?.ready) {
    let timer;
    try {
      await Promise.race([
        document.fonts.ready,
        new Promise((_, reject) => {
          timer = setTimeout(
            () =>
              reject(Error("Fonts are still loading. Try PNG again shortly.")),
            15000,
          );
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  }
  if (svg.isConnected === false)
    throw Error(
      "The diagram changed while preparing the image. Try Save PNG again.",
    );
  const snapshot = snapshotSVG(svg);
  const image = new Image();
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      image.src = "";
      reject(Error("PNG rendering timed out. Try Save SVG instead."));
    }, 15000);
    const finish = (error) => {
      clearTimeout(timer);
      image.onload = image.onerror = null;
      error ? reject(error) : resolve();
    };
    image.onload = () => finish();
    image.onerror = () =>
      finish(
        Error(
          "The browser could not render this diagram as PNG. Save SVG instead.",
        ),
      );
    image.src =
      "data:image/svg+xml;charset=utf-8," + encodeURIComponent(snapshot.xml);
  });
  const canvas = document.createElement("canvas");
  canvas.width = snapshot.width;
  canvas.height = snapshot.height;
  try {
    const context = canvas.getContext("2d");
    if (!context)
      throw Error(
        "PNG export is unavailable in this browser. Save SVG instead.",
      );
    context.fillStyle = snapshot.background;
    context.fillRect(0, 0, canvas.width, canvas.height);
    context.drawImage(image, 0, 0, canvas.width, canvas.height);
    return await new Promise((resolve, reject) => {
      canvas.toBlob(
        (blob) =>
          blob?.type === "image/png"
            ? resolve(blob)
            : reject(Error("PNG encoding failed. Save SVG instead.")),
        "image/png",
      );
    });
  } catch (error) {
    if (error?.name === "SecurityError")
      throw Error(
        "This browser cannot export this diagram's HTML labels as PNG. Save SVG instead.",
      );
    throw error;
  } finally {
    canvas.width = canvas.height = 0;
    image.src = "";
  }
}
export function saveBlob(blob, filename) {
  const url = URL.createObjectURL(blob),
    link = document.createElement("a");
  link.href = url;
  link.download = filename;
  document.body.append(link);
  try {
    link.click();
  } finally {
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
}
