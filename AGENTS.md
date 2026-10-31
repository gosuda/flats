# Developing Flats

Use the [development protocol](docs/development-protocol.md) for agent-driven
repository changes. Begin with [flats-dev-workflow](.agents/skills/flats-dev-workflow/SKILL.md)
for any change request or resumption unless explicitly limited to one stage.
Explicit design-only, implementation-only
and test-only requests use the corresponding stage and stop at its boundary.
Each stage includes review by a different model and correction before acceptance.
Agents without skill discovery must read these same linked instructions directly,

JavaScript: every `.js` and `.mjs` file declares `'use strict';` in its
directive prologue, after any shebang or leading comment. New files carry it
from the start; `.mjs` is strict implicitly, and the declaration is still
required so the rule stays uniform and grep-checkable. A script that agents
run through browser tools as one expression (`docs/agent/scripts/`) declares
it as the first statement of its outer function instead, so the file stays a
single expression.

Memory: allocate on hot paths only when ownership escapes. Reuse
short-lived buffers through `sync.Pool`, stream with `io.Copy` or
`io.CopyBuffer` rather than `io.ReadAll` of a whole body when a streaming
form exists, and prefer `json.Encoder`/`json.RawMessage` over an
intermediate `[]byte` or `string` copy. Reserve only the bytes a request
actually consumed; a speculative reservation sized to a limit starves
concurrent work.

Read the [documentation index](docs/README.md) and relevant
[specifications](docs/spec/README.md) before changing behavior. Keep specifications
and implementation consistent; disclose existing gaps rather than silently
changing requirements to match code.

PR publication follows the user's authorized scope. Never merge a PR, enable
auto-merge or bypass the PR through direct integration without explicit user
authorization for that specific existing PR. See the protocol for completion
and authorization boundaries.
