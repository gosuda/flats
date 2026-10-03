# Lifecycle regression gate

Run `scripts/lifecycle-gate.sh` from the integrated worktree.
`--prepare-only` runs just historical seed creation and runtime capability probing
against the legacy binary; its exit 0 is preparation success, never acceptance. Exit 0 means every
selected observable case passed; exit 1 means at least one failed. A partial run
(`--only case-id,...`) is diagnostic and never establishes full acceptance.
The launcher builds `cmd/flats`, a historical binary from the exact archived
pre-lifecycle base, and a test-only provider host. `FLATS_BIN` may supply an
integrated binary; `FLATS_LIFECYCLE_LEGACY_BIN` may supply the historical binary.
Neither variable refers to a running server.

Each case starts a new server with its own disposable data directory and two
free loopback ports. The runner never kills listeners it did not start. It
disables Portal, selects Local, removes inherited `TS_AUTHKEY`, bypasses HTTP
proxy settings, and never opens user data, authkeys, tailnet identities, or
relay configuration. Data and logs remain in the printed temporary evidence
directory for review. Raw execution logs must stay outside the public repo.

## Observable acceptance matrix

| Requirement | Case / observable oracle | Proof boundary |
|---|---|---|
| Repeated saves create no published v1; conflicting writer rejected | `draft-save-conflict`: three validated tar.gz uploads return number 0, revision 3, no history/live pointer; stale expected revision returns 409 and keeps revision | Actual binary, HTTP |
| Unsafe archive validation changes nothing | `archive-validation`: traversal upload rejected 422, prior flat view preserved | Actual binary, HTTP archive validation |
| Draft preview before publication is Private | `private-draft-traffic`: preview serves the exact uploaded marker over loopback, no live version/history/public URL | Actual binary, Local traffic |
| All publish entrypoints await human approval | `api-upload-deploy-pending`, `cli-console-pending`, `mcp-pending`: upload-and-deploy and explicit deploy produce pending approval, API 202/CLI 3/MCP pending, live 0/history empty; console request also pending | Actual integrated binary, CLI subprocess and JSON-RPC MCP |
| Agent cannot approve its request | `publish-human-idempotency-restart`: `/api/.../approve` unavailable; `mcp-pending`: initialized tool inventory has no approve capability | Existing console/API trust boundary; this does not establish owner auth |
| Explicit approval first creates v1/Published/Private | `publish-human-idempotency-restart`: console decision simulation approves once; published history exactly [1], live 1, publication Published; GET serves candidate marker | Actual binary, HTTP and traffic |
| Duplicate pending requests, concurrent decisions, retries do not activate twice | `publish-human-idempotency-restart`: same approval id; concurrent decisions plus repeat approve; one version and one deployment before/after restart | Actual binary, persisted observable history |
| Approval rejects candidate/current policy drift | `frozen-revision-policy`: save newer revision then approve old request => failed/no version/live; provider policy change likewise fails | Actual binary, HTTP |
| Claimed approval resumes once after restart | `restart-claimed-approval`: after stopping the disposable host, test-only SQLite update models crash after persisted pending→applying claim; actual startup must activate bytes, with one deployment across two starts | Deterministic crash boundary; real binary restore/activation |
| v1 stays serving while Draft changes | `publish-human-idempotency-restart` and provider lane: GET remains old marker after new upload; private preview serves new marker | Actual Local traffic and separately labeled simulated public traffic |
| Explicit provider permission; Tailscale never implicitly authorizes Funnel | `provider-permission-failure`: initial provider set empty/local; agent cannot grant; grant tailscale excludes tailscale-funnel; neither permission changes visibility; unavailable/unpermitted Public approve fails, keeps Published v1 and Local traffic | Actual binary, disabled external infrastructure; no real Tailscale connection claim |
| Both visibility directions through API/console/CLI/MCP await approval | `visibility-all-surfaces-teardown`: each surface cycles Private→Public→Private, asserting old visibility before decision and unchanged v1 numbering | Deterministic loopback provider host uses product API/core/CLI/MCP |
| Draft never on Public path | Provider lane: Public current GET still v1, preview GET is draft, simulated public preview host returns 404 | Deterministic route registration and actual loopback traffic, not internet Funnel proof |
| Public→Private teardown failure is not success | Provider lane: injected Stop failure leaves GET answering, approval fails, visibility remains Public; clear fault and reapprove => route 404, Private current still answers | Deterministic provider fault, actual HTTP serving oracle |
| Network failure separate from publication; no fallback permission | Local provider-failure and provider lane: connection/setup failure leaves Published/current version and visibility unchanged, no simulated public route opens | Fault injection and real Local traffic; external readiness remains separately unproved |
| Failed preparation preserves Draft/history/live data | `runtime-health-rollback-data`: real uploaded JS health handler writes sentinel then returns 503 on isolated check; before decision no execution; failure leaves live hits=1/version=1/history=[1] | Actual integrated binary and real runtime/SQLite, not fake DOM |
| Code rollback preserves number/data; restore separately approved | Runtime case: publish v2, increment data; rollback pending then approved to v1 retains hits=2 and history [1,2]; restore_data request freezes true, leaves hits unchanged until decision, rejection keeps data | Actual binary/runtime; restore execution coverage must be recorded separately |
| Current live/visibility drift invalidates pending publish | `frozen-current-live`, `frozen-visibility`: change live by approved rollback or visibility by approved Public transition, then old publish fails without new number or candidate traffic | Actual binary/loopback provider lane respectively |
| Approved explicit restore executes frozen data choice | `rollback-approved-restore-data`: snapshot at hits=1 before v2, pending restore keeps v2 and allows hits=3, approve restores v1/hits=1, preserved backup contains hits=3, retry/restart keep [1,2] | Actual binary/runtime and read-only disposable SQLite oracle |
| Runtime initialization distinct from health/first request | `runtime-initialization-data`: module capability/throw probe, unchanged live data pending, startup failure does not publish; success compares DB before/after live startup before serving request with impact output | Public runtime JS only; unsupported top-level env explicitly recorded |
| Mixed deployed and undeployed historical migration | `historical-mixed-migration`: real legacy v2/v3 deployment, saved v1/v4, separate never-deployed flat, DB hits/file/secret, private version previews and pending delete; preserve identity/hash/path/data, classify previews, repeated restart, next publish v4 | Actual historical binary seed; migrated acceptance pending |
| Historical never-deployed migration preserves files and restarts | `historical-migration`: baseline binary saves historical v1/v2 without deploy; integrated server migrates history empty/Unpublished; both file hashes survive; preview serves latest; restart stable; approved first publish is v1 with legacy bytes | Real historical binary seed and integrated binary migration/traffic |

The test adapter is a separate executable under `internal/lifecyclecheck/adapter`.
Its fault endpoint exists only there and does not approve actions or write product
state. It links the real product core, HTTP API, MCP server, CLI and runtime;
only the external provider is doubled. The current adapter uses the historical
`PublicNet` fallback, not the production network manager. It must be reconciled
to the final optional `LifecycleNet` interface before claiming integrated provider
acceptance. Portal is represented by a loopback route, not a real relay. No result in
this lane proves Funnel internet access, provider identity/ACL behavior, or remote
teardown. Private remains existing loopback/tailnet ACL policy; owner-only auth is
deferred. Console decisions are simulated with the existing same-origin headers,
which agents can forge under the intentionally retained admin trust boundary.

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
corrected preparation once: exit **0**, both preparation cases passed. Evidence:
`/var/folders/r_/5jr0xg0s7wg3xmd4ws3h5wlw0000gn/T/flats-lifecycle-gate.NN9dnI/evidence.json`;
mixed seed manifest is sibling `prepare-mixed-history/mixed-seed.json`.
The initial failed receipt remains at
`/var/folders/r_/5jr0xg0s7wg3xmd4ws3h5wlw0000gn/T/flats-lifecycle-gate.p6555P/evidence.json`.
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
