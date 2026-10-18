---
name: flats-dev-workflow
description: Coordinate a complete Flats repository change or resume development through design, implementation and testing with independent model review. Use for feature, fix, refactor, documentation, configuration, dependency and test-change requests; explicit stage-only requests use the corresponding stage skill.
---

# Flats development workflow

Read the [common development protocol](../../../docs/development-protocol.md)
first; it owns review independence, evidence, correction and completion rules.

1. Identify and state the intended scope, requested outcome, permitted changes
   and existing artifacts.
   Read repository instructions and relevant specs from the
   [documentation index](../../../docs/README.md). Respect narrower user scope.
2. Determine the earliest incomplete or stale stage from actual artifacts and
   review records. Reuse valid evidence; do not restart completed work by default.
3. Run [flats-design](../flats-design/SKILL.md), then
   [flats-implement](../flats-implement/SKILL.md), then
   [flats-test](../flats-test/SKILL.md). Each stage must pass its gate before
   progressing. A small change uses a short design record, not skipped design.
4. Route product defects back to implementation and requirement defects back to
   design. Validate the returned evidence, update the affected result, and renew
   affected reviews and checks. Do not reset budgets on a return.
5. Where independent units are delegated, preserve their scope and ownership,
   then review and verify the integrated candidate at the feature boundary.
6. Report stage states separately from candidate verdicts, including current
   identities, accepted impact assessments, limitations and follow-up. Stop at
   the authorized publication boundary; readiness is not merge authorization.

When the user requests a single stage, invoke only that stage and its own review
cycle. Missing prerequisites identify the next necessary action; they do not
authorize expanding the request.
