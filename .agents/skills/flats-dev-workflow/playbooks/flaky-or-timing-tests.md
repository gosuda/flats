# Playbook: flaky or timing-sensitive tests

Use with the normal stages. A flaky test is a defect report: find the cause
before changing the test.

## Reproduce

- [ ] Record the failing test, package, commit, OS and CI job.
- [ ] Linux-only races (for example `SQLITE_BUSY`) may not reproduce on macOS.
  Reproduce in a Linux container with a fixed CPU count, the module cache
  mounted read-only and `GOPROXY=off`, so every run uses the same inputs.
- [ ] Build the test binary with `go test -c` and run it from the package
  directory: tests read `testdata/` by relative path.
- [ ] To compare two commits, interleave their runs in the same container.
  Separate runs on a shared machine are too noisy to compare.
- [ ] Record machine load (`uptime`) with every local timing failure. Check for
  other test or build processes first, and never stop a process you did not
  start.

## Diagnose

- [ ] A budget failure can be a real regression. Do not raise a budget until
  you know what got slower and why.
- [ ] A passing `go test` run does not print `t.Logf` output, so CI margins are
  invisible. Before calling a failure an outlier, log every sample on CI (a
  draft PR runs the CI workflow) and look at the distribution.
- [ ] For parallel runs, read `go test -json` pause/cont events to see what ran
  alongside the failing test before blaming parallelism.
- [ ] When timing matters locally, run the CI shards one at a time
  (`scripts/ci-test.sh <shard>`) or `go test -p 1 ./...`. A local
  `go test ./...` runs the runtime and docs packages at once.

## Fix

- [ ] When disk latency is not what a test measures, the docs budget shard
  already moves test data to tmpfs. Follow the same pattern: set both `TMPDIR`
  and `GOTMPDIR` (`t.TempDir` prefers `GOTMPDIR`), and only for the test binary
  through `go test -exec`, because tmpfs may be mounted noexec. See the
  `docs-budget` case in [`scripts/ci-test.sh`](../../../../scripts/ci-test.sh).
- [ ] Keep elapsed-time tests serial, following the rule above each package's
  `TestMain` (see [test-parallelism](test-parallelism.md)).
- [ ] Test the fix with the same reproduction: the same container, the same
  number of runs, and both commits interleaved.

Related: [CI baseline](../../../../docs/gates.md#pull-request-baseline).
