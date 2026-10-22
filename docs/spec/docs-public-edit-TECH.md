# Anonymous public editing for documents: technical design

Status: design for [docs-public-edit.md](docs-public-edit.md), including the
board changes of its rules 8, 11 and 15. Rule numbers refer to that page;
"board rule N" refers to [board-public-edit.md](board-public-edit.md) and
"board TECH N" to [board-public-edit-TECH.md](board-public-edit-TECH.md).
The document app internals are in [docs-app.md](../internal/docs-app.md).

## 1. Decision summary

| Topic | Decision |
|---|---|
| Stored level | The board's `flats.public_access`, `access_seq` and `content_state.safe_point_rev`, now allowed for type `docs` as well as `board` |
| Approval, quotas, preflight | The board's code paths, with the type checks widened to documents and the messages worded per type |
| Telling the worker | Core writes the level and the epoch (`flats.access_seq`) into the worker database through a new `access` host operation, on every level change of a running worker and on every wake. Each public request also carries both as trusted headers. Guest power needs both to say `edit` with the same epoch |
| Epoch at commit | Every guest update transaction rereads the stored epoch and refuses on a mismatch, so a level drop takes effect in whichever VM commits next. A coalescing runtime signal, reconciled on every heartbeat, then closes guest sockets and tells viewers |
| Access epoch on the wire | `flats.access_seq` is sent as `access_epoch` in welcomes and `level` frames of both types, so clients can tell a new `edit` period from a reconnect |
| Connections | Three kinds: member (Private route), guest (Public, guest-capable, valid `guest` hello), viewer (Public otherwise). Public connections have their own slots and send budget in the runtime and in the worker, so a full Public side never takes member capacity |
| Guest identity | Server-issued token, key `guest:` + hash, as on boards. A guest may insert only with Yjs client ids that are new to the document or already its own; guest-owned ids are stored, so authorship survives reloads and existing text can never become guest-written |
| Projection | Presence and welcome are built per audience. Guests see members under their page nickname (`hello.nick`), never a Tailscale identity |
| Current text only | The initial sync is already a diff of the garbage-collected server document. Pending structs are dropped when a document loads and refused in new updates, so no hidden text can ride along |
| Guest pacing | The client merges and paces guest updates below the per-guest limit. When all guests together reach the flat's limit, the server answers `busy` and the client resends in order later, losing nothing. A refusal stops the page (Download local text, Rejoin) |
| Diagrams | Size bounds measured per diagram type, and after any slow render the page renders further diagrams only on request |
| Revisions | One fixed-window guest group per flat; outbox entries carry guest names; core marks guest-only revisions and uses the board's retention |
| Authorship | Per document, the line ranges whose current text a guest inserted, computed from Yjs item client ids; stored with each revision entry |
| Board changes | Popup with View only and Join to edit replaces the bottom panel; members send a nickname and guests see it instead of Member N |

## 2. Core, store, API, MCP

### 2.1 Type gates

Every board-only gate listed below accepts `contenttype.Docs` too. A helper
`publicEditType(t) bool` (board or docs) replaces the inline checks.

| Place | Change |
|---|---|
| `store.CommitVisibility` / `isBoardTx` | `editableTx` reads the type from `content_state`, falling back to the flat's manifest type when the row does not exist yet (a document gets its row on first import) |
| `core.requestVisibility`, `applyVisibilityApproval` | accept documents |
| `service.view()` | report `public_access` and `public_presence` for Public documents |
| `withPublicAccess` | set the header for a document flat (by manifest type), not only for `*board.Instance` |
| `markSafePoint`, `GetRevision` | mark the safe point for documents |
| `downgradeEditBoards` → `downgradeEdit` | accept documents; the type comes from the manifest when `content_state` is missing |
| `PublicEditPreflight`, `publicEditStatus` | documents use the docs catch-up and the reuse check of 2.3 |
| Notices, MCP texts, `PinRevision` and `SetRevisionQuotas` refusals | worded per type: "this board" / "these documents" |

The store's invariant (`edit` only for a Public board) becomes "only for a
Public board or document". The comment at `internal/store/publicaccess.go:14`
changes with it.

### 2.2 The `publicEdit` target

`publicBoard` (`internal/core/publicedit.go`) becomes an interface that both
types implement:

```go
type publicEditTarget interface {
	SafePoint(ctx context.Context, prev []RevisionFile) (seq int64, reuse bool, err error)
	SetPublicAccess(ctx context.Context, level string, epoch int64) error
	PublicPresence() (guests, viewers int, ok bool)
}
```

The board adapter wraps `*board.Instance` as today; the board keeps its
in-memory epoch for commits and now also sends the stored epoch to clients
(5.1). The docs adapter calls host
operations (3.6) through `docsHost`.

### 2.3 Applying `edit` to a document

`applyVisibilityApproval` with a target of `(public, edit)` on a document runs
under `publicEditMu` and the storage lock (`lockStorageCtx`, not the plain flat
lock, because docs host operations need the storage journal check):

1. Preflight as for boards (`editQuotaProblems`, a free pin or a reusable safe
   point).
2. Wake the worker with `acquireDocsLocked` (a sleeping document is normal; the
   board's "not running" refusal does not apply), then `importDocsLocked`.
3. Safe point: the file list F of `safe_point_rev` when that revision exists;
   host operation `safe_point` with F (3.6). On `reuse`, `pinLocked` first,
   then import. Otherwise import with `pinSeq` set to the returned outbox seq:
   `commitDocsEntries` pins that revision and sets `content_state.safe_point_rev`
   in the same transaction that inserts it, as `commitBoardEntries` does. A
   full pin quota imports the entry unpinned and fails the approval with
   `pin_quota_full`.
4. Commit `(public, edit)` as for boards (Public exposure steps when it was
   Private, then `CommitVisibility`, which bumps `access_seq`).
5. Host operation `access` with `{level: "edit", epoch: access_seq}` while the
   lease from step 2 is held.

Leaving `edit` commits first, then calls `access` with the new level through
`withRunningDocs`, which does not wake a sleeping worker. When the worker is
asleep, or the call fails, the commit stands and the next wake applies the
stored level (2.4). A failed call on a running worker for a level other than
`edit` restarts the instance, as the board path does.

### 2.4 Waking

`prepareDocsLocked` (every wake, including a lazy first start) calls `access`
with the stored level and `access_seq` next to the budget installation, before
any request reaches the worker.

### 2.5 Request headers

`withPublicAccess` sets, on Public requests of a document flat, the board's
`X-Flats-Public-Access` (`view` or `edit`) and a new `X-Flats-Access-Epoch`
(`access_seq`). `trustedAccess` strips client spellings of both. Private
requests carry neither.

### 2.6 Import

- `docsEntry` gains `guests` (up to 5 names), `guests_more` and, per document,
  `guest_lines` (2.8). Kind `safe_point` is new for documents.
- `commitDocsEntries` sets `GuestAuthors`, `GuestsMore` and
  `GuestOnly = len(authors) == 0 && (len(guests) > 0 || guests_more > 0)` as
  the board import does, and takes the optional `pinSeq` of 2.3.
- Retention keeps running after the import (`retainLocked`); `retainTx` is
  already type-agnostic and already retires guest-only revisions first above
  half the quota.

### 2.7 Presence counts

The worker keeps `meta.presence = {guests, viewers, at}` (3.9). The `sync`
response returns it, and `refreshDocsLocked` caches it in the flat's live state
with its time. `view()` reports it as `public_presence` while it is younger than
30 seconds and the worker is awake, and 0/0 for a sleeping worker. `view()`
never wakes a worker.

### 2.8 Guest marks for agents

- `GET /_docs/api/document` (host reads) gains `guest_text` (bool) and
  `guest_lines` (sorted, merged 1-based inclusive ranges `[[start, end], ...]`).
  `core.Document` and MCP `get_document` return them.
- Each outbox entry's `documents[]` items gain `guest_lines`. Migration 20 adds
  `revision_guest_lines(flat_id, revision, path, ranges TEXT)`, written in the
  import transaction for documents with a non-empty range list, deleted with
  its revision. `get_revision` returns `guest_lines` for each document file.
- `docs/agent/topics/docs.md` states rule 28 and the two fields.

### 2.9 MCP, API, CLI

- `set_visibility` `access: edit` is accepted for boards and documents; the
  descriptions and the hint say so.
- `FlatInfo.public_access` and `public_presence` describe both types.
- `GET /flats/{slug}/public-edit` works for documents (preflight).
- `flats visibility <slug> public --access edit` needs no change beyond the
  core gate.
- Generated references are rebuilt with `make docs-generate`.

## 3. Document worker (`internal/apps/docs/_web/src`)

### 3.1 Stored level and epoch

- `meta.access = {level, epoch}` written by the `access` host operation in one
  transaction. Missing means `""` at epoch 0.
- A Public WebSocket is **guest-capable** only when the request headers say
  `edit` with epoch E and `meta.access` is `{edit, E}` at admission.
- The epoch is `flats.access_seq`: stored in flats.db, incremented by every
  commit of visibility or level, and never reset by worker restarts or sleep.
- Every guest `update` transaction reads `meta.access` first and refuses with
  `editing_ended` unless it is still `{edit, E}` for the connection's E. This
  check runs in the transaction that would commit, in whichever VM handles it,
  so an update in flight when the level drops never commits.

### 3.2 The `access` signal

After writing `meta.access`, the request VM raises a runtime signal for the
WebSocket VM. The runtime gains one built-in-docs entry point next to `wake`:
`__flats_docsSignal("access")` (`internal/runtime/ws.go`, `prelude.js`),
dispatched to `websocket.signal(name, env)`. Unlike `wake`, which drops an
event when the queue is full, the signal is **coalescing and never lost**: it
sets a pending flag in the hub and tries a non-blocking send; the WebSocket
loop checks the flag after every event it handles and when the send
succeeds, so a set flag is always handled once the loop runs again.

On `access` the WebSocket VM rereads `meta.access` and:

- closes every guest socket whose epoch differs with
  `{t: "error", code: "editing_ended"}` and close `1008 editing_ended`;
- sends `{t: "level", access, access_epoch}` to every viewer socket.

Reconciliation: every client sends `ping` every 15 seconds today (members,
guests and viewers). On each `ping` from a Public socket the VM compares the
socket's known level and epoch with `meta.access` and applies the same two
actions when they differ.

**Completion boundary.** Stop guest editing is complete when the commit and
the `access` operation have returned (or, for a sleeping worker, the commit
alone). From then on no guest update commits (3.1). Guest sockets close when
the WebSocket loop next runs, and at the latest at their next heartbeat
(15 seconds). The console's confirmation states the first, not the closing.

### 3.3 Classification and admission

| Condition | Kind |
|---|---|
| `x-flats-access: private` | member |
| Public, guest-capable, hello has a valid `guest` object | guest |
| Public otherwise | viewer |

- `hello` gains `guest: {name, token?}`, `viewer_token?` and `nick?`.
- Guest token: 16 random bytes issued in `welcome.guest_token` when absent;
  the key is `guest:` + the first 16 hex digits of SHA-256(token). Kept in
  `sessionStorage` (`flats-docs-guest-token`).
- Guest name: `cleanGuestName` (trim, remove Cc and Cf characters, 1 to 40
  code points). An invalid name gets fatal `name_invalid`. The same function is
  used for nicknames.
- Viewer number: `viewer_token` (server-issued, `sessionStorage`
  `flats-docs-viewer-token`), `token → N` in WebSocket VM memory, a released N
  held for 60 seconds, N the lowest free number. One numbering for the whole
  flat.
- Pools. The runtime's `admit` decides before the handshake from the trusted
  `X-Flats-Access` header:

| Layer | Member (Private) | Public (guests and viewers) |
|---|---|---|
| Runtime slots (`MaxWSConnections`, includes handshakes and closing sockets) | 256, as today | a separate 80 |
| Runtime queued bytes per flat | 16 MiB, as today | a separate 4 MiB |
| Worker connections | 200, 50 per document, 8 documents, as today | guests 30 (20 per document, 30 new per minute); viewers 40 (10 per document, 4 documents), as today's reader pool |
| Worker queued bytes (`flatSendBytes`) | 16 MiB, as today | a separate 4 MiB |

  Public connections therefore never take a member slot or member send
  budget. A guest over its pool gets fatal `full` and the client offers Read
  (viewer) or Try again. A viewer over its pool is closed `1008 capacity` as
  today, and the client shows "Too many readers right now". A Public request
  refused by the runtime's Public slots gets HTTP 503 and the same message.
- The runtime's event queue stays shared. Guest input is bounded by the flat
  limits of 3.8 before it reaches the queue's consumer, so members' events
  wait at most for that bounded work.
- Member welcome is unchanged except for `nick` (3.5) and the `viewers` frames
  (3.4).

### 3.4 Welcome and projection

- `welcome` gains `access` (the level: `view`/`edit`, absent on Private),
  `access_epoch` and `me`:
  - member: as today (`you`), plus nothing new;
  - guest: `me: {kind: "guest", name}` and `guest_token`; `you.verified` is
    always false;
  - viewer: `me: {kind: "viewer", label: "Viewer N"}` and `viewer_token`.
- Presence (`aw`) is built per audience. The member frame is today's, with
  `guest: true` on guest states. The guest frame replaces each member's
  `user.name` with that member's nickname and sets `verified: false`; guests
  keep their typed name. The rewrite runs once per audience per state change.
- Nickname: `hello.nick`, cleaned with `cleanGuestName`. The docs client always
  sends its stored collaborator name (the random two-word name, or a typed
  one). A member with no valid nick appears to guests as "Member".
- Viewers receive no presence. Members and guests receive
  `{t: "viewers", n: [1, 2, ...]}` for the viewers of their room (or `{count}`
  above 20), at most once per second per room and only when the set changed.
- A test serializes every frame sent to a guest socket while a Tailscale
  member edits and fails on the member's `tailscale-user-login`, its
  `tailscale-user-name`, its `browser:` editor id or an agent label.

### 3.5 Yjs client ownership

- `validateUpdate` already decodes the structs; the update path now reads the
  client ids of all inserted structs.
- New table `flats_docs_guest_clients(doc TEXT, client INTEGER, owner TEXT,
  PRIMARY KEY(doc, client))`, holding only guest-owned ids.
- A guest update may insert with client id C in document D only when C is
  already owned by this guest key in D, or when C is in neither the table nor
  the server document's struct store (`doc.store.clients`). In the second case
  C becomes owned by the guest in the same transaction. Anything else is
  refused with `guest_client` (fatal, 3.8). Every other writer (members, seeds,
  activations, agent edits, restores) already exists in the document's store
  once committed, so a guest can never extend an existing client, and text
  that was not inserted by a guest can never become guest-written. Existing
  databases need no initialization: the struct store is the source of truth
  for "not a guest's".
- A member update that inserts with a guest-owned id is refused with
  `invalid_update` (a member's page never reuses a guest's random id; this
  keeps attribution exact).
- The awareness id of a guest socket follows the same rule.
- Guest-owned rows are capped at 5,000 per flat (3.8).
- Restoring a document (`resetDocument`) deletes its rows.

### 3.6 Host operations

New operations under `/_docs/host/revisions/` (private plus
`X-Flats-Host-Op: revisions`, as the existing ones):

| Operation | Body | Does | Returns |
|---|---|---|---|
| `access` | `{level, epoch}` | write `meta.access`, post the `access` signal; at a level other than `edit`, close the guest group (a `person` entry when dirty) | `{level, epoch}` |
| `safe_point` | `{prev: {source, documents: [{path, hash}]}}` | in one transaction: flat activation gate, record dirty groups as `person`, delete all groups, compare the current shape with `prev`; equal → reuse; else record a forced `safe_point` entry | `{seq, reuse}` |

`sync` also returns `presence` (2.7).

### 3.7 Current text only

- The initial sync and reconnects already send
  `Y.encodeStateAsUpdate(doc, sv)` of the server document, which is garbage
  collected (`new Y.Doc()`), and checkpoints store the same encoding. Raw log
  rows only reach sockets that were already joined when they were committed.
- New check for every update (members included): after `Y.applyUpdate`, the
  document must have no `pendingStructs` and no `pendingDs`; otherwise
  `invalid_update`. Pending structs would otherwise be encoded into welcomes
  and checkpoints without ever being visible.
- Existing data: today such updates are accepted, so logs and checkpoints may
  already hold pending state. When a document is loaded (`load`, catch-up after
  a reset), the server drops `pendingStructs` and `pendingDs` from the loaded
  `Y.Doc` before it is used. Pending state is never visible, so no visible
  text changes. The next compaction writes a checkpoint without it; until
  then every welcome is encoded from the cleaned document. A log line records
  the drop. Tests load fixtures holding pending structs and a pending delete
  set, check that visible text is unchanged, that welcomes and checkpoints
  carry no pending bytes, and that later edits commit.
- A test inserts and deletes marked text as a member, then joins a guest and a
  viewer (fresh and with an old state vector) and scans every byte they
  receive.

### 3.8 Guest limits and refusals

| Limit | Value | Where |
|---|---|---|
| Update frames | 8/s, burst 16 per guest socket | before the transaction |
| Update size | 64 KiB decoded | before the transaction |
| Presence | 10/s per guest socket; excess is dropped, not refused | `presence()` |
| All guests of the flat | 200 updates/s and 2 MiB/s read (refused frames counted); over it the update gets `busy` (below) | before the transaction |
| Guest presence relayed, all guests | 100 frames/s; excess dropped | `presence()` |
| Disk | refuse when the disk check's total is above 90% of the quota | `checkDiskQuota`, guests only |
| Guest byte budget | 256 MiB per flat, `meta.guest_charge` (below) | after the disk check |
| Document size | refuse when the update would take the document above 90% of an enforced limit: `LIMITS.text` (1 MiB), `LIMITS.state` (1.5 MiB) or `LIMITS.structs` (20,000, checked by `validateDocument`) | after applying |
| Lifetime capacity | guests may not take a document's `seq` above `LIMITS.receipts − 20,000`, nor the flat's receipts above `totalReceipts − 40,000` | `append` |
| Client ownership rows | 5,000 guest-owned per flat | 3.5 |

- Guest updates do not use the room's shared update bucket (`rateUpdate`),
  which stays for members only, so guests cannot use up members' rate.
- **`busy`.** When the flat's guest bucket is empty the server sends
  `{t: "busy", id}` instead of applying, marks the socket busy and answers
  every later update from it with `busy` until one arrives with `retry: true`.
  The client stops sending, waits one second, and resends every
  unacknowledged update in its original order, the first with `retry: true`.
  Already committed ids are acknowledged as duplicates, so nothing is applied
  twice or out of order. With 30 guests at the client pace (4 updates/s,
  4.1) the flat sees at most 120 updates/s, so `busy` appears only under a
  flood.
- Each worker has its own Public send budget (3.3), so one flat's guests and
  viewers cannot use another flat's or the members' send queues. This is how
  rule 25a's guest share of the host's Public traffic is met for documents.
- Guest byte budget: `meta.guest_charge` grows by the growth of the disk
  check's measured part (`page_count × page_size` plus `unimported`) across each
  committed guest update; when a budget installation reports a lower measured
  total than the previous one by d, the charge falls by d (not below 0).
- Every guest refusal is fatal for that socket: `{t: "error", code}` then close
  `1008 code`, with codes `guest_rate`, `guest_size`, `guest_room`,
  `guest_client`, `capacity` or `disk_quota`. The client stops editing
  (rule 22). The member path is unchanged.
- Guest connections do not count toward the member pool or the member send
  budget.

### 3.9 Groups, outbox and presence state

- Migration in `storage.js` (`PRAGMA table_info` then `ALTER TABLE`):
  `flats_docs_groups.names TEXT` and `flats_docs_groups.fixed INTEGER`,
  `flats_docs_outbox.guests TEXT` and `flats_docs_outbox.guests_more INTEGER`.
- A guest update touches the group `editor = "guests"`, `fixed = 1`, adding the
  guest name to `names` (first-edit order, at most 64; more are counted).
- `sync` closes a fixed group only when `started_at` is 5 minutes old; it is
  never closed for idleness.
- **Who triggers a record** (as board TECH 5.1). Today `sync` records when any
  group is due and the text is dirty. That changes to: record only when at
  least one **due** group is **dirty**; a due clean group is deleted without
  an entry. A member group is due after 30 s idle or 5 minutes; the guest
  group only at its window end. Recording clears `dirty` on every group but
  keeps the guest group's `started_at`. So an entry recorded because a member
  group closed credits that member (it is dirty by definition) and the guests
  since the last entry, and a guest-only entry is recorded only at a guest
  window end or a forced boundary. A test interleaves member groups that go
  clean after a guest-window entry and expire later while guests keep
  editing.
- Authors: dirty member groups' labels as today. Guests: the first 5 names of
  dirty fixed groups in `guests`, the rest counted in `guests_more`.
- Forced boundaries (agent edit, rollback, activation, safe point, `access`
  leaving `edit`) close the guest group as they close every group.
- Each entry's documents carry `guest_lines` computed from the recorded text's
  Yjs items (2.8): walk the `Y.Text` items, collect the visible characters of
  items whose client id is guest-owned, convert offsets to line ranges.
- `meta.presence` is written by the WebSocket VM when the guest or viewer count
  changes, at most once per second.

### 3.10 Rendering bound

Mermaid renders on the page's main thread, so a hard time limit cannot
interrupt one render. The bound has two parts:

- **Size bounds per diagram type.** Mermaid gets `maxTextSize: 20000` and
  `maxEdges: 500`, plus per-type caps the client checks before calling Mermaid
  (lines of source and, for flowcharts, nodes). A diagram above a cap renders
  as its source with a note. The testing stage renders generated worst cases
  for every diagram type Mermaid supports (deep nesting, long labels, many
  edges, cycles) in a real browser and sets each cap so the slowest stays
  under one second; the measured table goes into the test report.
- **Stop after a slow render.** The page times each render. After any render
  takes longer than 500 ms, the page renders further diagrams of that page
  load only when the reader clicks **Show diagram**. One hostile diagram can
  therefore cost at most one bounded render per page load.

Spec rule 21a is reworded to these two guarantees.

## 4. Document client

- **Popup** (rule 8 and 11): a dialog over the live document, name field with
  the 40-character counter and badge preview, **View only** and **Join and
  edit**. It opens on arrival at level `edit`, from **Join to edit**, and on a
  `level: edit` frame for a viewer without a guest session unless the tab has
  chosen View only since the level last became `edit`
  (`sessionStorage` `flats-docs-view-only` holds the epoch the choice was made
  at; a newer `access_epoch` clears it). A remembered name
  (`localStorage` `flats-docs-guest-name`) only fills the field.
- **Header:** viewer "You are Viewer N" and **Join to edit**; guest "You are
  Mina" with the badge and **Change name** (a dialog; saving reconnects with
  the new name). Members unchanged.
- **People list:** guests with the badge; viewers as "Viewer N" chips (or a
  count above 20).
- **Guest editor:** today's private editor (Edit/View, no History). Pending
  Yjs updates are merged (`Y.mergeUpdates`) and sent at most 4 frames per
  second, below the server's 8/s. Consecutive updates are merged only while
  the result stays within 64 KiB; a single update above it (a very large
  paste) is refused by the server and stops editing (rule 22). The status shows "Saving…" until every id is acknowledged.
  `busy` handling as in 3.8.
- **Refusals:** a fatal guest code stops editing with the notice "…, so editing
  stopped", **Download local text** and **Rejoin**. Rejoin sets
  `sessionStorage` `flats-docs-rejoin` and reloads; the page then joins with
  the remembered name without the popup.
- **Editing ended:** notice "The owner turned off guest editing. N of your
  latest changes weren't saved. You can keep reading live." with Download local
  text; the page reconnects as a viewer.
- **Full:** fatal `full` → "This document is full" with Read and Try again; a
  viewer closed with `capacity` → "Too many readers right now" with Try again.
- **History panel:** guest credits with the badge per name and "and N more
  guests"; kind `safe_point` as "Safe point before guest editing".
- Members send `hello.nick` = their stored collaborator name.

## 5. Board changes

### 5.1 Server (`internal/board`)

- `SetPublicAccess(level, epoch)` and `Options.AccessEpoch` carry
  `flats.access_seq`; the welcome and the `level` frame send it as
  `access_epoch`.
- `helloMsg` gains `Nick`. `welcome` stores `c.nick = model.CleanGuestName(h.Nick)`
  for members (both branches); a Tailscale member's display name is never used
  as a nickname.
- `conn.presFrames`: the guest frame uses `c.nick`, falling back to
  `"Member " + N` when empty. `key` stays `p:N`, `verified` stays false.
- Tests: `TestGuestFrameScan` and `TestGuestIdentityEveryFrame` keep the
  Tailscale display name and login forbidden, give each member a distinct
  nickname, and assert that guests see the nickname; a new test covers a
  Tailscale member without a nickname (Member N).

### 5.2 Client (`internal/board/web`)

- Every page sends `hello.nick`: the typed name when there is one, else a
  generated two-word name kept in `localStorage` `flats-board-nick`.
- `join` no longer starts as `!!guestName`; a remembered name only fills the
  popup. A live guest session (`sessionStorage` guest token present and a
  join already chosen in this tab) rejoins without the popup.
- `guestPrompt` gains **View only** (not in Change name mode). Closing or View
  only sets the tab's view-only epoch (as 4). `offerJoin` always uses the
  popup; `showJoinPanel` and its CSS are removed.
- `meLabel` shows **Join to edit** for a viewer at level `edit`.
- `onLevel` opens the popup when the level becomes `edit` and the tab's
  view-only choice was made at an older `access_epoch`
  (`sessionStorage` `flats-board-view-only`).
- Pure helpers for these decisions go in `guest.js` with node tests.

## 6. Console (`internal/console/static`)

- `accessOf`, the Share dialog choices, `drawInternet`, the Sharing row and
  safe point slot, `allowGuestEditing`, `blockedDialog`, `stopGuestEditing`
  and the approval page accept documents, with nouns from one table:
  board → "the board", "on the board", "this board"; docs → "the documents",
  "on the documents", "these documents".
- The guest note per type: board as today; documents "They can't restore
  earlier text or add files."
- "People on your networks" reads "Can edit" for boards and documents.
- Console UI tests cover both types.

## 7. Documentation

- `docs/internal/docs-app.md`: wire protocol, collaboration model, revision
  boundaries, disk quota, history panel (spec "Changes to existing specs").
- `docs/agent/topics/docs.md` and `approvals.md`; `make docs-generate`.
- `content-models.md` rules 4, 6, 11 and Quotas 4.
- `board-public-edit.md` current state and `board-public-edit-TECH.md` 3.3 and
  8.1 for the board changes; the TECH summary row about `member:N` is
  corrected to the `p:N` labels the code uses.
- The specs' status lines and "Current state" sections.

## 8. Verification plan

| Spec criterion | Evidence |
|---|---|
| Approval | core tests for documents: `edit` only through approval or console self-approval; stale after a level change, including a round trip; preflight problems fail without changing visibility; a sleeping document is woken for the safe point; legacy approvals apply as `view` |
| Name rules, Joining | worker tests: guest update refused before a valid name; invalid names refused; Viewer N kept across a reconnect within 60 s and across documents. Client node tests of the popup decisions: arrival, remembered name, View only, Join to edit, a `level` frame with a newer `access_epoch`, a reconnect with the same epoch, a missed view→edit→view→edit cycle, navigation between documents. Browser check of both types |
| Boundary | worker and core tests: history routes, block outline, host operations and earlier text refused at both levels; a `view` socket update and a forged Private-only request from a guest change nothing; forged `X-Flats-Public-Access`/`X-Flats-Access-Epoch` from a client are stripped |
| Current text only | the scan test of 3.7 with fresh and old state vectors, the pending-struct refusal, and the upgrade fixtures with existing pending state |
| Identity | the frame scans of 3.4 (documents) and 5.1 (boards), with a Tailscale member present |
| Client ownership | worker tests: a guest extending a member's, seed's or agent's client id, or another guest's, is refused; attribution of existing text never changes |
| Rendering | browser test with script, raw HTML, `javascript:` links, an outside image and generated worst-case diagrams of every supported type at the caps; the measured table; the stop-after-slow switch |
| Limits | worker tests per limit in 3.8, including `busy` resend order. Load test: 20 guests at the per-guest limit plus flood clients at the flat limit, with Public pools full, while a member connects, edits, rolls back and saves; measured: member admission succeeds, delivery of the member's update to another member's socket p95 within the document's existing target, rollback completes |
| Refusal | worker and client tests: refused A, dependent B never committed, download contains both, Rejoin works; pacing loses nothing at 50 keystrokes per second for 30 seconds |
| Lifetime | worker test with lowered limits: guests stop at their share, a member update still commits |
| Revisions | worker+core test over a simulated hour (fake clock): guests continuously and in pauses, a member editing with 31-second pauses across two documents; at most 12 guest-only entries plus forced boundaries; member closures credit the member. Retention: guest-only at most half (Q = 1, 2, 4, 100). Safe point: reused on an unchanged re-enable, refused unpin while `edit`, pinned in the import transaction under retention pressure |
| Kill switch | core integration tests: console Stop → commit → `access` → guest sockets closed and in-flight updates refused; the same with the worker asleep (stored level applies on wake), with the `access` call failing (instance restarts), with the signal queue full (reconciled at the next heartbeat), and with a quota change racing an `edit` approval |
| Recovery | integration test: guests delete all text of every document, rollback to the safe point and to the revision before |
| Agent safety | `get_document` and `get_revision` return `guest_lines` after a member edits around guest text |
| Consistency | console UI tests for both types; real browser check on a disposable development host |

## 8a. Rule coverage

| Spec rules | Inherited (board code, widened) | New for documents | Evidence (section 8 row) |
|---|---|---|---|
| 1–7, 6a | stored level, approval, staleness, safe point, quota rules (2.1–2.3) | docs safe point and wake path (2.3, 2.4, 3.6) | Approval, Revisions, Kill switch |
| 8–11 | — | popup, View only, Join to edit, `access_epoch` (3.3, 3.4, 4, 5.2) | Name rules, Joining |
| 12–15, 13a | label rules | projection and nicknames (3.4, 5.1) | Identity |
| 16, 16a | guest key | client ownership, current text only (3.5, 3.7) | Client ownership, Current text only |
| 17–21a | — | guest boundary, rendering (3.3, 3.10, 4) | Boundary, Rendering |
| 22–26, 25a | retention (2.6) | limits, `busy`, pools, groups (3.3, 3.8, 3.9) | Limits, Refusal, Lifetime, Revisions |
| 27–28 | — | guest lines (2.8, 3.9) | Agent safety |
| 29–31 | commit order | epoch at commit, signal, reconciliation (3.1, 3.2) | Kill switch, Recovery |
| 32–36 | console components | per-type nouns (6) | Consistency |
| Board 8, 11, 15 | — | 5.1, 5.2 | Joining, Identity |

## 9. Implementation units

Four units with disjoint files, joined by the interfaces of 2.2 to 2.8, 3.1 to
3.6 and the wire frames of 3.3 and 3.4:

| Unit | Files |
|---|---|
| A. Core | `internal/store`, `internal/core`, `internal/api`, `internal/mcpx`, `internal/cli`, `internal/runtime` (the signal), `docs/` |
| B. Docs worker | `internal/apps/docs/_web/src/{server,storage,revisions,protocol}.js` and their tests, `internal/apps/docs/*_test.go` |
| C. Docs client | `internal/apps/docs/_web/src/{client,provider,history,history-format,live-view,diagram-runtime}.js`, `client.css`, new client modules and tests |
| D. Board and console | `internal/board` (Go and web), `internal/console/static` and their tests |

`internal/apps/docs/dist` is rebuilt once at integration, after B and C merge.

## 10. Open technical questions

None that block implementation. The numbers in 3.3 and 3.8 are first values;
the load test may change them.
