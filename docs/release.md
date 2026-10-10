# Releases and installation

This is the release and deployment workflow for Flats: how maintainers cut a
release, what it publishes, and how operators install and upgrade a host.

## What a release publishes

Every `v*` tag on `main` produces a GitHub release with:

- `flats_<os>_<arch>.tar.gz` for `darwin` and `linux` on `amd64` and `arm64`.
  Each archive holds the CGO-free `flats` binary (built with `-trimpath`, the
  tag stamped into `flats version`), `LICENSE` and `README.md`.
- `checksums.txt`: SHA-256 of every archive, in `sha256sum` format.
- A GitHub build provenance attestation for every archive.
- A multi-platform container image, `ghcr.io/gosuda/flats` for `linux/amd64`
  and `linux/arm64`, that packages those Linux archives (see
  [Container image](#container-image)).

`install.sh` on `main` downloads from the latest non-prerelease release. Tags
with a hyphen (`v1.3.0-rc.1`) are prereleases: they never become latest, so
the installer and its upgrade path ignore them unless asked for with
`--version`. Once a tag exists, `go install github.com/gosuda/flats/cmd/flats@latest`
resolves to the highest released semantic version (prereleases excluded)
rather than `main`, possibly after a Go module proxy delay; use `@main` for
unreleased changes.

## Build version

Every build reports its version in `flats version`, `/api/status`
(`system.build`, with `system.version` kept for older clients), the MCP
server info and next to the brand in the console:

| Build | Version |
| --- | --- |
| Release archive or image | the tag, stamped by `build-release.sh` (`v0.2.0`) |
| `go install …@v0.2.0`, or `go build` of a clean tagged checkout | the module version (`v0.2.0`) |
| Main commit image (`sha-<commit>`, stamped `v0.0.0-sha-<commit>`) | the commit (`dba5279`) |
| `go build` of a checkout | the commit, 7 hex digits (`dba5279`), with `-dirty` for uncommitted changes |
| `go install …@main` | the commit prefix of the pseudo-version (`dba5279`) |
| none of these | `dev` |

A `v0.0.0-…` stamp marks a development build, never a release.
`internal/buildinfo` resolves it; `system.build` also carries the full commit
when the build recorded one, whether the source was modified, and the Go
version and platform. The console links a release to its release page and a
clean development build to its commit.

## Cutting a release

1. Land every change through a reviewed PR. Wait until CI on `main` passes,
   including the `install-script` jobs on Linux (systemd) and macOS (launchd).
2. Pick the version with semantic versioning. Call out data or flag changes
   that operators must act on.
3. Read the live `main` ref (`git ls-remote origin refs/heads/main`), then tag
   exactly the commit you verified and push the tag:

   ```sh
   git tag -a v1.2.3 <main-commit> -m v1.2.3
   git push origin v1.2.3
   ```

4. The `Release` workflow (`.github/workflows/release.yml`) refuses tags that
   are not on `main` and runs `go vet` and `go test` in a read-only job. A
   second job builds the archives with `scripts/build-release.sh`, refuses
   binaries whose build info is not clean tagged source, attests the archives
   and creates the release with generated notes. A stable tag becomes latest.
   A third job pushes and attests the container image.
5. Verify the published release before announcing it:

   ```sh
   gh release view v1.2.3 --repo gosuda/flats    # 4 archives + checksums.txt
   gh release download v1.2.3 --repo gosuda/flats --pattern '*darwin_arm64*'
   gh attestation verify flats_darwin_arm64.tar.gz --repo gosuda/flats
   ```

   On a disposable macOS and Linux machine, run the installer and confirm
   that `flats version` prints the tag and `flats status` reports a running
   host. Check the image as well:

   ```sh
   gh attestation verify oci://ghcr.io/gosuda/flats:1.2.3 --repo gosuda/flats
   docker run --rm ghcr.io/gosuda/flats:1.2.3 version
   ```
6. Edit the release notes to add upgrade notes: data migrations, changed
   flags, and whether operators should back up first.

Do not move, delete or re-push a published tag. The Go module proxy caches
tagged versions, and operators may already have verified its checksums.

## Backfilling a release without binaries

`v0.1.0` was published before the release tooling existed. Attach archives
to such a release by running the workflow manually from `main` with its tag:

```sh
gh workflow run release.yml --repo gosuda/flats --ref main -f tag=v0.1.0
```

The workflow tests the tag's source, builds it with the tooling from the
workflow's commit and uploads the archives, then `checksums.txt`. The upload
fails if an asset with the same name already exists; published assets are
never replaced. If a backfill fails partway, delete every asset it uploaded
and run it again: rebuilt archives are not byte-identical, so never mix
assets from two runs.

A backfilled attestation names the `main` commit the workflow ran from, not
the tag's commit. Add the tag's commit to the release notes, and verify
backfilled archives by signer workflow rather than by source ref:

```sh
gh attestation verify flats_darwin_arm64.tar.gz --repo gosuda/flats \
  --signer-workflow gosuda/flats/.github/workflows/release.yml
```

The binaries themselves record the tag's commit: `go version -m flats` shows
it as `vcs.revision`.

## Container image

The `image` job of the release workflow runs after the release exists. It
downloads the release's `flats_linux_*.tar.gz` and `checksums.txt`, checks the
checksums and archive attestations, and builds the [Dockerfile](../Dockerfile)
for `linux/amd64` and `linux/arm64` from them, so the image contains the
published binaries rather than a second build. It pushes to
`ghcr.io/gosuda/flats` and attests the image digest in the registry. Tags drop
the leading `v`:

| Tag | Pushed for |
| --- | --- |
| `1.2.3`, `1.3.0-rc.1` | every release, including backfills and prereleases |
| `1.2` | pushed stable tags |
| `latest` | pushed stable tags |

A backfill (`workflow_dispatch`) pushes only the exact version, so it never
moves `1.2` or `latest` back to an older release. Releases do not write
`sha-` tags: those identify the separate [commit builds](#commit-images).
Previously published release `sha-` tags remain until a commit build replaces
them. The `sha256-<digest>` tag is not a commit: it holds the image's
provenance attestation. Re-running the job rebuilds
the version tag from the same archives. The Dockerfile comes from the
workflow's commit, like `build-release.sh`.

The first push creates the package. An organization owner must make
`ghcr.io/gosuda/flats` public once in the package settings; until then pulls
need authentication. CI builds both platforms on every pull request and runs
`scripts/container-smoke.sh` against the runner's image. How operators run the
image is in [Running Flats in a container](container.md).

## Commit images

Pushes to `main` attempt to build and publish a separate multi-platform image for each
new commit along its first-parent history, including intermediate commits in
a batched push, under `sha-<7-character SHA>` and `sha-<40-hex commit>` tags.
A merge builds the merge commit, not every commit on the merged branch.
A newly created `main` branch builds its tip only. Existing historical commits
are not automatically backfilled. Other branches do not publish images;
pull requests, including forks, use CI's build-only checks.

GitHub skips push workflows for commit messages containing `[skip ci]`,
`[ci skip]`, `[no ci]`, `[skip actions]` or `[actions skip]`. A subsequent push
only selects its own new range, so it does not recover those skipped commits;
manually dispatch each missing SHA using the command below.

A commit is published only after vet, tests, clean revision verification and
the container smoke test succeed. A failure prevents that commit's publication;
later commits in its build lane are still attempted and the lane reports failure.
Each lane has a 360-minute timeout and remains subject to GitHub Actions runtime
limits; a timeout can leave unattempted commits that need manual recovery.

To build an older commit explicitly, run `Commit images`
(`.github/workflows/commit-images.yml`) from `main` with its full SHA:

```sh
gh workflow run commit-images.yml --repo gosuda/flats --ref main \
  -f commit=<full-sha>
```

Manual dispatch must run from `main`, and the selected commit must be
reachable from `origin/main`. The workflow uses
its own build tooling and Dockerfile to package the selected source.

Commit builds compile Linux archives from the selected commit with version
`v0.0.0-sha-<7-character SHA>` and package them in the image. The build checks
that binary metadata records that exact revision and clean source. These
archives are build inputs, not GitHub release assets; only versioned release
images promise the same binaries as the published release archives. Commit
images never move the release version, minor or `latest` tags. Prefer the
full SHA tag: a 7-character prefix is only a convenience and can collide.

Commit images include unsigned BuildKit provenance, not the GitHub-signed
attestation provided for release images. The build workflow's commit can
differ from the selected source commit for an intermediate push commit or a
manual build. Check `org.opencontainers.image.revision` and the binary's
`vcs.revision` against the full source SHA; the workflow's source ref alone
does not identify the selected source revision. The image label
`io.flats.image.tooling-revision` records the workflow/tooling commit separately
from `org.opencontainers.image.revision`, which records the selected source.

Check the binary identity and inspect the unsigned provenance:

```sh
docker run --rm ghcr.io/gosuda/flats:sha-<full-sha> version --json
docker buildx imagetools inspect ghcr.io/gosuda/flats:sha-<full-sha> \
  --format '{{json .Provenance}}'
```

The JSON version output's `commit` must equal the full selected SHA and `dirty`
must be `false`. Provenance inspection is supported by
[Docker Buildx](https://docs.docker.com/reference/cli/docker/buildx/imagetools/inspect/#format-the-output---format);
it does not verify a GitHub signature.

## Retracting a bad release

Never replace the assets of a published release: their checksums and
attestations would change under operators who already trust them. Publish a
fixed patch release instead. If the bad release must stop being installed
before the fix is ready, mark it as a prerelease and make the previous good
release latest:

```sh
gh release edit v1.2.3 --repo gosuda/flats --prerelease --latest=false
gh release edit v1.2.2 --repo gosuda/flats --latest
```

Operators who pinned `--version v1.2.3` can still download it. Point the
moving image tags back at the good release as well (also `1.2` when both are
in the same minor series):

```sh
docker buildx imagetools create -t ghcr.io/gosuda/flats:latest ghcr.io/gosuda/flats:1.2.2
```

A GitHub prerelease flag does not affect `go install …@latest`; add a
`retract v1.2.3` directive to `go.mod` in the fixed release so the Go
toolchain skips the bad version.

## Installing a host

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
```

The installer uses tools that macOS and common Linux distributions include:
`sh`, `curl` (or `wget`), `tar`, `awk`, `sha256sum` or `shasum`, and
`launchctl` or `systemctl --user` for the service. It needs no Go toolchain.
It:

1. Resolves the latest release, downloads the archive for this OS and CPU and
   its `checksums.txt`, and refuses a checksum mismatch.
2. Checks that the binary runs, then atomically installs it to
   `~/.local/bin/flats` (`--dir` changes this). It never uses sudo or edits
   shell profiles.
3. Registers the service with `flats install`: a launchd agent
   (`dev.flats.serve`) on macOS or a systemd user unit (`flats.service`) on
   Linux. It starts at login and restarts when it exits. The service runs
   `flats serve` with the defaults: Local network, Portal off, and the
   default data directory. On Linux the installer also enables lingering when
   polkit allows it.
4. Waits until `http://127.0.0.1:7878` answers.

Service registration is the default because a host is meant to stay up.
`--no-service` installs only the binary, for hosts that run `flats serve`
under another supervisor or in the foreground. Non-local providers
(Tailscale, Funnel, Portal) are never enabled by the installer; pass `flats
serve` flags after a second `--` to choose them explicitly:

```sh
curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh -s -- -- --network tailscale
```

| Option (after `sh -s --`) | Effect |
| --- | --- |
| `--version v1.2.3` | Install a specific release |
| `--dir DIR` | Install the binary somewhere other than `~/.local/bin` |
| `--no-service` | Install only the binary |
| `-- <serve flags>` | Reinstall the service with those flags written into `config.json` |

`flats status` shows whether the host answers. To check an archive you
downloaded yourself, run
`gh attestation verify flats_<os>_<arch>.tar.gz --repo gosuda/flats`.

## Upgrading a host

Run the installer again. It replaces the binary and, when the service is
running, restarts it with its existing settings. A stopped service stays
stopped; the installer prints the command that starts it. Passing `flats
serve` flags after `--` reinstalls the service with those flags written into
`config.json` instead.

A service installed before `config.json` existed still passes `flats serve`
flags. Its first start on the new release moves those flags, the console
settings and `network-provider.json` into `<data>/config.json`, after backing
up `flats.db` to `<data>/backups/`. The installer then prints the `flats
install` command that switches the service to `serve --config`. See
[Configuration and storage](configuration.md#moving-an-existing-installation).

The installer does not back up data. When the release notes mention data
changes, back up first:

1. Stop the service: `launchctl bootout gui/$(id -u)/dev.flats.serve` on
   macOS, `systemctl --user stop flats.service` on Linux.
2. Copy the whole data directory, including `config.json` and `secret.key`.
   It is `host.data_dir` in the service's `config.json`, or `FLATS_DATA` or
   the service's `--data` when set, otherwise
   `~/Library/Application Support/Flats` on macOS and
   `${XDG_CONFIG_HOME:-~/.config}/Flats` on Linux.
3. Run the installer, then start the service with the command it prints.

Published versions keep their numbers across the upgrade; versions that were
saved but never published become Private Draft revisions. Network grants are
not carried over from historical defaults; see
[Private and public access](networking.md#upgrading-provider-grants-from-an-older-host).

Install a specific release with `--version v1.2.3`. Downgrading across a data
migration is not supported; restore the backup taken before the upgrade.
`flats uninstall` removes the service and keeps the data.
