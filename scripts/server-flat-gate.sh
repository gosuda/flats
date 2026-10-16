#!/usr/bin/env bash
# server-flat-gate.sh: Phase 2 gate with the real binary.
#  1. a JS flat using env.DB deploys, keeps data across versions, rolls back
#  2. the sandbox cannot reach host files, credentials (parent env, secret
#     key) or the management API
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/flats-server-gate.XXXXXX")
PORT=${FLATS_GATE_PORT:-27978}
LPORT=$((PORT + 1))
export FLATS_URL="http://127.0.0.1:$PORT"
FAIL=0
# This gate owns only temp data on Local, with Portal disabled. Record exact
# simulated operator interventions; the client under test still only requests.
FLATS_GATE_DISPOSABLE_OPERATOR=1
# shellcheck source=scripts/approval-gate.sh
source "$ROOT/scripts/approval-gate.sh"
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAIL=1; }
# shellcheck disable=SC2317,SC2329 # Invoked indirectly by the EXIT trap.
cleanup() { [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

if [ -n "${FLATS_BIN:-}" ]; then cp "$FLATS_BIN" "$WORK/flats"; else CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/flats" ./cmd/flats || exit 1; fi
FLATS_PARENT_SECRET=parent-only-value "$WORK/flats" serve --data "$WORK/data" --listen "127.0.0.1:$PORT" --network local \
  --local-addr "127.0.0.1:$LPORT" --portal=false >"$WORK/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 50); do
  kill -0 "$PID" 2>/dev/null || { echo "gate host exited; see $WORK/serve.log" >&2; exit 1; }
  curl -fsS "$FLATS_URL/api/status" >/dev/null 2>&1 && break; sleep 0.2
done
if ! kill -0 "$PID" 2>/dev/null; then echo "gate host exited" >&2; exit 1; fi
if ! lsof -t -a -iTCP:"$PORT" -sTCP:LISTEN | /usr/bin/grep -qx "$PID"; then echo "gate does not own listener :$PORT" >&2; exit 1; fi
F="$WORK/flats"
URL="http://counter.localhost:$LPORT"

mkdir -p "$WORK/v1" "$WORK/v2"
echo '{"kind":"server"}' >"$WORK/v1/flats.json"
cat >"$WORK/v1/server.js" <<'EOF'
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    if (request.url.endsWith("/hit")) env.DB.exec("INSERT INTO hits VALUES (1)");
    const [{ c }] = env.DB.query("SELECT count(*) AS c FROM hits");
    return Response.json({ version: 1, hits: c, secret: env.API_KEY || null });
  }
}
EOF
cp "$WORK/v1/flats.json" "$WORK/v2/flats.json"
sed 's/version: 1/version: 2/' "$WORK/v1/server.js" >"$WORK/v2/server.js"

if gate_save_and_deploy counter "$WORK/v1" >/dev/null; then pass "v1 deployed"; else fail "v1 deploy"; fi
curl -s "$URL/hit" >/dev/null; r=$(curl -s "$URL/hit")
if grep -q '"hits":2' <<<"$r" && grep -q '"version":1' <<<"$r"; then pass "v1 counts in SQLite: $r"; else fail "v1 response: $r"; fi
if printf 's3cr3t-value' | "$F" secret set counter API_KEY >/dev/null; then pass "secret set (operator CLI)"; else fail "secret set"; fi
if gate_save_and_deploy counter "$WORK/v2" >/dev/null; then pass "v2 deployed"; else fail "v2 deploy"; fi
r=$(curl -s "$URL/hit")
if grep -q '"version":2' <<<"$r" && grep -q '"hits":3' <<<"$r" && grep -q '"secret":"s3cr3t-value"' <<<"$r"; then pass "v2 keeps data and sees its secret: $r"; else fail "v2 response: $r"; fi
if gate_request rollback counter >/dev/null; then pass "rollback"; else fail "rollback"; fi
r=$(curl -s "$URL/hit")
if grep -q '"version":1' <<<"$r" && grep -q '"hits":4' <<<"$r"; then pass "rollback restores code, keeps data: $r"; else fail "rollback response: $r"; fi
gate_request deploy --flat counter --version 2 >/dev/null
curl -s "$URL/hit" >/dev/null; curl -s "$URL/hit" >/dev/null
if gate_request rollback counter --restore-data >/dev/null && r=$(curl -s "$URL") && grep -q '"hits":4' <<<"$r"; then pass "rollback --restore-data puts back the pre-deploy DB: $r"; else fail "restore-data rollback: $r"; fi
# Read the entire successful CLI response before searching: grep -q can close
# a pipeline early, and pipefail would then mistake SIGPIPE for no secret.
# BEGIN secret log check
if logs=$("$F" logs counter); then
  if [[ $logs == *"s3cr3t-value"* ]]; then
    fail "secret value leaked into logs"
  else
    pass "secret value not in logs"
  fi
else
  fail "cannot read logs for secret check"
fi
# END secret log check

# Sandbox probes.
mkdir -p "$WORK/probe"
echo '{"kind":"server"}' >"$WORK/probe/flats.json"
cat >"$WORK/probe/server.js" <<EOF
export default {
  async fetch(request, env) {
    const out = {};
    const t = (name, f) => { try { out[name] = f(); } catch (e) { out[name] = "blocked: " + String(e).slice(0, 80); } };
    t("passwd", () => (typeof std !== "undefined" && std.loadFile) ? std.loadFile("/etc/passwd") : "no std");
    t("secretkey", () => (typeof std !== "undefined" && std.loadFile) ? std.loadFile("$WORK/data/secret.key") : "no std");
    t("parentEnv", () => (typeof std !== "undefined" && std.getenv) ? std.getenv("FLATS_PARENT_SECRET") : (env.FLATS_PARENT_SECRET || null));
    t("home", () => (typeof std !== "undefined" && std.getenv) ? std.getenv("HOME") : null);
    t("fetchApi", () => typeof fetch);
    t("dbAttach", () => env.DB.exec("ATTACH DATABASE '$WORK/data/flats.db' AS meta"));
    t("dbVacuumInto", () => env.DB.exec("VACUUM INTO '$WORK/stolen.db'"));
    t("filesTraversal", () => env.FILES.get("../../flats.db"));
    return Response.json(out);
  }
}
EOF
gate_save_and_deploy probe "$WORK/probe" >/dev/null || fail "probe deploy"
r=$(curl -s "http://probe.localhost:$LPORT")
echo "probe: $r"
python3 - "$r" "$WORK" <<'PY' || FAIL=1
import json, sys, os
r = json.loads(sys.argv[1]); work = sys.argv[2]
bad = []
if r.get("passwd") not in (None, "no std") and "blocked" not in str(r.get("passwd")) and "root:" in str(r.get("passwd")): bad.append("read /etc/passwd")
if r.get("secretkey") not in (None, "no std") and "blocked" not in str(r.get("secretkey")): bad.append("read secret.key")
if r.get("parentEnv") not in (None, "") and "blocked" not in str(r.get("parentEnv")): bad.append("saw parent env")
if r.get("home") not in (None, "") and "blocked" not in str(r.get("home")): bad.append("saw HOME")
if r.get("fetchApi") not in ("undefined",) : bad.append("has fetch")
if "blocked" not in str(r.get("dbAttach")): bad.append("ATTACH allowed")
if "blocked" not in str(r.get("dbVacuumInto")) or os.path.exists(os.path.join(work, "stolen.db")): bad.append("VACUUM INTO allowed")
if r.get("filesTraversal") not in (None,) and "blocked" not in str(r.get("filesTraversal")): bad.append("FILES traversal")
print("FAIL: sandbox: " + ", ".join(bad) if bad else "PASS: sandbox blocked host files, parent env, network, ATTACH/VACUUM INTO and FILES traversal")
sys.exit(1 if bad else 0)
PY
# The management API is not reachable from JS: there is no network API at all
# (fetchApi == undefined above), and the worker has no socket access.
echo
[ $FAIL = 0 ] && echo "== server flat gate: PASS ==" || echo "== server flat gate: FAIL (logs: $WORK) =="
exit $FAIL
