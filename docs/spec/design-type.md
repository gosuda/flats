# Design content type

Status: agreed product spec (PRD) for the first design release. Technical
design: [design-type-TECH.md](design-type-TECH.md). Background:
[how Claude Artifacts' Design works](../research/claude-artifacts-design.md).

## Summary

A **design** (`type: "design"`) is a canvas of artboards that people and
agents work on together: screens, landing pages, posters, social images,
diagrams and click-through prototypes. An agent writes the artboards as plain
HTML files and lays them out on the canvas. People open the canvas in a
browser, look at every artboard side by side, play the prototype, and change
the layout, the text and declared design values in place. Every change is
kept in the flat's revision history.

A design follows the **Revision model** of
[content-models.md](content-models.md): it has no Draft, versions, publish or
previews, changes are live at once, and history is kept as revisions.

| Type | Written by | Content | Changes reach the URL |
|---|---|---|---|
| `docs` | people and agents together | Markdown documents | at once |
| `design` | people and agents together | a canvas of HTML artboards | at once |
| `page` | an agent | one HTML page | after an approved version and deployment |
| `flat` | an agent | a website or app | after an approved version and deployment |

## Content

1. **Explicit type.** A flat is a design only when it is created as one:
   `create_flat {type: "design"}` or `save_design` on a new slug. The type
   never changes afterwards.
2. **Files.** A design's content is a set of files:
   - `canvas.json`, the index (below);
   - artboards: one `.html` file per artboard;
   - assets: images, fonts, stylesheets, scripts and data files that artboards
     reference by relative path.

   Paths are clean relative paths. A design holds at most 512 files. Each file
   is limited by the host's upload limit, and the whole design by the per-flat
   disk quota.
3. **The index, `canvas.json`.**
   - `title`: the canvas name.
   - `boards`: one entry per artboard, keyed by its file path, with its frame
     on the canvas: `x`, `y`, `w`, `h` in CSS pixels (`w` and `h` 40 to 8000).
     An entry may add:
     - `title`;
     - `fill: true`, for a fluid web page: it is laid out at `w` but fills the
       window when opened full size.
   - `order`: the artboard paths, back to front.
   - `notes`: free notes on the canvas, each with `x`, `y`, `text` and an
     optional `w` and color. They are for headings over a group of artboards
     and comments to the reader.
   - `launch` (optional): open on the whole canvas (the default) or focused on
     one artboard.

   Every artboard listed must exist. An `.html` file that is not listed is not
   an artboard: it can still be linked or embedded.
4. **Artboards are plain HTML.** An artboard is one complete HTML document
   that also works when opened on its own. There is no template language.
   A few authoring rules make it editable in the browser:
   - **Editable text** is text written directly in an element's source. An
     element whose content is only text (and inline tags such as `<b>`, `<i>`,
     `<span>`, `<br>`) can be retyped in place. Text produced by script cannot.
   - **Tweaks** are named design values that people adjust without touching
     the source, declared in one JSON block:
     `<script type="application/json" data-flats-tweaks>`. Each tweak has a
     type (`color`, `number`, `text`, `select` or `boolean`), a label, a value
     and type-specific bounds or options. The host applies each tweak as a CSS
     custom property `--<name>` on the artboard's root element, so styles use
     `var(--accent)`.
   - **Links between artboards** (`<a href="checkout.html">`) move a played
     prototype to that artboard.
   - Any other HTML, CSS and JavaScript works, inside the artboard's sandbox.

## Viewing

5. **The canvas.** The design's URL opens a canvas that shows every artboard at
   its frame with its title, plus the notes. People pan and zoom it, and fit it
   to the window.
6. **Focus and play.** Any artboard opens full window. In this view,
   prototype links move between artboards, and a `fill` artboard fills the
   window.
7. **Live.** An open canvas shows every change as it is saved, whoever made
   it, without a reload.
8. **Errors.** An artboard that fails to load, or whose script throws, is
   marked on the canvas.

## Editing in the browser

9. **Who can edit.** People on a Private route can edit. A Public route shows
   the same canvas read-only. Artboard scripts never act for the viewer: they
   run sandboxed and cannot change the design or its history.
10. **What people can change.**
    - Move and resize artboards, and change their titles.
    - Add, move, edit and delete notes.
    - Retype editable text in an artboard. Clicking it in edit mode opens an
      editor over that text in the canvas; the change saves when the person
      confirms it.
    - Change tweak values.

    Anything else needs an agent, or a change to the files.
11. **Concurrency.**
    - Every browser change targets the state the person saw. If that part
      changed since, the change is refused and the canvas reloads the current
      state; nothing is merged silently.
    - Layout changes apply per field. Two people moving different artboards
      both succeed.
    - An agent's save of a file, or a rollback, replaces that file whole. A
      person's change prepared against the previous content of that file is
      then refused, as above, even if the targeted text looks the same.

## Revisions

12. **Revision model.** History follows content-models.md:
    - one agent write call is one revision;
    - a person's browser changes are grouped per person, closing after 30
      seconds idle or 5 minutes;
    - before an agent write or a rollback, open groups close as one revision
      that credits everyone who edited;
    - a revision covers the whole design, and unchanged files are shared, not
      copied;
    - separate revision and pin quotas;
    - rollback creates a new revision equal to the chosen one, adds only
      metadata, and protects its target from retention until it completes;
    - the disk quota is checked before a change applies.
13. **Managed on the canvas.** People with write access list revisions, see
    what each changed, pin and unpin them, and roll back from the canvas.
    Agents do the same through MCP. Neither needs approval.

## Agents

14. **Tools.**
    - `create_flat {type: "design"}` creates a design with a starter canvas
      and one artboard.
    - `save_design` writes one or more files and can delete files. Files it
      does not name are left as they are.
    - `get_design` reads the index, the file list or one file.
    - The revision tools list, read, pin and roll back revisions.
15. **Guidance.** The guide has a design topic: when to choose a design over a
    page or a website, the files and index, the authoring rules, the
    concurrency rule, and design practice. Its craft section covers layout
    that survives editing, accessible text and contrast, no filler copy, and
    real assets instead of placeholders.

## Lifecycle and access

16. **Runs at once.** Creating or saving a design changes the live canvas
    without approval. A new design is Private.
17. **Visibility and deletion.** Making a design Public, making it Private
    again, and deleting it each need operator approval, as for every flat. A
    Public design shows every saved change at once.
18. **No versions.** Draft, versions, previews, publish, deploy and version
    rollback do not apply. Their tools refuse a design with a pointer to the
    revision tools.
19. **Console.** The console lists designs with their own type label and
    icon, and links to the canvas. It shows no versions, previews, database or
    server settings for a design.

## Export

20. **HTML archive.** The canvas offers a `.zip` of the design's files, as
    currently saved. Each artboard in it opens on its own with its tweak
    values applied, and without any Flats editing code.
21. **Print.** A print view lays out one artboard per page at its frame size.
    The browser's print dialog saves it as PDF.

## Non-goals for the first release

- Design systems (shared tokens and components).
- PNG or SVG export.
- A properties panel, drawing tools (shapes, arrows) and structural edits to
  artboard markup in the browser.
- A template language or component runtime.
- In-page model calls.
- Real-time co-editing of artboard source (character-level merging).
- The viewer's selection as agent context through MCP.
- Version-model features.

## Current state

Before this spec, Flats has no design type and no revision store. The first
release adds the revision store described in content-models.md for designs
only; documents adopt it later.

## Open questions

None at the moment.
