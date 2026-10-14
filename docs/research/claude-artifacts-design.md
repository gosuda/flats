# How the Claude Artifacts "Design" type works

Status: research note, 2026-10-14. It records observed behavior of a third-party
product as input for the Flats design type
([spec](../spec/design-type.md)). It is not a Flats contract.

## Sources

Everything here was observed on 2026-10-14 from:

- the type definitions that the Claude Artifact tool returns to an agent (the
  `Design` type, release `1791907405-0b4c`, contract `0.2.47`, and the
  `Design System` type, release `1791558544-c1cf`);
- the reference pages that type serves to agents (`format.md`,
  `view-state.md`, `diagrams.md`, `print.md`, `questions.md`, `craft.md`,
  `design-system-components.md`);
- the runtime files served on one private, empty probe canvas: the editor page,
  its bundled editor script, its component runtime and an editing worker.

The editor script is minified. Statements about it describe behavior seen in
its strings and structure, not a reading of its source. No code from it is
reproduced here, and Flats must not reuse it.

## The model: a type plus a project

An *Artifact type* is a published editor application. A *canvas* is an ordinary
artifact made from that type: it serves the type's files read-only and keeps its
own content as files under `project/`. Slides, Dashboard, Docs and Design System
are other types built the same way.

A type declares the runtime capabilities its canvases inherit. Design uses:

| Capability | Used for |
|---|---|
| `artifact` (self publish) | the in-page editor saves the canvas as a new artifact version |
| `assets` | uploaded images and fonts, addressed as `/_blob/<id>` |
| `comments` | comments anchored to elements |
| `room` | presence of viewers, and what each viewer is looking at |
| `db` | a small shared store (editor metadata, thumbnails) |
| `downloads` | exports |
| `sample` | an in-page model call for quick edits |
| `user` | the viewer's profile |

## Data model

- `project/canvas.json` is the index:
  - `boards`: one entry per artboard, keyed by its file path, with its frame on
    the canvas (`x`, `y`, `w`, `h` in CSS px) and options. The options include a
    title, a page, `expand: "fill"` for fluid web pages, print mode and paper,
    `is_interactive`, layout guides and a corner radius.
  - `order`: the artboards back to front.
  - `notes`: canvas annotations (titles over a row, sticky notes, and shapes a
    person drew).
  - `pages` and `launch`: canvas pages, and whether the canvas opens as the
    whole canvas or one focused artboard.
  - `designSystems`: installed design systems.
- One `*.dc.html` file per artboard holds that artboard's complete source.
- Assets are uploads, never inline data URIs.
- An installed design system is copied into `project/ds/<folder>/`. Its
  `tokens.json` feeds the editor's color and text-style pickers.
- Limits: 512 files and 256 MB per canvas, 16 MB per publish call.

## The artboard format (`.dc.html`)

An artboard is one HTML file that also opens on its own:

- `<head>` loads a fixed support script. The template sits in `<x-dc>`, with a
  `<helmet>` block for page-level styles and font links.
- The template has dotted `{{path}}` holes (lookups only, never expressions),
  `<sc-for>` loops, `<sc-if>` branches and `<dc-import>`, which embeds another
  artboard as a child component.
- `<x-import component-from-global-scope="Ns.Comp">` mounts a real component from
  a design system bundle.
- A `<script type="text/x-dc" data-dc-script data-props='…'>` block holds a
  classic-JS class `Component extends DCLogic` with `renderVals()`. It returns the
  template's inputs: values, arrays and event handlers. `data-props` declares
  "tweaks", typed controls such as text, color, enum and range, plus the preview
  size.
- The runtime compiles the template to React 18 elements. It evaluates the logic
  class in the iframe with `new Function`. `hint-*` attributes render placeholders
  while a file is still being streamed in.

The format's constraints have one purpose: the editor must map every rendered
element back to a source location. The constraints are inline styles as the
editable surface, literal copy, no script-built UI, and flex or grid layout.
Selections are addressed as `File.dc.html#<tid>:<path>`, the element's
depth-first index and child path inside `<x-dc>`. That mapping lets a person
edit text and styles in place, and the result is written back into the file.

## Rendering and isolation

- The editor shell renders every artboard in its own iframe with
  `sandbox="allow-scripts allow-same-origin"` and a `srcdoc`. The shell sends the
  document, its parts and asset URLs over `postMessage`. Theme and canvas mode
  travel the same way.
- Isolation relies on the artifact being served from its own sandboxed origin.
  Design content is treated as untrusted cross-user input. It never runs in the
  host page and is fenced before it enters a model prompt.

## Editor features

Observed actions include:

- an infinite canvas with pan and zoom, pages, and canvas notes;
- in-place text editing, a properties panel (layout, flex/grid, typography,
  shadows, alignment), Tweaks, undo/redo, duplicate, grouping and z-order;
- Artboard, Text, Image, Note and Arrow tools, with layout guides;
- "Play", "Play on canvas" and "Play full screen" for clickable prototypes.
  Links between artboards move Play from screen to screen.
- per-artboard error reporting, with "Ask Claude to fix";
- export: PDF of selected or all artboards (fixed pages or flowing documents),
  PNG, copy as PNG or SVG, HTML (.zip), and "Send to" other apps.

## Saving, versions and concurrency

- Every save creates a new artifact version: an agent publish and a person's save
  in the editor alike. There is no Draft and no approval. Anyone with edit
  access can save, and sharing is a separate Share menu.
- A person's edits are held in the editor (`dirty`) until they save. There is no
  real-time co-editing of artboard source.
- A save over a newer version is refused, and the refusal names the files to
  read again. Agent and person resolve conflicts by re-reading and redoing the
  edit.
- Undo and redo are per file and survive a save within the session. An agent can
  restore a file from an older version by copying it into a new version, but no
  version-history screen was found in the editor itself.

## Collaboration and agent context

- `room` publishes each viewer's state: mode, page, visible, selected and errored
  artboards, selected elements with short labels, a `dirty` flag and an edit
  counter. The chat client attaches it to the person's next message, so the
  agent can resolve "this button" without guessing.
- Comments are anchored to elements and can be sent to the agent.

## Design systems

A separate *Design System* type holds a brand's README, `tokens.json` (colors per
theme, type scale, spacing, radii), components with live previews, and assets. A
canvas installs a copy, so the canvas keeps working for viewers who cannot open
the system. When the system ships a component bundle, artboards mount its real
components instead of look-alikes.

## Agent protocol

The type's instructions direct the agent to:

1. pick the user's default design system, if any;
2. write `canvas.json` first, then publish artboards one at a time, so the
   canvas fills while the person watches;
3. not render, screenshot or re-read its own output unless asked;
4. use uploads for every binary asset, avoid filler copy, keep text accessible,
   and build diagrams from separate placed elements so each box can be moved.

## What this means for Flats

| Claude Design | Flats today | Fit |
|---|---|---|
| Type = shipped editor + per-canvas files | the built-in docs app plus flat bundles | same shape: a built-in design app plus the flat's files |
| Save = new version, no approval | the Revision model ([content-models](../spec/content-models.md)) | Flats adds revisions per edit unit, pins and quotas |
| Conflict refusal on save | docs merges live; the spec keeps merge rules | design files need per-file optimistic concurrency at first |
| Sandboxed iframe per artboard on the artifact's origin | per-flat hostnames, served by Flats | artboards need an origin separate from the console |
| `room` context in the chat prompt | no client integration | expose the view state through MCP instead |
| `sample` quick edits | Flats has no model | out of scope |
| Proprietary editor bundle | — | write our own; never reuse theirs |

Borrow the ideas that make agent and person work on the same files:
- an index plus one file per artboard;
- element addresses that map back to source;
- tweaks declared next to the artboard;
- a conflict rule that names the files to re-read.

Skip the in-page model and the dependency on a chat client.
