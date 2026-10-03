<!-- PROTECTED BRANDING: Do not modify the image or copy in this block without an explicit user request. -->
<p align="center">
  <img src="docs/assets/flats-logo.png" alt="flats caretaker gopher logo" width="280">
</p>

<p align="center">
  <strong>Open-source alternative to sites/artifacts</strong>
</p>
<!-- END PROTECTED BRANDING -->

# Flats

Flats lets agents deploy sites and small server apps directly to your own machine: private by default over Tailscale, optionally public through Portal, with no hosted Flats control plane.

Your machine runs the code and keeps the versions, SQLite databases, persistent files and secrets. Flats is a single CGO-free Go binary for macOS and Linux, with a CLI, HTTP API, MCP endpoint and web console.

- Each flat and preview gets its own origin and private Tailscale node.
- Draft saves preserve Current; approved, successful publishes create immutable numbered versions. Health checks and previews use isolated data.
- Static sites and sandboxed JavaScript/WASI server apps share the same deploy flow.
- Publish, activation, rollback and both visibility directions require a validated operator approval.

```text
Agent / CLI / web console → Flats on your machine → version + local data
                                    │
                                    ├─ Tailscale → private browser origin
                                    └─ Portal relay → optional public browser origin
```

## Install and try a local flat

Requires Go 1.27.1 or newer to build. Install from the canonical module path:

```sh
go install github.com/gosuda/flats/cmd/flats@latest
# From a checkout:
CGO_ENABLED=0 go build -o flats ./cmd/flats
```

Put the installed binary's directory on `PATH`. For a credential-free local trial, start a host in one terminal:

```sh
flats serve --data ./flats-demo-data --network local --portal=false
```

In another terminal, deploy a minimal static site:

```sh
mkdir -p hello
printf '<h1>Hello from Flats</h1>\n' > hello/index.html
flats deploy ./hello --flat hello
```

The deploy command saves Draft and returns a pending publish request. Approve it through the authenticated operator console before opening `http://hello.localhost:7879`; the console is at `http://127.0.0.1:7878`. Local mode serves loopback development origins and does not join Tailscale. Use `--listen` and `--local-addr` to select other ports. Each running host needs its own data directory; concurrent hosts sharing one directory are rejected before store or runtime startup.

## Private and public deployment

For normal private hosting, run `flats serve` with the default Tailscale network. Enable MagicDNS and HTTPS certificates in your tailnet, then follow the node login links in the console, or supply a reusable, untagged Tailscale auth key with `--authkey-file`. Flats embeds tsnet; each flat, preview and console has its own node. Tailnet ACLs decide which devices can reach those origins. Code and data stay on your machine.

Visibility is Private or Public, independently of publication and connection state. Private routes use Local or permitted Tailscale with existing tailnet ACLs; Public routes use explicitly permitted Portal or Tailscale Funnel. Configuration alone never publishes a version or changes visibility. Public requires a published version and an approved transition with a ready public route. Legacy listed/unlisted inputs map to Public. Relay availability and Tailscale connectivity are external dependencies.

`flats install` runs the host at login using launchd on macOS or a systemd user service on Linux; `flats uninstall` removes the service and keeps data. The default data directory comes from your OS user configuration directory (`~/Library/Application Support/Flats` on macOS), overridable with `--data` or `FLATS_DATA`.

## Static and server flats

Static builds need an `index.html`. For a JavaScript server, include `flats.json` containing `{"kind":"server"}` and a `server.js` ES module:

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

Deploy that directory with the same `flats deploy` command. JavaScript runs in QuickJS on WebAssembly with SQLite (`env.DB`), persistent string files (`env.FILES`), Web Crypto and WebSocket callbacks. It has no Node.js APIs or outbound network access; bundle imports into the uploaded version.

A `.wasm` server is a WASI preview1 command instantiated afresh per request: request JSON on stdin, response JSON on stdout, only the flat's secrets as environment variables, clocks and randomness. It has **no SQLite/FILES host ABI, filesystem mounts, outbound network or WebSocket API**. See the [capability table and response format](docs/design.md#server-flats-handler-abi).

Data survives deploys and ordinary rollbacks. `flats rollback hello` restores code; `--restore-data` requests a separately frozen restore approval for the pre-deploy DB and FILES snapshot. Historical DB-only snapshots preserve current FILES. Preview data is isolated from live data.

## Agent integration

Run `flats mcp-config` for Claude Code, Codex and Cursor setup. For a host
on a custom management port, use `flats mcp-config --url http://127.0.0.1:17878`.
For example:

```sh
claude mcp add --transport http flats http://127.0.0.1:7878/mcp
codex mcp add flats --url http://127.0.0.1:7878/mcp
```

Cursor project `.cursor/mcp.json`:

```json
{"mcpServers":{"flats":{"url":"http://127.0.0.1:7878/mcp"}}}
```

Restart/reconnect your client, list tools, then read resource
`flats://docs/runtime-api/v1` or call **`get_runtime_reference` with `{}`**.
The [runtime API v1 reference](docs/runtime-api-v1.md) is embedded in the host
and available through MCP without an installed skill or source checkout. It
contains complete synchronous FILES/DB signatures, text/binary semantics,
limits, handler examples and approvals. For a minimal MCP static walkthrough,
call `save_version` with `{"slug":"hello","files":[{"path":"index.html",
"content":"<h1>Hello</h1>","encoding":"utf8"}],"deploy":true}`. For a server,
include the `flats.json` and `server.js` shown above in the same complete inline
file list. Fetch the returned URL and check `get_flat`/`get_logs`; a successful
save alone does not establish a live site, and requesting deploy still waits for operator approval.

Agents connect to the Streamable HTTP endpoint at `http://127.0.0.1:7878/mcp` on the host, or the console's Tailscale URL plus `/mcp` from another allowed device. The bundled [deployment skill](plugins/flats/skills/flats-deploy/SKILL.md) describes the deploy and approval flow.

## Security and operations

Server code runs in separate worker processes with WebAssembly memory/time limits and restricted host capabilities. The loopback management listener and console node are privileged control surfaces: restrict console access with tailnet ACLs. Core denies approval decisions unless its operator validator accepts the request context; Console labels and same-origin headers are insufficient. The transport must supply a validated operator session and the app must wire that same authority into core. Protect the operator credential/session and OS account: a process holding operator authority can decide approvals. See the [core lifecycle contract](docs/lifecycle-core-contract.md) for implementation and integration boundaries.

Secret values are operator-managed, encrypted at rest with the local `secret.key`, and delivered to a flat at its next deploy. APIs expose names only. A flat can read and return its own injected secrets, so deploy code you trust with those values. Anyone who can read the data directory can recover them. Back up the key alongside metadata and flat data; immutable code versions alone are not data backups.

Shutdown attempts every component and reports failures. Portal drains in-flight HTTP before unregistering exposures on normal shutdown. Network teardown is bounded; a timed-out SDK/backend may continue cleanup in the background until process exit. In that case the data-directory lock stays held until exit, so another host cannot race that cleanup.

Read the [design and trust model](docs/design.md) and [automated/manual verification gates](docs/gates.md) for manifests, limits, APIs and operational details.
