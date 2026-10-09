<!-- PROTECTED BRANDING: Do not modify the image or copy in this block without an explicit user request. -->
<p align="center">
  <img src="docs/assets/flats-logo.png" alt="flats caretaker gopher logo" width="280">
</p>

<p align="center">
  <strong>Open-source alternative to sites/artifacts</strong>
</p>
<!-- END PROTECTED BRANDING -->

# Flats

Flats lets agents deploy sites and small server apps directly to your own machine: Private by default through local loopback or permitted Tailscale, optionally Public through permitted Portal or Tailscale Funnel, with no hosted Flats control plane.

Your machine runs the code and keeps the versions, SQLite databases, persistent files and secrets. Flats is a single CGO-free Go binary for macOS and Linux, with a CLI, HTTP API, MCP endpoint and web console.

- Each flat and preview gets its own origin; explicitly permitted Tailscale routes get their own private node.
- Draft saves preserve Current; approved, successful publishes create immutable numbered versions. Health checks and previews use isolated data.
- Static sites and sandboxed JavaScript/WASI server apps share the same deploy flow.
- Publish, activation, rollback and both visibility directions wait for an approval in the web console.

```text
Agent / CLI / web console → Flats on your machine → version + local data
                                    │
                                    ├─ Tailscale → private browser origin
                                    └─ Portal relay → optional public browser origin
```

## Install and try a local flat

On macOS or Linux (amd64 or arm64), run the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
```

It downloads the latest release binary for your platform, verifies its SHA-256 checksum, installs it to `~/.local/bin` and runs Flats as a background service: a launchd agent on macOS or a systemd user service on Linux. The service starts at login and restarts if it stops; on Linux the installer also enables lingering when allowed, so Flats keeps running after you log out. It runs `flats serve --config` with a new `config.json` in the data directory described below, using the defaults (Local network, Portal off). [Configuration and storage](docs/configuration.md) lists every setting and where Flats keeps each kind of data.

Open `http://127.0.0.1:7878` for the console.

Run the installer again to upgrade: it replaces the binary and restarts the service with its existing settings. A service installed by an earlier release still passes `flats serve` flags; on its first start the new release moves those flags and the stored console settings into `config.json`, and the installer prints the `flats install` command that switches the service to run from that file. Options go after `sh -s --`: `--version v1.2.3` installs a specific release, `--dir DIR` changes the install directory, `--no-service` installs only the binary, and `flats serve` flags after a second `--` reinstall the service with those flags written into `config.json`:

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh -s -- -- --network tailscale
```

If the service is stopped (not running), the installer only replaces the binary and prints how to start it. Releases include a GitHub build provenance attestation; check an archive with `gh attestation verify flats_<os>_<arch>.tar.gz --repo gosuda/flats`. The installer does not use sudo or edit shell profiles. `flats status` shows whether the host answers, and `flats uninstall` removes the service while keeping your data. The installer does not back up data; see [Releases and installation](docs/release.md) for upgrade backups and how releases are cut. To build from source instead (Go 1.27.1 or newer):

```sh
go install github.com/gosuda/flats/cmd/flats@latest   # highest stable release; @main for unreleased changes
# From a checkout:
CGO_ENABLED=0 go build -o flats ./cmd/flats
```

### Container

A Linux image for amd64 and arm64 is published with every release. Its `/data` volume holds `config.json`, flat data and materialized docs runtime modules; an empty volume starts with the defaults. On Linux, run it with host networking so the console, CLI and agents reach it on loopback as with a native install:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  -v flats-data:/data ghcr.io/gosuda/flats:latest
```

On macOS or other Docker hosts, publish the same ports on `127.0.0.1` only, or use Tailscale. Never publish the console port to your network: anything that reaches it can approve deploys. See [Running Flats in a container](docs/container.md) for each network mode, configuration, hardening, upgrades and building the image.

### Foreground host

Without the service, start a foreground host in one terminal:

```sh
flats serve --config ./flats-demo-data/config.json
```

The first start finds no `config.json`, so it creates one with the defaults (Local network, Portal off) and an empty database in `./flats-demo-data`, and logs that it initialized a new host. It never does that over a directory that already holds Flats data. The console is at `http://127.0.0.1:7878`.

### First flat

Request publication of a minimal static site:

```sh
mkdir -p hello
printf '<h1>Hello from Flats</h1>\n' > hello/index.html
flats deploy ./hello --flat hello
```

The deploy command saves Draft and returns a pending publish request (exit code 3, including with `--json`). Follow its approval URL and approve the frozen Draft in the console before opening `http://hello.localhost:7879`; the console is at `http://127.0.0.1:7878`. Local mode serves loopback development origins and does not join Tailscale. To use other ports, set `host.management_addr` and `host.local_addr` with `flats config set` while the host is stopped, or pass `--listen` and `--local-addr` for one run. Each running host needs its own data directory; concurrent hosts sharing one directory are rejected before store or runtime startup.

## Private and public deployment

The default network is Local; every other provider is off. Turn a provider on for the host in **Settings → Network providers**, grouped into Private (Local, Tailscale) and Public (Tailscale Funnel, Portal). That saves `network.permitted` in `config.json` and starts the provider's backend without a restart; nothing is served until you also allow the provider for a flat under Networks on the flat's Settings tab. The one exception is the private backend described below: on a host whose console and private routes run on Tailscale, new flats are allowed on Tailscale when they are created. Turning a provider off is refused while a flat still allows it. Permitting Tailscale does not permit Funnel. To run the console and private routes on your tailnet as well, set the private backend while the host is stopped:

```sh
flats config set network.permitted tailscale
flats config set network.private_backend tailscale
```
 With the private backend on Tailscale, a new flat is allowed on Tailscale from the start, so its private and preview links use the tailnet. Choosing that backend is your standing permission for the Private Tailscale routes of every flat created afterwards, including flats an agent creates; each grant is logged as a `provider` event, and you can turn it off under the flat's Networks to keep a flat on the host only. It never allows Funnel or Portal. Flats created earlier keep their providers. Enable MagicDNS and HTTPS certificates in your tailnet, then follow the node login links in the console, or put a reusable, untagged Tailscale auth key in a protected file and set its path as `credentials.tailscale_authkey_file`. A host started with `--config` refuses `TS_AUTHKEY` and the other `TS_*` variables, which would bypass `config.json`. Flats embeds tsnet; permitted flat and preview routes, and the console in Tailscale mode, use separate nodes. Tailnet ACLs decide which devices can reach those origins. Code and data stay on your machine.

Removing a flat’s Tailscale permission stops its Private Tailscale current, preview and redirect routes before saving the revocation; Local stays available, and a separately permitted Public Tailscale Funnel route stays up. An unconfirmed stop keeps permission allowed so the operator can retry. A currently Public flat must complete an approved change to Private before its Public provider can be revoked.

A Local link (`*.localhost`) opens on the machine where you click it. When you open the console from another device, such as over the tailnet, it shows Local links as "server only" text instead of links, because they would open that device rather than the Flats host.

Visibility is Private or Public, independently of publication and connection state. Private routes use Local or permitted Tailscale with existing tailnet ACLs; Public routes use explicitly permitted Portal or Tailscale Funnel. Configuration alone never publishes a version or changes visibility. Public requires a published version and an approved transition with a registered, configured and permitted public route. A route may still be Connecting after approval; open its link only after it reports ready. Legacy listed/unlisted inputs map to Public. Relay availability and Tailscale connectivity are external dependencies.

`flats install` runs `flats serve --config` at login using launchd on macOS or a systemd user service on Linux, writing any flags you pass into `config.json` first; `flats uninstall` removes the service and keeps data and `config.json`. The default data directory comes from your OS user configuration directory (`~/Library/Application Support/Flats` on macOS); `--data` or `FLATS_DATA` selects another one, and its `config.json` sits inside it.

## Upgrading an existing host

Back up the data directory and stop the old host before starting the upgrade. The first start of this release moves the old serve flags, `network-provider.json` and the console settings into `config.json`, after backing up the database to `backups/`; see [Configuration and storage](docs/configuration.md#moving-an-existing-installation). Until the service is reinstalled with `flats install`, later starts check that the flags they are given still match `config.json`. Published versions keep their numbers; saved-only versions become Private Draft revisions. Historical network configuration does not create provider permissions. Existing Public policy can remain Public while its route is unavailable; do not report a link as reachable until its endpoint is ready and has been checked.

Restore intended host grants explicitly: `--network tailscale` grants Tailscale, `--portal=true` grants Portal, or `--permit tailscale,tailscale-funnel,portal` records only the listed grants. `--portal=false` disables Portal even if its stored grant remains. A Tailscale grant does not grant Funnel. Configure the relevant backend and allow each desired provider separately under Networks on each flat’s page. Neither a host grant nor a per-flat grant publishes or changes visibility; both Private→Public and Public→Private still require approval. Schema 5 records pre-lifecycle flats as eligible for a one-time Private Tailscale choice. An old default-Tailscale host with historical tsnet state must explicitly choose `--network tailscale` to preserve eligible flats’ Private Tailscale opt-in, or `--network local` to consume eligibility as Local-only. Ambiguous startup stops before networking. An already persisted explicit Tailscale backend/grant also preserves eligible flats once. Hosts with neither historical tsnet state nor a persisted Tailscale choice consume eligibility as Local-only on their first upgraded startup; a later host Tailscale grant never opts those flats in. Existing explicit denials win; new flats and later revocations are never opted in by restart. Neither choice grants Funnel or Portal.

Local loopback also binds in Tailscale mode: concurrent hosts need distinct `host.management_addr`, `host.local_addr` and data directories.

Flats also supports Markdown documents with `type: "docs"`. The embedded
collaborative editor runs on the server-flat runtime with private editing and
public reading. Agents read live edits with `get_document`, save a Draft with
`save_document`, then request operator approval to publish. Agent publications
and code rollbacks merge into live Markdown while keeping independent human
edits (conflicting lines prefer the target version). Read the
[content types contract](docs/content-types.md) through MCP resource
`flats://docs/content-types/v1` or `get_content_types`. Overlapping edits and
bounded merge fallback preserve recent pre-activation live text for private
recovery (newest 8 records within
2 MiB per document); editors see a notice and view/copy link. Agent reads list
conflict metadata rather than full preserved documents. Retrieve preserved text
with `get_document {slug, conflict: generation}`; optional `doc` selects the file.

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

Deploy that directory with the same `flats deploy` command. JavaScript runs in QuickJS on WebAssembly with SQLite (`env.DB`), persistent string files (`env.FILES`), Web Crypto and WebSocket callbacks. It has no Node.js APIs; bundle imports into the uploaded version. Server-side `fetch` can call exact operator-granted HTTP(S) origins with bounded host enforcement. Browser-side `fetch` uses normal CORS/CSP protections. See [external API setup and examples](docs/external-api.md).

A `.wasm` server is a WASI preview1 command instantiated afresh per request: request JSON on stdin, response JSON on stdout, only the flat's configured environment variables and secrets, with no inherited host environment, plus clocks and randomness. It has **no SQLite/FILES host ABI, filesystem mounts, outbound network or WebSocket API**. See the [capability table and response format](docs/design.md#server-flats-handler-abi).

Data survives deploys and ordinary rollbacks. `flats rollback hello` restores code; `--restore-data` requests a separately frozen restore approval for the pre-deploy DB and FILES snapshot. Historical DB-only snapshots preserve current FILES. Preview data is isolated from live data.

## Agent integration

The Flats plugin bundles the deployment skill and an MCP server for Claude
Code, Codex and Cursor. Its MCP server runs `flats mcp`, which relays the
host's MCP endpoint over stdio, so the plugin never names a host. Choose the
host once per machine with `flats connect`; the CLI and every agent's plugin
then use it:

```sh
flats connect https://flats.example.ts.net   # or skip it on the host itself: loopback is the default
```

```sh
claude plugin marketplace add gosuda/flats && claude plugin install flats@flats
codex plugin marketplace add gosuda/flats && codex plugin add flats@flats
```

In Claude Code you can also run `/plugin install flats --marketplace gosuda/flats`.
For Cursor, add the marketplace with
`cursor-agent plugin marketplace add https://github.com/gosuda/flats` and
install Flats from Cursor's plugin list, or load a checkout with
`cursor-agent --plugin-dir plugins/flats`. The agent must find `flats` on its `PATH`;
on a client machine install only the CLI with the installer's `--no-service`
option. `flats connect` saves the address in
`<user config dir>/flats-client/connection.json` after checking that it answers
as a Flats host; `flats connect` alone shows the current host and
`flats connect --clear` returns to loopback. `--url` and `FLATS_URL` still
override it for one command, but agents generally do not pass your shell
environment to MCP servers, so use `flats connect` for plugins. Restart or
reconnect an agent after changing the host.

Without the plugin, register the HTTP endpoint directly. Run `flats mcp-config`
for Claude Code, Codex and Cursor setup. For a host on a custom management
port, use `flats mcp-config --url http://127.0.0.1:17878`. For example:

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

Agents and tools that read [llms.txt](https://llmstxt.org) can start from
`http://127.0.0.1:7878/llms.txt`. The host serves it on the management server
with the MCP endpoint and client setup, and links `/docs/agent-guide.md` (the
instructions and tool list the MCP server reports, plus core CLI commands) and
`/docs/runtime-api-v1.md` and `/docs/content-types.md`; `/llms-full.txt` concatenates the discovery page and all three references.

Agents connect to the Streamable HTTP endpoint at `http://127.0.0.1:7878/mcp` on the host, or the console's Tailscale URL plus `/mcp` from another allowed device. The bundled [deployment skill](plugins/flats/skills/flats-deploy/SKILL.md) describes the deploy and approval flow.

## App environment settings

Ordinary environment variables belong to one flat and are readable by management
clients. Use secrets for credentials. Set, inspect and remove ordinary values with:

```sh
flats env set hello GREETING 'Hello'
flats env set hello OPTIONAL ''
flats env ls hello
flats env rm hello OPTIONAL
```

The console's flat Settings page and MCP tools `list_env`, `set_env`, `delete_env`
manage the same values. Names match `[A-Z_][A-Z0-9_]*` and are at most 64
characters; `DB`, `FILES`, `__PROTO__`, `PROTOTYPE` and `CONSTRUCTOR` are reserved. Values may be empty, are at most
64 KiB, and must be valid UTF-8 without NUL. A name cannot be both an ordinary variable and
a secret; remove the existing setting before switching kinds. New secret writes use
the same validation. Existing historical secrets are retained: JavaScript keeps
`DB`/`FILES` as host bindings, while WASI receives historical values for those
names. Other historical names and values retain their runtime behavior.

Server JavaScript reads strings as `env.GREETING`; WASI receives environment
variables. They are never substituted into frontend bundles, static files or
builds. Saving does not change running handlers or previews. Approved deployment,
redeployment, rollback or standalone data snapshot restoration captures current variables and secrets when activation begins; its health check and live worker share that
snapshot. Settings are not pinned to the approval request or code version; writes
after capture apply at the next activation. A newly created preview and a Flats
host restart also load current settings. Automatic worker restarts reuse the
captured snapshot. Redeploy through the existing approval flow to apply changes.

## Security and operations

Server code runs in separate worker processes with WebAssembly memory/time limits and restricted host capabilities. The loopback management listener and console node are privileged control surfaces: restrict console access with tailnet ACLs. Approval decisions and provider changes are accepted only on console routes, which require the console header and a same-origin browser request. That is CSRF protection, not authentication: a local process that sends those headers to the loopback listener can decide approvals, so run only trusted agents on the host. A separate operator credential is planned; `credentials.operator_file` and the `--operator-credential-*` flags are still accepted but ignored. See the [core lifecycle contract](docs/lifecycle-core-contract.md) for implementation and integration boundaries.

Secret values are operator-managed, encrypted at rest with the local `secret.key`, and delivered to a flat at its next approved deployment, redeployment, rollback or standalone data snapshot restoration, or Flats host restart. Automatic worker restarts reuse their captured settings. Secret APIs expose names only; ordinary environment-variable APIs expose their values. A flat can read and return its own injected secrets, so deploy code you trust with those values. Anyone who can read the data directory can recover them. Back up the key alongside metadata and flat data; immutable code versions alone are not data backups.

Shutdown attempts every component and reports failures. Portal drains in-flight HTTP before unregistering exposures on normal shutdown. Network teardown is bounded; a timed-out SDK/backend may continue cleanup in the background until process exit. In that case the data-directory lock stays held until exit, so another host cannot race that cleanup.

Read the [design and trust model](docs/design.md) and [automated/manual verification gates](docs/gates.md) for manifests, limits, APIs and operational details.
