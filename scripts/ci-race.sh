#!/bin/sh
# Runs one lane of the CI race tests, so CI can run the lanes in parallel
# jobs.
#
#   scripts/ci-race.sh <lane> [go test flags...]
#
# Together the lanes run every package and test that CI checks with -race:
# internal/core in its own lane; the API, app, MCP, store, Portal and
# WebSocket packages, the selected runtime tests and the board engine in
# another; internal/notebook in its own lane; internal/board in two lanes
# split by test name. board-1 runs the tests named below, board-2 every other
# test of the package (-skip on the same pattern), so a new or renamed board
# test always runs. internal/board/benchdata only generates fixed reference
# data and is not run with -race.
#
# The board engine's code starts no goroutines and keeps no mutable package
# state. Its seeded simulations already run with their full seed counts,
# without -race, in the board shard of scripts/ci-test.sh. Here they run with
# a few seeds per CPU, which still runs seeds concurrently under the race
# detector.
#
# The notebook lane runs the whole package with its default model seed count;
# -notebook-model-seeds=200 is a separate stress run. No run filter: the
# writer and measurement goroutines are reached from most tests, and the
# slowest tests copy and undo large page trees whatever the seed count.
# The package takes about nine minutes under -race on a CI runner, close to
# go test's default 10-minute timeout, so the lane sets 18 minutes inside
# its 20-minute job. A caller's own -timeout comes later and wins.
# The three notebook-large lanes use the same memory fixtures as ci-memory.sh,
# with race detection. They run weekly or manually, outside routine PR CI.
# Each keeps a 25-minute test timeout within its 30-minute job.
#
# Every command of a lane runs even after an earlier one fails, so the log
# shows all failures; the lane then fails.

set -u

[ $# -ge 1 ] || {
	echo "usage: $0 core|services|notebook|notebook-large-reader|notebook-large-markdown|notebook-large-source|board-1|board-2 [go test flags...]" >&2
	exit 2
}
lane=$1
shift

runtime_concurrency='TestConcurrentFiles|TestWASIRejectsJSHostABI|TestWebSocket(AdmissionCeiling|RejectedUpgrade|PostFailureReleasesSlot|DefaultByteLimits|MessageLimit)$'
engine_seeds=16
# The longest board tests under -race: about half of the package's time.
board_1='^(TestTwentyGuestsHourOfRevisions|TestLargeFrameMoveDeltaArrivesInParts|TestGuestGroupWindows|TestGuestIdentityEveryFrame|TestStalledGuestsStayInTheirShare)$'

failed=0
run() {
	"$@" || failed=1
}

case "$lane" in
core)
	run go test -race "$@" ./internal/core
	;;
services)
	run go test -race "$@" ./internal/api ./internal/app ./internal/mcpx ./internal/store ./internal/expose/portal ./internal/wsconn
	run go test -race "$@" -run "$runtime_concurrency" ./internal/runtime
	run env BOARD_SEEDS=$engine_seeds go test -race "$@" ./internal/board/engine ./internal/board/model ./internal/board/schema
	;;
notebook)
	run go test -race -timeout=18m "$@" ./internal/notebook/...
	;;
notebook-large-reader)
	run "$(dirname "$0")/ci-memory.sh" reader -race "$@"
	;;
notebook-large-markdown)
	run "$(dirname "$0")/ci-memory.sh" markdown -race "$@"
	;;
notebook-large-source)
	run "$(dirname "$0")/ci-memory.sh" source -race "$@"
	;;
board-1)
	run go test -race "$@" -run "$board_1" ./internal/board
	;;
board-2)
	run go test -race "$@" -skip "$board_1" ./internal/board
	;;
*)
	echo "unknown lane: $lane" >&2
	exit 2
	;;
esac
exit "$failed"
