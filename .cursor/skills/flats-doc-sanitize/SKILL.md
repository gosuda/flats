---
name: flats-doc-sanitize
description: Sanitize, restructure, deduplicate, and verify documentation in the Flats repository. Use when the user asks to sanitize docs, clean up outdated or contradictory documentation, reorganize documentation directories, merge duplicate specs, or prune volatile verification receipts in the flats repo.
disable-model-invocation: true
---

# Flats Document Sanitize

Maintain a contradiction-free, single-source-of-truth documentation tree by dynamically discovering the current structure, verifying claims against the codebase, asking the user instead of guessing on ambiguities, and removing volatile or redundant content.

## Core Principles

1. **Resolve contradictions**: Cross-check claims across documents and against the actual codebase. Eliminate conflicting statements so every behavior is defined consistently.
2. **Update outdated content**: Sync documentation with current implementation (CLI commands/flags, MCP tools, HTTP routes, storage layout, limits, and UI behavior).
3. **Never guess on ambiguity**: When documents conflict with each other or with code and the intended product policy is unclear, or when a structural split/deletion decision is ambiguous, stop and ask the user via `AskQuestion` before editing.
4. **Deduplicate (Single Source of Truth)**: Each concept, rule, or state machine belongs in one canonical document. Other documents should summarize briefly and link to the canonical source rather than repeating details.
5. **Re-split blurry boundaries**: When multiple documents have tangled or overlapping scopes, collect them and re-split cleanly along domain boundaries (e.g., turning N overlapping files into M focused domain documents).
6. **Delete low-value or volatile content**: Remove information that does not have ongoing maintenance value:
   - Point-in-time commit/tree/binary hashes, worker handoff notes, and historical test execution logs.
   - One-off review or polish artifacts once their change has shipped.
   - Separate implementation design notes for already-shipped features, after folding their durable architectural invariants into the canonical architecture docs.

## Structural & Domain Principles

Organize documentation by **audience and purpose** into clear domain directories, reachable through a master index:

- **Operations (operator & user guides)**: How operators and users install, configure, network, containerize, and connect agents. Must not duplicate internal state machine proofs.
- **Architecture (maintainer design contracts)**: How the implemented system works internally (process lifecycle, storage/data models, API/CLI/MCP/Console surfaces, runtime sandboxes, network provider manager, built-in apps). Each file should own one cohesive subsystem.
- **Product Specs (PRDs)**: What the product must do and why, independent of implementation.
  - Keep a current-state gap section only while a spec is partially unimplemented; remove stale gap bullets as features ship.
  - Once a feature's separate technical design note is implemented, fold its durable design invariants into the architecture docs and delete the temporary technical note.
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
- [ ] Step 2: Audit for Contradictions, Drift, Overlap & Volatility
- [ ] Step 3: Ask User on Ambiguities (No Guessing Gate)
- [ ] Step 4: Consolidate, Re-split, Update & Prune
- [ ] Step 5: Sync Generated Docs & Update Cross-Links
- [ ] Step 6: Run Verification Suite
```

### Step 1: Discover Current Doc Graph & Code Contracts

1. Run `python3 .cursor/skills/flats-doc-sanitize/scripts/check_docs.py` to build the link reachability graph from `README.md` and Go code entrypoints (`//go:embed`, `.go` references), checking for broken relative links, unreachable (orphan) Markdown files, and 40-char hash literals.
2. Inspect the current index files and documentation tests to understand the active structure and automated invariants.

### Step 2: Audit for Contradictions, Drift, Overlap & Volatility

Compare the discovered documents against each other and the live codebase:
- **Contradictions**: Conflicting terminology, lifecycle steps, quotas, or authority rules across files.
- **Outdated claims**: Documented flags, routes, tools, or unimplemented-gap notes that no longer match the code.
- **Boundary overlap**: Multiple documents explaining the same subsystem without clear separation of concerns.
- **Volatile/redundant artifacts**: Shipped technical design notes, one-off review notes, or historical commit/hash receipts.

### Step 3: Ask User on Ambiguities (No Guessing Gate)

If any conflict, unimplemented spec requirement, or proposed document merge/split/deletion has more than one reasonable interpretation:
- Stop and formulate clear options via `AskQuestion` (or structured questions).
- Never guess product intent or silently drop an unimplemented PRD requirement.

### Step 4: Consolidate, Re-split, Update & Prune

1. Reorganize or re-split overlapping documents so each file has a single, clear responsibility within its domain directory.
2. Fold durable architecture knowledge from completed implementation notes or one-off specs into the canonical architecture document, then delete the redundant file.
3. Strip volatile historical receipts and update outdated descriptions to match the verified codebase.

### Step 5: Sync Generated Docs & Update Cross-Links

1. Regenerate any derived documentation via the repository's test update flags.
2. Update all relative links across Markdown indexes and any Go files referencing documentation paths.

### Step 6: Run Verification Suite

Verify both link graph integrity and repository test contracts before finishing:

```bash
python3 .cursor/skills/flats-doc-sanitize/scripts/check_docs.py
go test ./docs/... ./plugins/... ./internal/mcpx/...
```
