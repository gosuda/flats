# Content data models: Version and Revision

Status: agreed product principles (PRD) for future development. This page
describes the target. Several parts are not implemented yet:
[Current state](#current-state) lists the differences, and
[Open questions](#open-questions) lists what is still undecided. Agent guides
(`docs/agent/`) keep describing implemented behavior only; they change when
each part ships.

## Summary

Flats has two data models for content. Every content type uses exactly one of
them.

| | Version model | Revision model |
|---|---|---|
| For | content an agent prepares and an operator releases | content people and agents edit live |
| Types | websites (`flat`, static or server), pages (`page`) | documents (`docs`), design (planned) |
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

Applies to documents (`docs`), the planned design type and any later type that
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
      network's existing access control allows. A Public route is read-only:
      it shows no revision list and accepts no revision operation.
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
   revision quota are deleted at once, oldest first. Versions and pins are
   never deleted: they all stay, and new ones are refused until the count is
   below the quota.

## Model per content type

| Type | Model | Status |
|---|---|---|
| website (`flat`) | Version | creation and deployment are separate; quotas, deletion and tool names are not implemented |
| page (`page`) | Version | as for websites |
| document (`docs`) | Revision | revisions, quotas, pins and rollback; saving one document while keeping the others is not implemented |
| design | Revision | implemented |

A flat keeps its first save's type, so its model never changes.

The core already uses the word *revision* for Draft saves (`draft-revs`,
`expected_revision`, "Draft revision and hash" in approvals). That counter
belongs to the Version model's Draft and is not a Revision in this document's
sense: it has no pins, quotas or rollback.

## Current state

Compared with the implementation after version creation was split from
deployment (gosuda/flats PR #95):

- **Version model.**
  - Creating a version and deploying it are separate approved steps, as this
    spec requires. The version-creation step is still named `publish` in MCP,
    the CLI and the API (`approval action "publish"`), and deployment is
    `deploy` with a positive version number (`action "activate"`).
    `save_draft`, `save_version`, `save_page` and `flats deploy <dir>` with
    `deploy` requested only ask to create a version.
  - Versions are still pruned automatically after each deployment and version
    creation, but only deployed history: the newest `keep_versions` (default
    10) deployed versions are kept, plus the live version, any version with an
    open preview or a pending deployment approval, and every version that was
    never deployed. There is no version quota or version deletion yet.
  - `save_version` and `save_version_from_dir` write the Draft without
    approval despite their names. `create_version`, `save_draft_from_dir` and
    version deletion do not exist.
- **Revision model.**
  - Designs implement the Revision model with the revision store described in
    [design-type-TECH.md](design-type-TECH.md): whole-flat revisions, shared
    content-addressed storage, per-editor edit groups, revision and pin
    quotas, rollback and the revision tools.
  - Documents use the same store, quotas, retention, pins and revision tools.
    The docs worker records each revision boundary (agent writes, people's
    edit groups, a new version's merge and rollbacks) in the same database
    transaction as the change, and the host imports it
    ([document-revisions-TECH.md](document-revisions-TECH.md)). Existing
    `flats_docs_conflicts` records were migrated into `recovered` revisions,
    and that store is no longer written.
  - Rule 8's single-document behavior is implemented: `save_document` preserves
    sibling documents, assets and manifest settings, and adding a document keeps
    the existing entry. `title` changes only the manifest name. Whole-bundle
    replacement still uses `save_version`, although documents have no versions;
    moving that explicit operation to a Revision-model tool remains unfinished.
  - Quota rule 3 is not complete for documents: a person's live edit is not
    checked against the disk quota, and the revision recording it is stored
    without that check.

## Migration

1. **Documents.** A record of overwritten text in `flats_docs_conflicts` holds
   one document's text, not the whole flat, so each record becomes a synthetic
   recovery revision, oldest first: the flat's current content with only that
   document replaced by the preserved text. A document that no longer exists
   is restored with that text; assets and the manifest are the current ones.
   These revisions are marked as migrated, not as historical snapshots. Then
   one revision holds the flat's current content, so it is the latest. The
   preserved text can be recovered by rollback, and the separate store is
   removed afterwards. Migrated revisions follow the revision quota like any
   other: when a flat has more records than the quota allows, the oldest
   migrated revisions are deleted, as accepted for this rare case.
2. **Versions keep their numbers.** Existing versions keep their numbers, and
   new numbers continue after the highest one.
3. **Nothing is deleted to fit the quota.** A flat that already holds more
   versions than its quota keeps them all; creating a version is refused until
   the user deletes enough versions or raises the quota.
4. **Pruned versions do not count.** A version whose files `keep_versions`
   already deleted (`pruned`) stays only as a record. It does not count toward
   the version quota and cannot be deployed.
5. **`keep_versions` becomes the version quota.** A host that set
   `system.keep_versions` explicitly uses that value as its default version
   quota (`0`, never prune, becomes unlimited); otherwise the default is 20.
   Automatic pruning stops.

## Open questions

None at the moment. Record new ones here as they come up.
