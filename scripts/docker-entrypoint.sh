#!/bin/sh
# Entrypoint of the Flats container image (see docs/container.md).
#
# With no arguments, flags only, or `serve` and flags, it runs `flats serve`
# with the operator credential file in the /run/flats-operator volume
# (FLATS_CREDENTIAL_FILE). Like install.sh, it creates that file with a random
# credential on first start and never prints the credential. Passing
# --operator-credential-file or --operator-credential-stdin turns this off.
#
# Any other arguments run that flats subcommand, for example `version` or
# `status`; a leading `flats` is accepted and dropped.

set -eu

credential_file=${FLATS_CREDENTIAL_FILE:-/run/flats-operator/credential}

say() {
	printf 'flats-entrypoint: %s\n' "$*" >&2
}

die() {
	say "$*"
	exit 1
}

# has_credential_flag reports whether the serve flags choose a credential
# source themselves. Flags end at the first non-flag argument or `--`.
has_credential_flag() {
	for arg in "$@"; do
		case $arg in
		--) return 1 ;;
		--operator-credential-file | -operator-credential-file | \
			--operator-credential-file=* | -operator-credential-file=* | \
			--operator-credential-stdin | -operator-credential-stdin | \
			--operator-credential-stdin=* | -operator-credential-stdin=*) return 0 ;;
		esac
	done
	return 1
}

ensure_credential() {
	if [ -e "$credential_file" ] || [ -L "$credential_file" ]; then
		# flats serve checks the type, mode and owner itself.
		return 0
	fi
	cred_dir=$(dirname "$credential_file")
	[ -d "$cred_dir" ] && [ -w "$cred_dir" ] ||
		die "cannot create the operator credential: $cred_dir is not a writable directory (mount a volume owned by $(id -u) there)"
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
	say "created operator credential $credential_file (mode 0600); it is not printed"
	say "copy it to your password manager once: docker exec <container> cat $credential_file"
}

[ "${1:-}" = flats ] && shift
case ${1:-} in
'' | -*) set -- serve "$@" ;;
esac

if [ "$1" = serve ]; then
	shift
	if ! has_credential_flag "$@"; then
		ensure_credential
		set -- --operator-credential-file "$credential_file" "$@"
	fi
	exec flats serve "$@"
fi
exec flats "$@"
