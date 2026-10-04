# Lifecycle regression gate

Run `scripts/lifecycle-gate.sh` from the integrated worktree.
`--prepare-only` runs just historical seed creation and runtime capability probing
against the legacy binary; its exit 0 is preparation success, never acceptance. Exit 0 means every
selected observable case passed; exit 1 means at least one failed. A partial run
(`--only case-id,...`) is diagnostic and never establishes full acceptance.
The launcher builds `cmd/flats`, a historical binary from the exact archived
pre-lifecycle base, and a test-only provider host. `FLATS_BIN` may supply an
integrated binary only when its clean source identity matches; `FLATS_LIFECYCLE_LEGACY_BIN` is rejected with exit 2.
Neither variable refers to a running server.

Each case starts a new server with its own disposable data directory and two
free loopback ports. The runner never kills listeners it did not start. It
disables Portal, selects Local, removes inherited `TS_AUTHKEY`, bypasses HTTP
proxy settings, and never opens user data, authkeys, tailnet identities, or
relay configuration. Data and logs remain in the printed temporary evidence
directory for review. Raw execution logs must stay outside the public repo.

## Current acceptance and proof boundaries

The full source-bound launcher builds the candidate, pinned historical source, real Manager adapter and tagged crash adapter from the clean worktree. It rejects an unverified legacy-binary override. Run without `FLATS_BIN`, `FLATS_LIFECYCLE_LEGACY_BIN`, `FLATS_URL`, authkeys, live-provider opt-ins or fault variables. The private integration handoff records exact commands, exits, source/tree/binary hashes and rendered receipts; past pass counts below are historical only.

The actual-binary lane checks Draft saves/conflicts/archive validation; private previews; pending HTTP/CLI/MCP requests and successful censused tools; separately provisioned operator authority, forged requests and retained-cookie revocation; frozen policy drift; health/data isolation; publish/rollback/restore; migrations; restart and four real SIGKILL publish phases. No SQL mutation substitutes for a crash.

The production-provider-manager lane links real core/API/CLI/MCP and Manager with disposable loopback backends. It checks host+flat grants, distinct Private Tailscale current/Draft URLs, independently controlled asynchronous Portal/Funnel readiness, approved Public Connecting followed by ready without reopening, in-use refusal and confirmed delete/rename/expiry teardown, Public-to-Private failures, current bytes and no fallback. Authentication uses real operator credential/session bootstrap and individual candidate decisions, not headers alone. This proves capability separation under the documented operator-secret/OS boundary, not identification of a human.

`internal/console/testdata/browser_test.mjs` runs the production frozen binary on disposable loopback ports at desktop/mobile sizes, recording exact identity, pending/approved/rejected requests, real typed 409s, clearly marked response fixtures, consent/focus, page errors and overflow. Inspect screenshots and receipt. It does not prove live providers or restore execution (covered by the runtime gate).

Nonblocking limitations: live Portal/Funnel ingress, ACME and tailnet ACLs are not exercised; real service managers and production tailnet-console identity are not exercised. Real-process crash injection remains publish-only; activation/rollback/restore swap/visibility have persistence/unit and execution coverage, not their own SIGKILL phases. Runtime module-scope env is unsupported and never counted as live-startup write proof. Operator-assisted corrupt restore-journal recovery is documented in the core contract. Multiple hosts must choose distinct Local/management ports and data directories. Secret-set changes are not frozen into approval policy; operator secret writes remain privileged and the next approved activation delivers the then-current values.

## Historical receipts

Everything below records earlier checkpoints and their scope at that time; it neither supersedes the current contract above nor certifies the current frozen tree.

## Baseline execution receipt

Verified `git ls-remote origin refs/heads/main` and local `git rev-parse HEAD` both
returned `29edc2a6e13e27867603a23ff5b1b5a8d1b84df0` before implementation. The proposed
core contract was read as a proposal, not implemented guarantees. The coordinator
confirmed that later current Draft revision/hash drift invalidates an earlier
pending approval, and authorized only test-harness loopback Public adapters.

Executed argv: `scripts/lifecycle-gate.sh`, exit **1**. At the pre-lifecycle base,
13 cases ran: archive validation **passed**, other 12 **failed**. The baseline
returned number 1 on the first save, rejected draft preview input, lacked the
publish endpoint, and immediately activated CLI/API/MCP deploy requests. These
are expected baseline failures, not acceptance passes. Several dependent cases
stop at missing publish and therefore do not establish their later oracles.
The temporary receipt is `evidence.json` in the printed gate directory; its
request/response traces and exact CLI argv/exits are local review artifacts.
The executed baseline binary SHA256 is
`5247e8d999ba4b395c3125f82dabba2384cb7857c49e673410d94d419583d065`;
the local JSON evidence SHA256 is
`d775c20ed1a28bf97b1d9207a788dc4d3b75a39f8b7f2740af90d64fafb3c159`.
`bash -n scripts/lifecycle-gate.sh`, Python AST parsing, and
`CGO_ENABLED=0 go build ./internal/lifecyclecheck/adapter` each completed with
exit 0. No generated bytecode was committed. The binary and test-host build
inside the full launcher also succeeded before the failing behavioral checks.

Integrated execution and the exact prerequisite commit list are still pending;
the coordinator requested this committed preparation handoff rather than an idle
worker awaiting adoption. All post-publish oracles remain unexecuted against the
candidate because no exact prerequisite adoption was granted in this Dispatch.

## Expanded preparation receipt

The independent harness expansion adds five acceptance populations, for 18 total
cases. These new acceptance cases have not run against a candidate. The final
`LifecycleNet` exports and actual provider manager are not adopted: root has not
granted an exact compiling prerequisite commit. No product source changed.
The next integration task must wire the test host to the real manager with its
injected backend interfaces, then test persisted host permission **and** per-flat
permission, canonical `tailscale-funnel`, no fallback, Draft Private-only routing,
unconfirmed teardown and surviving Private traffic. The retained PublicNet
loopback double does not establish any of those production-manager facts.

Executed `scripts/lifecycle-gate.sh --prepare-only`, initially exit **1**:
Local-only legacy mode refused a Public visibility request with 409 because
Portal was disabled. No exposure was enabled. Changed only the historical
pending-reference fixture to a real pending delete request, then reran the
corrected preparation once: exit **0**, both preparation cases passed. Evidence
was retained in the corrected run's temporary gate evidence directory as
`flats-lifecycle-gate.<run>/evidence.json`;
mixed seed manifest is sibling `prepare-mixed-history/mixed-seed.json`.
The initial failed receipt was retained separately as
`flats-lifecycle-gate.<failed-run>/evidence.json`.
Corrected receipt SHA256:
`ed0d620638b4a3637dd04af6d6e21a551202853d4db37f0bebe76ce4c0a6686e`.
Preparation seeded using the archived pre-lifecycle binary, never the candidate.

The real mixed seed served v3/hits=2, a disposable FILES value and a boolean
secret-use oracle; v2/v3 were actually deployed, v1/v4 were never deployed,
and a separate flat was never deployed. Both old private previews served their
expected content before stopping the legacy host. Migration assertions preserve
v2/v3 numbers, hashes and file paths; require undeployed bytes in draft revisions;
check pending delete ID/action/flat and visibility; accept a historical ephemeral
preview only if it retains the exact content or is absent with a 404 route.
They do not claim a pending visibility rewrite is covered by the delete fixture.
Migration/restart acceptance is still unexecuted, and final core preview
classification must be reconciled with its approved migration contract.

Runtime preparation returned `topEnv:"undefined"`: current JS modules cannot
access public `env.DB` or `env.FILES` at top level. This is an explicit capability
limit, not startup-data-isolation proof. The probe separately observed a durable
first-request initialization write and an additional write after real restart;
a first request is not labeled live startup. Candidate acceptance includes a
module evaluation failure and, when public top-level env is supported, DB/FILES
writes. It compares actual live DB state before and immediately after successful
activation, before a serving request, and rejects `data_impact:none` if that DB
changed. The final integrated DTO and failure-impact output still need execution
and reconciliation; no blanket `none` claim follows from copied health data.

Approved restore execution and current-live/visibility drift oracles are prepared
but unexecuted against the candidate. Existing Draft revision/hash and provider
drift, pending/reject/no-restore rollback, duplicate activation and restart
oracles remain. Successful restore preserves a read-only observed backup of the
latest pre-restore data and creates no new numbered code snapshot.

Verification argv: `bash -n scripts/lifecycle-gate.sh`; Python AST parsing of
both harness modules; `CGO_ENABLED=0 go build -o
/tmp/flats-lifecycle-gate-adapter-check ./internal/lifecyclecheck/adapter`.
Each exited **0**. The launcher built both actual binaries and the adapter.
Python uses `-B` to keep generated bytecode outside the worktree. No unchanged
full failing baseline was rerun. All raw logs/data remain outside the public repo.

Do not use either preparation or baseline receipt as approval to merge. Claude
review must inspect the final integrated tree, execute the full gate there, and
reconcile every case and proof boundary before a success claim.

## Claude correction checkpoint

Independent harness review of frozen `3ec41298` returned WITHHOLD. The integration
corrections add required-lane and source/binary identity checks to the machine
verdict. A legacy fallback cannot satisfy `production-provider-manager`; the
candidate adapter now uses actual `provider.Manager` through the exact core
`LifecycleNet`, with loopback Tailscale/Funnel and Portal backends. The added
host/per-flat permission matrix exercises available backends, canonical Funnel,
denial causes and no fallback. This remains loopback proof, not live internet/ACL.

The suite now defines 20 cases. Human authorization stays machine-readable OPEN
until the separately scoped operator-authority implementation and positive
authorized decision path are adopted and tested. Forged console-header probes,
HTTP/CLI decision route checks and an MCP behavioral census establish their own
boundaries; they do not automatically close that requirement.

Source identity comes from the harness root regardless of caller directory and
includes HEAD/tree/dirty state, Go version, legacy archive tree and harness file
hashes. Candidate and adapter must both report matching clean VCS build metadata
for acceptance. All used binaries are hashed, including the separate historical
provider fixture built against the archived legacy core. Preparation records only
used legacy executables and never hashes an unused candidate as its test subject.

Graceful stops reject nonzero exits, forced kills and panic/fatal log markers;
approval decisions reject HTTP 500. Expected failures assert their cause and
fresh-request recovery controls. Data-impact checks require an actual DTO field.
Healthy and failing health handlers write DB/FILES sentinels so isolation can be
observed in both paths. Unknown module-scope data support remains unproven.

The historical fixture adds Public and pending-visibility references, deployment
history comparison, flat-scoped file preservation, post-migration rollback,
decision of historical pending approvals and new publish bytes distinct from
legacy saved v4. Snapshot backup probes use the snapshots API and its filename
contract rather than treating every directory entry as SQLite.

The old SQL injection of `status=applying` is removed. Crash acceptance now
requires named test-only phases after claim/allocation/live switch, then checks
versions, deployment count, live pointer and served bytes across two restarts.
The exact core hook export and final data-impact/layout/restore scope are pending;
this case fails closed while that capability is absent. Final full integrated
execution and independent Claude product review remain pending.

## Integrated execution additions

Core behavior and tagged phase hooks have since been adopted through exact
committed grants, as has the frozen HTTP Draft/Publish/PendingApproval transport.
The suite currently defines 23 cases, including public v1→v2→rollback route
binding and an ordinary production-build check that fault environments are
ignored. The separate tagged binary pauses at four real core phases; SIGKILL
and two subsequent process restarts observe exactly-once recovery. All binary
identities, including tagged build metadata, are required for acceptance.

Operator-boundary proof now requires forged console headers to fail, separately
provisioned credential unlock to succeed, explicit candidate approval to succeed
and session revocation to fail afterward. The behavioral census still tests
HTTP/CLI/MCP surfaces. This proof assumes agents cannot read operator secrets or
server/browser memory; it is not identification of an individual human.

The actual legacy fixture retains snapshot hashes. Migration restores a real
legacy DB-only snapshot while checking current FILES and secrets remain intact.
New full-data restore checks both restored DB/FILES and the pre-restore backup.
Final CLI/MCP transport adoption, full integrated gate and rendered browser
results are reported in the private integration handoff, not inferred from these
prepared cases.

Dual-provider fault coverage uses selective Funnel/Portal serve and stop failures,
checks confirmed and unconfirmed routes through actual HTTP, preserves current
Private traffic and requires a fresh successful approval afterward.

Rejected decisions persist validated actor/time but perform no apply operation.
Their result_data must be empty/null; no execution/data-impact DTO is fabricated.
The census checks unchanged current pointer, bytes, visibility, providers and
version count. Approved/applied/failed executions still require actual typed DTOs.

## Final integrated validation

The integrated candidate passes all 23 actual-binary cases with both required
lanes, matching clean source/binary identities and the machine acceptance verdict.
The final full Go tests, scoped transport race tests, whole-tree vet and build
pass. Whole-tree race tests passed before the final transport follow-on; the
changed final transport packages were then checked with race detection again.
Owned expose/app/console race checks also pass. Historical failing receipts above
remain failed evidence and were not relabeled as successful acceptance.

Published visibility changes now remain pending even when a provider grant is
absent; permission/readiness checks occur at approved apply. Known provider
failures return typed user-correctable HTTP results. Rejections persist actor/time
and have no execution receipt. The final gate checks these contracts, current
bytes on both public/private paths through publish/rollback/restart, partial
provider cleanup, four real crash phases, migration and DB/FILES restore.

Actual rendered desktop/mobile console checks use disposable production Local
servers and real APIs, including consecutive source archive saves, revision/dirty
status, current-preview selection, tabs, Access/Operations, credential unlock,
separate publish confirmation, result/audit rendering, cancel focus and logout.
At 390 px, the checked page had no horizontal overflow. Evidence is retained
privately outside this repository; no operator credentials or raw logs are public.

The gate's human_approval verdict proves credential separation and explicit
candidate decisions under its stated trust boundary. It does not identify an
individual human or protect against operator-secret/server/browser/OS access.
Injected provider faults prove loopback behavior, not live Funnel/Portal internet
reachability or tailnet ACL enforcement. Independent Claude review is still
required before product acceptance; no PR, push or merge is authorized here.
