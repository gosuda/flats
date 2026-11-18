#!/bin/sh
# Runs one lane of the CI race tests, so CI can run the lanes in parallel
# jobs.
#
#   scripts/ci-race.sh <lane> [go test flags...]
#
# Together the lanes run every package and test that CI checks with -race:
# internal/core in two lanes; the API, app, backup, MCP, store, webhook, Portal
# and WebSocket packages, the selected runtime tests and the board engine in
# another; internal/notebook in two lanes; internal/board in two lanes;
# internal/sheet and the packages below it in five lanes.
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
# The five sheet lanes run the sheet engine, its formula package and its
# browser client tests. sheet-1 and sheet-2 split the engine's undo tests:
# sheet-1 the seven slowest by name, sheet-2 the other undo tests. sheet-3
# runs the engine's tests named below, with the snapshot, paged-capture and
# rowless-receipt tests; sheet-4 the load, revision, rollback, recovery,
# sort, merge, capture, ordering and client tests; and sheet-5 everything
# else of the engine and the packages below it. On a developer machine at a load average of 15 to 33 (November
# 2026) they took 6.0, 6.2, 7.2, 6.6 and 7.2 minutes, under half of the
# timeout; hosted runners are not measured. The engine's full-size cases do
# not run under the race detector; they run in the sheet shards of
# scripts/ci-test.sh. Each lane keeps a 15-minute test timeout within its
# 20-minute job in place of go test's 10 minutes.
#
# Every command of a lane runs even after an earlier one fails, so the log
# shows all failures; the lane then fails.

set -u

[ $# -ge 1 ] || {
	echo "usage: $0 core-1|core-2|services|notebook-1|notebook-2|notebook-large-reader|notebook-large-markdown|notebook-large-source|board-1|board-2|sheet-1|sheet-2|sheet-3|sheet-4|sheet-5 [go test flags...]" >&2
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
# The slowest undo tests under -race: about half of the undo tests' time.
sheet_1='^TestUndo(StoppedKeepsTheCollectedStamps|NotesStayInBudget|StopsWhileItReadsItsPayload|CountsWhatAShrunkTabKeeps|StopsAtTheDeadline|FollowsAlikeInMemoryAndFromScratch|EndsAtTheDeadlineInEveryPart)$'
sheet_undo='^TestUndo'
sheet_3='^(TestActionsCountTheirOwnWorkAndMeetTheDeadline|TestGuardsCountTheirWorkAndMeetTheDeadline|TestFrameTableAgainstEngine|TestCollectedStamp.*|TestSnapshot.*|TestPaged.*|TestRowless.*)$'
sheet_4='^Test(Load|Revision|Rollback|Recovery|Sort|Merge|Capture|Ordering|Client)'

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
	run go test -race "$@" ./internal/api ./internal/app ./internal/backup ./internal/mcpx ./internal/store ./internal/webhook ./internal/expose/portal ./internal/wsconn
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
sheet-1)
	run go test -race -timeout=15m "$@" -run "$sheet_1" ./internal/sheet
	;;
sheet-2)
	run go test -race -timeout=15m "$@" -run "$sheet_undo" -skip "$sheet_1" ./internal/sheet
	;;
sheet-3)
	run go test -race -timeout=15m "$@" -run "$sheet_3" ./internal/sheet
	;;
sheet-4)
	run go test -race -timeout=15m "$@" -run "$sheet_4" ./internal/sheet
	;;
sheet-5)
	run go test -race -timeout=15m "$@" -skip "$sheet_undo|$sheet_3|$sheet_4" ./internal/sheet/...
	;;
*)
	echo "unknown lane: $lane" >&2
	exit 2
	;;
esac
exit "$failed"
