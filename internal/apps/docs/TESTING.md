# Testing Flats Docs

Run commands from the repository root unless shown otherwise. Use disposable
output locations: `$BIN` is a newly built test binary, `$EVIDENCE_DIR` stores
local screenshots/logs, and `$FLATS_PLAYWRIGHT_MODULE` points to an installed
Playwright ESM entry point. Browser tests need Chromium installed. Tests use
private temporary data and free loopback ports (the product test excludes
7878–7891); `scripts/container-smoke.sh` defaults to ports 17878/17879. Do not point test commands at an existing service's data.

## Test layers

* Node unit tests cover merge/conflict recovery, protocol limits and provider behavior.
* Go tests execute the committed real bundle in the Flats QuickJS worker, covering
  persistence, concurrent edits, compaction, activation, private recovery and budgets.
* `FLATS_FULL_SOAK=1` enables long ownership and document soaks. Routine counts
  still exercise repeated calls; runtime near-cap tests use 120 by default and
  600 HTTP/catch-all/WebSocket calls or 900 live-state calls in full mode.
  Docs full counts are 3,000 edits at 300 KiB, 200 near-1 MiB ASCII/Korean edits
  each and 200 public 1 MiB reads.
* The opt-in browser test uses separate Chromium contexts for collaboration,
  private conflict recovery, read-only/mobile views and synthetic composition.
* The real-binary product test covers console draft preview, publication,
  collaboration, rollback and restart with disposable host state.
* The container smoke test checks built-in docs deployment, persistence across
  restart and hardened container operation; it requires a Docker host.

```sh
npm ci --prefix internal/apps/docs/_web
npm test --prefix internal/apps/docs/_web
npm run fixtures --prefix internal/apps/docs/_web
npm run build --prefix internal/apps/docs/_web
go test ./...
go vet ./...
gofmt -l .
go mod tidy
git diff --exit-code -- go.mod go.sum
go test -race ./internal/runtime -run 'TestConcurrentFiles|TestWASIRejectsJSHostABI|ByteBudget'
go test -race ./internal/core ./internal/app ./internal/mcpx
FLATS_FULL_SOAK=1 go test ./internal/runtime ./internal/qjs -run 'NearCap|Ownership|CycleCollection' -count=1 -timeout 30m -v
FLATS_FULL_SOAK=1 go test ./internal/apps/docs -run Soak -count=1 -timeout 30m -v
FLATS_DOCS_BROWSER_TEST=1 FLATS_DOCS_BROWSER_EVIDENCE="$EVIDENCE_DIR/browser" \
  go test ./internal/apps/docs -run 'TestBrowserCollaboration|TestBuildInputs' -count=1 -v
go build -o "$BIN" ./cmd/flats
node internal/apps/docs/testdata/product_test.mjs "$BIN" "$EVIDENCE_DIR/product"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$EVIDENCE_DIR/flats-linux-amd64" ./cmd/flats
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$EVIDENCE_DIR/flats-linux-arm64" ./cmd/flats
FLATS_RELEASE_TARGETS="linux/amd64 linux/arm64" scripts/build-release.sh v0.0.0-ci dist
docker build -t flats:ci .
scripts/container-smoke.sh flats:ci v0.0.0-ci
```

Export `FLATS_PLAYWRIGHT_MODULE` before browser/product commands. Remove only
binaries and temporary data created by your own test run when finished.

## Reproducible build inputs

`dist/BUILD-INPUTS.sha256` hashes sorted `_web/src/**` (including unit tests),
package metadata, the build script and all other dist outputs. `TestBuildInputs`
recomputes it without Node, so source or test changes require rebuilding dist.
The generated fixture bytes are real Yjs updates consumed by Go tests.
Verify a byte-identical rebuild:

```sh
mkdir -p "$EVIDENCE_DIR"
shasum -a 256 internal/apps/docs/dist/* > "$EVIDENCE_DIR/build.sha256"
npm run build --prefix internal/apps/docs/_web
shasum -a 256 -c "$EVIDENCE_DIR/build.sha256"
go test ./internal/apps/docs -run TestBuildInputs -count=1
```

## Reference measurements

Measured on an Apple M1 Pro; these are examples, not performance guarantees.
The tests enforce their own budgets. Survival tests do not measure peak heap/RSS.

| Workload | Reference measurement |
|---|---:|
| 200k live objects, 192 HTTP requests | p50 0.40 ms, mean 2.15 ms, max 57.5 ms |
| 3,000 single-character edits, 300 KiB document | 146 s |
| 200 near-1 MiB ASCII / Korean edits | 25 / 11 s |
| 200 public 1 MiB reads | 81 s |
| Maximum 1 MiB rewrite activation, ASCII / Korean | 1.67 / 1.05 s |

Cycle collection runs on every call once linear memory reaches 3/4 of the VM
cap; otherwise traffic, growth and a 32-call backstop trigger it. Linear memory
never shrinks and cannot establish live usage. Guest-caught OOM/InternalError
has no persistent signal in the current QuickJS binding after the exception is
cleared; it cannot reliably cause targeted collection or VM replacement.
Uncaught engine errors and failed collections do discard the VM.
