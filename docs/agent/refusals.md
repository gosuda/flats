# Refusals

A call that Flats refuses reports `category` in its error JSON and ends its
hint with `See guide refusal.<category>`. Unless the section or the error's
hint says otherwise, a refused call changed nothing; for example, a save with
`deploy: true` whose publish request fails keeps the saved Draft and says so.
`get_approval` `result_data.failure_code` uses the same names, plus
`apply_failed` for an unclassified failure while applying an approved action.

Arguments that do not match a tool's input schema (a missing required field
or a wrong type) are rejected by the MCP layer before Flats runs the tool, so
that error has no category: fix the arguments against the tool's input
schema and call again.

## invalid

The request or upload is malformed: a bad slug, manifest field, path,
encoding, negative number, unknown visibility, or a docs rule. Upload
validation lists every problem with its `path` and `fix` at once. Nothing was
saved. Fix every listed problem and send the complete request again. Rules:
`topic.static`, `topic.manifest`, `topic.docs`, `topic.env-secrets`.

## not_found

The flat, version, approval or snapshot does not exist. Call `list_flats` for
slugs, `list_versions` for published version numbers, and use the
`approval_id` a request returned.

## document_not_found

The docs flat has no such Markdown path, or the requested conflict generation
is missing or was evicted. Read `get_document` without `doc` for the entry,
or use a generation from the current `conflicts` list.

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
`find` there), or the document's stored history is full. Nothing changed. Split a large edit
into several calls or make the document smaller. Full history needs the
operator; tell the user.

## not_docs

The tool needs a docs flat (`type: "docs"`), and this flat is a website.
Check `get_flat`; use `save_draft` for websites, or a new slug for a document.

## conflict

The request contradicts the current state: `expected_revision` differs from
the Draft, the version is already live, the flat is not published yet (so it
cannot be Public), or no snapshot exists for `restore_data`. Nothing was
overwritten. Read `get_draft`, `get_flat` or `list_versions`, reconcile, and
retry with current values. Never discard someone else's content to win.

## stale_approval

What an approval froze (Draft revision and hash, live version, snapshot,
visibility or providers) changed before it applied, so it was not applied.
Read the current state and request a new approval (`topic.approvals`).

## unchanged_content

This exact content is already published. There is nothing new to approve;
change the Draft first, or use `deploy` with an existing version.

## not_deployed

Nothing is live yet, so there is nothing to roll back or restore. Request
publication of a Draft and wait for approval first.

## forbidden

This surface may not perform the action, for example
`save_version_from_dir` from a non-loopback caller. Use `save_version` with
inline files, or the operator's console or host CLI where the action belongs.
Do not try to imitate the console.

## health_check_failed

The candidate started but its health path did not answer 2xx/3xx within 15
seconds on an isolated data copy. The previous code stays live. Read the
returned health result and `get_logs`, fix the build, save a new Draft and
request again (`topic.server`).

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
Portal or Tailscale Funnel for Public. Agents cannot grant providers. Ask the
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
