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
   Route issue classification to [flats-issue-triage](../flats-issue-triage/SKILL.md)
   and documentation cleanup or restructuring to
   [flats-doc-sanitize](../flats-doc-sanitize/SKILL.md).
2. Determine the earliest incomplete or stale stage from actual artifacts and
   review records. Reuse valid evidence; do not restart completed work by default.
   Pick every [playbook](#playbooks) that matches the change type and copy its
   checklist into the task plan.
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

## Playbooks

A playbook adds task-specific steps and known pitfalls inside the stages. It
never replaces or weakens a stage gate, a review or the merge rule; where a
playbook and the protocol differ, the protocol wins. Product requirements
belong in `docs/spec/`, not in playbooks.

| Change type | Playbook |
| --- | --- |
| Flaky, slow or timing-sensitive tests | [flaky-or-timing-tests](playbooks/flaky-or-timing-tests.md) |
| Making tests parallel or reshaping CI shards | [test-parallelism](playbooks/test-parallelism.md) |
| Performance, disk, memory or cold-start work | [performance-and-resources](playbooks/performance-and-resources.md) |
| Worker processes, QuickJS/wazero runtime, metrics | [worker-and-runtime](playbooks/worker-and-runtime.md) |
| Renaming CLI, MCP or API names | [interface-rename](playbooks/interface-rename.md) |
| Docs, specs, guide topics, the deploy skill | [docs-and-agent-guidance](playbooks/docs-and-agent-guidance.md) |
| CodeQL findings, GitHub Actions workflows | [security-and-ci](playbooks/security-and-ci.md) |
| Console, docs editor or design UI | [console-ui](playbooks/console-ui.md) |
| Network providers, console domains, reverse proxies | [networking-and-providers](playbooks/networking-and-providers.md) |
| Hangs, freezes and failing gates | [debugging](playbooks/debugging.md) |
| Publishing a PR, checking a merge or release | [pull-requests-and-release](playbooks/pull-requests-and-release.md) |
| Testing how an agent uses Flats without source access | [agent-behavior-gates](playbooks/agent-behavior-gates.md) |

Every prose-producing step (specs, design records, PR descriptions, commit
messages) finishes with [flats-unslop](../flats-unslop/SKILL.md).
Implementation runs [flats-deslop](../flats-deslop/SKILL.md) on its diff
before review.
