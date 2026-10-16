---
name: flats-doc-sanitize
description: Sanitize, restructure, deduplicate, and verify documentation in the Flats repository. Use when the user asks to sanitize docs, clean up outdated or contradictory documentation, reorganize documentation directories, merge duplicate specs, or prune volatile verification receipts in the flats repo.
---

# Flats Document Sanitize

Maintain a contradiction-free, single-source-of-truth documentation tree by dynamically discovering the current structure, verifying claims against the codebase, asking the user instead of guessing on ambiguities, and removing volatile or redundant content.

## Core Principles

1. **Resolve contradictions**: Cross-check claims across documents and against the actual codebase. Eliminate conflicting statements so every behavior is defined consistently.
2. **Update outdated content**: Sync operational, architecture, and agent-reference documentation with current implementation (CLI commands/flags, MCP tools, HTTP routes, storage layout, limits, and UI behavior).
3. **Never guess on ambiguity**: When documents conflict with each other or with code and the intended product policy is unclear, or when a file/section deletion or structural split is ambiguous, stop and ask the user before editing (use your host's interactive question tool if it has one, or numbered questions in plain text otherwise). Batch questions into a single round and skip asking about gaps that are already accurately documented under a spec's `Current state` or `Open questions`.
4. **Deduplicate (Single Source of Truth)**: Each concept, rule, or state machine belongs in one canonical document; other documents should summarize briefly and link to the canonical source.
   - **Exception**: Preserve intentional projections and test-enforced duplicated safety/discovery statements (e.g., generated references assembled from agent topics, or exact sentences asserted across multiple docs by `*reference_test.go`).
5. **Re-split blurry boundaries**: When multiple documents have tangled or overlapping scopes, collect them and re-split cleanly along domain boundaries (e.g., turning N overlapping files into M focused domain documents).
6. **Delete low-value or volatile content**: Remove information that lacks ongoing maintenance value:
   - Ephemeral worker/branch handoff notes, `git merge-base` receipts, and temporary scratch hashes. (Preserve normative protocol test vectors such as RFC SHA-1 vectors, and ask the user before removing durable milestone/gate acceptance evidence that an index explicitly names.)
   - One-off review or polish artifacts once their change has shipped.
   - Separate implementation design notes (`*-TECH.md`) for already-shipped features **after** folding their durable architectural invariants into the canonical architecture docs, updating the spec index in the same change, and confirming with the user when deleting standalone design files.

## Handling Spec vs. Implementation Divergence

When the codebase does not faithfully or completely implement a specification (PRD), **never silently downgrade the spec to match incomplete or buggy code**. Follow this protocol:

1. **Do not weaken normative specs without user confirmation**:
   - If code is missing a requirement or deviates from a spec, do not delete the requirement from the spec or document a code bug as intended design.
2. **Classify the gap with the user**:
   - Skip gaps already accurately recorded in the spec's `Current state` or `Open questions`.
   - For newly discovered or changed discrepancies, present what the spec requires vs. what the code actually does and ask the user whether it is:
     - **(a) Unimplemented / planned work** (valid target requirement not built yet),
     - **(b) An implementation bug / omission** (code failed to follow the spec),
     - **(c) An intentional design change** (the spec is outdated and should be updated to match the code), or
     - **(d) Still undecided** (belongs in `Open questions`).
3. **Apply the separation rule by document role**:
   - **If (a) Unimplemented or (b) Code bug/omission**:
     - **Product Specs**: Keep the status line at the top, preserve the target requirement in the spec body, and record the exact implementation gap under `Current state`.
     - **Operational, Architecture & Agent Docs**: Describe only what the running code actually supports today so users and agents are not misled into calling unimplemented behavior.
     - **Implementation Gap Report**: Surface a dedicated list of spec-vs-code omissions in the final sanitize summary.
   - **If (c) Intentional design change (confirmed by user)**:
     - Update the spec body first ("a change that needs to break one updates the page first"), and remove the corresponding stale bullets from `Current state` and `Open questions`.
   - **If (d) Undecided**:
     - Record the open decision under the spec's `Open questions` section.

## Structural & Domain Principles

Organize documentation by **audience and purpose** into clear domain directories, reachable through the master index (`docs/README.md`):

- **Operations (operator & user guides)**: How operators and users install, configure, network, containerize, and connect agents. Must not duplicate internal state machine proofs.
- **Architecture (maintainer design contracts)**: How the implemented system works internally. Each file should own one cohesive subsystem.
- **Product Specs (PRDs)**: What the product must do and why, independent of implementation. Follow the conventions in `docs/spec/README.md` (status line, `Current state`, `Open questions`).
- **Verification (repeatable gate guides)**: Repeatable verification procedures, CI shard rules, and acceptance criteria. Keep *how to run and what must pass*; prune ephemeral worker handoff logs while asking the user before deleting indexed gate evidence.
- **Generated & Embedded Agent Docs (`docs/agent/`)**: Single source of truth for MCP `guide` topics (`TopicOrder` in `docs/runtime_reference.go`, routed from `topic.index`), `refusals.md` (`## <category>` sections), and generated references. Avoid relative Markdown links inside `docs/agent/` that break when served via MCP or `llms.txt`.

## Repository Invariants (Discover Dynamically)

Before moving, renaming, or deleting any file, discover programmatic contracts from the codebase:
- **Protected blocks**: Never modify `<!-- PROTECTED ... -->` blocks in Markdown files (including embedded asset paths such as logos).
- **Generated references**: Search for update flags (`grep -rn 'flag.Bool("update"' --include='*_test.go' .`) and run the generator (`make docs-generate` for the references built from `docs/agent/`) after editing source topics. Never hand-edit generated files.
- **Code & test consumers of paths and prose**: Search across all source/build/test files (`.go`, `.mjs`, `.js`, `.sh`, `.yml`, `.json`; `grep -rn` or `rg`) for any path you plan to move or delete (Markdown files, example scripts/manifests, plugin manifests, assets) and check `*reference_test.go` for pinned sentences. Note that `/docs/*.md` routes in `internal/mcpx/llms.go` are HTTP endpoints served by the host, distinct from repository file paths.
- **No review screenshots in Git**: Never keep or commit verification screenshots in the repository.

## Sanitize Workflow

Copy this checklist (or mirror it in your task-tracking tool, if you have one) and track progress during execution:

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

1. Run `make docs-audit`. It parses every repository Markdown file with a CommonMark/GFM parser (`internal/doccheck`) and reports:
   - **Errors** (CI runs `make docs-check`, and `TestRepositoryDocs` runs the same check under `go test`): broken relative links and `#anchor` fragments, case mismatches, links to files Git does not ship or outside the repository, and relative links inside `docs/agent/`.
   - **Review lists**: docs nothing reaches from `README.md` or code (`//go:embed`, quoted `.md` paths), colocated `internal/`, `examples/` and `cmd/` notes, and lines with 40/64-character hex literals.
   Run `make help` for every documentation target.
2. Inspect `docs/README.md`, `docs/spec/README.md`, and documentation tests to understand the active structure and automated invariants.

### Step 2: Audit for Contradictions, Spec-vs-Code Gaps, Drift, Overlap & Volatility

Compare the discovered documents against each other and the live codebase:
- **Contradictions**: Conflicting terminology, lifecycle steps, quotas, or authority rules across files.
- **Spec-vs-Code Gaps**: Requirements in product specs that are missing, partial, or differently implemented in code.
- **Outdated claims**: Documented flags, routes, tools, or `Current state` notes that no longer match the code.
- **Boundary overlap**: Multiple documents explaining the same subsystem without clear separation of concerns.
- **Volatile/redundant artifacts**: Shipped technical design notes, one-off review notes, or ephemeral commit/hash receipts.

### Step 3: Ask User on Ambiguities & Spec-vs-Code Gaps (No Guessing Gate)

If any conflict, unimplemented/partial spec requirement, or proposed document merge/split/deletion (including deleting `*-TECH.md` or gate evidence files) has more than one reasonable interpretation:
- Batch the questions and ask the user before editing.
- Never guess product intent, silently drop an unimplemented PRD requirement, or enshrine a code bug as documentation.

### Step 4: Consolidate, Re-split, Update & Prune

1. Reorganize or re-split overlapping documents so each file has a single, clear responsibility within its domain directory.
2. Fold durable architecture knowledge from completed implementation notes or one-off specs into the canonical architecture document, update index tables, and delete the redundant file (when confirmed).
3. Update operational/architecture/agent docs to match verified code, while keeping unimplemented spec requirements in `docs/spec/` with accurate `Status`, `Current state`, and `Open questions` sections.
4. Strip ephemeral historical receipts.

### Step 5: Sync Generated Docs & Update Cross-Links

1. Regenerate any derived documentation (`make docs-generate`).
2. Update all relative links and anchor fragments across Markdown indexes and any source/test files referencing moved paths.

### Step 6: Run Verification Suite & Report Implementation Gaps

1. Verify link/anchor graph integrity and repository test contracts:
   ```bash
   make docs-verify   # docs-check, compile every package, then docs-test
   make docs-audit    # review lists for the final pass
   ```
   **Pass criteria**:
   - `make docs-verify` exits 0: no broken links, anchors, case mismatches, links to unshipped or outside-repository files, or relative links inside `docs/agent/`; every package compiles; the documentation tests pass.
   - Every unreachable doc that `make docs-audit` lists is linked from the appropriate index, deleted (if obsolete and confirmed), or accounted for as an intentional colocated package/command/example note.
   - Every hash literal it lists has been reviewed: ephemeral worker receipts removed, valid test vectors and pins preserved.
2. In the final summary, explicitly list any **Spec-vs-Implementation Gaps** discovered during the audit so the user can decide on follow-up code fixes.
