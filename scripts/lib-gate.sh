#!/bin/sh
# expected_version for the gate scripts; sourcing defines it and does nothing
# else. The sourcing script provides the fail() it exits through.

# expected_version prints the version stamp a binary reports: $1 itself, or,
# for a v0.0.0-… development stamp (a main commit image), the build commit
# from the binary $2's own build information.
expected_version() {
	case $1 in
	v0.0.0-*)
		info=$(go version -m "$2") || fail "cannot read the build information of $2"
		rev=$(printf '%s\n' "$info" | sed -n 's/.*vcs\.revision=\([0-9a-f]*\).*/\1/p')
		[ -n "$rev" ] || fail "$2 records no commit"
		dirty=
		printf '%s\n' "$info" | grep -q 'vcs\.modified=true' && dirty=-dirty
		printf '%.7s%s\n' "$rev" "$dirty"
		;;
	*) printf '%s\n' "$1" ;;
	esac
}
