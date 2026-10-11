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
# somewhere; at worst the shards become unbalanced. Running every shard covers
# exactly `go test ./...`.

set -eu

[ $# -ge 1 ] || {
	echo "usage: $0 runtime-soak|runtime-codec|runtime|docs-soak|docs-budget|docs|other [go test flags...]" >&2
	exit 2
}
shard=$1
shift

module=github.com/gosuda/flats
runtime_soak='^(TestOutboundFetchLoopOwnershipCycleCollection|TestNearCapCycleCollection)$'
# A serial soak whose requests take seconds each: alone, it keeps its margin
# against the request timeout.
runtime_codec='^(TestLargeHostAndCodecOwnership)$'
docs_soak='^(TestConflictPrivacyAndRollbackStorageBudgets|TestPublicDocumentReadSoak|TestDocumentOwnershipSoak)$'
# The docs tests that stay serial because they check a wall-clock budget or a
# rate limit that refills over time.
docs_budget='^(TestMaximumRewriteActivationBudget|TestLargeDocumentEditAndPingBudgets|TestLargeDisjointActivation|TestFullRewriteActivation|TestAgentEditManyBlocksWithinDeadline|TestVisitorHealthIsCheapAndCannotBlockCommit|Test100KiBDocumentTimings|TestUnauthorizedUpdatesPreserveWriterBudget|TestConnectionInputLimitPreservesOtherWriter|TestAuthorizedUpdatesConsumeWriterBudget)$'

case "$shard" in
runtime-soak) exec go test "$@" -run "$runtime_soak" ./internal/runtime ;;
runtime-codec) exec go test "$@" -run "$runtime_codec" ./internal/runtime ;;
runtime) exec go test "$@" -skip "$runtime_soak|$runtime_codec" ./internal/runtime ;;
docs-soak) exec go test "$@" -run "$docs_soak" ./internal/apps/docs ;;
docs-budget) exec go test "$@" -run "$docs_budget" ./internal/apps/docs ;;
docs) exec go test "$@" -skip "$docs_soak|$docs_budget" ./internal/apps/docs ;;
other)
	# shellcheck disable=SC2046 # one argument per package path.
	exec go test "$@" $(go list ./... | grep -vx -e "$module/internal/runtime" -e "$module/internal/apps/docs")
	;;
*)
	echo "unknown shard: $shard" >&2
	exit 2
	;;
esac
