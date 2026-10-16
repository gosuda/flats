# Lifecycle regression gate

`scripts/lifecycle-gate.sh` is a black-box acceptance runner for the
lifecycle: Draft saves, version creation, deploy, rollback, restore,
visibility, providers, migration and crash recovery, driven through the real
HTTP API, CLI and MCP of actual binaries. Run it from any directory; it finds
the repository from its own path and builds everything into a new temporary
work directory:

- the candidate `cmd/flats` from the checkout, or a copy of `FLATS_BIN` when
  that is set;
- the historical pre-lifecycle binary, from `git archive` of the pinned
  commit `29edc2a6e13e27867603a23ff5b1b5a8d1b84df0` (`LEGACY_REVISION` in
  `internal/lifecyclecheck/gate.py`);
- a historical provider fixture linked against that archived core;
- the provider adapter (`internal/lifecyclecheck/adapter`), which links the
  real core, API, CLI, MCP and `provider.Manager` with disposable loopback
  Tailscale/Funnel and Portal backends;
- the same adapter built with the `lifecycle_testhooks` tag, which pauses at
  named core phases for crash injection. Ordinary builds ignore those phase
  variables, and a case checks that.

The script unsets `FLATS_PORTAL_E2E`, `TS_AUTHKEY`, `TS_AUTH_KEY`, `FLATS_URL`
and `FLATS_CONFIG`, so none of them affects a run. `FLATS_BIN` is always
copied and used as the candidate, but acceptance requires a clean checkout and
a candidate, provider adapter and tagged crash adapter whose build metadata
(`go version -m`) names the checkout's `HEAD` with `vcs.modified=false`. A
binary built from other source therefore runs the cases but is never accepted.
`FLATS_LIFECYCLE_LEGACY_BIN` is rejected: the historical binary always comes
from the pinned archive.

Each case starts a new server with its own disposable data directory and two
free loopback ports. The runner never kills listeners it did not start. It
disables Portal, selects Local, removes proxy settings from the server's
environment, and never opens user data, authkeys, tailnet identities, or relay
configuration. The run prints `PASS` or `FAIL` per case and writes
`evidence.json` (source and binary identities, lanes, per-case traces) to the
printed work directory, where data and logs remain for review. Raw execution
logs stay outside the public repository.

## Options and exit codes

- No options: the full suite, the only mode that can establish acceptance.
- `--only case-id,...`: the named cases, as a diagnostic. It never establishes
  acceptance.
- `--prepare-only`: the two preparation cases (`prepare-mixed-history`,
  `prepare-runtime-capability`), which seed history and probe runtime
  capabilities with the historical binary only. Exit 0 means preparation
  succeeded, never acceptance.

| Exit | Meaning |
|---|---|
| 0 | Every selected case passed. For a full run, also that acceptance holds. |
| 1 | A case failed, or a full run did not reach acceptance. |
| 2 | `FLATS_LIFECYCLE_LEGACY_BIN` was set. |

Acceptance requires all of: the full suite selected and passing, both required
lanes (`actual-binary-local` and `production-provider-manager`) passing,
source and binary identities bound as above, and `human_approval` equal to
`proven`. Every run starts with `human_approval` OPEN, and no case can prove
it: console decisions rely only on CSRF headers, and the console has no
separate operator credential, so any local process can forge a decision and
the gate cannot prove that a human made it. **A full run therefore exits 1
even when every case passes**; read the per-case results and the other
acceptance fields in `evidence.json`. The criterion stays until a new operator
boundary exists or an explicit decision changes it. The approval-boundary
census still checks that agent HTTP routes, CLI commands and MCP tools cannot
decide; the gate's own console decisions send the same headers a local process
could forge.

## Cases

The full suite has 28 cases. Eighteen run the candidate binary
(`actual-binary-local` lane):

`draft-save-conflict`, `archive-validation`, `private-draft-traffic`,
`publish-human-idempotency-restart`, `frozen-revision-policy`,
`cli-console-pending`, `mcp-pending`, `api-upload-deploy-pending`,
`approval-boundary-census`, `version-quota-explicit-deletion`,
`production-fault-hooks-absent`, `actual-binary-provider-authority`,
`runtime-health-rollback-data`, `historical-migration`,
`historical-mixed-migration`, `runtime-initialization-data`,
`rollback-approved-restore-data`, `frozen-current-live`.

Ten run the provider adapter (`production-provider-manager` lane; a legacy
public fallback fails the lane), the last one as the tagged crash adapter:

`provider-permission-failure`, `visibility-all-surfaces-teardown`,
`public-publish-rollback-route-binding`, `partial-provider-failures`,
`frozen-visibility`, `provider-host-flat-permission-matrix`,
`async-public-readiness`, `private-tailscale-distinct-provider`,
`provider-revocation-rename-delete-expiry`, `restart-claimed-approval`.

The candidate lane checks Draft saves, conflicts and archive validation;
private previews; pending HTTP, CLI and MCP requests and a census of every MCP
tool; frozen Draft revision, policy and live-version drift (each fails as
`stale_approval`); health/data isolation; version creation, deploy,
rollback and restore; the version quota and explicit deletion; migration from
the historical binary; and restart. Healthy and failing health handlers write
DB/FILES sentinels, so isolation is observed on both paths. Data-impact checks
compare live DB state before and right after a start (before any serving
request) and reject `data_impact` `none` if it changed. Migration restores a real legacy DB-only
snapshot while checking that current FILES and secrets stay intact; a
full-data restore checks both the restored DB/FILES and the pre-restore
backup.

The provider lane checks host and per-flat grants, distinct Private Tailscale
current/Draft URLs, independently controlled asynchronous Portal/Funnel
readiness, approved Public Connecting followed by ready without reopening,
in-use refusal and confirmed delete/rename/expiry teardown, Public-to-Private
failures, frozen visibility drift, current bytes and no fallback. Selective Funnel/Portal serve and
stop failures check confirmed and unconfirmed routes through actual HTTP,
preserve current Private traffic and require a fresh successful approval
afterwards. Decisions use console routes, one candidate decision at a time.

`restart-claimed-approval` kills the tagged crash adapter with SIGKILL at four
real core phases: `before_health`, `after_health` and
`after_live_before_finalize` during an approved deploy of version 1, and
`after_allocation_before_live` during approved version creation. After each
kill, two restarts must resume the claimed approval exactly once: one version,
and for a deploy one deployment with the right live pointer and served bytes.
No SQL mutation substitutes for a crash.

Rejected decisions persist actor and time but perform no apply operation.
Their `result_data` must be empty or null; no execution or data-impact DTO is
fabricated. The census checks an unchanged current pointer, bytes, visibility,
providers and version count. Approved, applied and failed executions require
actual typed DTOs, and a failure must carry its expected `failure_code`.

Graceful stops reject nonzero exits, forced kills and panic/fatal log markers;
approval decisions reject HTTP 500. Expected failures assert their cause and
fresh-request recovery. Source identity comes from the harness root regardless
of the caller's directory and includes `HEAD`, tree, dirty state, Go version,
the legacy archive tree and harness file hashes; every binary used is hashed.

## Console checks

The gate does not render the console. Console UI regressions run as Go tests
(`TestConsoleUI`, `TestConsoleHomeFeedback`, `TestConsoleContentLabels`) that
run `internal/console/testdata/ui_test.mjs`, `home_test.mjs` and
`content_labels_test.mjs` with Node against a fake DOM and a stubbed fetch;
they skip when `node` is not installed. They do not exercise a real browser,
live providers or restore execution.

## Limits

Live Portal/Funnel ingress, ACME and tailnet ACLs are not exercised; injected
provider faults prove loopback behavior only. Real service managers and
production tailnet-console identity are not exercised. Real-process crash
injection covers version creation and deploy only; activation/rollback/restore
swap/visibility have persistence/unit and execution coverage, not their own
SIGKILL phases. Runtime module-scope `env` is unsupported (JS modules cannot
reach `env.DB` or `env.FILES` at top level) and never counts as live-startup
write proof. Operator-assisted corrupt restore-journal recovery is documented
in the core contract. Multiple hosts must choose distinct Local/management
ports and data directories. Secret-set changes are not frozen into approval
policy; operator secret writes remain privileged and the next approved
activation delivers the then-current values.

## Recorded runs

These runs record earlier checkpoints. They do not certify the current tree.

- **Pre-lifecycle base.** The gate, run against the historical base itself,
  exited 1 with 13 cases at the time: archive validation passed and the other
  12 failed as expected. The base numbered the first save 1, rejected Draft
  preview input, lacked the publish endpoint and activated CLI/API/MCP deploy
  requests immediately. Several dependent cases stopped at the missing endpoint
  and did not establish their later checks.
- **Preparation.** The first `--prepare-only` run exited 1: Local-only legacy
  mode refused a Public visibility request with 409 because Portal was
  disabled, and no exposure was enabled. With the historical pending fixture
  changed to a real pending delete request, the rerun exited 0 with both
  preparation cases passing. The mixed seed served v3 with two hits, a FILES
  value and a secret-use check; v2/v3 were deployed, v1/v4 and a separate flat
  never were, and both old private previews served their expected content.
  The runtime probe reported `topEnv:"undefined"`.
- **Integrated candidate.** All 23 cases defined at the time passed in both
  required lanes, with matching clean source/binary identities and the machine
  acceptance verdict, together with full Go tests, scoped race tests, vet and
  build. That verdict relied on an operator credential that has since been
  removed; `human_approval` is OPEN for the current console. Rendered
  desktop/mobile checks of the console of that time, on disposable Local
  servers with real APIs, found no horizontal overflow at 390 px; that console
  has since been replaced.
