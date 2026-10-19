# Board content type

Status: partly implemented. The board type runs end to end, but not every
success criterion is met or measured yet; see
[Current state](#current-state). The technical design is in
[board-TECH.md](board-TECH.md).

This page sets what to build, why, and the rules a board must keep. The
technical design decides the storage format, where computation runs and the
protocol.

## Summary

A **board** (`type: "board"`) is an infinite-canvas whiteboard that people
and agents use together. As in Miro, FigJam, Excalidraw or Apple Freeform,
people place sticky notes, shapes, connectors (arrows), freehand drawings,
text, images and frames anywhere on the canvas. Several people edit at the
same time and see each other's cursors. An agent reads the board through MCP
as structured data and adds or changes elements, so it builds sorted
brainstorms, flowcharts and retrospective boards on the same canvas as the
people.

A board follows the **Revision model** of [content-models.md](content-models.md).
It has no Draft, versions, deployment previews or deployment. Edits apply at
once without approval, and history is kept as revisions.

| Type | Written by | What people change in the browser | Changes reach the URL |
|---|---|---|---|
| `docs` | people and agents | Markdown text | at once |
| `design` | people and agents | artboard layout, text written directly in the source, tweak values and notes. People cannot change an artboard's structure (its HTML). | at once |
| **`board`** | **people and agents** | **every element: create, move, connect, delete, style and text** | **at once** |
| `page` | an agent | nothing | after an approved version and deployment |
| `flat` | an agent | nothing | after an approved version and deployment |

## Why

- **No picture lets people fix its structure directly.** A `design` can hold
  notes and diagram artboards, but the boxes and arrows inside an artboard are
  HTML, so a person cannot add a box or reconnect an arrow. Work where several
  people keep changing **the elements and how they connect**, such as
  brainstorms, flowcharts and retrospectives, needs native elements.
- **How to choose.** Pixel-tuned screens, prototypes and posters belong in a
  `design`. A workspace for spreading out, grouping and connecting ideas is a
  `board`. Prose belongs in `docs`.
- **An agent's answer in pictures is hard for people to take over.** Today the
  only options are Mermaid (inside a document, changed only through its
  source) and HTML artboards. People must be able to move and fix a diagram an
  agent made by hand.
- **Self-hosted whiteboards take work to run.** Miro and FigJam need accounts
  and outside servers. Excalidraw can be self-hosted, but the collaboration
  server, storage, history and agent integration must be set up separately.
  Flats already has a single binary, Private networking, the Revision model
  and MCP.

## Users and scenarios

| User | Scenario |
|---|---|
| An operator sorting out their own thinking | Asks an agent to "put this PR discussion on a board", then moves and groups the resulting stickies by hand. |
| A small team (tailnet members) | Edits a retrospective, brainstorm or sprint plan on one board at the same time, and sees each other's cursors and selections. |
| A planner or designer | Draws user flows and system diagrams with shapes and connectors, and asks an agent which paths are missing. |
| An agent | Reads the stickies people added, groups them by topic, adds summary stickies and builds flowcharts. |
| An outside viewer | Views, read-only and live, a board the operator approved as Public. |

A typical flow:

1. An agent creates a board with `create_flat {type: "board"}` and gives the
   user its Private URL.
2. People open it in a browser, add stickies and draw by hand. They see each
   other's cursors.
3. The user selects a few stickies, presses **Send to agent**, and asks the
   agent to "group these by topic and summarize them".
4. The agent reads the sent selection and the board. In one write it creates
   frames, moves the stickies and adds summary stickies. The open canvas shows
   the changes live, and that write becomes one revision.
5. If the user does not like the result, there are two ways back.
   - **Whole-board rollback.** In History, the user rolls back to the
     revision just before the agent's write. Edits other people made after it
     are rolled back too. The rollback is itself a revision, so it can also be
     rolled back.
   - **Element recovery.** The user opens an earlier revision, picks only the
     elements they need and copies them into the current board. Everything
     else stays as it is.

## Principles

1. **People can fix the picture directly.** People can select, move, resize,
   change and delete every element in the browser. Elements an agent made are
   handled exactly like elements a person made.
2. **Agents are full collaborators.** An agent reads and writes the board as
   structured data (elements, text, connections and membership), without
   screenshots or guessed coordinates. An agent's write appears on open
   canvases at once.
3. **Concurrent editing is the default.** Several people and agents edit the
   same board at the same time. An edit that passes permission, validation,
   limit, disk quota and guard checks is never refused because someone else
   changes another element at the same moment. When people change the same
   element at the same time, their edits are merged by the
   [concurrency rules](#concurrency-rules) instead of refused. This differs
   from the design type.
4. **Nothing in retained history disappears.** The state a retained revision
   records (elements, values and assets) can always be viewed and recovered.
   Intermediate states between revisions, and revisions the quota deleted
   automatically, are not guaranteed (see [Revisions](#revisions)).
5. **Fast and light.** Panning and zooming stay smooth with thousands of
   elements, and other people's edits appear inside the tailnet without
   noticeable delay. No outside service or account is needed.
6. **Flats' existing rules apply unchanged.** A board does not redefine the
   Revision model, Private and Public access, the actions that need approval
   (visibility changes and deletion), the disk quota or fixed types.

## Content

1. **Explicit type.** A flat becomes a board only through
   `create_flat {type: "board"}` or a board write to a new slug. The type
   never changes, and saves of another type are refused.
2. **Elements.** A board is a set of elements. Each element has an id unique
   within the board, a kind, a position, a size, a rotation, a style and a
   stacking order. The element kinds in B1:

   | Element | Description |
   |---|---|
   | Sticky note | A colored square note that holds plain text. |
   | Text | A free text box with plain, multi-line text. |
   | Shape | A rectangle, rounded rectangle, circle or diamond, with plain text inside. |
   | Connector | A straight or elbow line with an end shape at each end (none or arrow), a line style and one label. |
   | Pen | A freehand stroke in two widths (pen and highlighter). |
   | Image | An uploaded or pasted image with alt text. |
   | Frame | A named area. It moves its member elements with it, and it is a unit for export and navigation. |

3. **Membership rules (B1).**
   - An element belongs to one frame **explicitly**, or to none. Placing an
     element inside a frame makes it a member, and dragging it out ends the
     membership. An element that a person creates inside a frame (with a
     tool, by pasting, by quick creation or by dropping an image) becomes a
     member of the topmost frame that contains its center. An agent sets
     membership explicitly. Position alone never implies membership later.
   - Frames do not nest. Groups also allow one level only (no group inside a
     group).
   - Deleting a frame keeps its member elements and only ends their
     membership. "Delete the frame and its contents" is a separate, explicit
     action.
   - All members of a group belong to the same frame, or all are outside any
     frame. Moving a group into or out of a frame changes the membership of
     every member together.
   - A locked element is not the target of clicks, area selection, or direct
     moves, changes or deletion. Moving the frame or group it belongs to also
     moves the locked element: a lock blocks only direct manipulation.
   - To unlock a locked element, a person right-clicks it (or long-presses
     it), or picks it from the "Locked elements" list and unlocks it. A lock
     prevents mistakes and is not a permission: anyone can unlock it.
   - Personal undo and layout help never change a locked element. An agent
     write can change a locked element only when it explicitly unlocks it
     first in the same call. Otherwise the host refuses the call.
4. **Connector rules.**
   - Each end of a connector is attached to an element (including the
     attachment point) or placed freely at canvas coordinates.
   - An attached end follows its target element when the target moves,
     resizes or rotates.
   - People can drag an end to attach it to another element or detach it.
   - When the target element is deleted, that end becomes **detached** and
     stays free at its last position. The connector itself is not deleted.
   - When elements are duplicated or copied and pasted, connections inside the
     copied range connect the copies to each other. Connections to elements
     outside the range become free ends.
5. **Text.** All text in B1 is unformatted, multi-line plain text. Stickies
   and shapes shrink the font size to fit overflowing text, but never below a
   minimum size; below it they show an overflow mark.
6. **Assets.** Images are stored as files of the board, and image elements
   reference them. An asset that a retained revision references stays as long
   as that revision exists.
7. **Limits.** Limits apply to the **current board**, and the host refuses an
   edit that exceeds one with the reason. B1 targets: 10,000 elements, 4,000
   characters of text per element, and limits on the number of points in one
   pen stroke and on the number of assets. The final numbers are set by
   measurement before release (see [Open questions](#open-questions)). Each
   limit is judged on its own. If a lowered limit leaves an item already over
   it, the board and rollbacks to its revisions are not refused. Only edits
   that increase how far that item is over the limit are refused. Edits that
   reduce it or leave it unchanged (deletion, cleanup, edits to other items)
   are allowed.

## Viewing

8. **Infinite canvas.** Pan (drag, Space and drag, trackpad, two fingers),
   zoom (10% to 400%), fit everything and fit the selection. A dot-grid
   background can be turned on and off.
9. **Find.** People search the board's text (stickies, shapes, text, labels,
   frame names and image alt text) and jump to the results. They can also
   jump through the frame list.
10. **Live.** An open canvas shows every change without a reload, whoever made
    it.
11. **Connection state.** The screen always shows whether the board is
    saving, saved or disconnected (see
    [Disconnection and reconnection](#disconnection-and-reconnection)).
12. **Theme.** The canvas follows the light or dark theme, and element colors
    stay readable in both.
13. **Accessibility.**
    - With the keyboard alone, people can move between elements (reading order
      is per frame, top to bottom, then left to right), select, move, edit
      text and delete.
    - A screen reader reads each element's kind, text, connections ("from A to
      B") and frame. For images it reads the alt text.
    - Color is never the only way to convey meaning, for example for selection
      or for marks of other people's edits.

## Editing in the browser

Editing is available on Private routes.

14. **Tools.** Select, hand (pan), sticky, text, shape, connector, pen,
    eraser, image and frame. Every tool has a keyboard shortcut.
15. **Basic actions.** Select (click, area drag, Shift to add), move, resize,
    rotate, copy, paste, duplicate, delete, bring forward and send backward,
    align and distribute, snap (to the grid and to other elements' edges and
    center lines), group and lock.
16. **Quick creation.** A + handle beside a sticky or shape creates a
    connected element directly. Double-clicking an empty spot creates text.
17. **Text editing.** Double-clicking an element edits its text in place.
    - While an input method (IME, for example for Korean) is composing, remote
      changes to that element do not break the composition. They apply after
      the composition ends.
    - An element another person is editing shows "<name> is editing". This mark
      is **not a lock**. If two people commit the same text at the same time,
      the later value stays by the [concurrency rules](#concurrency-rules), and
      the person whose text was overwritten gets a notice. The notice's
      **Keep my text** action creates that person's overwritten text as a new
      text element beside the original element. The later value stays as it
      is. This text can be kept only while that tab remembers it.
18. **Style.** A small toolbar over the selected elements changes color, line
    width, line style, fill, font size and connector end shapes. There is no
    heavy properties panel.
19. **Paste.**
    - Pasting while editing text puts the text into that element.
    - Pasting multi-line text on the canvas creates one sticky per line. With
      many lines, the board asks whether to put it into one text element
      instead.
    - Pasting an image creates an image element.
    - Pasting elements copied from the same board or another board creates
      them with new ids.
20. **Touch and pen.** On a tablet, fingers pan and zoom and a stylus draws by
    default.
21. **Personal undo and redo.**
    - Undo reverses **only the person's own edits**. It never reverses edits
      by other people or agents.
    - The unit is one drag, one text edit (from the start of editing to the
      commit), or one action such as a paste, an alignment or a deletion.
    - If someone else later changed a property the person had changed, undo
      leaves that property as it is and says "Changes other people made later
      were not undone." It still reverses the other properties.
    - If someone else later changed an element the person created, undo does
      not undo the creation (does not delete the element) and shows the same
      notice.
    - Undoing the person's own deletion restores the element with its original
      id.
    - Undo never changes a locked element and shows the same notice. As an
      exception, a person can undo their own lock or unlock action, unless
      someone else changed the lock state afterwards; then undo skips it.
    - Each browser tab has its own history. A reload, a whole-board rollback or a whole-board replacement (import with replace)
      clears it.

## Collaboration

22. **Cursors and selections.** Private editors see each other's cursors,
    names and selected elements live.
23. **Identity.** On Tailscale routes, the board shows the trusted Tailscale
    identity as **verified**. On Local and NetBird routes, it uses the name
    entered on first opening and shows it as **unverified**. A value the
    client sends can never earn the verified mark.
24. **Participant list.** A list of the people on the board now. Selecting a
    person moves the view to where that person is looking.
25. **Agent activity.** An agent's write appears as a short notice, such as
    "The agent added N elements", and a brief highlight of the changed
    elements.
26. **Send to agent.** A person can explicitly **send** selected elements **to
    an agent**.
    - Each send creates a short share number (for example `#7`), and the
      screen says something like "Tell the agent `#7`." The person refers to
      that number when asking the agent.
    - A share is fixed to the list of element ids at the moment it is sent,
      and records the sender, whether that sender is verified, and the time.
    - The agent reads a share by its number, or lists recent shares. Elements
      deleted in the meantime, or removed by a rollback, are shown as missing.
    - A share disappears after 24 hours or when the board restarts. It is not
      board content, so no revision records it.
    - Cursors and live selections are never exposed to the agent.

## Concurrency rules

The design type refuses a person's edit made against an older state, to
protect its HTML source. A board's elements are small, independent pieces of
data, so a person's edit is merged by the rules below instead of refused.

27. **Property groups.** Values that only make sense when they change together
    form one group. The values in a group change together and are overwritten
    together.

    | Group | Contains |
    |---|---|
    | Shape | position, size and rotation (for a pen, also its list of points) |
    | Endpoint (each) | the target element and attachment point of one connector end, or its free coordinates |
    | Text | the element's whole text |
    | Style property (each) | color, fill, line width, line style, font size, end shape and so on, one each |
    | Stacking order, membership, group, lock, image alt text | one each |

28. **Different elements and different groups all apply.** If one person
    changes a position while another changes a color at the same time, both
    stay.
29. **In the same group, the last write stays.** When the same group of the
    same element changes at the same time, the value the server received
    later stays. An overwritten value can be recovered from history only when
    a retained revision records it (principle 4).
30. **Deletion wins.** Another person's edit to a deleted element does not
    apply and is ignored. Edits to other elements in the same action still
    apply.
31. **Restored elements.** When an element reappears through undoing a
    deletion, element recovery or a rollback, older edits prepared before
    that do not apply to it.
32. **Whole-board rollback.** It follows the rollback rule of the shared
    Revision model. Edits that arrived before the rollback stay in the
    revision closed just before it, and the result is exactly R's board.
    Edits that arrive after the rollback apply on top of R's board in arrival
    order, by the rules on this page, except that:
    - an edit to an element the rollback removed is ignored (rule 30);
    - an edit prepared before the rollback, to an element the rollback brought
      back, is ignored (rule 31);
    - an agent call with a guard is refused if its guard does not match
      (rule 33);
    - open canvases resync to the rollback result, and everyone's personal
      undo history is cleared.
33. **Agent writes.**
    - One agent write call **applies whole or not at all**. If validation, a
      limit, the disk quota or a guard fails, nothing changes.
    - If an element the call targets was deleted in the meantime, the host
      refuses the whole call, and the agent reads again and resends.
    - An agent can give a guard: refuse if the whole board changed since the
      agent read it, or refuse if named elements changed since then. A guard
      checks the **current state**, including people's edits that are not yet
      part of a revision.
    - An agent's change without a guard is merged by the same group rules as
      a person's.
34. **Reported results.** For a person's action, the result tells apart what
    was applied, what was ignored (deleted targets) and what was refused
    (validation, limit, quota). An agent's call either applies whole or is
    refused with its reason (rule 33); nothing in it is ignored.
    - Grouping is one operation on all its elements: if one of them changed
      frame or joined another group in the meantime, the whole grouping is
      ignored and reported, and the person groups again.

## Disconnection and reconnection

B1 does not support offline-first editing, but people do not lose edits
through ordinary disconnections.

35. The board shows **Saved** only after the server confirms that it recorded
    the edit durably. Edits shown as saved stay even if the host restarts
    right after.
36. A person's own edits that the server has not confirmed yet stay on screen
    and are sent again on reconnection.
37. A resent edit **never applies twice** (no duplicate stickies).
38. On reconnection, the browser receives other people's changes made in the
    meantime and brings the screen up to date. The person's own unconfirmed
    edits are merged by the [concurrency rules](#concurrency-rules).
39. If, after a disconnection, the server can no longer accept those edits,
    editing stops and the board explains why. The person can then export
    their own unconfirmed edits to a file and import them later.
40. When a person tries to close or reload a tab with unconfirmed edits, the
    browser warns and offers the same export.

## Revisions

A board follows the Revision model of [content-models.md](content-models.md)
unchanged.

41. **Revision unit.**
    - One agent write call is one revision.
    - A person's edits are grouped per person and become a revision after 30
      seconds idle or 5 minutes from the group's start.
    - One rollback is one revision.
    - Before an agent write, a rollback or a whole-board replacing import
      applies, open groups close: the whole board just before it is kept as
      one revision credited to everyone who edited.
42. **Whole board.** A revision holds the whole board (every element and
    asset). Unchanged parts share storage.
43. **Quotas.** The revision quota and pin quota follow the existing rules
    (defaults of 100 and 20, changeable per flat, 0 means unlimited). The
    oldest unpinned revisions above the quota are deleted automatically.
44. **Pins.** A pinned revision is never deleted automatically. When the pin
    quota is full, a new pin is refused. An unpinned revision counts toward
    the revision quota again.
45. **Rollback.** Rolling back to revision R creates a new revision holding
    the same whole board as R. History is never rewritten. A rollback adds
    no new stored content (it reuses R's files), so it is never refused for
    disk quota. The board may then be over its quota (rule 59).
46. **Managed on the canvas.** People with write access use the canvas's
    History panel to list revisions (kind, author, message and number of
    changed elements), open an earlier revision read-only, pin and unpin, and
    roll back.
47. **Element recovery.** With an earlier revision open read-only, a person
    can pick elements and copy them into the current board. The copies get
    new ids, connections inside the copied range are kept, and connections to
    elements outside it become free ends. This copy is an ordinary edit.
48. **Agents.** Agents do the same through the existing revision tools
    (`list_revisions`, `get_revision`, `rollback_revision`, `pin_revision`).
    No approval is needed.

## Lifecycle and access

49. **Runs at once.** Creating or writing a board applies at once without
    approval. A new board is Private.
50. **Visibility and deletion need approval.** Making a board Public, making
    it Private again, and deleting it each need operator approval, as for
    every flat. A Public approval does not pin a specific revision. Content
    changes while it is pending do not make it stale, and the board as it is
    when the approval applies becomes Public. A Public board shows every
    change to everyone at once.
51. **Write access.** Only Private routes (Local, Tailscale and NetBird) and
    MCP can write. The board trusts only the route decision of the Flats
    core for the access level, and treats a missing or unknown decision as
    Public. It accepts browser write requests only from the same origin.
52. **Public boundary.** A Public route receives **only the current board
    content**.
    - Cursors, names, selections, the participant list and sent selections
      are neither sent nor received.
    - The revision list, earlier revisions and assets that exist only in
      earlier revisions are not reachable.
    - There is no editing, undo or Send to agent.
53. **Withdrawing Public access.** When a board returns from Public to
    Private, its route stops, or the board is renamed or deleted, open Public
    connections close at once.
54. **No versions.** Draft, version, deployment preview, deployment and
    version rollback tools refuse a board and point to the revision tools.
    Version-model tools cannot write board content.
55. **Console.** The console lists boards with their own label and icon and
    links to the canvas. It shows no versions, deployment previews, database
    or server settings for a board.

## Disk quota

A board follows the existing [quota rules](content-models.md#quotas)
unchanged.

56. The host checks a person's edit **before** it applies the edit to the
    server's committed state or passes it to other participants. If the edit,
    including the space it will need when it later becomes part of a
    revision, does not fit the per-flat disk quota, the host refuses it. The
    optimistic display the editor saw first is undone when the edit is
    refused.
57. Agent writes and imports, including a whole-board replacement, get the
    same check.
58. The host never deletes history automatically to make room.
59. A revision rollback is never refused for disk quota. It can leave the
    board over its quota. While a board is over its quota, every edit is
    refused, a deletion included, because each edit still adds to the
    board's history (the same rule as for documents). The board accepts
    edits again once the operator raises the quota or space is freed, for
    example by lowering the revision quota or unpinning revisions so that
    old revisions are deleted.

## Agents

60. **Create.** `create_flat {type: "board"}` creates an empty board. The
    board write tool, `update_board`, also creates a board on a slug that
    does not exist yet.
61. **Read.** `get_board` reads the board as structured data: elements (kind,
    text, position, size and style), the targets of both connector ends,
    frame and group membership, locks, and an identifier of the current state.
    - For a large board, the agent narrows the read by frame, area or id, and
      reads it in several pages.
    - One read is a consistent state at one point in time: every page of a
      read comes from the same state, even when people edit meanwhile. The
      host keeps that state for a limited time; when it has expired, the read
      says so and the agent reads again from the start.
62. **Write.** `update_board` sends several element additions, changes and
    deletions in one call, with the atomicity and guards of rule 33. Elements
    the call does not name stay as they are. The agent replaces the whole
    board only when it asks for that explicitly. The same call also adds image
    assets.
63. **Layout help.** So that the agent does not have to compute every
    coordinate, a write call can include "lay out these elements in a grid,
    row or column". The same input gives the same result, and the call returns
    the resulting coordinates. The host checks the layout against the state
    after the call's earlier operations apply. If a layout target is locked at
    that point, the host refuses the call. If the same call unlocked it
    earlier, the layout applies.
64. **Import.** `import_board` imports a Flats board data file as a new board,
    or **adds** it to an existing board (with new ids). Replacing an existing
    board whole is an explicit choice.
65. **Sent selections.** `get_board_share` reads a selection a person sent to
    the agent, by its share number (rule 26).
66. **Guidance.** The guide has `topic.board`: when to choose a board, a
    design (`topic.canvas`), docs or a page; the element model; the
    concurrency rules and guards; layout help; and readability (no overlaps,
    color used for meaning, frames for grouping, readable font sizes). The
    guide index makes clear that `topic.canvas` is the design topic.

## Export, import and safety

67. **Export (B1).** The whole board, one frame or the selected elements as
    PNG, at a chosen scale. The whole board as a Flats board data file, with
    its elements and assets. The data file format has a format version, and
    later Flats releases keep reading earlier format versions.
68. **Import (B1).** Flats board data files (rule 64).
69. **Uploaded images.** The host accepts only PNG, JPEG, GIF and WebP. It
    does not accept SVG uploads. File size follows the host upload limit, and
    pixel count has its own limit. The host refuses images over those limits
    or images it cannot decode.
70. **Import files.** The host checks the total uncompressed size, the number
    of files and that every path is a safe relative path. If a file exceeds a
    limit or breaks a rule, the host refuses it and changes nothing.
71. **No outside requests.** The board screen, export results and imports
    request no outside resources and run no script or HTML. Element text is
    always drawn as text only.
72. **Operation limits.** Pen points, operations per call, layout targets and
    export result size each have a limit.

## Success criteria

These are the release criteria for B1, with fixed measurement conditions.

**Reference environment**

- Desktop: an Apple M2-class laptop, the latest stable Chrome and Safari, a
  1440×900 window.
- Tablet: an iPad (M1 class) with the latest Safari.
- Network: tailnet RTT of 50 ms or less, bandwidth of 50 Mbps or more.
- Data "mixed 5k": 2,980 stickies (40 characters on average), 1,000 shapes
  (15 characters on average), 700 connectors (30% with labels), 250 pen
  strokes (at most 200 points per stroke), 50 images (each at most 1 MP, at
  most 30 MB in total) and 20 frames, exactly 5,000 in total. "Mixed 10k" is
  twice as many in the same proportions (10,000 in total). The technical
  design fixes the reference data generator.
- Measurement: panning and zooming are measured with a fixed 60-second
  interaction script, remote display during a 10-minute load.

| Criterion | Target |
|---|---|
| Pan and zoom | With mixed 5k, p95 frame time of 16.7 ms or less (desktop) and 33 ms or less (tablet). With mixed 10k, 33 ms or less, with no errors or freezes. |
| First screen | Mixed 5k with a warm cache: first interactive screen in 1.5 seconds or less. With a cold cache: 3 seconds or less. |
| Remote display | p95 of 200 ms or less from the server receiving an edit to it showing on another editor's screen, while 5 people each edit twice per second. |
| Concurrent editing | Under that load, every edit that the concurrency rules say must apply stays, with the same result on every screen. |
| Reconnection | After disconnecting and reconnecting, each unconfirmed edit applies exactly once. |
| Recovery | Every element, value and asset of a retained revision can be viewed and recovered. Rollback succeeds even with the disk quota full. |
| Refusal | On Public routes, writes, presence and history access are all refused. An edit over the disk quota is refused before it reaches the committed state or other participants' screens. |
| Agent | A task of sorting 30 stickies: every element kept, zero overlaps, text at least 12 px at 100% zoom, both ends of every connector attached to the intended elements. |
| Dependencies | Runs from the single Flats binary, with no outside service, account or separate server. |

## Phases

| Phase | Scope |
|---|---|
| **B1 (first release)** | The elements in [Content](#content) (plain text, straight and elbow connectors, pen without pressure); everything in [Viewing](#viewing) through [Disk quota](#disk-quota) (find, accessibility, personal undo, reconnection, the Public boundary, the disk quota, and the History list, viewing, rollback, pins and element recovery); agent reads, writes, guards, grid, row and column layout, and sent selections; PNG and data file export; data file import. |
| B2 | Curved connectors, pen pressure, a minimap, highlighting elements changed between revisions, automatic tree layout, converting a Mermaid flowchart into editable elements, SVG export, per-frame print and PDF, a frame presentation mode, following another person, comments and mentions, voting, a timer, and templates. |
| B3 | Text formatting, character-level concurrent text editing, tables, a mind-map tool, Excalidraw import and export, embedding other flats (document and design artboard cards), and offline editing. |

## Out of scope for B1

- Accounts, per-board permissions and guest links. In Flats, the network
  decides access.
- Video or voice calls, and chat.
- Model calls inside the board (an AI button). Agents work only through MCP.
- Plugins, a widget runtime and arbitrary HTML elements. Content that needs
  HTML belongs in the design type.
- Replication across several hosts, and offline-first sync.
- Version-model features.

## Current state

The `board` type, its canvas app, the board tools (`get_board`,
`update_board`, `get_board_share`, `import_board`), the HTTP routes and CLI
commands, revisions, the disk quota and `topic.board` are implemented but not
released. Measured in desktop Chrome on an Apple laptop one generation older
than the reference, over loopback or an emulated 50 ms network:

- Pan and zoom: mixed 10k meets 33 ms. Mixed 5k runs at 60 frames per second
  with at most one missed frame per minute, but its frame-interval p95 is
  about 18.4 ms against 16.7 ms, which looks like timestamp jitter. Whether
  that reading is accepted is open.
- First screen (mixed 5k), remote display (an upper bound from send to
  display), concurrent editing, reconnection, recovery, refusal and the agent
  task meet their targets.

Not done yet:

- The canvas has not run on Safari or an iPad: touch and pen, Korean IME
  composition during remote changes, VoiceOver, and the tablet and Safari
  measurements are unchecked. Remote display over a real tailnet is not
  measured.
- The limits of [Open question 2](#open-questions) are the planned values.

## Open questions

1. **Stronger recovery guarantee.** B1 guarantees recovery only up to
   retained revisions (principle 4). Whether to also guarantee overwritten
   values, by placing a revision boundary each time an edit overwrites
   another person's value or deletes an element, is decided after real use,
   weighed against how fast it uses up the revision quota.
2. **Limit numbers.** The limits on elements, pen points, assets, image
   pixels and operations per call are set before release, after measurement
   with the reference data.
3. **Exposing presence to agents.** B1 exposes only sent selections. Whether
   to open cursors or live selections to agents is decided from real-use
   needs.
