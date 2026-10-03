#!/usr/bin/env bash
# Run from any directory. Never uses the operator's data or provider identity.
set -euo pipefail
# Gate backends are disposable loopback doubles; discard live-provider inputs.
unset FLATS_PORTAL_E2E TS_AUTHKEY TS_AUTH_KEY FLATS_URL
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/flats-lifecycle-gate.XXXXXX")
if [[ -n ${FLATS_BIN:-} ]]; then
  cp "$FLATS_BIN" "$WORK/flats"
else
  CGO_ENABLED=0 go build -C "$ROOT" -buildvcs=true -o "$WORK/flats" ./cmd/flats
fi
# Historical code must always come from the pinned committed tree. An external
# legacy executable has no verified relationship to that source and cannot prove
# migration acceptance merely by having its bytes hashed.
if [[ -n ${FLATS_LIFECYCLE_LEGACY_BIN:-} ]]; then
  echo "legacy binary overrides are unverified; use the pinned archive build" >&2
  exit 2
fi
mkdir "$WORK/legacy-source"
git -C "$ROOT" archive 29edc2a6e13e27867603a23ff5b1b5a8d1b84df0 | tar -x -C "$WORK/legacy-source"
CGO_ENABLED=0 go build -C "$WORK/legacy-source" -o "$WORK/legacy-flats" ./cmd/flats
# The historical provider fixture links only archived core/API/runtime.
mkdir -p "$WORK/legacy-source/internal/lifecyclecheck/legacyadapter"
cp "$ROOT/internal/lifecyclecheck/legacyadapter/main.go" "$WORK/legacy-source/internal/lifecyclecheck/legacyadapter/main.go"
CGO_ENABLED=0 go build -C "$WORK/legacy-source" -o "$WORK/legacy-provider-adapter" ./internal/lifecyclecheck/legacyadapter
CGO_ENABLED=0 go build -C "$ROOT" -buildvcs=true -o "$WORK/provider-adapter" ./internal/lifecyclecheck/adapter
CGO_ENABLED=0 go build -C "$ROOT" -buildvcs=true -tags=lifecycle_testhooks -o "$WORK/crash-adapter" ./internal/lifecyclecheck/adapter
exec python3 -B "$ROOT/internal/lifecyclecheck/gate.py" --binary "$WORK/flats" --work "$WORK" \
  --legacy-binary "$WORK/legacy-flats" --adapter-binary "$WORK/provider-adapter" \
  --legacy-adapter-binary "$WORK/legacy-provider-adapter" --crash-binary "$WORK/crash-adapter" "$@"
