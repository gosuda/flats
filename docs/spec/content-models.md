# Content data models: Version and Revision

Status: implemented. The Version model and the Revision model are in place
for every type. Agent guides (`docs/agent/`) describe implemented behavior
only.

## Summary

Flats has two data models for content. Every content type uses exactly one of
them.

| | Version model | Revision model |
|---|---|---|
| For | content an agent prepares and an operator releases | content people and agents edit live |
| Types | websites (`flat`, static or server), pages (`page`) | documents (`docs`), designs (`design`) |
| Working copy | a Private Draft | the live content itself |
| History unit | a numbered version (v1, v2, v3, ...) | a revision of the whole flat per edit unit |
| Needs operator approval | creating or deleting a version, every deployment, rollback | no content change; visibility changes and deleting the flat do |
| Old history | kept until deleted with approval; a full quota refuses new versions | oldest unpinned revisions deleted automatically |
| Managed in | the console (versions, deployment, rollback) | the content itself (its editor) and MCP |

## Terms

- **Draft**: the working copy of a Version-model flat. It is always Private.
- **Version**: an immutable snapshot of a Draft, numbered v1, v2, v3, ... per
  flat. Numbers increase and are never reused.
- **Deployment**: making one version the live content of a flat.
- **Revision**: one entry in the history of a Revision-model flat, created
  automatically by an edit.
- **Pin**: a mark on a revision that keeps it from automatic deletion.
- **Write access**: permission to change a flat's content (defined below for
  the Revision model).

## Version model

Applies to websites (the `flat` type, static and server) and pages.

1. **Draft changes need no approval.** Saving the Draft changes only the
   Private Draft and never the live content. Draft previews stay Private.
2. **Creating a version is its own step and needs explicit approval.** The
   request names the Draft state it freezes (revision and content hash). If the
   Draft changes before the decision, the request fails as stale and nothing is
   created. On approval the Draft is stored as the next number.
3. **Deployment is a separate step and needs explicit approval, including the
   first deployment.** The order is always: create a version, then, when
   wanted, deploy that version. No request creates and deploys in one step,
   and creating a version never deploys it. A version can therefore exist
   without ever being deployed.
4. **Deploy and roll back by version.** Rollback is a deployment of an earlier
   version and needs approval. A server flat's data is a separate axis: data
   restore keeps its own explicit request and approval, as today. A data
   snapshot is taken right before every deployment and rollback, not when a
   version is created. Snapshots belong to the flat, not to a version:
   deleting a version keeps them, and they follow today's retention.
5. **A failed deployment leaves the version in place.** The version stays in
   the list, and the previously live version keeps serving.
6. **Versions are never deleted automatically.** Each flat has a version quota,
   a count of versions with a host default (20) that each flat can override. When it
   is full, a request to create a version is refused and nothing is
   created. The refusal tells the user to delete older versions or raise the
   quota. The check runs when the request is made and again when an approval
   is applied. The live version cannot be deleted.
7. **Published means deployed at least once.** A flat with no successful
   deployment is Unpublished, even if it holds versions, and an Unpublished
   flat cannot become Public. Otherwise visibility (`private`/`public`) stays
   independent, and every change in either direction needs approval.
8. **The console manages versions.** Operators list versions, approve their
   creation, and deploy or roll back there.
9. **Deleting a version needs the operator.** The operator deletes a version
   directly in the console. An agent can only request a deletion through MCP:
   the request returns an approval link for the user, and nothing is deleted
   until the user opens it and explicitly approves. Whether the version can be
   deleted (it is not live) is checked again when the deletion is applied.
   Open previews of that version are closed first, then its files are deleted
   and it stops counting toward the quota.
10. **Tool names follow the model.** A tool that only changes the Draft never
    has *version* in its name: Draft saves go through `save_draft` and
    `save_draft_from_dir`, and the only tool that creates a version is
    `create_version`. `save_version` and `save_version_from_dir` are replaced,
    not kept as aliases.
11. **Approvals stay frozen.** Every approval in this model (create, deploy,
    roll back, delete a version, restore data) keeps today's guarantees: the
    request freezes its target and the state it depends on, such as the live
    version, visibility and provider permissions, and fails as stale if that
    state changed before the decision.

## Revision model

Applies to documents (`docs`), designs (`design`) and any later type that
people and agents edit live.

1. **No Draft, versions or publish.** The content is always live, and the
   console has no version or deployment management for these types. Deleting
   the whole flat still needs approval and removes all of its revisions,
   pinned ones included.
2. **Edits need no approval.** An agent changes the content at once. Private
   content stays Private. On Public content every change is visible to everyone
   as soon as it is saved. Visibility changes still need approval in both
   directions. Such an approval covers the flat, not one revision: content
   changes while it is pending do not make it stale, and approving it exposes
   the content as it is when the change applies.
3. **Live concurrent editing.** People and agents can edit the same content at
   the same time.
4. **Every edit unit creates a revision.**
   - One agent write call (a save or a live edit through MCP) is one revision.
   - A person's live edits are grouped per editor. A group closes, and becomes
     one revision, when that editor has been idle for 30 seconds or 5 minutes
     have passed since the group began.
   - On a board or document that anyone can edit, all anonymous guests share
     one group, which closes only at a fixed 5-minute window or a forced
     boundary ([board-public-edit.md](board-public-edit.md) rule 25,
     [docs-public-edit.md](docs-public-edit.md) rule 25).
   - A rollback is one revision.
   - Before an agent write, a rollback or an overwrite applies, the open
     groups close: the current state is kept as one revision that credits
     every person who edited since the previous revision, so the state just
     before it is always kept.
5. **A revision covers the whole flat.** It records every document or file of
   the flat at that point, so files that belong together (a design's index and
   its artboards, several documents) stay consistent. Assets that a retained
   revision references stay available as long as that revision exists.
   - Storage is shared: each file's content is stored once and referenced by
     every revision that holds it, so a revision costs only the files it
     changes plus metadata. Stored content is deleted only when no retained
     revision references it.
6. **Two separate quotas.**
   - A *revision quota* is the maximum number of unpinned revisions kept. When
     it is exceeded, the oldest unpinned revisions are deleted automatically.
     On boards and documents, revisions credited only to anonymous guests are
     limited to half of the quota and deleted first above that, including
     after guest editing stops ([board-public-edit.md](board-public-edit.md)
     and [docs-public-edit.md](docs-public-edit.md) rule 25).
   - A *pin quota* is the maximum number of pinned revisions. Pinned revisions
     do not count toward the revision quota and are never deleted
     automatically.
   - Pinning when the pin quota is full is refused; Flats never unpins a
     revision by itself to make room.
   - An unpinned revision counts toward the revision quota again from then on.
   - Both quotas count revisions only, with no byte limit, and have a host
     default that each flat can override: 100 revisions and 20 pins.
7. **Overwritten content is kept as a revision.** When a save replaces live
   content that people had changed, the content before the overwrite is a
   revision like any other. There is no separate store for overwritten text.
8. **Documents are written with document tools.** `save_document` writes one
   document or a whole docs bundle with several documents and assets;
   `update_document` edits live text. Saving one document changes only that
   document and leaves the flat's other documents and assets as they are. A
   save that replaces the whole bundle says so explicitly, and files it omits
   are deleted.
   - Saves keep today's merge behavior: people's concurrent edits that do not
     overlap the save are kept, and where they overlap, the save wins. The
     overwritten text stays in the revision closed just before the save.
   - Today's merge bounds also stay: when a document is too large to merge
     (for example more than 16,000 lines combined), the save replaces it
     whole, and people's edits remain recoverable from the revision closed
     just before it.
   - `update_document` keeps its guards: if any guarded text changed since
     the agent read it, the whole call is refused and nothing changes. Revision-model content is never written
   through a Version-model tool.
9. **Rollback creates a new revision.** Rolling back to revision R creates a
   new revision whose content equals R's whole flat. History is never
   rewritten, and the rollback itself can be rolled back. The rollback wins
   over concurrent edits: edits that arrived before it are kept in the
   revision closed just before it, and the result is exactly R's content.
   Edits that arrive after it apply on top of R's content; an edit to a
   document the rollback removed is discarded.
   - A rollback adds only metadata: its revision references R's stored
     content, and the edit group it closes first holds live content that was
     already admitted. A rollback is therefore never refused for disk quota.
   - A rollback resets every document's merge base to the restored content, so
     a later save merges against what the rollback restored.
   - Retention never removes a rollback's target before the rollback's new
     revision references it: automatic deletion runs only after the rollback
     completes. This protection is not a pin and uses no pin quota.
10. **Revisions are managed inside the content.** Listing, viewing, pinning,
    unpinning and rolling back happen in the content's own editor, and through
    MCP for agents, not in the console.
11. **Only writers manage revisions.** Every revision operation needs write
    access to the content.
    - Today write access means the Private route and MCP. The Private route is
      Local loopback or a private mesh provider (Tailscale or NetBird), as that
      network's existing access control allows. A Public route shows no
      revision list and accepts no revision operation. It is read-only,
      except that boards and documents whose operator allowed anyone to edit
      accept guests' element or text edits there
      ([board-public-edit.md](board-public-edit.md),
      [docs-public-edit.md](docs-public-edit.md)).
    - Agents may pin, unpin and roll back without approval.

## Quotas

These rules apply to the version, revision and pin quotas alike.

1. **A quota is a count.** The version, revision and pin quotas have no byte
   limit.
2. **0 means unlimited.**
3. **The disk quota stays.** The existing per-flat disk quota
   (`system.disk_quota_bytes`) is separate and unchanged. A save, a version
   creation or a person's edit that would exceed it is refused, and no history
   is deleted automatically to make room. A person's edit is checked when it
   arrives, including the space its revision will need when its group closes,
   and is refused before it becomes live if that would not fit. A revision
   rollback adds only metadata and is never refused for disk quota.
4. **Lowering a quota below what is held.** Unpinned revisions above the new
   revision quota are deleted at once, oldest first (on boards and
   documents, with the guest-only share of rule 6 applied first). Versions and pins are
   never deleted: they all stay, and new ones are refused until the count is
   below the quota.

## Model per content type

| Type | Model | Status |
|---|---|---|
| website (`flat`) | Version | implemented: separate creation/deployment approvals, quota and explicit deletion |
| page (`page`) | Version | as for websites |
| document (`docs`) | Revision | implemented |
| design | Revision | implemented |

A flat keeps its first save's type, so its model never changes.

The core already uses the word *revision* for Draft saves (`draft-revs`,
`expected_revision`, "Draft revision and hash" in approvals). That counter
belongs to the Version model's Draft and is not a Revision in this document's
sense: it has no pins, quotas or rollback.

## Upgrading older hosts

1. **Recovery records of documents.** A record of overwritten text in
   `flats_docs_conflicts` holds one document's text, not the whole flat, so
   each record becomes a synthetic recovery revision, oldest first: the
   flat's current content with only that document replaced by the preserved
   text. These revisions are marked as recovered, not as historical
   snapshots, and follow the revision quota like any other. Then one
   revision holds the flat's current content, so it is the latest. The old
   store is no longer written.
2. **Versions keep their numbers.** Existing versions keep their numbers, and
   new numbers continue after the highest one.
3. **Nothing is deleted to fit the quota.** A flat that already holds more
   versions than its quota keeps them all; creating a version is refused until
   the user deletes enough versions or raises the quota.
4. **Pruned versions do not count.** A version whose files an older release
   already pruned stays only as a record. It does not count toward the
   version quota and cannot be deployed.
5. **The old retention count becomes the version quota.** See
   [moving an existing installation](../configuration.md#moving-an-existing-installation)
   for how `system.keep_versions` becomes `system.version_quota`.

## Open questions

None at the moment. Record new ones here as they come up.
