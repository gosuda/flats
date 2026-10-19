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
