<!-- PROTECTED BRANDING: Do not modify the image or copy in this block without an explicit user request. -->
<p align="center">
  <img src="docs/assets/flats-logo.png" alt="flats caretaker gopher logo" width="280">
</p>

<p align="center">
  <strong>Open-source alternative to sites/artifacts</strong>
</p>
<!-- END PROTECTED BRANDING -->

# Flats

Flats lets agents deploy sites and small server apps directly to your own
machine. Flats are Private by default, through local loopback or permitted
Tailscale, and optionally Public through permitted Portal or Tailscale Funnel.
There is no hosted Flats control plane.

Your machine runs the code and keeps the versions, SQLite databases,
persistent files and secrets. Flats is a single CGO-free Go binary for macOS
and Linux, with a CLI, HTTP API, MCP endpoint and web console.

- **Isolated origins.** Each flat and preview gets its own origin; explicitly
  permitted Tailscale routes get their own private node.
- **Drafts and versions.** Draft saves preserve the live version; approved,
  successful publishes create immutable numbered versions. Health checks and
  previews use isolated data.
- **Static and server.** Static sites and sandboxed JavaScript/WASI server
  apps share the same deploy flow.
- **Human approval.** Publish, activation, rollback and both visibility
  directions wait for an approval in the web console.

```text
Agent / CLI / web console → Flats on your machine → version + local data
                                    │
                                    ├─ Tailscale → private browser origin
                                    └─ Portal relay → optional public browser origin
```

## Quick start

On macOS or Linux (amd64 or arm64), run the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
```

It verifies the release checksum, installs `flats` to `~/.local/bin` and runs
it as a background service (launchd on macOS, a systemd user service on
Linux) with the defaults: Local network, Portal off. It does not use sudo or
edit shell profiles. Open the console at `http://127.0.0.1:7878`.

Deploy a minimal static site:

```sh
mkdir -p hello
printf '<h1>Hello from Flats</h1>\n' > hello/index.html
flats deploy ./hello --flat hello
```

`flats deploy` saves a Draft and returns a pending publish request (exit
code 3, including with `--json`). Follow its approval URL, approve the frozen
Draft in the console, then open `http://hello.localhost:7879`. Local mode
serves loopback development origins and does not join Tailscale.

Run the installer again to upgrade. `flats status` shows whether the host
answers, and `flats uninstall` removes the service while keeping your data.
[Releases and installation](docs/release.md) covers installer options,
upgrades and backups.

### Other ways to run it

- **Container.** A Linux image for amd64 and arm64 is published with every
  release. On Linux, use host networking so the console, CLI and agents reach
  it on loopback:

  ```sh
  docker run -d --name flats --restart unless-stopped --network host \
    -v flats-data:/data ghcr.io/gosuda/flats:latest
  ```

  Never publish the console port to your network: anything that reaches it
  can approve deploys. See [Running Flats in a container](docs/container.md).

- **Foreground.** `flats serve --config ./flats-demo-data/config.json` creates
  a new host with the defaults in `./flats-demo-data` on its first start. It
  never does that over a directory that already holds Flats data. Each running
  host needs its own data directory and ports; see
  [Configuration and storage](docs/configuration.md).

- **From source** (Go 1.27.1 or newer):

  ```sh
  go install github.com/gosuda/flats/cmd/flats@latest   # highest stable release; @main for unreleased changes
  CGO_ENABLED=0 go build -o flats ./cmd/flats           # from a checkout
  ```

## Static and server flats

Static builds need an `index.html`. For a JavaScript server, include
`flats.json` containing `{"kind":"server"}` and a `server.js` ES module:

```js
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    env.DB.exec("INSERT INTO hits VALUES (1)");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
};
```

Deploy that directory with the same `flats deploy` command.

- **JavaScript** runs in QuickJS on WebAssembly with SQLite (`env.DB`),
  persistent string files (`env.FILES`), Web Crypto and WebSocket callbacks.
  It has no Node.js APIs; bundle imports into the uploaded version.
  Server-side `fetch` can call exact operator-granted HTTP(S) origins; see
  [Calling external APIs](docs/external-api.md).
- **WASI.** A `.wasm` server is a WASI preview1 command instantiated afresh
  per request: request JSON on stdin, response JSON on stdout, only the
  flat's configured environment variables and secrets, with no inherited host
  environment, plus clocks and randomness. It has **no SQLite/FILES host ABI,
  filesystem mounts, outbound network or WebSocket API**. See the
  [capability table and response format](docs/design.md#server-flats-handler-abi).
- **Markdown documents.** A flat with `type: "docs"` runs the built-in
  collaborative editor: private visitors edit live Markdown, public visitors
  read it, and agent publications merge into human edits. See
  [Content types](docs/content-types.md).

Data survives deploys and ordinary rollbacks. `flats rollback hello` restores
code; `--restore-data` requests a separately approved restore of the
pre-deploy DB and FILES snapshot. Preview data is isolated from live data.

Ordinary environment variables (`flats env set hello GREETING 'Hello'`) and
encrypted secrets reach server code as `env.NAME`, or as environment variables
for WASI. Changes apply at the next approved activation; see
[App environment variables](docs/design.md#app-environment-variables) and
[Secrets](docs/design.md#secrets).

## Private and public access

The default network is Local; every other provider is off. Turn a provider on
for the host in **Settings → Network providers**, then allow it for a flat
under Networks on the flat's Settings tab:

| Visibility | Providers |
| --- | --- |
| Private | Local (always on), Tailscale (tailnet ACLs decide access) |
| Public | Tailscale Funnel, Portal |

Permitting a network never publishes a version or changes visibility, and
Tailscale does not imply Funnel. Making a flat Public needs a published
version, a permitted public route and an approval. See
[Private and public access](docs/networking.md) for Tailscale setup, running
the console on your tailnet and upgrading an older host.

## Agent integration

Install the Flats plugin for Claude Code, Codex or Cursor. Its MCP server runs
`flats mcp`, which relays the host's MCP endpoint, so choose the host once per
machine:

```sh
flats connect https://flats.example.ts.net   # or skip it on the host itself: loopback is the default
claude plugin marketplace add gosuda/flats && claude plugin install flats@flats
codex plugin marketplace add gosuda/flats && codex plugin add flats@flats
```

Without the plugin, register the Streamable HTTP endpoint
`http://127.0.0.1:7878/mcp` directly; `flats mcp-config` prints the setup for
each client. [Connecting agents](docs/agents.md) covers Cursor, remote hosts
and llms.txt.

The MCP instructions stay short and send the agent to the read-only `guide`
tool, whose topics live in [`docs/agent/`](docs/agent).

For the whole server contract in one document, read resource
`flats://docs/runtime-api/v1` or call **`get_runtime_reference` with `{}`**.
The [runtime API v1 reference](docs/runtime-api-v1.md) is embedded in the host
and available through MCP without an installed skill or source checkout.

## Security

- Server code runs in separate worker processes with WebAssembly memory and
  time limits and restricted host capabilities.
- The loopback management listener and the console are privileged control
  surfaces. Approval decisions require the console header and a same-origin
  browser request. That is CSRF protection, not authentication: a local
  process that sends those headers can decide approvals, so run only trusted
  agents on the host, and restrict console access with tailnet ACLs.
- Secrets are encrypted at rest with the local `secret.key`, and secret APIs
  expose names only. A flat can read and return its own secrets, so deploy
  code you trust with them. Anyone who can read the data directory can recover
  them.
- Immutable code versions are not data backups. Back up the data directory,
  including `secret.key`, as described in
  [Configuration and storage](docs/configuration.md#what-to-back-up).

See the [design and trust model](docs/design.md) for the details.

## Documentation

The [documentation index](docs/README.md) lists every page: operating a host,
building on Flats, design contracts and verification gates.
