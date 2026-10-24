# Playbook: test parallelism and CI shards

The existing rules are authoritative: the comment above `TestMain` in
`internal/runtime/runtime_test.go` and `internal/apps/docs/runtime_test.go`,
the shard patterns in [`scripts/ci-test.sh`](../../../../scripts/ci-test.sh)
and the race lanes in [`scripts/ci-race.sh`](../../../../scripts/ci-race.sh).
[CI baseline](../../../../docs/gates.md#pull-request-baseline) explains them.

## Choose tests

- [ ] Add `t.Parallel` only to tests whose store, temp directory and other
  fixtures are owned by that test alone.
- [ ] Keep these serial: tests that mutate process-global state (`t.Setenv`,
  package variables, global hooks), latency or activation budget tests, rate
  limits that refill over time, and contention tests arranged with sleeps.
- [ ] A new or renamed budget or soak test must match its shard pattern in
  `scripts/ci-test.sh`; otherwise it runs in the general shard.

## Verify

- [ ] Passing under load does not prove that the intended overlap happened.
  For a contention test, show the overlap with evidence (logs, counters), not
  only a pass.
- [ ] Compare the complete list of test and subtest names before and after the
  change (`go test -json` `run` events) so no test silently stopped running.
- [ ] For durations, use the wall span of each test's own events and the
  package elapsed time. In parallel `go test -json` output a parent's elapsed
  field excludes work in its parallel subtests that finished after it.
- [ ] Run the touched packages with `-race` at least once.
