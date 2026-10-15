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
F="$WORK/flats"
DELETED_THIRD=0
PORT=${FLATS_GATE_PORT:-27878}
export FLATS_URL="http://127.0.0.1:$PORT"
SUFFIX=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c4)
FAIL=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAIL=1; }
api() { curl -fsS --max-time 30 "$@"; }
# Real-tailnet work always requires the operator's exact explicit decision.
# Do not inherit disposable-host auto-approval from the caller's environment.
FLATS_GATE_DISPOSABLE_OPERATOR=0
# shellcheck source=scripts/approval-gate.sh
source "$ROOT/scripts/approval-gate.sh"
# host_state prints "<state> <detail>" of a private host.
host_state() {
  api "$FLATS_URL/api/status" | python3 -c '
import json,sys
for h in json.load(sys.stdin)["system"]["private"].get("hosts") or []:
    if h["host"] == sys.argv[1]: print(h["state"], h.get("detail",""))' "$1"
}
# wait_ready waits up to $2 seconds for a host to report ready.
wait_ready() {
  local t0
  t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt "$2" ]; do
    case "$(host_state "$1")" in ready*|key-expiring*) return 0;; esac
    sleep 3
  done
  return 1
}
# gone reports whether a node has left the tailnet: absent from the peer
# list, or logged out (key expired) and offline, which control removes soon.
gone() {
  tailscale status --json 2>/dev/null | python3 -c '
import json,sys
for p in (json.load(sys.stdin).get("Peer") or {}).values():
    if p.get("HostName") == sys.argv[1] and (p.get("Online") or not p.get("Expired")):
        sys.exit(1)' "$1"
}
# shellcheck disable=SC2317,SC2329 # Invoked indirectly by the EXIT trap.
cleanup() {
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
    for i in 1 2 3; do
      [ "$i" = 3 ] && [ "$DELETED_THIRD" = 1 ] && continue
      FLATS_GATE_APPROVAL_TIMEOUT=30 gate_request delete "tg$SUFFIX-$i" >/dev/null || echo "Cleanup requires the operator's decision for tg$SUFFIX-$i" >&2
    done
    kill "$PID" 2>/dev/null
  fi
  wait 2>/dev/null
  if [ -n "${ACME_CACHE:-}" ] && [ ! -f "$ACME_CACHE" ] && [ -f "$WORK/data/tsnet/acme-account.key.pem" ]; then
    mkdir -p "$(dirname "$ACME_CACHE")" && install -m 600 "$WORK/data/tsnet/acme-account.key.pem" "$ACME_CACHE"
  fi
  # The console node is kept across restarts; log it out so the run leaves
  # no device behind.
  [ -d "$WORK/data/tsnet/flats-gate-$SUFFIX" ] &&
    go run -C "$ROOT" ./scripts/tsnet-logout "$WORK/data/tsnet/flats-gate-$SUFFIX" >/dev/null 2>&1
}
trap cleanup EXIT
command -v tailscale >/dev/null || { echo "tailscale CLI not found"; exit 2; }
TAILNET=$(tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["MagicDNSSuffix"])')
echo "tailnet: $TAILNET"

# Reuse one Let's Encrypt account across gate runs: each fresh data directory
# would otherwise register a new account, and Let's Encrypt allows only 10
# per IP address in 3 hours.
ACME_CACHE=${FLATS_GATE_ACME_KEY:-${XDG_CACHE_HOME:-$HOME/.cache}/flats-gate/acme-account.key.pem}
mkdir -p "$WORK/data/tsnet"
[ -f "$ACME_CACHE" ] && install -m 600 "$ACME_CACHE" "$WORK/data/tsnet/acme-account.key.pem"

if [ -n "${FLATS_BIN:-}" ]; then cp "$FLATS_BIN" "$WORK/flats"; else CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/flats" ./cmd/flats || exit 1; fi
"$WORK/flats" serve --data "$WORK/data" --listen "127.0.0.1:$PORT" --network tailscale --authkey-file "$KEY" \
  --console-host "flats-gate-$SUFFIX" --portal=false >"$WORK/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do
  kill -0 "$PID" 2>/dev/null || { echo "flats serve exited:"; tail -5 "$WORK/serve.log"; exit 1; }
  curl -fsS --max-time 5 "$FLATS_URL/api/status" >/dev/null 2>&1 && break; sleep 0.3
done
# Refuse to test against some other server already listening on the port.
kind=$(curl -fsS --max-time 10 "$FLATS_URL/api/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["system"]["private"]["kind"])' 2>/dev/null)
if ! kill -0 "$PID" 2>/dev/null || [ "$kind" != tailscale ]; then
  echo "server on :$PORT is not this gate's tailscale server (kind=$kind)"
  tail -5 "$WORK/serve.log"
  exit 1
fi
F="$WORK/flats"
lsof -t -a -iTCP:"$PORT" -sTCP:LISTEN | /usr/bin/grep -qx "$PID" || { echo "gate does not own listener :$PORT" >&2; exit 1; }
echo "Before approving deployment, enable Tailscale for each gate flat in this disposable host's console if not already permitted." >&2

start=$(date +%s)
for i in 1 2 3; do
  mkdir -p "$WORK/s$i"
  echo "<h1>tailnet gate $i</h1>" >"$WORK/s$i/index.html"
  gate_save_and_deploy "tg$SUFFIX-$i" "$WORK/s$i" >/dev/null || fail "deploy $i"
done
st=$(host_state "tg$SUFFIX-1")
[[ "$st" == starting* ]] && pass "a new flat reports starting until it has its certificate ($st)" || echo "note: flat 1 state right after deploy: $st"
for i in 1 2 3; do
  url="https://tg$SUFFIX-$i.$TAILNET"
  if wait_ready "tg$SUFFIX-$i" 600; then
    echo "flat $i ready after $(( $(date +%s) - start ))s"
    body=$(curl -fsS --max-time 20 "$url" 2>/dev/null)
    if grep -q "tailnet gate $i" <<<"$body"; then pass "flat $i served over HTTPS at $url once ready"; else fail "flat $i reported ready but $url did not answer"; fi
  else
    fail "flat $i not ready after 600s: $(host_state "tg$SUFFIX-$i")"
  fi
done
echo "time to 3 HTTPS flats: $(( $(date +%s) - start ))s"
echo "RSS with 3 flat nodes + console node: $(ps -o rss= -p $PID) KB"

# The TLS certificate is a real public one.
issuer=$(echo | timeout 30 openssl s_client -connect "tg$SUFFIX-1.$TAILNET:443" -servername "tg$SUFFIX-1.$TAILNET" 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null)
if [ -n "$issuer" ]; then pass "certificate issuer: $issuer"; else fail "no TLS certificate"; fi

# Preview node: ephemeral, removed on deploy.
mkdir -p "$WORK/s1b"; echo "<h1>preview candidate</h1>" >"$WORK/s1b/index.html"
saved_preview=$("$F" --json save-draft "tg$SUFFIX-1" "$WORK/s1b") || fail "save preview Draft"
purl=$("$F" preview "tg$SUFFIX-1" --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["url"])')
phost=$(sed -E 's#https?://([^.]+)\..*#\1#' <<<"$purl")
if [[ "$phost" =~ ^tg$SUFFIX-1-[a-z0-9]{8}$ ]]; then pass "preview host $phost"; else fail "preview host $phost"; fi
pstart=$(date +%s)
if wait_ready "$phost" 600; then
  echo "preview ready after $(( $(date +%s) - pstart ))s"
  if curl -fsS --max-time 20 "$purl" | grep -q candidate; then pass "preview served at $purl"; else fail "preview reported ready but did not answer"; fi
else
  fail "preview not ready after 600s: $(host_state "$phost")"
fi
dstart=$(date +%s)
preview_version=$(gate_create_saved "tg$SUFFIX-1" "$saved_preview") || fail "create preview version"
gate_request deploy --flat "tg$SUFFIX-1" --version "$preview_version" >/dev/null || fail "deploy closing the preview"
echo "deploy closing the preview took $(( $(date +%s) - dstart ))s"
for _ in $(seq 1 20); do gone "$phost" && break; sleep 3; done
if gone "$phost"; then pass "preview node logged out of the tailnet after deploy"; else fail "preview node $phost still online after deploy"; fi

# Delete logs the node out.
if gate_request delete "tg$SUFFIX-3" >/dev/null; then DELETED_THIRD=1; else fail "operator-approved delete"; fi
for _ in $(seq 1 20); do gone "tg$SUFFIX-3" && break; sleep 3; done
if gone "tg$SUFFIX-3"; then pass "deleted flat's node logged out of the tailnet"; else fail "deleted flat's node still online"; fi

# Management API is not on flat hosts.
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://tg$SUFFIX-2.$TAILNET/api/flats")
if [ "$code" != 200 ]; then pass "management API not served on a flat host ($code)"; else fail "management API reachable on a flat host"; fi
if wait_ready "flats-gate-$SUFFIX" 600 &&
  curl -fsS --max-time 20 "https://flats-gate-$SUFFIX.$TAILNET/api/status" >/dev/null; then
  pass "console node answers over HTTPS"
else
  fail "console node not reachable"
fi

# Identity headers from a real peer: flat 2 becomes a server flat that echoes
# them (after the management API check, which it would otherwise answer).
mkdir -p "$WORK/s2b"
cat >"$WORK/s2b/server.js" <<'JS'
export default {
  async fetch(request) {
    const h = request.headers;
    const get = (n) => h[n] ?? h[n.toLowerCase()] ?? "";
    return Response.json({ login: get("Tailscale-User-Login"), name: get("Tailscale-User-Name"),
      pic: get("Tailscale-User-Profile-Pic") });
  }
}
JS
echo '{"kind": "server"}' >"$WORK/s2b/flats.json"
gate_save_and_deploy "tg$SUFFIX-2" "$WORK/s2b" >/dev/null || fail "deploy the header echo flat"
me=$(tailscale status --json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["User"][str(d["Self"]["UserID"])]["LoginName"])')
got=$(curl -fsS --max-time 20 -H "Tailscale-User-Login: attacker@example.com" -H "Tailscale-User-Profile-Pic: spoofed" "https://tg$SUFFIX-2.$TAILNET/")
if python3 - "$me" "$got" <<'PY'
import json, sys
me, got = sys.argv[1], json.loads(sys.argv[2])
sys.exit(0 if got["login"] == me and got["name"] and got["pic"] in ("", None) else 1)
PY
then
  pass "identity headers set from the real peer's login; spoofed headers replaced"
else
  fail "identity headers wrong"
fi

echo
[ $FAIL = 0 ] && echo "== tailnet gate: PASS ==" || echo "== tailnet gate: FAIL (logs: $WORK) =="
exit $FAIL
