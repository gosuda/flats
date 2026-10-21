# Anonymous public editing for boards

Status: partly implemented, on an unreleased branch; see
[Current state](#current-state). This page changes the
[board spec](board.md) and parts of [content-models.md](content-models.md);
every changed rule is listed in
[Changes to existing specs](#changes-to-existing-specs). Only the board type
is in scope.

This page sets what to build, why, and the rules it must keep. The technical
design is in [board-public-edit-TECH.md](board-public-edit-TECH.md).

## Summary

Today a Public board is a read-only live view, and only Private routes
(Local, Tailscale, NetBird) edit. This adds a second kind of Public board:
**Public, anyone can edit**. Anyone on the internet who opens the link needs
no account: they type a name and edit the board together with everyone else.

A Public board has one of two **public access levels**:

| Level | Who can do what | Name |
|---|---|---|
| **View only** (today's Public) | Anyone with the link watches the board live. Only Private routes edit. | Never asked. Each visitor is called **Viewer 1, 2, 3, …** automatically. |
| **Anyone can edit** (new) | Anyone on the internet who opens the link watches and edits, and sees other editors' cursors. | The visitor types a name before taking part. |

The operator chooses the level in the board's **Share** dialog and in the
flat's Sharing settings. Widening a board to "anyone can edit" is
always an explicit, confirmed step, and an agent can only ask for it through
an approval.

## Why

- A board is most useful when a group works on it at once. Today everyone who
  edits must be on a Private network, so working with a customer, a class or a
  community needs a screen-share or an export.
- Flats says plainly that Public is not access control. "Anyone can edit"
  keeps that honesty: it is a deliberate, confirmed, visible state, never a
  side effect.
- The risks are known: vandalism, flooding, filling the disk or revision
  quota, identity leaks and hostile text that an agent later reads. The rules
  below put limits and a kill switch around them so the operator can offer an
  open board without being able to lose it.

## Principles

1. **Opt-in and explicit.** A board never becomes editable by anonymous
   visitors without the operator's confirmation of exactly that. A view-only
   Public approval never grants editing.
2. **Anonymous means unverified.** A guest's name is a label the guest typed.
   It never proves who edited, and a badge drawn by the board, outside the
   name, marks every guest.
3. **Guests get less than members.** A guest changes the board's elements. A
   guest reaches no history, rollback, pin, recovery, import, upload, sent
   selection or anything that exists only in earlier revisions.
4. **The operator can always get the board back.** Whatever guests do, a
   pinned safe point exists, the operator can roll back from a Private route,
   and one action stops guest editing at once.
5. **Open does not mean unbounded.** Guest activity is limited per guest and
   per board so that it cannot use up what Private editors and the operator
   rely on: connections, the revision and pin quotas, the disk quota, the
   element limits and the server's per-board state.
6. **Guest text is untrusted.** It is plain text to people and untrusted input
   to agents.
7. **Flats' other rules apply unchanged.** The Revision model, the concurrency
   rules, the disk quota, approval of visibility changes and deletion, and the
   Private identity rules keep working as written, except where
   [Changes to existing specs](#changes-to-existing-specs) says otherwise.

## Access levels

1. **Two stored values.** Visibility stays `private` or `public`. A Public
   board also has a public access level, `view` or `edit`. A board that is not
   Public has no level. Every board that becomes Public defaults to `view`,
   including every board that is already Public when this ships.
2. **Who changes it, and how.**
   - **Agents** (through `set_visibility` with the new level) always get an
     approval link. Nothing changes before the operator approves.
   - **The operator** in the console changes it directly, since the console
     action is the approval. Widening to "anyone can edit" first shows the
     confirmation text of rule 3.
   - **Stop guest editing** (`edit` to `view`) from the console applies at
     once with no confirmation step. Any other narrowing (to Private) keeps
     the confirmation the console already shows.
   - An approval is bound to the visibility and level the board had when it
     was requested. If either changed since, the approval fails as stale, so
     a pending request can never undo the operator's Stop (rule 5).
3. **The confirmation says what it grants.** For "anyone can edit": anyone on
   the internet who opens the link can add, change and delete everything on
   the board; the operator can restore earlier states from History. If a
   permitted provider lists the flat publicly, the text says so.
4. **Reading the state.** `get_flat`, `list_flats`, the status responses and
   the console report the level as `public_access` (`view` or `edit`, absent when not Public).
5. **Stop guest editing.** The Share dialog and the flat's Sharing settings have a
   **Stop guest editing** action. It sets the level to `view` immediately.
   The level change is durable first, then applied to the running board; the
   console confirms only after both (rule 29).
6. **Safe point, pinned and protected.** When "anyone can edit" takes effect,
   the host closes open edit groups and records the board at that moment as a
   revision labeled as the safe point before guest editing, and pins it. While
   the level is `edit`, the host refuses to unpin the safe point for everyone,
   agents and Private editors included; once the level is `view` or Private it
   is an ordinary pinned revision. If the board is unchanged since the last
   safe point and that revision still exists (for example after Stop and
   re-enable), the host re-pins it instead of adding another; otherwise it
   creates a new one. If the pin quota
   is full, the change is refused and the confirmation and approval say so
   before anything is applied.
6a. **Required quotas.** The level cannot be `edit` while the host disk
   quota is off (0) or the board's revision quota is unlimited (0). While the
   level is `edit`, a per-flat revision quota of 0 is refused, and a host
   setting change that would put an `edit` board in that state (disk quota 0,
   or a default revision quota of 0 for a board without its own value) is
   refused with the names of the affected boards. The confirmation says all this and names
   the setting to change.
7. **Boards only.** Websites, pages, documents and designs get no public edit
   level, and their Share dialog and settings do not show the edit choice.

## Joining the board

8. **Anyone can edit: ask for a name.** A visitor who opens a Public board at
   level `edit` is asked for a name before taking part, and cannot edit
   without one. The name is 1 to 40 characters after trimming, plain text,
   with control and invisible formatting characters removed. The browser
   remembers the name and lets the guest change it. Names are not unique.
9. **View only: never ask.** A visitor who opens a Public board at level
   `view` is not asked for anything. The board names the visitor **Viewer N**,
   where N is the lowest number not held by a connected viewer or by a reconnect token younger than one minute. The
   viewer sees their own name ("You are Viewer 3"). A tab that reconnects
   within a minute keeps its number through a random token held only in that
   tab's session and in server memory; nothing is stored.
10. **Viewers are visible only as presence-less entries.** Private editors and
    guests see viewers in the participant list as "Viewer N" entries (or a
    count when there are more than 20). A viewer's browser sends nothing but its
    connection and its reconnect token, and receives no presence, as today. A viewer has no cursor,
    selection or visible viewport.
11. **The prompt is an overlay on the live board.** The name prompt appears
    over the live view, not instead of it. A viewer who has the board open
    when the level goes to `edit` gets the same prompt and is not made a guest
    until they type a name; until then they can only watch.
12. **Guest presence.** At level `edit`, guests and Private editors see each
    other's cursors, selections and "is editing" marks
    ([board rules 22 to 25](board.md#collaboration)).
13. **The guest badge.** The board draws a **guest** badge next to every guest
    name, outside the name text, in cursors, the participant list, "is
    editing" marks, notices and revision credits. Because the badge is not part
    of the name, no name can imitate the verified mark, an agent or a member.
    Names are always drawn as text only. The creator line members see on a
    guest's element names that guest only while the tab shows it as present.
    Once it leaves, or its presence goes stale (30 to 40 seconds without an
    update), the line reads "Guest" with the badge, as in a tab opened after
    it left.

## The guest projection

14. **A third projection.** Guests are neither Private editors nor Public
    viewers: they write, so they need more than the public projection of
    [TECH 7.3](board-TECH.md#73-public-projection), and they must not receive
    what Private editors receive. A **guest projection** is rebuilt from an
    allow-list, in the same way as the public one, for snapshots, deltas,
    acknowledgements, refusals, undo notices and presence. Exports use the
    public view projection.
15. **Identity in it.** Every author or editor key in the guest projection is
    replaced: who made an element or change is reduced to its kind (member,
    guest or agent), and people on the board now get short labels that last
    while they are connected. A verified Tailscale login, a typed name of a
    Private editor and an agent label never appear in it. Private editors
    appear to guests only as **Member 1, 2, …** with their cursor and
    selection.
16. **Guest keys are issued by the server.** A guest's editor key is a random
    value assigned by the host in a separate namespace. The client cannot
    choose it, so a guest cannot claim another editor's key or undo their
    edits.

## What a guest can and cannot do

16a. **No earlier content.** A guest receives only the board as it is now and
    the changes after joining. A guest never receives deleted content or
    earlier changes, and can bring back only elements it deleted itself.
17. **Can.** Everything in
    [Editing in the browser](board.md#editing-in-the-browser) that changes
    elements: tools, selection, move, resize, rotate, copy and paste (text and
    elements), duplicate, delete, ordering, align, snap, group, lock and
    unlock, text editing, style, quick creation, personal undo and redo of the
    guest's own edits, find, PNG export and the data file of the current board
    (the public projection).
18. **Cannot.** History (list, open, pin, roll back, element recovery),
    import, **Send to agent**, and the details of Private editors.
19. **No new images from guests.** In this version a guest cannot upload or
    paste an image, and cannot name an asset. A guest can create an image
    element only by duplicating or pasting back an image element that is live
    now in the same board (copy, duplicate). Undoing a guest's own delete of an image element is
    refused when no live element references its asset. Any other asset reference from a guest is refused. Such elements count
    toward the reserved asset room (rule 24). An asset that exists only in
    earlier revisions never becomes reachable through a guest.
20. **The host decides, per request.** Permission comes from the route
    decision and the stored level, checked for every request and at the
    commit of every edit, never from what the client claims. A `view`
    connection cannot edit by sending an edit; a guest cannot use a
    Private-only operation by sending it. Writes keep the same-origin
    rules of [TECH 7.1](board-TECH.md#71-common-rules).
21. **A lock is not a permission.** A guest can unlock a locked element
    ([board rule 3](board.md#content)).

## Limits against abuse

Admission on a Public route cannot rely on a trustworthy client address, so
limits are per connection and per board. A determined attacker can use up the
guest slots; the remedy is **Stop guest editing**, and the confirmation text
says so.

22. **Per-guest limits.** Each guest connection has a rate limit for
    operations and a limit on the size of one operation, below those of a
    Private editor. Over a limit the host refuses the operation with a reason
    and keeps the connection.
23. **Per-board connection limits.** A board has a maximum number of
    simultaneous guest connections and of new guest connections per minute,
    and a separate, larger limit for viewers. A visitor who cannot get in is
    told the board is full. Guests and viewers never use the connection budget
    that Private editors and the operator rely on.
24. **Reserved room.** Private editors keep room that guests cannot use:
    - **Disk.** A guest edit that would leave less than a reserved share of
      the disk quota free is refused with a reason. Guests also have an
      absolute byte budget per board, so the share of a large quota does not
      decide how much a guest flood can store. It is accounting, not an
      ownership claim: guests are charged the growth of the board's measured
      disk use caused by their committed edits, and the charge falls when the
      board's disk use falls, for example when retention frees space.
    - **Elements and assets.** A guest edit that would take the board above a
      reserved share of the element limit is refused with a reason.
    - **Server state.** Guests have their own caps for the per-board state
      that is shared today: duplicate-prevention receipts, personal undo
      history, deleted-element rows, session records, created-id records and
      the permanent group-id records, so a guest flood cannot evict a Private
      editor's undo history or receipts, or fill those tables. The shared
      change log is not reserved: heavy guest traffic can make a member who
      reconnects after a gap download a fresh copy of the board instead of the
      missed changes, which loses nothing.
25. **One revision group for guests, on fixed windows.** All guest edits on a
    board share one edit group. Unlike a Private editor's group it has **no
    idle close**: it closes only at a fixed 5-minute window or at a forced
    boundary (an agent write, a rollback, a replacing import). However the
    guests edit, they create at most 12 revisions an hour plus forced
    boundaries, so they cannot rotate the revision quota by pausing 31
    seconds. A **guest-only revision** is one whose credited editors are all
    guests. Guest-only revisions may hold at most half of the revision quota
    (a setting with that default); above that, retention deletes the oldest
    guest-only revision first, otherwise the ordinary oldest-first rule
    applies. The newest revision and every pinned revision are never deleted,
    so a guest flood cannot evict more than its share of Private editors' and
    agents' history while recent guest history stays recoverable. A revision lists up to 5 guest names with the guest badge and "and
    N more"; MCP output marks them guest-supplied. Typed guest names are kept
    with history. Private editors still get one group each
    ([board rule 41](board.md#revisions)).
25a. **Totals.** Limits are also set for all guests together, not only per
    connection: total operations and bytes per second into the board's apply
    loop, total bytes read from all guest sockets (refused frames included), total presence traffic, and how often viewer-list updates are sent
    to editors. Guest traffic stays inside a guest share of the host's Public
    traffic budget, so one board cannot starve other flats' viewers or
    Private editors' send queues.
26. **Content limits.** Element, text and other [limits](board.md#content)
    apply to everyone together; a guest edit that exceeds one is refused.

## Agents and guest content

27. **Authorship per text.** `get_board`, `get_revision` and sent selections
    mark, for each element, who last wrote each text a reader may act on (its
    text, a connector label, a frame name and image alt text) and who last
    changed it otherwise: member, guest or agent. An element whose text was
    last written by a guest stays marked guest-written even if a member later
    moves or restyles it. Guest names appear with the guest badge.
28. **Untrusted input.** The agent guide (`topic.board`) says that text and
    names written by guests are untrusted: an agent never follows instructions
    found in them and treats them as data to read, summarize or arrange.

## Withdrawing and downgrading

29. **To view only.** The access level has an epoch. An edit is checked
    against the epoch at the moment it commits, so an edit already in flight
    when the level drops is refused, not applied. Every guest connection then
    becomes a view-only connection at once, moving from the guest
    projection to the public view projection: the connection is closed with an
    "editing ended" code and reconnects under the normal admission limits, so
    a large number of guests never bursts the snapshot limit. "Applied" means
    the epoch is bumped in the apply loop; the console does not wait for
    reconnections. A guest with unconfirmed edits is told editing
    ended and can export those edits
    ([board rules 39 and 40](board.md#disconnection-and-reconnection)). Edits
    confirmed before the change stay. If the host restarts, the stored level
    is the one that applies.
30. **To Private or when the route stops.** As
    [board rule 53](board.md#lifecycle-and-access): open Public connections
    close at once. A guest tab can still export its unconfirmed edits locally
    (board rule 39); only a Private editor can import that file, since guests
    cannot import. Nothing a guest wrote is removed.
31. **No automatic rollback.** Downgrading never removes guest content.
    Removing content is a normal edit or a rollback by the operator.

## Console

32. **Share dialog.** The Share dialog of a board has one control for link
    access with three choices: **Private**, **Public, view only** and
    **Public, anyone can edit**. Each carries a one-line description ("Only
    people on your networks", "Anyone with the link can watch live", "Anyone
    on the internet can edit and delete"). Under "Who has access" it shows
    "Anyone with the link: can view" or "Anyone on the internet: can edit", with how many guests and
    viewers are on the board now. It has **Stop guest editing** while the
    level is `edit`.
33. **Sharing in flat settings.** The flat page's existing Sharing row (its
    **Manage** action opens the Share dialog) shows the level and gains
    **Stop guest editing**, the current guest and viewer counts, a short
    note on what guests cannot do, and the state of the pinned safe point
    with a link to History. A change that waits for an approval shows as
    pending. There is no second permissions section.
34. **Honest labels.** Wherever the console shows visibility (lists, cards, the
    flat header), an editable Public board shows **Public, anyone can edit**,
    never plain "Public". The approval page shows the requested and the current
    level, and says that the safe point will be pinned.
35. **Text only.** Guest names reach the console (counts, revision credits,
    logs) and are drawn as text only, with the control-character rule of
    rule 8.

## Success criteria

Reference: a board with 20 guests editing continuously for an hour.

| Criterion | Target |
|---|---|
| Approval | A board cannot reach level `edit` without a confirmation or approval that states it. A pending view-only approval cannot apply as `edit`. A pending `edit` approval fails as stale after **Stop guest editing**. |
| Name rules | A guest edit is accepted only after a valid name. A view-only visitor is never asked and is named Viewer N, and sees that name. |
| Boundary | On a Public route, history, recovery, import, upload, Send to agent and assets that exist only in earlier revisions are refused at both levels. A forged edit at `view` and a forged Private-only operation from a guest change nothing. |
| Identity | A scan of every frame sent to a guest, including snapshots, deltas, acknowledgements, presence and exported data, finds no Tailscale login, no Private typed name and no agent label (exports are the public view projection). |
| Limits | While guests send operations at the per-guest limit on every allowed connection, a Private editor can still connect, edit, roll back and save, and remote display keeps the board's p95 of 200 ms. At the reserved disk and element room and at the guest byte budget, guests are refused and Private edits succeed. |
| Revisions | However 20 guests edit for an hour (continuously or in pauses of any length), they create at most 12 revisions plus forced boundaries. The pinned safe point is retained and cannot be unpinned while the level is `edit`. Guest-only revisions hold at most half of the revision quota (the newest revision is always kept, so with a quota of 1 it may be guest-only), so member and agent revisions keep at least the other half. Re-enabling editing on an unchanged board adds no revision. |
| Kill switch | After **Stop guest editing**, no edit from a guest connection commits. The console confirms only after the stored level is saved and the epoch is bumped. If the board instance is not running, the stored level alone is changed and applies on the next start. |
| Recovery | After a hostile session that deletes every element, History still lists the safe point and a revision from just before the session, and the operator restores either. |
| Agent safety | Elements whose text a guest wrote are marked as guest-written in `get_board`, including after a member moved them. |
| Dependencies | The feature adds no outside service or account. |

## Changes to existing specs

Board spec ([board.md](board.md)):

- Users table: the "outside viewer" row gains the guest.
- Rule 3 note and "Editing in the browser" heading: editing is on Private
  routes **and, at level `edit`, for guests**, with rules 17 and 18.
- Rules 22 to 25 (collaboration): guests take part at level `edit`; viewers
  appear as entries only.
- Rule 23 (identity): guests are a third kind; their names are unverified,
  server-keyed and badged. Private editors are shown to guests only as
  Member N.
- Rule 41 (revision unit): guest edits share one group.
- Rule 51 (write access): Private routes and MCP, **and Public routes at level
  `edit` for the element operations of rule 17 only**.
- Rule 52 (Public boundary): holds for everything except rule 17's operations
  and the presence of rules 10 and 12. The Public **view** projection is
  unchanged; a guest uses the guest projection of rule 14.
- Rule 53 (withdrawal): adds the downgrade of rule 29.
- Disk quota section ("follows the quota rules unchanged"): guests also respect
  the reserved room of rule 24.
- Principle 6 ("does not redefine Private and Public access"): the public
  access level is a new stored value of Public.
- "Out of scope": "Accounts, per-board permissions and guest links" narrows.
  Accounts, per-person permissions and expiring or per-person links stay out;
  this adds one board-wide anonymous edit level.
- Success criterion "Refusal": reads "On Public routes at level `view`".
- Agent topic `topic.board`: the untrusted-input rule 28.

Technical design ([board-TECH.md](board-TECH.md), updated when this ships):
sections 3.1 and 5.2 (a per-text authorship field in board.db and in
revision snapshots; a member who edits part of a guest's text replaces the
whole text, so the element becomes member-written, which `topic.board`
states), section 5.3 (guest-only revision retention), section 6.1 (identity and "Public connections neither send nor receive
presence"), the route table and section 7.3 (a guest projection and a guest
write path), and the receipt, delta-log, undo and session limits of
sections 4.8, 4.10 and 12 (guest caps).

Other docs:

- [content-models.md](content-models.md): the line "A Public route is read-only"
  gets the exception for boards at level `edit`; the Revision model's "grouped
  per editor" gets the shared guest group; the summary table's "needs operator
  approval" row notes that the console operator's own change is the approval.
- MCP: `set_visibility` and `get_flat` gain the level. The server
  instructions stay within their size cap; the explanation that Public may be
  editable goes to `topic.board`. The generated references and
  `docs/agent/` topics change with it.
- Retention: board rule 43 and content-models Revision rule 6 and Quotas rule
  4 ("oldest first"): guest-only revisions are limited to a share and retired
  first above it; the revision just before an agent write is kept as before.
- Revision model: board rules 44 (pins), 46 and 48 (unpin by Private editors
  and agents are refused for the active safe point) and 50 (a Public approval
  does not pin a revision; at level `edit` the host pins the safe point), and
  content-models Revision rules on pins and rollback by agents.
- Console: `setVisibility` keeps its existing confirmation except for Stop
  guest editing; the approval page shows the level.

## Current state

Implemented end to end: the stored level and its approvals, the safe point,
guests and viewers on the board, the guest projection and limits, revision
grouping and retention, authorship, the board app and the console. Not done
or not measured:

- Remote display under load is measured as delivery to the other
  connections (50 guests at their limit: p95 23 ms), not as the time until
  the browser has drawn the change.
- No guest edit can reach the asset room refusal today, because guests can
  only reuse assets live elements already use. The check stays as a second
  line of defense, for example if guests may add images later (open
  question 4), and a unit test calls it directly.

## Open questions

1. **Just look.** The owner asked that visitors to an editable board be asked
   for a name. Whether an editable board also offers a way to watch without a
   name is undecided; as written, there is none.
2. **Show typed names of Private editors to guests?** Rule 15 shows only
   "Member N" so that nothing identifying leaves the tailnet. A member could
   instead choose to show a display name.
3. **Stronger guest identity.** Names are not unique, and there is no way to
   remove or block a single guest, because an anonymous visitor can open a new
   session. Whether to add a passphrase on the link, or per-guest removal, is
   decided after real use. This page adds no accounts.
4. **Guest images.** Whether guests may add images later, behind an operator
   switch, is decided after the disk and abuse limits have been measured.
5. **Numbers.** The rate limits, the guest and viewer limits, the reserved
   room shares and the guest state caps are fixed by measurement in the
   technical design.
6. **Other types.** Whether documents and designs get the same level later.
