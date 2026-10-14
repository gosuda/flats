<!-- PROTECTED BRANDING: Do not modify the image or copy in this block without an explicit user request. -->
<p align="center">
  <img src="docs/assets/flats-logo.png" alt="flats caretaker gopher logo" width="280">
</p>

<p align="center">
  <strong>Open-source alternative to sites/artifacts</strong>
</p>
<!-- END PROTECTED BRANDING -->

<h1 align="center">Flats</h1>

<p align="center">
  Sites, apps, documents and designs — built by agents, hosted on your machine.
</p>

<p align="center">
  <a href="https://github.com/gosuda/flats/releases"><img src="https://img.shields.io/github/v/release/gosuda/flats?style=flat-square&amp;color=427a59" alt="Latest release"></a>
  <a href="https://github.com/gosuda/flats/actions/workflows/ci.yml"><img src="https://github.com/gosuda/flats/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-427a59?style=flat-square" alt="License: MIT"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#what-you-can-build">What you can build</a> ·
  <a href="#agent-integration">Connect an agent</a> ·
  <a href="docs/README.md">Documentation</a>
</p>

---

Flats turns your own machine into a home for the things you build with AI.
Deploy a website, run a small server app, share an HTML page, collaborate
on a Markdown document or lay out a design canvas. Start Private on local
loopback, then choose who can reach it through explicitly permitted network
providers.

One CGO-free Go binary. macOS and Linux. A web console, CLI, HTTP API and MCP
endpoint. **No hosted Flats control plane.**

| Your machine, your data | Built for agent workflows | Private by default |
| --- | --- | --- |
| Keep code, SQLite databases, persistent files and encrypted secrets locally. | Connect Claude Code, Codex, Cursor or another MCP client. Inspect and approve deployments in the console. | Use Local or a permitted private network. Public sharing is a separate, approved choice. |

## What you can build

| Type | Made for | How it goes live |
| --- | --- | --- |
| **Website** | Static sites and sandboxed JavaScript or WASI server apps | Save a Private Draft and preview it. Approve saving it as a numbered version, then approve publishing that version. Published versions support rollback. |
| **Page** | A single HTML document with bundled assets: charts, reports and visualizations | The same Draft, version and publish flow as websites. No server code or data storage. |
| **Document** | Live, collaborative Markdown | Create or save to run immediately, initially Private. No Draft, versions or publish step. |
| **Design** | A canvas of HTML artboards: screens, landing pages, posters and click-through prototypes | Changes are live at once, initially Private. Every change is kept as a revision you can pin or roll back. |

Websites and pages follow the **version model**: you work on a Private Draft
while the live version keeps serving, and previews use isolated data. Saving a
version (v1, v2, ...) and publishing it are separate approved steps, so a
version can exist without ever going live. Publication and rollback require
console approval.

Documents and designs are edited live by people and agents together: private
visitors can edit while public visitors read, and changes to an already Public
flat are visible immediately. Documents merge agent saves into people's edits;
designs keep every change as a whole-canvas revision. **Changing visibility in
either direction requires approval for every type.**

Explore [content types](docs/content-types.md) for the full lifecycle and
agent tools, including `save_page`, `update_document` and `save_design`.

## Quick start

### 1. Install Flats

On macOS or Linux, with an amd64 or arm64 machine:

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
```

The installer verifies the release checksum, installs to `~/.local/bin` and
starts a background service: launchd on macOS, a systemd user service on
Linux. It uses no sudo and does not edit shell profiles. The default network
is Local; other providers are off.

### 2. Deploy your first site

```sh
mkdir -p hello
printf '<h1>Hello from Flats</h1>\n' > hello/index.html
flats deploy ./hello --flat hello
```

### 3. Approve, publish and open

The command asks to save the Draft as version 1. Follow the approval URL it
returns, or open the [local console](http://127.0.0.1:7878), and approve it.
That creates v1 but puts nothing live yet. Publish it with **Publish v1** in
the console, or request it from the CLI and approve that request too:

```sh
flats deploy --flat hello --version 1
```

Then visit [hello.localhost:7879](http://hello.localhost:7879).

> `flats deploy` returns exit code **3** while a request awaits approval,
> including with `--json`. Saving a Draft or a version does not make a site
> live; only publishing a version does.

Run `flats status` to check the host. Run the installer again to upgrade;
`flats uninstall` removes the service and keeps your data. See
[installation, upgrades and backups](docs/release.md) before upgrading an
existing host.

<details>
<summary><strong>Other ways to run Flats: container, foreground or source</strong></summary>

#### Container

A Linux image for amd64 and arm64 is published with every release. On Linux,
use host networking so the console, CLI and agents reach it on loopback:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  -v flats-data:/data ghcr.io/gosuda/flats:latest
```

Never publish the console port to your network: anything that reaches it can
approve deploys. See [Running Flats in a container](docs/container.md) for
other Docker hosts and network modes.

#### Foreground

```sh
flats serve --config ./flats-demo-data/config.json
```

On first start, this creates a host with the defaults in `./flats-demo-data`.
It never does that over a directory that already holds Flats data. Each
running host needs its own data directory and ports; see
[Configuration and storage](docs/configuration.md).

#### From source

Requires Go 1.27.1 or newer:

```sh
go install github.com/gosuda/flats/cmd/flats@latest
```

Use `@main` for unreleased changes. This build does not include NetBird,
which needs `replace` directives that `go install` refuses. From a checkout,
build the release binary, NetBird included, from the `netbird` module:

```sh
cd netbird && CGO_ENABLED=0 go build -o flats ./cmd/flats
```

or `CGO_ENABLED=0 go build -o flats ./cmd/flats` at the root for the build
without NetBird.

</details>

## Agent integration

Connect **Claude Code, Codex or Cursor** with the Flats plugin. The plugin
runs `flats mcp`, which relays the selected host's MCP endpoint.
Install the CLI on the agent's machine and make sure `flats` is on its `PATH`.
For a client-only machine, use the installer's `--no-service` option.
On the host itself, loopback is the default; for a remote host, select it once:

```sh
flats connect https://flats.example.ts.net
```

<details open>
<summary><strong>Claude Code</strong></summary>

```sh
claude plugin marketplace add gosuda/flats
claude plugin install flats@flats
```

</details>

<details>
<summary><strong>Codex</strong></summary>

```sh
codex plugin marketplace add gosuda/flats
codex plugin add flats@flats
```

</details>

<details>
<summary><strong>Cursor and other MCP clients</strong></summary>

For Cursor, add the marketplace, then install Flats from Cursor's plugin list:

```sh
cursor-agent plugin marketplace add https://github.com/gosuda/flats
```

Without a plugin, register the Streamable HTTP endpoint
`http://127.0.0.1:7878/mcp` directly. `flats mcp-config` prints setup instructions
for each client. Remote clients use the host's reachable console URL plus
`/mcp`.

</details>

Restart or reconnect the client and list its tools. The agent starts with
read-only `check_client`, then reads the server's current `guide`.
[Connecting agents](docs/agents.md) covers setup, client updates and llms.txt.
For the whole server contract in one document, read resource
`flats://docs/runtime-api/v1` or call **`get_runtime_reference` with `{}`**.
The [runtime API v1 reference](docs/runtime-api-v1.md) is embedded in the host
and available through MCP without an installed skill or source checkout.

## Static and server flats

Static builds need an `index.html`. For a JavaScript app, add a `flats.json`:

```json
{"kind":"server"}
```

Then create `server.js`:

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

Deploy the directory with the same `flats deploy` command. JavaScript runs in
QuickJS on WebAssembly, with SQLite (`env.DB`), persistent string files
(`env.FILES`), Web Crypto and WebSocket callbacks. There are no Node.js APIs;
bundle imports into the uploaded version. Server-side `fetch` can call exact
operator-granted HTTP(S) origins; see [Calling external APIs](docs/external-api.md).

<details>
<summary><strong>WASI, persistent data and environment variables</strong></summary>

A `.wasm` server is a WASI preview1 command instantiated afresh per request:
request JSON on stdin, response JSON on stdout, only the flat's configured
environment variables and secrets, with no inherited host environment, plus
clocks and randomness. It has **no SQLite/FILES host ABI, filesystem mounts,
outbound network or WebSocket API**. See the
[capability table and response format](docs/design.md#server-flats-handler-abi).

Data survives deploys and ordinary rollbacks. `flats rollback hello` restores
code; `--restore-data` requests a separately approved restore of the
pre-deploy DB and FILES snapshot. Preview data is isolated from live data.

Ordinary environment variables (`flats env set hello GREETING 'Hello'`) and
encrypted secrets reach server code as `env.NAME`, or as environment variables
for WASI. Saving a value leaves running workers on their snapshot. The live
worker picks up current values at the next approved deployment, redeployment,
rollback or data restore, or when the Flats host restarts; a newly created
preview also loads them. See
[App environment variables](docs/design.md#app-environment-variables) and
[Secrets](docs/design.md#secrets).

</details>

## Private and public access

Start locally, then enable the providers you need in
**Settings → Network providers**. Allow a provider for an individual flat
under **Networks** on its Settings tab.

| Access | Providers |
| --- | --- |
| **Private** | Local (always on), Tailscale (tailnet ACLs), NetBird¹ (access policies) |
| **Public** | Portal, Tailscale Funnel, zrok, Cloudflare quick tunnel, NetBird Expose¹ |

¹ NetBird and NetBird Expose are in the release binary (`install.sh` and the
container image) but not in `go install`; see
[NetBird setup](docs/networking.md#serving-through-netbird).

Host permission alone does not grant a provider to a flat. Configured provider
defaults can grant providers to new flats; changing those defaults does not
retroactively change existing flats. Tailscale permission does not imply
Funnel permission.

Making a flat Public requires an approved transition and a configured,
permitted public route. Websites and pages also need a published version;
documents and designs run without publication. Configuration alone never
publishes or changes visibility. Wait for a route to report ready and verify
its endpoint before sharing it: relay availability and network connectivity
are external dependencies.

See [Private and public access](docs/networking.md) for provider setup,
tailnet consoles and upgrades from older hosts.

## Security

- **Sandboxed execution.** Server code runs in separate worker processes with
  WebAssembly memory and time limits, bounded request bodies and WebSocket
  connections, SQLite and FILES quotas, and restricted host capabilities.
  Each flat and preview gets its own origin.
- **Privileged console.** The management listener and console can approve
  deployments. Approval decisions require the console header and a same-origin
  browser request. That is CSRF protection, not authentication: a local
  process that sends those headers can decide
  approvals. Run only trusted agents on the host and restrict console access
  with tailnet ACLs.
- **Local secrets.** Secrets are encrypted with the local `secret.key`; secret
  APIs expose names only. A flat can read and return its own secrets, so
  deploy code you trust with them. Anyone who can read the data directory
  can recover those secrets.
- **Back up your data.** Immutable code versions are not data backups. Back up
  the data directory, including `secret.key`; see
  [what to back up](docs/configuration.md#what-to-back-up).

Read the [design and trust model](docs/design.md) for the details.

## Documentation

| Get started | Build and operate |
| --- | --- |
| [Install and upgrade](docs/release.md) | [Runtime API reference](docs/runtime-api-v1.md) |
| [Connect an agent](docs/agents.md) | [Content types and lifecycles](docs/content-types.md) |
| [Configure a host](docs/configuration.md) | [Call external APIs](docs/external-api.md) |
| [Run in a container](docs/container.md) | [Design and trust model](docs/design.md) |
| [Set up networking](docs/networking.md) | [Verification gates](docs/gates.md) |

Browse the **[complete documentation index](docs/README.md)** for contracts,
internal design and verification guides, and the
**[product specs](docs/spec/README.md)** for what Flats is meant to do,
including targets that are not fully implemented yet.

---

<p align="center">
  <a href="LICENSE">MIT licensed</a> ·
  <a href="https://github.com/gosuda/flats/releases">Releases</a> ·
  <a href="https://github.com/gosuda/flats/issues">Issues</a>
</p>
