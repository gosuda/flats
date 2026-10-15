## Static sites

A static flat serves the uploaded files as they are. Build first, then upload
the build output (`dist/`, `out/`, `build/`), never sources or `node_modules`.
Before writing pages, read `topic.design`: the page contract (title,
viewport, icon, both color schemes, phone width), design defaults and the
asset policy (bundle by default; CDN only with an exact version).

* The entry defaults to `index.html` at the bundle root. One wrapping
  directory such as `dist/` is stripped.
* `spa: true` serves the entry for unknown paths (client-side routers);
  `not_found: "404.html"` serves that file with status 404 (`topic.manifest`).
* Each flat is its own origin (`<slug>` host), so root-relative asset paths
  work. Static flats need no runtime and also work on hosts started with
  `--runtime=false`.

### Save

`save_draft` accepts `slug`, `files: [{path, content,
encoding}]`, optional `expected_revision`, `message`, `git_sha` and `git_dirty`. Send the **complete** build every time: the
Draft is replaced, not patched. Use `utf8` for text and RFC 4648 `base64` for
binary files. A missing flat is created on the first save. A minimal call:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}]}
```

Saving never creates a version or approval. Call `create_version {slug,
revision, hash}` separately to request approval to freeze the Draft as a numbered
version (`v1`, `v2`, ...). After approval creates `vN`, call `deploy {slug,
version: N}` and obtain another approval to make it live (`topic.approvals`). `expected_revision` is the Draft revision you
expect (0 when no Draft exists); a mismatch is refused as `conflict` and
overwrites nothing. Read `get_draft`, reconcile, then save again.

A successful save may return `warnings: [{path, message, fix}]`: a missing
title or viewport, an unpinned external script, a reference to a file that is
not in the upload, a large image, no favicon or screenshot. They never block
the save; fix them and save again (`topic.design` lists every check).

On the Flats host itself, `save_draft_from_dir {slug, dir}` uploads an
absolute build directory (loopback callers only; it also skips
`node_modules`). Remote agents send files inline. The CLI flow is
`flats save-draft <slug> <dir>`, then `flats create-version <slug>` and its
approval, then `flats deploy --flat <slug> --version <n>` and another approval.
`deploy` never uploads or creates a version; directory arguments are refused.

### Upload limits

Upload size is operator-configurable (default **20 MiB** uncompressed; the
current value is in the MCP instructions and `get_runtime_reference`); max
**20,000 upload files**, no symlinks, special files or paths outside the root.
`.git` and `.DS_Store` are skipped. Every validation problem is reported at
once with its path and a fix (`refusal.invalid`). Upload encoding is separate
from the text encoding of `env.FILES`.

### Version retention and quotas

Versions are never deleted automatically, including after lowering a quota.
Each flat follows the host's version quota (default 20), unless the operator
sets an override in the console; 0 means unlimited. `get_flat` reports
`version_count`, `version_quota` (null follows the host) and
`effective_version_quota`. A full quota refuses creation both when requested
and when approved; delete older versions or raise the quota. Existing pruned
records are unavailable and do not count. Disk quota is separate and never
causes automatic deletion.

`delete_version {slug, version}` requests operator approval; share its
`approval_url` and wait. The live version cannot be deleted and is checked
again at approval. Deletion closes its previews first, removes its files and
frees one quota slot. It keeps the flat's data snapshots. Operators may delete
directly in the console. Version numbers only increase and are never reused.

A save returns `draft` metadata of that exact save: `revision` and `hash` are
the authoritative target for `create_version`, even if another editor saves
later. `saved_at` is this save's time. It has no `version` or `deploy` object.
`get_draft` reads the current working copy, which additionally reports mutable
`dirty`, `base_version`, `created_at` and `updated_at` metadata. Reconcile
concurrent edits before saving again; a changed Draft makes an old creation
request stale.

An upgraded host's explicit old `keep_versions: N` becomes a quota of N total
available versions, including live, rather than N historical versions plus
live. Existing versions stay; a full quota refuses creation until space is
made or the operator raises it. Quota 1 therefore blocks creating a replacement
for a live version.

A deletion receipt may report `cleanup_pending: true` (with direct status
`cleanup_pending` or in an approved request's `result_data`). The version has
already been deleted, but durable storage cleanup is incomplete. While its
`.version-delete-N` staging folder remains, guarded storage changes are blocked.
A pending receipt alone is not a universal lock: if no staged files remain after
a sync failure, actions such as renaming, deleting the flat or reusing its slug may
still be admitted. Report its message; do not retry the version deletion.
The operator must repair the storage issue and restart Flats for recovery.
If recovery still fails, host startup may fail rather than discard data.
