#!/usr/bin/env bash
# Run from any directory. Never uses the operator's data or provider identity.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/flats-lifecycle-gate.XXXXXX")
if [[ -n ${FLATS_BIN:-} ]]; then
  cp "$FLATS_BIN" "$WORK/flats"
else
  CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/flats" ./cmd/flats
fi
if [[ -n ${FLATS_LIFECYCLE_LEGACY_BIN:-} ]]; then
  cp "$FLATS_LIFECYCLE_LEGACY_BIN" "$WORK/legacy-flats"
else
  # Known pre-lifecycle base. Building an archived tree leaves this worktree
  # untouched and seeds migrations through the real historical HTTP server.
  mkdir "$WORK/legacy-source"
  git -C "$ROOT" archive 29edc2a6e13e27867603a23ff5b1b5a8d1b84df0 | tar -x -C "$WORK/legacy-source"
  CGO_ENABLED=0 go build -C "$WORK/legacy-source" -o "$WORK/legacy-flats" ./cmd/flats
fi
CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/provider-adapter" ./internal/lifecyclecheck/adapter
exec python3 "$ROOT/internal/lifecyclecheck/gate.py" --binary "$WORK/flats" --work "$WORK" \
  --legacy-binary "$WORK/legacy-flats" --adapter-binary "$WORK/provider-adapter" "$@"
