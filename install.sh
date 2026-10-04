#!/bin/sh
# Flats installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/gosuda/flats/main/install.sh | sh
#
# Downloads the prebuilt flats binary for this OS and CPU from the GitHub
# release, verifies its SHA-256 checksum, installs it and runs Flats as a
# background service that starts at login and restarts if it exits: a
# launchd agent on macOS, a systemd user service on Linux.
#
# A new service runs `flats serve` with its defaults (Local network, Portal
# off) and an operator credential file. The installer creates that file with
# a random credential when it does not exist and never prints the credential.
# When the service is already installed, the installer replaces the binary
# and restarts the service with its existing settings.
#
# Options (pass them after `sh -s --` when piping):
#   --dir DIR              install directory (FLATS_INSTALL_DIR, default ~/.local/bin)
#   --version TAG          release to install (FLATS_VERSION, default latest)
#   --credential-file PATH operator credential file for a new service
#                          (FLATS_CREDENTIAL_FILE, default ~/.config/flats-operator/credential)
#   --no-service           install only the binary (FLATS_NO_SERVICE=1)
#   -- SERVE-FLAGS...      (re)install the service with these `flats serve` flags
#
# FLATS_DATA selects the service data directory. FLATS_DOWNLOAD_BASE replaces
# the release download location with another https URL or a local directory
# holding the release files (for mirrors and tests).
#
# The installer never uses sudo and never edits shell profiles.

set -eu

REPO=gosuda/flats
DEFAULT_LISTEN=127.0.0.1:7878

tmp_dir=

say() {
	printf 'flats-install: %s\n' "$*"
}

warn() {
	printf 'flats-install: warning: %s\n' "$*" >&2
}

die() {
	printf 'flats-install: error: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [ -n "$tmp_dir" ] && [ -d "$tmp_dir" ]; then
		rm -rf "$tmp_dir"
	fi
}

usage() {
	cat <<'EOF'
Usage: install.sh [options] [-- serve-flags...]

Installs the prebuilt flats binary for macOS or Linux and runs it as a
background service (launchd agent on macOS, systemd user service on Linux).

  --dir DIR               install directory (default: ~/.local/bin)
  --version TAG           release tag to install (default: latest)
  --credential-file PATH  operator credential file for a new service
                          (default: ~/.config/flats-operator/credential)
  --no-service            install only the binary
  -- SERVE-FLAGS...       (re)install the service with these `flats serve` flags
  -h, --help              show this help

Environment: FLATS_INSTALL_DIR, FLATS_VERSION, FLATS_CREDENTIAL_FILE,
FLATS_NO_SERVICE=1, FLATS_DATA, FLATS_DOWNLOAD_BASE.
EOF
}

has() {
	command -v "$1" >/dev/null 2>&1
}

# fetch SOURCE DEST copies a release file from https or a local directory.
fetch() {
	case "$1" in
	https://*) ;;
	*)
		cp "$1" "$2"
		return
		;;
	esac
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

# api_up URL succeeds when a Flats server answers at URL.
api_up() {
	if has curl; then
		curl -fsS --max-time 3 -o /dev/null "$1/api/status" 2>/dev/null
	elif has wget; then
		wget -q -T 3 -O /dev/null "$1/api/status" 2>/dev/null
	else
		return 1
	fi
}

sha256_of() {
	if has sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif has shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

detect_platform() {
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported operating system: $(uname -s) (Flats supports macOS and Linux)" ;;
	esac
	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported CPU architecture: $arch (supported: amd64, arm64)" ;;
	esac
	# An x86_64 shell under Rosetta still runs on Apple silicon.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
}

# service_file prints the launchd plist or systemd unit path.
service_file() {
	if [ "$os" = darwin ]; then
		printf '%s\n' "$HOME/Library/LaunchAgents/dev.flats.serve.plist"
	else
		printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/flats.service"
	fi
}

# listen_url prints the console URL for the serve flags given as arguments.
listen_url() {
	listen=$DEFAULT_LISTEN
	while [ $# -gt 0 ]; do
		case "$1" in
		--listen | -listen)
			[ $# -ge 2 ] && listen=$2
			;;
		--listen=* | -listen=*) listen=${1#*=} ;;
		esac
		shift
	done
	printf 'http://%s\n' "$listen"
}

# service_url prints the console URL of the installed service, read from the
# words of its plist or unit file.
service_url() {
	# shellcheck disable=SC2046 # split the file into words on purpose.
	listen_url $(sed -e 's/<[^>]*>/ /g' -e 's/"/ /g' -e 's/^ExecStart=//' "$(service_file)")
}

has_credential_flag() {
	for a in "$@"; do
		case "$a" in
		--operator-credential-file | -operator-credential-file | \
			--operator-credential-file=* | -operator-credential-file=*) return 0 ;;
		esac
	done
	return 1
}

# check_service_manager fails when the per-user service manager is unusable.
check_service_manager() {
	if [ "$os" = darwin ]; then
		launchctl print "gui/$(id -u)" >/dev/null 2>&1 ||
			die "no macOS login session for $(id -un) (launchd gui domain); log in on the Mac, or rerun with --no-service"
	else
		has systemctl ||
			die "systemd is required to run the service; rerun with --no-service and run \`flats serve\` under your service manager"
		systemctl --user show-environment >/dev/null 2>&1 ||
			die "no systemd user manager for $(id -un) (a login session provides one; is XDG_RUNTIME_DIR set?); rerun with --no-service"
	fi
}

ensure_credential() {
	if [ -e "$credential_file" ] || [ -L "$credential_file" ]; then
		[ -f "$credential_file" ] && [ ! -L "$credential_file" ] ||
			die "$credential_file must be a regular file, not a link or directory"
		# shellcheck disable=SC2046 # split ls output into fields on purpose.
		set -- $(ls -ln "$credential_file")
		# Drop the xattr/ACL/SELinux marker that ls may append to the mode.
		[ "${1%[@+.]}" = "-rw-------" ] && [ "$3" = "$(id -u)" ] ||
			die "$credential_file must be owned by $(id -un) with mode 0600 (chmod 600 it)"
		say "using existing operator credential $credential_file"
		return 0
	fi
	has od || die "od is required to generate the operator credential"
	cred_dir=$(dirname "$credential_file")
	(umask 077 && mkdir -p "$cred_dir") || die "cannot create $cred_dir"
	staged=$credential_file.tmp.$$
	(
		umask 077
		# 32 random bytes as 64 hex characters.
		od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$staged"
		printf '\n' >>"$staged"
	) || die "cannot write $credential_file"
	chmod 600 "$staged"
	if [ "$(wc -c <"$staged" | tr -d ' ')" -ne 65 ]; then
		rm -f "$staged"
		die "could not read 32 random bytes from /dev/urandom"
	fi
	mv "$staged" "$credential_file" || die "cannot write $credential_file"
	created_credential=1
	say "created operator credential $credential_file (mode 0600)"
}

enable_linger() {
	has loginctl || return 0
	user=$(id -un)
	[ "$(loginctl show-user "$user" -p Linger --value 2>/dev/null || true)" = yes ] && return 0
	if loginctl --no-ask-password enable-linger "$user" >/dev/null 2>&1; then
		say "enabled systemd lingering so Flats keeps running after you log out"
	else
		warn "Flats stops when you log out; to keep it running, run: sudo loginctl enable-linger $user"
	fi
}

restart_service() {
	if [ "$os" = darwin ]; then
		launchctl kickstart -k "gui/$(id -u)/dev.flats.serve" >/dev/null ||
			die "could not restart the launchd agent; reinstall it with: $binary install -- <serve flags>"
	else
		{ systemctl --user daemon-reload && systemctl --user restart flats.service; } ||
			die "could not restart flats.service; see: systemctl --user status flats"
	fi
}

# service_running succeeds when the service manager reports a running service.
service_running() {
	if [ "$os" = darwin ]; then
		launchctl print "gui/$(id -u)/dev.flats.serve" 2>/dev/null | grep -q 'state = running'
	else
		[ "$(systemctl --user is-active flats.service 2>/dev/null || true)" = active ]
	fi
}

wait_healthy() {
	i=0
	while [ $i -lt 30 ]; do
		if service_running && api_up "$url"; then
			return 0
		fi
		sleep 1
		i=$((i + 1))
	done
	return 1
}

main() {
	install_dir=${FLATS_INSTALL_DIR:-}
	version=${FLATS_VERSION:-latest}
	credential_file=${FLATS_CREDENTIAL_FILE:-}
	no_service=${FLATS_NO_SERVICE:-}
	serve_args_given=
	while [ $# -gt 0 ]; do
		case "$1" in
		--dir | --version | --credential-file)
			[ $# -ge 2 ] || die "$1 requires a value"
			case "$1" in
			--dir) install_dir=$2 ;;
			--version) version=$2 ;;
			--credential-file) credential_file=$2 ;;
			esac
			shift 2
			;;
		--dir=*) install_dir=${1#*=} && shift ;;
		--version=*) version=${1#*=} && shift ;;
		--credential-file=*) credential_file=${1#*=} && shift ;;
		--no-service) no_service=1 && shift ;;
		--)
			shift
			serve_args_given=1
			break
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
	# Everything left in "$@" is serve flags.
	case "$no_service" in
	"" | 0) no_service= ;;
	*) no_service=1 ;;
	esac
	[ -z "$no_service" ] || [ -z "$serve_args_given" ] ||
		die "serve flags after -- need the service; drop --no-service"

	[ -n "${HOME:-}" ] || die "HOME is not set"
	[ -n "$install_dir" ] || install_dir=$HOME/.local/bin
	[ -n "$credential_file" ] || credential_file=${XDG_CONFIG_HOME:-$HOME/.config}/flats-operator/credential
	case "$install_dir" in /*) ;; *) install_dir=$(pwd)/$install_dir ;; esac
	case "$credential_file" in /*) ;; *) credential_file=$(pwd)/$credential_file ;; esac
	binary=$install_dir/flats

	case "$version" in
	"") die "empty version" ;;
	latest | v*) ;;
	[0-9]*) version=v$version ;;
	esac

	detect_platform
	has tar || die "tar is required"
	has awk || die "awk is required"

	# Decide what happens to the service before touching anything, so a
	# conflict stops the installer with the old binary still in place.
	mode=none
	if [ -z "$no_service" ]; then
		check_service_manager
		if [ -f "$(service_file)" ] && [ -z "$serve_args_given" ]; then
			mode=restart
			url=$(service_url)
			if ! grep -qF "$binary" "$(service_file)"; then
				warn "the installed service does not run $binary and keeps its current binary"
				warn "to switch it, rerun with: -- <flats serve flags>"
			fi
		else
			mode=install
			url=$(listen_url "$@")
			if [ ! -f "$(service_file)" ] && api_up "$url"; then
				die "a Flats server is already running at $url outside the service; stop it first, or rerun with --no-service"
			fi
		fi
	fi

	asset=flats_${os}_${arch}.tar.gz
	if [ -n "${FLATS_DOWNLOAD_BASE:-}" ]; then
		base=${FLATS_DOWNLOAD_BASE%/}
	elif [ "$version" = latest ]; then
		base=https://github.com/$REPO/releases/latest/download
	else
		base=https://github.com/$REPO/releases/download/$version
	fi

	tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/flats-install.XXXXXX")
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	say "downloading $asset ($version)"
	fetch "$base/$asset" "$tmp_dir/$asset" ||
		die "download failed: $base/$asset (does release $version exist?)"
	fetch "$base/checksums.txt" "$tmp_dir/checksums.txt" ||
		die "download failed: $base/checksums.txt"
	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp_dir/checksums.txt")
	[ -n "$want" ] || die "checksums.txt has no entry for $asset"
	[ "$want" = "$(sha256_of "$tmp_dir/$asset")" ] || die "checksum mismatch for $asset"
	tar -xzf "$tmp_dir/$asset" -C "$tmp_dir" flats || die "cannot extract $asset"
	[ -f "$tmp_dir/flats" ] || die "$asset has no flats binary"
	chmod 0755 "$tmp_dir/flats"
	new_version=$("$tmp_dir/flats" version 2>/dev/null) ||
		die "the downloaded binary does not run on this system"

	mkdir -p "$install_dir" || die "cannot create $install_dir; pass a writable --dir"
	[ -w "$install_dir" ] || die "$install_dir is not writable; pass a writable --dir"
	# Copy next to the target and rename, so a running flats keeps its old
	# executable and nobody observes a partially written binary.
	staged=$install_dir/.flats.install.$$
	cp "$tmp_dir/flats" "$staged" || die "cannot write to $install_dir"
	chmod 0755 "$staged"
	mv -f "$staged" "$binary" || {
		rm -f "$staged"
		die "cannot install $binary"
	}
	say "installed $binary: $new_version"

	created_credential=
	case "$mode" in
	restart)
		say "restarting the existing Flats service"
		restart_service
		;;
	install)
		if ! has_credential_flag "$@"; then
			ensure_credential
			set -- "$@" --operator-credential-file "$credential_file"
		fi
		say "installing the Flats service"
		"$binary" --url "$url" install --executable "$binary" -- "$@" ||
			die "service installation failed"
		;;
	esac

	if [ "$mode" != none ]; then
		if [ "$os" = linux ]; then
			enable_linger
		fi
		if wait_healthy; then
			say "Flats is running at $url"
		elif [ "$os" = darwin ]; then
			die "the service did not become healthy; see the logs listed above and \`$binary status\`"
		else
			die "the service did not become healthy; see: journalctl --user -u flats.service"
		fi
	fi

	case ":${PATH}:" in
	*":$install_dir:"*) ;;
	*)
		say "$install_dir is not on PATH; add this line to your shell profile:"
		# shellcheck disable=SC2016 # $PATH is meant literally here.
		printf '\n    export PATH="%s:$PATH"\n\n' "$install_dir"
		;;
	esac

	if [ -n "$created_credential" ]; then
		copy="cat '$credential_file'"
		if [ "$os" = darwin ]; then
			copy="pbcopy < '$credential_file'"
		fi
		cat <<EOF

Operator credential: $credential_file (not shown here)
  Save it in your password manager. Enter it in the console under
  "Unlock decisions" to approve publishes. To copy it: $copy
  Keep it away from agents: do not paste it into agent chats or logs.
EOF
	fi
	if [ "$mode" = none ]; then
		cat <<'EOF'

Run a host in the foreground:  flats serve --operator-credential-stdin
Or as a background service:    rerun this installer without --no-service
EOF
	else
		status="flats status"
		if [ "$url" != "http://$DEFAULT_LISTEN" ]; then
			status="flats --url $url status"
		fi
		cat <<EOF

Console:  $url
Status:   $status
Remove:   flats uninstall   (keeps your flats and data)
EOF
	fi
}

main "$@"
