#!/bin/sh
# Run from the workflow checkout with COMMIT_BATCH (JSON array), IMAGE and
# RUNNER_TEMP set. Build tooling/container files come from that checkout;
# each tested and packaged source checkout is the selected commit.
set -eu

root=$(pwd)
work=$(mktemp -d "$RUNNER_TEMP/flats-images.XXXXXX")
trap 'rm -rf "$work"' EXIT
printf '%s' "$COMMIT_BATCH" | python3 -c '
import json, re, sys
values = json.load(sys.stdin)
if not isinstance(values, list) or not values or not all(isinstance(v, str) and re.fullmatch(r"[0-9a-f]{40}", v) for v in values):
    sys.exit("invalid commit batch")
print("\n".join(values))
' >"$work/commits"

failed=0
while IFS= read -r commit; do
	# A failed historical commit must not stop later commits from being built.
	# A separate shell preserves errexit inside the single-commit build.
	# Child tests/builds must not consume the remaining commit-list stdin.
	if "$root/scripts/build-commit-image.sh" "$commit" </dev/null; then
		echo "Published $commit"
	else
		echo "::error::Image build or publication failed for $commit" >&2
		failed=1
	fi
done <"$work/commits"
exit "$failed"
