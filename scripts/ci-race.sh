#!/bin/sh
# Runs one lane of the CI race tests, so CI can run the lanes in parallel
# jobs.
#
#   scripts/ci-race.sh <lane> [go test flags...]
#
# Together the lanes run every package and test that CI checks with -race:
# internal/core in two lanes; the API, app, backup, MCP, store, Portal and
# WebSocket packages, the selected runtime tests and the board engine in
# another; internal/notebook in two lanes; internal/board in two lanes.
# Each two-lane package is split by test name: the first lane runs the tests
# matched below, the second every other test of the package (-skip on the
# same pattern), so a new or renamed test always runs. internal/board/benchdata only generates fixed reference
# data and is not run with -race.
#
# The board engine's code starts no goroutines and keeps no mutable package
# state. Its seeded simulations already run with their full seed counts,
# without -race, in the board shard of scripts/ci-test.sh. Here they run with
# a few seeds per CPU, which still runs seeds concurrently under the race
# detector.
#
# The notebook lanes run the whole package between them with its default
# model seed count; -notebook-model-seeds=200 is a separate stress run.
# notebook-1 takes the Markdown and reader tests, about half of the package's time;
# notebook-2 everything else. The package took about
# nine minutes under -race in one lane, so both lanes keep the 18-minute
# test timeout inside their 20-minute jobs. A caller's own -timeout comes
# later and wins. core-1 and core-2 split internal/core the same way.
# The three notebook-large lanes use the same memory fixtures as ci-memory.sh,
# with race detection. They run weekly or manually, outside routine PR CI.
# Each keeps a 25-minute test timeout within its 30-minute job.
#
# Every command of a lane runs even after an earlier one fails, so the log
# shows all failures; the lane then fails.

set -u

[ $# -ge 1 ] || {
	echo "usage: $0 core-1|core-2|services|notebook-1|notebook-2|notebook-large-reader|notebook-large-markdown|notebook-large-source|board-1|board-2 [go test flags...]" >&2
	exit 2
}
lane=$1
shift

runtime_concurrency='TestConcurrentFiles|TestWASIRejectsJSHostABI|TestWebSocket(AdmissionCeiling|RejectedUpgrade|PostFailureReleasesSlot|DefaultByteLimits|MessageLimit)$'
engine_seeds=16
# About half of each package's race time, by test name prefix.
notebook_1='^Test(Markdown|Reader)'
core_1='^Test(Docs|Public|Notebook|Board|Usage)'
# The longest board tests under -race: about half of the package's time.
board_1='^(TestTwentyGuestsHourOfRevisions|TestLargeFrameMoveDeltaArrivesInParts|TestGuestGroupWindows|TestGuestIdentityEveryFrame|TestStalledGuestsStayInTheirShare)$'

failed=0
run() {
	"$@" || failed=1
}

case "$lane" in
core-1)
	run go test -race "$@" -run "$core_1" ./internal/core
	;;
core-2)
	run go test -race "$@" -skip "$core_1" ./internal/core
	;;
services)
	run go test -race "$@" ./internal/api ./internal/app ./internal/backup ./internal/mcpx ./internal/store ./internal/expose/portal ./internal/wsconn
	run go test -race "$@" -run "$runtime_concurrency" ./internal/runtime
	run env BOARD_SEEDS=$engine_seeds go test -race "$@" ./internal/board/engine ./internal/board/model ./internal/board/schema
	;;
notebook-1)
	run go test -race -timeout=18m "$@" -run "$notebook_1" ./internal/notebook/...
	;;
notebook-2)
	run go test -race -timeout=18m "$@" -skip "$notebook_1" ./internal/notebook/...
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
