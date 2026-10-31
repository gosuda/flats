## Notebooks

A notebook (`type: "notebook"`) is a tree of pages that people and agents
edit live. Each page holds blocks: paragraphs, headings, lists, to-dos,
toggles, quotes, callouts, code, dividers, images, links to other pages and
inline databases. An inline database is a small typed table that people view
as a table or a kanban board, sorted and filtered. You read and write it as
structured data.

Choose a notebook for wikis, meeting notes, reading lists and trackers: prose
and records on the same page, such as a project page with its goals next to a
task table people sort by status. Choose a document (`topic.docs`) for
Markdown files people edit as text, a board (`topic.board`) to arrange ideas
spatially, a design (`topic.canvas`) for pixel-exact screens, and a page
(`topic.page`) for one finished HTML document.

A notebook runs at once and has no Draft, versions, previews, publish or
approval for its content. A new notebook is Private. Making it Public,
Private again, or deleting it needs approval (`topic.approvals`). A Public
notebook is read-only for its viewers and shows every change to everyone at
once, so tell the user before you write to one. Version tools refuse a
notebook (`refusal.invalid`); notebook tools refuse other types
(`refusal.not_notebook`).

### Create and read

1. `create_flat {slug, name?, type: "notebook"}` creates a notebook with one
   empty top-level page, or `update_notebook` on a new slug creates it and
   applies your operations. Give the user `private_url`: it opens the
   notebook. A page's address is `<private_url>#/<page id>`.
2. Read before you change an existing notebook. People may have edited since
   you last looked. `get_notebook` takes one selector:
   - `{slug}` returns the page tree: each page's reference, title, parent and
     position.
   - `{slug, page}` returns that page's blocks with their children, and the
     schema and views of its inline databases. `format: "markdown"` returns
     the page as Markdown instead (Markdown, below).
   - `{slug, database}` returns rows, optionally with an ad-hoc `sort` and
     `filter`, or `{slug, database, view}` returns them through a saved view.
     Give a view or a sort and filter, not both.
   - A selector is an id (`"p_…"`) or a full reference
     (`{"id": "p_…", "inc": "7"}`), so you can follow a page link or a tree
     entry directly.
3. A result holds up to `limit` records (default 500, at most 2,000) or about
   1 MiB. While `complete` is false, call `get_notebook {slug, cursor: next}`
   with nothing else for the following page. Every page of one read comes
   from the same state, even while people edit. A cursor lasts 60 seconds
   after its last page; an expired one is refused with `conflict` (reason
   `read_expired`), so read again from the start. A block or row larger than
   one page arrives in explicit continuation parts.
4. `state` (`{store, seq}`) identifies the notebook's current state, people's
   unrecorded edits included. `pending` says people have edits that are not
   a revision yet. `revision: N` reads retained revision N the same way.

When read admission returns `unavailable` with `retry: true`, its `detail`
contains an `admission` ticket, queue `position` and `expires_in_ms`. Repeat
that read's selectors unchanged with `retry_hint: admission` before expiry
to keep your place in the host-wide queue. The management API uses the
`retry_hint` query parameter; `flats notebook get` uses `--retry-hint`.
Omitting the ticket joins the back of the queue. An unknown, expired or
already-used ticket returns `conflict` with reason `read_expired`; start a
new read without it. Admission tickets are not continuation cursors, so do
not combine `retry_hint` with `cursor` or `op_id`.

### References and versions

Every page, block, database, property, option, row and view you read carries
`ref` (`{"id": "b_…", "inc": "12"}`), `ver`, and `v` (the version of each of
its value groups). Ids start with `p_`, `b_`, `d_`, `pr_`, `o_`, `r_` or
`v_` for pages, blocks, databases, properties, options, rows and views.

- Copy `ref` unchanged into every write that targets an existing item, as
  the destination parent, the ordering anchor, or a property, option or view
  you depend on. `inc` changes when an item is deleted and comes back (undo,
  recovery, rollback), so a stale reference is refused instead of editing
  the wrong incarnation. Never build `inc` yourself.
- `inc`, `ver` and `seq` are decimal strings. Compare and copy them as
  strings.
- `ver` grows with every change to the item. A page's `ver` covers its
  blocks and inline databases, not its subpages; a database's covers its
  schema, options, views, rows and cells.
- A page-link block and a rich-text page link store only the page id. They
  work again if that page comes back.

### Write

`update_notebook {slug, op_id, ops, guards?, message?, assets?}` applies all
operations or none, and is one revision.

```json
{"op_id": "release-tasks-1",
 "guards": {"items": [{"ref": {"id": "p_aaaaaaaaaaaaaaaaaaaaaaaaaa", "inc": "7"}, "ver": "41"}]},
 "ops": [
  {"op": "create_block", "ref": "intro", "content": {"kind": "paragraph",
    "text": [{"text": "Open items from the review", "marks": ["bold"]}]},
   "place": {"page": {"id": "p_aaaaaaaaaaaaaaaaaaaaaaaaaa", "inc": "7"}, "parent": null, "before": null}},
  {"op": "create_database", "ref": "tasks", "block_ref": "tasks-block", "title": "Tasks",
   "place": {"page": {"id": "p_aaaaaaaaaaaaaaaaaaaaaaaaaa", "inc": "7"}, "parent": null, "after": {"tmp": "intro"}}},
  {"op": "create_property", "ref": "status", "database": {"tmp": "tasks"},
   "definition": {"name": "Status", "type": "select"}},
  {"op": "create_option", "ref": "todo", "property": {"tmp": "status"}, "name": "Todo", "color": "gray"},
  {"op": "create_row", "ref": "row1", "database": {"tmp": "tasks"}, "before": null,
   "cells": [{"property": {"tmp": "tasks.title"}, "value": "Fix the login timeout"},
             {"property": {"tmp": "status"}, "value": {"tmp": "todo"}}]}],
 "message": "Add review follow-ups"}
```

- Creations take a temporary name in `ref`. Later operations in the same
  call name the item as `{"tmp": "<name>"}`; a name cannot refer forward.
  `create_database` also creates its block (`block_ref`), its title property
  `<ref>.title` and its first table view `<ref>.view`.
- The result's `refs` maps each temporary name to `{ref, ver}`, and
  `mapping.copies` maps each duplicated item to its copy. When the mapping
  does not fit the result, `mapping` is `{complete: false, total, cursor}`:
  read every entry with `get_notebook {slug, op_id, cursor}`, following
  `next`. Keep the mapping until you have it: current content cannot rebuild
  which new id belongs to which name.
- A placement is `{page, parent, before}` or `{page, parent, after}`. `parent`
  is a list item, to-do or toggle block, or `null` for the page level;
  `before: null` means at the end.
- Items and values you do not name stay as they are. Each write replaces one
  whole value group: a page's title or place, a block's content or place, a
  database title, a property's name and type, an option's name and color, an
  order, one cell, a row's place, or one view's definition.
- Operations: `create_page`, `set_page_title`, `move_page`, `delete_page`
  (with its subtree), `duplicate_page`; `create_block`, `set_block`,
  `turn_blocks`, `move_blocks`, `delete_blocks`, `duplicate_blocks`;
  `create_database`, `set_database_title`; `create_property`,
  `set_property`, `delete_property`, `set_property_order`; `create_option`,
  `set_option`, `delete_option`, `set_option_order`; `create_row`,
  `set_cell`, `move_rows`, `delete_rows`; `create_view`, `set_view`,
  `delete_view`, `set_view_order`; and `markdown_to_blocks`. Their exact
  fields are in the tool's input schema.
- `op_id` (1 to 128 characters: letters, digits, `.`, `_`, `:`, `-`) is
  required. Resending the same request with the same `op_id` returns the
  first result and applies nothing again, for at least 24 hours after its
  revision is recorded, so retry with it after a timeout. The same `op_id`
  with a different request is refused with `conflict`.
- On a new slug the notebook exists only if the call applies: a refused call
  leaves no flat behind.

The result is `{state, revision, revision_pending, replay, created, refs?,
mapping, applied, warnings, copies?}`. `revision` is null, with a
`revision_pending` warning, while the call's revision is still being
recorded; the call itself applied, so do not resend it with a new `op_id`.

### Content

| Block `kind` | Fields |
|---|---|
| `paragraph`, `heading1`, `heading2`, `heading3`, `quote` | `text` |
| `bullet`, `numbered` | `text`; holds child blocks |
| `todo` | `text`, `checked`; holds child blocks |
| `toggle` | `text` (the summary), `open_default`; holds child blocks |
| `callout` | `text`, `tone`: `neutral`, `info`, `success`, `warning`, `danger` |
| `code` | `text` (a plain string), `language` |
| `divider` | none |
| `image` | `asset` (a SHA-256 hash), `alt`, `caption` (rich text) |
| `page_link` | `target` (a page id) |
| `database` | `database`; created only by `create_database` |

- Rich text is a list of runs `{text, marks, link?}`. `marks` is any of
  `bold`, `italic`, `strike` and `code`; a link is `{"url": "https://…"}`
  (`http`, `https` or `mailto`) or `{"page": "p_…"}`. Rich text never holds
  HTML.
- Pages nest at most 8 levels; blocks nest at most 6 levels below the page,
  and only under list items, to-dos and toggles.
- `set_block` replaces the block's whole content and can turn a text kind
  into another text kind or code, and code into a text kind. Dividers,
  images, page links and databases cannot change kind. `turn_blocks` changes
  several blocks at once. Children of a block whose new kind holds none move
  up, right after it.
- Property types: `title` (exactly one, always first), `text`, `number`
  (`format`: `plain`, `comma`, `percent`), `select`, `multi_select`, `date`,
  `checkbox`, `url`. Option colors: `gray`, `brown`, `orange`, `yellow`,
  `green`, `blue`, `purple`, `pink`, `red`.
- Cell values: text as a string; a number as a decimal string such as
  `"12.5"`; a select as an option reference or `null`; a multi-select as a
  list of option references; a date as `"2026-10-30"` or
  `"2026-10-30T14:00:00"` (no time zone); a checkbox as `true` or `false`;
  a URL as an `http` or `https` string or `null`.
- Changing a property's type converts the values it can (number and text
  both ways, select and multi-select both ways, text to select creating one
  option per distinct value) and empties the rest at once. Read the rows
  first and check with the user before a change that empties values; the earlier
  values stay in the revision recorded before it. Deleting a property or an
  option also removes the sort keys, filter conditions and columns of views
  that used it, and a kanban grouped by a deleted property becomes a table.
- A view is `{name, display, sort, filter}`. `display` is
  `{layout: "table", visible, widths}` or
  `{layout: "kanban", group_by, visible, show_empty}` with a select
  property. Filter conditions all must hold:
  - title, text and URL: `contains`, `does not contain`, `is`, `is not`,
    `is empty`, `is not empty`;
  - number: `=`, `≠`, `<`, `≤`, `>`, `≥`, `is empty`, `is not empty`;
  - select: `is`, `is not`, `is any of`, `is empty`, `is not empty`;
  - multi-select: `contains`, `does not contain`, `is empty`, `is not empty`;
  - date: `is`, `before`, `after`, `on or before`, `on or after`,
    `is empty`, `is not empty`;
  - checkbox: `is checked`, `is not checked`.
- A view write is checked against the current schema when it applies. A
  view naming a deleted property or option, or a setting that does not fit
  the property's current type, refuses the whole call.
- `assets` adds images in the same call: `[{"ref": "logo", "data": "<base64>"}]`.
  PNG, JPEG, GIF and WebP only, each at most the upload limit and
  24 megapixels; no SVG. An image block names an asset by hash; Markdown
  image paths map to assets through `asset_refs`
  (`[{"path": "logo.png", "asset": {"tmp": "logo"}}]`).

Limits per notebook: 2,000 pages, 2,000 blocks per page, 8,000 characters
of rich text per block, 200 inline databases, 5,000 rows, 32 properties and
10 views per database, 100 options per property, 64 MiB of data per page
and per database, and 500 operations per call. A write over a limit is
refused with `notebook_capacity`.

### Markdown

- `markdown_to_blocks {place, markdown, asset_refs?}` turns Markdown into
  blocks at `place`. It reads CommonMark with GitHub tables, task lists and
  strikethrough. `#` to `###` become headings (deeper ones heading 3); lists,
  task lists, quotes, fenced code with its language and `---` become their
  kinds; `> [!NOTE]`, `[!TIP]`, `[!IMPORTANT]`, `[!WARNING]` and
  `[!CAUTION]` become callouts. A table becomes an inline database: the first
  column the title, the others text properties, and one table view.
- Nesting deeper than the limit becomes siblings at the deepest level. Raw
  HTML, footnotes and other constructs without a block kind stay as literal
  paragraph text, and `warnings` lists each with its line. Read the warnings
  and tell the user what changed.
- An image converts only when its path is in `asset_refs`; any other image
  path refuses the whole call with `invalid` and names the path. Nothing is
  fetched from the internet.
- `get_notebook {slug, page, format: "markdown"}` returns the page as
  `markdown`, with `<!-- flats:block b_… -->` before each block, and a
  `references` list mapping each marker to its `ref` and `ver`. Copy those
  references into writes. Toggles, page links, image captions and non-text
  cells come back as readable approximations with warnings: this is a read
  format, not a lossless copy. Sending the Markdown back through
  `markdown_to_blocks` creates new blocks; it never edits the blocks the
  comments name.

### People editing at the same time

- Writes to different value groups all apply: you can set one cell while a
  person edits another cell of the same row, or retitle a page while
  someone types in it.
- In the same value group, the later write wins whole. Text inside one block
  is not merged: your `set_block` replaces what a person typed there since
  your read unless you guard it.
- If an item you target was deleted, or came back under another `inc`, the
  whole call is refused with `conflict`. Read again and rebuild the call.
- `guards.state` (the `state` of your read) refuses the call if anything in
  the notebook changed. `guards.items` (`[{ref, ver}]`) refuses it if any of
  those items changed. Both include people's edits that are not yet a
  revision. Guard every block you rewrite from what you read, and the page
  or database when you replace a part of it.
- After a guard refusal, read again, apply your change to what people wrote
  and send it with a new `op_id`. Never resend without the guard to
  overwrite them.

### History

- One `update_notebook` call is one revision. A person's edits are grouped
  per person (closed after 30 s idle or 5 min), and open groups close into a
  revision before your write.
- Use the shared revision tools, without approval: `list_revisions` (with the
  pages each revision changed), `get_revision` (with `page` or `database`,
  read as `get_notebook` reads, or `get_notebook {slug, revision}`),
  `rollback_revision` (a new revision equal to that one's whole notebook,
  never refused for the disk quota, applied to open tabs at once) and
  `pin_revision`. When `rollback_revision` answers with a `warning` and no
  revision, the rollback applied but its revision is not recorded yet: do not
  call it again.
- To bring back one deleted page rather than the whole notebook, tell the
  user to open History in the notebook, open a revision that still has the
  page, and restore it. It returns with its original ids if they are free.
- Old unpinned revisions are deleted beyond the flat's quota (default 100,
  20 pins); the newest revision is always kept.

### Verify

1. Read `refs`, `mapping` and `warnings`, then `get_notebook {slug, page}` for
   each page you changed: every block is in the right place and kind, and
   every database has the properties, options, views and rows you meant.
2. For a database, read it through the view the user will look at.
3. Give the user `private_url` with the page's `#/<page id>`. If you have a
   browser tool, open it and look at the changed page.
