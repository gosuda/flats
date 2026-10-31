# Notebook transaction editor

This package implements the notebook content type: the item model, the
single writer with durable receipts, concurrency rules, undo, revisions, the
live protocol, the Markdown reader and writer, and the browser app under
`web/`. The product spec is [docs/spec/notebook.md](../../docs/spec/notebook.md);
its Current state section lists what is not done yet.

## Transaction boundary

`applyEdit` and `undoEdit` run inside a caller-owned SQL transaction. They do
not commit, publish deltas or allocate mutation sequences. The caller supplies
a positive, never-reused `editOptions.Seq` and an existing browser session.
Keep `meta.seq` at the call-start state while evaluating guards. Advance it
only after execution, including a durable refusal when the writer records one.
Sequence allocation must not reuse a number after a refusal or rollback.

The executor returns canonical changed records separately from its undo
journal. The canonical records include physical rekeys and aggregate-only page
updates. Those effects do not become logical place assignments or undo parts.
A failed unit contributes neither kind of effect.

Every semantic operation must preserve the page/block tree and minimum page
count, including operations inside a `unit_id`. A unit binds rollback and
inverse dependencies; it does not permit transient cycles, orphan ownership,
or deletion of the last page. Editor compounds must order valid operations.

## Undo retention

The existing `undo` table holds inverse stacks, fixed-size eviction state and
indexed `closed:<action_id>` rows. Closed identities are session bookkeeping,
not inverse payloads, so they have `bytes=0` for the inverse budget. They still
consume persistent SQLite pages. The later writer's disk-quota reservation
must charge this metadata, as it charges `created` and receipts. Deleting a
session cascades all its undo, redo and closed-identity rows. Evicting inverse
payloads must not reopen an old action id.

A `turn_blocks` selection has one undo entry with independent conversion
components. Conversions sharing structural effects join one component,
regardless of selection order; each kind assignment and its child hoists
remain inseparable. An explicit compound unit, move, cascade or duplicate binds
its dependent effects.
Undo checks every guard against the start state and skips components that
would violate structure or increase current item-limit excess. Redo contains
only the effects actually undone.

## Verification and remaining integration

The independent adjacency-list oracle compares logical sibling order, content,
outcomes, incarnations and versions after deterministic multi-session edits,
undo, redo, revival and reopen. Structure checks run against unit-start state.
The default is 16 seeds; use `-notebook-model-seeds=200` for the full stress run.
A divergence prints its seed, first divergent prefix and a deletion-minimal
replay sequence containing both edits and control events. Focused tests cover
overlapping conversion dependencies, typing runs, split/merge races, 501-block
duplication and rebalancing during later operations of one undo unit.

Bulk copies allocate root positions from one sibling scan. Undo recording reads
only open entries or the last typing entry; pruning reads metadata and maintains
one byte total for its eviction pass. Inverse neighbors share one reconstructed
pre-unit sibling list per namespace rather than rebuilding it for every part.

Current admission and structural validation use streaming SQL recounts. They
have no second mutable server content model, but they have not established the
release latency targets. Measure and optimize these scans and undo lookup work
before release; passing the package's correctness tests is not performance
evidence. Database copying must extend the same two-pass remapper, and page
recovery will supply its identity-collision map to that remapper.
