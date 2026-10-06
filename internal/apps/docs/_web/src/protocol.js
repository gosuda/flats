import * as enc from "lib0/encoding";
import * as dec from "lib0/decoding";
export const LIMITS = Object.freeze({
  frame: 512 * 1024,
  update: 256 * 1024,
  awareness: 8192,
  text: 1024 * 1024,
  state: 1536 * 1024,
  rooms: 8,
  roomBytes: 4 * 1024 * 1024,
  connections: 200,
  perRoom: 50,
  receipts: 100000,
  structs: 20000,
  roomStructs: 30000,
  documents: 128,
  totalReceipts: 200000,
  activations: 1000,
  conflicts: 8,
  conflictBytes: 2 * 1024 * 1024,
  sendBytes: 4 * 1024 * 1024,
  flatSendBytes: 16 * 1024 * 1024,
});
const alphabet =
  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
const reverse = new Int16Array(128).fill(-1);
for (let i = 0; i < 64; i++) reverse[alphabet.charCodeAt(i)] = i;
export function base64(bytes) {
  if (globalThis.__flats_docsCodec)
    return globalThis.__flats_docsCodec.encode(bytes);
  const chunks = [];
  let part = "";
  for (let i = 0; i < bytes.length; i += 3) {
    const n =
      (bytes[i] << 16) | ((bytes[i + 1] || 0) << 8) | (bytes[i + 2] || 0);
    part +=
      alphabet[n >>> 18] +
      alphabet[(n >>> 12) & 63] +
      (i + 1 < bytes.length ? alphabet[(n >>> 6) & 63] : "=") +
      (i + 2 < bytes.length ? alphabet[n & 63] : "=");
    if (part.length >= 128) {
      chunks.push(part);
      part = "";
    }
  }
  chunks.push(part);
  return chunks.join("");
}
export function unbase64(s, max = LIMITS.update) {
  if (
    typeof s !== "string" ||
    s.length > Math.ceil(max / 3) * 4 ||
    s.length % 4 ||
    (!globalThis.__flats_docsCodec && !/^[A-Za-z0-9+/]*={0,2}$/.test(s))
  )
    throw new Error("invalid base64");
  const padding = s.endsWith("==") ? 2 : s.endsWith("=") ? 1 : 0,
    length = (s.length / 4) * 3 - padding;
  if (length > max) throw new Error("payload too large");
  if (
    padding &&
    reverse[s.charCodeAt(s.length - padding - 1)] & (padding === 2 ? 15 : 3)
  )
    throw new Error("noncanonical base64");
  if (globalThis.__flats_docsCodec)
    return globalThis.__flats_docsCodec.decode(s);
  const bytes = new Uint8Array(length);
  let pos = 0;
  for (let i = 0; i < s.length; i += 4) {
    const n =
      (reverse[s.charCodeAt(i)] << 18) |
      (reverse[s.charCodeAt(i + 1)] << 12) |
      ((i + 2 < s.length - padding ? reverse[s.charCodeAt(i + 2)] : 0) << 6) |
      (i + 3 < s.length - padding ? reverse[s.charCodeAt(i + 3)] : 0);
    bytes[pos++] = n >>> 16;
    if (pos < length) bytes[pos++] = n >>> 8;
    if (pos < length) bytes[pos++] = n;
  }
  return bytes;
}
export function utf8Bytes(s) {
  if (s.length > 65536 && globalThis.__flats_docsCodec)
    return globalThis.__flats_docsCodec.length(s);
  if (/^[\x00-\x7f]*$/.test(s)) return s.length;
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 128) n++;
    else if (c < 2048) n += 2;
    else if (
      c >= 0xd800 &&
      c <= 0xdbff &&
      i + 1 < s.length &&
      s.charCodeAt(i + 1) >= 0xdc00 &&
      s.charCodeAt(i + 1) <= 0xdfff
    ) {
      n += 4;
      i++;
    } else n += 3;
  }
  return n;
}
export function parseFrame(s) {
  if (
    typeof s !== "string" ||
    s.length > LIMITS.frame ||
    utf8Bytes(s) > LIMITS.frame
  )
    throw new Error("frame too large");
  const m = JSON.parse(s);
  if (!m || Array.isArray(m) || typeof m.t !== "string")
    throw new Error("invalid envelope");
  return m;
}
export function decodePresence(bytes) {
  const d = dec.createDecoder(bytes),
    n = dec.readVarUint(d);
  if (n !== 1) throw new Error("one awareness id per connection");
  const id = dec.readVarUint(d),
    clock = dec.readVarUint(d),
    state = JSON.parse(dec.readVarString(d));
  if (
    d.pos !== bytes.length ||
    !Number.isSafeInteger(id) ||
    !Number.isSafeInteger(clock) ||
    (state !== null && (typeof state !== "object" || Array.isArray(state)))
  )
    throw new Error("invalid awareness");
  return { id, clock, state };
}
export function encodePresence(items) {
  const e = enc.createEncoder();
  enc.writeVarUint(e, items.length);
  for (const { id, clock, state } of items) {
    enc.writeVarUint(e, id);
    enc.writeVarUint(e, clock);
    enc.writeVarString(e, JSON.stringify(state));
  }
  return enc.toUint8Array(e);
}
export const cleanName = (name) =>
  (typeof name === "string" ? name : "Guest")
    .replace(/[\u0000-\u001f\u007f]/g, "")
    .trim()
    .slice(0, 64) || "Guest";
// Relay only valid Yjs relative positions for the single Markdown text.
export function safeCursor(cursor) {
  if (cursor == null) return null;
  const id = (value) =>
    value == null
      ? null
      : typeof value === "object" &&
          Number.isSafeInteger(value.client) &&
          value.client >= 0 &&
          Number.isSafeInteger(value.clock) &&
          value.clock >= 0
        ? { client: value.client, clock: value.clock }
        : (() => {
            throw new Error("invalid cursor id");
          })();
  const position = (p) => {
    if (
      !p ||
      typeof p !== "object" ||
      Array.isArray(p) ||
      (p.tname != null && p.tname !== "markdown") ||
      (p.assoc != null && !Number.isSafeInteger(p.assoc))
    )
      throw new Error("invalid cursor position");
    return {
      type: id(p.type),
      tname: p.tname ?? null,
      item: id(p.item),
      assoc: p.assoc ?? 0,
    };
  };
  return { anchor: position(cursor.anchor), head: position(cursor.head) };
}
