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
  `create_flat {type: "page"}` write it for you; `save_draft`,
  `save_version`, `save_version_from_dir` and `flats deploy` need it in the
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
   message?, deploy?}` saves the Draft. `html` is the whole document (saved as
   `index.html`); `files` are the assets (`[{path, content, encoding}]`, utf8
   or base64), omitted for a self-contained page; `screenshot` names one of
   those files as the console thumbnail. It replaces the whole Draft: send
   the html, every asset and the screenshot each time.
3. Read the save `warnings`, then `open_preview {slug, version: 0}` and look
   at it in your browser (`topic.preview-check`).
4. `publish` (or `deploy: true` on the save) and wait for the operator
   (`topic.approvals`). `rollback` returns to an earlier version; a page has
   no data, so `restore_data` is refused.

### Charts and data

The page contract in `topic.design` applies (title, viewport, favicon,
both color schemes, phone width, accessibility). For charts in particular:

* **Library.** Bundle the chart library file (Chart.js, D3, ECharts, Vega,
  Plotly...) into `files`, or load it from a CDN URL that names an exact
  version, with an `integrity` hash. Inline SVG needs no library.
* **Data.** Embed the data in the HTML (a `<script type="application/json">`
  block or a JS constant) or bundle it as a file loaded by a relative path
  (`fetch("data/sales.csv")`). Loading data from another origin while the page
  renders is a save warning: the published version then no longer fixes what
  readers see, and the page breaks offline.
* **Readable in both schemes.** Take series, axis and grid colors from the
  page's color tokens, re-read them when `prefers-color-scheme` changes (or
  draw with `currentColor`), and keep at least 3:1 contrast against the
  background. Do not rely on color alone: label series directly or add a
  legend with distinct markers.
* **Text alternative.** Give every chart a caption that states its point
  (`<figure>` with `<figcaption>`), and the exact numbers in a table, a
  `<details>` block or an `aria-label`.
* **Phone width.** Charts resize with their container (`max-width: 100%`,
  a responsive option, or an SVG `viewBox`); a wide chart may scroll inside
  its own `overflow-x: auto` box.
* **First frame.** Render charts on load, not on scroll, and respect
  `prefers-reduced-motion` for chart animations.
