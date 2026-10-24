# Anonymous public editing for documents

Status: partly implemented, on an unreleased branch; see
[Current state](#current-state). This page changes the document parts of
[content-models.md](content-models.md), the
[docs app internals](../internal/docs-app.md) and
[board-public-edit.md](board-public-edit.md) rules 7, 8, 11 and 15; every changed rule is
listed in [Changes to existing specs](#changes-to-existing-specs). The
document type (`docs`) is in scope, plus the board changes to rules 8, 11
and 15 that keep the two types the same.

This page sets what to build, why, and the rules it must keep. The technical
design is in [docs-public-edit-TECH.md](docs-public-edit-TECH.md). It follows
[board-public-edit.md](board-public-edit.md) (the board feature) rule for rule
wherever a document allows it, so the two types share one model, one console
and one vocabulary. Where a rule here only restates a board rule for
documents, it names that rule as "board public rule N".

## Summary

Today a Public document is a read-only live rendering, and only Private
routes (Local, Tailscale, NetBird) edit. This adds the board's second kind of
Public to documents: **Public, anyone can edit**. Anyone on the internet who
opens the link needs no account: they type a name and edit the document
together with everyone else.

A Public document has one of the same two **public access levels** as a
board:

| Level | Who can do what | Name |
|---|---|---|
| **View only** (today's Public) | Anyone with the link reads the document live. Only Private routes edit. | Never asked. Each visitor is called **Viewer 1, 2, 3, …** automatically. |
| **Anyone can edit** (new) | Anyone on the internet who opens the link reads and edits every document of the flat, and sees other editors' cursors. | The visitor types a name before editing. |

The operator chooses the level in the same **Share** dialog and Sharing
settings as for a board. Widening a document to "anyone can edit" is always
an explicit, confirmed step, and an agent can only ask for it through an
approval.

## Why

- A document is the other content people most often write together: meeting
  notes, a shared agenda, a sign-up list, a draft a customer comments on.
  Today everyone who edits must be on a Private network.
- The board feature already settled the model, the limits and the console
  for an open, editable flat. Giving documents the same model keeps one way
  to share in Flats instead of two that differ in small ways.
- The risks are the board's, with one more: guest text is Markdown that other
  people's browsers render and that agents read. The rules below keep
  rendering and agent reading safe.

## Principles

Board public principles 1 to 7 apply to documents unchanged, with "board"
read as "document flat" and "elements" as "the text of its documents":

1. **Opt-in and explicit.**
2. **Anonymous means unverified.**
3. **Guests get less than members.** A guest changes the current text. A
   guest reaches no history, rollback, pin, earlier text or anything else
   that exists only in earlier revisions.
4. **The operator can always get the document back.**
5. **Open does not mean unbounded.**
6. **Guest text is untrusted.** It is plain Markdown source to people, is
   rendered under the same restrictions as any other text, and is untrusted
   input to agents.
7. **Flats' other rules apply unchanged**, except where
   [Changes to existing specs](#changes-to-existing-specs) says otherwise.

And one for consistency:

8. **One feature, two types.** The stored level, the approval, the safe
   point, the quota rules, Stop guest editing, the console and the guest
   badge behave and read the same for a document as for a board. A
   difference between the two types exists only where the content forces
   it, and this page names each one.

## Access levels

1. **The board's stored values.** Visibility stays `private` or `public`. A
   Public document also has a public access level, `view` or `edit`. A
   document that is not Public has no level. Every document that becomes
   Public defaults to `view`, including every document that is already
   Public when this ships. The level covers every document of the flat: a
   flat with several documents is open to guests as a whole, as its Public
   view is today.
2. **Who changes it, and how.** As board public rule 2: agents through
   `set_visibility` with `access` always get an approval link; the operator in
   the console changes it directly, after the confirmation of rule 3 when
   widening; **Stop guest editing** applies at once without confirmation;
   an approval is bound to the visibility and level the flat had when it was
   requested and fails as stale if either changed since.
3. **The confirmation says what it grants.** For "anyone can edit": anyone
   on the internet who opens the link can add, change and delete all the text
   of every document in this flat; the operator can restore earlier states
   from History. If a permitted provider lists the flat publicly, the text
   says so. The wording is the board's, with "the board" replaced by "the
   documents".
4. **Reading the state.** `get_flat`, `list_flats`, the status responses and
   the console report the level as `public_access` (`view` or `edit`, absent
   when not Public), as for a board.
5. **Stop guest editing.** As board public rule 5.
6. **Safe point, pinned and protected.** As board public rule 6: when "anyone
   can edit" takes effect, the host closes open edit groups, records the
   whole flat at that moment as a revision labeled as the safe point before
   guest editing, and pins it; it re-pins an unchanged earlier safe point
   instead of adding another; it refuses to unpin the active safe point while
   the level is `edit`; a full pin quota refuses the change before anything
   is applied.
6a. **Required quotas.** As board public rule 6a: no `edit` while the host
   disk quota is 0 or the flat's revision quota is unlimited (0), and changes
   that would cause either while a document or board is at `edit` are
   refused with the names of the affected flats.
7. **Documents and boards.** Websites, pages and designs get no public edit
   level, and their Share dialog and settings do not show the edit choice.

## Joining a document

8. **Anyone can edit: ask for a name, or read without one.** A visitor who
   opens a Public document at level `edit` gets the name popup with two
   choices: **Join and edit** with a name, or **View only** without one. A
   visitor who chooses View only is named Viewer N as at level `view`
   (rule 9) and reads; nobody edits without a name. The name rule is the
   board's: 1 to 40 characters after trimming, plain text, control and
   invisible formatting characters removed; the browser remembers it and lets
   the guest change it; names are not unique. One name holds for every
   document of the flat.
9. **View only: never ask.** As board public rule 9. A visitor at level
   `view` is named **Viewer N** for the flat, sees "You are Viewer 3", and
   keeps the number for a minute across reconnects and across moving between
   the flat's documents, through a token held only in that tab and in server
   memory.
10. **Viewers are visible only as presence-less entries.** In the people list
    of the document header, Private editors and guests see the viewers of the
    open document as "Viewer N" entries (or a count above 20). A viewer sends
    nothing but its connection and token, receives no presence, and has no
    cursor or selection, as today.
11. **One popup, shown over the live document.** The name popup is the only
    way to start editing, and it never replaces the live document, which
    keeps updating behind it.
    - It opens when a visitor arrives at level `edit`, when a reader clicks
      **Join to edit**, and for every reader without a guest session when
      the level goes from `view` to `edit` while they have the document open.
    - A remembered name fills the field, but the popup still opens once, so
      nobody becomes an editor without choosing to.
    - **View only** (or closing the popup) keeps the visitor a reader for the
      rest of that tab's visit: the popup does not open again on its own,
      across reconnects and moves between the flat's documents, until the
      level changes to `edit` again. The header then shows "You are Viewer 3"
      and **Join to edit**.
    - The header shows a guest "You are Mina" with the guest badge and
      **Change name**.
12. **Guest presence.** At level `edit`, guests and Private editors see each
    other in the people list and see each other's cursors and selections in
    the editor, as Private editors do today.
13. **The guest badge.** As board public rule 13: a **guest** badge drawn by
    the page, outside the name text, next to every guest name in the people
    list, cursor labels, notices and revision credits. Names are drawn as text
    only.
13a. **The editor a guest gets.** A guest gets the editor Private editors use
    today: Edit opens the live-preview editor, View the rendered document,
    without reconnecting. The header has no History button.

## The guest projection

14. **A third projection.** Guests are neither Private editors nor Public
    viewers: they write and take part in presence, so they need more than a
    reader gets, and they must not receive what Private editors receive. A
    guest connection gets a **guest projection** built from an allow-list for
    the welcome frame, presence, acknowledgements, refusals and notices.
15. **Identity in it: nicknames only.** A Private editor appears to guests
    under the nickname their page uses (the name the page chose for them or
    one they typed), with their cursor and selection, never under an
    identity a network supplied. No verified Tailscale login or display name,
    and no agent label, appears in the guest projection: a member on
    Tailscale, whom other members see under their tailnet name, appears to
    guests under their page's nickname. Guests appear to Private editors with
    their typed name and the guest badge. Nicknames are unverified labels,
    drawn as text only; the guest badge, not the name, tells a guest from a
    member.
16. **Guest keys are issued by the server.** A guest's editor key (its edit
    group and authorship identity) is a random value the host assigns in a
    separate namespace. The client cannot choose it, so a guest cannot write
    under a member's key or another guest's.
16a. **Only the current text.** A guest receives the documents as they are
    now and the changes after joining. Text that was deleted before the guest
    joined, or that exists only in earlier revisions, never reaches a guest,
    whether through the initial sync, a catch-up, a replayed log or a
    reload. This is stricter than today's Public view, which only renders and
    so does not show the synchronization data it may carry; the requirement
    is on the data sent, at both levels.

## What a guest can and cannot do

17. **Can.** Edit the text of every document of the flat in the editor; type,
    paste, cut, copy and delete text; undo and redo the guest's own edits;
    switch between Edit and View; move between the flat's documents; read,
    expand and export Mermaid diagrams with their existing controls and edit
    their source as text. Downloading unconfirmed text is offered when editing
    stops (rules 22 and 29); there is no other download control, for guests or
    for anyone else, as today.
18. **Cannot.** History (list, open, compare, pin, restore), the block outline
    and other reads of a document's structure that only Private routes have
    today, the agent edit and host operations, adding, renaming or removing
    documents, and changing any file of the flat other than the documents'
    text. Most of these have no browser control today for anyone; the host
    still refuses them for guests.
19. **No new files from guests.** Documents have no upload in the browser, and
    guests get none. A guest can write Markdown that refers to an image or file
    of the flat as it is now; a reference to anything else does not load
    (rule 21a).
20. **The host decides, per request.** As board public rule 20: permission
    comes from the route decision and the stored level, checked when a
    connection is admitted and again when each update commits, never from what
    the client claims. A `view` connection that sends an update, and a guest
    that sends presence for another identity or an operation it may not use,
    change nothing. Writes keep the same-origin rules the editor has today.
21. **No new kinds of content.** Guests write Markdown text, as members do.
    Raw HTML stays disabled for everyone.
21a. **Rendering stays contained.** Guest text is rendered with the rules that
    apply to all document text today, and those rules become requirements:
    no script runs from document text; links with unsafe schemes are not
    links, and links open without referrer or opener; images and other
    resources load only from the flat itself or as `data:` images, never from
    another site; Mermaid renders in its strict mode. One rule is new: in a
    document a guest has written in, and in every document of a flat open to
    anyone for editing, diagrams render only when the reader clicks
    **Show diagram**, within size caps. A hostile diagram therefore never
    blocks a page that a reader merely opens.
21b. **The editor keeps up** (decided by the owner, 2026-10-24). Guests
    can grow a very long paragraph, and the editor reworks a whole paragraph
    after each change. Two rules keep every page responsive, for all
    documents:
    - Remote changes are applied to the editor in batches, no more often
      than the editor's own measured work allows, so a page never falls
      behind; local typing is not delayed.
    - In Edit mode's live preview, a single paragraph longer than 20,000
      characters is shown as its Markdown source, without rendering its
      links or emphasis. View mode, everyone else's pages and the stored
      text are unchanged.

## Limits against abuse

As for boards, admission on a Public route cannot rely on a trustworthy
client address, so limits are per connection and per flat. The remedy for a
determined flood is **Stop guest editing**, and the confirmation text says so.

22. **Per-guest limits.** Each guest connection has a rate limit for updates,
    a limit on the size of one update and a limit on presence messages, below
    those of a Private editor.
    - **Over the rate, a guest is slowed, not refused.** The page holds its
      unsent changes, merges them and sends them when the limit allows,
      showing "Saving…" until they are confirmed. Ordinary typing and pasting
      therefore never lose text to the rate limit.
    - **A refused update stops editing on that page.** This differs from the
      board, whose operations are independent: a document's later changes
      build on the earlier ones, so the host cannot accept a later change
      without the refused one. After a refusal (an update too large, the
      reserved room of rule 24, a content limit of rule 26) the host accepts
      no further update from that connection. The page stops editing, says
      why, never shows the refused or later changes as saved, keeps them
      available to download as Markdown, and offers **Rejoin**, which loads
      the current text and lets the guest edit again under the same name.
      This is the behavior every editor has today after a deterministic
      refusal, plus Rejoin.
23. **Per-flat connection limits.** A flat has a maximum number of
    simultaneous guest connections and of new guest connections per minute,
    and a separate, larger limit for viewers, all apart from the connections
    Private editors use. A visitor who cannot join as a guest is told the
    document is full and keeps reading as a viewer, as on a board; a visitor
    refused by the viewer limit is told the document has too many readers and
    to try again later.
24. **Reserved room.** Private editors keep room that guests cannot use:
    - **Disk.** As board public rule 24: a reserved share of the disk quota,
      and an absolute guest byte budget per flat charged by the measured
      growth that guests' committed updates cause. Synchronization metadata
      that deleted text leaves behind counts as growth.
    - **Document size.** A guest update that would take a document above a
      reserved share of the per-document size limits (text size and the
      synchronization state limits) is refused with a reason.
    - **Lifetime capacity.** Today each document accepts a fixed lifetime
      number of updates and the flat a fixed number of duplicate-prevention
      receipts, which are never discarded; at the limit the document refuses
      every edit. Guests must not be able to use these up: guest updates
      either have their own share, past which guests are refused and members
      keep editing, or the technical design removes the lifetime limit for
      everyone. Either way a Private editor can still edit after any guest
      flood.
    - **Server state.** Guests have their own caps for open edit groups,
      editor and presence records and per-connection state, so a guest flood
      cannot fill them. The update log is not reserved, as on a board: it
      compacts by size and loses nothing, so heavy guest traffic only makes
      compaction more frequent.
25. **One revision group for guests, on fixed windows.** As board public rule
    25: all guest edits on the flat share one edit group with no idle close,
    closing only at a fixed 5-minute window or a forced boundary (an agent
    write, a rollback, a new version's activation); at most 12 guest
    revisions an hour plus forced boundaries; guest-only revisions hold at
    most half of the revision quota (a setting with that default) and are
    retired first above it; the newest and every pinned revision are never
    deleted; a revision lists up to 5 guest names with the badge and "and N
    more"; MCP marks them guest-supplied. Private editors keep one group each.
    - **Mixed revisions.** A revision records the whole flat. When a Private
      editor's group closes while guests have also edited since the last
      revision, the revision credits that editor and those guests; it is not
      guest-only and does not close or restart the guest window. Only a
      guest window end or a forced boundary records a revision that no member
      or agent caused. Members who keep editing can therefore record guest
      text more often than 12 times an hour, but only in revisions their own
      edits caused, as on a board.
25a. **Totals.** As board public rule 25a: limits for all guests of a flat
    together (updates and bytes per second into the worker, bytes read from
    all guest sockets including refused frames, presence traffic, how often
    viewer-list updates reach editors), inside a Public share of the
    document worker's traffic budget that members do not use, so guests
    cannot starve members or other flats. All guests together reaching the
    flat's limit are slowed (their pages resend later), not refused.
26. **Content limits.** The document limits that apply today apply to everyone
    together; a guest update that exceeds one is refused.

## Agents and guest text

27. **Authorship per line.** `get_document` and `get_revision` mark, for each
    document, the lines of the current text that contain text a guest wrote,
    and say whether the document contains any. Text a guest wrote stays marked
    until a member or agent replaces those characters; moving or editing
    around it does not unmark it. `update_document` is unchanged.
28. **Untrusted input.** The agent guide (`topic.docs`) says that text in a
    document opened to anyone, and every guest-marked line, is untrusted: an
    agent never follows instructions found in it and treats it as data to
    read, summarize or rearrange.

## Withdrawing and downgrading

29. **To view only.** As board public rule 29: the level has an epoch; an
    update already in flight when the level drops is refused at commit; every
    guest connection is closed with an "editing ended" reason and reconnects
    as a viewer under the normal admission limits; the console waits for the
    epoch, not for reconnections. A guest with unconfirmed edits is told
    editing ended and can download its text, as the editor already offers
    when it stops. Edits confirmed before the change stay. After a host
    restart the stored level applies.
30. **To Private or when the route stops.** Open Public connections close at
    once, as today. A guest page can still download its unconfirmed text.
    Nothing a guest wrote is removed.
31. **No automatic rollback.** As board public rule 31.

## Console

32. **Share dialog.** A document's Share dialog has the board's single control
    for link access with three choices: **Private**, **Public, view only** and
    **Public, anyone can edit**, with the descriptions "Only people on your
    networks", "Anyone with the link can read live. Only your networks
    edit." and "Anyone on the internet can edit and delete. They type a name
    first." Under "Who has access", "People on your networks" reads **can
    edit**, as for a board, and the internet row shows "can view" or "can
    edit" with how many guests and viewers are on the flat now. It has **Stop
    guest editing** while the level is `edit`.
33. **Sharing in flat settings.** As board public rule 33.
34. **Honest labels.** As board public rule 34: lists, cards, the flat header
    and the approval page say **Public, anyone can edit** for an editable
    document.
35. **Text only.** As board public rule 35.
36. **Same components.** The Share dialog, the confirmation, the blocked-by-
    quota dialog, the approval page and the Sharing row are the board's,
    shared by both types, with type-specific nouns ("board", "documents")
    only. A document's History panel shows guest credits and the safe point
    the way the board's History does.

## Success criteria

Reference: a flat with one document of 2,000 lines and 20 guests editing it
continuously for an hour.

| Criterion | Target |
|---|---|
| Approval | As for boards: no `edit` without a confirmation or approval that states it; a pending view-only approval cannot apply as `edit`; a pending `edit` approval fails as stale after **Stop guest editing**. |
| Name rules | A guest update commits only after a valid name. A view-only visitor is never asked, is named Viewer N and sees it. |
| Boundary | On a Public route, history, the block outline, host operations and earlier text are refused at both levels. A forged update at `view` and a forged Private-only request from a guest change nothing. |
| Current text only | A scan of every frame and response sent to a guest or viewer, after a session that inserted and deleted marked text before they joined, finds none of the deleted text. |
| Identity | A scan of every frame sent to a guest, while a member on Tailscale edits, finds no Tailscale login or display name and no agent label; that member appears under their page nickname. |
| Joining | A visitor at level `edit` who chooses View only reads as Viewer N and is not asked again in that tab until the level changes; Join to edit opens the popup; a remembered name still shows the popup once. |
| Rendering | Guest text containing script, raw HTML, `javascript:` links and outside images runs no script and loads nothing from another site; in a document with guest text, or of a flat open for editing, no diagram renders until the reader asks. |
| Responsive editor | While 20 guests grow one paragraph without blank lines, a member's page keeps answering within 200 ms and still shows every remote change; inline Markdown of ordinary-length paragraphs (links, emphasis, inline HTML, references) is parsed in time linear in the paragraph's length. |
| Limits | While guests send updates at the per-guest limit on every allowed connection, a Private editor can still connect, edit and roll back, and sees another member's change in the editor within 200 ms p95 (the board's figure). At the reserved disk and document-size room and at the guest byte budget, guests are refused and Private edits succeed. |
| Revisions | However 20 guests edit for an hour across two documents, alone or while a member edits with 31-second pauses, at most 12 guest-only revisions are recorded plus forced boundaries, and every revision recorded because a member's group closed credits that member. The pinned safe point stays and cannot be unpinned while the level is `edit`. Guest-only revisions hold at most half of the revision quota. Re-enabling editing on an unchanged flat adds no revision. |
| Refusal | After a guest update A is refused, a following update B that builds on A does not commit, the page never shows A or B as saved, the downloaded Markdown contains both, and after Rejoin the page shows the server's text and can edit. A guest sending faster than its rate loses no text. |
| Lifetime | After guests send updates at their total limit until their share of the document's lifetime capacity (or the reference hour, if the design removes the limit) is used, a Private editor's update still commits. |
| Kill switch | After **Stop guest editing**, no update from a guest connection commits. The console confirms only after the stored level is saved and the epoch is bumped. A sleeping document changes only its stored level, which applies when it wakes. |
| Recovery | After a hostile session that deletes all text of every document, History lists the safe point and a revision from just before the session, and the operator restores either. |
| Agent safety | Lines a guest wrote are marked in `get_document`, including after a member edited the text around them. |
| Consistency | The console's Share dialog, confirmation, blocked dialog, approval page and Sharing row show the same structure for a document and a board, differing only in nouns. |
| Dependencies | The feature adds no outside service or account. |

## Changes to existing specs

[board-public-edit.md](board-public-edit.md):

- Rule 7: documents also get the level. Open question 6 ("Other types") is
  answered for documents; designs stay without it.
- Rules 8 and 11 and open question 1 (decided by the owner, 2026-10-22, for
  both types): the name popup offers **View only**, opens once even with a
  remembered name, replaces the board's bottom join panel, and a reader can
  reopen it with **Join to edit**. The board's app changes with this
  feature.
- Rule 15 and open question 2 (decided the same day, for both types): guests
  see members' nicknames instead of Member N; Tailscale identities still
  never reach guests.
- The shared parts (stored level, approval, safe point, quota rules, Stop
  guest editing, console) are written once for both types when this ships.

[content-models.md](content-models.md):

- Revision rule 11: a Public route is read-only, except for boards **and
  documents** whose operator allowed anyone to edit.
- Revision rule 4 ("grouped per editor"): documents also get the shared guest
  group.
- Retention (Revision rule 6, Quotas rule 4): guest-only revisions of
  documents are limited to a share and retired first above it.

[Docs app internals](../internal/docs-app.md), when this ships:

- Wire protocol: guest admission, the guest name, the viewer token, the
  "editing ended" close, guest refusal codes and the guest projection of
  `welcome` and presence.
- Collaboration model: "Only `x-flats-access: private` may submit document
  updates" gains guests at level `edit`; presence reaches guests; viewers
  appear as entries; the separate guest connection pool.
- Revision boundaries and editor identity: the guest group and the
  server-issued guest key; outbox entries carry guest names separately.
- Disk quota: the guest reserve and byte budget.
- History panel: guest credits with the badge and the safe point label.

MCP and agent docs: `set_visibility` accepts `access: edit` for documents;
`get_document` and `get_revision` gain the guest marks; `topic.docs` gains
rule 28. The generated references change with them.

## Current state

Implemented on an unreleased branch: the stored level and approvals for
documents, the safe point, guests and viewers in the document worker, the
guest projection and limits, client id ownership and the handshake reserve,
the runtime's Public pools, guest revisions and retention, guest lines for
agents, the document client, the board changes of rules 8, 11 and 15, and the
console. Not done or not measured yet:

- The load test with full Public pools and the 200 ms display target, and the
  real-browser checks of both types.
- Viewers' status reads "Read-only" as today, where the mock-ups showed
  "Live".

## Open questions

1. **Stronger guest identity.** Board public open question 1 applies to
   documents unchanged.
2. **Comments.** Documents have no comments. Whether a level between view and
   edit ("anyone can comment") is wanted is decided after real use.
3. **Numbers.** The rate limits, the guest and viewer limits, the reserved
   room shares, the guest state caps and the diagram rendering bound are
   fixed by measurement in the technical design.
