# Agent self-update technical design

## Architecture

Use a shared `internal/clientstate` package for serializable manifest/check types and a pure assessment function. Contract revision begins at integer `1` and changes only when local client behavior becomes incompatible. Server build comes from `buildinfo.Get()`. A release target is populated only when Release is true and Dirty is false; validate tags as strict release names before constructing paths.

Expose GET `/api/client` with the manifest and embedded thin skill; `check_client` and `get_client_skill` use the same source. Use existing management API network protections. The manifest is read-only public management metadata, not credentials. Embed the thin skill from a Go file in `plugins/` with `//go:embed flats/skills/flats-deploy/SKILL.md`; do not ship Go sources inside the installed plugin. The skill contains `Flats bootstrap revision: 1`; bootstrap revision changes require a plugin metadata version bump, covered by a contract test. Exact-byte SHA-256 is a separate bootstrap_sha256. Guide revision is SHA-256 over deterministic ordered names and bytes of all guide topics, refusal intro/pages, reference headers, appended scripts/types and the rendered dynamic instructions (including upload limit). Move shared instruction rendering to docs to avoid API/MCP dependency cycles. Return guide revision in every guide response without changing the documented runtime API version.

Shared exported Go contract (all JSON names below use snake_case):

```go
const ContractRevision = 1
const BootstrapRevision = 1
// Manifest fields: Server buildinfo.Info; ClientContract int;
// GuideRevision string; BootstrapRevision int; BootstrapSHA256 string;
// CLITarget string (omitempty); Skill string (omitempty); ServerURL string (omitempty).
type CheckInput struct {
    CLIPresent *bool `json:"cli_present,omitempty"`
    CLIVersion string `json:"cli_version,omitempty"`
    ClientContract int `json:"client_contract,omitempty"`
    GuideRevision string `json:"guide_revision,omitempty"`
    BootstrapPresent *bool `json:"bootstrap_present,omitempty"`
    BootstrapRevision int `json:"bootstrap_revision,omitempty"`
}
// CheckResult embeds Manifest, and has CLIStatus, GuideStatus, BootstrapStatus string.
func NewManifest(build buildinfo.Info, uploadLimit int64) Manifest
func ValidateManifest(m Manifest) error
func Check(m Manifest, in CheckInput) (CheckResult, error)
```

Manifest validation requires positive contract/bootstrap revision, well-formed SHA-256s, bounded valid skill frontmatter/revision, matching skill hash, and a clean release tag equal to Server.Version whenever cli_target exists. A clean release manifest must supply that target; a non-release has none. Check input rejects negative revisions and contradictory `*_present:false` with populated identity/revision fields. Zero/empty revisions mean unknown. `Check` clears Skill from its response to avoid repeatedly returning text. `/api/client` and `get_client_skill` include Skill. MCP request context supplies ServerURL from the request scheme/Host, without trusting forwarded headers; this is an observed routing hint and must be reconciled with the user's configured/intended host, never treated as authentication. CLI check always overwrites ServerURL with the actual selected URL.

Do not add executable action strings or a redundant actions enum: statuses form the action contract. CLI compatible/not_applicable -> continue; unknown -> inspect local identity if available, otherwise remain unknown; update_required -> explicitly update and reconnect, or report unavailable target. Guide unknown/refresh_required -> read task guide; current -> use it. Bootstrap unknown -> report actual visible revision when available, otherwise read get_client_skill for this task; refresh_required -> read bootstrap now and report durable update path; current/not_applicable -> no bootstrap work. No status alone authorizes a write.

Mutation JSON results are objects: `status` (updated/current), `path`, `server_url` (omitted for explicit release), `version` and `client_contract` for update, `bootstrap_revision` and `bootstrap_sha256` for skill sync; update adds `previous_version`, `direction` (upgrade/downgrade/same/unordered), `reconnect_required` and `compatibility_assessed`. Skill sync adds `reload_required`. Local check adds `executable` and optional `skill_file`, `skill_sha256`, `skill_exists`; missing skill uses a known stale revision sentinel internally or explicitly overrides bootstrap_status to refresh_required. An explicit installed skill without a readable revision remains unknown, with its exact digest available for replacement.

For new commands, `--json` errors return `{"error":{"code":"...","message":"..."}}`. Usage errors exit 2, all refused/failed operations exit 1; completed checks and mutations exit 0. Stable codes: invalid_manifest, transport, invalid_target, package_managed, service_binary, locked, dev_target, assets_unavailable, checksum, candidate_mismatch, server_changed, file_conflict, filesystem, cancelled. Existing CLI-wide error formatting need not change; contain structured errors in new command wrappers.

CLI surfaces: `flats client check [--skill-file path]`, `flats update [--version tag]`, `flats skill sync --output path [--expected-sha256 hash]`; all support existing `--url` and `--json`. An agent may use an explicit version from check_client when it cannot resolve the MCP address, provided its running executable identity and authorization are established; it must recheck on that same MCP connection afterward. An explicit version update does not require a reachable server and its result says compatibility has not been assessed. Default update reads `/api/client`, compares compiled contract, no-ops when compatible, otherwise selects the manifest's release target and rechecks target/contract before commit. Skill sync similarly binds bytes/hash to a single manifest and rechecks revision before commit.

`flats mcp` appends a small local identity line to successful initialize result instructions, preserving the remote instructions and all other JSON fields. This JSON-encoded line includes version, contract, absolute executable path and selected endpoint; it tells the agent to pass the running bridge identity to check_client. Do not rewrite caller tool arguments, intercept tools or infer the agent's own version. Existing stdio JSON framing, session headers and SSE behavior stay unchanged. Server instructions remain below the existing 2300-byte cap (local append is separately bounded).

## Local updates

Implement release downloads in a focused package, using context-bound HTTP with finite timeouts and bounded sizes (archive 128 MiB, checksums 1 MiB, executable 256 MiB). Construct only `https://github.com/gosuda/flats/releases/download/<validated-tag>/flats_<os>_<arch>.tar.gz` and matching checksums URL. Use a dedicated release HTTP client independent of the management client. Allow HTTPS redirects needed for GitHub release assets; never HTTP downgrades. The trusted publisher is gosuda/flats on GitHub over HTTPS; SHA-256 checks transport/artifact consistency, not a separately signed trust root. Tests inject transports, never public URL overrides.

Require exactly one valid checksum entry for the expected archive. Extract only one regular root `flats` file; reject duplicate entries, symlinks/hardlinks for that name, traversal names and oversized content. Stage beside the target with 0755 permissions; run the candidate as argv `--json version`, parse build info, require exact requested release, clean release identity and positive client_contract. Default updates also require the candidate contract to equal the checked server contract. Add client_contract alongside existing buildinfo fields in `flats --json version`; no first-release constant is needed. Classify direction using semantic version comparison, or unordered for dev builds. No shell is involved. Replace with atomic rename after validation and final host check.

Default target is os.Executable with lstat/EvalSymlinks checks; refuse if resolution changes that path, known package-manager roots such as Homebrew Cellar and Nix store, temporary go-build executables, and non-regular/unwritable paths. Linux may already resolve invocation symlinks; do not promise to detect original argv symlinks. Read the current user launchd plist/systemd unit and compare resolved executable paths; refuse a matching service binary before staging. An unreadable existing service file fails closed. No process scanning is added; help and successful update output warn that a custom/foreground server using this path can immediately start new-version workers and must not share the client update path. Do not add --force or arbitrary binary destination. Concurrency uses nonblocking kernel flock in `<UserConfigDir>/flats-client/locks/`, keyed by canonical destination path. Keep the lock file permanently, releasing only the flock/descriptor; the kernel handles process death. Under the lock, clean only deterministic staging files owned by this target. Sync staged contents before rename and the parent directory afterward. A failure after rename is reported as a durability failure that may already have installed the new bytes; never claim the old bytes survived post-commit errors. Context cancellation cleans ordinary staging and releases the lock.

Skill sync uses an explicit final file path with basename SKILL.md, validates bootstrap frontmatter/name/revision and a 32 KiB maximum, creates parent directories, refuses symlink final path and acquires the same per-target flock. Compare expected digest immediately before atomic rename; synchronize cooperating Flats writers, and disclose that arbitrary external concurrent editors are not transactionally coordinated. Existing non-identical files require the digest flag; no blanket force. Managed installations are a caller/agent ownership decision: CLI cannot reliably recognize every plugin cache, so help and guide explicitly limit this command to standalone files. No agent-specific directory guessing or sidecar manifest is needed.

## Validation and ownership

1. Shared/server: table-test every assessment state, invalid contradictory inputs, release versus dev targets, deterministic hash sensitivity; API/MCP manifest equality; complete embedded skill bytes; guide revision presence; MCP instruction size.
2. Local CLI: offline, unreadable/missing skill, no-write check behavior and explicit endpoint; release fixture success, checksum duplicates/mismatch, archive hazards/limits, wrong executable version, unavailable release, dev target, HTTPS downgrade, cancellation, permission failure, concurrent lock, symlink/package-manager/service-binary refusal and preservation of old bytes; default update server-change race and explicit pinned update independence.
3. Skill: identical/new/changed/expected-hash-mismatch files; manifest checksum failure, sync race, endpoint preservation, no managed auto-discovery and stale-revision refusal.
4. Bridge: initialization adds only local facts, preserves server instructions/fields, JSON and SSE initialize replies and existing sessions/tests pass; active old bridge identity remains old after disk replacement.
5. Integration: disposable local server with CLI on a separate logical endpoint; check -> guide -> standalone sync -> recheck, then release fixture update -> fresh process version -> bridge reconnect. Test direct HTTP with no CLI separately. Run gofmt, go vet ./..., go test ./... and generated-doc checks. No actual installed CLI or production host mutation is needed.

Suggested ownership: server/shared contract + docs embed/API/MCP; CLI update/client/skill commands + updater package; integration/bridge/docs/readme/spec alignment. Shared command registration and common files need one owner with coordinated interface constants.

## Design review disposition

Independent Claude review identified two P1s: shared service executable replacement and self-referential bootstrap hashes. Both are resolved by service-path refusal plus honest foreground-server limits, and visible bootstrap revision separate from bytes SHA-256. Other accepted corrections: no-skill direct HTTP state; executable identity/path mismatch; candidate contract checks; dynamic instructions in guide hash; flock and durability semantics; bounded SKILL.md-only standalone writes and existing-authorization-aware consent. No automatic plugin adapter is promised. Optional bridge headers are deferred: clients dropping instructions honestly retain unknown bridge identity. Older-server fallback is intentionally excluded by the user's no-backward-compatibility requirement. Older explicit binary targets fail candidate contract validation instead of a guessed first-release floor.
