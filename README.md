# Flats

Self-hosted, agent-agnostic web hosting. Each hosted site is a **flat**:
coding agents (Claude Code, Codex, Cursor, …) create and deploy flats over
MCP, a CLI or an HTTP API, and you manage them in a web console that only your
tailnet can reach. Private flats are served over Tailscale; public flats go
out through [Portal](https://github.com/gosuda/portal-tunnel).

Everything ships as one CGO-free Go binary. macOS (Apple Silicon) first;
Linux builds (amd64, arm64) compile and install as a systemd user service,
but have not yet been run end to end on a Linux host.

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
./flats install                         # or: run in the background (launchd on macOS,
                                        # a systemd user service on Linux)
```

`flats serve` options:

| Flag | Default | Meaning |
|---|---|---|
| `--data` | `~/Library/Application Support/Flats` on macOS, `~/.config/Flats` on Linux (`$FLATS_DATA`) | data directory (a relative path is resolved against the current directory) |
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

Every flat and every preview is its own tailnet host with its own Let's
Encrypt certificate, which Tailscale obtains with a DNS challenge. A new
flat or preview therefore answers only after about 1-2 minutes (sometimes
longer); until then its state is `starting` with "waiting for its HTTPS
certificate", in the console, `flats info` and the MCP tools. Two
consequences of public certificates:

- Host names are published in Certificate Transparency logs, so flat slugs
  and preview names under your `*.ts.net` name are publicly visible (the
  pages are not; they stay reachable only inside the tailnet). Do not put
  secrets in slugs.
- Let's Encrypt limits certificates per registered domain (currently 50 per
  week for your tailnet's `ts.net` name). Each new flat and each preview
  uses one, so opening many previews in a week can delay new ones; existing
  flats keep their certificates and renew normally.

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
flats rollback my-blog [--to 3]          # code only; --restore-data also replaces the database
flats visibility my-blog public-unlisted # waits for your approval in the console
flats logs my-blog --follow
flats secret set my-blog API_KEY         # value from stdin; operator only; applies on the next deploy
flats deploy --flat my-blog --version 3  # redeploy the live version to apply secrets now
flats list | info | versions | rename | delete | approvals | status
```

## Manifest (`flats.json`, optional)

```json
{ "name": "My blog", "kind": "static", "entry": "index.html", "spa": false,
  "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

`health` is a URL path such as `/healthz` (percent-encode spaces). Every
problem in an upload is reported at once, each with a fix.

Static files with a content hash in their name (`app.3f9a1c2b.js`,
`index-BqZ2x8Ka.css`) are cached by browsers for a year. Everything else is
revalidated on every visit with an ETag, so a deploy or rollback reaches
returning visitors immediately.

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
| Portal relays | Portal default relays with discovery, up to 3 active (`portal_relays`, `portal_discovery`, `portal_max_relays`; applied when `flats serve` restarts; `--relays` overrides the relay list) |

## Security model

- The management API, MCP endpoint and console listen only on loopback and on
  the tailnet console node; they are never exposed through Portal. They answer
  only to their own host names (`127.0.0.1`, `localhost`, `[::1]` with the
  listen port, and the console node's tailnet names), so a web page that
  points its DNS name at them (DNS rebinding) is refused, as is any browser
  request whose `Origin` is another site.
- The agent API (`/api`) refuses cross-site browser requests. Requests that
  change something must send `Content-Type: application/json`, an archive
  type for uploads, or an `X-Flats-Client` header, which a web page cannot
  send to another origin without a CORS preflight that Flats never grants.
- There are no accounts. Who can open a private flat is decided by your
  tailnet membership and ACLs. Restrict which devices can reach the console
  node with Tailscale ACLs, ideally to your own browser devices: any device
  that reaches it can operate Flats. Approvals made on the console node record
  your tailnet login.
- What approvals enforce: agents using MCP, the CLI or the agent API cannot
  approve their own requests, go public or delete a flat; those calls only
  create a pending approval. What they do not enforce: a program that can
  send arbitrary HTTP from one of your devices (for example an agent with a
  shell on the Flats host, imitating the console's browser headers) or that
  can read the Flats data directory can act as you. Run agents you do not
  trust under another user or on another machine (see "Approvals" in
  `docs/design.md`).
- Secrets are encrypted at rest (AES-256-GCM, key in the data directory) and
  never returned by any API. The console and `flats secret set` on the Flats
  host can set them; an agent with a shell on that host can run the same
  command.
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
