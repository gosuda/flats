## JavaScript env.FILES

A per-flat **local-disk string key/value store**, not S3-compatible storage.
It has no buckets, credentials, remote object URLs, metadata/options objects,
streaming, byte retrieval API, or automatic HTTP serving. All four methods
are **synchronous** (ordinary values, not Promises); errors throw JS exceptions.

| Method | Arguments and result | Missing behavior |
|---|---|---|
| `env.FILES.get(key)` | key coerced with `String(key)`; returns stored string | `null` for absent key/store or directory key |
| `env.FILES.put(key, data)` | key coerced to string; replaces entire value atomically; returns `undefined` | creates key and parent directories |
| `env.FILES.delete(key)` | key coerced to string; returns boolean | `false` if absent or not a regular file; `true` if removed |
| `env.FILES.list(prefix = "")` | string-coerced literal prefix; returns sorted array of full relative keys | `[]` if store absent/no matches |

`put` stores text: strings unchanged, null/undefined as empty string, other
nonbinary values via `String(data)` (objects become `[object Object]`, so use
`JSON.stringify`). ArrayBuffer/typed-array views are decoded as UTF-8 where
possible, otherwise converted to a character string; this is **not** a raw
binary round trip. Values travel through JSON and UTF-8 disk bytes. For binary,
store an application-encoded base64 ASCII string and decode it yourself:

```js
const bytes = new Uint8Array([0, 128, 255]);
const encoded = btoa(Array.from(bytes, b => String.fromCharCode(b)).join(""));
env.FILES.put("attachments/demo.b64", encoded);
const value = env.FILES.get("attachments/demo.b64");
const decoded = value === null ? null : Uint8Array.from(atob(value), c => c.charCodeAt(0));
const keys = env.FILES.list("attachments/");
const removed = env.FILES.delete("attachments/demo.b64");
```

Keys for get/put/delete: nonempty valid UTF-8, at most **512 UTF-8 bytes**,
relative slash-separated paths. No leading/trailing slash, empty segments,
`.` or `..` segments, backslash, U+0000–U+001F or U+007F. Each segment must
not start `.flats-tmp-` (reserved for atomic writes).
Key identity follows the host filesystem. Typical Linux ext4 is case-sensitive;
default macOS APFS is case-insensitive and Unicode-normalization-insensitive.
On such APFS volumes, keys differing only by case or NFC/NFD form name the same
value: put can overwrite it, and get/delete can resolve either spelling. List
returns the spelling stored by the filesystem (on default APFS, the first-created
spelling), and matches its prefix literally: get("CASE/a") can find "case/a"
while list("CASE/") is empty. For portable apps use canonical names such as
lowercase ASCII IDs; do not distinguish keys by case or normalization alone.
The 512-byte check is a whole-key validation limit, not a guarantee that the
host filesystem accepts the filename. Each path segment also obeys the host's
name limits: ext4 typically allows **255 UTF-8 bytes** per segment, while APFS
uses different Unicode name semantics (a multibyte segment can exceed 255 bytes).
Overlong segments can make get/put/delete throw filesystem errors even when the
whole key passes validation. Use short segments for portable keys.
A file cannot also be a parent directory: conflicting writes throw disk errors;
get/delete through a regular-file parent (e.g. `a/b` when `a` is a file) also
throw a filesystem "not a directory" error (underlying `ENOTDIR`) instead of
returning null/false; JS error text need not contain the errno name.

List uses a **literal string prefix**, not glob/path normalization: `notes`
matches `notes.txt` and `notes/a`; `notes/` matches only descendants. Its prefix
validation is intentionally looser: at most 512 bytes, no backslash or NUL;
empty and trailing slash are allowed. It returns regular files only, excludes
reserved temporary filenames, and stops after **10,000 keys** in filesystem
walk order **before sorting** the collected keys, with no cursor or truncation
flag. This need not select the lexicographically first 10,000 matching keys.
Traversal failures are skipped, so do not treat a list as a
transactional/complete snapshot of concurrently changing storage.

Each value is capped at **10 MiB (10,485,760 bytes)** of stored UTF-8 text;
base64 overhead counts. This explicit size limit does not guarantee a put/get
will fit in the **48 MiB JS heap**: strings and serialization copies compete
with other live allocations, so operations near 10 MiB can exhaust the heap
and produce a generic 500 even with a value within the limit.
Per-flat FILES total is **1 GiB (1,073,741,824 bytes)**;
overwriting charges the new size minus old size, deletion releases space.
Quota checks/mutations are serialized within a worker; individual put publishes
by rename. Multi-call sequences are not transactions and have no compare/swap.
There is no FILES TTL or versioning. Invalid keys/prefixes, value/quota overflow,
permissions, disk-full and incompatible file/directory paths throw; catch errors
and choose an HTTP response. Error wording can contain the key/OS details;
do not depend on exact text or expose it blindly. Failed atomic put keeps the
old value, but may leave newly created empty parent directories. get/put/delete
can all throw I/O errors; null/false apply only to the missing/directory cases
above, not to arbitrary failures to resolve a path.

Live DB/FILES belong to the flat, shared by its serving request VMs, and survive
successful redeploy, ordinary rollback and host restart on the same data dir.
Deleting the flat deletes its data. Preview uses its own copy. Activation
snapshots capture DB and FILES together (`topic.rollback-data`); versions are
code, not storage backups.
