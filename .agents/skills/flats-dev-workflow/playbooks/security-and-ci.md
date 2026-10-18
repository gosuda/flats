# Playbook: CodeQL findings and GitHub Actions workflows

## CodeQL

- [ ] Treat every new CodeQL alert on the PR as a blocker, and check alerts on
  the final PR head, not an earlier one.
- [ ] Before changing code, read the alert's data flow (the SARIF `codeFlows`
  of the analysis, from the code scanning API). A guard on the wrong value does
  not clear an alert.
- [ ] For paths built under a configured directory, keep the existing sanitizer
  form `filepath.Clean("/" + p)` (`cleanDataDir` in `internal/config/config.go`);
  static analysis recognizes it.
- [ ] Dismissing an alert is a maintainer decision with a recorded rationale,
  as in [WebSocket handshake](../../../../docs/internal/websocket-handshake.md).
  Do not suppress rules or exclude files.

## Workflows (`.github/workflows/`)

- [ ] A reusable workflow called after a failing job needs explicit status
  guards (`!cancelled()` plus the needed `needs.<job>.result`) on its nested
  jobs, or its fallback path never runs. See `commit-images.yml`.
- [ ] Keep publication permissions (`packages: write`, `contents: write`) in
  the calling workflows. Do not grant them inside a reusable workflow that
  pull-request callers also use.
- [ ] A workflow that checks out older source uses restore-only cache steps
  (`actions/cache/restore`) directly, as `release.yml` does. CodeQL reports a
  cache step that could save from that source as cache poisoning, even when
  a condition disables the save, so the shared `setup-go` action does not fit.
- [ ] Commit images must cover every introduced main commit, including
  intermediate commits; see [Commit images](../../../../docs/release.md#commit-images).

## CI cost

- [ ] Compare PR and main runs separately: only pushes to main save caches;
  pull requests restore them (`.github/actions/setup-go`).
- [ ] Report elapsed workflow time and summed runner time as separate numbers.
  A cached Go test duration does not estimate an uncached run.
