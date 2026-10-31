// Clipboard (rule 22, TECH 15 and 18). Plain text goes into the block.
// Markdown and HTML from another application are sent as inert text to the
// server's paste route, which converts them with the markdown_to_blocks
// rules; the browser never parses pasted HTML or inserts it into the DOM.
// An image file is uploaded and becomes an image block.
'use strict';


export const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];

// MARKDOWN recognizes text that carries block structure or marks worth
// converting; anything else is inserted as plain text.
const MARKDOWN = /(^|\n)(#{1,3} |[-*] |\d+\. |> |```|---\s*$|- \[[ x]\] |\|.+\|)|\*\*[^*\n]+\*\*|~~[^~\n]+~~|`[^`\n]+`|\[[^\]\n]+\]\([^)\n]+\)/;

// classify decides how a paste is handled from the clipboard's types.
export function classify(data) {
  const types = Array.from(data.types || []);
  const files = Array.from(data.files || []).filter((f) => IMAGE_TYPES.includes(f.type));
  if (files.length) return { kind: 'image', files };
  const html = types.includes('text/html') ? data.getData('text/html') : '';
  const text = types.includes('text/plain') || !types.length ? data.getData('text/plain') : '';
  if (html && /<(p|h[1-6]|ul|ol|li|pre|blockquote|table|strong|b|em|i|a|code|br)\b/i.test(html)) return { kind: 'html', html, text };
  if (text && (text.includes('\n\n') || MARKDOWN.test(text))) return { kind: 'markdown', text };
  return { kind: 'plain', text };
}

const HEADERS = { 'X-Flats-Notebook': '1' };

// convert asks the server to turn pasted Markdown or HTML into a semantic
// fragment for a placement. The response carries the markdown_to_blocks
// operation's source and the deterministic temporary names it creates.
export async function convert(fetchImpl, body) {
  const res = await fetchImpl('/_notebook/api/paste', {
    method: 'POST', credentials: 'same-origin',
    headers: { ...HEADERS, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error((await res.text()) || 'paste was refused');
  return res.json();
}

// upload stages one image and returns its asset hash and extension.
export async function upload(fetchImpl, file) {
  const res = await fetchImpl('/_notebook/api/assets', {
    method: 'POST', credentials: 'same-origin',
    headers: { ...HEADERS, 'Content-Type': file.type },
    body: file,
  });
  if (!res.ok) throw new Error((await res.text()) || 'upload was refused');
  return res.json();
}
