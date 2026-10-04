#!/bin/sh
# Builds the release archives that install.sh downloads.
#
#   scripts/build-release.sh v1.2.3 dist
#
# Writes flats_<os>_<arch>.tar.gz for every supported platform and a
# checksums.txt in sha256sum format to the output directory.
# FLATS_RELEASE_TARGETS (space-separated os/arch) narrows the platforms.

set -eu

[ $# -eq 2 ] || {
	echo "usage: $0 <version> <output-dir>" >&2
	exit 2
}
version=$1
out=$2

case "$version" in
v[0-9]*) ;;
*)
	echo "version must look like v1.2.3, got: $version" >&2
	exit 2
	;;
esac

# Keep macOS tar from adding AppleDouble files when building locally.
COPYFILE_DISABLE=1
export COPYFILE_DISABLE

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/flats-release.XXXXXX")
trap 'rm -rf "$work"' EXIT

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	owner_flags='--owner=0 --group=0 --numeric-owner'
else
	owner_flags='--uid 0 --gid 0 --uname root --gname wheel'
fi

for target in ${FLATS_RELEASE_TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64}; do
	os=${target%/*}
	arch=${target#*/}
	name=flats_${os}_${arch}
	stage=$work/$name
	mkdir -p "$stage"
	echo "building $name"
	(cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X github.com/gosuda/flats/internal/cli.Version=$version -X github.com/gosuda/flats/internal/app.Version=$version" \
		-o "$stage/flats" ./cmd/flats)
	cp "$root/LICENSE" "$root/README.md" "$stage/"
	# Record root ownership so archives do not carry the build user's name.
	# shellcheck disable=SC2086 # owner_flags is a word list.
	(cd "$stage" && tar -czf "$out/$name.tar.gz" $owner_flags flats LICENSE README.md)
done

(cd "$out" && sha256 flats_*.tar.gz >checksums.txt)
cat "$out/checksums.txt"
