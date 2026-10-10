## Manifest (`flats.json`)

`flats.json` is optional and sits at the bundle root. Unknown fields are
rejected; every problem is reported at once with a fix.

```json
{ "type": "flat", "name": "My blog", "kind": "static", "entry": "index.html",
  "spa": false, "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

| Field | Meaning |
|---|---|
| `type` | `flat` (default, a website) or `docs` (Markdown documents, `topic.docs`) |
| `name` | display name |
| `kind` | `static` (default) or `server` (`topic.server`) |
| `entry` | static: default `index.html`; server: first of `server.js`, `index.js`, `main.wasm`, `server.wasm` |
| `spa` | static only: serve the entry for unknown paths |
| `not_found` | static only: file served with status 404 |
| `health` | URL path checked before activation, default `/`; use a side-effect-free path such as `/healthz` |
| `screenshot` | image path shown as the thumbnail in the operator's console |

An upload with no manifest `type`, no `kind`, no `index.html` and no server
entry, but with a Markdown entry candidate, is a `docs` upload. `type: "docs"`
has its own rules for `entry`, `name`, `kind`, `spa`, `not_found` and `health`
(`topic.docs`).
