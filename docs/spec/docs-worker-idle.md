# Idle document workers

Status: implemented — implementation and test-source reviews passed; integrated verification passed; final report review is pending.

## Summary

Documents release their server worker memory after two minutes without active use, and resume automatically when their live content is needed. Sleeping does not unpublish a document or change its data, permissions, address, or revision history.

## Behavior

1. This behavior applies only to built-in documents. Websites, arbitrary JavaScript/WASI server apps, designs, their previews, and their existing lifecycle remain unchanged.
2. A document becomes eligible to sleep after two continuous minutes without an active document request or collaboration connection. Time starts when the last such use finishes. The host checks eligibility on its existing document maintenance cadence; eligibility is not an exact shutdown deadline. Failed drains retry with increasing delay up to five minutes while ordinary maintenance continues; new foreground use resets that delay.
3. An open collaboration connection keeps its document awake, including while a person pauses typing. The existing connection timeout still applies. Sleep never deliberately disconnects a live collaborator or interrupts an accepted request.
4. Accessing live document content, opening its editor, editing through the API/MCP, saving changed content, or restoring a revision automatically makes the document available. Concurrent visitors share the same wake-up. The first response may take longer; no manual start action is required.
5. Host-served attachments and already-recorded revision files need not wake the document. Routine listing, provider reconciliation, quota updates, revision maintenance, and retention do not wake sleeping documents or count as visitor activity.
6. Before sleeping, acknowledged edits and their revisions remain durable. Pending revision recording is completed and imported before sleep; a failed or incomplete drain postpones sleep. A document with unresolved recovery state must not be treated as safe to discard or sweep. Already-clean sleepers still allow retention to reclaim unused historical files without waking.
7. Waking preserves live edits, document identity, attachments, revision history, and idempotent edit receipts. It does not create a new save, public-visibility approval, content revision, or deployment merely because the worker restarted.
8. A changed quota while sleeping applies before the first subsequent edit or collaboration message can commit. Missing or failed quota preparation refuses normal mutations, never allowing an unchecked edit. Reads and existing revision-recovery operations remain available when their own requirements are satisfied; an unconfirmed protection change blocks admission until cleanup/retry.
9. After host restart, previously saved live documents keep their routes and availability but need not all start workers. First use performs recovery before serving mutable content. Unfinished revisions remain protected until recovered; startup recovery and retention must not silently delete them.
10. A newly saved document still passes the existing isolated health/activation checks. A failed isolated validation or live-version commit leaves the previously live document and its text available. If serving initialization fails after a successful live-version commit, the saved content stays committed and the operation reports its serving failure; a later access retries. Saving changed content remains a way to repair a broken old document even when its save label could not be recorded (reported as an event). New saves do not become successful merely by deferring their validation until first use.
11. Sleeping remains an internal execution state: console/API running means a logical live document, including after lazy restoration before its first physical startup. First-wake failure is reported as a retryable error and an event. Its Private/Public visibility, provider grants, approvals, URLs, and redirects do not change. Existing access checks apply before a sleeping document can be accessed.
12. Delete, replacement, rename, and host shutdown cannot let a stale wake-up resurrect an obsolete document. A failed worker shutdown cannot cause a second worker to serve the same logical live instance concurrently. A newly committed replacement may serve while the retired old version drains, under the existing version-isolation rules; saving changed content during idle shutdown may omit its attribution label with an event rather than block repair.
13. A cancelled waiting request does not cancel a wake-up shared with other requests; it may stop waiting. Worker initialization and recovery remain bounded. Failure returns a retryable error and a later request may retry under existing failure backoff. Demand waiting on idle shutdown waits at most four seconds before receiving a retryable unavailable response; an unconfirmed shutdown still forbids a competing wake of that same logical instance.
14. Unexpected worker failure retains existing error reporting and retry semantics. It is not logged as a successful idle sleep. Successfully sleeping documents remain asleep across maintenance ticks until actual demand arrives.
15. The initial idle interval is a fixed two minutes, with no new manifest field, settings control, or public runtime API. This trades a short cold start after inactivity for lower idle memory without exposing premature tuning controls.

16. A capacity-limited document remains accessible for reading and revision recovery after wake; incomplete background revision import does not by itself make it unavailable. History reads and pin/quota controls do not wake sleepers, and may show only already-imported history until demand-driven recovery.

## Current state

The candidate implements automatic idle sleep, wake preparation and lazy host restoration. Independent design, code and test-source reviews passed. Integrated verification passed; final report review remains pending; this document is not a production rollout receipt.

## Open questions

No undecided product choices. See the test report for executed validation and explicit coverage limits.
