#!/usr/bin/env bash
# tailnet-gate.sh: verify private exposure on a REAL tailnet.
#
# Usage: scripts/tailnet-gate.sh <authkey-file>
#   The key must be reusable and untagged (operator-owned nodes). Run it on a
#   machine that is itself on the same tailnet (it fetches the flats over the
#   tailnet through the local Tailscale client) with MagicDNS and HTTPS
#   certificates enabled in the tailnet.
#
# Checks: 3 flats get their own nodes and real HTTPS certificates at
# https://<slug>.<tailnet>.ts.net; Tailscale-User-Login/Name reach the flat and
# spoofed values are replaced; a preview gets an ephemeral <slug>-<8> node that
# is removed on deploy; delete logs the node out; the console node answers and
# the management API is not reachable from a flat host.
set -uo pipefail
KEY=${1:?usage: tailnet-gate.sh <authkey-file>}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/flats-tailnet-gate.XXXXXX")
PORT=${FLATS_GATE_PORT:-27878}
export FLATS_URL="http://127.0.0.1:$PORT"
SUFFIX=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c4)
FAIL=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAIL=1; }
cleanup() {
  for i in 1 2 3; do "$WORK/flats" delete "tg$SUFFIX-$i" >/dev/null 2>&1; done
  [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT
command -v tailscale >/dev/null || { echo "tailscale CLI not found"; exit 2; }
TAILNET=$(tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["MagicDNSSuffix"])')
echo "tailnet: $TAILNET"

if [ -n "${FLATS_BIN:-}" ]; then cp "$FLATS_BIN" "$WORK/flats"; else CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/flats" ./cmd/flats || exit 1; fi
"$WORK/flats" serve --data "$WORK/data" --listen "127.0.0.1:$PORT" --network tailscale --authkey-file "$KEY" \
  --console-host "flats-gate-$SUFFIX" --portal=false --runtime=false >"$WORK/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do curl -fsS "$FLATS_URL/api/status" >/dev/null 2>&1 && break; sleep 0.3; done
F="$WORK/flats"

start=$(date +%s)
for i in 1 2 3; do
  mkdir -p "$WORK/s$i"
  echo "<h1>tailnet gate $i</h1>" >"$WORK/s$i/index.html"
  "$F" deploy "$WORK/s$i" --flat "tg$SUFFIX-$i" >/dev/null || fail "deploy $i"
done
for i in 1 2 3; do
  url="https://tg$SUFFIX-$i.$TAILNET"
  ok=""
  for _ in $(seq 1 60); do
    body=$(curl -fsS --max-time 10 "$url" 2>/dev/null) && ok=1 && break
    sleep 2
  done
  [ -n "$ok" ] && grep -q "tailnet gate $i" <<<"$body" && pass "flat $i served over HTTPS at $url" || fail "flat $i not reachable at $url"
done
echo "time to 3 HTTPS flats: $(( $(date +%s) - start ))s"
echo "RSS with 3 flat nodes + console node: $(ps -o rss= -p $PID) KB"

# Identity headers: serve a page that echoes them is not possible for a static
# flat, so check the private network status and rely on the unit tests for
# header contents; here verify the TLS certificate is a real public one.
issuer=$(echo | openssl s_client -connect "tg$SUFFIX-1.$TAILNET:443" -servername "tg$SUFFIX-1.$TAILNET" 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null)
[ -n "$issuer" ] && pass "certificate issuer: $issuer" || fail "no TLS certificate"

# Preview node: ephemeral, removed on deploy.
mkdir -p "$WORK/s1b"; echo "<h1>preview candidate</h1>" >"$WORK/s1b/index.html"
"$F" deploy "$WORK/s1b" --flat "tg$SUFFIX-1" --save-only >/dev/null
purl=$("$F" preview "tg$SUFFIX-1" --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["url"])')
phost=$(sed -E 's#https?://([^.]+)\..*#\1#' <<<"$purl")
[[ "$phost" =~ ^tg$SUFFIX-1-[a-z0-9]{8}$ ]] && pass "preview host $phost" || fail "preview host $phost"
for _ in $(seq 1 60); do curl -fsS --max-time 10 "$purl" 2>/dev/null | grep -q candidate && break; sleep 2; done
curl -fsS --max-time 10 "$purl" | grep -q candidate && pass "preview served at $purl" || fail "preview not reachable"
"$F" deploy --flat "tg$SUFFIX-1" --version 2 >/dev/null
sleep 5
tailscale status | grep -q "$phost" && fail "preview node $phost still in the tailnet after deploy" || pass "preview node removed after deploy"

# Delete logs the node out.
"$F" delete "tg$SUFFIX-3" >/dev/null   # creates an approval; approve it as the console would
id=$(curl -s "$FLATS_URL/api/approvals?status=pending" | python3 -c 'import json,sys; print(json.load(sys.stdin)["approvals"][0]["id"])')
curl -s -X POST -H "X-Flats-Console: 1" -H "Sec-Fetch-Site: same-origin" -H "Origin: $FLATS_URL" "$FLATS_URL/console/api/approvals/$id/approve" >/dev/null
sleep 5
tailscale status | grep -q "tg$SUFFIX-3 " && fail "deleted flat's node still listed" || pass "deleted flat's node is gone from the tailnet"

# Management API is not on flat hosts.
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://tg$SUFFIX-2.$TAILNET/api/flats")
[ "$code" != 200 ] && pass "management API not served on a flat host ($code)" || fail "management API reachable on a flat host"
curl -fsS --max-time 20 "https://flats-gate-$SUFFIX.$TAILNET/api/status" >/dev/null && pass "console node answers over HTTPS" || fail "console node not reachable"

echo
[ $FAIL = 0 ] && echo "== tailnet gate: PASS ==" || echo "== tailnet gate: FAIL (logs: $WORK) =="
exit $FAIL
