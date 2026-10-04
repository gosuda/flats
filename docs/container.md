# Running Flats in a container

Each release publishes a Linux image for `amd64` and `arm64`:

```text
ghcr.io/gosuda/flats:1.2.3     # one release (tags drop the leading v)
ghcr.io/gosuda/flats:1.2       # newest patch of a minor release
ghcr.io/gosuda/flats:latest    # newest stable release; prereleases never get it
```

The image runs the same `flats` binary as that release's
`flats_linux_<arch>.tar.gz`, on Alpine, as the unprivileged user `65532`. It
has two volumes:

| Path | Holds |
| --- | --- |
| `/data` | the data directory (`FLATS_DATA`): flats, versions, SQLite databases, FILES, `secret.key`, Tailscale and Portal state |
| `/run/flats-operator` | only the operator credential, kept apart from data and its backups |

Verify an image before you run it:

```sh
gh attestation verify oci://ghcr.io/gosuda/flats:1.2.3 --repo gosuda/flats
```

## Choose a network mode

Flats trusts loopback: the CLI, local agents and the console reach the
management listener on `127.0.0.1:7878`, and the console accepts the operator
credential only over loopback or HTTPS. A container must keep that property,
so two modes are supported.

**Linux host networking.** The container shares the host's loopback, so the
console, CLI, MCP endpoint and `http://<flat>.localhost:7879` work exactly as
with a native install:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  -v flats-data:/data -v flats-operator:/run/flats-operator \
  ghcr.io/gosuda/flats:latest
```

**Tailscale, on any Docker host** (including Docker Desktop or Rancher Desktop
on macOS). Publish no ports. The console and permitted flats get their own
tailnet nodes with HTTPS, and their state lives in `/data`:

```sh
docker run -d --name flats --restart unless-stopped \
  -v flats-data:/data -v flats-operator:/run/flats-operator \
  -v "$PWD/tailscale-authkey:/run/secrets/tailscale-authkey:ro" \
  ghcr.io/gosuda/flats:latest serve --network tailscale \
  --authkey-file /run/secrets/tailscale-authkey
```

The auth key must be reusable and untagged, and readable by uid `65532`.
Without one, each node waits for an interactive login; `docker exec flats
flats status` shows its login link. Agents outside the container reach MCP at
the console's tailnet URL plus `/mcp`, with the same limits as any non-loopback
agent.

**Do not publish the management port** with `-p 7878:7878` on a bridge
network. Published connections do not arrive over loopback, so the console
refuses to unlock (`operator_secure_transport_required`) and loopback-only MCP
tools are unavailable, while the agent API stays open to anything that can
reach the port.

Arguments after the image name are `flats serve` flags; Portal and other
providers are chosen the same way as for a native host (see the
[README](../README.md#private-and-public-deployment)). Any other subcommand
runs instead of `serve`, for example `docker run --rm ghcr.io/gosuda/flats
version`.

## Operator credential

On first start the entrypoint creates a random credential at
`/run/flats-operator/credential` (mode 0600, owned by `65532`) and does not
print it. Copy it into your password manager once and do not paste it into
agent chat, commands or logs:

```sh
docker exec flats cat /run/flats-operator/credential
```

Then open the console and choose **Unlock decisions**, as with a native host.
Later starts reuse the file. Anyone who can run `docker` on the host can read
the volume, so treat Docker access as operator access and do not give it to
agents.

To bring your own credential, place a file in the operator volume that is a
regular file owned by uid `65532`, mode 0600, holding one 32–4096-byte value;
or pass `--operator-credential-file PATH` or `--operator-credential-stdin`
(foreground, `docker run -it`), which turns off the generated file.
`FLATS_CREDENTIAL_FILE` moves the default path.

## Using the host

With host networking, use the host's own `flats` CLI and agents as usual.
The image also carries the CLI:

```sh
docker exec flats flats status
docker exec flats flats approvals
```

`docker exec` runs inside the container, so `flats deploy` there needs the
site files inside it as well; deploying from the host CLI or an agent is
usually simpler. The image healthcheck runs `flats status`; if you move the
listener with `--listen`, also set `-e FLATS_URL=http://<that address>`.

## Hardening

The host needs no capabilities and no writable root filesystem:

```sh
docker run -d --name flats --restart unless-stopped --network host \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  -v flats-data:/data -v flats-operator:/run/flats-operator \
  ghcr.io/gosuda/flats:latest
```

Server flats still run in separate worker processes with WebAssembly limits.
`scripts/container-smoke.sh` checks this configuration end to end on a Linux
Docker host.

## Upgrading and backups

Upgrade by replacing the container and keeping both volumes:

```sh
docker pull ghcr.io/gosuda/flats:latest
docker rm -f flats
docker run -d --name flats ...   # the same command as before
```

Pin a version tag rather than `latest` when you want upgrades to happen only
when you choose them. Back up before upgrades whose release notes mention data
changes: stop the container (`docker stop flats`), then copy the whole data
volume, including `secret.key`:

```sh
docker run --rm -v flats-data:/data:ro -v "$PWD:/backup" alpine \
  tar -czf /backup/flats-data.tar.gz -C /data .
```

Downgrading across a data migration is not supported; restore the backup taken
before the upgrade.

## Building the image

The [Dockerfile](../Dockerfile) packages release archives from `dist/`; it does
not compile:

```sh
FLATS_RELEASE_TARGETS="linux/amd64 linux/arm64" scripts/build-release.sh v0.0.0-dev dist
docker buildx build --platform linux/amd64,linux/arm64 -t flats:dev .
```

A single-platform `docker build -t flats:dev .` needs only the archive for
that platform. `--build-arg ALPINE_IMAGE=...` selects another Alpine image or
mirror. See [Releases and installation](release.md#container-image) for how
releases publish it.
