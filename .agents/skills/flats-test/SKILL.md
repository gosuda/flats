---
name: flats-test
description: Verify a Flats repository candidate against its expected behavior and review the test evidence with a different model. Use when the user explicitly asks only to verify an existing candidate or add/correct tests within verification scope, or when called by flats-dev-workflow. Unqualified test-fix requests use flats-dev-workflow; report product or requirement defects without changing them.
---

# Flats testing

Read the [common development protocol](../../../docs/development-protocol.md)
for review, evidence and completion rules. Enter only for explicit verification-only
scope or a coordinator call; route “fix the flaky test” and other unqualified
change requests to [flats-dev-workflow](../flats-dev-workflow/SKILL.md).

## Verify the candidate

Read expected behavior, completion criteria, exact candidate/dependency versions
and existing tests/results. Full-workflow entry requires current implementation
approval. Standalone verification can inspect unreviewed work, but must report
missing upstream approval and cannot establish whole-workflow completion.

Execute checks appropriate to the actual change; add or correct tests/fixtures
within verification scope; include the verification steps of the matching
[playbooks](../flats-dev-workflow/SKILL.md#playbooks). Keep product code and requirements unchanged. Map
completion criteria to evidence and record environment, commands, actual outcomes,
failures and unavailable coverage. Test-source inspection, execution and deployment
validation are separate claims. Never report skipped or timed-out checks as passes.

## Review and handoff

An eligible different-model reviewer challenges test oracles, omitted scenarios,
result reliability and whether the evidence justifies candidate acceptance. Give
them actual test sources, results and access to relevant product code; permit
bounded independent checks within existing authorization. Apply the common
correction/re-review cycle to tests and the verification report. Independence is
from test-result authors; disclose any reviewer product-code authorship.
Record the review's evidence with [flats-review-evidence](../flats-review-evidence/SKILL.md).

Product defects return needs-implementation; missing or contradictory requirements
return needs-design. Carry reviewed or reproducible evidence, and have the receiving
stage validate it against the design. In standalone mode report and stop; in the
full workflow let the coordinator route the return. Changes beyond verification
scope also return to implementation.

Review all test edits and obtain earlier-stage impact acceptance when they change
a previously reviewed snapshot. Report execution state and candidate verdict
separately: completed verification may reject a candidate. Return criteria-to-
evidence mapping, review record, defects, coverage limits and next action without
claiming a passing gate when required checks or candidate acceptance are missing.
