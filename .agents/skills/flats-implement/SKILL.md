---
name: flats-implement
description: Implement a defined Flats repository change with development-time tests and independent adversarial code review. Use when the user explicitly limits work to implementation or when called by flats-dev-workflow; requires current design-review evidence. Unqualified implement or fix requests use flats-dev-workflow.
---

# Flats implementation

Read the [common development protocol](../../../docs/development-protocol.md)
for review, evidence and completion rules. Enter only for explicit implementation-only
scope or a coordinator call; route unqualified “implement X” requests to
[flats-dev-workflow](../flats-dev-workflow/SKILL.md).

## Establish readiness and implement

Read the requested change, relevant code/contracts and design record. Verify
current design-review evidence, including a concise reviewed record for small
changes. Missing readiness returns needs-prior-stage with the exact missing
record/review and design as the next step. A standalone invocation does not
silently perform design; independent invocation is not prerequisite-free.

Implement within the design and any applicable worktree/ownership rules. Add or update
relevant tests while developing; do not postpone them until final verification.
Follow the matching [playbooks](../flats-dev-workflow/SKILL.md#playbooks).
Before requesting review, run [flats-deslop](../flats-deslop/SKILL.md) on the
diff and rerun the affected tests; edit prose with [flats-unslop](../flats-unslop/SKILL.md).
Explain material deviations. A needed contract change returns needs-design in
standalone mode without editing the contract; unresolved consequential choices
return needs-input. In the full workflow the coordinator handles the return.

## Review and handoff

Give an eligible different-model reviewer the original request, accepted design,
complete implementation/test snapshot and actual check results. Ask them to find
spec violations, regressions, failure-handling errors, resource/concurrency issues,
compatibility problems and weak or incorrect tests. Review all material candidate
changes, not only author-selected files.
Record the review's evidence with [flats-review-evidence](../flats-review-evidence/SKILL.md).

Apply the common correction/re-review cycle. Return the accepted candidate,
review record, development-test results and remaining integrated verification
needs. A standalone implementation ends here and does not claim the final testing
stage or full workflow is complete.
