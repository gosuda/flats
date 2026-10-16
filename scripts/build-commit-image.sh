#!/bin/sh
# Build, verify and publish one exact commit using current workflow tooling.
set -eu
commit=$1
# Retrying a failed CI job can rerun its dependent image job. An existing full
# SHA tag in our trusted package namespace records completed publication; do
# not rebuild it on an automatic retry. Explicit manual runs remain a repair
# path. A missing image (or failed registry lookup) takes the normal build path.
if [ "${GITHUB_EVENT_NAME:-}" = push ] && [ "${GITHUB_REF:-}" = refs/heads/main ] &&
	[ "${GITHUB_RUN_ATTEMPT:-1}" -gt 1 ] &&
	docker buildx imagetools inspect "$IMAGE:sha-$commit" >/dev/null 2>&1; then
	echo "Already published $commit; skipping CI retry"
	exit 0
fi
root=$(pwd)
work=$(mktemp -d "$RUNNER_TEMP/flats-image.XXXXXX")
source_dir=$work/source
trap 'git -C "$root" worktree remove --force "$source_dir" 2>/dev/null || true; rm -rf "$work"' EXIT
short=$(printf '%.7s' "$commit")
version=v0.0.0-sha-$short
git worktree add --detach "$source_dir" "$commit"
# This value comes only from the main CI caller after all seven shards (and
# vet) succeeded. Match the exact source/tooling SHA, never a PR or descendant.
if [ "${GITHUB_EVENT_NAME:-}" = push ] && [ "${GITHUB_REF:-}" = refs/heads/main ] &&
	[ "${VERIFIED_COMMIT:-}" = "$commit" ] && [ "${GITHUB_SHA:-}" = "$commit" ] &&
	[ "$(git rev-parse HEAD)" = "$commit" ]; then
	echo "Reusing successful CI vet and all test shards for $commit"
else
	# go test ./... as CI's shards, one after another: run all at once on one
	# runner, the docs tests miss their wall-clock budgets. The shard script comes
	# from the workflow's commit, like the rest of the tooling; see ci-test.sh.
	# Every shard runs even after one fails, so the log shows all failures.
	(
		cd "$source_dir"
		go vet ./...
		failed=0
		for shard in runtime-soak runtime-codec runtime docs-soak docs-budget docs other; do
			"$root/scripts/ci-test.sh" "$shard" || failed=1
		done
		[ "$failed" = 0 ]
	)
fi
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
# Smoke-test the amd64 filesystem layers before publishing either architecture.
# The pushed image config additionally carries the source/tooling labels below.
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
