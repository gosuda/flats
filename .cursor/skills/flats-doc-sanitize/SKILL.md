---
name: flats-doc-sanitize
description: Sanitize, restructure, deduplicate, and verify documentation in the Flats repository. Use when the user asks to sanitize docs, clean up outdated or contradictory documentation, reorganize documentation directories, merge duplicate specs, or prune volatile verification receipts in the flats repo.
disable-model-invocation: true
---

# Flats Document Sanitize

Maintain a contradiction-free, single-source-of-truth documentation tree by dynamically discovering the current structure, verifying claims against the codebase, asking the user instead of guessing on ambiguities, and removing volatile or redundant content.

## Core Principles

1. **Resolve contradictions**: Cross-check claims across documents and against the actual codebase. Eliminate conflicting statements so every behavior is defined consistently.
2. **Update outdated content**: Sync operational, architecture, and agent-reference documentation with current implementation (CLI commands/flags, MCP tools, HTTP routes, storage layout, limits, and UI behavior).
3. **Never guess on ambiguity**: When documents conflict with each other or with code and the intended product policy is unclear, or when a structural split/deletion decision is ambiguous, stop and ask the user via `AskQuestion` before editing.
4. **Deduplicate (Single Source of Truth)**: Each concept, rule, or state machine belongs in one canonical document. Other documents should summarize briefly and link to the canonical source rather than repeating details.
5. **Re-split blurry boundaries**: When multiple documents have tangled or overlapping scopes, collect them and re-split cleanly along domain boundaries (e.g., turning N overlapping files into M focused domain documents).
6. **Delete low-value or volatile content**: Remove information that does not have ongoing maintenance value:
   - Point-in-time commit/tree/binary hashes, worker handoff notes, and historical test execution logs.
   - One-off review or polish artifacts once their change has shipped.
   - Separate implementation design notes for already-shipped features, after folding their durable architectural invariants into the canonical architecture docs.

## Handling Spec vs. Implementation Divergence

When the codebase does not faithfully or completely implement a specification (PRD), **never silently downgrade the spec to match incomplete or buggy code**. Follow this protocol:

1. **Do not weaken normative specs without user confirmation**:
   - If code is missing a requirement or deviates from a spec, do not delete the requirement from the spec or document a code bug as intended design.
2. **Ask the user to classify the gap (`AskQuestion`)**:
   - Present the exact discrepancy (what the spec requires vs. what the code in `internal/...` actually does) and ask whether it is:
     - **(a) Unimplemented / planned work** (valid target requirement not built yet),
     - **(b) An implementation bug / omission** (code failed to follow the spec), or
     - **(c) An intentional design change** (the spec is outdated and should be updated to match the code).
3. **Apply the separation rule by document role**:
   - **If (a) Unimplemented or (b) Code bug/omission**:
     - **Product Specs (`docs/spec/`)**: Preserve the target requirement in the spec body, and record the exact implementation gap under the spec's `Current state` section.
     - **Operational, Architecture & Agent Docs (`docs/agent/`, operator/architecture guides)**: Describe only what the running code actually supports today so users and agents are not misled into calling unimplemented behavior.
     - **Implementation Gap Report**: Surface a dedicated list of spec-vs-code omissions in the final sanitize summary (so they can be tracked as issues or follow-up implementation tasks).
   - **If (c) Intentional design change (confirmed by user)**:
     - Update the spec body to reflect the confirmed decision and remove the stale requirement.

## Structural & Domain Principles

Organize documentation by **audience and purpose** into clear domain directories, reachable through a master index:

- **Operations (operator & user guides)**: How operators and users install, configure, network, containerize, and connect agents. Must not duplicate internal state machine proofs.
- **Architecture (maintainer design contracts)**: How the implemented system works internally (process lifecycle, storage/data models, API/CLI/MCP/Console surfaces, runtime sandboxes, network provider manager, built-in apps). Each file should own one cohesive subsystem.
- **Product Specs (PRDs)**: What the product must do and why, independent of implementation.
  - Keep a `Current state` gap section while a spec is partially unimplemented; remove gap bullets only after verifying the code implements them completely.
  - Once a feature's separate technical design note is fully implemented, fold its durable design invariants into the architecture docs and delete the temporary technical note. (If parts of the technical note remain unimplemented, confirm with the user before deleting.)
- **Verification (repeatable gate guides)**: Repeatable verification procedures, CI shard rules, and acceptance criteria. Keep *how to run and what must pass*; delete historical point-in-time execution receipts.
- **Generated & Embedded Agent Docs**: Single source of truth for MCP `guide` topics and generated references.

## Repository Invariants (Discover Dynamically)

Never assume fixed filenames; discover programmatic contracts from the codebase before moving or editing files:
- **Protected blocks**: Respect explicit `<!-- PROTECTED ... -->` comments in Markdown files.
- **Generated references**: Check tests (`go test ./...`) for generated documentation targets. Edit only the source files and run the corresponding `-update` test rather than hand-editing generated outputs.
- **Code-referenced doc paths**: Search the codebase (`rg` for `.md` paths and `//go:embed` directives) before moving or deleting any Markdown file so embedded assets, `llms.txt` links, and test assertions are updated together.
- **No review screenshots in Git**: Never keep or commit verification screenshots in the repository.

## Sanitize Workflow

Copy this checklist and track progress during execution:

```markdown
Sanitize Progress:
- [ ] Step 1: Discover Current Doc Graph & Code Contracts
- [ ] Step 2: Audit for Contradictions, Spec-vs-Code Gaps, Drift, Overlap & Volatility
- [ ] Step 3: Ask User on Ambiguities & Spec-vs-Code Gaps (No Guessing Gate)
- [ ] Step 4: Consolidate, Re-split, Update & Prune
- [ ] Step 5: Sync Generated Docs & Update Cross-Links
- [ ] Step 6: Run Verification Suite & Report Implementation Gaps
```

### Step 1: Discover Current Doc Graph & Code Contracts

1. Run `python3 .cursor/skills/flats-doc-sanitize/scripts/check_docs.py` to build the link reachability graph from `README.md` and Go code entrypoints (`//go:embed`, `.go` references), checking for broken relative links, unreachable (orphan) Markdown files, and 40-char hash literals.
2. Inspect the current index files and documentation tests to understand the active structure and automated invariants.

### Step 2: Audit for Contradictions, Spec-vs-Code Gaps, Drift, Overlap & Volatility

Compare the discovered documents against each other and the live codebase:
- **Contradictions**: Conflicting terminology, lifecycle steps, quotas, or authority rules across files.
- **Spec-vs-Code Gaps**: Requirements in product specs that are missing, partial, or differently implemented in code.
- **Outdated claims**: Documented flags, routes, tools, or `Current state` notes that no longer match the code.
- **Boundary overlap**: Multiple documents explaining the same subsystem without clear separation of concerns.
- **Volatile/redundant artifacts**: Shipped technical design notes, one-off review notes, or historical commit/hash receipts.

### Step 3: Ask User on Ambiguities & Spec-vs-Code Gaps (No Guessing Gate)

If any conflict, unimplemented/partial spec requirement, or proposed document merge/split/deletion has more than one reasonable interpretation:
- Stop and formulate clear options via `AskQuestion` (or structured questions).
- Never guess product intent, silently drop an unimplemented PRD requirement, or enshrine a code bug as documentation.

### Step 4: Consolidate, Re-split, Update & Prune

1. Reorganize or re-split overlapping documents so each file has a single, clear responsibility within its domain directory.
2. Fold durable architecture knowledge from completed implementation notes or one-off specs into the canonical architecture document, then delete the redundant file.
3. Update operational/architecture/agent docs to match verified code, while keeping unimplemented spec requirements in `docs/spec/` with accurate `Current state` gap notes.
4. Strip volatile historical receipts.

### Step 5: Sync Generated Docs & Update Cross-Links

1. Regenerate any derived documentation via the repository's test update flags.
2. Update all relative links across Markdown indexes and any Go files referencing documentation paths.

### Step 6: Run Verification Suite & Report Implementation Gaps

1. Verify both link graph integrity and repository test contracts:
   ```bash
   python3 .cursor/skills/flats-doc-sanitize/scripts/check_docs.py
   go test ./docs/... ./plugins/... ./internal/mcpx/...
   ```
2. In the final summary, explicitly list any **Spec-vs-Implementation Gaps** discovered during the audit so the user can decide on follow-up code fixes.
