# Page content type: technical design

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


Behavior: [page-type.md](page-type.md).

## Type name and listing

`internal/contenttype` gains `Page = "page"`. `Types()` returns
`flat`, `docs`, `page` in that order with `{Page, "Page", "Static HTML page:
a document with charts, graphs and visualizations"}`. `FromManifest` needs no
change: it already returns the manifest type verbatim. The store keeps the
type in the version/Draft manifest, so no schema or migration change is
needed.

## Bundle validation (`internal/bundle`)

`ParseManifest` accepts `page` next to `flat` and `docs`; the messages for an
empty or unknown type list `"flat", "docs" or "page"`. Automatic detection is
unchanged (only an explicit manifest type makes a page). A page dispatches to
a new `parsePage(files, index, fields, m, &verr)`, analogous to `parseDocs`:

* `kind`: absent or `"static"`; anything else is refused ("pages are always
  static; remove kind or use a website flat for server code"). `spa`,
  `not_found` and `health` present in `fields` are refused with a fix each.
  `m.Kind = "static"`, `m.Health = "/"`.
* `entry`: default `index.html`; it must be a clean relative path ending in
  `.html`/`.htm` and exist in the bundle.
* HTML count: every file with an HTML extension other than the entry is
  refused ("a page has one HTML document; inline this content into the entry,
  or use a website flat for several pages"). The check uses the existing
  `isHTML` helper, moved or shared from `lint.go`.
* `screenshot` is checked as for websites. Every problem is reported at once,
  following the existing `ValidationError` pattern.

The returned `Manifest` has `Kind: "static"`, so `core` builds and serves a
page with the existing static path (`internal/site`) and no runtime.

## Save warnings (`bundle.Lint`)

`Lint` already runs for every non-docs bundle, so pages get the existing
checks unchanged. For `m.Type == page` it adds one heuristic,
`externalData` (`internal/bundle/pagelint.go`): in inline `<script>` text of
the entry (scripts without `src`, outside `<template>`, excluding the data
types `importmap`, `application/json`, `application/ld+json`, `text/plain`
and `text/template`) and in bundled `.js`/`.mjs` files, find a string literal
(single, double or backtick quotes) naming an absolute `http(s)://` or
`ws(s)://` URL, or a protocol-relative `//host/...` URL (reported as
`https`), in these argument positions:

* the first argument of `fetch(`, `new EventSource(`, `new WebSocket(`,
  `Papa.parse(` and of the d3-fetch style loaders `.json(`, `.csv(`,
  `.tsv(`, `.text(`, `.xml(`;
* the second argument of `.open("METHOD", url)` (XMLHttpRequest).

Code loading (`import(...)`, `<script src>`, module imports) is not a data
load and stays with the existing pinned-version check of `<script src>` and
stylesheets; dynamic `import()` of code is not inspected.

Matches are found one at a time (no slice of every match), a repeated origin
costs no allocation beyond the per-call match indices, and a file stops after
`maxSeen` distinct origins. One warning per (file, origin), within the
existing `maxWarnings` bound:
"page loads data from <origin> while it renders, so a published version no
longer fixes what readers see, and the page breaks offline" with fix "bundle
the data file (e.g. data.csv) and load it by a relative path, or embed it in
the HTML". Module scripts loaded by a pinned CDN URL are already covered by
the existing external-code check. It is a lexical heuristic (regular
expressions over source text, as the rest of Lint is over tokens): it never
blocks a save and documents that dynamic URLs are not detected. Input size is
already bounded by the upload limit; scanning is linear.

## Type stickiness (`internal/core`)

`checkTypeChange(ctx, f, docs bool)` becomes `checkTypeChange(ctx, f,
typ string)`. The flat's current type is computed by a new helper
`flatType(ctx, f)`: the live version's manifest type when `LiveVersion > 0`,
otherwise the Draft's, otherwise `""` (untyped). A non-empty current type
that differs from `typ` returns an error wrapping `ErrTypeChange` (so
`errors.Is` keeps working) that names both types and the fix: "flat <slug> is
a page and this save is a website; add flats.json {"type":"page"} (or use
save_page) to update it, or use another slug". The sentinel's own text
becomes "a flat cannot change its content type (website, page or document)".
`SaveVersion` passes `m.Type`. `isDocs` keeps its behavior and is
reimplemented as `flatType(...) == contenttype.Docs`.

Activation boundaries: a helper `checkCandidateType(ctx, f, manifest)` refuses
with the same error when `f.LiveVersion > 0` and the candidate's type differs
from the live version's. It runs at request time in `Publish`,
`requestDeploy` (Draft and version branches) and `requestRollback`, and again
in `applyPublishApproval`, `applyActivateApproval` and
`applyRollbackApproval`, so content of a mixed type stored by an earlier
release (the existing "legacy docs Draft over a running website" case) and
approvals requested before this release cannot switch a running flat's type.
A refused apply fails the approval with `failure_code` `conflict` and leaves
the live version untouched. Documents are unaffected: their saves call
`publishRevision` directly after `checkTypeChange`.

Behavior change: before, only a running flat was protected, so an
unpublished website Draft could be replaced by a document (and the reverse
could not happen, since a saved document always runs). Now any typed flat is
protected. The refusal page `refusal.conflict` and the docs topic are updated
to the general wording.

## Create (`internal/core`)

`CreatePage(ctx, slug, name, via)` mirrors `CreateDocument`: `CreateFlat`,
then `SaveVersion` of the starter bundle with `SaveMeta{Message: "created"}`
and an expected Draft revision of 0, so a save that reached the new flat
between the two steps is kept and creation reports a conflict instead of
replacing it. It does not publish. The starter `index.html` is a complete small page:
doctype, `<meta charset="utf-8">`, the viewport tag, `<title>` and `<h1>`
from the HTML-escaped name (slug when empty), and `:root` color tokens for
light and dark as in `topic.design`. `flats.json` is
`{"type":"page","name":<title>}`. The REST create endpoint is unchanged; only
MCP `create_flat` offers the type.

## MCP (`internal/mcpx`)

* `create_flat`: `type` accepts `flat`, `docs` or `page`; `page` calls
  `CreatePage` and replies "Created unpublished Private page <slug> with a
  starter Draft (revision 1). Next: save_page, open_preview, then publish
  and wait for operator approval." Schema text lists the three types.
* New `save_page` tool in `page.go`, input `SavePageIn{Slug, HTML, Title,
  Screenshot, Files []FileIn, ExpectedRevision *int, Message, Deploy}`. It
  decodes `files` only when non-empty (`decodeFiles` refuses an empty list,
  and a self-contained page has no assets), refuses (`invalid`) a file named
  `flats.json` or any HTML path, adds `index.html` from `html` (refusing an
  empty `html`) and a generated manifest (`type: page`, plus `name` when
  `title` is set and `screenshot` when given; `ParseManifest` then checks the
  screenshot file exists), normalizes with `bundle.FromFiles` and calls the
  shared `t.save`. The result,
  warnings and deploy handling are those of `save_draft`. Description: "Save
  a page (one static HTML document with charts or visualizations) as a
  Private Draft ... Saving never publishes; deploy=true requests operator
  approval. Replaces the whole Draft: send every asset each time
  (topic.page)."
* `flatInfo`, `stateText`, `flatText` and `save` already treat every
  non-docs type as a website; only schema strings change
  (`VersionInfo.Type` "flat (website), page (HTML page) or docs (document)").
* `get_content_types` description, `create_flat` description and the
  `llms.txt` content-types line mention pages.

## Guidance (`docs/`)

* New topic file `docs/agent/topics/page.md` (`## Pages`): when to choose
  page vs docs vs flat, bundle rules and refused manifest fields, `save_page`
  and `create_flat {type:"page"}`, chart and data practice (bundle the
  library or pin an exact CDN version with integrity; bundle or inline the
  data; text alternative such as a caption or a data table for every chart;
  chart colors as tokens in both schemes; fit at phone width), and a pointer
  to the website lifecycle (`topic.index` workflow map, `topic.approvals`).
* New topic file `docs/agent/topics/page.visuals.md` (`## Charts and
  diagrams in pages`): form by the data's job, color by role (identity,
  magnitude, polarity, status), marks/labels/scale, hover with tap and
  keyboard equivalents, data tables, diagrams, and where to spend the preview
  look. `topic.design` gains a token plan, structure-as-information, dashboard,
  copy and clean-build guidance and points charts to it.
* `TopicOrder` inserts `page` and `page.visuals` after `docs`;
  `contentTypesSections` becomes `docs`, `page`, `page.visuals`, so the
  generated `docs/content-types.md` gains both sections.
  The reference header (`docs/agent/references/content-types.md`) adds the
  page row to its type table and drops "Slides and other types" only as far
  as it now lists three types.
* `topic.index` adds the routing row; `topic.manifest` lists `page` for
  `type` and its refused fields; `topic.design` says it applies to pages;
  `refusal.conflict` and `topic.docs` use the general type wording.
* MCP instructions stay within the 2,300-byte test cap; the first sentence
  may name "HTML pages" if it fits, otherwise the index routes to the topic.
* `go test ./docs -run TestGeneratedReferences -update` regenerates the
  references. README "Content types" and `docs/README.md` mention pages.
* The connection skill (`plugins/flats/skills/flats-deploy/SKILL.md`) and its
  bootstrap revision are not changed; the guide revision changes
  automatically, which `check_client` reports as `refresh_required`.

## Console (`internal/console/static`)

* `dom.js` `ICONS.code`: a `</>` glyph in the existing 24-unit stroke style,
  `M8 7l-5 5l5 5M16 7l5 5l-5 5M14 4l-4 16`.
* `list.js` `TYPES.page = {key: 'page', label: 'Page', icon: 'code'}`;
  `typeOf` maps by key with `flat` as the fallback.
* `app.css` tokens `--type-page`/`--type-page-bg` for dark (default) and
  light schemes, chosen for at least 4.5:1 icon contrast on the tile, and
  `.type-page` / `.preview-page` rules next to the docs ones.
* `flat.js` and `site.js`: a small `isStatic()`/`isPage()` predicate hides the
  Database tab and the environment, secret and network sections for pages,
  as already done for documents; the deployments, versions and previews
  sections stay.

## Rollback with data

A page has no database, so `requestRollback` with `restore_data=true` fails
in `liveSnapshot` with the existing "no snapshot" conflict before an approval
is created, exactly as for any static flat. This is documented in
`topic.page` and tested; plain rollback goes through approval as usual.

## CLI and REST

No new flags or fields. `displayType` already prints the stored type, and
the docs-specific branches in `internal/cli` and `internal/api` key on
`docs`, so pages follow the website output.

## Tests

* `contenttype`: `FromManifest` page case; `Types()` order and names.
* `bundle`: page parse matrix (default entry, custom HTML entry, missing
  entry, non-HTML entry, second HTML file, `kind: server`, `kind: static`
  accepted, `spa`/`not_found`/`health` refused, screenshot), no automatic
  page detection, unknown-type message; Lint external-data warnings (each
  call form, inline vs bundled JS, JSON script blocks ignored, relative URLs
  and pinned CDN code not flagged, one warning per origin, websites not
  checked).
* `core`: `CreatePage` starter Draft is unpublished, typed `page` and passes
  `Lint` without title/viewport warnings; publish via approval serves the
  page statically with `--runtime=false`; type stickiness for page vs flat
  vs docs, both live and Draft-only, and untyped flats taking the first
  save's type; existing docs/website refusal tests keep passing.
* `core` additionally: publishing a legacy Draft or activating/rolling back
  to a version whose type differs from the live one is refused at request
  time and at apply; `rollback` with `restore_data` on a page is refused
  before approval, plain rollback requests approval.
* `mcpx`: `create_flat {type:"page"}`, `save_page` success with assets, with
  `files` omitted and with `files: []`, with a screenshot (and a missing
  screenshot file refused), refusal of HTML or manifest in `files`, empty
  `html`, expected_revision conflict, deploy request returning
  `pending_approval`, warnings passthrough; a generic save without a page
  manifest into a page flat refused with the naming hint; the exact tool
  inventory in `TestListTools` and the lifecycle authority census gain
  `save_page`, verifying it leaves live content and approval states
  unchanged; guide topic discoverable and instructions within the cap.
* `console`: UI test that a page flat renders the Page label and code icon
  and omits the Database tab and server sections.
* Generated references up to date (`TestGeneratedReferences`).
