#!/bin/sh
# Build, verify and publish one exact commit using current workflow tooling.
set -eu
commit=$1
root=$(pwd)
work=$(mktemp -d "$RUNNER_TEMP/flats-image.XXXXXX")
source_dir=$work/source
trap 'git -C "$root" worktree remove --force "$source_dir" 2>/dev/null || true; rm -rf "$work"' EXIT
short=$(printf '%.7s' "$commit")
version=v0.0.0-sha-$short
git worktree add --detach "$source_dir" "$commit"
(cd "$source_dir" && go vet ./... && go test ./...)
FLATS_SOURCE_DIR="$source_dir" FLATS_RELEASE_TARGETS='linux/amd64 linux/arm64' \
	"$root/scripts/build-release.sh" "$version" "$work/dist"
for archive in "$work"/dist/*.tar.gz; do
	tar -xzOf "$archive" flats >"$work/flats.bin"
	go version -m "$work/flats.bin" >"$work/build-info"
	if ! grep -q 'vcs.modified=false' "$work/build-info" ||
		! grep -q "vcs.revision=$commit" "$work/build-info"; then
		echo "$archive was not built from clean source at $commit" >&2
		cat "$work/build-info" >&2
		exit 1
	fi
done
# Smoke-test the actual amd64 image before publishing either architecture.
cp "$root/Dockerfile" "$root/.dockerignore" "$work/"
docker buildx build --platform linux/amd64 --load -t "flats:sha-$commit" "$work"
tar -xzf "$work/dist/flats_linux_amd64.tar.gz" -C "$work" flats
"$root/scripts/container-smoke.sh" "flats:sha-$commit" "$version" "$work/flats"
# SHA refs are exclusively development builds; release tags never move them.
docker buildx build --platform linux/amd64,linux/arm64 --push \
	--provenance=mode=max \
	--tag "$IMAGE:sha-$short" --tag "$IMAGE:sha-$commit" \
	--label "org.opencontainers.image.revision=$commit" \
	--label "org.opencontainers.image.version=$version" \
	--label "io.flats.image.tooling-revision=$GITHUB_SHA" "$work"
git worktree remove --force "$source_dir"
