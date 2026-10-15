## Page design

Every page a flat serves (a static site, an HTML page flat, or the HTML a
server flat returns) should meet the page contract below. Saves check part of
it and return non-blocking `warnings` (see "Save warnings"). Precedence: the
user's own words, then the project's existing design system (a tokens or
theme file, existing component styles), then these defaults.

Design artboards (`topic.canvas`) follow the same contract for title,
viewport, themes, contrast, accessibility and assets. The screenshot field,
Draft and preview steps here apply to websites and pages only, and the
phone-width rule applies to `fill` artboards, not to fixed-size posters or
social images.

### Page contract

* **Title.** A real `<title>` that names the page: a short noun phrase, two
  to four words, specific enough to pick it out among many tabs ("Team Lunch
  Poll", not "Poll" and not "Lunch Poll: vote for Friday's lunch"). Use the
  user's name for the thing when they have one; never a scaffold default such
  as "Vite + React". Put explanations in the page, not the title. Keep it
  stable across versions.
* **Viewport.** `<meta name="viewport" content="width=device-width,
  initial-scale=1">` in every page's `<head>`.
* **Icon and thumbnail.** A favicon (`<link rel="icon" href="favicon.svg">`
  with the file in the bundle, or `favicon.ico` at the root), and a
  `screenshot` image in `flats.json` that the operator's console shows as the
  flat's thumbnail (`topic.manifest`). Capture it from the Draft preview at
  desktop width; keep it small (PNG or WebP, well under 500 KiB).
* **Both color schemes.** Define every color as a token on `:root` (light
  values), redefine only the tokens for dark, and paint `body` from a token:

  ```css
  :root { --bg: #f7f7f4; --fg: #1d1f1c; --muted: #5d625a; --accent: #2f6f5e; color-scheme: light }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #151714; --fg: #e8eae4; --muted: #a2a89c; --accent: #7cc2ad; color-scheme: dark }
  }
  body { background: var(--bg); color: var(--fg) }
  ```

  A color that exists only inside the dark block, or a literal color in a
  component rule, is unreadable in one scheme. A page that deliberately
  commits to one look may skip the dark block but still sets `body`
  background and every color explicitly.
* **Phone width.** At about 400px the page body never scrolls sideways. Keep
  a side gutter of at least 16px at every width, set once on `body` or one
  wrapper (use `padding-block` for vertical padding so the sides stay). Let
  flex and grid rows wrap or stack; give text-holding flex/grid children
  `min-width: 0`; put `max-width: 100%` on images. Only tables, code,
  charts and diagrams may be wider, each inside its own `overflow-x: auto`
  box, and charts only after simplifying them (`topic.page.visuals`).
* **Complete first frame.** Everything meant to be read is visible right
  after load, without scrolling to trigger it: no content parked at
  `opacity: 0` for a scroll observer, no `100vh` hero that pushes the content
  out of the first screen. A tool opens in a realistic working state with
  clearly marked example data, or a designed empty state that says what will
  appear and how to add the first item.
* **Accessibility basics.** Visible keyboard focus on every control
  (`:focus-visible`), text contrast of at least 4.5:1 in both schemes, a
  `<label>` or `aria-label` for every input, `alt` text on meaningful images,
  real `<button>` and `<a href>` elements, and `prefers-reduced-motion`
  respected for anything that moves.
* **Bundle assets by default.** Ship the page's scripts, styles, fonts and
  images in the upload. Flats sets no Content-Security-Policy, so a page can
  load anything, but every external request is a dependency the host does not
  control: it breaks offline and on a tailnet without internet, tells a third
  party who opened a Private page, and runs whatever that host serves next.
  A CDN script or stylesheet is acceptable only when its URL names an exact
  version (`react@18.3.1` on jsDelivr or unpkg, `react/18.3.1/...` on cdnjs);
  never a bare package, a range or a tag (`react`, `react@18`, `@^18`,
  `@latest`). Add an `integrity` hash and `crossorigin="anonymous"` when the
  CDN publishes one. Hosted font stylesheets (Google Fonts) cannot be pinned:
  prefer bundled `.woff2` files with `@font-face`.

### Design taste

* **Read the request.** A memo, plan or internal tool gets a polished,
  utilitarian treatment: real type hierarchy, considered spacing, a proper
  palette, no giant hero. A landing page, game or something the user will
  share can take a bolder, editorial treatment with one deliberate risk.
* **Ground it in the subject.** Use real content, never lorem ipsum, and at
  least one detail only this subject has (its units, terms, conventions).
* **Plan with tokens.** Settle the palette (four to six named colors) and
  the type roles as tokens at the top of the stylesheet before writing the
  page, and derive every later color and font from them.
* **Neutrals on purpose.** Tint greys slightly toward the accent instead of
  using a default mid-grey; pick white or near-black backgrounds only by
  choice.
* **Pair typefaces.** One display face used with restraint, one body face,
  optionally a utility face for data; always declare a real fallback stack
  (`font-family: "Fraunces", Georgia, serif`). Keep running text near 65
  characters wide, use one type scale, `text-wrap: balance` on headings and
  `font-variant-numeric: tabular-nums` where digits line up.
* **Layout.** Space sibling groups with flex or grid `gap`, not per-element
  margins. Give repeated items (cards, rows, badges) the same edges and
  padding, let content set their height, and pick a column count the items
  fill. Use borders, fills and shadows to set off the one element that needs
  it, not every block. Open with big-number tiles only when those numbers
  are the point of the page.
* **Structure is information.** Numbering, eyebrows, dividers and labels
  should encode something true about the content (a real sequence, a real
  grouping), not decorate it.
* **Dashboards and tools** are scanned, not read: summary before detail,
  state shown in form as well as numbers (a chip, a colored stripe with a
  label), status colors kept separate from the accent, and controls that
  look clickable.
* **Charts and diagrams** follow `topic.page.visuals` in any flat: the form
  chosen from the data's job, one scale per axis, chart text in text
  tokens, and hover details that also appear on tap and keyboard focus.
* **Avoid the common AI-generated looks** unless the user asks for them:
  cream background with a serif display and terracotta accent; near-black
  with one acid-green pop; a purple-to-blue gradient hero; Inter or Space
  Grotesk as the default face; emoji as section markers; everything centered;
  the same large radius and shadow on every card; an accent bar down the
  side of rounded cards; numbered 01/02/03 markers on content that is not a
  sequence.
* **Copy.** Write from the user's side: name things as people see them
  ("notifications", not "webhook config"). Use active voice; a button says
  what it does ("Publish") and the confirmation says what happened
  ("Published"). Errors say what went wrong and how to fix it, without
  apology. Prefer short, plain, specific sentences over clever ones, and
  avoid mannered devices: dash-set asides, "not X, but Y" framing, a colon
  before a reveal, scare quotes, and filler such as "it's worth noting".
* **Build cleanly.** Close every element and quote attributes; watch for
  rules that override each other's spacing; give form controls stable `id`s;
  for decorative or generative graphics use a canvas instead of long
  hand-written SVG paths.

### Check once, then publish

1. Save the Draft and read the save `warnings`; fix what applies.
2. `open_preview {slug, version: 0}` and wait until its `state` is `ready`
   (`topic.preview-verify`).
3. With a browser tool, check the preview once in four views (desktop and
   phone width, light and dark) and run the check script
   (`topic.preview-check`). Without a browser, fetch the HTML and its assets,
   check the status codes, and tell the user you did not see the rendered
   page.
4. Make one pass of fixes, save again, then `create_version` (`topic.approvals`).
   For a page that charts real numbers, spend that one look on the charts
   (`topic.page.visuals`).

Do not loop: no second round of screenshots or DOM probes. Further polish is
for the user to request; when they report something visibly broken, fix that
and look once more.

### Save warnings

`save_draft`, `save_draft_from_dir` and `save_page`
return `warnings: [{path, message, fix}]` for website and page bundles; the
text result lists them too. They never block or change the save. Docs bundles are not checked.

| Check | Applies to |
|---|---|
| no `<title>`, an empty one, or a scaffold placeholder ("Document", "Vite App") | the entry and every HTML file that is a full document (has a doctype, `<html>` or `<head>`) |
| no viewport meta tag | same |
| external `<script src>`, stylesheet, `modulepreload`, script or style `preload`, or import-map URL without an exact version; hosted font stylesheets | same |
| a relative or root-relative asset reference (script, stylesheet, icon, manifest, img/srcset, video/poster, audio, source, track, iframe, embed, object) that names no file in the bundle | static flats; the entry's references resolve from the flat root, where visitors open it. The server's HTML answers (the entry at `/`, `dir/index.html`, `about` → `about.html`, a single-page app's fallback) count only for an iframe, since a browser refuses HTML as a script, stylesheet or image. Import-map prefixes (`"lib/"`) are not files. Skipped for server flats (the handler owns routing) and pages with `<base href>`; an external base still gets the version check |
| a relative (not root-relative) asset path, which breaks at nested URLs | a single-page app's entry and the `not_found` page, which are served at any path |
| image file larger than 1 MiB | every image in the bundle |
| no `<link rel="icon" href>` and no root `favicon.ico` (an Apple touch icon is not a tab favicon) | a static flat's HTML entry |
| no `screenshot` in `flats.json` | static flats with an HTML entry |
| data loaded from another origin while the page renders: an absolute or protocol-relative URL literal as the first argument of `fetch`, `EventSource`, `WebSocket`, `Papa.parse` or a d3-style loader (`.csv`, `.json`, `.tsv`, `.text`, `.xml`), or the second of `XMLHttpRequest.open` | pages (`topic.page`): the entry's inline scripts and bundled `.js`/`.mjs` files; one warning per file and origin |

A single-page app without its entry file is already a validation error. The
checks read HTML tokens, not a rendered page: apart from the page data check,
which reads script text for URL literals, they do not see URLs that scripts
or CSS load, layout, color or contrast. At most 50 warnings are
listed per save, and long references are shortened.
