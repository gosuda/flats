# Flats

Self-hosted, agent-agnostic web hosting. Each hosted site is a **flat**:
coding agents (Claude Code, Codex, Cursor, …) create and deploy flats over
MCP, a CLI or an HTTP API, and you manage them in a web console that only your
tailnet can reach. Private flats are served over Tailscale; public flats go
out through [Portal](https://github.com/gosuda/portal-tunnel).

Everything ships as one CGO-free Go binary. macOS (Apple Silicon) first.

## What you get

- **Versions**: every upload is an immutable version (content hash, optional
  git commit and dirty flag). Saving never changes what is live.
- **Deploy with a health check**: a version goes live only after
  `GET /` (or the manifest's `health` path) succeeds; a failed deploy keeps the
  old version serving. Rollback is one call.
- **Previews**: open any saved version at its own temporary address
  (`<flat>-<random>`), closed on the next deploy or after 24 hours unused.
- **Private by default**: each flat is its own Tailscale node,
  `https://<flat>.<tailnet>.ts.net`, so flats never share an origin.
  Tailscale identity headers (`Tailscale-User-Login`, `Tailscale-User-Name`)
  tell a flat who is visiting.
- **Public when you approve it**: `public-listed` or `public-unlisted` through
  Portal. Agents can only *request* it; you approve in the console.
  *Unlisted is not access control*: anyone with the URL can open it.
- **Server flats**: a JavaScript `fetch` handler (QuickJS on WebAssembly) or a
  WASI module, with a per-flat SQLite database, file store and secrets.
- **Web console**: flat list, per-flat settings (versions, previews, logs,
  secrets, usage, rename, delete), system settings and the approval queue.

## Install and run

```sh
go build -o flats ./cmd/flats          # CGO_ENABLED=0 works
./flats serve                           # foreground
./flats install                         # or: run at login with launchd (macOS)
```

`flats serve` options:

| Flag | Default | Meaning |
|---|---|---|
| `--data` | `~/Library/Application Support/Flats` (`$FLATS_DATA`) | data directory |
| `--listen` | `127.0.0.1:7878` | loopback address for the CLI, local agents and the console |
| `--network` | `tailscale` | `local` serves flats at `http://<flat>.localhost:7879` without Tailscale (development) |
| `--authkey-file` | | file with a reusable, untagged Tailscale auth key (or `TS_AUTHKEY`; otherwise each node prints a login URL) |
| `--console-host` | `flats` | tailnet host name of the console (`https://flats.<tailnet>.ts.net`) |
| `--portal` | `true` | enable public flats |
| `--relays` | Portal default | comma-separated Portal relays |
| `--runtime` | `true` | enable server flats |

Tailscale requirements: MagicDNS and HTTPS certificates enabled in the
tailnet (otherwise flats are served over plain HTTP inside the tailnet). Flats
nodes are owned by your user, so they count as user devices (free and
unlimited on the Personal plan) and inherit your key expiry; the console warns
14 days before a node key expires.

## Connect an agent

```sh
flats mcp-config        # prints setup for Claude Code, Codex and Cursor
```

For Claude Code on the Flats host:

```sh
claude mcp add --transport http flats http://127.0.0.1:7878/mcp
```

Agents on other machines use `https://flats.<tailnet>.ts.net/mcp`. The
`plugins/flats` directory packages the `flats-deploy` skill for Claude Code,
Codex and Cursor.

## CLI

```sh
flats deploy ./dist --flat my-blog       # save + deploy (records git SHA/dirty)
flats deploy ./dist --flat my-blog --save-only
flats preview my-blog                    # preview the newest version
flats rollback my-blog [--to 3]
flats visibility my-blog public-unlisted # waits for your approval in the console
flats logs my-blog --follow
flats secret set my-blog API_KEY         # value from stdin; operator only
flats list | info | versions | rename | delete | approvals | status
```

## Manifest (`flats.json`, optional)

```json
{ "name": "My blog", "kind": "static", "entry": "index.html", "spa": false,
  "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

## Server flats

```js
// server.js  (flats.json: {"kind": "server"})
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (at TEXT)");
    env.DB.exec("INSERT INTO hits VALUES (datetime('now'))");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
}
```

Each running version is a separate `flats worker` process. JavaScript has no
file system, network or Node.js APIs; it reaches data only through `env.DB`,
`env.FILES` and secrets (`env.NAME`). Limits: 64 MiB memory and 10 s per
request.

## Defaults

All are configurable in the console's system settings.

| Setting | Default |
|---|---|
| Versions kept | 10 newest plus the live one |
| Upload size | 20 MB |
| Disk per flat | 30 GB |
| Preview expiry | 24 h after the last visit |
| Rate limit | 50 requests/s per flat |
| Rename redirect | 7 days |

## Security model

- The management API, MCP endpoint and console listen only on loopback and on
  the tailnet console node; they are never exposed through Portal.
- There are no accounts. Who can open a private flat is decided by your
  tailnet membership and ACLs. Restrict which devices can reach the console
  node with Tailscale ACLs: any device that reaches it can operate Flats.
- Approvals stop agents from going public or deleting flats by themselves.
  They are not a defense against a malicious process on your own machine (see
  `docs/design.md`).
- Secrets are encrypted at rest (AES-256-GCM, key in the data directory) and
  never returned by any API.
- Portal does not pass visitor IP addresses, so page views are request
  counts and rate limits are per flat.

## Development

```sh
go test ./...                 # unit and integration tests (local network, test control server)
FLATS_PORTAL_E2E=1 go test ./internal/expose/portal/   # publishes a temporary test page
```

See `docs/design.md` for the design and the HTTP API.

## License

MIT
