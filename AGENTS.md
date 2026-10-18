# Developing Flats

Use the [development protocol](docs/development-protocol.md) for agent-driven
repository changes. Begin with [flats-dev-workflow](.agents/skills/flats-dev-workflow/SKILL.md)
for any change request or resumption unless explicitly limited to one stage.
Explicit design-only, implementation-only
and test-only requests use the corresponding stage and stop at its boundary.
Each stage includes review by a different model and correction before acceptance.
Agents without skill discovery must read these same linked instructions directly,
including the change-type playbooks and cleanup skills the workflow links.

Read the [documentation index](docs/README.md) and relevant
[specifications](docs/spec/README.md) before changing behavior. Keep specifications
and implementation consistent; disclose existing gaps rather than silently
changing requirements to match code.

PR publication follows the user's authorized scope. Never merge a PR, enable
auto-merge or bypass the PR through direct integration without explicit user
authorization for that specific existing PR. See the protocol for completion
and authorization boundaries.
