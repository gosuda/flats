#!/bin/sh
# Flats installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
#
# Builds the CGO-free flats binary from source with `go install`. If a
# suitable Go toolchain is not on PATH, the official Go release for this
# platform is downloaded into a temporary directory, verified against its
# published SHA-256 checksum, used for the build and removed afterwards.
#
# Options (flags take precedence over environment variables):
#   --dir DIR          install directory     (FLATS_INSTALL_DIR, default ~/.local/bin)
#   --version VERSION  module version or ref (FLATS_VERSION, default latest; e.g. main, v1.2.3)
#
# The installer never uses sudo, never edits shell profiles and does not start
# or install the Flats host service.

set -eu

# Keep in sync with the go directive in go.mod.
MIN_GO_VERSION=1.27.1
MODULE=github.com/gosuda/flats
GO_DOWNLOAD_BASE=https://dl.google.com/go

tmp_dir=

say() {
	printf 'flats-install: %s\n' "$*"
}

die() {
	printf 'flats-install: error: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [ -n "$tmp_dir" ] && [ -d "$tmp_dir" ]; then
		# The Go module cache is read-only by default.
		chmod -R u+w "$tmp_dir" 2>/dev/null || true
		rm -rf "$tmp_dir"
	fi
}

usage() {
	cat <<'EOF'
Usage: install.sh [--dir DIR] [--version VERSION]

Installs the flats binary for macOS or Linux.

  --dir DIR          install directory (default: ~/.local/bin, env FLATS_INSTALL_DIR)
  --version VERSION  module version or ref to build (default: latest, env FLATS_VERSION)
  -h, --help         show this help
EOF
}

has() {
	command -v "$1" >/dev/null 2>&1
}

download() {
	if has curl; then
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$2" "$1"
	elif has wget; then
		# BusyBox wget has no --https-only; every URL used here is https.
		if wget --help 2>&1 | grep -q -- --https-only; then
			wget -q --https-only -O "$2" "$1"
		else
			wget -q -O "$2" "$1"
		fi
	else
		die "curl or wget is required"
	fi
}

sha256_of() {
	if has sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif has shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "sha256sum or shasum is required to verify the Go download"
	fi
}

# version_ge A B: succeeds when dotted version A >= B. Missing parts count as 0.
version_ge() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, x, "."); nb = split(b, y, ".")
		n = na > nb ? na : nb
		for (i = 1; i <= n; i++) {
			xi = (i <= na) ? x[i] + 0 : 0
			yi = (i <= nb) ? y[i] + 0 : 0
			if (xi > yi) exit 0
			if (xi < yi) exit 1
		}
		exit 0
	}'
}

detect_platform() {
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported operating system: $(uname -s) (Flats supports macOS and Linux)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported CPU architecture: $(uname -m) (supported: amd64, arm64)" ;;
	esac
}

# Prints the release number (for example 1.27.1) of the go on PATH, or
# nothing when go is missing or is not a release build.
installed_go_version() {
	has go || return 0
	v=$(go env GOVERSION 2>/dev/null) || return 0
	case "$v" in
	go[0-9]*) ;;
	*) return 0 ;;
	esac
	v=${v#go}
	v=${v%%[!0-9.]*}
	printf '%s\n' "$v"
}

bootstrap_go() {
	latest=$(mktemp "$tmp_dir/version.XXXXXX")
	download "https://go.dev/VERSION?m=text" "$latest"
	go_release=$(head -n 1 "$latest")
	case "$go_release" in
	go[0-9]*) ;;
	*) die "could not determine the latest Go release" ;;
	esac
	if ! version_ge "${go_release#go}" "$MIN_GO_VERSION"; then
		go_release=go$MIN_GO_VERSION
	fi

	archive=$go_release.$os-$arch.tar.gz
	say "downloading $go_release for $os/$arch (used only for this build)"
	download "$GO_DOWNLOAD_BASE/$archive" "$tmp_dir/$archive"
	download "$GO_DOWNLOAD_BASE/$archive.sha256" "$tmp_dir/$archive.sha256"
	want=$(awk '{print $1; exit}' "$tmp_dir/$archive.sha256")
	got=$(sha256_of "$tmp_dir/$archive")
	[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for $archive"

	tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
	rm -f "$tmp_dir/$archive"

	# Keep every Go side effect inside the temporary directory.
	PATH=$tmp_dir/go/bin:$PATH
	GOROOT=$tmp_dir/go
	GOPATH=$tmp_dir/gopath
	GOMODCACHE=$tmp_dir/gopath/pkg/mod
	GOCACHE=$tmp_dir/gocache
	GOENV=off
	GOFLAGS=-modcacherw
	GOTOOLCHAIN=auto
	export PATH GOROOT GOPATH GOMODCACHE GOCACHE GOENV GOFLAGS GOTOOLCHAIN
}

main() {
	install_dir=${FLATS_INSTALL_DIR:-}
	version=${FLATS_VERSION:-latest}
	while [ $# -gt 0 ]; do
		case "$1" in
		--dir)
			[ $# -ge 2 ] || die "--dir requires a value"
			install_dir=$2
			shift 2
			;;
		--dir=*)
			install_dir=${1#--dir=}
			shift
			;;
		--version)
			[ $# -ge 2 ] || die "--version requires a value"
			version=$2
			shift 2
			;;
		--version=*)
			version=${1#--version=}
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown option: $1"
			;;
		esac
	done
	if [ -z "$install_dir" ]; then
		[ -n "${HOME:-}" ] || die "HOME is not set; pass --dir"
		install_dir=$HOME/.local/bin
	fi
	[ -n "$version" ] || die "empty version"

	detect_platform
	has tar || die "tar is required"
	has awk || die "awk is required"

	tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/flats-install.XXXXXX")
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	current_go=$(installed_go_version)
	if [ -n "$current_go" ] && version_ge "$current_go" "$MIN_GO_VERSION"; then
		say "using installed Go $current_go"
	elif [ -n "$current_go" ] && version_ge "$current_go" 1.21; then
		say "installed Go $current_go is older than $MIN_GO_VERSION; Go will fetch a newer toolchain"
		GOTOOLCHAIN=auto
		export GOTOOLCHAIN
	else
		bootstrap_go
	fi

	say "building $MODULE/cmd/flats@$version"
	GOBIN=$tmp_dir/bin CGO_ENABLED=0 go install "$MODULE/cmd/flats@$version" || die "build failed"
	built=$tmp_dir/bin/flats
	[ -x "$built" ] || die "build produced no flats binary"
	resolved=$(go version -m "$built" 2>/dev/null | awk '$1 == "mod" { print $3; exit }')

	mkdir -p "$install_dir" || die "cannot create $install_dir; pass a writable --dir"
	[ -w "$install_dir" ] || die "$install_dir is not writable; pass a writable --dir"
	# Copy next to the target and rename, so a running flats keeps its old
	# executable and nobody observes a partially written binary.
	staged=$install_dir/.flats.install.$$
	cp "$built" "$staged" || die "cannot write to $install_dir"
	chmod 0755 "$staged"
	mv -f "$staged" "$install_dir/flats" || {
		rm -f "$staged"
		die "cannot install $install_dir/flats"
	}

	say "installed $install_dir/flats (${resolved:-$version})"

	case ":${PATH}:" in
	*":$install_dir:"*) ;;
	*)
		say "$install_dir is not on PATH; add this line to your shell profile:"
		# shellcheck disable=SC2016 # $PATH is meant literally here.
		printf '\n    export PATH="%s:$PATH"\n\n' "$install_dir"
		;;
	esac

	cat <<'EOF'
Next steps:
  flats serve --network local --portal=false --operator-credential-stdin
  Then open http://127.0.0.1:7878. See https://github.com/gosuda/flats#readme.

If a Flats host is already running, restart it to use the new binary.
EOF
}

main "$@"
