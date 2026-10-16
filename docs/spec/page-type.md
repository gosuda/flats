# Page content type

Interface update (Version-model completion): current MCP tools save Private
Drafts with `save_draft`, `save_draft_from_dir` or `save_page` without any
creation/deployment shortcut. `create_version` requests explicit approval to
freeze the Draft as a numbered version; `deploy` requires that positive
version and separate approval. Removed `save_version`,
`save_version_from_dir` and `publish` names below describe the earlier
implementation only. Docs bundles now use `save_document` with
`replace_bundle: true` and complete `files`. CLI uses `save-draft`,
`create-version`, then `deploy --version`; saved Draft output is `draft`,
with no synthetic version 0. The current interface is detailed in
[content-models.md](content-models.md).


## Summary

A **page** (`type: "page"`) is a static document written as one HTML page. It
is for content that reads like a document but needs more than Markdown:
charts, graphs, interactive visualizations and free layout. A page has the
same lifecycle as a website flat (Private Draft, numbered versions, explicit
publish with operator approval, rollback) and is served as static files. It
has no server code, database or file storage.

The three content types then have distinct roles:

| Type | Written by | Content | Changes reach the URL |
|---|---|---|---|
| `docs` | people and agents together | Markdown with Mermaid diagrams | at once (collaborative editor) |
| `page` | an agent | one HTML page with charts and visualizations | after an approved publish |
| `flat` | an agent | a website or app (static or server) | after an approved publish |

## Behavior

1. **Explicit type.** A bundle is a page only when its `flats.json` says
   `"type": "page"`. `save_page` and `create_flat {type: "page"}` write that
   manifest themselves; every other save (`save_draft`, `save_version`,
   `save_version_from_dir`, `flats deploy`) must include it. Detection of
   other uploads is unchanged: a bundle without a manifest type is still a
   website (or a document, by the existing Markdown rule), so such a save into
   a page flat is refused by rule 7.
2. **One HTML document.** A page bundle has exactly one HTML file (`.html` or
   `.htm`), its entry. The entry defaults to `index.html`; `entry` may name
   another HTML file. Every other file is an asset the page uses: images,
   SVG, CSS, JavaScript, fonts, and data files such as CSV or JSON for charts.
   A second HTML file is refused with a fix (inline it, or use a website flat
   for several pages).
3. **Static only.** A page is always served exactly like a static website
   flat without `spa`, by the same static file server: `/` serves the entry,
   bundled files are served at their paths (with that server's existing
   extensionless and directory-index lookups, and never `flats.json`), and
   other paths are 404. The manifest fields `kind` (other than `"static"`),
   `spa`, `not_found` and `health` are refused for pages, each with a fix.
   `name`, `entry` and `screenshot` work as for websites. Pages work on hosts
   started with `--runtime=false`.
4. **Same lifecycle as websites.** Saving a page (`save_page`, `save_draft`,
   `save_version`, `save_version_from_dir`, `flats deploy`) only replaces its
   Private Draft. `open_preview`, `publish`, `deploy`, `rollback`,
   `list_versions`, `get_draft`, retention, visibility and deletion behave
   exactly as for website flats, including every operator approval.
   A page has no database, so `rollback` with `restore_data` is refused
   (`refusal.conflict`, no snapshot) before any approval is requested, as for
   any static flat; a plain `rollback` works.
5. **Create.** `create_flat {slug, name?, type: "page"}` creates an
   unpublished Private flat and saves a starter Draft (revision 1): an
   `index.html` with the name as `<title>` and heading, a viewport tag and
   light/dark color tokens, plus `flats.json` with `type: "page"`. Nothing is
   published. The agent replaces the starter with its own page.
6. **`save_page`.** `save_page {slug, html, title?, screenshot?, files?,
   expected_revision?, message?, deploy?}` saves a page as a Private Draft in
   one call. `html` (non-empty) becomes the entry `index.html`; `title`, when
   given, becomes the manifest `name`; `files` lists the assets
   (`[{path, content, encoding}]`, as in `save_draft`) and may be omitted or
   empty for a self-contained page; `screenshot`, when given, names one of
   those files as the console thumbnail. Flats writes `flats.json` itself.
   Like `save_draft`, it replaces the whole Draft: send every asset and the
   screenshot each time, or the next Draft has none. `files` must not contain
   `flats.json` or an HTML file. A missing flat is created. `expected_revision`
   and `deploy` behave as in `save_draft`; `deploy: true` only requests
   publish approval.
7. **A slug keeps its type.** Once a flat has a type, a save of another type
   is refused (`refusal.conflict`); the refusal names the flat's type and the
   save's, so an agent that forgot `"type": "page"` can add it, and otherwise
   uses another slug. A flat's type is the type of its live version, or of
   its Draft while nothing is live, the rule `get_flat` already reports. A
   flat created without a type (`create_flat` for a website) takes the type
   of its first save. This extends the existing website/document rule, which
   applied only to a running flat, to unpublished Drafts and to the new type.
   Content stored by earlier releases may still hold a Draft or version of
   another type than the live one (for example a docs Draft left over a
   running website); publishing, activating or rolling back to such content
   is refused at request time and again when an approval is applied. Nothing
   is migrated or deleted.
8. **Save warnings.** Page saves return the same non-blocking `warnings` as
   static websites (title, viewport, favicon, screenshot, unpinned CDN code,
   missing files, large images; `topic.design`). Pages additionally warn when
   the page loads data from another origin while it renders (for example
   `fetch("https://...")` or `d3.csv("https://...")`, detected from URL
   literals in the page's inline and bundled scripts): the published version
   then no longer fixes what readers see, and the page breaks offline.
   Bundle the data, or embed it in the HTML.
9. **Agent guidance.** A new guide topic `topic.page` explains when to choose
   a page instead of a document or a website, the bundle rules, `save_page`,
   chart and data practice (bundle the chart library or pin an exact CDN
   version; bundle or inline the data; give every chart a text alternative
   and readable colors in both schemes) and the website lifecycle it shares.
   A second topic, `topic.page.visuals`, covers charts and diagrams: choosing
   the form from the data's job, color by role, marks and labels, hover with
   tap and keyboard equivalents, data tables, and diagrams of mechanisms.
   `topic.index` routes "write a rich HTML document with charts or
   visualizations" to both. The content-types reference
   (`flats://docs/content-types/v1`, `get_content_types`) gains a page
   section, and `get_content_types` lists the type.
10. **Console.** The console names the type "Page" and shows a `</>` code
    icon with its own color in the flat list, grid and detail header.
    Pages show the website screens (deployments, versions, previews,
    publish) but not the server-only parts: no Database tab and no ordinary
    environment variable, secret or server HTTP(S) permission sections.
11. **CLI and API.** `flats ls`, `flats status`, `flats versions` and
    `flats deploy` show the type `page` and otherwise treat a page as a
    website. The REST save endpoint accepts page bundles with no new fields.
12. **Reported type.** `get_flat`, `list_flats`, version and Draft objects
    report `type: "page"`, with the website fields (`publication`,
    `live_version`, `versions`, `draft`).

## Non-goals

Partial edits of a page Draft (a `replace` text operation like
`update_document`), host-provided chart libraries, a browser editor for
pages, several HTML pages per page flat, converting existing website flats
to pages, and changing the connection skill are outside this feature. MCP
environment, secret and network tools keep their current behavior for every
website-lifecycle flat; they have no effect on static content.
