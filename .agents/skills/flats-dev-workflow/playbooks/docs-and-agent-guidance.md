# Playbook: docs, specs and agent guidance

For a cleanup or restructure of existing docs, use
[flats-doc-sanitize](../../flats-doc-sanitize/SKILL.md) instead. Prose follows
[flats-unslop](../../flats-unslop/SKILL.md).

## Specs

- [ ] Spec conventions (status line, `Current state`, `Open questions`,
  when a `-TECH.md` page exists) are in
  [the spec index](../../../../docs/spec/README.md). Follow them; do not copy
  them elsewhere.
- [ ] A `-TECH.md` page exists only while its feature is unshipped. When it
  ships, fold its durable design into the architecture docs.
- [ ] When folding or deleting docs, have an independent reviewer compare the
  deleted text with what remains. API contracts and storage-format details are
  easy to lose.

## Agent guidance (`docs/agent/`)

- [ ] `docs/agent/` is the single source for MCP `guide` topics, both generated
  references and the llms.txt documents. Never edit `docs/runtime-api-v1.md`
  or `docs/content-types.md` by hand; run `make docs-generate`.
- [ ] MCP instructions are always loaded, so `TestInstructionsStaySmall`
  (`internal/mcpx/guide_test.go`) caps their size. Put details in guide topics.
- [ ] Each new error category needs a `refusal.<category>` section in
  `docs/agent/refusals.md` (`TestRefusalPagesCoverEveryCategory`).
- [ ] A new topic must be routed from `topic.index` (`TestTopicsAreReachable`).

## The distributed deploy skill

- [ ] A change to `plugins/flats/skills/flats-deploy/SKILL.md` raises its
  visible `Flats bootstrap revision` and `BootstrapRevision` in
  `internal/clientstate`, pairs the new revision with a plugin release in
  `TestBootstrapReleaseContract`, and updates every plugin manifest and
  marketplace catalog that `plugins/skill_test.go` checks. See
  [Revisions](../../../../docs/release.md#revisions).
- [ ] Development skills stay in `.agents/skills/` with their symlinks;
  `TestPluginSkillBoundary` checks both sides
  ([Skills](../../../../docs/agents.md#skills-for-flats-and-skills-for-this-repository)).

## Verify

- [ ] Run `make docs-verify`. It runs `docs-check` uncached; a cached
  `go test` pass of `TestRepositoryDocs` can be stale.
- [ ] Run `make docs-audit` and account for every new unreachable doc.
