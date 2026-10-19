// Element and group ids, and fractional stacking keys (TECH 3.1, Appendix A).

const DIGITS = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz';

function randomBase62(n) {
  const bytes = new Uint8Array(n * 2);
  crypto.getRandomValues(bytes);
  let out = '';
  for (let i = 0; out.length < n; i++) {
    if (i === bytes.length) { crypto.getRandomValues(bytes); i = 0; }
    // 248 = 4 * 62: rejecting larger bytes keeps every digit equally likely.
    if (bytes[i] < 248) out += DIGITS[bytes[i] % 62];
  }
  return out;
}

export const newId = () => 'e_' + randomBase62(10);
export const newGroupId = () => 'g_' + randomBase62(10);

const digit = (c) => DIGITS.indexOf(c);

// midpoint returns a base62 fraction strictly between a and b, where a is ''
// (zero) or a key and b is null (one) or a key greater than a. Keys never end
// in '0', so every key has a successor and a predecessor.
function midpoint(a, b) {
  if (b != null) {
    let n = 0;
    while ((a[n] || '0') === b[n]) n++;
    if (n > 0) return b.slice(0, n) + midpoint(a.slice(n), b.slice(n));
  }
  const da = a ? digit(a[0]) : 0;
  const db = b != null ? digit(b[0]) : 62;
  if (db - da > 1) return DIGITS[(da + db) >> 1];
  if (b != null && b.length > 1) return b.slice(0, 1);
  return DIGITS[da] + midpoint(a.slice(1), null);
}

// keyBetween returns a stacking key between a and b; either may be null for
// "before everything" or "after everything".
export function keyBetween(a, b) {
  if (a != null && b != null && a >= b) throw new Error(`keyBetween: ${a} >= ${b}`);
  return midpoint(a || '', b ?? null);
}

// keysBetween returns n increasing keys after a (and before b when given).
export function keysBetween(a, b, n) {
  const out = [];
  let prev = a;
  for (let i = 0; i < n; i++) {
    const k = keyBetween(prev, b);
    out.push(k);
    prev = k;
  }
  return out;
}

// zCompare orders elements by (z, id), the full stacking order every client draws.
export function zCompare(a, b) {
  if (a.z !== b.z) return a.z < b.z ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}
