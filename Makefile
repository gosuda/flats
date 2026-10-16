# Documentation maintenance. `make help` lists the targets; the
# flats-doc-sanitize skill runs them in this order: docs-audit before
# editing, docs-generate after editing docs/agent/, docs-verify at the end.

GO ?= go

# Tests that pin documentation content: generated references, README and
# skill sentences, MCP guide topics, and runtime reference examples.
DOC_TEST_PACKAGES = ./docs/... ./plugins/... ./internal/mcpx/... ./internal/doccheck/...
DOC_RUNTIME_TESTS = ^(TestRuntimeReference|TestTypesMatch)

.PHONY: help docs-check docs-audit docs-generate docs-test docs-verify

help: ## List the targets.
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

docs-check: ## Fail on broken Markdown links/anchors and relative links in docs/agent/ (CI runs the same check).
	$(GO) run ./internal/doccheck/cmd/doccheck

docs-audit: ## docs-check plus review lists: unreachable docs, colocated notes, hash literals.
	$(GO) run ./internal/doccheck/cmd/doccheck -audit

docs-generate: ## Regenerate the references built from docs/agent/ (never edit them by hand).
	$(GO) test ./docs -run TestGeneratedReferences -update

docs-test: ## Run the tests that pin documentation content.
	$(GO) test $(DOC_TEST_PACKAGES)
	$(GO) test ./internal/runtime -run '$(DOC_RUNTIME_TESTS)'

docs-verify: docs-check ## Full check after a docs change: links, every package compiles, doc tests.
	$(GO) test -run '^$$' ./...
	$(MAKE) docs-test
