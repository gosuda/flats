# Running Flats in a container

Releases and commits pushed to `main` publish Linux images for `amd64` and `arm64`:

```text
ghcr.io/gosuda/flats:1.2.3     # one release (tags drop the leading v)
ghcr.io/gosuda/flats:1.2       # newest patch of a minor release
ghcr.io/gosuda/flats:latest    # newest stable release; prereleases never get it
ghcr.io/gosuda/flats:sha-1a2b3c4  # a development commit build (7-character SHA)
ghcr.io/gosuda/flats:sha-<40-hex commit>  # the full commit identity
```

A versioned release image runs the same `flats` binary as that release's
`flats_linux_<arch>.tar.gz`. A commit image builds that commit separately,
stamps `flats version` as `v0.0.0-sha-<7-character SHA>`, and does not publish
a GitHub release or replace `latest`. Prefer the full SHA tag when pinning
a commit; short SHAs can collide. See [Commit images](release.md#commit-images)
for push coverage and provenance details. Historical release `sha-` tags
remain release images until an explicit commit build replaces them.

Both kinds run on distroless (`gcr.io/distroless/static-debian12:nonroot`:
CA certificates and tzdata, no shell or package manager), as the unprivileged
user `65532`. Everything Flats keeps lives in one volume, `/data`:
`config.json`, `flats.db`, `secret.key`, the flats and their data, and
Tailscale and Portal state (see [Configuration and storage](configuration.md)).
The image sets `FLATS_DATA=/data` and `FLATS_CONFIG=/data/config.json` and runs
`flats serve`. On an empty volume, `flats serve` bootstraps a new host with the
defaults (Local network, Portal off) and logs `initialized a new host in
/data`; on later starts it reuses the volume, migrating older formats and
refusing newer ones ([Bootstrapping a new host](configuration.md#bootstrapping-a-new-host)).

Always mount a named volume (or a host directory) at `/data`. Without `-v`,
Docker creates an anonymous volume, and recreating the container would start
another empty host.

Verify a release image before you run it:

```sh
gh attestation verify oci://ghcr.io/gosuda/flats:1.2.3 --repo gosuda/flats
```

## Choose a network mode

The management listener (`host.management_addr`, `127.0.0.1:7878`) is a
privileged control surface: approval decisions need only the console's
same-origin headers, not a credential, so anything that can reach it can
approve. A container must keep it reachable only from the host, and the
browser must reach it under the same port it listens on (the listener accepts
only loopback `Host` names with its own port).

**Linux host networking.** The container shares the host's loopback, so the
console, CLI, MCP endpoint and `http://<flat>.localhost:7879` behave exactly
as with a native install:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  -v flats-data:/data ghcr.io/gosuda/flats:latest
```

**Published loopback ports, on any Docker host** (including Docker Desktop or
Rancher Desktop on macOS). Listen on all container interfaces, publish the
same port numbers on the host's `127.0.0.1` only, and use a dedicated network
so other containers cannot reach the listener:

```sh
docker network create flats
docker run -d --name flats --restart unless-stopped --network flats \
  -p 127.0.0.1:7878:7878 -p 127.0.0.1:7879:7879 \
  -v flats-data:/data ghcr.io/gosuda/flats:latest \
  serve --listen 0.0.0.0:7878 --local-addr 0.0.0.0:7879
```

The host logs a warning that the management API is not on loopback; that is
expected here. Never publish without `127.0.0.1:`, which would let your
network approve deploys. Connections through published ports are not loopback
to Flats, so MCP's `save_version_from_dir` is unavailable: agents use
`save_version` or the `flats` CLI, which uploads the files.

**Tailscale.** Permit Tailscale and make it the private backend while the
host is stopped, then start it; the console and permitted flats get their own
tailnet nodes with HTTPS, and no ports need publishing:

```sh
docker run --rm -v flats-data:/data ghcr.io/gosuda/flats:latest config init
docker run --rm -v flats-data:/data ghcr.io/gosuda/flats:latest config set network.permitted tailscale
docker run --rm -v flats-data:/data ghcr.io/gosuda/flats:latest config set network.private_backend tailscale
docker run -d --name flats --restart unless-stopped \
  -v flats-data:/data ghcr.io/gosuda/flats:latest
```

Each node prints a login link to `docker logs flats` and shows it in `docker
exec flats flats status` until you log it in. To log nodes in automatically,
mount a reusable, untagged auth key that uid `65532` can read outside the data
volume, for example `-v "$PWD/tailscale-authkey:/run/secrets/tailscale-authkey:ro"`,
and set `credentials.tailscale_authkey_file` to
`/run/secrets/tailscale-authkey`. A host started from `config.json` refuses
`TS_AUTHKEY` and other `TS_*` variables.

## Configuration

`flats config set` and `unset` work offline and refuse while the host runs.
Stop the container, change the settings with one-off containers on the same
volume, and start it again:

```sh
docker stop flats
docker run --rm -v flats-data:/data ghcr.io/gosuda/flats:latest config set system.keep_versions 20
docker start flats
docker exec flats flats config show
```

The image's entrypoint is `flats` and its default command is `serve`, so
arguments after the image name replace the command: start them with `serve`
to pass flags to the host, as in the published-ports example, or name another
subcommand, for example `docker run --rm ghcr.io/gosuda/flats:latest version`.
With `config.json`, only `--listen`, `--local-addr`, `--console-host` and
`--runtime` can be overridden for a run; set everything else in the file.
The console's Settings page can change the system and Portal settings and
`network.permitted` while the host runs, as on a native host.

## Using the host

With host networking or published ports, use the host's own `flats` CLI and
agents as usual. The image also carries the CLI:

```sh
docker exec flats flats status
docker exec flats flats approvals
```

`docker exec` runs inside the container, so `flats deploy` there needs the site
files inside it as well; deploying from the host CLI or an agent is usually
simpler. The image healthcheck runs `flats status`; if you move the listener
to another port, also set `-e FLATS_URL=http://127.0.0.1:<port>`.

The image has no shell. To look at the volume, stop the host and mount it in a
one-off container, for example `docker run --rm -v flats-data:/data:ro alpine
ls -la /data`, or use `docker debug flats` where available.

## Hardening

The host needs no capabilities and no writable root filesystem:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  -v flats-data:/data ghcr.io/gosuda/flats:latest
```

Server flats still run in separate worker processes with WebAssembly limits.
`scripts/container-smoke.sh` checks this configuration end to end, with host
networking and with published loopback ports, on a Linux Docker host. Anyone
who can run `docker` on the host can read the data volume and reach the
container, so treat Docker access as operator access and do not give it to
agents.

## Upgrading and backups

Upgrade by replacing the container and keeping the volume:

```sh
docker pull ghcr.io/gosuda/flats:latest
docker rm -f flats
docker run -d --name flats ...   # the same command as before
```

Pin a version tag rather than `latest` when you want upgrades to happen only
when you choose them. Back up before upgrades whose release notes mention data
changes. Stop the container first, so `flats.db` is not copied while live, then
copy the whole volume:

```sh
docker stop flats
docker run --rm -v flats-data:/data:ro -v "$PWD:/backup" alpine \
  tar -czf /backup/flats-data.tar.gz -C /data .
docker start flats
```

Keep the Tailscale auth key, if you use one, backed up separately. Restore with
the same or a newer release: an older binary refuses a newer `config.json` or
database.

## Building the image

The [Dockerfile](../Dockerfile) packages release archives from `dist/`; it does
not compile:

```sh
FLATS_RELEASE_TARGETS="linux/amd64 linux/arm64" scripts/build-release.sh v0.0.0-dev dist
docker buildx build --platform linux/amd64,linux/arm64 -t flats:dev .
```

A single-platform `docker build -t flats:dev .` needs only the archive for
that platform. `--build-arg BASE_IMAGE=...` selects another base image and
`--build-arg UNPACK_IMAGE=...` the image that checks and unpacks the archive
(for example a registry mirror). See
[Releases and installation](release.md#container-image) for how releases
publish it.
