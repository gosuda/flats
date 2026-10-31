# Notebook content type

Status: partly implemented. The notebook type runs end to end, but no success
criterion has been measured yet; see [Current state](#current-state). The
technical design is in
[notebook-technical-design.md](notebook-technical-design.md).

This page sets what the first notebook release (N1) must do, why, and the
rules a notebook keeps. The technical design decides the storage format, the
protocol and where computation runs.

## Summary

A **notebook** (`type: "notebook"`) is a tree of pages that people and agents
edit live. Each page holds a list of blocks: paragraphs, headings, lists,
to-dos, toggles, quotes, callouts, code, dividers, images, links to other
pages, and inline databases. An inline database is a small typed table that
people view as a table or a kanban board, sorted and filtered. People use a
notebook for wikis, notes, trackers and task lists. Agents read it through MCP
as structured data and write pages, blocks and rows the same way people do.

A notebook follows the **Revision model** of
[content-models.md](content-models.md) unchanged. It has no Draft, versions,
previews or publish. Edits apply at once without approval, and history is
kept as revisions.

| Type | Written by | What people change in the browser | Changes reach the URL |
|---|---|---|---|
| `docs` | people and agents | Markdown text | at once |
| `design` | people and agents | artboard layout, text written in the source, tweaks and notes | at once |
| `board` | people and agents | every element of a whiteboard | at once |
| **`notebook`** | **people and agents** | **every page, block, database, row and view** | **at once** |
| `page` | an agent | nothing | after an approved version and deployment |
| `flat` | an agent | nothing | after an approved version and deployment |

## Why

- **No type holds structured data that people keep editing.** A `docs` flat
  is a set of Markdown documents with no structure people can query: a task
  list in Markdown cannot be sorted by due date or grouped by status. A
  `board` is spatial. A `page` or `flat` is written by an agent and changes
  only after approval.
- **Wikis and trackers need both prose and records.** A team keeps a project
  page with its goals and decisions next to the list of its open tasks. In
  N1 both live on the same page: blocks for the prose, an inline database for
  the tasks.
- **Agents should keep these up to date.** An agent that reads a PR
  discussion should be able to add the decisions to the right page and the
  follow-ups as rows in the tracker, without rewriting a Markdown file people
  are editing.
- **Hosted notebooks need accounts and outside servers.** Flats already has a
  single binary, Private networking, the Revision model and MCP.

## Users and scenarios

| User | Scenario |
|---|---|
| An operator keeping notes | Writes meeting notes and a reading list, nests them under a few top pages and finds them again from the sidebar. |
| A small team (tailnet members) | Keeps a team wiki and a task tracker in one notebook, and edits the same page at the same time. |
| An agent | Turns a Markdown summary into blocks on the right page, adds rows to a tracker, and reads the tracker filtered by status. |
| An outside viewer | Reads, read-only and live, a notebook the operator approved as Public. |

A typical flow:

1. An agent creates a notebook with `create_flat {type: "notebook"}` and gives
   the user its Private URL.
2. The user adds a page "Release 0.16", writes its goals and adds an inline
   database "Tasks" with Status, Owner and Due properties.
3. The user asks the agent to "add the open items from this PR thread to
   Tasks". The agent reads the database's properties and options, then in one
   write adds the rows. The open page shows them live, and that write becomes
   one revision.
4. Teammates switch the database to its kanban view and drag cards from Todo
   to Done. Each person's edits become their own revisions.
5. Someone deletes a subpage by mistake. A person with write access opens the
   History panel, opens a revision that still has the page, and restores it.

## Principles

1. **Structure people can edit and agents can read.** Every page, block,
   database, row and view is structured data. People change all of it in the
   browser. Agents read and write the same data through MCP, never by
   scraping rendered HTML or editing a serialized file.
2. **Concurrent editing is the default.** Several people and agents edit the
   same notebook at the same time. An edit that passes permission,
   validation, limit, disk quota and guard checks is never refused because
   someone else changed another value at the same moment. When two writers
   change the same value, the [concurrency rules](#concurrency-rules) decide;
   the server's order is final.
3. **Nothing in retained history disappears.** Every page, block, row and
   asset that a retained revision records can be viewed and recovered.
   Intermediate states between revisions, and revisions the quota deleted
   automatically, are not guaranteed.
4. **Text is text.** Notebook content never runs script or HTML. Rich text is
   a list of text runs with a fixed set of marks.
5. **Flats' existing rules apply unchanged.** A notebook does not redefine the
   Revision model, Private and Public access, the actions that need approval
   (visibility changes and deletion), the disk quota or fixed types.

## Content

1. **Explicit type.** A flat becomes a notebook only through
   `create_flat {type: "notebook"}` or a notebook write to a new slug. The type
   never changes, and saves of another type are refused.
2. **Pages.** A notebook is a tree of pages.
   - Each page has an id unique within the notebook, a title (plain text,
     possibly empty), a parent page or none, a position among its siblings,
     and a list of blocks.
   - Pages without a parent are top-level pages. A new notebook has one
     top-level page.
   - The tree is at most **8 levels** deep: a top-level page is level 1. A
     move or creation that would put any page below level 8, or a page inside
     its own subtree, is refused.
   - Deleting a page deletes its whole subtree. The browser asks for
     confirmation inline, naming how many subpages go with it. Deleted pages
     are recovered from history ([Revisions](#revisions)).
   - A notebook always keeps at least one page. Deleting the last top-level
     page is refused.
3. **Blocks.** A block has an id unique within the notebook, a kind, and the
   fields of its kind. The block kinds in N1:

   | Block | Fields |
   |---|---|
   | Paragraph | rich text |
   | Heading 1, 2, 3 | rich text |
   | Bulleted list item | rich text, child blocks |
   | Numbered list item | rich text, child blocks |
   | To-do | rich text, checked, child blocks |
   | Toggle | rich text (the summary), open by default or not, child blocks |
   | Quote | rich text |
   | Callout | rich text, a tone (neutral, info, success, warning, danger) |
   | Code | plain text, a language label |
   | Divider | none |
   | Image | an asset of the notebook, alt text, a rich-text caption |
   | Page link | a target page id |
   | Inline database | a database ([Databases](#databases)) |

   - List items, to-dos and toggles may hold child blocks, nested up to 6
     levels below the page. Other kinds have no children.
   - Consecutive numbered list items at the same level number themselves
     1, 2, 3 in display. The number is not stored.
   - A toggle's open or closed state in the browser is personal and not
     saved. Its "open by default" field is content.
   - A page link shows the target's current title and follows it on click.
     If the target is deleted, the block stays and shows "Missing page". If
     the page is restored later with its original id, the link works again.
   - **Turning a block into another kind** keeps its id. Only these
     conversions exist:

     | From | To | What is kept |
     |---|---|---|
     | a text kind (paragraph, headings, list items, to-do, toggle, quote, callout) | another text kind | the rich text; fields only the new kind has get their defaults (not checked, closed by default, neutral tone) |
     | a text kind | code | the text without marks; a link keeps its text and loses its target |
     | code | a text kind | the text as one unmarked run |

     Divider, image, page link and inline database blocks cannot be turned
     into another kind, and no block can be turned into them; the menu does
     not offer it. Child blocks stay when the new kind holds children and
     move to the parent level, right after the block, when it does not.
     Turning several selected blocks into a kind is offered only when every
     one of them can take it.
4. **Rich text.** Rich text is a list of runs. Each run has text and any of
   the marks **bold**, *italic*, ~~strike~~, `inline code` and a link.
   - A link is an `http`, `https` or `mailto` URL, or a page of the same
     notebook by id. Other schemes are refused.
   - Rich text never holds HTML, images, mentions or equations.
   - Line breaks inside a run are allowed (Shift+Enter in the browser).
5. **Assets.** Images are stored as files of the notebook. The host accepts
   PNG, JPEG, GIF and WebP, not SVG. File size follows the host upload limit,
   and pixel count has its own limit. An asset that a retained revision
   references stays as long as that revision exists.
6. **Limits.** Limits apply to the current notebook, and the host refuses an
   edit that exceeds one with the reason. N1 targets, fixed by measurement
   before release ([Open questions](#open-questions)):

   | Limit | Target |
   |---|---|
   | Pages per notebook | 2,000 |
   | Blocks per page | 2,000 |
   | Characters of rich text per block | 8,000 |
   | Inline databases per notebook | 200 |
   | Rows per database | 5,000 |
   | Properties per database | 32 |
   | Options per select or multi-select property | 100 |
   | Views per database | 10 |
   | Data per database (serialized row values) | 64 MiB |
   | Data per page (serialized blocks) | 64 MiB |

   Each limit is judged on its own. If a lowered limit leaves an item already
   over it, the notebook and rollbacks to its revisions are not refused. Only
   edits that increase how far that item is over the limit are refused.

## Databases

7. **An inline database lives in one block.** It has a title, a list of
   properties, a list of rows and a list of views. Deleting the block deletes
   the database with it. Moving the block moves it. A database is not shared
   between pages in N1.
8. **Properties.** Each property has an id, a name unique within the
   database, and a type. Exactly one property has the type title, and it is
   always first; it cannot be deleted or change type.

   | Type | Value | Empty value |
   |---|---|---|
   | Title | plain text | empty text |
   | Text | plain text | empty text |
   | Number | a finite decimal number, shown with an optional format (plain, comma-grouped, percent) | none |
   | Select | one option | none |
   | Multi-select | a set of options | empty set |
   | Date | a calendar date, with an optional time and no time zone conversion | none |
   | Checkbox | true or false | false |
   | URL | an `http` or `https` URL | none |

   - Select and multi-select options are items of their own, each with an
     id, a name unique within the property and a color. Rows store option
     ids, so renaming or recoloring an option changes it in every row at
     once. Deleting an option removes it from every row.
   - Changing a property's type converts each value where a conversion is
     defined (number to text, text to number when it parses, select to
     multi-select, multi-select to select keeping the first option, text to
     select creating one option per distinct value) and empties the rest.
     The options of a property that stops being a select or multi-select are
     deleted. The browser says how many values will be emptied before the
     change applies. The previous values stay in the revision closed just
     before it.
   - Property deletion and type changes also adjust the views that depend
     on them (rule 10a).
9. **Rows.** A row has an id and one value per property. A missing value is
   the empty value of its type. Rows are not pages in N1: they hold no
   blocks.
10. **Views.** Each database has at least one view. A view has an id, a name,
    a layout and its settings:
    - **Table.** Rows as lines and properties as columns. Settings: visible
      properties and their order, and column widths.
    - **Kanban.** One column per option of a chosen select property, plus a
      "No value" column, in option order. Each card shows the title and the
      visible properties. Moving a card to another column sets that
      property's cell of the row. Settings: the grouping property, visible
      properties, and whether empty columns are shown.
    - **Sort.** An ordered list of properties, each ascending or descending.
      Empty values sort last in both directions. Ties keep the manual row
      order.
    - **Filter.** A list of conditions that all must hold (AND). Each
      condition names a property, an operator and a value:

      | Property type | Operators |
      |---|---|
      | Title, Text, URL | contains, does not contain, is, is not, is empty, is not empty |
      | Number | =, ≠, <, ≤, >, ≥, is empty, is not empty |
      | Select | is, is not, is any of, is empty, is not empty |
      | Multi-select | contains, does not contain, is empty, is not empty |
      | Date | is, before, after, on or before, on or after, is empty, is not empty |
      | Checkbox | is checked, is not checked |

      Text comparison ignores letter case.
    - **Manual order.** Each database has one stored row order, shared by all
      its views. Without a sort, a table shows rows in that order and a
      kanban column shows its cards in that order. Dragging a row in a
      table, or a card within or across kanban columns, moves the row in the
      stored order to just before the row it was dropped above, or to the
      end. With a sort, rows and cards show in sorted order and dragging to
      reorder is unavailable, and the view says why; dragging a card to
      another column still sets the grouping cell, and the card lands at its
      sorted place.
    - View settings are content: changing them changes them for everyone and
      is part of history. N1 has no personal, unsaved view settings.

10a. **Views stay valid when the schema changes.** Every saved view refers
    only to properties and options that exist, with settings valid for the
    property's current type.
    - When a property is deleted, or its type changes, the same edit removes
      the sort keys, filter conditions and visible-property entries that no
      longer fit it. A kanban whose grouping property is deleted or stops
      being a select becomes a table view with the same name and settings.
    - When an option is deleted, filter conditions that name it are removed.
    - Each view changed this way shows a notice the next time someone opens
      it, naming what was removed. The notice is not content and is not
      saved in history.
    - A view write, from a person or an agent, is checked against the schema
      when it applies, not against what the writer saw. A person's view
      write that names a missing property or option, or a setting that does
      not fit the current type, is applied without those parts and reported
      as adjusted. An agent's call with such a view write is refused whole
      (rule 36).
11. **Rows that stop matching.** A row added in a filtered view gets the
    values that make it match the view's `is` and `is checked` conditions,
    so it does not disappear at once. If a row the person added or edited in
    this view, including a card they moved to another column, no longer
    matches, the view keeps it visible to that person until they leave the
    view, marked as hidden by the filter. Other people's views follow the
    filter at once.

## Viewing

12. **Layout.** A sidebar lists the page tree. Each page expands and collapses
    its subpages, and the expanded state is personal and not saved. The main
    area shows the selected page: its breadcrumb (ancestors), title and
    blocks.
13. **Routing.** The address of a page is `/#/<page id>`. Opening a notebook
    without a page id shows its first top-level page. An id that does not
    exist shows "This page does not exist" with a link to the first page, and
    for writers a link to History to look for it.
14. **Live.** An open notebook shows every change without a reload, whoever
    made it, including changes to the page tree and to pages other than the
    open one.
15. **Connection state.** The screen always shows whether the notebook is
    saving, saved or disconnected
    ([Disconnection and reconnection](#disconnection-and-reconnection)).
16. **Theme.** The notebook follows the light or dark theme. Callout tones,
    option colors and code stay readable in both.
17. **Phone width.** At about 400 px the sidebar becomes a menu and the page
    never scrolls sideways; only code blocks and table views scroll inside
    their own box.
18. **Accessibility.**
    - With the keyboard alone, people move through the sidebar, open a page,
      move between blocks, edit text, change block kinds, reorder blocks and
      rows, and use every database control.
    - A screen reader reads the page tree as a tree, headings as headings,
      lists as lists, to-dos as checkboxes and table views as tables.
    - Color is never the only way to convey meaning: option colors always
      come with the option name, and another person's activity is also named.

## Editing in the browser

Editing is available on Private routes only.

19. **Writing.** People type into blocks directly.
    - Enter at the end of a block creates a new block below it: a list item,
      to-do or numbered item continues the list, other kinds create a
      paragraph. Enter in an empty list item ends the list.
    - Enter inside a block splits it at the caret. Backspace at the start of
      a block merges it into the previous one when both hold rich text, and
      otherwise selects the previous block.
    - Tab and Shift+Tab nest and unnest a block under the block above, within
      the nesting rules.
    - Markdown shortcuts at the start of an empty block change its kind:
      `# `, `## `, `### `, `- ` or `* `, `1. `, `[] `, `> `, ```` ``` ````,
      `---`. Typing `**text**`, `*text*`, `~~text~~` and `` `text` `` applies
      the mark.
    - While an input method (IME, for example for Korean) is composing,
      remote changes to that block do not break the composition. They apply
      after the composition ends.
20. **Block menu.** Typing `/` in a block, or pressing the + beside an empty
    block, opens a menu of block kinds filtered as the person types. Every
    block has a handle that opens actions (turn into, duplicate, move to
    another page, copy link, delete) and drags the block to reorder it, also
    into or out of a toggle or list.
21. **Selection.** People select several blocks by dragging across them or
    with Shift and the arrow keys, then move, delete, duplicate or turn them
    into another kind together.
22. **Paste.** Pasting plain text inserts it into the block. Pasting
    Markdown, or HTML from another application, converts it with the same
    rules as `markdown_to_blocks` (rule 44), keeping only what those rules
    produce. Pasting an image uploads it as an image block.
23. **Pages.** People create a page from the sidebar, as a subpage from a
    page's menu, or as a page link block with "New page". They rename a page
    in its title, drag pages in the sidebar to reorder or nest them, and
    delete a page from its menu.
24. **Databases.** People edit cells in place, add and delete rows,
    properties, options and views, change a view's sort and filter, drag
    rows (table, without sort) and cards (kanban), and resize columns.
25. **Personal undo and redo.** As on a board: undo reverses only the
    person's own edits, never other people's or agents'.
    - The unit is one typing run in one block (closed by a pause, a caret
      jump or leaving the block), or one action such as a paste, a move, a
      deletion or a cell edit.
    - If someone else later changed a value the person had changed, undo
      leaves that value and says "Changes other people made later were not
      undone".
    - Undoing a person's own deletion restores the page, block or row with
      its original id.
    - Each tab has its own history. A reload or a whole-notebook rollback
      clears it.

## Collaboration

26. **Presence.** Private editors see who else has the notebook open, which
    page each person is on, and the block or cell each person is editing,
    marked with their name. This mark is not a lock.
27. **Identity.** On Tailscale routes the notebook shows the trusted
    Tailscale identity as verified. On Local and NetBird routes it uses the
    name entered on first opening and shows it as unverified. A value the
    client sends can never earn the verified mark.
28. **Agent activity.** An agent's write appears as a short notice, such as
    "The agent added 4 rows to Tasks", with a brief highlight of what changed.
    Presence is never exposed to agents.

## Concurrency rules

The issue calls the unit of last-writer-wins a *bundle*. This spec calls it a
**value group**: values that only make sense together change together and
are overwritten together.

29. **Value groups.**

    | Group | Contains |
    |---|---|
    | Page title | the page's title |
    | Page place | the page's parent and position among its siblings |
    | Block content | the block's kind and every field of that kind (rich text, checked, tone, language, asset, alt, caption, target) |
    | Block place | the block's page, parent block and position |
    | Database title | the database's title |
    | Property | one property's name, type and number format |
    | Option | one option's name and color |
    | Option order | the order of one property's options |
    | Property order | the order of the properties |
    | Cell | one row's value for one property |
    | Row place | the row's position in the stored order |
    | View | one view's name, layout and all its settings |
    | View order | the order of the views |

30. **Different values all apply.** If one person retitles a page while
    another types in one of its blocks, both stay. Two people editing
    different blocks, or different cells of the same row, both stay.
31. **In the same group, the last write stays.** When two writers change the
    same group, the value the server received later stays. Text inside one
    block is not merged character by character in N1: the later block
    content wins whole.
    - The person whose text was overwritten gets a notice. Its **Keep my
      text** action inserts their overwritten text as a new block right after
      the original. This text can be kept only while that tab remembers it.
    - An overwritten value can be recovered from history only when a
      retained revision records it (principle 3).
32. **Deletion wins.** An edit to a deleted page, block, database, property,
    option, row or view does not apply and is reported as ignored. Edits to
    other values in the same action still apply, except within one editing
    step that only makes sense whole, such as splitting or merging blocks,
    indenting, pasting several blocks, or dropping a card into another
    column: if any part of it is ignored or refused, none of it applies and
    the person's text from it is offered back. An edit to a block inside a
    deleted page is an edit to a deleted block. Because options are their
    own items, renaming a property never restores an option someone deleted,
    and setting a cell to a deleted option is ignored.
33. **Restored items.** When a page, block or row reappears through undo,
    page recovery or a rollback, edits prepared before that do not apply to
    it.
34. **Structure stays valid.** The server checks every move against the
    current tree, not against what the writer saw. A move that would create
    a cycle, exceed the depth limits or put children under a kind that holds
    none is refused, and the writer's screen returns to the server's state.
35. **Whole-notebook rollback** follows the rollback rule of the shared
    Revision model. Edits that arrive after the rollback apply on top of the
    restored notebook by the rules on this page, except that edits prepared
    before the rollback to items it removed or brought back are ignored
    (rules 32 and 33). Open tabs resync and everyone's personal undo history
    is cleared.
36. **Agent writes.**
    - One agent write call applies whole or not at all. If validation, a
      limit, the disk quota or a guard fails, nothing changes.
    - If an item the call targets was deleted in the meantime, the host
      refuses the whole call, and the agent reads again and resends.
    - An agent can give a guard: refuse if the notebook changed since the
      agent read it, or refuse if named pages, blocks or rows changed since
      then. A guard checks the current state, including people's edits that
      are not yet part of a revision.
    - An agent's change without a guard is merged by the same group rules as
      a person's.
37. **Reported results.** For a person's action, the result tells apart what
    was applied, what was ignored (a deleted target, or a block content write
    whose kind the current block cannot turn into by rule 3) and what was refused
    (validation, structure, limit, quota). An agent's call either applies
    whole or is refused with its reason.

## Disconnection and reconnection

N1 does not support offline editing, but people do not lose edits through
ordinary disconnections. These rules match the board's.

38. The notebook shows **Saved** only after the server confirms that it
    recorded the edit durably. Edits shown as saved stay even if the host
    restarts right after.
39. A person's own unconfirmed edits stay on screen and are sent again on
    reconnection. A resent edit never applies twice.
40. On reconnection the browser receives other people's changes made in the
    meantime and merges its own unconfirmed edits by the
    [concurrency rules](#concurrency-rules). If the server can no longer
    accept them, editing stops and the notebook explains why; the person can
    copy their unconfirmed text out of the notice. Closing or reloading a tab
    with unconfirmed edits warns first.

## Agents

41. **Create.** `create_flat {type: "notebook"}` creates a notebook with one
    empty top-level page. `update_notebook` also creates a notebook on a slug
    that does not exist yet.
42. **Read.** `get_notebook` reads the notebook as structured data.
    - Without a page, it returns the page tree (ids, titles, parents,
      positions) and an identifier of the current state.
    - With a page, it returns that page's blocks with their ids, kinds,
      fields and children, and the schema and views of its inline databases.
      `format: "markdown"` returns the page as Markdown instead, with block
      ids kept in a form `markdown_to_blocks` and the agent can refer to.
    - With a database, it returns rows, optionally through one of its views
      or an ad-hoc sort and filter of the same kinds as views, in pages of
      results.
    - Every page of one read comes from the same state, even when people
      edit meanwhile. The host keeps that state for a limited time; when it
      has expired, the read says so and the agent reads again.
43. **Write.** `update_notebook` sends a list of operations in one call, with
    the atomicity and guards of rule 36: create, update, move and delete
    pages, blocks, databases, properties, options, rows and views, and
    upload image assets. Operations may refer to items created earlier in
    the same call by a temporary name, and the result returns the new ids.
    Items the call does not name stay as they are.
44. **Markdown to blocks.** An `update_notebook` operation
    `markdown_to_blocks` converts Markdown into blocks inserted at a given
    place.
    - CommonMark with GitHub tables, task lists and strikethrough.
    - `#`, `##`, `###` become headings; deeper headings become heading 3.
    - Lists, task lists, block quotes, fenced code with its language, and
      thematic breaks become their block kinds, with nesting kept up to the
      nesting limit and flattened below it.
    - A GitHub alert (`> [!NOTE]`, `[!TIP]`, `[!IMPORTANT]`, `[!WARNING]`,
      `[!CAUTION]`) becomes a callout with the matching tone.
    - A table becomes an inline database: the first column becomes the title
      property, the other columns text properties, and one table view.
    - An image becomes an image block only when its path names an asset of
      the notebook or one uploaded in the same call; otherwise the call is
      refused with that path.
    - Raw HTML, footnotes and other constructs without a block kind are kept
      as plain paragraph text, and the result lists a warning for each.
    - The same input always produces the same blocks.
45. **Revisions.** Agents use the existing revision tools (`list_revisions`,
    `get_revision`, `rollback_revision`, `pin_revision`) without approval.
    `get_revision` reads one page of an earlier revision as `get_notebook`
    does.
46. **No versions.** Draft, version, preview, deployment and version
    rollback tools refuse a notebook and point to the notebook and revision
    tools. Version-model tools cannot write notebook content.
47. **Guidance.** The guide gets `topic.notebook` when the feature ships: when
    to choose a notebook over `docs`, a board or a design, the page, block
    and database model, the concurrency rules and guards, and
    `markdown_to_blocks`. The guide index routes to it.

## Revisions

A notebook follows the Revision model of [content-models.md](content-models.md)
unchanged.

48. **Revision unit.** One agent write call is one revision. A person's edits
    are grouped per person and close after 30 seconds idle or 5 minutes from
    the group's start. One rollback is one revision. Before an agent write or
    a rollback applies, open groups close as one revision crediting everyone
    who edited.
49. **Whole notebook.** A revision holds the whole notebook: every page,
    block, database, row, view and asset. Unchanged parts share storage.
50. **Quotas and pins** follow the existing rules: defaults of 100 revisions
    and 20 pins, changeable per flat, 0 means unlimited.
51. **Rollback.** Rolling back to revision R creates a new revision equal to
    R's whole notebook. It adds only metadata, is never refused for disk
    quota, and may leave the notebook over its quota (rule 58).
52. **History panel.** People with write access open History from the
    notebook to list revisions (kind, author, time, message and which pages
    changed), open an earlier revision read-only, pin and unpin, and roll
    back the whole notebook.
53. **Page recovery.** With an earlier revision open, a person picks a page
    and restores it.
    - If the page does not exist now, it comes back with its original ids,
      its subpages and their blocks and databases, under its original parent
      if that still exists and at the top level otherwise. Page links to it
      work again.
    - If the page exists now, the person can restore it as a copy, with new
      ids, next to the current page and titled "<title> (restored)".
    - Any page, block, database, property, option, row or view of the
      restored subtree whose id already exists in the current notebook, for
      example a subpage moved elsewhere since, gets a new id in the restored
      copy, as does everything in a copy.
    - References follow the restored items. A page link or rich-text link
      whose target is part of what is restored points to the restored item,
      under its new id if it got one. A reference to a page outside what is
      restored keeps its original id: it works if that page exists now and
      shows "Missing page" otherwise. Existing pages, and their links, are
      not changed.
    - Recovery is an ordinary edit: it is part of the person's edit group,
      it can be undone, and it does not change other pages.

## Lifecycle and access

54. **Runs at once.** Creating or writing a notebook applies at once without
    approval. A new notebook is Private.
55. **Visibility and deletion need approval.** Making a notebook Public,
    making it Private again, and deleting it each need operator approval, as
    for every flat. A Public approval does not pin a revision, and the
    notebook as it is when the approval applies becomes Public.
56. **Write access.** Only Private routes (Local, Tailscale and NetBird) and
    MCP can write. The notebook trusts only the Flats core's route decision
    for the access level and treats a missing or unknown decision as Public.
    It accepts browser write requests only from the same origin.
57. **Public boundary.** A Public route receives only the current notebook
    content.
    - Viewers navigate pages, open toggles, follow links and switch between
      a database's saved views. None of this is saved.
    - There is no editing, undo, presence or History. The revision list,
      earlier revisions and assets that exist only in earlier revisions are
      not reachable.
    - When the notebook returns to Private, its route stops, or it is renamed
      or deleted, open Public connections close at once.
58. **Disk quota.** A notebook follows the existing
    [quota rules](content-models.md#quotas) unchanged. A person's edit is
    checked before it applies, including the space its revision will need,
    and refused if it does not fit; the optimistic display is undone. Agent
    writes get the same check. While a notebook is over its quota, every edit
    is refused, a deletion included, until the quota is raised or space is
    freed.
59. **Console.** The console lists notebooks with their own label and icon
    and links to the notebook. It shows no versions, previews, database or
    server settings for a notebook.
60. **No outside requests.** The notebook screen requests no outside
    resources and runs no content script or HTML. Links open in a new tab
    with no referrer.

## Success criteria

These are the release criteria for N1. The technical design fixes the
reference data generator and the measurement scripts.

**Reference environment**

- Desktop: an Apple M2-class laptop, the latest stable Chrome and Safari, a
  1440×900 window. Phone: an iPhone of the last three generations, latest
  Safari.
- Network: tailnet RTT of 50 ms or less, bandwidth of 50 Mbps or more.
- Data "reference notebook": 1,000 pages up to 8 levels deep; one page with
  1,000 mixed blocks; one inline database with 5,000 rows, 12 properties of
  every type and 4 views.

| Criterion | Target |
|---|---|
| Open | Warm cache: the reference notebook's first page is interactive in 1.5 seconds or less; cold cache: 3 seconds or less. |
| Typing | On the 1,000-block page, p95 of 16.7 ms or less from a key press to the character on screen. |
| Database | On the 5,000-row database, changing a sort or filter updates the table view in 200 ms or less, and scrolling keeps p95 frame time of 16.7 ms or less. |
| Remote display | p95 of 200 ms or less from the server receiving an edit to it showing on another editor's screen, while 5 people each edit twice per second. |
| Concurrent editing | Under that load, every edit the concurrency rules say must apply stays, with the same result on every screen. |
| Reconnection | After disconnecting and reconnecting, each unconfirmed edit applies exactly once. |
| Recovery | Every page, block, row and asset of a retained revision can be viewed and restored. Rollback succeeds with the disk quota full. |
| Refusal | On a Public route, writes, presence and history access are all refused. An edit over the disk quota is refused before other participants see it. |
| Agent | A fixed reference corpus of Markdown documents, set by the technical design, each at most 300 lines and using only constructs rule 44 maps without loss (headings 1–3, lists nested within the limit, task lists, single-paragraph quotes, code, alerts, dividers, marks, links and one table whose cells hold plain text). For every document, `markdown_to_blocks` followed by `get_notebook` with `format: "markdown"` returns the same document after removing block ids and normalizing whitespace, list markers and emphasis markers. A second corpus of lossy input (deeper headings and nesting, raw HTML, footnotes) produces exactly the normalized output and warnings rule 44 defines. |
| Dependencies | Runs from the single Flats binary, with no outside service, account or separate server. |

## Not in N1

Records as pages, relations, rollups, formulas, gallery and calendar views,
templates, comments, mentions, anonymous guest editing, Notion import,
character-level text merging inside a block, offline editing, synced blocks,
embeds, search across pages, and export of a page or notebook as a file. The
row model leaves room for rows to hold blocks later.

Also outside N1, as for every type: accounts and per-person permissions,
model calls inside the notebook, plugins and arbitrary HTML, replication
across hosts, and Version-model features.

## Current state

The `notebook` type, its browser app, the tools `get_notebook` and
`update_notebook`, the HTTP routes, the `flats notebook` CLI commands,
revisions, the read-only Public route and `topic.notebook` are implemented but
not released. Desktop Chrome was driven against a disposable host over
loopback for editing, undo and redo, table and kanban views, Markdown and
image paste, history, Korean IME composition, reconnection after a host
restart, and the read-only Public route and its withdrawal.

Not done yet:

- No success criterion is measured. The reference data generator and the
  scripts of the technical design's section 22.5 do not exist yet.
- Safari, the iPhone, touch, VoiceOver and Korean IME on Safari are
  unchecked, and nothing has run over a real tailnet.
- The lifecycle authority census (`scripts/lifecycle-gate.sh`) has not run
  with notebook cases: its pinned legacy adapter no longer builds
  (`rt.Close` is undefined), on main as well.
- No fault-injection test checks that a content read still answers when the
  catch-up revision import before it fails.
- The limits of [Open question 1](#open-questions) are the planned values.

## Open questions

1. **Limit numbers.** The limits in rule 6 are planned values, fixed after
   measurement with the reference notebook.
