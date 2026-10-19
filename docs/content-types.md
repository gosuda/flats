# Content types

Contract version: `1`. MCP resource: `flats://docs/content-types/v1`.
Read-only fallback tool: `get_content_types` (no arguments); the document part
is also `guide {"items":["topic.docs"]}`, and the page part
`guide {"items":["topic.page"]}`.

A flat's **type** says what an agent authored; the **kind** says how Flats runs
it. Agents upload content of a type; a type adapter in the host turns that
content into something Flats can serve. Websites and pages share the
lifecycle of Drafts, versions, approvals and previews; documents, designs and
boards run at once and keep revisions (below). Visibility and exposure work the same for every type.

| Type | Agent authors | Host runs it as | Live data |
|---|---|---|---|
| `flat` (default) | a website: static files, or a server module (`kind` `static`/`server`) | static file server or the server-flat runtime | server flats: `env.DB` / `env.FILES` |
| `docs` | Markdown documents (plus images/other assets) | the built-in collaborative editor app on the server-flat runtime (kind `server`) | the live collaborative document state |
| `page` | one static HTML document with charts or visualizations, plus its assets | static file server (kind `static`) | none |
| `design` | a canvas of HTML artboards (`canvas.json`, `.html` files and assets) | the built-in design canvas inside the host | the current canvas files, which people edit live |
| `board` | whiteboard elements (stickies, shapes, connectors, text, pen strokes, images and frames) | the built-in board canvas inside the host | the live board state |

A flat keeps its type: a save of another type is refused. Slides and other
types are candidates for later adapters; they are not implemented. Websites are described in `flats://docs/runtime-api/v1`.

The built-in editor uses host helpers (`runtimeGeneration`,
`ws.setSendLimits`, `__flats_docsCodec`) that are not part of runtime API v1
and are not supported for agent-authored server flats.

## Markdown documents (`type: "docs"`)

A docs flat runs the built-in collaborative editor: people edit live Markdown
in the browser. A Private link opens the editor and can toggle between Edit
and View; a Public link is view only (a read-only rendered document).

A document has **no Draft, versions, previews or publish**: those are for
websites. Creating or saving a document runs it at once, Private, with no
approval, and its Private URL serves it as soon as the host is ready. Its
history is kept as **revisions** instead (below).
Visibility still works as for websites: making it Public (or Private again)
and deleting it wait for the operator's approval (`topic.approvals`). On
documents, `list_versions`, `get_draft`, `create_version`, `deploy`, `open_preview`
and `rollback` are refused (`refusal.invalid`).

### Create and write

1. `create_flat {slug, name?, type: "docs"}` creates a document holding
   `index.md` with a `# <name>` heading and starts it. Report its
   `private_url` once `get_flat` shows `private_state: ready`.
2. For an existing docs flat, call `get_document {slug, doc?}` first. It
   returns the **live** Markdown including people's edits. It is idempotent
   and non-destructive but may seed or activate live state. For a small
   change, prefer `update_document` (below) over saving the whole document.
3. `save_document {slug, markdown, title?, path?, message?}` saves one
   Markdown document (`index.md` by default), creating a missing flat, and
   runs it at once. Other documents, images and assets are preserved, as are
   existing `flats.json` settings. An optional `title` changes only the
   manifest's `name`. Adding a document keeps the current entry; if it was
   implicit, the host makes it explicit when needed. People's concurrent edits
   are merged as described below. The result names the saved `doc` and the
   `entry`; `get_document` without `doc` reads the entry, which may differ
   from the default save path `index.md`. The result's `flat` shows `running`.
4. For several documents or assets, use `save_document {slug, replace_bundle: true,
   files: [{path, content, encoding}]}` with the complete bundle, including
   `flats.json` with `{"type":"docs"}`. This explicitly replaces the whole
   bundle: omitted documents and assets are deleted. It runs at once.
   The CLI equivalent is `flats save-document <slug> <dir|archive>`; the HTTP
   archive route is `POST /api/flats/{slug}/document`. Both replace the whole
   bundle explicitly. Draft tools and routes refuse document content.
5. A slug keeps its type: saving a document into a website or a page, or
   either into a document, is refused (`refusal.conflict`); use another slug.

If a saved document cannot start (for example its health check fails), the
save is refused and the running document keeps its previous text. Fix the
startup problem and repeat that same save: a different sibling save does not
carry the failed change. If no document has ever started, the files remain
saved internally until its first successful start. A single-document save validates the resulting
bundle against upload limits. If retained files put it over a limit, shrink the
target document further, raise `upload_max_bytes` for an upload-size refusal,
or explicitly save the complete desired bundle. No files are deleted to make
room; the disk quota still applies. **On a Public document a save, like a live edit,
is visible to everyone at once**; tell the user.

### Edit live text

`update_document {slug, doc?, ops, if_hash?}` edits the **live** Markdown of a
running document directly, the way a person edits in the Private editor: there
is no publish and **no approval**. Connected editors see the
change at once and their own concurrent edits elsewhere are kept.
**On a Public flat the change is visible to everyone on the internet as soon
as the call returns**; such results carry `public_notice`, so tell the user.
Use it for small corrections and additions; use `save_document` for a
rewrite of one document, or when the document is not running (which refuses
`update_document` with `not_deployed`). Repeat `save_document` to write other
documents; use an explicit whole-bundle save for assets or replacement.

1. Read `get_document {slug, doc?, blocks: true}`. It returns `hash` (of the
   whole document) and `blocks: [{hash, kind, level?, line, preview}]`. A
   block is a run of non-blank lines; a fenced code block is one block and a
   `#` heading line is always its own block. One read lists at most 1,000
   blocks; `blocks_total` gives the count and `block_offset` the next page.
2. Send 1–32 `ops`. They apply in order, each to the result of the previous
   one, and **all apply or none do**:
   * `{op: "replace", find, with, nth?}` replaces exact text. `find` must occur
     once, or give `nth` (1-based). An empty `with` deletes the text.
   * `{op: "replace_block", block, with}` and `{op: "delete_block", block}`
     act on a whole block named by its hash.
   * `{op: "insert", text, before|after|section_end: <block hash>}`, or
     `{op: "insert", text, at: "start"|"end"}`. `section_end` takes a heading
     block and inserts after the last block before the next heading of the
     same or a higher level. Inserted text becomes its own block(s): Flats
     adds the blank lines around it. To add a list item or a sentence to an
     existing block, use `replace` on its last line instead.
   * `nth` also picks among identical blocks with the same hash. Because it
     selects by position, any op with `nth` needs `if_hash`.
3. The guards are the text itself: if someone changed the `find` text or the
   block since you read it, the hash no longer matches and the whole call is
   refused with `edit_conflict` and **changes nothing** (their edit wins).
   Read again and rebuild your operations; never overwrite people's text to
   win. `if_hash` (from `get_document`) additionally refuses the call if
   anything in the document changed.
4. The result gives the new `seq` and `hash`, the `revision` the edit
   created and, per operation, the line and the characters removed and
   inserted. Each applied edit is also recorded in `get_logs` (kind
   `document`) with that summary, not the text. To undo an edit, roll back to
   the revision before it (below) or apply the reverse edit.

Limits: a request of at most 512 KiB, one resulting update of at most
256 KiB, Markdown up to 1 MiB per document, the document's stored history,
block operations only on documents of at most 50,000 blocks (use
`replace` beyond that), and the per-flat disk quota, which must also hold the
revision the edit creates (`document_capacity`); about 10 edits per second per flat (`unavailable`
when exceeded: wait and retry). Any other `unavailable` says whether the edit
applied (at which seq; do not repeat it) or did not apply (retry). Only if
Flats could not check is the outcome unknown: then read `get_document`
before retrying so an insert is not applied twice.

Live edits change only the live text, not the files the document was last
saved with. The next `save_document` merges into them exactly as into
people's edits (below): where the save changes the same lines, the save wins,
and the live text before the save stays in the revision closed just before
it. To keep a live edit in a save, start from the live text that
`get_document` returns.

When a document is saved, its Markdown is merged line by line into the live
text: disjoint and adjacent human edits survive; overlapping lines prefer the
saved text.

### Revisions

Every change of a document is kept as a revision of the whole flat (all its
documents and assets):

* each `save_document`, docs bundle save or `update_document` call is one
  revision, credited to you;
* people's edits in the browser are grouped per person: a group becomes one
  revision when that person has been idle for 30 seconds or 5 minutes after
  it began;
* before your save or edit applies, people's edits not yet in a revision
  become one, so the state just before your write is always kept.

| Tool | Use |
| --- | --- |
| `list_revisions {slug, before?, limit?}` | newest first, with kind (`created`, `agent`, `person`, `rollback`, `recovered`), authors, message, pinned and changed paths; pass `next` as `before` for older ones |
| `get_revision {slug, revision, path?}` | one revision's files, or with `path` a document's Markdown (or an asset as base64) as it was then |
| `rollback_revision {slug, revision}` | every document and asset becomes as it was in that revision, as a new revision; it can be undone the same way. Live at once; on a Public flat everyone sees it |
| `pin_revision {slug, revision, pinned}` | pinned revisions are never deleted automatically; a full pin quota refuses a new pin |

None of them needs approval. Old unpinned revisions are deleted beyond the
host's quota (default 100 kept, 20 pins); the operator can change both per
flat. People see the same history in the document's History panel on its
Private link. `recovered` revisions hold text that an older Flats preserved
when a save overwrote people's edits.

### Bundle rules

* Every `.md`/`.markdown` file is one document: valid UTF-8, at most 1 MiB
  each and 4 MiB together, at most 128 documents. Other files are assets,
  served as static files. Images (including SVG), audio, video, fonts and
  plain text (`.txt`, `.csv`, `.log`) open in the browser; every other asset,
  such as HTML, scripts or PDF, downloads instead. Assets never run scripts.
  Paths under `_docs/` are reserved.
* `entry` is the main document. Default: `index.md`, then `README.md`, then the
  only `.md`/`.markdown` file at any depth. It must be a Markdown file.
* `name` is the document title shown in the editor (default: the first `# `
  heading of the entry, else its path).
* `kind`, `spa` and `not_found` are rejected (the host chooses the runtime).
  `health` defaults to `/_docs/healthz`; a different value is rejected.
  `screenshot` keeps its meaning.
* Raw HTML in Markdown is not rendered and unsafe links are rejected.
  Relative links and image paths resolve relative to the document's directory.
* A fenced code block whose language is exactly `mermaid` is drawn as a
  [Mermaid](https://mermaid.js.org/) diagram (strict security level: no
  click handlers or HTML labels) in the light or dark theme of the reader.
  Diagram controls offer an expanded view with zoom, pan and fit, source
  viewing/copying, and SVG or PNG download. The expanded view supports keyboard
  navigation and Escape to close, restoring focus to its opener.
  Invalid syntax preserves the source and shows a syntax error; loading
  failures offer retry. Diagrams in quotes and lists also render in Edit.
  While editing a diagram's source, an optional preview updates after typing
  pauses. In Edit mode, Edit source opens the editable fence and Done returns
  to its diagram. View mode retains a read-only Source toggle.
* A document runs as kind `server` but keeps no data snapshots: it has no
  rollback or data restore. Docs flats get no outbound network, even with
  saved origin grants, and do not use environment variables or secrets.

The editor's runtime layout, storage schema and wire protocol are host
internals, not part of this contract or of runtime API v1.

### Not in scope

Comments, rich-text (WYSIWYG) editing, DOCX/PDF export, account management,
offline editing beyond in-memory reconnect buffering, multi-host replication,
and arbitrary HTML in Markdown.

## HTML pages (`type: "page"`)

A page is one static HTML document plus the assets it uses: a report, memo
or explainer that needs charts, graphs, interactive visualizations or a free
layout that Markdown cannot give. It has the website lifecycle: saves change
only a Private Draft, and the page serves after an operator-approved publish
(`topic.index` workflow map, `topic.approvals`). It has no server code,
database or file storage.

### Choose the type

| The user wants | Type |
|---|---|
| a document people edit together, or quick notes (Markdown, Mermaid) | `docs` (`topic.docs`) |
| a document you write as HTML with charts or visualizations | `page` |
| a site with several pages, an app, or anything with a server | `flat` (`topic.static`, `topic.server`) |

### Bundle rules

* `flats.json` must say `"type": "page"`. `save_page` and
  `create_flat {type: "page"}` write it for you; `save_draft`, `save_draft_from_dir` and `flats save-draft` need it in the
  upload. A flat keeps its type: saving a website into a page (for example an
  upload without the manifest) is refused (`refusal.conflict`).
* Exactly one HTML file, the entry (`index.html` by default; `entry` may
  name another `.html` file). Everything else is an asset: scripts, styles,
  images, SVG, fonts and data files such as CSV or JSON. A second HTML file is
  refused: inline it, or use a website flat.
* `kind` other than `"static"`, `spa`, `not_found` and `health` are refused.
  `name`, `entry` and `screenshot` work as for websites (`topic.manifest`).
* The page is served like a static site: `/` is the entry, assets are at
  their paths, other paths answer 404.

### Write and publish

1. `create_flat {slug, name, type: "page"}` makes an unpublished page with a
   starter Draft (revision 1). Optional: `save_page` also creates the flat.
2. `save_page {slug, html, title?, screenshot?, files?, expected_revision?,
   message?}` saves the Draft. `html` is the whole document (saved as
   `index.html`); `files` are the assets (`[{path, content, encoding}]`, utf8
   or base64), omitted for a self-contained page; `screenshot` names one of
   those files as the console thumbnail. It replaces the whole Draft: send
   the html, every asset and the screenshot each time.
3. Read the save `warnings`, then `open_preview {slug, version: 0}` and look
   at it in your browser (`topic.preview-check`).
4. `create_version` requests approval to save the
   Draft as a numbered version (`v1`, `v2`, ...), and `deploy {slug, version:
   N}` requests approval to activate that saved version (`topic.approvals`).
   `rollback` returns to an earlier version; a page has no data, so
   `restore_data` is refused.

### Write a document that reads well

The page contract and design guidance in `topic.design` apply; charts and
diagrams are in `topic.page.visuals`. For documents in particular:

* **Plan the design in the file.** Before writing the page, put a short
  design plan at the top of its `<style>` as tokens: four to six named color
  tokens, font tokens for the display, body and (if needed) data roles, and a
  one-line comment naming the layout. Build every later rule from those
  tokens.
* **Lead with the point.** Open with the title, one or two sentences that
  state the conclusion, and only then the evidence. A long document gets a
  short contents list linking to section ids (`<a href="#costs">`).
* **Structure is information.** Headings, eyebrows, numbering and dividers
  should say something true about the content: number steps only when the
  order matters, and use one heading level per level of the outline.
* **From Markdown or notes.** When the user gives you a Markdown document or
  notes to turn into a page, keep their substance and wording, but design the
  page instead of transcribing it line by line: tables become real tables,
  figures become charts where the numbers support one.
* **Reading width.** Body text about 65 characters wide. Tables and code
  may be wider inside their own `overflow-x: auto` box; charts follow the
  phone-width rule in `topic.page.visuals`.
* **Print.** People print and save documents as PDF. Add an `@media print`
  block that resets the color tokens and `color-scheme` to light (SVG charts
  that use the tokens follow; canvas and library charts redraw on
  `beforeprint`), drops shadows and sticky elements, removes the height and
  overflow limits of scrolling boxes, and shows data that is otherwise
  collapsed (open `<details>` in a `beforeprint` handler, or print the table
  separately). Undo it in `afterprint`: close what you opened and redraw the
  charts with the screen tokens. Keep a figure or a table row on one sheet
  (`break-inside: avoid`) but let long tables run across sheets; their
  `<thead>` repeats on each. Hide controls that do nothing on paper.
* **Data stays with the page.** Embed the data in the HTML (a
  `<script type="application/json">` block or a JS constant) or bundle it as
  a file loaded by a relative path (`fetch("data/sales.csv")`). Loading data
  from another origin while the page renders is a save warning: the
  published version then no longer fixes what readers see, and the page
  breaks offline.
* **Libraries.** Bundle a chart or diagram library into `files`, or load it
  from a CDN URL that names an exact version, with an `integrity` hash. Use
  one only when it does real work; inline SVG covers most document charts.

## Charts and diagrams in pages

How to make the visuals of a page (`topic.page`) carry information and
stay readable. The page contract in `topic.design` still applies. These are
defaults: depart from one when the content reads better without it.

### Pick the form before the colors

Decide what the reader must do with the numbers, then pick the form. Color
comes last.

| The reader should | Use |
|---|---|
| see one current value, maybe with its change | a stat tile (value, signed delta against a named period); not a one-bar chart |
| compare magnitudes | bars or columns, sorted when the order of the categories carries no meaning; a heatmap for a grid |
| follow a trend over time | a line; an area only for a single series |
| compare several series | grouped bars, several lines, or small multiples |
| see the parts of a meaningful whole | a stacked bar of additive parts; a pie or donut only for a few clearly different shares |
| see why one series matters | emphasis: that series in the accent color, the others quieter but still visible |
| see above or below a baseline or target | a diverging bar, or a line against the baseline |
| see a relationship between two measures | a scatterplot |
| see how values are distributed | a histogram or a box plot |
| look up many exact values | a table, possibly next to a smaller chart |

Never put two y-axes on one chart: the alignment of the two scales is
arbitrary and suggests a relation the data does not have. Use two charts, or
index both series to a common start (= 100).

Every chart states its source, units and period, and says what a gap or
missing value means.

### Scale and layout

* Map marks and ticks through the same scale. Bars and areas start at zero;
  a line chart may use a tighter range when its axis is clearly labeled. Use
  rounded domain bounds that contain all the data, and label units and any
  logarithmic scale.
* Leave room in the SVG `viewBox` for the outermost labels, give every shape
  an explicit fill or stroke, and give the chart's container room for its
  axis labels.
* At phone width, keep labels readable first: fewer ticks, shorter labels, a
  horizontal bar chart instead of columns, or small multiples. Shrinking an
  SVG also shrinks its text and hit areas, so when a chart cannot be
  simplified, give it a minimum readable width inside its own labeled
  `overflow-x: auto` box (the one exception to the phone-width rule in
  `topic.design` that charts share with tables, code and diagrams). The page
  body never scrolls sideways.

### Color by its job

* **Identity** (which series): a fixed order of distinct hues, assigned to
  each series once. A series keeps its color when filters hide others. Past
  six to eight series, fold the rest into "Other" or split into small
  multiples instead of inventing more hues.
* **Magnitude**: one hue from light to dark. No rainbow scales.
* **Polarity** (above/below): two hues that read as opposites (blue and
  orange, not blue and teal) with a neutral grey midpoint.
* **Status** (good, warning, critical): reserved for status, always with a
  text label or icon, never reused as a series color.
* Essential marks need at least 3:1 contrast against the background and
  against neighboring marks. Check that neighboring series stay
  distinguishable for color-blind readers (your browser's vision-deficiency
  emulation, or a palette checker); when two are close, add a second cue
  (direct labels, line style, markers).
* Every chart color is a theme token with a dark value chosen for the dark
  background, not an automatic inversion. SVG drawn in the page references
  the tokens through CSS (`fill: var(--series-1)`), so it follows the scheme
  by itself. A canvas or library chart copies colors when it draws: read the
  tokens with `getComputedStyle` and redraw when
  `matchMedia("(prefers-color-scheme: dark)")` changes, on `beforeprint` and
  again on `afterprint`.
* Text is never set in a series color: labels, values and legends use the
  page's text tokens, and a colored mark beside the text (a dot, a short
  line, a swatch) carries the identity.

### Marks and labels

* Keep marks light: lines about 2px, markers at least 8px across, bars that
  do not fill their whole slot, area fills as a faint wash. Gridlines and
  axes are thin, solid and quiet; keep dashed lines for thresholds and
  forecasts.
* Make identity readable without color matching: direct labels on the
  series (at the last point, for example) or, when they would collide, a
  legend. Label values sparingly (the last point, the extreme, the series the
  text is about) rather than every point, unless there are only a few. Round
  axis ticks to clean values with thousands separators.
* A label that does not fit inside its bar goes outside the bar end or into
  the tooltip; never clip it.

### Interaction

Interaction is worth adding when it reveals useful detail (exact values of a
dense chart) or supports exploration. A clearly labeled static chart with a
data table is enough for a simple figure.

* Lines and areas: a vertical guide that snaps to the nearest data position,
  with one tooltip listing every series there. Bars, dots and cells: a
  tooltip per mark, and the hovered mark visibly responds.
* The hit area is larger than the mark: a whole column per data position,
  or at least 24px around a point.
* Touch and keyboard get the same details. Open the tooltip on `click`
  (a tap; `pointerdown` also fires when a reader starts scrolling) and on
  keyboard focus. Keep it open while the pointer is over the trigger or the
  tooltip, close it with Escape, on blur and on a tap elsewhere.
* Keep keyboard stops bounded: one focusable chart (or one per series) that
  moves between data points with the arrow keys, rather than one Tab stop
  per datum. Announce the focused point with an `aria-label` on the focused
  element or a polite live region.
* In the tooltip the value is the prominent part and the series name is
  secondary. Keep it inside the chart's box: flip it below the point when
  there is no room above, and clamp it at the sides. When it adds a
  description to a focused element, give it `role="tooltip"` and point to it
  with `aria-describedby`.
* Tooltips add detail; they never hold the only copy of a value. Every
  value is also in direct labels or a data table.
* Insert category and series names with `textContent`, not `innerHTML`:
  they often come from a CSV header or a tool's output.
* Respect `prefers-reduced-motion` for tooltip and chart transitions.

### Text alternatives and data tables

* Wrap every chart in `<figure>` with a `<figcaption>` that states its
  point.
* A static SVG chart gets `role="img"` and `aria-labelledby` pointing to its
  `<title>` or the caption; a `<desc>` adds a longer description, not the
  name. An interactive chart keeps its focusable elements reachable: do not
  put `role="img"` on a container that holds them.
* Put the exact numbers in a table (it may sit in a `<details>` block).
  Numeric columns are right-aligned with `font-variant-numeric:
  tabular-nums`; large standalone figures (stat tiles) keep proportional
  digits.

### Diagrams

Draw a diagram when it shows a mechanism a reader would otherwise assemble
from prose: where data flows, which parts talk, what state something moves
through, what differs between two options. If a sentence says it faster,
write the sentence.

* Draw the mechanism, not its name: the path through the parts and the
  boundary being crossed. For a comparison, draw both options and the edge
  that differs between them.
* Label arrows with what happens on them (`writes`, `polls every 30s`); use
  a legend only when an encoding repeats.
* Hand-written inline SVG is enough: `viewBox` sized to the content and
  scaled with CSS (`max-width: 100%; height: auto`), strokes and text in
  `currentColor` so both schemes work, arrowheads as `<marker>` or a small
  polygon, text about 11-13px at the drawn size, shapes aligned to a grid.
  Caption and label it like a static chart.
* Mermaid also works in a page when you bundle the library (or pin an exact
  CDN version) and render `<pre class="mermaid">` blocks. Its colors are
  rendering settings, not CSS: use `theme: "base"` with `themeVariables` set
  to the token colors converted to hex (`#rrggbb`), keep each diagram's
  source, and render again
  from that source when the color scheme changes.

### Check the chart

In the one preview check before publishing (`topic.preview-check`), spend the
look on the charts: labels that collide or clip, marks outside the axes, a
tooltip that leaves the chart, colors that vanish in dark mode, and the phone
width. Switch the color scheme without reloading once, to catch charts that
do not redraw.

## Designs

This is the design topic. For a whiteboard of sticky notes, shapes and
connectors that people rearrange by hand, read `topic.board`.

A design (`type: "design"`) is a canvas of HTML artboards: screens, landing
pages, posters, social images, diagrams and click-through prototypes.
People open it in a browser, see the artboards side by side, play the
prototype, and move, retype and tweak things in place. Choose a design for
visual work people will look at and adjust together. Choose a page
(`topic.page`) for one finished document, and a website for a real site.

A design runs at once and has no Draft, versions, previews, publish or
approval. It is Private unless the operator approved Public. On a Public
design every save is visible to everyone at once, so tell the user. Every
change is kept as a revision.

### Create and write

1. `create_flat {slug, name?, type: "design"}` makes a design with a starter
   canvas, or `save_design` on a new slug creates it with your files. Give
   the user `private_url`: it opens the canvas.
2. For an existing design, call `get_design {slug}` first. It returns the
   canvas, the file list with `hash` and `gen`, the revision number and each
   artboard's tweaks. People may have edited since you last looked.
3. `save_design {slug, files, delete?, message?, expected_revision?}` writes
   files. One call is one revision.
   - Files you do not name stay as they are, so send only what changes.
   - `delete` removes files.
   - Pass `expected_revision` (from `get_design`) to refuse the write if
     anyone changed the design since you read it. That includes people's
     edits that are not yet a revision (`pending` in `get_design`); read it
     again, or retry after about 30 seconds.
   - Read `warnings` in the result.
4. Write `canvas.json` first, together with the first artboard, then the
   other artboards in later calls. People watching the canvas see it fill.

### Files

- `canvas.json`, the index (required, at most 256 KiB):
  ```json
  {"title": "Checkout redesign",
   "boards": {"cart.html": {"x": 0, "y": 0, "w": 390, "h": 844, "title": "Cart"},
              "pay.html":  {"x": 470, "y": 0, "w": 390, "h": 844, "title": "Payment"},
              "home.html": {"x": 0, "y": 1000, "w": 1440, "h": 900, "fill": true}},
   "order": ["cart.html", "pay.html", "home.html"],
   "notes": {"flow": {"x": 0, "y": -140, "w": 520, "text": "Checkout flow", "color": "yellow"}},
   "launch": {"view": "canvas"}}
  ```
  - `boards`: each `.html` artboard and its frame in CSS px. `w` and `h` are
    40 to 8000. Leave 80 px between frames in a row and 120 px between rows.
    Use `fill: true` for a fluid web page that fills the window when opened.
  - `order`: back to front. Boards missing from it are appended.
  - `notes`: headings over a group of artboards and comments to the reader.
    Colors are `gray`, `yellow`, `blue`, `green`, `pink` and `purple`.
  - `launch`: `{"view": "focused", "board": "cart.html"}` opens one artboard
    first.
- Artboards: one complete HTML document per `.html` file. Each should also
  work when opened on its own.
- Assets: images, fonts, CSS, JS and data files, referenced by relative path.
- Paths:
  - Segments use letters, digits, `_`, `-` and `.`, starting with a letter,
    digit or `_`.
  - At most 8 segments.
  - No `flats.json` and nothing under `_design/`.
  - Two paths may not differ only in case.
  - At most 512 files.

### Authoring rules (what people can edit)

- **Text.** People retype text that is written directly in an element:
  - The element holds only text and inline tags (`b`, `strong`, `i`, `em`,
    `u`, `s`, `small`, `sub`, `sup`, `code`, `mark`, `span`, `br`), and every
    tag is closed explicitly.
  - Text built by script is not editable. Keep copy as literal markup, and
    use `<p>`, `<h1>`, `<li>…</li>` with explicit end tags.
- **Tweaks.** Tweaks are named values people adjust without the source. At
  most one block per artboard:
  ```html
  <script type="application/json" data-flats-tweaks>
  {"accent": {"type": "color", "label": "Accent", "value": "#2459d6"},
   "gap":    {"type": "number", "label": "Spacing", "value": 24, "min": 8, "max": 64, "step": 4, "unit": "px"},
   "density":{"type": "select", "label": "Density", "options": ["compact", "roomy"], "value": "roomy"}}
  </script>
  ```
  - Types are `color`, `number` (with `unit` of `px`, `rem`, `em`, `%` or
    none), `text`, `select` and `boolean`.
  - The host sets each tweak as `--<name>` on `:root`, so style with
    `var(--accent)`.
  - Declare few, deliberate levers: one accent, a spacing scale, a variant.
    Copy is not a tweak.
- **Links.** `<a href="pay.html">` moves a played prototype to that
  artboard. Style the `<a>` itself as the button.
- Any other HTML, CSS and JavaScript works. Artboards run sandboxed and
  cannot call the host.
- **Opening a file directly** at `/<path>` on a Private route works for
  viewing, but cross-origin loads such as `fetch` of bundled data or web
  fonts work only inside the canvas.

### People editing at the same time

People change layout, notes, text and tweaks in the browser while you work.
Your `save_design` replaces each file you send whole. A person's change
prepared against the old content of that file is then refused, and their
canvas reloads.

To keep people's edits, `get_design {slug, path}` the file and change that
content. Do not rewrite it from an older copy.

### History

- One `save_design` call is one revision.
- A person's edits are grouped per person (closed after 30 s idle or 5 min).
- Before your write, open groups close into a revision, so nothing people
  did is lost.
- Revision tools:

  | Tool | Use |
  |---|---|
  | `list_revisions {slug, before?, limit?}` | newest first; pass `next` as `before` for older ones |
  | `get_revision {slug, revision, path?}` | one revision, or one of its files |
  | `rollback_revision {slug, revision}` | creates a new revision equal to that one, and can be undone the same way |
  | `pin_revision {slug, revision, pinned}` | pinned revisions are never deleted automatically |

- Old unpinned revisions are deleted beyond the host's quota (default 100).
  Pins have their own quota (default 20).
- None of this needs approval.
- Version tools (`save_draft`, `create_version`, `deploy`, `rollback`,
  `open_preview`, `list_versions`) refuse a design.

### Verify

There are no previews. After a save:

1. `get_design` checks the file list and `artboards[*].error` (a tweaks block
   that does not parse).
2. Give the user `private_url`.
3. If you have a browser tool, open the canvas and check each artboard.

The page-design contract in `topic.design` applies to each artboard: title,
viewport, contrast, accessible controls and pinned assets. Its screenshot
field, Draft and preview steps do not apply to designs.

### Craft

- **Sizes.**

  | Format | Frame |
  |---|---|
  | Phone screen | 390×844 |
  | Desktop web page | `w` 1280–1440, with `fill` |
  | Social post | 1080×1080 or 1080×1350 |
  | Poster on Letter | 816×1056 (CSS px at 96 px/in) |

- **Layout that survives editing.**
  - Lay out with flex or grid and `gap`, not absolute pixel positions, so
    retyped text reflows.
  - Give fixed-size artboards a root sized exactly `w`×`h`.
- **Real content.** No lorem ipsum or invented statistics. Missing facts
  become placeholders such as `[PRICE]`. Bundle real images and fonts, or
  pin a CDN URL to an exact version.
- **Accessible as drawn.** Use real `<button>`, `<a href>`, `<input>` and
  `<label>`. Text contrast is at least 4.5:1, or 3:1 at 24 px and above.
  Inline stroke SVG for icons, with `aria-hidden` when decorative. No emoji
  as icons.
- **Options.** Several directions are several artboards side by side, with
  a note naming each one.

## Boards

A board (`type: "board"`) is an infinite-canvas whiteboard of sticky notes,
shapes, connectors, text, pen strokes, images and frames. People edit it in
the browser at the same time as you, and see each other's cursors. You read
and write it as structured data.

Choose a board to spread out, group and connect ideas: brainstorms, sorted
feedback, flowcharts, retrospectives and plans that people will rearrange by
hand. Choose a design (`topic.canvas`) for pixel-exact screens, prototypes
and posters, a document (`topic.docs`) for prose, and a page (`topic.page`)
for one finished HTML document with charts.

A board runs at once and has no Draft, versions, previews, publish or
approval. A new board is Private. Making it Public, Private again, or
deleting it waits for the operator (`topic.approvals`). A Public board shows
every change to everyone at once, so tell the user before you write to one.
Version tools refuse a board (`refusal.invalid`); board tools refuse other
types (`refusal.not_board`).

### Create and read

1. `create_flat {slug, name?, type: "board"}` creates an empty board, or
   `update_board` on a new slug creates it and applies your operations. Give
   the user `private_url`: it opens the canvas.
2. Read before you change an existing board. People may have edited since you
   last looked. `get_board {slug, format?, frame?, rect?, ids?, revision?,
   limit?, cursor?}` returns `{seq, revision, pending, elements, next?}`.
   - `format: "outline"` lists element types per frame, short ids, text and
     connections ("A → B"). Use it to understand a board cheaply, and
     `"full"` (the default) for coordinates and styles before you move things.
   - `frame`, `rect` (an area `[x, y, w, h]`) or `ids` narrow the read.
   - A page holds up to `limit` elements (default 500, at most 2,000) or
     1 MiB. Pass `next` as `cursor` for the following page. Every page comes
     from the same state, even while people edit. A cursor lasts 60 seconds;
     an expired one is refused with `conflict`, so read again from the start.
   - `seq` identifies the current state. Each element carries `ver`, which
     grows with every change to it, including a rollback or replacement that
     only gives a pen other points or an image another picture. `pending`
     says people have edits that are not a revision yet.
   - `revision: N` reads an earlier revision (no cursor expiry).

### Write

`update_board {slug, op_id, ops, layout?, assets?, if_seq?, message?,
replace?}` applies all operations or none, and is one revision.

```json
{"op_id": "sort-onboarding-1", "if_seq": 233,
 "ops": [
  {"op": "add", "ref": "$f1", "el": {"type": "frame", "text": "Onboarding", "x": 0, "y": 0, "w": 900, "h": 600}},
  {"op": "add", "ref": "$s1", "el": {"type": "sticky", "text": "Fewer sign-up steps"}},
  {"op": "reparent", "ids": ["$s1", "e_7Hk2pQ9xLm"], "frame": "$f1"},
  {"op": "set", "id": "e_7Hk2pQ9xLm", "fields": {"style": {"color": "blue"}}, "if_ver": 57},
  {"op": "add", "el": {"type": "connector", "ends": [{"el": "$s1"}, {"el": "e_7Hk2pQ9xLm"}]}},
  {"op": "delete", "ids": ["e_old0000001"]}],
 "layout": [{"kind": "grid", "ids": ["$s1", "e_7Hk2pQ9xLm"], "in": "$f1", "columns": 4, "gap": 24}],
 "message": "Group the onboarding feedback"}
```

- `add` creates an element. A `ref` starting with `$` names it for later
  operations and layouts in the same call; the result's `ids` maps each ref
  to the new id. Without coordinates and without a layout, new elements go
  in a row to the right of the board. Default sizes: sticky 200×200, shape
  160×100, text fitted to its content.
- A frame you add without `z` goes behind every element, so the elements you
  move into it stay visible; several new frames in one call keep their
  order. Any other new element goes in front. A frame's fill hides what is
  behind it, so never give a frame a `z` above its members.
- `set {id, fields}` changes named fields. Elements and fields you do not
  name stay as they are, and `style` changes only the keys you give. A
  field cannot be `null`: it never clears text or unlocks. Change `frame`
  and `group` with `reparent`, `group` and `ungroup`.
- `reparent {ids, frame}` moves elements into a frame, or out with `null`.
  Grouped elements move together. Frames do not nest.
- `delete {ids}` deletes elements. Deleting a frame keeps its members;
  connectors attached to a deleted element keep a free end where they were
  attached, kept within the canvas range (±10,000,000).
- `group`, `ungroup`, `zorder` and `delete_frame_contents` group elements,
  change stacking order and delete a frame with its members. `group` makes
  a new group id unless you pass one; group ids are never reused, so an id
  the board used before is refused.
- Moving a frame by `x` or `y` alone moves its members (`carry: "members"`
  is the default). A `set` that also changes `w` or `h` leaves members in
  place unless you pass `carry: "members"`. `carry: false` never moves them,
  and `carry: "group"` moves the element's group with it.
- A locked element refuses the call unless an earlier operation in the same
  call sets `locked: false`. Unlock only what the user asked you to change.
- `layout` places elements in input order: `grid` (with `columns` and
  `gap`), `row` or `column`. With `in`, placement starts 32 px inside that
  frame, and the frame grows to fit. The same input gives the same
  coordinates; the result's `layout` returns them as `{id: [x, y]}`. Layout
  runs after the operations, so it sees their result.
- Connector ends keep the side they were attached to when their elements
  move. When you move or lay out elements that connectors join, set those
  connectors' ends again without `at` in the same call, for example
  `{"op": "set", "id": "e_conn000001", "fields": {"ends": [{"el": "e_7Hk2pQ9xLm"}, {"el": "$s1"}]}}`.
  The host then picks the facing sides from the final places, after
  `layout`. Otherwise a line can leave a note on its far side and run
  through its text.
- `assets` adds images for image elements in the same call:
  `[{"ref": "$logo", "content": "<base64>"}]`. An image element gives the
  ref as its `asset`, for example
  `{"op": "add", "el": {"type": "image", "asset": "$logo", "alt": "Logo"}}`;
  the host replaces it with the image's hash and fills `natural` (its pixel
  size) when you leave it out. The result's `assets` maps each ref to its
  hash, which later calls can use as `asset`. PNG, JPEG, GIF and still WebP
  only, each at most the upload limit; no SVG. One call carries images of
  about one upload limit in total, since they travel as base64. Add more over
  several calls (name earlier images by their hash), or put them in a board
  data file and import it with the CLI or HTTP API. A call or import whose
  images do not fit the disk quota is refused before any image is stored.
- `replace: true` replaces the whole board with the call's `add` operations.
  `layout` can place those new elements. Use it only when the user asked for
  a fresh board.
- On a new slug the board exists only if the call applies: a refused call
  leaves no flat behind.
- `op_id` (8 to 120 characters) is required. Resending the same request with
  the same `op_id` within 24 hours returns the first result and applies
  nothing again, so retry with it after a timeout. The same `op_id` with a
  different request is refused with `conflict`.

The result is `{revision, seq, ids, layout, results, assets?}`. `revision` is
null, with a `warning`, while the call's revision is still being recorded;
the call itself applied, so do not resend it with a new `op_id`.

### Import and shared selections

- `import_board {slug, op_id, archive, mode}` takes a Flats board data file
  (`.zip` as base64, at most the host upload limit). `mode: "add"` copies it
  into the board with new ids, keeps connections inside the file and places
  it to the right. `mode: "replace"` replaces the whole board. The CLI
  (`flats board import`) accepts files up to four times that limit; pass
  `--op-id` so that resending the same file after a timeout imports it once.
- People can select elements and press **Send to agent**. The screen then
  says "Tell the agent `#7`". `get_board_share {slug, n: 7}` returns that
  selection; without `n` it lists recent ones. Elements deleted since are
  marked `missing`. A share lasts 24 hours or until the board restarts. You
  never see cursors or live selections.

### Elements

Every element has `id`, `type`, `x`, `y`, `w`, `h` and `rot` (CSS px at
100% zoom; the host computes a connector's box from its ends), `z`, `frame`,
`group`, `locked`, and its `style` keys. A missing style key takes its
default; an unknown key is refused.

| Type | Fields | `style` keys |
|---|---|---|
| `sticky` | `text` | `color`: `yellow`, `orange`, `pink`, `blue`, `green`, `gray`; `fontSize`: `"auto"` (16, shrinking to fit) or 10 to 72 |
| `text` | `text` | `color` (ink); `fontSize` 12 to 144 (20); `align`: `left`, `center`, `right` |
| `shape` | `text` | `shape`: `rect`, `round`, `ellipse`, `diamond`; `fill`: `none`, `white` or a sticky color; `stroke` (ink); `strokeWidth`: 1, 2, 4; `dash`: `solid`, `dashed`, `dotted`; `fontSize` 10 to 72 (14) |
| `connector` | `ends`, `text` (label) | `stroke`, `strokeWidth`, `dash`; `route`: `straight`, `elbow`; `head0`, `head1`: `none` or `arrow`; `fontSize` 10 to 32 |
| `pen` | `pts` | `stroke` (ink or `highlight`); `width`: `pen`, `marker` |
| `image` | `asset`, `alt`, `natural` | none |
| `frame` | `text` (name) | `fill`: `white`, `none`, `gray` |

Ink colors are `ink`, `gray`, `red`, `orange`, `green`, `blue` and `purple`.
Colors are names; the canvas picks values that read in light and dark
themes. A connector end is `{"el": id, "at": [ax, ay]}` (attached, `at` from
0 to 1 inside the target's box) or `{"pt": [x, y]}` (free); `at` and `pt`
have exactly two numbers. Without `at`, the
host attaches the end to the middle of the target's side that faces the other
end, after `layout` has placed the elements, so the line starts on the
outline; an attached other end without `at` counts at its target's center.
The host stores that side as the end's `at`, so it stays when the elements
move later; attach the end again without `at` to recompute it (Write).
A frame is drawn behind its members only if its `z` is lower: the canvas
draws elements in order of `z` compared as plain strings (ties by `id`),
later ones on top.
Text is plain, at most 4,000 characters (200 for frame names and connector
labels, 1,000 for an image's `alt`).

Limits per board: 10,000 elements, 512 images (24 megapixels each), 2,000
points per pen stroke, 2,000 operations and 1,000 layout targets per call.
An edit that would exceed the per-flat disk quota is refused with
`board_capacity`.

### People editing at the same time

- People's edits merge by property: position and size, each connector end,
  text, and each style key change on their own. Your change to one property
  does not undo another person's change to a different one; on the same
  property the later write wins.
- If an element you target was deleted, the whole call is refused with
  `conflict`. Read again and rebuild the call.
- `if_seq` refuses the call if anything on the board changed since your read.
  `if_ver` on an operation refuses the whole call if that element changed:
  a version for one element or `{id: ver}`. `delete_frame_contents` takes
  the frame's version, and `ungroup` takes `{id: ver}` of its members. Both
  guards include people's edits that are not yet a revision. Use `if_ver`
  on elements you rewrite from what you read, such as text you edit.
- After a guard refusal, read again, apply your change to what people wrote
  and send it with a new `op_id`. Never resend without the guard, or use
  `replace`, to overwrite them.

### History

- One `update_board` or `import_board` call is one revision. A person's edits
  are grouped per person (closed after 30 s idle or 5 min), and open groups
  close into a revision before your write.
- Use the shared revision tools, without approval: `list_revisions`,
  `get_revision` (the revision's file list; read its elements with
  `get_board {slug, revision}`), `rollback_revision` (a new revision equal to
  that one, applied to open canvases at once) and `pin_revision`. When
  `rollback_revision` answers with a `warning` and no revision, the restore
  applied but its revision is not recorded yet: do not call it again.
- Old unpinned revisions are deleted beyond the host's quota (default 100,
  20 pins). People see the same history in the canvas's History panel.

### Verify

1. Read `results` and `layout`, then `get_board {slug, format: "outline"}`:
   every element is there, in the right frame, and every connector joins the
   intended elements.
2. The outline does not show stacking. For each frame you created or
   filled, read `get_board {slug, frame: <id>}` (the full format lists the
   frame and its members with their `z`) and check that every member's `z`
   is greater than the frame's. If not, send the frame back with
   `{"op": "zorder", "ids": [<frame id>], "to": "back"}`.
3. Give the user `private_url`. If you have a browser tool, open the canvas
   and look at the changed area.

### Readability

- No overlaps: use `layout`, or leave at least 24 px between elements.
- Group related elements in frames with short, clear names.
- Use color for meaning, for example one color per topic or status, and say
  what each color means in a text element. Also state that meaning in frame
  names or text, so the board reads without color.
- Keep text readable at 100% zoom: at least 12 px, and short stickies.
  Sticky `fontSize: "auto"` starts at 16 px and shrinks to fit, down to
  10 px, inside the sticky minus 12 px on each side (more at the bottom
  when people show author names). A line takes 1.38 × the font size. A
  default 200×200 sticky holds 7 lines at 16 px; a small one, such as
  120×90, drops below 12 px with a short sentence. Keep stickies with text
  at the default size or larger, or set `fontSize` to 12 or more and make
  the sticky big enough: a fixed size never shrinks, and text that does not
  fit is cut off with "…". Shape text also shrinks from its `fontSize` down
  to 10 px, so size shapes for their text. Never set a `fontSize` below 12.
- Attach both ends of a connector to elements; point arrows in the reading
  direction, left to right or top to bottom.
