#!/bin/sh
# Large notebook memory fixtures, without race instrumentation by default.
# ci-race.sh supplies -race for the weekly and manual checks.
set -u

[ $# -ge 1 ] || {
	echo "usage: $0 reader|markdown|source [go test flags...]" >&2
	exit 2
}
group=$1
shift

case "$group" in
reader)
	pattern='^(TestReaderRetainedMemoryAndContinuations|TestReaderSelectionPreparationMemory|TestReaderCollectionQueryMemory|TestReaderHistoricalSourceMemory)$'
	package=./internal/notebook
	;;
markdown)
	pattern='^TestMarkdownRetainedMemory$'
	package=./internal/notebook
	;;
source)
	pattern='^TestNotebookSourceHistoryMemory$'
	package=./internal/core
	;;
*)
	echo "unknown memory group: $group" >&2
	exit 2
	;;
esac

exec env NOTEBOOK_LARGE_MEMORY_TESTS=1 go test "$@" -count=1 -timeout=25m -run "$pattern" "$package"
