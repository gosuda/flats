# Refusals

A call that Flats refuses reports `category` in its error JSON and ends its
hint with `See guide refusal.<category>`. Unless the section or the error's
hint says otherwise, a refused call changed nothing; a refused version-creation request keeps the separately saved Private Draft.
`get_approval` `result_data.failure_code` uses the same names, plus
`apply_failed` for an unclassified failure while applying an approved action.

Arguments that do not match a tool's input schema (a missing required field
or a wrong type) are rejected by the MCP layer before Flats runs the tool, so
that error has no category: fix the arguments against the tool's input
schema and call again.

## invalid

The request or upload is malformed: a bad slug, manifest field, path,
encoding, negative number, unknown visibility, or a docs rule. A version,
Draft, preview, publish or rollback call on a document is also `invalid`:
documents have none of them; use `save_document` or `update_document`. The same
call on a board is `invalid` too: use `update_board` and the revision tools.
A board write over a board limit or with a bad element is `invalid`. Upload
validation lists every problem with its `path` and `fix` at once. Nothing was
saved. Fix every listed problem and send the complete request again. Rules:
`topic.static`, `topic.manifest`, `topic.docs`, `topic.board`, `topic.env-secrets`.

## not_found

The flat, version, approval or snapshot does not exist. Call `list_flats` for
slugs, `list_versions` for published version numbers, and use the
`approval_id` a request returned.

## document_not_found

The docs flat has no such Markdown path. Read `get_document` without `doc`
for the entry. For text a document had earlier, use `list_revisions` and
`get_revision {path}`.

## edit_conflict

`update_document` did not apply: a `find` text or block hash no longer
matches the live text (someone edited or removed it), occurs more than once
without `nth`, `nth` is past the last match, or `if_hash` differs. Their edit
wins: none of the operations in the call changed anything. Read
`get_document {slug, doc, blocks: true}` again, rebuild the operations
against the current text and retry. Never retry by replacing the whole
document to win.

## document_capacity

`update_document` would exceed a document limit: the request is larger than
512 KiB, the single update larger than 256 KiB, the Markdown above 1 MiB, a
block operation on a document of more than 50,000 blocks (use `replace` with
`find` there), the document's stored history is full, or the edit and the
revision recording it would put the flat over its disk quota (the message
says `disk quota`). Nothing changed. Split a large edit
into several calls or make the document smaller. Full history and a full disk
quota need the operator; tell the user. A `rollback_revision` is never refused
for the disk quota.

## not_docs

The tool needs a docs flat (`type: "docs"`), and this flat is a website.
Check `get_flat`; use `save_draft` for websites, or a new slug for a document.

## not_design

The tool needs a design (`type: "design"`), and this flat is another type.
Check `get_flat`. Create a design with `create_flat {type: "design"}` or
`save_design` on a new slug; designs keep their own revision history.

## not_board

The tool (`get_board`, `update_board`, `import_board` or `get_board_share`)
needs a board (`type: "board"`), and this flat is another type. Check
`get_flat`. Create a board with `create_flat {type: "board"}` or
`update_board` on a new slug. A flat never changes its type.

## board_capacity

The board write or import, together with the revision that records it, would
put the flat over its per-flat disk quota. Nothing changed, and Flats never
deletes history to make room. While a board is over its quota, every edit is
refused, deletions included. Tell the user: the operator can raise the disk
quota, or free space by lowering the revision quota or unpinning revisions.
A `rollback_revision` is never refused for the disk quota.

## public_edit_blocked

A board or document cannot become editable by anyone yet, so the
`set_visibility` approval with `access: "edit"` failed and its visibility
and level did not change. The message and `problems` list each reason:
`disk_quota_off` (the host disk quota is 0), `revision_quota_unlimited`
(the flat's revision quota is 0) or `pin_quota_full` (no pin is free for
the safe point; a document may then hold an unpinned safe point revision). Tell the user what the operator must change (a disk quota,
a revision quota, or a free pin), then request the change again.

## no_revisions

The revision tools work on documents, designs and boards, and this flat is a
website or a page: those keep numbered versions instead (`list_versions`, `rollback`).
A document that never ran has no revisions yet; save it with `save_document`
first.

## conflict

Version creation is also refused when the retained version count is at or above
the effective quota. Nothing is deleted: ask the operator to delete older
versions or raise its version quota (0 means unlimited). The separate disk
quota can refuse a save or version creation too; free space or raise it.
Lowering the version quota preserves every retained version.


The request contradicts the current state: `expected_revision` differs from
the Draft, the version is already live, the flat is not published yet (so it
cannot be Public), no snapshot exists for `restore_data`, or the content's
type differs from the flat's (website, page or document): the message names
both types. For a page, add `flats.json` `{"type":"page"}` or use
`save_page`; otherwise use another slug. Nothing was
overwritten. Read `get_draft`, `get_flat` or `list_versions`, reconcile, and
retry with current values. Never discard someone else's content to win.

On a board, `update_board` and `import_board` are refused with `conflict` when
`if_seq` or `if_ver` no longer matches, a target element was deleted, a target
is locked and the call did not unlock it, or the same `op_id` came with a
different request. An expired `get_board` cursor is `conflict` too. Nothing in
the call applied. Read `get_board` again (from the first page for a cursor),
rebuild the call against what people wrote, and retry with a new `op_id`.

While anyone can edit a board or document, unpinning its safe point is
`conflict`; it stays pinned until the operator stops guest editing.

## stale_approval

What an approval froze (Draft revision and hash, live version, snapshot,
visibility or providers) changed before it applied, so it was not applied.
Read the current state and request a new approval (`topic.approvals`).
A pre-upgrade `publish` request without the frozen live-version field is also
refused as stale: it lacks the state required by the current approval contract,
even if the live version did not change. Read `get_draft`, then request
`create_version` with its current revision and hash and obtain a new approval.

## unchanged_content

This exact content is already published. There is nothing new to approve;
change the Draft first, or use `deploy` with an existing version.

## not_deployed

Nothing is live yet, so there is nothing to roll back or restore. Request
publication of a Draft and wait for approval first. For `update_document`, the
document is not running: save it with `save_document`, which starts it at
once.

## forbidden

This surface may not perform the action, for example
`save_draft_from_dir` from a non-loopback caller. Use `save_draft` with
inline files, or the operator's console or host CLI where the action belongs.
Do not try to imitate the console.

## health_check_failed

The candidate started but its health path did not answer 2xx/3xx within 15
seconds on an isolated data copy. The previous code stays live. Read the
returned health result and `get_logs`, fix the build, save a new Draft and
request again (`topic.server`).

## snapshot_failed

The required data snapshot could not be captured before deployment or rollback.
The target version is retained, the previous live version keeps serving, and
the candidate was not activated on live data. Isolated health checks may
already have run; inspect `result_data.health_data`, the backup/storage error
and `get_logs`,
free space or repair storage, then request a new deployment or rollback
approval. A code-only rollback also requires this snapshot; a snapshot failure
never silently skips the backup or authorizes deleting version history.

## runtime_start_failed

The candidate could not start (bad entry, module error, missing export). The
previous code stays live; check `result_data` for whether live startup wrote
data. Read `get_logs`, fix the build or its `flats.json`, and request again.

## runtime_unavailable

Server flats (including docs flats) cannot run on this host right now: the
runtime is disabled or the built-in app is not configured. Tell the user; the
operator must enable it. Static flats still work.

## unavailable

The host could not complete the operation now (for example the docs app
returned an error). Retry once later; if it persists, show the user the error
and `get_logs`.

For `update_document` the error says which case applies:

* The edit was applied at a given seq: do not repeat it.
* It did not apply: retry.
* The outcome is unknown: read `get_document` first, so an insert is not applied twice.
* Too many live edits: wait a second.

## provider_not_permitted

The flat needs a network provider the operator has not permitted, typically
Portal, zrok, Tailscale Funnel, a Cloudflare quick tunnel, NetBird Expose or
the public reverse proxy for Public. Agents
cannot grant providers. Ask the
user to permit one in the console, then request again.

## provider_unavailable

The provider is permitted but not configured or not running on this host (for
example Public exposure is disabled). Tell the user; do not switch providers
on your own.

## provider_not_ready

The provider exists but no route opened yet. Check `get_flat` connection
state later; report the actual state instead of a URL.

## provider_in_use

The operator tried to revoke a provider that still serves routes, or its
routes could not be confirmed stopped. This is an operator-side setting; tell
the user what the error says.

## public_stop_unconfirmed

Flats could not confirm that a Public route stopped, so the flat may still be
reachable from the internet. Tell the user immediately, quote the error and
check `get_flat` endpoints; do not report the flat as Private.

## config_overridden

A host setting is fixed by a command-line flag or service file and cannot be
changed through settings. Operator-side; the error names how to change it.

## config_changed

Host settings changed after they were read. Operator-side: reload the
settings and retry.

## internal

An unexpected host error with no specific category. Nothing is known to have
changed; check `get_flat` and `get_logs`, and show the user the error before
retrying.
