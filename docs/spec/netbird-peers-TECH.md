# NetBird peers per flat and console domain: technical design

Technical design of [netbird-peers.md](netbird-peers.md). "Rule N" refers to
that spec; "CS rule N" to [console-settings.md](console-settings.md).

## NetBird module (`netbird/`)

### Layout

```
<data>/netbird/
  peers/<name>/netbird.json    embed config: the peer's WireGuard identity (0600)
  peers/<name>/dns.json        embed state file
  legacy-peer.json             notice of a dropped shared peer (rule 20)
```

The shared-peer files (`account.json`, `netbird.json`, `netbird-dns.json`,
`ports.json`) are legacy and removed by `CleanupLegacy`.

### `Net`

`Config{Dir, ManagementURL, SetupKey func() string, ShutdownTimeout, Logf}`.
`SetupKey` returns the key currently stored (empty when none); it is read at
every peer start, so a key registered in the console applies to the next
peer without restarting anything (rule 12).

A `peer` holds one embedded client and up to three roles on it:

| Role | Listener | Methods |
|---|---|---|
| private | overlay `:80`, plain HTTP | `ServePrivate`, `StopPrivate`, `RetirePrivate`, `PrivateURL`, `PrivateStatus` |
| public | overlay `:8080`, published with `Expose` | `ServePublic`, `StopPublic`, `RetirePublic`, `PublicURL`, `PublicStatus` |
| console | overlay `:80` | `ServeConsole`, `StopConsole`, `RetireConsole`, `ConsoleURL`, `ConsoleStatus` |

A name is a flat or a console domain, never both (rule 1), so private and
console never share a peer. The per-role run loop (listen, expose, keep-alive,
backoff) stays as today; only the port allocation goes away.

- **Start.** The first role of a name starts its peer under the peer's lock:
  `embed.New` with `DeviceName = name`, `ConfigPath` and `StatePath` in
  `peers/<name>/`, `ManagementURL`, and
  - `SetupKey` = the current key when one is stored. embed reuses the identity
    in `ConfigPath` and logs in with it; only when management answers that the
    peer is unknown does it register with the setup key, keeping that identity
    (rule 5);
  - otherwise, when `ConfigPath` holds an identity, `PrivateKey` = that key, as
    the shared peer did. A management rejection is reported as
    `ErrPeerRejected` ("NetBird no longer recognises this peer; register a
    setup key in Settings > Keys"), recognised as in PR #103 (the
    `InvalidArgument` registration fallback with an empty setup key);
  - otherwise `ErrNoSetupKey`.

  The key is scrubbed from every returned error.
- **Process-wide start lock.** `embed.New` sets `NB_DNS_STATE_FILE` in the
  process environment and the engine reads it during `Start`. Peers therefore
  start one at a time under a package mutex held from `embed.New` until
  `Start` returns (bounded by `startTimeout`). `logrus` output and level are
  process-wide as well and set identically for every peer. The WireGuard
  port is 0 (random), so peers do not compete for 51820.
- **Stop.** When a peer's last role stops, its client stops; its directory
  stays.
- **Retire.** `Retire*` stops the role; when the peer then has no role left,
  its client stops and `peers/<name>/` is deleted (rule 6). Retirement
  never contacts NetBird to delete the peer: the embedded client cannot.
- **Assigned name.** `Status().LocalPeerState.FQDN` gives the assigned FQDN;
  its first label differing from the name is reported as `assigned` (rule 1).
- **Identity.** `IdentityForIP(name, ip)` wraps `embed.Client.IdentityForIP`
  for console decisions (rule 17).
- **Close** stops every role, then every client.

### Driver (`provider.NetbirdDriver`)

`New(NetbirdConfig) (NetbirdNet, error)`, `RetireStored(dir, name) error`
(delete `peers/<name>/` without a backend), `CleanupLegacy(dir) (LegacyPeer,
bool, error)`, `DisconnectHint()` removed. `LoadAccount`, `Enroll` and
`Disconnect` are removed.

## Provider manager

`NetbirdNet` keeps the private and public methods (ports are gone, so
"Retire" now means "delete the peer identity") and gains the console methods,
`SetSetupKey(string)` and `PeerCount() int`. `Account()` is removed: the
approval token's fingerprint comes from the stored key (below). The offline
path in `StopSlug` calls `RetireStored(dir, host)`, which deletes the peer
directory.

## Host (`internal/app`)

### Setup key

- `host_keys` row `netbird` now holds the key value permanently, like
  `tailscale` (rule 9). `resolveNetbirdKey` applies the same seed rule as
  `resolveTailscaleKey` (flag `--netbird-setup-key-file`, then
  `FLATS_NETBIRD_SETUP_KEY[_FILE]`, then the stored row) and reports
  `external_differs`. The NetBird branch of `enrollWaiting` and
  `netbirdEnrollable` are removed.
- `core.CheckHostKeyValue(netbird, key)` requires a UUID (rule 11).
- Console API: `PUT /console/api/keys/netbird {"key"}` and
  `DELETE /console/api/keys/netbird`, implemented as for Tailscale; both call
  `netbirdNet.SetSetupKey`. `PUT/DELETE /console/api/providers/netbird/account`
  and `ConnectNetbird`/`DisconnectNetbird` are removed.
- `flats key set netbird` stores the key (not a waiting enrollment).
- Keys card fields for NetBird: `set`, `source`, `updated_at`,
  `external_differs`, `management_url`, `peers` (running peer count), and
  `legacy_peer` (rule 20).
- The approval policy token is `management_url + "\x00" +
  hex(sha256("flats-netbird-key:" + key))[:32]` while a NetBird provider is
  granted (rule 13); it is recomputed when the key changes.

### Backend

The NetBird backend is created whenever `netbird` or `netbird-expose` is
permitted and the binary links NetBird, with or without a key: routes then
report `ErrNoSetupKey` until one is stored.

### Legacy shared peer (rule 20)

At start, before the backend is built, `CleanupLegacy` finds `account.json`,
writes `legacy-peer.json` (device name, management URL), and deletes the
legacy files. A `host_keys` row `netbird` without a value (the old connection
record) is deleted; one with a value (a waiting enrollment) becomes the stored
key. The console shows the notice on the NetBird panels and the Keys card
until the operator dismisses it
(`DELETE /console/api/providers/netbird/legacy-peer`).

### Console domains, both providers

- **Store.** Migration `user_version` 14 rebuilds `console_nodes` with a
  `provider TEXT NOT NULL DEFAULT 'tailscale' CHECK (provider IN ('tailscale',
  'netbird'))` column and primary key `(provider, name)`; existing rows become
  Tailscale rows.
- **Backends.** `consoledomains.go` works on a small interface implemented for
  Tailscale (the existing `tsNet` calls) and NetBird (`ServeConsole`,
  `RetireConsole`, `ConsoleURL`, `ConsoleStatus`):

  ```go
  type consoleBackend interface {
      Serve(ctx context.Context, name string, h http.Handler) error
      Retire(name string) error
      URL(name string) string
      State(name string) (core.HostInfo, bool)
  }
  ```

  Every map in `consoleState` is keyed by `(provider, name)`. The request
  context records both, so `ConsoleURLFor` returns the domain a request came
  through.
- **Reservation.** `consoleReserved` is the union of both lists and every
  `console_nodes` row.
- **Handler.** A NetBird console domain is `HostGuard` (the name, the
  assigned FQDN and its first label; ports `""` and `80`) →
  `withConsoleDomain` → `api.PlainHTTPConsole` (rule 19) →
  `api.WithApprover(netbird identity)` (rule 17) → the management mux.
- **Order.** `ConsoleURL` prefers the first served Tailscale domain (HTTPS),
  then the first served NetBird domain, then loopback.
- **Delete (rule 15).** The last-domain check counts both lists; the redirect
  candidate for a self-delete is any ready domain, Tailscale first.
- **Provider off.** `SetNetworkProvider` refuses to turn `netbird` off while
  `netbird.console_hosts` is not empty, as for Tailscale.
- **API.** `GET /console/api/console-domains` returns, per provider, the
  existing view (`domains`, `retiring`, `on`, `pinned`), plus `via`
  (`{provider, name}`), `loopback`, `pending_approvals` and `etag`.
  `POST /console-domains {"provider", "name"}` and
  `DELETE /console-domains/{provider}/{name}`. `--console-host` still pins the
  Tailscale list only.

### CSRF guard over HTTP (rule 19)

`api.PlainHTTPConsole(next)` marks the request context. `consoleRequest`
accepts a mutation on a marked request when `Sec-Fetch-Site` is absent, the
`Origin` header is present and `sameOrigin` holds. When `Sec-Fetch-Site` is
present, it must be `same-origin`, as everywhere. The other conditions
(`X-Flats-Console: 1`, no `X-Flats-Client`, `HostGuard`'s Host and Origin
checks) are unchanged, so a page on another origin, including another flat
on the same NetBird domain, still needs a CORS preflight that the server
never grants. Only the NetBird console handler is wrapped; the loopback
listener and Tailscale domains keep the strict rule.

## Configuration (`schema_version` 3)

- `netbird.console_hosts`: list, default `[]`, each item matching
  `ConsoleHostRE`, no duplicates. A non-empty list needs `netbird` in
  `network.permitted`.
- `netbird.device_name` is removed. Migration 2→3 deletes it; it changes no
  network or credential meaning, so it needs no confirmation.

## Console

- NetBird panels: no setup key form; the key lives on the Keys card, with the
  recommendation of rule 10 and a link to the NetBird dashboard's setup keys.
  The panels show the running peer count, the HTTP limits (rule 18) and the
  legacy notice.
- Console domains card: one section per provider, each with its list, add
  form and state; NetBird domains show their `http://` URL.

## Documentation

`docs/networking.md`, `docs/network-provider-lifecycle.md`,
`docs/configuration.md`, `docs/container.md`, `docs/lifecycle-console.md`
and console-settings.md (CS rules 7 and 9, the NetBird open question) follow
this design.

## Tests

- `netbird`: fake clients for per-name peers; start with a key, with only an
  identity, with neither; rejection without a key; key scrubbing; retirement
  deletes the directory only when no role is left; console role; assigned-name
  reporting; the start lock serialises starts; legacy cleanup.
- `internal/app`: key seeding, replacement and removal for NetBird; the policy
  token follows the key; console domains on both providers (add, delete,
  last-domain rule across providers, self-delete redirect, provider-off
  refusal); legacy notice; store migration 14; config migration 2→3.
- `internal/api`: the HTTP console guard accepts a same-origin mutation
  without `Sec-Fetch-Site` only on a marked request and refuses missing or
  foreign `Origin`, missing custom header and `same-site`/`cross-site`.
- Console UI tests for the Keys card and both console domain sections.
