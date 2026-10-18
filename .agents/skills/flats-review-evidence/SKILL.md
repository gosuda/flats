---
name: flats-review-evidence
description: Record the evidence of a Flats review with the devreview helper. Use whenever a design, implementation or testing stage hands a candidate to a different-model reviewer, including re-reviews and approval carry-forward checks. Records and checks evidence only; it never approves a stage.
---

# Flats review evidence

The [development protocol](../../../docs/development-protocol.md#snapshot-and-evidence)
requires every review to be bound to an exact snapshot with recorded reviewer
identity, budget and task report. The
[review evidence helper](../../../internal/devreview/README.md) records that
evidence mechanically. The README is the reference for its commands, flags,
exit codes and the user-level reviewer configuration; this skill says when to
use it and in what order.

The helper records and checks evidence. It never approves anything: a complete
invocation means the evidence is complete, and the verdict is whatever the
reviewer wrote.

## When to use it

Use the helper by default when a reviewer is configured in your user-level
reviewer configuration. Without one, record the same evidence by hand as the
protocol requires: snapshot identity, manifest and before/after comparison,
reviewer requested and effective identity, session, budget and report. The
helper is not mandatory yet.

Use it for every review hand-off in a stage: the first review, each re-review
after corrections, and each request for an earlier stage's reviewer to accept
carrying its approval to a new snapshot (for example after test-only edits or
a rebase).

## Procedure

Keep one evidence directory per task, outside the repository, for all of the
task's manifests, copies, invocations and reports.

1. `snapshot` the candidate against its base. Exclude review reports, logs and
   probe output; include ignored inputs the review depends on.
2. `materialize` a read-only copy from that manifest and give the reviewer the
   copy. Review the live working tree only when you can state how concurrent
   writes are prevented.
3. `run` the configured reviewer on the copy with every material author's
   model ID and the stage. Set the task's invocation budget on the first run;
   raise it later only with a stated reason. A re-review may continue the
   first review's session; a first review needs a fresh one.
4. After the review, `verify` the copy, or the working tree for a live review,
   against the manifest. Drift makes the review incomplete: take a new
   snapshot and review again.
5. `report` writes the task report skeleton from the recorded evidence. Fill in
   the reviewer's exact verdict and findings from the receipts, the
   dispositions, verification evidence and next action; never replace a
   verdict with a summary.

If `run` refuses a start because an earlier invocation's end cannot be
established, check whether its reviewer process still runs. Once you are sure
it does not, settle it with `abandon` and a reason that says how you checked.

A new candidate (corrections, a rebase, test-only edits) is a new snapshot:
repeat from step 1 for its review.
