---
name: flats-deslop
description: Remove AI-generated code slop from a Flats branch diff against main without changing behavior. Use during implementation, before requesting code review, or when the user asks to deslop or clean up code style on a Flats branch.
---

# Flats deslop

Check the branch diff against main (`git diff origin/main...HEAD`, plus
uncommitted changes) and remove AI-generated slop that the branch introduced.
Read the surrounding file first: the local style is the reference.

## Focus areas

Go:

- Comments that restate the code, narrate the change ("now we...", "added
  for..."), or do not match the file's comment style. Keep comments that
  explain why, and doc comments on exported names.
- Checks that are abnormal for a trusted path: nil checks on values that
  cannot be nil, re-validation of input an earlier layer already validated,
  `recover` that hides a programming error, errors that are logged and also
  returned.
- Type assertions or conversions to `any` used only to silence the compiler.
- Deep nesting that early returns or `continue` would flatten.
- Wrapping that adds no context (`fmt.Errorf("%w", err)`), single-use helpers
  that only add indirection, and names inconsistent with the package.

JavaScript (console, docs editor, design app):

- `try`/`catch` around code that cannot throw, or a `catch` that swallows the
  error silently.
- Optional chaining or `?? default` on values that are always present.
- Type-checker escapes (`// @ts-ignore`, `/** @type {any} */`) or casts that
  only silence a warning.
- Patterns that differ from the module: other DOM helpers, other naming,
  another event style.

## Guardrails

- Keep behavior unchanged. Anything that would change behavior, including an
  apparent bug fix, is not a deslop edit: report it, and it goes through
  normal implementation and review.
- Prefer minimal, focused edits over broad rewrites. Touch only lines the
  branch changed, unless an adjacent line must change with them.
- Do not remove a check you cannot prove redundant.
- Run the affected tests after editing (`go vet` and `go test` for the touched
  packages, and the relevant JavaScript tests).
- Finish with a summary of one to three sentences.

## Source

Adapted from the `deslop` skill in
[cursor/plugins](https://github.com/cursor/plugins/blob/0b8e952052d482ee6de06deebf14d590d77b1b34/cursor-team-kit/skills/deslop/SKILL.md)
(MIT License, Copyright (c) 2026 Cursor). The full upstream license is in
[references/UPSTREAM-LICENSE.txt](references/UPSTREAM-LICENSE.txt). The Flats
version adds Go and JavaScript examples and the workflow guardrails, and it
never changes behavior: where upstream allows fixing a clear bug, the Flats
version reports the bug instead.
