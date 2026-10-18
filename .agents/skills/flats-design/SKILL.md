---
name: flats-design
description: Design a Flats repository change and produce or revise its specification with independent adversarial review. Use when the user explicitly limits work to design (such as designing a feature without implementing it), or when called by flats-dev-workflow. Ordinary doc/spec edits use flats-dev-workflow.
---

# Flats design

Read the [common development protocol](../../../docs/development-protocol.md)
for review, evidence and completion rules. This stage does not authorize implementation. Enter only for a clearly design-only
request or a coordinator call; route ordinary doc/spec edits to
[flats-dev-workflow](../flats-dev-workflow/SKILL.md).

## Produce the design

Read the original request, relevant current behavior and contracts, and the
[spec conventions](../../../docs/spec/README.md). Define the problem, scope and
exclusions, observable behavior, error/boundary cases, compatibility, completion
criteria and the evidence needed to verify them. Resolve routine design choices;
surface consequential unknown requirements rather than inventing user decisions.

For new functionality or changes to behavior, interfaces, data, security or
lifecycle, create or update the relevant `docs/spec/` document. For a small fix or
refactor that preserves contracts, a concise durable record in the task report, linked from the PR body when published, is
sufficient. Even trivial changes need a purpose and verification criterion.
Preserve the distinction between target requirements and implemented behavior.
Add the design steps of the matching [playbooks](../flats-dev-workflow/SKILL.md#playbooks),
and edit the written design with [flats-unslop](../flats-unslop/SKILL.md) before review.

## Review and handoff

Submit the identified design snapshot to an eligible different-model reviewer.
Ask them to challenge requirement coverage, contradictions, counterexamples,
feasibility and whether completion criteria are actually verifiable. Provide the
original request and relevant contracts, not just an author's explanation.
Record the review's evidence with [flats-review-evidence](../flats-review-evidence/SKILL.md).

Apply the common disposition/correction/re-review cycle. Unresolved behavior
choices or required evidence prevent acceptance. Return the reviewed design,
review report and implementation prerequisites. In standalone mode, stop here.
Design approval is a stage verdict, not permission to implement, publish, deploy or merge.
