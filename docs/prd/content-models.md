# Content data models: Version and Revision

Status: agreed product principles (PRD) for future development. This page
describes the target. Several parts are not implemented yet:
[Current state](#current-state) lists the differences, and
[Open questions](#open-questions) lists what is still undecided. Agent guides (`docs/agent/`) keep describing implemented
behavior only; they change when each part ships.

## Summary

Flats has two data models for content. Every content type uses exactly one of
them.

| | Version model | Revision model |
|---|---|---|
| For | content an agent prepares and an operator releases | content people and agents edit live |
| Types | websites (`flat`, static or server), pages (`page`) | documents (`docs`), design (planned) |
| Working copy | a Private Draft | the live content itself |
| History unit | a numbered version (v1, v2, v3, ...) | a revision per edit unit |
| Needs operator approval | creating a version, every deployment, rollback | nothing in the content; only visibility changes |
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

1. **Draft changes need no approval.** Saving the Draft (`save_draft`,
   `save_version`, `save_page`, `save_version_from_dir`, `flats deploy`)
   changes only the Private Draft and never the live content. Draft previews
   stay Private.
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
   restore keeps its own explicit request and approval, as today.
5. **A failed deployment leaves the version in place.** The version stays in
   the list, and the previously live version keeps serving.
6. **Published means deployed at least once.** A flat with no successful
   deployment is Unpublished. Visibility (`private`/`public`) stays independent
   and every change in either direction needs approval.
7. **The console manages versions.** Operators list versions, approve their
   creation, and deploy or roll back from the console.

## Revision model

Applies to documents (`docs`), the planned design type and any later type that
people and agents edit live.

1. **No Draft, versions or publish.** The content is always live, and the
   console has no version or deployment management for these types.
2. **Edits need no approval.** An agent changes the content at once. Private
   content stays Private. On Public content every change is visible to everyone
   as soon as it is saved. Visibility changes still need approval in both
   directions.
3. **Live concurrent editing.** People and agents can edit the same content at
   the same time.
4. **Every edit unit creates a revision.** What one edit unit is remains open
   (see [Open questions](#open-questions)).
5. **Two separate quotas.**
   - A *revision quota* is the maximum number of unpinned revisions kept. When
     it is exceeded, the oldest unpinned revisions are deleted automatically.
   - A *pin quota* is the maximum number of pinned revisions. Pinned revisions
     do not count toward the revision quota and are never deleted
     automatically.
   - Pinning when the pin quota is full is refused; Flats never unpins a
     revision by itself to make room.
   - An unpinned revision counts toward the revision quota again from then on.
6. **Rollback creates a new revision.** Rolling back to revision R creates a
   new revision whose content equals R's. History is never rewritten, and the
   rollback itself can be rolled back. To people editing at that moment, a
   rollback is one more concurrent edit.
7. **Revisions are managed inside the content.** Listing, viewing, pinning,
   unpinning and rolling back happen in the content's own editor, and through
   MCP for agents, not in the console.
8. **Only writers manage revisions.** Every revision operation needs write
   access to the content.
   - Today write access means the Private route (Local loopback, or Tailscale
     as the existing tailnet ACL allows) and MCP. A Public route is read-only:
     it shows no revision list and accepts no revision operation.
   - Agents may pin, unpin and roll back without approval.

## Model per content type

| Type | Model | Status |
|---|---|---|
| website (`flat`) | Version | versions exist; separate version creation is not implemented |
| page (`page`) | Version | as for websites |
| document (`docs`) | Revision | no revisions yet; planned next |
| design | Revision | planned type |

A flat keeps its first save's type, so its model never changes.

The core already uses the word *revision* for Draft saves (`draft-revs`,
`expected_revision`, "Draft revision and hash" in approvals). That counter
belongs to the Version model's Draft and is not a Revision in this document's
sense: it has no pins, quotas or rollback.

## Current state

Compared with `main` at `aa5bf97`:

- **Version model.** A version is created only by an approved publish, which
  allocates `vN` and deploys it in one step; a failed health check consumes no
  number. `save_version` writes the Draft without approval despite its name.
  Splitting version creation from deployment is not implemented.
- **Revision model.** Documents have no revisions. When a new document version
  overwrites live text, the overwritten text is preserved in
  `flats_docs_conflicts` (up to 8 records and 2 MiB per document). A docs
  bundle with several documents or assets is saved with the tool named
  `save_version`, although documents have no versions.

## Open questions

- **Edit unit.** Real-time editing sends a change per keystroke, which is far
  too fine for a revision. Candidates: one revision per agent write call; for
  people, edits grouped per editor by idle time or by a maximum interval.
- **Version retention.** Today files of versions beyond the newest
  `keep_versions` (default 10) are pruned after each deployment, unless live.
  With undeployed versions, it is open how they count and whether one can be
  pruned before it was ever deployed.
- **Revision scope.** A docs flat can hold several documents and a design holds
  several files: a revision per document/file or per flat.
- **Quota defaults and limits.** Default values, where they are configured
  (host or per flat), and whether a byte limit applies as well as a count.
- **Assets.** Images and fonts referenced by any retained revision must stay
  available while that revision exists.
- **Conflict records.** Whether revisions replace `flats_docs_conflicts`, by
  keeping the text before an overwrite as a revision.
- **Naming.** Whether the Draft save counter and the `save_version` tool are
  renamed to keep *revision* and *version* unambiguous.
- **Migration.** Existing documents would get their current content as the
  first revision. Existing published versions would keep their numbers.
