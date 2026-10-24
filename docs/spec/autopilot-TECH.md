# Autopilot: technical design

Status: planned. Technical design for [autopilot.md](autopilot.md). Rule
numbers such as (rule 17) refer to that page; section numbers such as (4.2)
refer to this page. File and line references are to `main` at `fb91754`.

## 1. Decision summary

| Topic | Decision |
|---|---|
| Storage | A new single-row table `autopilot` in `flats.db` (migration 20): `enabled`, an operation bit set `ops`, `guide`, `updated_at`, and `autopilot_changes` holding each change's log line until it is logged. No `config.json` key, flag, environment variable, CLI command or MCP tool |
| Reads and writes | Every setter reads and writes the stored row in one `BEGIN IMMEDIATE` transaction; the in-memory copy is a read cache refreshed only from the database. The approval claim reads the table itself, inside the claiming `UPDATE` |
| Console API | Four routes under `/console/api/experiments/autopilot`, registered only for the console prefix and guarded by `consoleRequest` |
| Classifier | A pure function `core.AutopilotNeeds(action, params)` that reads only the frozen approval and fails closed |
| Agent decisions | `Service.DecideAsAgent` shares `Decide`'s locking and status rules. The claim or rejection is one conditional `UPDATE` that also requires Autopilot on and every needed bit set |
| Record | `decided_by` = `autopilot:` followed by a JSON object (agent, network, and the login, node, NetBird peer, caller IP or origin). Event texts and the console derive the decider from it; the applied operation's events say `via autopilot` |
| MCP | Two cached servers per upload limit, off and on. Each HTTP request reads the Autopilot copy once and gets the matching server. Guide pages, `check_client` and the guide revision change only while on |
| On-mode texts | Exact substring replacements on the off texts, each checked by a test to match exactly once, plus `topic.autopilot` and two refusal pages from a separate embedded file |
| Console | Settings › Experiments with Turn on / Turn off, nine immediate switches and a guide form; Agents page and approval page changes |

## 2. Storage (store)

### 2.1 Table and migration

Migration 20, `{20, "autopilot", addAutopilot}`, is appended to `migrations`
(`internal/store/migrate.go:43-59`). If another branch takes 20 first, use the
next free number; nothing else depends on the number. New databases run every
migration from 5 (`migrate`, `internal/store/migrate.go:225-255`), so the same
step creates the table on new and upgraded hosts.

```sql
CREATE TABLE autopilot (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  ops INTEGER NOT NULL DEFAULT 0 CHECK (ops >= 0 AND ops < 512),
  guide TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO autopilot (id) VALUES (1);
CREATE TABLE autopilot_changes (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  line TEXT NOT NULL,          -- the host log line of 3.1, never the guide text
  logged INTEGER NOT NULL DEFAULT 0 CHECK (logged IN (0, 1))
);
```

`autopilot_changes` holds the host log line of each committed change until
it is written to the log (3.1). It is written in the same transaction as the
change, so a committed change always has its line with the original actor.

The inserted row is the default: off, no operation allowed, empty guide
(rules 1, 5, 6, 7). The table lives next to the other operator-only state in
`flats.db`, like `host_keys` (`internal/store/hostkeys.go:10-27`). It does not
use the legacy `settings` key-value table, which only the configuration
upgrade reads (`internal/store/legacy.go:38`) and which core tests require to
stay untouched (`internal/core/settings_test.go:17`). Nothing in the row is
secret, so it is not encrypted.

### 2.2 Operation bits

Bit positions are stable identifiers and never reused. Server lists, such
as `allowed` (4.1), follow the bit order. The bit order is not the display
order: the console shows the operations in the groups of 6.1.

| Bit | ID | Console label |
|---|---|---|
| 0 | `create_version` | Create versions |
| 1 | `delete_version` | Delete versions |
| 2 | `deploy` | Deploy |
| 3 | `rollback` | Roll back |
| 4 | `rollback_restore_data` | Roll back with data restore |
| 5 | `make_public` | Make Public |
| 6 | `make_private` | Make Private |
| 7 | `public_editing` | Public editing |
| 8 | `delete_flat` | Delete flats |

### 2.3 Store functions (`internal/store/autopilot.go`)

```go
type AutopilotRow struct {
    Enabled   bool
    Ops       uint16 // bits of 2.2
    Guide     string
    UpdatedAt time.Time
}

var ErrAutopilotDenied  = errors.New("autopilot does not allow this decision")
var ErrApprovalNotPending = errors.New("approval no longer pending")

type AutopilotChange struct {
    Seq  int64
    At   time.Time
    Line string
}

func (s *Store) GetAutopilot(ctx context.Context) (AutopilotRow, error)

// UpdateAutopilot runs one write transaction: it reads the stored row, calls
// change with it, and, when change returns a row different from the stored
// one, writes that row and appends line to autopilot_changes before
// committing. It returns the row as committed (the stored row when nothing
// changed) and whether it wrote. An error from change rolls back and is
// returned as is.
func (s *Store) UpdateAutopilot(ctx context.Context, now time.Time,
    change func(stored AutopilotRow) (next AutopilotRow, line string, err error)) (AutopilotRow, bool, error)

// AutopilotSync reads the row and every change with logged = 0, in seq
// order, in one read transaction.
func (s *Store) AutopilotSync(ctx context.Context) (AutopilotRow, []AutopilotChange, error)
// MarkAutopilotLogged sets logged = 1 up to seq and deletes logged changes
// except the newest 100.
func (s *Store) MarkAutopilotLogged(ctx context.Context, seq int64) error

func (s *Store) ClaimApprovalAutopilot(ctx context.Context, id, actor string, need uint16, now time.Time) error
func (s *Store) RejectApprovalAutopilot(ctx context.Context, id, actor, result string, need uint16, now time.Time) error
```

`UpdateAutopilot` takes a dedicated connection (`db.Conn`) and starts with
`BEGIN IMMEDIATE`, so it holds the write lock before it reads; a second
writer waits on `busy_timeout` instead of committing over a value the first
one read. Because every change reads the stored row inside the transaction
that writes it, two console tabs changing different switches cannot undo
each other's change, and no setter decides anything from a cached value.

`ClaimApprovalAutopilot` is the existing `ClaimApprovalAuthorized`
(`internal/store/lifecycle.go:512-523`) with one more condition:

```sql
UPDATE approvals SET status='applying', decided_by=?, authorized_at=?
WHERE id=? AND status='pending'
  AND EXISTS (SELECT 1 FROM autopilot
              WHERE id=1 AND enabled=1 AND (ops & ?) = ?)
```

The last two parameters are both `need`.

`RejectApprovalAutopilot` adds the same `EXISTS` to the statement of
`RejectApprovalAuthorized` (`internal/store/lifecycle.go:525-537`). SQLite runs
the whole statement in one write transaction, and the console's setters are
write transactions too, so either a setter commits first and the claim
matches no row, or the claim commits first and stands (rule 17). When no row
changed, the function reads the approval's status and the `autopilot` row and
returns `ErrApprovalNotPending` if the status is no longer `pending`, else
`ErrAutopilotDenied`. That second read only picks the error; the outcome was
already decided by the `UPDATE`. `need` is never 0 (3.2); the function
refuses 0 as a programming error.

## 3. Core

### 3.1 Settings, snapshot and host log (`internal/core/autopilot.go`)

```go
type AutopilotOp string // the IDs of 2.2
var AutopilotOps = []AutopilotOp{"create_version", ..., "delete_flat"} // bit i = index i
type OpSet uint16
func (s OpSet) Has(op AutopilotOp) bool
func (s OpSet) List() []AutopilotOp // in AutopilotOps order
func ParseOpSet(ids []string) (OpSet, error) // ErrInvalid for an unknown ID

const AutopilotGuideMax = 4000 // Unicode code points

type Autopilot struct {
    Enabled   bool
    Allowed   OpSet
    Guide     string
    UpdatedAt time.Time
}

func (s *Service) Autopilot() Autopilot
func (s *Service) SetAutopilotEnabled(ctx context.Context, on bool, confirmed *OpSet, who string) (Autopilot, error)
func (s *Service) SetAutopilotOp(ctx context.Context, op AutopilotOp, allowed bool, who string) (Autopilot, error)
func (s *Service) SetAutopilotGuide(ctx context.Context, guide, who string) (Autopilot, error)
```

The database row is the only source of truth. The in-memory copy is only a
cache for readers, and nothing that writes ever reads it.

- **Setters.** Each setter calls `UpdateAutopilot` (2.3) on
  `context.WithoutCancel(ctx)`, so a cancelled request cannot stop a started
  change. Its `change` function receives the stored row and decides from it
  alone:
  - `SetAutopilotEnabled(on=true)` needs `confirmed`, the operations the
    confirmation dialog listed. If the stored `ops` differ, `change` returns
    `ErrConflict` ("the allowed operations changed after the page loaded;
    review them and turn Autopilot on again"). `on=false` takes no
    `confirmed`;
  - `SetAutopilotOp` sets or clears its bit in the stored `ops`;
  - `SetAutopilotGuide` refuses more than `AutopilotGuideMax` code points
    (`utf8.RuneCountInString`) with `ErrInvalid` and stores the text as given.

  `change` also returns the host log line for the change, built from the
  stored and the new row and the `who` of 4.3. When the new row equals the
  stored one (already on, switch already in that state, same guide), nothing
  is written and nothing is logged. A Turn off therefore writes whenever the
  stored row is on, whatever the cache says.
- **Sync.** After `UpdateAutopilot` returns, with or without an error, the
  setter calls `syncAutopilot()`, which holds `Service.autopilotMu`:
  1. `AutopilotSync` reads the row and the unlogged change lines;
  2. it writes each line to the host log in `seq` order, with its stored
     time, and calls `MarkAutopilotLogged`;
  3. it replaces the cache with the row it read and clears
     `Service.autopilotStale`.

  If step 1 fails, it sets `autopilotStale` and leaves the cache as it was.
  If marking fails, the lines are written again at the next sync: the log is
  at least once, never lost. The setter returns the row it read in step 1
  when it has one, and the error of `UpdateAutopilot` if there was one; the
  console shows that error and reloads (6.1).
- **Readers.** `Autopilot()` returns a copy of the cache. While
  `autopilotStale` is set it first calls `syncAutopilot()`. If that still
  fails, it returns the cache with `Enabled` false, so MCP shows the off
  surface, and logs "autopilot: settings unavailable: ..." at most once a
  minute. This hides tools at worst and never authorizes anything:
  decisions check the row itself (2.3). Changes therefore reach the next MCP
  request without another edit or a restart.
- **Startup.** `core.New` calls `syncAutopilot()` after the store opens, so a
  change that committed just before a crash is logged at the next start. A
  failure there fails startup like the other loads in `New`
  (`internal/core/service.go:230-260`).

The cache is `Service.autopilot atomic.Pointer[Autopilot]`, replaced only by
`syncAutopilot` from a row it read.

Host log lines go through `s.logf`, which is `log.Printf` with its date and
time prefix (`internal/core/service.go:235-236`, `internal/app/serve.go:345`).
`who` is built by the API (4.3); core writes it with `%s` because the API has
already quoted the identity. The line is stored in `autopilot_changes` and
logged as stored, followed by ` (at <RFC 3339 time of the change>)` from its
`at` column, so a line written late by a recovery sync still says when the
change happened:

```text
autopilot: turned on by tailnet user "alice@example.com"; agents may decide: create_version, deploy (at 2026-10-24T09:00:00Z)
autopilot: turned on by the console on the loopback listener (no tailnet identity); agents may decide: nothing (at 2026-10-24T09:00:00Z)
autopilot: turned off by tailnet user "alice@example.com" (at 2026-10-24T09:00:00Z)
autopilot: agents may now decide make_public; changed by NetBird peer "laptop.netbird.cloud" (at 2026-10-24T09:00:00Z)
autopilot: agents may no longer decide make_public; changed by tailnet user "alice@example.com" (at 2026-10-24T09:00:00Z)
autopilot: operation guide changed by tailnet user "alice@example.com" (812 of 4000 characters) (at 2026-10-24T09:00:00Z)
```

The guide line has the length only, never the text (rule 4).

### 3.2 Classifier

```go
// AutopilotNeeds returns the operations an approval needs (rule 6). It reads
// only the action and frozen params. An approval it cannot classify returns
// *UnclassifiedError, which no switch allows.
func AutopilotNeeds(action string, params json.RawMessage) (OpSet, error)

type UnclassifiedError struct{ Reason string }
```

It is pure: no store, no flat, no clock. It lives in core so it can use the
same unexported parameter types the apply path decodes. Parameters are
readable only when all of these hold; otherwise the approval is unclassified
("its frozen parameters cannot be read"):

1. `params` is a JSON object (not `null`, an array, a string or a number).
2. No top-level member is JSON `null`. Go's decoder would read a `null`
   field as the zero value; the classifier does not accept that guess.
3. `json.Unmarshal` into the action's parameter type succeeds. A field of the
   wrong type, such as `{"version":"broken"}` for `activate`, fails here.
   Unknown members are ignored, as the apply path ignores them, so older
   approvals with extra or fewer fields stay readable.

| Action | Parameter type | Needs |
|---|---|---|
| `create_version`, `publish` | `publishParams` (`internal/core/lifecycle.go:45-52`) | `create_version` |
| `delete_version` | `deleteVersionParams` (`internal/core/version.go:78-84`) | `delete_version` |
| `activate` | `activateParams` (`internal/core/lifecycle.go:54-60`) | `deploy` |
| `rollback` | `rollbackParams` (`internal/core/lifecycle.go:62-71`) | `RestoreData` false: `rollback`; true: `rollback_restore_data` |
| `restore_data` | `rollbackParams` | `rollback_restore_data` |
| `delete` | ``struct{ Slug string `json:"slug"` }``, the map `Delete` stores (`internal/core/service.go:1922`) | `delete_flat` |
| `set_visibility` | `visibilityParams` | see below |
| anything else | none | unclassified ("unknown action") |

An absent `restore_data` reads as false, as the apply path reads it. The
`delete` slug is not compared with the approval's flat: a rename moves the
approval to the new slug but keeps its parameters
(`internal/store/store.go:535`).

Because step 4 of 3.3 runs before any status rule, an unreadable approval is
refused for approve and reject alike and stays unchanged (rule 6).

For `set_visibility`, after the readability rules above on
`visibilityParams` (`internal/core/lifecycle.go:73-97`), it resolves levels as
`applyVisibilityApproval` does (`internal/core/lifecycle.go:1053-1065`):

1. `From` empty: unclassified ("no frozen starting visibility", rule 6).
2. Both raw values must satisfy `store.Visibility.Valid()`
   (`internal/store/store.go:35-38`), checked before any canonicalization:
   `private`, `public`, and the legacy `public-listed` and `public-unlisted`.
   These four are the only values any Flats version has defined (`main` and
   every earlier definition of the constants in `internal/store/store.go`;
   the first versions had `private` and the two legacy names, `public` came
   later). Any other value, including empty `Visibility`, is unclassified.
   `Canonical()` maps every unknown value to `private`
   (`internal/store/store.go:40-46`), and the apply path canonicalizes the
   target before its own validity check (`internal/core/lifecycle.go:1051-1053`),
   so checking after canonicalization would let `{"from":"public",
   "visibility":"bogus"}` classify, and apply, as Make Private. Only then
   `from := Visibility(From).Canonical()` and
   `to := Visibility(Visibility).Canonical()`.
3. Requested level when `to` is Public: `edit` only when `AccessSeq != nil`
   and `Access == "edit"`, otherwise `view`. With `AccessSeq` set, an `Access`
   other than empty, `view` or `edit` is unclassified (the apply path refuses
   it too).
4. Starting level when `from` is Public: `FromAccess` (`view` or `edit`);
   empty is "unknown", any other value is unclassified.
5. Result:
   - Private to Public: `make_public`, plus `public_editing` when the level is `edit`;
   - Public to Private: `make_private`, whatever the levels;
   - Public to Public: `public_editing` when the starting level is known and
     differs; unknown starting level or the same level is unclassified;
   - Private to Private: unclassified ("requests no change").

Current requests never store the last two unclassified cases
(`requestVisibility` answers "unchanged" instead, `internal/core/lifecycle.go:327-335`);
the classifier still fails closed for them.

The core also exports `func (OpSet) Labels() string`, the console labels of
2.2 joined with ", ", for refusal messages.

### 3.3 Agent decisions

```go
type Arrival struct {
    Network string // "local", "listener", "tailscale", "netbird" or "external"
    IP      string // listener only: the caller's IP address
    Login   string // tailscale only: the tailnet login of the caller
    Node    string // tailscale only, when Login is empty: the caller's node name
    Peer    string // netbird only: the calling peer's name (FQDN)
    Origin  string // external only: the operator-declared origin
}

type AgentDecision struct {
    Agent   string
    Arrival Arrival
}

var (
    ErrAutopilotOff        = errors.New("autopilot is off")
    ErrAutopilotNotAllowed = errors.New("autopilot does not allow this operation")
)

func (s *Service) DecideAsAgent(ctx context.Context, id string, approve bool, d AgentDecision) (store.Approval, error)
```

`Decide` (`internal/core/service.go:2058-2113`) becomes a wrapper around an
unexported `decide(ctx, id, approve, agent *AgentDecision)`. With `agent` nil
the code path, store calls (`ClaimApprovalAuthorized`,
`RejectApprovalAuthorized`), `decided_by` value and texts are exactly today's,
so console behavior does not change. `DecideAsAgent` passes a non-nil
decision. Steps for an agent, in order:

1. `Agent`: refused with `ErrInvalid` when `strings.TrimSpace(Agent)` is empty
   or `utf8.RuneCountInString(Agent) > 100` (rule 16, rule 17). Otherwise it
   is kept as given, including surrounding spaces.
2. `st.GetAutopilot`. Off: `ErrAutopilotOff` (rule 17).
3. Take `s.lock("approval:" + id)` as `Decide` does, then `GetApproval`.
   Missing: the store's `ErrNotFound` (rule 17).
4. `need, err := AutopilotNeeds(a.Action, a.Params)`. Unclassified, or
   `need &^ row.Ops != 0`: `ErrAutopilotNotAllowed` wrapped in a message that
   names the missing operations by label and ID, or the unclassified reason.
   This applies to approve and reject alike (rule 17).
5. Status rules, unchanged from `Decide` (rule 20): reject of `rejected` and
   approve of `approved` return the approval unchanged with no write; any
   other non-pending status, including `applying`, is `ErrConflict` via the
   same `lost` helper.
6. Pending: `ClaimApprovalAutopilot` or `RejectApprovalAutopilot` with
   `actor = EncodeAgentDecider(d)` and `need`. `ErrApprovalNotPending` goes to
   `lost(nil)`. `ErrAutopilotDenied` re-reads the row and returns
   `ErrAutopilotOff` or `ErrAutopilotNotAllowed` as in steps 2 and 4; if the
   row now allows the decision, it returns `ErrAutopilotNotAllowed` with
   "Autopilot changed during the call; nothing changed; call again".
7. After a claim, `finishApplying` runs exactly as for the operator: frozen
   state checks, staleness and failures are shared (principle 3, rule 20).
   After a rejection, the event of 3.4 is written.

Steps 2 and 4 read the row before the lock only to give early, specific
refusals. The authoritative check is step 6, at the moment the decision takes
effect (rule 17). A switch turned off after step 6 has no effect on that
decision, and `resumeApplying` after a restart finishes it like any claimed
approval, since the record has `decided_by` and `authorized_at`
(`internal/core/lifecycle.go:593-608`).

`ErrorCategory` (`internal/core/lifecycle.go:1720-1790`) gets two cases
before `ErrConflict`: `autopilot_off` and `autopilot_not_allowed`. They are
listed in a new `AutopilotErrorCategories`, not in `ErrorCategories`
(`internal/core/lifecycle.go:1711-1717`), because their refusal pages exist
only while Autopilot is on (5.5). These errors never reach a claimed
approval, so they never become a `failure_code`. `statusOf` in the API maps
both to 403 for completeness.

### 3.4 Records

`decided_by` of an agent decision:

```text
autopilot:{"agent":"pr-bot","network":"tailscale","login":"bot@example.com"}
autopilot:{"agent":"pr-bot","network":"tailscale","node":"ci-runner"}
autopilot:{"agent":"pr-bot","network":"local"}
autopilot:{"agent":"pr-bot","network":"listener","ip":"192.168.1.20"}
autopilot:{"agent":"pr-bot","network":"netbird","peer":"ci.netbird.cloud"}
autopilot:{"agent":"pr-bot","network":"external","origin":"https://flats.example.com"}
```

`EncodeAgentDecider` writes the prefix and `json.Marshal` of the fields with
`omitempty` on ip, login, node, peer and origin. `ParseAgentDecider(decidedBy)` accepts a
value only when it has the prefix, decodes into the struct, has a non-blank
agent and a known network; anything else is an operator value. Console values
today are `console`, a tailnet login, or `netbird:<fqdn>`
(`internal/api/api.go:914-945`, `internal/app/consoledomains.go:148-160`), none
of which can start with `autopilot:{`.

`DeciderText(decidedBy)` returns `the operator` for operator values and, for
an agent decision:

```text
Autopilot (agent "pr-bot" via Tailscale, bot@example.com)
Autopilot (agent "pr-bot" via Tailscale node ci-runner)
Autopilot (agent "pr-bot" via Local)
Autopilot (agent "pr-bot" via the console listener from 192.168.1.20)
Autopilot (agent "pr-bot" via NetBird peer ci.netbird.cloud)
Autopilot (agent "pr-bot" via external origin https://flats.example.com)
```

The agent name is written with Go `%q`, so a name with a newline or quote
cannot forge another line in the event log.

Uses:

- `finishApplying` writes `"%s %s by %s: %s"` with `DeciderText(a.DecidedBy)`
  where it writes "by the operator" today (`internal/core/lifecycle.go:677`).
  It reads the stored `decided_by`, not the context, so a decision resumed
  after a restart keeps its decider. Operator decisions keep the exact text.
- Restart recovery. When Flats stops after an approved operation committed
  but before the approval was finalized, `resumeApplying` calls
  `finishApplying`, which today finalizes through one of two shortcuts that
  return before the event statement: an `applied` receipt
  (`internal/core/lifecycle.go:610-629`, used by visibility, version creation
  and version deletion) or a recorded deployment
  (`internal/core/lifecycle.go:631-650`, used by activation and rollback). For
  an agent decision both shortcuts write the same decision event as the
  normal path, `"%s approved by %s: %s"` with the message the shortcut
  stores, after `FinishApprovalData` succeeds. `FinishApprovalData` moves the
  approval from `applying` exactly once (`internal/store/lifecycle.go:500-510`),
  so only the call that finalizes writes the event; a second resume, or a
  console decision racing it, fails that update and writes nothing. For an
  operator decision the shortcuts keep today's behavior and write no event,
  so operator output does not change. The events of the applied operation
  itself (3.5) are written while it applies, before the crash point; a crash
  between an operation's commit and its own event loses that event for both
  deciders, as today. The decision event and the approval record are the
  durable record.
- The rejection event `a.Action+" rejected by the operator"`
  (`internal/core/service.go:2093`) uses `DeciderText` the same way.
- An approved deletion writes no event, as today: the flat's events go with
  it, and the approval row, which has no foreign key to the flat
  (`internal/store/store.go:209-224`), keeps `decided_by` (rule 28).
- `get_approval` text for `applying` (`internal/mcpx/tools.go:976`) becomes
  `DeciderText`-based: "The operator approved it" stays for operator values,
  and an agent decision reads `Autopilot (agent "pr-bot" via Local) approved it;
  the operation is still applying.` Its `pending` text changes only as 5.3
  describes.

### 3.5 Events of the applied operation

The apply functions name the surface they run as in their events and host
log lines, for example "saved version 3 from draft revision 7 via console"
(`internal/core/lifecycle.go:833`), "rollback: version 2 is live (was 3) via
console" (`internal/core/service.go:1584`) and the host log line "flat blog
deleted via console" (`internal/core/service.go:2036`), because the
apply functions pass `ViaConsole`. After an agent decision they say `via autopilot` (rule 28):

- a new `ViaAutopilot Via = "autopilot"` next to the other surfaces
  (`internal/core/service.go:37-45`);
- the five places that pass `ViaConsole` while applying an approval today
  (`internal/core/lifecycle.go:689`, `774`, `969`, `1029`, `1031`) pass
  `applyVia(a)` instead: `ViaAutopilot` when `ParseAgentDecider(a.DecidedBy)`
  succeeds, else `ViaConsole`. Because it reads the stored `decided_by`, a decision resumed
  after a restart keeps it.

`via` is only text on these paths today. The functions reached from
`applyApproval` (`applyDelete`/`deleteLocked`, `saveVersionFromDraft`,
`publishRevision`, `deployLocked`, `activate`, `rollbackLocked`,
`restoreLocked`; `internal/core/lifecycle.go:777-915`,
`internal/core/service.go:1488-1760`, `1927-2036`) use it in messages and
never compare it with `ViaConsole`. A test keeps it that way: an agent-decided
and an operator-decided approval of each action produce the same state,
`result_data` and events, except `console` versus `autopilot` in the via
words and the decider text. `applyVisibilityApproval` and
`deleteVersionLocked` take no `via`, so their events do not change.
Operator decisions keep `ViaConsole` and today's texts.

### 3.6 Arrival network

The MCP handler (5.1) builds `Arrival` once per HTTP request from the request
and its context, in this order:

1. `api.ExternalOrigin(ctx) != ""` (`internal/api/guard.go:56-61`): `external`
   with that origin.
2. `core.ConsoleDomainOf(ctx)` reports a console domain: `tailscale` or
   `netbird`. `withConsoleDomain` (`internal/app/consoledomains.go:49-53`)
   also calls the new `core.WithConsoleDomain(ctx, prov, name)`; the app's own
   key stays for its existing users.
3. Otherwise the request came to the console listener (`host.console_addr`),
   and the connection's source address decides between `local` and
   `listener`, as `hostOrigin(r)` in `internal/mcpx/autopilot.go`:
   - Parse `r.RemoteAddr` with `netip.ParseAddrPort` and `Unmap()` it, so an
     IPv4-mapped IPv6 address compares as IPv4. Keep its zone.
   - `local` when the source is a loopback address, or when it is one of this
     host's own interface addresses. A process on the host that connects to
     the listener bound to `0.0.0.0` or `::` through the host's LAN or global
     address arrives with that address as its source, so it is `local`
     (rule 26: a connection from the host itself).
   - The host's addresses come from `net.Interfaces()` and each interface's
     `Addrs()`, cached for 10 seconds and re-read once on a miss, so an
     address that DHCP or SLAAC just added is found. The comparison ignores
     the zone, except for link-local addresses (`fe80::/10`, `169.254/16`),
     which match only when the source's zone names the interface that holds
     that address: the same link-local address on another link is another
     machine.
   - Otherwise `listener`, with `IP` the unmapped source address written with
     `String()`, zone included (`fe80::1%en0`).
   - Only the socket's source address counts. `Forwarded`, `X-Forwarded-For`,
     `X-Real-Ip` and `Tailscale-User-*` headers are client-sent and are not
     read. A reverse proxy on the host that forwards to the listener without
     being configured as an external origin is therefore `local`, which is
     true of the connection it makes.
   - The lookup is a package variable `hostAddrs func() []netip.Addr`, each
     link-local address carrying its interface name as zone, so tests replace
     it. A non-link-local source matches by `WithZone("")` equality, a
     link-local one by full equality including the zone.

For `netbird`, `Peer` comes from `core.NetbirdPeerOf(ctx)`. The NetBird
console handler (`nbConsole.handler`, `internal/app/consoledomains.go:148-160`)
already resolves the caller with `IdentityForIP` for console decisions; it
also calls the new `core.WithNetbirdPeer(ctx, fqdn)` with the same value.
When the peer is unknown, `Peer` is empty and the record has the network
only.

For `tailscale`, login and node come from `core.TailnetPeerOf(ctx)`. The tsnet
identity middleware (`internal/expose/tsnet/tsnet.go:748-767`) calls the new
`core.WithTailnetPeer(ctx, login, node)` with the WhoIs result: login from
`UserProfile.LoginName` for an untagged node, as today's header, and node from
`Node.ComputedName` (else the first label of `Node.Name`) for every node,
tagged or not. The value travels in the context only, not in a header, so
flat handlers on tsnet nodes see no new header. The record keeps the login
when there is one and the node name otherwise (rule 26).

Rule 19 lets every one of these arrivals use the tools, including the console
listener on a non-loopback address; there is no loopback check (5.3).

## 4. Console API

### 4.1 Routes

Registered inside the `if console` block of `Server.Handler`
(`internal/api/api.go:149`), in a new file `internal/api/experiments.go`:

| Method and path | Body | Result |
|---|---|---|
| `GET /console/api/experiments/autopilot` | none | 200 view |
| `PUT /console/api/experiments/autopilot` | `{"enabled": true, "confirmed_allowed": ["deploy"]}` or `{"enabled": false}` | 200 view |
| `PUT /console/api/experiments/autopilot/operations/{op}` | `{"allowed": true}` | 200 view |
| `PUT /console/api/experiments/autopilot/guide` | `{"guide": "..."}` | 200 view |

The view:

```json
{"autopilot": {
  "enabled": true,
  "allowed": ["create_version", "deploy"],
  "guide": "Approve deploys of PR preview flats only.",
  "guide_max": 4000,
  "updated_at": "2026-10-24T09:00:00Z"
}}
```

`allowed` is always a list in the order of 2.2, `[]` when empty.
`updated_at` is omitted while the row was never changed.

Bodies are decoded with `decode` (`internal/api/api.go:301-311`), which
refuses unknown fields. Errors:

- `enabled` missing, `confirmed_allowed` missing with `enabled: true` or
  present with `enabled: false`, `allowed` missing, an unknown operation ID,
  `guide` missing or over 4,000 code points: 400 `invalid`.
- `confirmed_allowed` differing from the stored operations: 409 `conflict`,
  nothing changed.

### 4.2 Console only

`h` registers each pattern for `/api` and `/console/api`
(`internal/api/api.go:86-104`); these four live in the console-only block, so
`/api/experiments/...` has no route and answers 404. The console routes pass
`consoleRequest` (`internal/api/guard.go:211-243`): the `X-Flats-Console: 1`
header, no `X-Flats-Client`, same origin, and for changes a same-origin
`Sec-Fetch-Site` with `Origin` (or the plain-HTTP NetBird rule). The CLI and
API clients are refused with 403 (rule 3). MCP has no tool for them. As the
spec says (rule 2), this keeps other websites out; it does not tell a person
from a local program.

### 4.3 Who changed it

A helper `consoleActor(r)` returns the same phrases the decide handler uses
for its event (`internal/api/api.go:922-930`), with the identity quoted:

- `approverOf` starts with `netbird:`: `NetBird peer "<fqdn>"`;
- other `approverOf` value: `tailnet user "<login>"`;
- external origin: `the console via external origin <origin> (no authenticated identity)`;
- otherwise: `the console on the loopback listener (no tailnet identity)`.

The handlers pass it as `who` to the core setters (3.1).

## 5. MCP

### 5.1 One state per request

`Handler` (`internal/mcpx/mcpx.go:26-55`) reads `svc.Autopilot()` once per HTTP
request and stores it in the request context next to the loopback and
client-origin values, together with the `Arrival` of 3.6. Everything that
depends on Autopilot during that request reads this copy, passed explicitly
as 5.9 specifies: the server choice, `guide`, `check_client`,
`get_client_skill` and `get_approval`. The decision tools check again in core
(3.3), so the copy only shapes texts and tool lists.

### 5.2 Server cache

`serverCache` (`internal/mcpx/mcpx.go:57-89`) keeps two servers, `off` and
`on`, for the current upload limit, and drops both when the limit changes.
`mcp.NewStreamableHTTPHandler`'s callback becomes
`func(r *http.Request) *mcp.Server { return servers.get(stateOf(r.Context()).Enabled) }`.
The servers depend only on the limit and on/off, never on the switches or the
guide, so changing a switch rebuilds nothing.

- The off server is built by today's code with today's arguments. Every off
  output therefore comes from the same functions as before (principle 2).
- The on server uses `runtimeref.AutopilotInstructions(limit)` (5.4),
  registers the request tools with their on descriptions (5.4), registers the
  `guide` tool with the on description, registers `check_client` with the on
  result type (5.7), and adds `approve_request` and `reject_request` (5.3).

The server is stateless (`Stateless: true`), so a client that listed tools
while Autopilot was on keeps that list until it lists again. A call to
`approve_request` on the off server is answered by the SDK as an unknown tool,
which is today's answer for that name; a call that reached the on server
after Autopilot was turned off is refused by core with `autopilot_off`
(rule 15, rule 17).

### 5.3 The decision tools (`internal/mcpx/autopilot.go`)

```go
type DecideIn struct {
    ID    string `json:"id" jsonschema:"approval id (apr-...)"`
    Agent string `json:"agent" jsonschema:"your name for yourself, for example the bot's name; 1-100 characters; recorded as given with the decision and not verified"`
}
```

Both fields are required (no `omitempty`). Output type: `ApprovalOut`
(`internal/mcpx/tools.go:945-960`), the same as `get_approval`.

| Tool | Annotations | Description |
|---|---|---|
| `approve_request` | destructive, idempotent | "Autopilot: approve a pending approval when the operator allows every operation it needs (guide topic.autopilot). It applies like the operator's approval in the console: the frozen state is checked and the result is what get_approval returns. agent is your name, recorded with the decision. Approving an approved request again returns it unchanged." |
| `reject_request` | not destructive, idempotent | "Autopilot: reject a pending approval when the operator allows every operation it needs (guide topic.autopilot). Nothing is applied. agent is your name, recorded with the decision. Rejecting a rejected request again returns it unchanged." |

The handler builds `AgentDecision{Agent: in.Agent, Arrival: arrivalOf(ctx)}`
(3.6) and calls `DecideAsAgent`. As in the console handler
(`internal/api/api.go:922`), a returned approval whose status is `failed`
with an error other than `ErrConflict` is a decision that took effect: the
tool returns it as a normal result, and the text reports the failure and its
`failure_code`. Every other error is a tool error through `toolErr`
(`internal/mcpx/tools.go:243`) with its category: `invalid`, `not_found`,
`conflict`, `autopilot_off` or `autopilot_not_allowed`. The result text is
the `get_approval` text of 3.4, including its public notice after an approved
`set_visibility`.

Every network that serves `/mcp` reaches these tools; there is no loopback
check (rule 19).

`get_approval` on a `pending` approval (`internal/mcpx/tools.go:973-975`)
uses the request's Autopilot copy (5.1). While Autopilot is on and
`AutopilotNeeds` classifies the approval with every needed operation allowed,
the sentence "Waiting for the operator to decide in the Flats console." is
replaced by "Waiting for a decision: the operator can decide it in the Flats
console, or an agent with approve_request or reject_request, since Autopilot
allows its operations." For any other pending approval, and always while
Autopilot is off, the text is today's (rule 23, rule 24). The structured
`ApprovalOut` does not change. The text can be stale by the time the agent
acts; the decision tools check again (3.3).

### 5.4 On texts

Each on text is the off text with exact replacements. A table of
`{location, old, new}` lives next to the texts it changes:
`docs/autopilot.go` for instructions and guide topics, and
`internal/mcpx/autopilot.go` for tool descriptions. A test asserts that every
`old` occurs exactly once in its off text, so editing an off text without
updating its replacement fails the build. The off files and strings are not
edited.

Instructions (`docs/instructions.go:12-22`), as `runtimeref.AutopilotInstructions(limit)`:

| Old | New |
|---|---|
| `wait for explicit operator approval and change nothing before it.` | `wait for an approval and change nothing before it.` |
| `Give the user approval_url exactly as returned and poll get_approval. Never approve your own request or imitate the console; MCP has no approval tool.` | `Autopilot is on: approve_request and reject_request decide approvals of operations the operator allows; read guide topic.autopilot first. Otherwise give the user approval_url exactly as returned and poll get_approval. Never imitate the console.` |
| `- Ask the user before set_visibility, delete_flat or rollback with restore_data.` and its newline | removed |

Rendered at the default 20 MiB limit the off instructions are 2,298 bytes and
the on instructions 2,296; `TestInstructionsStaySmall`
(`internal/mcpx/guide_test.go:119-131`) applies its 2,300-byte cap to both,
keeps its off invariants for the off text, and checks the on text for
`topic.autopilot`, `approve_request`, `approval_url`, `get_approval`,
`NOT access control`, `Never infer state from a URL`, `BOTH directions`,
`refusal.<category>` and `topic.instructions`.

Tool descriptions (`internal/mcpx/tools.go:44-77`):

| Tool | Old | New |
|---|---|---|
| `deploy` | `Request operator approval to activate` | `Request approval to activate` |
| `create_version` | `request explicit operator approval to save it` | `request approval to save it` |
| `delete_version` | `Request explicit operator approval to delete` | `Request approval to delete` |
| `rollback` | `Request operator approval to activate` | `Request approval to activate` |
| `rollback` | `Ask the user before restore_data. Nothing changes before operator approval.` | `Nothing changes before approval.` |
| `set_visibility` | `BOTH directions require explicit operator approval; ask the user first.` | `BOTH directions require approval.` |
| `delete_flat` | `Always waits for the operator's approval (returns approval_url).` | `Always waits for approval (returns approval_url).` |

Each of these seven descriptions then gets one appended sentence: ` While
Autopilot is on, an agent may decide it with approve_request when the operator
allows the operation (guide topic.autopilot).` The `restore_data` input field
description of `rollback` (`internal/mcpx/tools.go:740`) replaces
`Frozen for explicit operator approval.` with `Frozen for approval.`; the on
server sets `Tool.InputSchema` to the inferred schema with that one property
description changed. The `guide` tool description adds `topic.autopilot`
after `topic.approvals` in its list.

Guide topics, as `runtimeref.AutopilotTopicOverlay(name)`:

| Topic | Old | New |
|---|---|---|
| `approvals` | `Agents never decide publication, exposure or deletion.` | ``Autopilot is on: agents may decide the approvals `topic.autopilot` lists, and the operator decides the others.`` |
| `approvals` | `until the operator approves it in the Flats console:` | `until it is approved:` |
| `approvals` | `say you are waiting for operator approval.` | `say you are waiting for approval.` |
| `approvals` | ``* Never approve your own request, call console routes or send console⏎  headers. MCP has no approval or provider-grant tool.`` | ``* Decide approvals only with `approve_request` and `reject_request`⏎  (`topic.autopilot`). Never call console routes or send console⏎  headers. MCP has no provider-grant tool.`` |
| `approvals` | ``* Ask the user before requesting `set_visibility`, `delete_flat` or a rollback⏎  with `restore_data`.`` | ``* Whether to ask a person before `set_visibility`, `delete_flat` or a⏎  rollback with `restore_data` is up to the operation guide in⏎  `topic.autopilot` and your own policy.`` |
| `rollback-data` | ``Nothing is restored before operator approval.⏎Ask the user before requesting it, then poll`` | ``Nothing is restored before approval⏎(`topic.autopilot`). After requesting it, poll`` |
| `index` | ``\| `set_visibility`, `delete_flat` (ask the user first) \|`` | ``\| `set_visibility`, `delete_flat` \|`` |
| `index` | `\| Understand a refused call \|` | ``\| Decide pending approvals while Autopilot is on \| `topic.autopilot` \| `approve_request`, `reject_request` \|⏎\| Understand a refused call \|`` |
| `index` | `` `delete_flat` also wait for the operator (`topic.approvals`). `` | `` `delete_flat` also wait for approval (`topic.approvals`, `topic.autopilot`). `` |
| `board` | ``deleting it waits for the operator (`topic.approvals`).`` | ``deleting it waits for approval (`topic.approvals`).`` |
| `board` | `It waits for the operator's⏎  approval like every visibility change` | `It waits for⏎  approval like every visibility change` |
| `docs` | ``wait for the operator's approval (`topic.approvals`).`` | ``wait for approval (`topic.approvals`).`` |
| `static` | `requests operator approval; share its` | `requests approval; share its` |
| `client-update` | `never replace the operator approval flow` | `never replace the approval flow` |

`⏎` marks a newline in the source file, and `\|` a literal `|` of the
Markdown table row being replaced. All fourteen anchors were checked against
`main` to occur exactly once.

A census test (5.8) enforces that no on text keeps an operator-only claim,
so a later topic that adds one fails until it gets a replacement. The fixed
reference documents keep their text: the runtime reference and content types
reference are assembled from the unmodified topics
(`docs/runtime_reference.go:83-84`), and `llms.txt`
(`internal/mcpx/llms.go:123`) is not touched (rule 22).

### 5.5 `topic.autopilot` and its refusal pages

A new embedded file `docs/agent/autopilot.md`, outside `agent/topics/` so the
topic list, references and off guide do not see it
(`docs/runtime_reference.go:23`, `65-81`). It has a topic part and two
refusal sections, `## autopilot_off` and `## autopilot_not_allowed`.
`runtimeref.AutopilotPage(st)` (5.9) renders the topic part from the
request's state:

- what Autopilot is, and that `approve_request` and `reject_request` take
  `{id, agent}`, with what `agent` is and that Flats records it with the
  network and, over Tailscale, the login or node name;
- the operations allowed now, as a list of label and requests (from the
  table of rule 6), or "None: the operator allows no operation yet; give the
  user approval_url for every approval";
- how an approval maps to operations, including the `set_visibility` cases;
- that both tools refuse an approval needing an operation that is not
  allowed, and the operator decides it in the console;
- the decision rules of rule 20, that stale checks apply, and that the
  operator can turn Autopilot or an operation off at any moment;
- that Autopilot grants no providers, keys or secrets (principle 5), and that
  Public is not access control;
- that agents must never change Settings › Experiments, including through a
  browser or by calling the console;
- that these rules take precedence over the runtime reference, the content
  types reference and `llms.txt` where those say agents cannot decide;
- the operation guide under `### Operation guide from the operator`, in a
  fenced block whose fence is one backtick longer than the longest backtick
  run in the text (at least three), so the guide cannot add Flats headings; or
  "The operator wrote no operation guide." when empty. Flats does not enforce
  it; whether to ask a person first is left to it and the agent's policy.

While on, `guide` resolves `topic.autopilot`, `refusal.autopilot_off` and
`refusal.autopilot_not_allowed`, and `Available` adds `topic.autopilot` after
`topic.approvals` and the two refusal items at the end. While off they are
unknown items, as today (rule 24).

### 5.6 Guide revision

`runtimeref.GuideRevision(limit)` (`docs/runtime_reference.go:134-149`) stays
the off revision. `runtimeref.AutopilotGuideRevision(limit, st)` hashes the
off revision, the bytes of `docs/agent/autopilot.md`, the instruction and
topic replacement tables of `docs/autopilot.go`, and the IDs in `st.Allowed`
and `st.Guide`, each length-prefixed as `GuideRevision` does. Tool
descriptions are not part of it, as today. `EffectiveGuideRevision(limit, st)`
(5.9) returns one or the other and is used by the `guide` output,
`check_client`, `get_client_skill` and `GET /api/client`
(`internal/api/client.go:11-23`), so all of them report the revision of the
guide a client would read with the same state. A switch or guide change while
on therefore makes clients see `guide_status: refresh_required`, and turning
Autopilot off returns them to today's revision.

### 5.7 `check_client`

The off server registers `check_client` with today's `clientstate.CheckResult`
(`internal/mcpx/client.go:22-37`). The on server registers it with

```go
type checkResultOn struct {
    clientstate.CheckResult
    Autopilot autopilotInfo `json:"autopilot"`
}
type autopilotInfo struct {
    Enabled bool     `json:"enabled"`
    Allowed []string `json:"allowed"`
    Read    string   `json:"read"` // "guide topic.autopilot"
}
```

so the off output schema and result keep their bytes (rule 23, rule 24).
Existing clients keep working: every existing field keeps its name, type and
meaning, the new member is additive, and the clients in this repository
decode the result without `DisallowUnknownFields`. A client that read the off
guide and then calls `check_client` while Autopilot is on gets
`guide_status: refresh_required`, which is the existing signal to read the
guide again (`docs/agent/topics/client-update.md`).

### 5.8 Off-state identity

Principle 2 holds by construction (the off server and off pages come from
today's code) and is pinned by tests (7.3).

### 5.9 Rendering contract and state ownership

Core owns the authoritative settings (2, 3.1). MCP takes one copy per HTTP
request (5.1) and passes it to every renderer as an argument. `runtimeref`
keeps no Autopilot state: its functions are pure over their arguments and
the embedded files, so two overlapping requests with different copies each
render consistent pages and revisions.

```go
package runtimeref // docs/autopilot.go; imports nothing from internal/core

type AutopilotOperation struct {
    ID       string // "deploy"
    Label    string // "Deploy"
    Requests string // "deploy of a saved version (activation or redeploy)"
}

// AutopilotState is what the on texts depend on. A nil *AutopilotState
// means Autopilot is off.
type AutopilotState struct {
    Allowed []AutopilotOperation // in the order of 2.2; empty when none
    Guide   string
}

func AutopilotInstructions(uploadLimit int64) string
func AutopilotTopicOverlay(name string) (string, bool) // on text of an existing topic; static
func AutopilotPage(st AutopilotState) string            // topic.autopilot
func AutopilotRefusal(category string) (string, bool)  // autopilot_off, autopilot_not_allowed
func AutopilotRefusalCategories() []string
func AutopilotGuideRevision(uploadLimit int64, st AutopilotState) string
func EffectiveGuideRevision(uploadLimit int64, st *AutopilotState) string // nil: GuideRevision
```

```go
package core

type AutopilotOpInfo struct{ ID AutopilotOp; Label, Requests string }
var AutopilotOpInfos []AutopilotOpInfo // the rows of 2.2 with the requests column of rule 6

// GuideState converts a copy for the renderers: nil when Enabled is false.
func (a Autopilot) GuideState() *runtimeref.AutopilotState
```

```go
package mcpx

type requestState struct {
    Autopilot core.Autopilot            // the copy of 5.1
    Guide     *runtimeref.AutopilotState // Autopilot.GuideState()
    Arrival   core.Arrival              // 3.6
}
func stateOf(ctx context.Context) requestState

func guide(items []string, uploadLimit int64, st *runtimeref.AutopilotState) GuideOut
func guidePage(id string, uploadLimit int64, st *runtimeref.AutopilotState) (string, bool)
func clientManifest(ctx context.Context, svc *core.Service) clientstate.Manifest // revision from stateOf(ctx)
```

`clientstate.NewManifest` keeps its signature; callers set
`m.GuideRevision = runtimeref.EffectiveGuideRevision(limit, st)` after it.
`GET /api/client` uses `s.Svc.Autopilot().GuideState()`, its own single read.
With `st` nil, `guide` and `guidePage` run today's code unchanged.

## 6. Console UI

### 6.1 Experiments page (`settings.js`)

- `SECTIONS` (`internal/console/static/assets/js/settings.js:11`) gets
  `['experiments', 'Experiments']` after Agents; `mount` routes it to
  `experimentsPage(page)` (rule 1).
- `pageHead` with title "Experiments" and intro: "These features may change
  or be removed. Only people change this page: agents must never change it,
  including through a browser or by calling the console. Flats cannot enforce
  that. The console has no login and only refuses requests from other
  websites, so a program that can reach this address can do what this page
  does." (rule 2)
- Group "Autopilot", hint "Agents approve or reject pending approvals for the
  operations you allow." One row with an On/Off `pill` and a Turn on or Turn
  off button, styled like `providerToggle`
  (`internal/console/static/assets/js/settings.js:429-447`).
- Off: only that row (rule 9).
- Turn on opens `confirmDialog` (`internal/console/static/assets/js/ui.js:9`)
  titled "Turn on Autopilot?" listing the allowed operations by label, or
  "No operation is allowed yet; agents can decide nothing until you turn one
  on.", then an `alert-warn` paragraph "Anyone on the internet can open a
  Public flat." and "Agents follow the operation guide, but Flats does not
  enforce it." The dialog lists them in the display order below. Confirm
  sends `confirmed_allowed` with exactly the listed IDs, in the order the
  server returned them.
  A 409 shows `infoDialog` with the server message and reloads the page
  (rule 10).
- Turn off sends `{enabled: false}` at once, without a dialog, and toasts
  "Autopilot is off." (rule 10).
- On: the row shows Turn off, followed by group "Operations agents may
  decide" with nine rows from an `OPERATIONS` table (group, ID, label,
  description), each with a `role="switch"` checkbox like the staged toggle
  (`settings.js:1383`), and group "Operation guide" (rule 11). The rows are
  under three sub-headings, in this display order, which differs from the
  bit order of 2.2:
  - "Versions and deployments": `create_version`, `deploy`, `rollback`,
    `delete_version`;
  - "Visibility": `make_public`, `make_private`, `public_editing`;
  - "Data and deletion": `rollback_restore_data` (tag "replaces data"),
    `delete_flat` (tag "permanent").

  The confirmation dialog and the Agents page (6.2) list allowed operations
  in the same display order.
- A switch acts on `change`: it is disabled during the request; on success it
  takes the returned value and toasts "Agents may now decide Deploy." or
  "Agents can no longer decide Deploy."; on failure it returns to the stored
  value and toasts the error message with kind `error` (rule 12). Switches
  are not part of a save bar. The page sends switch requests one at a time,
  in the order the switches changed, so each response is at least as new as
  the one before it and an older response cannot undo a newer committed
  change. A switch whose request has not answered yet keeps the value the
  operator chose until its own response; every other switch takes the value
  of each response.
- The guide is a `textarea` with a counter "812 / 4,000 characters" counted
  as code points (`[...text].length`), matching the server. Save is enabled
  while the text differs from the stored guide and is within the limit; over
  the limit the counter has the error style and Save stays disabled. A
  success toasts "Operation guide saved." and stores the new value. Leaving
  the page drops unsaved text, as the console's other forms do (rule 13).
- Descriptions of the nine switches:

| ID | Description |
|---|---|
| `create_version` | Save a Draft as a new numbered version (`create_version`). |
| `delete_version` | Delete a saved version that is not live (`delete_version`). |
| `deploy` | Activate or redeploy a saved version (`deploy`). |
| `rollback` | Activate an earlier version and keep the current data (`rollback`). |
| `rollback_restore_data` | Roll back and replace the live data with the snapshot from before the current version (`rollback` with `restore_data`). |
| `make_public` | Change a flat from Private to Public, where anyone on the internet can open it. |
| `make_private` | Change a Public flat back to Private. |
| `public_editing` | Let anyone on the internet edit a Public board, or stop it. |
| `delete_flat` | Permanently delete a flat with its versions and data. |

`api.js` gets `autopilot()`, `setAutopilot(enabled, confirmedAllowed)`,
`setAutopilotOperation(id, allowed)` and `setAutopilotGuide(guide)` on the
routes of 4.1.

### 6.2 Agents page

`agentsPage` (`settings.js:1711-1767`) becomes async and loads `api.autopilot()`.
If that fails it draws today's page with an error notice for the Autopilot
parts.

- Intro, off: today's text, "Agents can create flats, save Drafts, open
  previews and read logs. Saving versions, deploying, visibility changes and
  deleting wait for your approval under Approvals." (`settings.js:1751`).
  On, the second sentence is replaced so that the introduction itself states
  the exception (rule 25):
  - with at least one operation allowed: "Agents can create flats, save
    Drafts, open previews and read logs. Autopilot is on: agents decide
    requests of the operations under What agents can decide themselves. Every
    other request to save a version, deploy, change visibility or delete
    waits for your approval under Approvals.";
  - with none allowed: "Agents can create flats, save Drafts, open previews
    and read logs. Autopilot is on, but it allows no operation yet, so every
    request to save a version, deploy, change visibility or delete waits for
    your approval under Approvals."

  "Approvals" stays the link it is today in every variant.
- On: the title row shows `pill('Autopilot on', 'on')`, and a group "What
  agents can decide" lists the allowed operations (or "None yet") with a
  button to `/settings/experiments`.
- "What the agent tools can't do": the "Approve requests" row stays as today
  while off. While on it reads "Decide other requests: agent tools decide only
  the operations listed above; you decide the rest under Approvals." A new
  row, shown in both states: "Change Experiments: no agent tool changes
  Experiments. Agents are told never to change them, also not through a
  browser; Flats cannot tell them apart from you."

### 6.3 Approval page

`draw` (`internal/console/static/assets/js/approval.js:76-95`) adds a
"Decided by" row after "Decided" when `decided_by` is not empty (rule 27):

- `console`: "the console";
- `netbird:<fqdn>`: "NetBird peer <fqdn>", as recorded today;
- an `autopilot:` value that parses as in 3.4: `Autopilot: agent "pr-bot" via
  Tailscale (bot@example.com)`, `... via Tailscale node ci-runner`,
  `... via Local`, `... via the console listener from 192.168.1.20`,
  `... via NetBird peer ci.netbird.cloud`, `... via external origin https://...`;
- anything else, including an unparsable `autopilot:` value: the stored text.

All values are inserted as text nodes. The approvals list does not change.

## 7. Tests

### 7.1 Unit

| Test | Covers |
|---|---|
| `store`: migration 20 on an empty and a v19 database; the row and defaults; `CHECK`s | rules 1, 3, 5-7 |
| `store`: `ClaimApprovalAutopilot`/`RejectApprovalAutopilot` with Autopilot off, a missing bit, a non-pending approval; a setter and a claim racing in two goroutines, repeated, never leave a claimed approval whose bits were off at commit | rule 17 |
| `core`: `AutopilotNeeds` table test: every action of the rule 6 table, legacy `publish`, `restore_data`, rollback with and without `restore_data`, every `set_visibility` combination of from, to, level, `AccessSeq` nil or set, missing `From`, unknown values, invalid JSON, non-object params (`null`, array, string, number), a top-level `null` member, a wrong field type for every action (for example `{"version":"broken"}` for `activate`, `{"restore_data":"yes"}` for `rollback`, `{"access_seq":"x"}` for `set_visibility`), unknown members ignored, unknown action | rule 6 |
| `core`: `set_visibility` raw values: `private`, `public`, `public-listed` and `public-unlisted` classify for `from` and `visibility`; `bogus`, `Public` (case differs), `""`, `"private "` and a number are unclassified in either field, including `{"from":"public","visibility":"bogus"}` and `{"from":"bogus","visibility":"private"}` | rule 6 |
| `core`: an approval with each unreadable form above, and each invalid visibility value, decided by an agent through `approve_request` and through `reject_request`, is refused with `autopilot_not_allowed` while every switch is on, and the stored approval row is byte-identical afterwards | rules 6, 17 |
| `core`: `DecideAsAgent` refusals in order: blank and 101-code-point agent, off, unknown id, each missing operation (approve and reject), unclassified approval | rules 16, 17 |
| `core`: rule 20 matrix for agent and operator decisions: same direction returns unchanged with no write, opposite direction and `applying` are `conflict`; the operator path writes today's `decided_by` and event text byte for byte | rules 20, 21 |
| `core`: a stale `set_visibility` and a stale `activate` approved by an agent fail as `stale_approval`, as for the operator | principle 3 |
| `core`: `EncodeAgentDecider`/`ParseAgentDecider` round trip for each network (local, listener with IPv4 and IPv6, tailscale with login and with node, netbird with and without peer, external); operator values never parse; `DeciderText` for each; a name with a newline stays on one event line | rules 26, 28 |
| `core`: `resumeApplying` after a crash between claim and apply finishes an agent-claimed approval, writes the Autopilot event and applies with `via autopilot` | rules 17, 26, 28 |
| `core`: crash after application and before finalization, using the existing `after_live_before_finalize` phase hook (`internal/core/lifecycle.go:652`) and a stop after the `applied` receipt, for `create_version`, `delete_version`, `activate`, `rollback` and `set_visibility`: `resumeApplying` finalizes each, writes exactly one Autopilot decision event; a second `resumeApplying` and a console decision afterwards add none; the same crash after an operator decision gives today's approval row and events | rules 26, 28 |
| `core`: for each action, an agent-decided and an operator-decided approval give the same flat state and `result_data`; their events and host log lines differ only in `via autopilot` versus `via console` and the decider text (3.5) | rule 28, principle 3 |
| `core`: setters log the lines of 3.1 with `Logf` captured; the guide line has no guide text; no-op calls log nothing | rule 4 |
| `core`: with store test hooks, for each setter: the request context is cancelled right after `BEGIN IMMEDIATE`; the commit succeeds but `UpdateAutopilot` reports an error; the commit succeeds and the following `AutopilotSync` also fails. In every case the next `Autopilot()` (once the store answers) returns the committed row, and the committed change's line is logged exactly once with its original `who` and time and without guide text; with marking failing, it is logged again at the next sync, never lost; while the store cannot be read, `Autopilot()` reports `Enabled` false | rules 4, 15 |
| `core`: the round-2 sequence: Turn on commits but reports an error and the sync read fails, so the cache still says off; then Turn off. Turn off reads the stored row (on), writes off and logs; afterwards the row, the next `Autopilot()` and the next MCP `tools/list` are all off, an agent decision is refused with `autopilot_off`, and both lines (on, then off) are in the log with their actors | rules 4, 15, 17 |
| `core`: a change committed and not logged before a simulated crash is logged by the sync in `core.New` at the next start | rule 4 |
| `store`: two `UpdateAutopilot` calls on separate goroutines changing different bits keep both bits and append two change lines | rules 4, 12 |
| `mcpx`: `hostOrigin` with an injected `hostAddrs`: IPv4 and IPv6 loopback, the host's LAN IPv4, the host's global IPv6, an IPv4-mapped host address, the host's link-local address with its zone are `local`; another machine's IPv4, IPv6 and link-local address (and the host's link-local address with another zone) are `listener` with that address and zone; forwarding headers never change the result | rule 26 |
| `core`: approving a Private to Public change does not grant a provider; the flat is reachable only through permitted providers | principle 5, rule 33 |

### 7.2 Integration (real HTTP)

Over the `newEnv` harness (`internal/mcpx/mcpx_test.go:39-80`), with a
management API server for console calls:

- A console `PUT`, sent with the headers the console page sends, turns
  Autopilot on; the next MCP
  `tools/list` has `approve_request` and `reject_request`; after Turn off the
  next list has neither, and a client still calling `approve_request` through
  the on server of an earlier list is refused (rules 14, 15, 17).
- `create_version`, `deploy`, `set_visibility` and `delete_flat` requested over
  MCP, then approved over MCP by another session; results equal `get_approval`;
  the flat changes; events say "by Autopilot (agent ...)" and the applied
  operation's events say `via autopilot`; after `delete_flat` the approval row
  still has `decided_by` (rules 18, 20, 26, 28).
- The same approval decided in the console and by an agent concurrently:
  exactly one wins, the other gets the approval unchanged or `conflict`
  (rules 20, 21).
- Arrival: the loopback env records `local`; a test server bound to
  `0.0.0.0` reached through one of the test machine's non-loopback addresses
  records `local` (skipped when the machine has none); the env's server that
  sets `RemoteAddr` to `100.64.0.7` records `listener` with that IP and can
  use both tools; a loopback connection carrying `X-Forwarded-For` still
  records `local`; a handler wrapped with
  `core.WithConsoleDomain` and `core.WithTailnetPeer` records `tailscale` with
  login, and with a tagged peer the node; one wrapped with
  `core.WithConsoleDomain` and `core.WithNetbirdPeer` records `netbird` with
  the peer, and without a peer the network only; a `HostGuard` with an
  external origin records `external` and the origin (rules 19, 26).
- `get_approval` while on: a pending approval whose operations are all allowed
  has the Autopilot sentence; one needing an operation that is not allowed,
  and an unclassified one, have today's text; turning one of its operations
  off restores today's text on the next call (rule 23).
- `/api/experiments/autopilot` answers 404; `/console/api/...` without
  `X-Flats-Console`, with `X-Flats-Client`, or cross-origin answers 403; in
  both MCP states no tool reads or changes Experiments (rule 3).
- Turn on with a stale `confirmed_allowed` answers 409 and changes nothing
  (rule 10).
- `check_client` while on reports `autopilot` and a different
  `guide_revision`; changing a switch changes it again; off restores today's
  value; `/api/client` agrees (rule 23, 5.6).
- Existing-client compatibility: a client that read the off guide calls
  `check_client` with today's input, including that `guide_revision`, while
  Autopilot is on. The result decodes into today's `clientstate.CheckResult`
  and the CLI's result type with every existing field unchanged in name, type
  and meaning, `guide_status` is `refresh_required`, and after reading the
  guide again with the new revision it is `current` (5.7).
- Overlapping requests: a test hook in `Handler` pauses one MCP request after
  it took its copy while the console changes a switch and the guide, then a
  second request runs, then the first resumes. Each response's
  `topic.autopilot` lists the operations and guide of its own copy, and its
  `guide_revision` equals `AutopilotGuideRevision` of that copy. A unit test
  also runs `guide` from many goroutines with two different states and checks
  the same pairing (5.9).

### 7.3 Off-state identity

- Never on: initialize instructions equal `runtimeref.Instructions(limit)`;
  every `guide` item (all of `guideItems()`, plus `topic.autopilot`), tools/list
  JSON, `check_client` and `get_client_skill` results, both resources, and the
  llms documents are captured.
- The same captures after turning Autopilot on, setting all nine switches and
  a guide, and turning it off again are byte-identical to the never-on capture
  (principle 2, rules 8, 24, 30). They include `get_approval` of a pending
  approval whose operations are all allowed, so its off text is checked too.
- With Autopilot off, operator decisions of every action keep today's
  `decided_by`, approval events and `via console` texts.
- In the testing stage, the same captures from a `main` binary and from the
  candidate with Autopilot off are diffed once and the result is recorded in
  the task report.

### 7.4 On-mode texts

- Every replacement `old` occurs exactly once in its off text (5.4).
- Census: in the on instructions, every on tool description and input schema,
  and every on guide page, none of these phrases appears: `no approval tool`,
  `no approval or provider-grant tool`, `approve your own`, `Ask the user
  before`, `ask the user first`, `Agents never decide`, `operator approval`,
  `operator's approval`, `until the operator approves`, `wait for the
  operator`, `waits for the operator` (rule 22).
- `topic.autopilot` lists exactly the allowed operations, contains the guide
  in full inside one fence (including a guide containing triple backticks and
  `#` lines), and says it takes precedence over the fixed references (rule 23).
- `TestInstructionsStaySmall` for both variants (5.4).
- `TestRefusalPagesCoverEveryCategory` (`guide_test.go:76`) stays for
  `ErrorCategories`; a new test requires an on refusal page for each of
  `AutopilotErrorCategories`.

### 7.5 Console UI

`internal/console/console_experiments_test.go` runs
`testdata/experiments_test.mjs` with `ui_test.mjs`, as
`console_usage_test.go` does:

- off: Turn on visible, no switches or guide (rule 9);
- Turn on opens one dialog listing kept operations or "No operation is
  allowed yet", the Public and enforcement sentences; confirming sends
  `confirmed_allowed`; a 409 reloads (rule 10);
- Turn off sends at once with no dialog (rule 10);
- a switch change sends one `PUT`; a failure restores the switch and toasts
  the message (rule 12);
- a second switch changed while the first one's request is still running
  waits for it, keeps its chosen value meanwhile, and the switches end equal
  to the stored operations (6.1);
- the counter counts an emoji as one; Save is disabled when unchanged or over
  4,000 (rule 13);
- the nine `OPERATIONS` IDs equal the server's list as a set (a fixture
  taken from `core.AutopilotOps` through a Go-generated JSON file in the
  test's temp directory), and the switches appear under the sub-headings and
  in the display order of 6.1;
- Agents page: the exact introduction text off, on with operations allowed
  and on with none allowed, the pill and groups of 6.2 (rule 25);
- approval page "Decided by" for each value form of 6.3, including the console
  listener with an IP, a NetBird peer, and a name with `<b>` shown as text
  (rule 27).

### 7.6 Browser check

On a disposable development host with synthetic data: open Settings ›
Experiments, turn Autopilot on through the dialog, flip switches including a
forced failure (stop the host mid-request), save a guide near the limit, check
the Agents page, have an MCP client on loopback approve a `create_version` and
reject a `set_visibility`, and open both approvals to see "Decided by" and
the flat's events with `via autopilot`. Repeat one decision from another
machine to a non-loopback bound console listener and check that it is
recorded with that machine's IP. Check the host log lines. Screenshots use synthetic names only.

## 8. Implementation units

Unit A defines the Go contract; B, C and D build against it and can start
from stubs with these signatures.

| Unit | Owns | Contract it provides or uses |
|---|---|---|
| A: store and core | `internal/store/autopilot.go` (+test), `internal/store/migrate.go` (one entry), `internal/core/autopilot.go` (+tests), `internal/core/service.go` (`Decide` split), `internal/core/lifecycle.go` (event text, `applyVia`, `ErrorCategory`), `internal/expose/tsnet/tsnet.go` (peer context), `internal/app/consoledomains.go` (`core.WithConsoleDomain`, `core.WithNetbirdPeer`) | provides 2.3, 3.1-3.6: `Autopilot`, `OpSet`, `AutopilotOps`, setters, `AutopilotNeeds`, `DecideAsAgent`, `AgentDecision`, `Arrival`, `EncodeAgentDecider`, `ParseAgentDecider`, `DeciderText`, `WithConsoleDomain`/`ConsoleDomainOf`, `WithTailnetPeer`/`TailnetPeerOf`, `WithNetbirdPeer`/`NetbirdPeerOf`, `ViaAutopilot`, `AutopilotOpInfos`, `Autopilot.GuideState`, the two errors and `AutopilotErrorCategories` |
| B: console API | `internal/api/experiments.go` (+test), `internal/api/api.go` (route lines, `statusOf` cases) | uses A's setters; provides the HTTP contract of 4.1 |
| C: MCP and agent texts | `internal/mcpx/autopilot.go` (+tests), `internal/mcpx/mcpx.go`, `internal/mcpx/tools.go` (description hook, `get_approval` text), `internal/mcpx/client.go`, `internal/mcpx/guide.go`, `internal/mcpx/guide_test.go`, `docs/autopilot.go`, `docs/agent/autopilot.md`, `internal/api/client.go` (guide revision only) | uses A's `Autopilot()`, `GuideState`, `DecideAsAgent`, `DeciderText`, `Arrival`; provides 5, including the renderers of 5.9 and `hostOrigin` of 3.6 |
| D: console UI | `settings.js`, `approval.js`, `api.js`, `app.css`, `internal/console/console_experiments_test.go`, `testdata/experiments_test.mjs` | uses the HTTP contract of 4.1 and the `decided_by` format of 3.4 |

`internal/api/client.go` belongs to C so that one unit owns every guide
revision consumer. The two `runtimeref` types of 5.9 are fixed by this page:
A compiles `GuideState` against a stub with exactly those definitions until
C's `docs/autopilot.go` replaces it. The integrated candidate then gets the tests of 7.2, 7.3
and 7.6, which no single unit can run alone.

## 9. Compatibility, rollback and security

- **Upgrade.** Migration 20 adds the row with everything off, so upgraded
  hosts behave as before (rule 1). Existing approvals, including legacy
  `publish` and pre-level `set_visibility` approvals, classify as in 3.2.
- **Downgrade.** An older Flats refuses schema 20 (`ErrSchemaTooNew`,
  `internal/store/migrate.go:19-20`). Going back means restoring a
  pre-migration backup, which `migrate` keeps (three, `backupsKept`); agent
  decisions made since are lost with it. Turning Autopilot off is the
  supported way to stop it without a downgrade.
- **Stopping without the console.** There is none by design (rule 3); stop
  Flats.
- **Records.** `decided_by` values written by Autopilot are plain strings, so
  older clients and the CLI show them as given.
- **The boundary is an instruction.** The console has no login, and
  `consoleRequest` only keeps other websites out. An agent on the host, or any
  program that can reach a console address, can send console requests and
  turn Autopilot on, change switches or approve in the console. The spec
  accepts this (rule 2); the texts tell agents not to, and the host log
  records every settings change. Run only trusted agents on hosts that can
  reach the console.
- **Agent names.** Untrusted text. They are bounded to 100 code points,
  quoted with `%q` in events, inserted as text in the console, and never
  written to the host log.
- **Operation guide.** Operator text delivered to agents; it is fenced in
  `topic.autopilot` and not logged. Flats does not enforce it (principle 7).
- **No new reach.** Decisions go through the same apply functions, which keep
  provider permission, public edit preflight and stale checks (principle 5,
  rule 33).

## 10. Docs at ship time

When the feature ships, the durable parts of this page move into the
architecture docs and this page is removed (see the
[spec conventions](README.md)): the routes, MCP tools and the Experiments
screen into [Surfaces](../surfaces.md), the approval trust model and
`decided_by` into [design.md](../design.md), the `autopilot` table into
[configuration.md](../configuration.md). `autopilot.md` changes its status and
"Current state".

## 11. Open questions for the product owner

None. The four questions of the first draft were decided and are now spec
rules 19, 23, 26 and 28.
