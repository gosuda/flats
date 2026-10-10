// Flats preview check. Run it in your browser tool's JavaScript runner (or the
// DevTools console) on a Draft preview, once per width and color scheme. It
// only reads the rendered page; it cannot see console errors logged earlier.
(() => {
  const root = document.documentElement;
  const vw = root.clientWidth;
  const out = {
    url: location.href,
    viewport: { width: vw, height: root.clientHeight }, // layout viewport in CSS px
    scheme: matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light",
    title: document.title.trim(),
    viewportMeta: /width\s*=\s*device-width/i.test(document.querySelector('meta[name="viewport"]')?.content ?? ""),
    icon: !!document.querySelector('link[rel~="icon"][href]'),
    problems: [],
    consoleErrors: "read them from your browser tool; this script cannot see them",
  };
  const add = (check, detail) => { if (out.problems.length < 40) out.problems.push({ check, ...detail }); };
  const name = (el) => el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") +
    [...el.classList].slice(0, 2).map((c) => "." + c).join("");
  const style = (el) => getComputedStyle(el);
  const canvas = document.createElement("canvas").getContext("2d", { willReadFrequently: true });
  const rgba = (color) => { // any CSS color (oklch, color(), named) to [r, g, b, a]
    canvas.clearRect(0, 0, 1, 1);
    canvas.fillStyle = "#000"; canvas.fillStyle = color; canvas.fillRect(0, 0, 1, 1);
    const [r, g, b, a] = canvas.getImageData(0, 0, 1, 1).data;
    return [r, g, b, a / 255];
  };
  const lum = ([r, g, b]) => [r, g, b].map((v) => (v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4)
    .reduce((s, v, i) => s + v * [0.2126, 0.7152, 0.0722][i], 0);
  const over = (under, [r, g, b, a]) => [r * a + under[0] * (1 - a), g * a + under[1] * (1 - a), b * a + under[2] * (1 - a), 1];
  const mixed = (a, b, t) => a.map((v, k) => v * (1 - t) + b[k] * t);
  // The pixel colors behind and of el's text, composed the way CSS does: each element's background
  // and content form a group that is faded onto its backdrop by its opacity. null when a background
  // image is in the way or no element in the chain paints an opaque background.
  const colors = (el, text) => {
    const chain = [];
    for (let e = el; e; e = e.parentElement) {
      const s = style(e);
      if (s.backgroundImage !== "none") return null;
      chain.push({ bg: rgba(s.backgroundColor), opacity: Number(s.opacity) });
    }
    if (!chain.some((c) => c.bg[3] > 0.99)) return null;
    const dark = /dark/.test(style(root).colorScheme) && out.scheme === "dark";
    const canvasColor = dark ? [18, 18, 18, 1] : [255, 255, 255, 1];
    const render = (withText) => {
      const paint = (i, backdrop) => {
        const base = over(backdrop, chain[i].bg);
        const content = i > 0 ? paint(i - 1, base) : withText ? over(base, text) : base;
        return mixed(backdrop, content, chain[i].opacity);
      };
      return paint(chain.length - 1, canvasColor);
    };
    return { bg: render(false), fg: render(true) };
  };
  const painted = (el) => { const s = style(el); return s.backgroundImage !== "none" || rgba(s.backgroundColor)[3] > 0.99; };

  if (!out.title) add("title", { fix: "name the page in <title>" });
  if (!out.viewportMeta) add("viewport", { fix: 'use <meta name="viewport" content="width=device-width, initial-scale=1">; without width=device-width a phone lays the page out at a fixed width (about 980px), so the overflow check below does not apply' });
  if (!painted(document.body) && !painted(root)) add("background", { fix: "paint body from a color token in both schemes" });

  if (root.scrollWidth > vw + 1) {
    const clipped = (el) => { for (let e = el.parentElement; e && e !== root; e = e.parentElement) {
      if (["auto", "scroll", "hidden", "clip"].includes(style(e).overflowX)) return true; } return false; };
    const wide = [...document.body.querySelectorAll("*")].slice(0, 3000)
      .filter((el) => el.getBoundingClientRect().right > vw + 1 && !clipped(el));
    const outer = wide.filter((el) => !wide.includes(el.parentElement)).slice(0, 8);
    add("overflow", { scrollWidth: root.scrollWidth, width: vw, elements: outer.map(name),
      fix: "let rows wrap, give text children min-width: 0, put wide tables/code in an overflow-x: auto box" });
  }

  for (const img of document.images) {
    if (img.complete && img.naturalWidth === 0 && img.currentSrc) add("image", { src: img.currentSrc });
    else if (!img.complete && img.loading === "lazy") add("lazy-image", { src: img.getAttribute("src") ?? img.getAttribute("srcset"),
      fix: "not requested yet: scroll to it, or fetch its URL, before calling images fine" });
  }
  for (const link of document.querySelectorAll('link[rel~="stylesheet"][href]')) {
    if (!link.sheet && !link.disabled) add("stylesheet", { href: link.href });
  }
  for (const r of performance.getEntriesByType("resource")) {
    if (r.responseStatus >= 400) add("resource", { url: r.name, status: r.responseStatus });
  }

  const faded = (el) => { // opacity does not inherit, so check every ancestor
    for (let e = el; e; e = e.parentElement) if (Number(style(e).opacity) === 0) return e;
    return null;
  };
  const hidden = new Set();
  const pairs = new Set();
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT,
    { acceptNode: (n) => n.data.trim() ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT });
  for (let n, i = 0; (n = walker.nextNode()) && i < 500; i++) {
    const el = n.parentElement;
    const s = style(el);
    const range = document.createRange();
    range.selectNodeContents(n);
    const rect = range.getBoundingClientRect(); // the text's own box: display: contents wrappers have none
    if (rect.width === 0 || rect.height === 0 || el.closest("[hidden], [aria-hidden=true]")) continue;
    const parked = s.visibility === "hidden" ? el : faded(el);
    if (parked) {
      if (!hidden.has(parked)) { hidden.add(parked); add("hidden-text", { element: name(parked), text: n.data.trim().slice(0, 40),
        fix: "readable content should be visible at rest, not parked for a scroll observer; ignore closed menus and tooltips" }); }
      continue;
    }
    const px = colors(el, rgba(s.color));
    if (!px) continue;
    const bg = px.bg;
    const [hi, lo] = [lum(px.fg), lum(bg)].sort((a, b) => b - a);
    const ratio = (hi + 0.05) / (lo + 0.05);
    const large = parseFloat(s.fontSize) >= 24 || (parseFloat(s.fontSize) >= 18.66 && Number(s.fontWeight) >= 700);
    const key = s.color + "|" + bg.map(Math.round).join();
    if (ratio < (large ? 3 : 4.5) && !pairs.has(key)) {
      pairs.add(key);
      add("contrast", { element: name(el), text: n.data.trim().slice(0, 40), ratio: Math.round(ratio * 100) / 100,
        fix: "use color tokens that keep 4.5:1 (3:1 for large text) in this scheme" });
    }
  }
  return out;
})()
