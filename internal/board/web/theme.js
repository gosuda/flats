// Theme tokens for the canvas (TECH 11.4). Elements store semantic color
// names; this table maps them to light and dark values. The CSS custom
// properties in app.css hold the same UI tokens for the DOM.

export const THEMES = {
  light: {
    paper: '#ffffff', canvas: '#f4f4f0', ink: '#242924', muted: '#566056', line: '#dce0d8', by: '#454d45',
    accent: '#356649', sel: '#2f7a52', dot: '#d6dad2', guide: '#c2266b', labelBg: '#f4f4f0',
    frameLine: 'rgba(36,41,36,0.22)', frameAlpha: 0.55, shadow: 'rgba(30,40,30,0.10)',
    sticky: { yellow: '#fbe6a2', orange: '#fbd3b2', pink: '#f6cfdc', blue: '#cfe0f6', green: '#d3eacf', gray: '#e4e4df' },
    fill: { white: '#ffffff', gray: '#e4e4df' },
    inks: { ink: '#242924', gray: '#5a615a', red: '#b42318', orange: '#a8460c', green: '#2b6f4b', blue: '#1d5fd1', purple: '#7139ad' },
    highlight: 'rgba(233,185,73,0.5)',
  },
  dark: {
    paper: '#1c211e', canvas: '#151916', ink: '#e3e9e2', muted: '#a6b1a6', line: '#353d36', by: '#d3dbd2',
    accent: '#95c6a5', sel: '#8fd0a8', dot: '#2a312b', guide: '#ff7fb5', labelBg: '#151916',
    frameLine: 'rgba(227,233,226,0.24)', frameAlpha: 0.55, shadow: 'rgba(0,0,0,0.35)',
    sticky: { yellow: '#5b4f22', orange: '#5e4027', pink: '#5a3342', blue: '#2c4462', green: '#2f4a33', gray: '#3a3d39' },
    fill: { white: '#262c28', gray: '#3a3d39' },
    inks: { ink: '#e3e9e2', gray: '#a6b1a6', red: '#ff9a8a', orange: '#f5a35c', green: '#86cf9f', blue: '#8ab4ff', purple: '#c4a6ff' },
    highlight: 'rgba(201,162,39,0.5)',
  },
};

export function stickyColor(t, name) { return t.sticky[name] || t.sticky.yellow; }
export function inkColor(t, name) {
  if (name === 'highlight') return t.highlight;
  return t.inks[name] || t.inks.ink;
}
// shapeFill returns the fill of a shape, or null for "none".
export function shapeFill(t, name) {
  if (name === 'none') return null;
  return t.fill[name] || t.sticky[name] || t.fill.white;
}
export function frameFill(t, name) {
  if (name === 'none') return null;
  return blend(t.fill[name] || t.fill.white, t.canvas, t.frameAlpha);
}

// ---- color math (WCAG 2.x relative luminance and contrast) ----

function parse(hex) {
  const h = hex.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
}
const toHex = (rgb) => '#' + rgb.map((v) => Math.round(v).toString(16).padStart(2, '0')).join('');

export function blend(fg, bg, alpha) {
  const a = parse(fg), b = parse(bg);
  return toHex(a.map((v, i) => v * alpha + b[i] * (1 - alpha)));
}

export function luminance(hex) {
  const [r, g, b] = parse(hex).map((v) => {
    const c = v / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrast(a, b) {
  const la = luminance(a), lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

export const currentTheme = () =>
  (typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches ? THEMES.dark : THEMES.light);
