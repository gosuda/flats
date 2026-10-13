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
