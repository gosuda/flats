#!/bin/sh
# Runs one shard of the CI test suite, so CI can run shards in parallel jobs.
#
#   scripts/ci-test.sh <shard> [go test flags...]
#
# The QuickJS runtime and docs app packages hold most of the suite's time in
# a few long soak tests. Each package is split in two: one shard runs the
# named soak tests with -run, the other runs everything else with -skip on
# the same pattern. A new or renamed test therefore always runs somewhere;
# at worst the shards become unbalanced. Running every shard covers exactly
# `go test ./...`.

set -eu

[ $# -ge 1 ] || {
	echo "usage: $0 runtime-soak|runtime|docs-soak|docs|other [go test flags...]" >&2
	exit 2
}
shard=$1
shift

module=github.com/gosuda/flats
runtime_soak='^(TestOutboundFetchLoopOwnershipCycleCollection|TestLargeHostAndCodecOwnership|TestNearCapCycleCollection)$'
docs_soak='^(TestConflictPrivacyAndRollbackStorageBudgets|TestPublicDocumentReadSoak|TestDocumentOwnershipSoak|TestMaximumRewriteActivationBudget|TestLargeDocumentEditAndPingBudgets)$'

case "$shard" in
runtime-soak) exec go test "$@" -run "$runtime_soak" ./internal/runtime ;;
runtime) exec go test "$@" -skip "$runtime_soak" ./internal/runtime ;;
docs-soak) exec go test "$@" -run "$docs_soak" ./internal/apps/docs ;;
docs) exec go test "$@" -skip "$docs_soak" ./internal/apps/docs ;;
other)
	# shellcheck disable=SC2046 # one argument per package path.
	exec go test "$@" $(go list ./... | grep -vx -e "$module/internal/runtime" -e "$module/internal/apps/docs")
	;;
*)
	echo "unknown shard: $shard" >&2
	exit 2
	;;
esac
