#!/bin/sh
# Runs one shard of the CI test suite, so CI can run shards in parallel jobs.
#
#   scripts/ci-test.sh <shard> [go test flags...]
#
# The QuickJS runtime and docs app packages hold most of the suite's time.
# Their tests run in parallel (t.Parallel) except those whose result depends
# on elapsed time, which Go runs one at a time before the parallel ones. Each
# package is split by test name: the named soak, codec and budget tests run in
# their own shards with -run, and the remaining shard runs everything else
# with -skip on the same patterns. A new or renamed test therefore always runs
# somewhere; at worst the shards become unbalanced. The board packages
# (internal/board and below) run in their own shard. The sheet packages
# (internal/sheet and below) run in three shards at their full sizes: the
# engine's undo tests in two, split by test name, and everything else in the
# third. The other shard runs every remaining package. Running every shard
# covers exactly `go test ./...`.

set -eu

[ $# -ge 1 ] || {
	echo "usage: $0 runtime-soak|runtime-codec|runtime|docs-soak|docs-budget|docs|board|sheet-undo-1|sheet-undo-2|sheet|other [go test flags...]" >&2
	exit 2
}
shard=$1
shift

runtime_soak='^(TestOutboundFetchLoopOwnershipCycleCollection|TestNearCapCycleCollection)$'
# A serial soak whose requests take seconds each: alone, it keeps its margin
# against the request timeout.
runtime_codec='^(TestLargeHostAndCodecOwnership)$'
docs_soak='^(TestConflictPrivacyAndRollbackStorageBudgets|TestPublicDocumentReadSoak|TestDocumentOwnershipSoak)$'
# The docs tests that stay serial because they check a wall-clock budget or a
# rate limit that refills over time. TestGuestUpdateLimits is serial for the
# same reason and stays out of this list on purpose: the Windows job leaves out
# this shard and must run a test of the guest limit.
docs_budget='^(TestGuestRoomsLeaveMemberRoomMemory|TestMaximumRewriteActivationBudget|TestLargeDocumentEditAndPingBudgets|TestLargeDisjointActivation|TestFullRewriteActivation|TestAgentEditManyBlocksWithinDeadline|TestVisitorHealthIsCheapAndCannotBlockCommit|Test100KiBDocumentTimings|TestUnauthorizedUpdatesPreserveWriterBudget|TestConnectionInputLimitPreservesOtherWriter|TestAuthorizedUpdatesConsumeWriterBudget|TestGuestBusyResendKeepsOrder)$'

# The sheet engine's undo tests, and about half of their time at full size:
# sheet-undo-1 runs these, sheet-undo-2 the other undo tests.
sheet_undo='^TestUndo'
sheet_undo_1='^(TestUndoNotesStayInBudget|TestUndoStampLinksStayBounded|TestUndoLargeTabDeletions|TestUndoNotesDoNotGrowWhenRowsBetweenThemAreWritten|TestUndoAfterBlankCollection)$'

# sheet runs the sheet's tests with their full-size memory and volume cases
# (docs/gates.md), which run in these shards and in no other lane. The
# shards took 2.6, 4.3 and 8.3 minutes on a developer machine at a load
# average of 20 to 36 (November 2026). The timeout leaves about 1.7 times
# the slowest, inside the 20-minute job; hosted runners are not measured.
# A caller's own -timeout comes later and wins.
sheet() {
	FLATS_SHEET_FULL=${FLATS_SHEET_FULL-1}
	export FLATS_SHEET_FULL
	GOFLAGS="-count=1${GOFLAGS:+ $GOFLAGS}" exec go test -timeout=14m "$@"
}

# The release and commit image workflows run this script from main against
# older source, which may predate a package. Its shards then have nothing to
# run.
need() {
	[ -d "$1" ] || {
		echo "$1 is not in this source; nothing to run" >&2
		exit 0
	}
}

case "$shard" in
runtime-soak) need internal/runtime && exec go test "$@" -run "$runtime_soak" ./internal/runtime ;;
runtime-codec) need internal/runtime && exec go test "$@" -run "$runtime_codec" ./internal/runtime ;;
runtime) need internal/runtime && exec go test "$@" -skip "$runtime_soak|$runtime_codec" ./internal/runtime ;;
docs-soak) need internal/apps/docs && exec go test "$@" -run "$docs_soak" ./internal/apps/docs ;;
docs-budget)
	need internal/apps/docs
	# Budgets measure the docs engine, not the disk (docs/gates.md has the
	# numbers), so the test binary's t.TempDir alone moves to /dev/shm:
	# t.TempDir prefers GOTMPDIR, while the go command's own build
	# directory stays put (/dev/shm can be small or noexec, as in Docker).
	# Without /dev/shm (macOS), nothing changes.
	if [ -d /dev/shm ] && [ -w /dev/shm ]; then
		exec go test -exec 'env TMPDIR=/dev/shm GOTMPDIR=/dev/shm' "$@" -run "$docs_budget" ./internal/apps/docs
	fi
	exec go test "$@" -run "$docs_budget" ./internal/apps/docs
	;;
docs) need internal/apps/docs && exec go test "$@" -skip "$docs_soak|$docs_budget" ./internal/apps/docs ;;
board) need internal/board && exec go test "$@" ./internal/board/... ;;
sheet-undo-1) need internal/sheet && sheet "$@" -run "$sheet_undo_1" ./internal/sheet ;;
sheet-undo-2) need internal/sheet && sheet "$@" -run "$sheet_undo" -skip "$sheet_undo_1" ./internal/sheet ;;
sheet) need internal/sheet && sheet "$@" -skip "$sheet_undo" ./internal/sheet/... ;;
other)
	# Separate assignments, so set -e stops on a failing go list instead of
	# testing no packages. Older source has another module path.
	module=$(go list -m)
	all=$(go list ./...)
	packages=$(printf '%s\n' "$all" | awk -v m="$module" '
		$0 != m "/internal/runtime" && $0 != m "/internal/apps/docs" &&
		$0 != m "/internal/board" && index($0, m "/internal/board/") != 1 &&
		$0 != m "/internal/sheet" && index($0, m "/internal/sheet/") != 1')
	[ -n "$packages" ] || {
		echo "no packages left for the other shard" >&2
		exit 1
	}
	# shellcheck disable=SC2086 # one argument per package path.
	GOFLAGS="-count=1${GOFLAGS:+ $GOFLAGS}" exec go test "$@" $packages
	;;
*)
	echo "unknown shard: $shard" >&2
	exit 2
	;;
esac
