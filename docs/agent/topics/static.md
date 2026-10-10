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

`save_draft` and `save_version` accept `slug`, `files: [{path, content,
encoding}]`, optional `expected_revision`, `message`, `git_sha`, `git_dirty`
and `deploy` (default false). Send the **complete** build every time: the
Draft is replaced, not patched. Use `utf8` for text and RFC 4648 `base64` for
binary files. A missing flat is created on the first save. A minimal call:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}],"deploy":true}
```

Saving never publishes: `deploy: true` only requests publish approval after
the save (`topic.approvals`). `expected_revision` is the Draft revision you
expect (0 when no Draft exists); a mismatch is refused as `conflict` and
overwrites nothing. Read `get_draft`, reconcile, then save again.

A successful save may return `warnings: [{path, message, fix}]`: a missing
title or viewport, an unpinned external script, a reference to a file that is
not in the upload, a large image, no favicon or screenshot. They never block
the save; fix them and save again (`topic.design` lists every check).

On the Flats host itself, `save_version_from_dir {slug, dir}` uploads an
absolute build directory (loopback callers only; it also skips
`node_modules`). The CLI `flats deploy <dir> --flat <slug> --save-only` saves
the Draft the same way; without `--save-only` it also requests publish
approval. Remote agents send files inline.

### Upload limits

Upload size is operator-configurable (default **20 MiB** uncompressed; the
current value is in the MCP instructions and `get_runtime_reference`); max
**20,000 upload files**, no symlinks, special files or paths outside the root.
`.git` and `.DS_Store` are skipped. Every validation problem is reported at
once with its path and a fix (`refusal.invalid`). Upload encoding is separate
from the text encoding of `env.FILES`.
