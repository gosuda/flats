# Playbook: renaming CLI, MCP or API names

A lifecycle name (a CLI command, MCP tool, HTTP route or field) is a contract.
Design updates the relevant spec and [Surfaces](../../../../docs/surfaces.md)
first.

## Census every consumer

- [ ] Search the whole repository for the old name, including `.sh`, `.py`,
  `.yml`, `.json`, `.js`, `.mjs` and `.md` files, not only Go code.
- [ ] [`scripts/container-smoke.sh`](../../../../scripts/container-smoke.sh)
  runs in CI and has separate paths for websites (`deploy_approved`) and live
  documents (`deploy_document`). Update and run both. `sh -n` and
  `shellcheck` do not detect a removed CLI command.
- [ ] Update the gate scripts in `scripts/`, the guide topics in `docs/agent/`
  (then `make docs-generate`), the distributed deploy skill and the plugin
  catalogs checked by `plugins/skill_test.go`. See
  [docs-and-agent-guidance](docs-and-agent-guidance.md).

## Test

- [ ] Exercise the canonical calls end to end with values captured from earlier
  responses: the Draft revision and hash, and the exact approval IDs.
- [ ] Keep raw negative tests (old names refused, wrong revision refused)
  outside any compatibility helper that rewrites old requests into new ones,
  or the helper hides the refusal.
- [ ] Run [`scripts/lifecycle-gate.sh`](../../../../docs/lifecycle-verification.md)
  and compare its result with a run on main.
