# Anonymous public editing for boards: technical design

Status: design for the unshipped feature of
[board-public-edit.md](board-public-edit.md). Rule numbers refer to that page
unless they say "board rule", which refers to [board.md](board.md). Section
numbers such as (TECH 7.3) refer to [board-TECH.md](board-TECH.md); plain
numbers such as (3.2) refer to this page.

## 1. Decision summary

| Topic | Decision |
|---|---|
| Stored level | New `flats.public_access` column (`''`, `view`, `edit`). Only a Public board can hold `edit` |
| Changing it | The existing `set_visibility` approval gains `access` and `from_access`. The console self-approves as today; Stop guest editing is that same call with `access: view`, with no confirmation dialog |
| Telling the board | Core adds the trusted header `X-Flats-Public-Access` on Public requests and pushes the level to a running instance with `Instance.SetPublicAccess`. The instance keeps an access epoch |
| Safe point | Applying `edit` asks the instance for a forced `safe_point` revision, imports it synchronously, pins it and stores its number in `content_state.safe_point_rev`, all before the level commits |
| Guest identity | The server issues a random guest token; the key is `guest:` plus a hash of the token. The client never chooses the key |
| Guest projection | Private wire form with every author key and name rewritten by a per-instance label map (`member:N`, `guest:N`, `agent`) |
| Limits | Guests have their own rate, size, connection, server-state and room limits, checked in the reader and again in the apply loop |
| Revisions | One shared `guests` group on fixed 5-minute windows. Outbox entries carry guest names separately; core marks guest-only revisions and retires them first above half the quota |
| Authorship | Two new element content fields: `lk` (last changer kind) and `gt` (text bundles last written by a guest) |

## 2. Stored level and approvals (core, store)

### 2.1 Storage

- Migration 19 adds `flats.public_access TEXT NOT NULL DEFAULT ''`. Existing
  Public flats are migrated to `view`; other flats keep `''`.
- `store.Flat` gains `PublicAccess string \`json:"public_access,omitempty"\``.
  `FlatView` (and so `get_flat`, `list_flats`, the console list and the MCP
  `FlatInfo`) reports it. A non-board or Private flat always reports it absent.
- `content_state` gains `safe_point_rev INTEGER` (nullable).
- `flats` also gains `access_seq INTEGER NOT NULL DEFAULT 0`, incremented by
  every commit that changes visibility or `public_access`.
- Invariant, enforced in `CommitVisibility`: `public_access` is `''` when
  visibility is `private`, `view` or `edit` when `public`, and `edit` only for
  type `board`.

### 2.2 The approval

`visibilityParams` gains:

```go
Access     string `json:"access,omitempty"`      // requested: "view" or "edit"; empty means "view" when Public
FromAccess string `json:"from_access,omitempty"` // level when requested; empty for Private
AccessSeq  *int64 `json:"access_seq,omitempty"`  // flats.access_seq when requested
```

- `SetVisibility(slug, visibility, access)` normalizes `access`: empty becomes
  `view` for Public; any value for Private is refused as `invalid`; `edit` on a
  non-board is refused as `invalid`.
- "Unchanged" now compares the pair (visibility, access).
- **Staleness** (`applyVisibilityApproval`): in addition to today's checks, the
  approval is stale when the current `access_seq` differs from `AccessSeq`.
  Any visibility or level change in between, even a round trip that restores
  the same pair, therefore makes it stale for good. A legacy approval without
  `access_seq` (requested before this feature) keeps today's comparison of
  `from` and applies only as `view`; it can never grant `edit`.
- The idempotency key includes the new fields, so a pending `view` request and
  a pending `edit` request are different approvals.
- `CommitVisibility` becomes a compare-and-set on
  `(visibility, public_access, access_seq, live_version)`, writes both columns
  and increments `access_seq`.

### 2.3 Applying `edit`

`applyVisibilityApproval` for a target of `(public, edit)` runs these steps
under the flat lock, in order. Any failure fails the approval and changes
nothing that a guest could use:

1. **Preflight** (`publicEditProblems`): host `disk_quota_bytes > 0`;
   effective revision quota `> 0`; either a reusable safe point (step 2) or
   one free pin. Each problem has a code (`disk_quota_off`,
   `revision_quota_unlimited`, `pin_quota_full`) and the approval fails with
   the new refusal category `public_edit_blocked`, listing them.
2. **Safe point.** Call `inst.SafePoint(ctx)` on the running board instance
   (opening it if needed). The instance, in its apply loop, closes every open
   group (a `person` entry when dirty) and records a **forced** entry of kind
   `safe_point` (5.3 below). Core imports the outbox synchronously and gets the
   revision number R.
   - **Reuse:** when `content_state.safe_point_rev` names a revision that
     still exists and its file list equals R's, core deletes nothing extra and
     uses that revision instead of R for the pin (R stays as an ordinary
     revision).
3. **Pin.** Pin the chosen revision through the existing pin path; a full pin
   quota fails here with `pin_quota_full`. Store its number in
   `content_state.safe_point_rev`.
4. **Commit.** If the flat was Private, run today's Public exposure steps.
   Then `CommitVisibility(public, edit)`.
5. **Notify.** `inst.SetPublicAccess("edit")`.

A pin created in step 3 is kept even if step 4 fails; it is an ordinary pinned
revision the operator can unpin. This avoids a rollback path that could itself
fail.

### 2.4 Leaving `edit`

`(public, view)` and Private commit as today. After the commit core calls
`inst.SetPublicAccess(new)` (`view` or `""`), which closes the guest group and
ends guest editing (6.1). Moving to Private also revokes the public route
epoch as today, closing every public socket.

### 2.5 Protecting the safe point and the quotas

- `PinRevision(unpin)` refuses with `conflict` ("the safe point stays pinned
  while anyone can edit") when the flat is `edit` and the revision is
  `safe_point_rev`. This covers HTTP, the board History route and MCP, since
  all reach `PinRevision`.
- The rule is checked on the **effective** values after the change, not on
  the submitted value. `SetRevisionQuotas` refuses any change (a value of 0,
  or clearing the override while the host default is 0) whose resulting
  effective revision quota would be 0 while the flat is `edit`.
- Host settings (`cleanSettings`/`UpdateSettingsMatch`) refuse
  `disk_quota_bytes: 0` while any board is `edit`, and a default
  `revision_quota: 0` while any `edit` board has no per-flat override, naming
  the affected flats.
- **Serialization.** A service-wide `publicEditMu` is held by the whole of
  2.3 (applying `edit`), by `SetRevisionQuotas` and by the host settings
  update, each around both its check and its commit. It is taken before the
  flat lock, and the settings path takes no flat lock, so the order is fixed
  and the two checks cannot pass concurrently.
- The disk quota is a host setting (there is no per-flat disk quota), so spec
  rule 6a's "per-flat change" applies to the revision quota only.
- `flats config set` edits the file offline. At startup, a board at `edit`
  whose effective quotas violate the rule is set to `view`, with a
  `visibility` event that says why.

### 2.6 Telling the board instance

- `trustedAccess` deletes any client `X-Flats-Public-Access`. `serveSite`, on
  a Public request, sets it from the flat it already re-reads per request:
  `view` or `edit`.
- `board.Options` gains `PublicAccess string`, so an instance opened later
  starts with the stored level. `Instance.SetPublicAccess(level)` updates it at
  runtime.
- The instance treats a request as guest-capable only when **both** the header
  says `edit` and its own level is `edit`. Either one alone is not enough, so a
  race between the header and the push fails closed.

### 2.7 Interfaces

- **MCP** `set_visibility` gains `access` (`view`|`edit`). `get_flat` and
  `list_flats` show `public_access`. The approval result data includes both
  fields.
- **HTTP** `POST /flats/{slug}/visibility` accepts `access`.
  `GET /flats/{slug}/public-edit` (console) returns the preflight:
  `{problems: [...], disk_quota, revision_quota, pin_free, safe_point, listed}`
  where `listed` says whether a permitted provider lists the flat publicly.
- **CLI** `flats visibility <slug> public --access edit`.
- New refusal category `public_edit_blocked` in `ErrorCategories` and
  `docs/agent/refusals.md`.

## 3. Connections (board)

### 3.1 Classification

`serveWS` classifies a socket after `hello`:

| Condition | Kind |
|---|---|
| `X-Flats-Access: private` | member (today's private socket) |
| Public, guest-capable (2.6), hello has a valid `guest` object | guest |
| Public otherwise | viewer |

- The hello gains `guest: {name, token?}` and `viewer_token?`.
- **Guest token.** Without a token the server generates 16 random bytes and
  returns them in `welcome.guest_token`; the client keeps it in
  `sessionStorage` (`flats-board-guest-token`). The editor key is
  `guest:` + first 16 hex digits of SHA-256(token). A client that sends a
  token it was never given just gets a new, unrelated key.
- **Name.** `cleanGuestName`: trim, remove C0/C1 controls and Unicode format
  characters (category Cf), 1 to 40 runes after cleaning; otherwise the hello
  is refused with fatal `name_invalid`.
- **Viewer number.** Each viewer has a token (`viewer_token`, server-issued,
  kept in `sessionStorage`). The instance keeps `token → N` in memory and holds
  a released N for 60 seconds. N is the lowest number not held. Nothing is
  stored on disk.
- **Admission.** Guests and viewers come out of the existing public pool (256)
  and the host public budget. Guests additionally have a cap of 50
  simultaneous guest sockets per board and 30 new guest welcomes per minute.
  Over a guest cap the welcome is refused with fatal `full`; the client offers
  Watch (reconnect as a viewer) or Try again.

### 3.2 Sessions

- A guest gets an ordinary session row whose key is its guest key and label is
  its name. The welcome carries a session as for members, so receipts,
  duplicate prevention, the resend of pending edits and undo work unchanged.
- **No history for guests.** A guest socket never receives the delta log: on
  every welcome, including a reconnect, the server ignores `watermark` and
  sends a fresh guest snapshot. The guest snapshot contains only live
  elements (no deleted rows, no `pre_rel` of deleted rows). Deltas sent to a
  guest after its welcome are the current changes, rewritten (3.3).
- **Revive.** Rows gain `del_key` (the editor key that deleted them; new
  column `elements.del_key`, replacing the `del_guest` flag of 4.4). A guest
  batch may `revive` only rows whose `del_key` is that guest's own key and
  whose `del_seq` is later than the guest session's start; anything else is
  reject `guest_revive`. A guest therefore can only bring back what it
  deleted itself, never content removed before it joined.
- Guest sessions expire 1 hour after their last socket closes (members keep
  24 hours), and at most 200 guest session rows are kept; above that the
  oldest guest sessions without a live socket are dropped (with their created
  rows, receipts and undo stacks, as `dropSession` does).

### 3.3 Projection

A guest socket uses the private wire form (it needs `inc`, `v`, `end_inc` and
the private delta to merge and undo) with one rewrite, `guestLabels`, applied
to every serialized author:

| Real key | Guest sees | Name sent |
|---|---|---|
| `ts:*`, `browser:*` | `member:N` | `Member N` |
| `guest:*` | `guest:N` | the guest's name |
| `agent:*` | `agent` | `Agent` |
| `system` | `system` | none |

- N is assigned per instance in first-seen order and kept for the instance's
  life; it is never derived from the key.
- Rewritten: element `by`; delta `by`; presence `key`, `name`, `verified`
  (always false for members as seen by guests: a guest is never told a member
  is verified); gone `key`; agent activity labels.
- Not sent to guests: shares, `welcome.me` of others, session labels, the
  History and recovery routes.
- The rewrite runs once per delta per audience (like the lazy public body,
  `broadcast`), not per socket.
- Exports for a guest use the public projection (TECH 7.3).
- A test serializes every frame type to a guest socket and fails if it finds
  a `ts:` or `browser:` key, a member's name or an agent label.

### 3.4 Presence

- Guests send `pres` (rate 10/s per socket instead of 20/s) and receive the
  presence of members and guests (rewritten). Members receive guests'
  presence with `guest: true`.
- Viewers send nothing but their connection. Members and guests receive a
  `viewers` frame `{t: "viewers", n: [1, 2, ...]}` (or `{count}` above 20),
  sent at most once per second per board, only when the set changed.
- Each socket learns its own label in the welcome: `me.label` is `Viewer N`
  for a viewer and the guest name for a guest.

### 3.5 What a guest may send

| Frame or route | Guest |
|---|---|
| `ops` | yes, with the guest limits (4.1) |
| `ops_part` | no (fatal `protocol`) |
| `pres` | yes |
| `share` | no |
| `POST /_board/api/assets`, `/import`, `/recover`, revisions routes | no (403 or 404 as for public today) |

- `checkRefs` for a guest batch: an `add` that names an asset is accepted only
  when a live element references that asset now (`Refs > 0`); a `revive` of an
  image element is accepted only under the same condition. Otherwise the batch
  gets reject `guest_asset`.

## 4. Limits (board)

### 4.1 Per guest

| Limit | Value | Where |
|---|---|---|
| Batches | 10/s, burst 20 | reader (`submit`) |
| Ops per batch | 100 | reader |
| Frame size | 64 KiB | reader |
| Presence | 10/s | reader |

Over a limit the operation gets reject `guest_rate` or `guest_size` and the
socket stays open.

### 4.2 Per board, all guests together

| Limit | Value | Where |
|---|---|---|
| Ops | 200/s token bucket | apply loop, before SAVEPOINT |
| Bytes read from guest sockets | 2 MiB/s token bucket, refused frames counted | reader |
| Presence relayed from guests | 200 frames/s | `relayPres` |
| Guest share of the host public budget | 64 MiB of the 256 MiB | `reservePublic` |

### 4.3 Reserved room

Checked in `stageBatch` for guest batches only, after the batch applies and
before it commits (the existing post-write measurement), so a refusal rolls
back to the savepoint:

- **Disk.** Refuse with `guest_room` when usage after the batch is above 90%
  of the disk quota.
- **Guest byte budget.** `meta.guest_charge` grows by the positive usage
  change of every committed guest batch. On each tick it is lowered to
  `min(guest_charge, current usage)`, so it falls when retention frees space.
  A guest batch that would take it over 512 MiB is refused with `guest_room`.
  This is the accounting rule 24 asks for; spec rule 24 is updated to describe
  it.
- **Elements and assets.** Refuse with `guest_room` when the batch would take
  live elements above 9,000 (90% of 10,000) or referenced assets above 460.

### 4.4 Server state

| State | Guest cap | How |
|---|---|---|
| Receipts | 4 MiB of the 16 MiB | `TrimReceipts` trims guest sessions' receipts first while their total is over 4 MiB |
| Undo stacks | 8 MiB of the 32 MiB | `TrimUndo` trims guest stacks first while their total is over 8 MiB |
| Deleted rows | 4 MiB of the 16 MiB | `elements.del_key` (3.2); `TrimDeleted` trims rows deleted by guest keys first while their total is over 4 MiB |
| Sessions | 200 rows, 1 hour TTL | 3.2 |
| Created-id rows | 5,000 per guest session, 50,000 for all guest sessions | counted in memory from `created`; a guest batch that would add rows over a cap is reject `guest_room`, so id-reuse protection is never dropped to make room |
| Group-id records | 2,000 group ids first used by guests | `meta.guest_groups` counts new `usedGroups` rows added by guest batches; a guest group op over the cap is reject `guest_room`. These rows stay permanent, as today, so no group id is ever reused |
| Delta log | shared | see below |

The delta log stays shared. Sustained guest traffic, within the per-board
limit of 4.2, can push older entries out of it, so a member who reconnects
after a long gap may get a snapshot instead of a catch-up. A snapshot loses
no edits (the member's pending edits are resent as today); it costs a larger
download. Spec rule 24 is reworded to promise only what is kept: guests
cannot evict members' undo history, receipts or sessions.

## 5. Revisions (board, core)

### 5.1 The guest group

- Guest batches touch one group with editor `guests` and a new column
  `fixed = 1`. `closeGroups` never closes a fixed group for idleness; it closes
  it when `started_at` is 5 minutes old, or at a forced boundary (agent write,
  rollback, import, safe point, leaving `edit`).
- The group keeps a JSON list of contributing guest names in first-edit order
  (new column `names`, at most 64 entries).
- **Who triggers a record.** Today `closeGroups` records one entry whenever
  any group is due and any group is dirty, and recording clears every group's
  `dirty`. That changes to: record only when at least one **due** group is
  dirty; a due group that is clean is deleted without a record. The guest
  group is due only at its window end. An entry recorded because a member
  group closed credits that member (and the guests who contributed since the
  last entry), so it is not guest-only. Recording clears the guest group's
  `dirty` but keeps its `started_at`; at its window end a clean guest group is
  deleted without a record.
- Result: a guest-only entry is recorded only at a guest window end or a
  forced boundary, at most 12 an hour plus forced boundaries, however members'
  groups interleave. Tested with interleaved member idle expiries.

### 5.2 Outbox and credits

- Outbox entries gain `guests` (JSON array of up to 5 names) and
  `guests_more` (count of the rest). Guest names never go into `authors`.
- Core's revision rows gain `guest_authors`, `guests_more` and `guest_only`
  (`authors` empty and `guests` non-empty). `list_revisions` and
  `get_revision` return `guest_authors` with `guest: true`; the History panel
  shows them with the guest badge and "and N more guests".
- Kind `safe_point` is a new revision kind, shown as "Safe point before guest
  editing".

### 5.3 Retention

`retainTx` becomes:

1. Let Q be the revision quota (0 means unlimited: nothing to do) and
   G = floor(Q / 2).
2. While the guest-only unpinned revisions, excluding the newest revision,
   number more than G, delete the oldest of them.
3. Then today's rule: while unpinned revisions exceed Q, delete the oldest,
   never the newest.

The newest revision and pinned revisions are never deleted. The "revision
before an agent write" is an ordinary entry and follows these rules.

The guarantee is the one spec rule 25 states: guest-only revisions never hold
more than half of the quota, so among unpinned revisions other than the
newest, member and agent revisions keep at least Q − G places. Guests do
**not** get first eviction below their half: recent guest history stays
recoverable. The spec's Revisions criterion is reworded to this.

## 6. Level changes (board)

### 6.1 Epoch

- `Instance.SetPublicAccess(level)` sends an item into the apply loop. In the
  loop it sets the level, increments `accessEpoch`, and if the new level is not
  `edit`, closes the guest group (a `person` entry when dirty).
- Each guest socket records the epoch at its welcome. `stageBatch` refuses a
  guest batch whose socket epoch differs from the current epoch, with reject
  `permission`, before SAVEPOINT. A batch that committed before the item is
  kept.
- After the item, every guest socket gets fatal `editing_ended` and is closed.
  The client shows the banner with the export of unconfirmed edits and
  reconnects as a viewer (spec rule 29). Reconnecting goes through the normal
  admission and snapshot limits.
- When the level becomes `edit`, viewer sockets get `{t: "level", access:
  "edit"}` and show the join prompt.
- Core confirms Stop guest editing to the console after `CommitVisibility`
  and `SetPublicAccess` returned; when no instance runs, the stored level alone
  changes and `Options.PublicAccess` applies it at the next open.

### 6.2 Safe point

`Instance.SafePoint(ctx) (seq int64, err error)` runs in the apply loop: record
a `person` entry for dirty groups, delete all groups, record a forced
`safe_point` entry, return its outbox seq. Core then imports and maps it to a
revision through `board_imports` (by outbox seq).

## 7. Authorship (board, model)

- Element content gains `lk` (`member`, `guest`, `agent` or `system`: the kind
  of the last change of any bundle) and `gt` (list of text bundles last written
  by a guest: any of `text`, `label`, `title`, `alt`).
- A guest write of a text bundle adds it to `gt`; a member or agent write of
  that bundle removes it. Any write sets `lk`.
- Both fields are part of `Content()`, so revision snapshots and `get_revision`
  carry them. The public projection leaves them out. `get_board` returns them
  as `last_change` and `guest_text`.
- Old elements without the fields read as `lk` unknown and `gt` empty.

## 8. Clients

### 8.1 Board app (`internal/board/web`)

- `welcome.access` gains `guest`. A new `body.guest` class: guests see the
  tools, undo, presence and avatars; `.priv-only` items (History, Send to
  agent) and upload, import and the image tool stay hidden.
- At level `edit`, a visitor without a stored guest name sees the name prompt
  (a dialog over the live canvas): name input with a 40-character counter,
  the guest badge preview and the notes of the design. Joining restarts the
  socket with `hello.guest`. The stored name lives in `localStorage`
  `flats-board-guest-name`, the token in `sessionStorage`.
- Viewers show "You are Viewer N" and a View only chip; `level: edit` shows the
  join prompt as a non-modal panel at the bottom.
- The guest badge (`.badge.guest`) is drawn next to every guest name: cursor
  tags, the people list, "is editing" marks and notices. Member entries seen
  by a guest show "Member N" with a member badge.
- Rejects `guest_rate`, `guest_size`, `guest_room`, `guest_asset` and
  `permission` map to error toasts. Fatal `editing_ended` shows the banner
  "The owner turned off guest editing" with "Save my changes to a file", then
  reconnects as a viewer. Fatal `full` shows the Watch / Try again dialog.
  Fatal `name_invalid` reopens the name prompt with the reason.

### 8.2 Console (`internal/console/static`)

- `VISIBILITY` handling gains the access level. Labels: "Private",
  "Public, view only", "Public, anyone can edit".
- The Share dialog replaces the select with a radio group (two choices for
  non-boards, three for boards), lists "People on your networks" and "Anyone
  on the internet" with live guest and viewer counts (from a new field
  `public_presence: {guests, viewers}` in the board's flat view, read from the
  running instance), and shows **Stop guest editing** at level `edit`.
- Choosing "anyone can edit" fetches `GET /flats/{slug}/public-edit`. With
  problems it shows the blocked dialog with one action per problem (Limits,
  History); otherwise the confirmation dialog of the design, then calls
  `setVisibility` with `access: edit` and self-approves.
- Stop guest editing calls `setVisibility(public, view)` and self-approves with
  no confirmation dialog. The existing confirmation for Public → Private
  stays.
- The flat page Sharing row shows the level, counts, the safe point state and
  Stop guest editing. List badges use the new labels. The approval page shows
  the requested and current access level and the safe point note.

## 9. Docs

- `docs/agent/topics/board.md`: public access levels, guests, guest-written
  text is untrusted, `last_change` and `guest_text`.
- `docs/agent/topics/approvals.md`: `access`, `public_edit_blocked`.
- MCP instructions stay under the 2,300-byte cap; no change there.
- Regenerate references with `make docs-generate`.
- board.md, content-models.md, surfaces.md, design.md and board-TECH.md get the
  changes listed in the spec's "Changes to existing specs" when this ships.

## 10. Verification plan

| Spec criterion | Evidence |
|---|---|
| Approval | core tests: `edit` only through an approval or console self-approval; stale after a level change; legacy approval applies as `view`; preflight problems fail the approval without changing visibility |
| Name rules | board tests: guest ops refused before a valid name (viewer socket); invalid names refused; viewer gets `Viewer N` and keeps it across a reconnect within 60 s |
| Boundary | board tests: guest `ops_part`, `share`, upload, import, recover, revisions refused; `view` socket ops refused; guest asset reference to a non-live asset refused |
| Identity | frame-scan test over every frame type to a guest socket |
| Limits | board tests for each limit in section 4, plus a load test with 50 guests at the per-guest limit while a member edits and a measured remote-display p95 |
| Revisions | board+core test over a simulated hour (fake clock) with guests editing continuously and in pauses, and members' groups expiring in between: at most 12 guest-only entries plus forced boundaries; retention keeps guest-only at most half and members at least Q − G (including Q = 1, 2, 4); safe point never unpinned while `edit` |
| Guest boundary | board tests: a guest reconnecting with an old watermark gets a snapshot without deleted content; a guest `revive` of content deleted before it joined is refused |
| Approval round trip | core test: an `edit` approval requested at Public view stays stale after Private → Public edit → view |
| Quota transitions | core tests: clearing an override to a host default of 0 is refused while `edit`; a host change to 0 racing an `edit` approval cannot leave an `edit` board with a 0 quota |
| State caps | board test: one guest session creating and deleting elements and groups past the caps gets `guest_room`, and created/group rows stop growing |
| Kill switch | board test: a batch in flight when the epoch changes is refused; guest sockets get `editing_ended` |
| Recovery | integration test: guests delete everything, rollback to the revision before the session and to the safe point |
| Agent safety | `get_board` shows `guest_text` after a member moves a guest-written sticky |
| Console | console UI tests for the dialogs and labels; a real browser check on a disposable development host (console-ui playbook) |

## 11. Implementation units

Three units with disjoint files, joined by the interfaces of 2.6, 2.7, 3, 6.2
and 5.2:

| Unit | Files |
|---|---|
| A. Core, store, API, MCP, CLI, docs | `internal/store`, `internal/core`, `internal/api`, `internal/mcpx`, `internal/cli`, `docs/` |
| B. Board server | `internal/board` Go code, `internal/board/engine`, `internal/board/model` |
| C. Clients | `internal/board/web`, `internal/console/static`, their tests |

## 12. Open technical questions

None that block implementation. The numbers in section 4 are first values;
the load test in section 10 may change them.
