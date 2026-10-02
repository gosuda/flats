#!/usr/bin/env bash
# agent-gate.sh: Phase 1 gate "Claude Code, Codex and Cursor each deploy after
# one setup". Starts an isolated Flats host (local network, temp data dir),
# gives each agent ONE MCP setting pointing at it, asks it to deploy a small
# site, and checks through the API that the flat is live.
#
# Usage: scripts/agent-gate.sh [claude|codex|cursor ...]   (default: all three)
# It uses your installed agent CLIs and accounts. Nothing global is modified:
# each agent gets its MCP setting through a per-run flag or project file.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/flats-agent-gate.XXXXXX")
PORT=${FLATS_GATE_PORT:-17878}
LPORT=$((PORT + 1))
BASE="http://127.0.0.1:$PORT"
AGENTS=("$@")
[ ${#AGENTS[@]} -eq 0 ] && AGENTS=(claude codex cursor)

cleanup() { [ -n "${SERVE_PID:-}" ] && kill "$SERVE_PID" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

echo "building flats..."
if [ -n "${FLATS_BIN:-}" ]; then cp "$FLATS_BIN" "$WORK/flats"; else CGO_ENABLED=0 go build -C "$ROOT" -o "$WORK/flats" ./cmd/flats || exit 1; fi
"$WORK/flats" serve --data "$WORK/data" --listen "127.0.0.1:$PORT" --network local \
  --local-addr "127.0.0.1:$LPORT" --portal=false --runtime=false >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 50); do curl -fsS "$BASE/api/status" >/dev/null 2>&1 && break; sleep 0.2; done

mk_site() { # $1 dir $2 label
  mkdir -p "$1"
  printf '<!doctype html><title>%s</title><h1>Deployed by %s</h1>\n' "$2" "$2" >"$1/index.html"
}

prompt() { # $1 slug $2 site dir
  cat <<EOF
Use the "flats" MCP server (its tools: save_version, deploy, get_flat, ...) to deploy the static site in the directory $2 as a flat with slug "$1".
Read the files in that directory and call save_version with them (encoding utf8) and deploy=true.
Then call get_flat for "$1" and reply with the private_url and the live version number. Do not change visibility.
EOF
}

declare -A RESULT
for a in "${AGENTS[@]}"; do
  slug="gate-$a"
  dir="$WORK/proj-$a"
  mk_site "$dir/site" "$a"
  start=$(date +%s)
  case "$a" in
  claude)
    printf '{"mcpServers":{"flats":{"type":"http","url":"%s/mcp"}}}' "$BASE" >"$dir/mcp.json"
    (cd "$dir" && timeout 600 claude -p "$(prompt "$slug" "$dir/site")" --mcp-config "$dir/mcp.json" --strict-mcp-config \
      --allowedTools "mcp__flats__save_version,mcp__flats__deploy,mcp__flats__get_flat,mcp__flats__list_flats,Read,Glob,LS" \
      </dev/null >"$WORK/$a.out" 2>&1)
    ;;
  codex)
    (cd "$dir" && timeout 600 codex exec --skip-git-repo-check -s read-only \
      -c "mcp_servers.flats.url=\"$BASE/mcp\"" -c 'mcp_servers.flats.default_tools_approval_mode="approve"' "$(prompt "$slug" "$dir/site")" </dev/null >"$WORK/$a.out" 2>&1)
    ;;
  cursor)
    mkdir -p "$dir/.cursor"
    printf '{"mcpServers":{"flats":{"url":"%s/mcp"}}}' "$BASE" >"$dir/.cursor/mcp.json"
    (cd "$dir" && timeout 600 cursor-agent -p --trust --approve-mcps --workspace "$dir" \
      "$(prompt "$slug" "$dir/site")" </dev/null >"$WORK/$a.out" 2>&1)
    ;;
  esac
  rc=$?
  secs=$(($(date +%s) - start))
  live=$(curl -fsS "$BASE/api/flats/$slug" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("live_version",0), d.get("private_url",""))' 2>/dev/null)
  ver=${live%% *}
  url=${live#* }
  body=""
  [ -n "$url" ] && body=$(curl -fsS "$url" 2>/dev/null || true)
  if [ "${ver:-0}" -gt 0 ] && grep -q "Deployed by $a" <<<"$body"; then
    RESULT[$a]="PASS (exit $rc, ${secs}s, live v$ver at $url)"
  else
    RESULT[$a]="FAIL (exit $rc, ${secs}s; see $WORK/$a.out)"
  fi
  echo "$a: ${RESULT[$a]}"
done

echo
echo "== agent gate =="
for a in "${AGENTS[@]}"; do echo "$a: ${RESULT[$a]}"; done
echo "logs: $WORK"
