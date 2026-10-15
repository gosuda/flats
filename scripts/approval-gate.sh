#!/usr/bin/env bash
# Shared gate helpers. Sourcing this file performs no action.
# A real provider gate always waits for the operator. Only a caller that owns a
# disposable Local-only host may simulate the operator for its exact request.
gate_json() {
  python3 -c 'import json,sys
value=json.load(sys.stdin)
for key in sys.argv[1].split("."): value=value[key]
print(value)' "$1"
}

gate_request() {
  local payload code id link state receipt deadline timeout
  if payload=$("$F" --json "$@"); then code=0; else code=$?; fi
  if [ "$code" -ne 3 ]; then
    echo "Expected an approval request (exit 3), got $code: $payload" >&2
    return 1
  fi
  id=$(gate_json approval.id <<<"$payload") || return 1
  link=$(gate_json approval_url <<<"$payload") || return 1
  case "$id" in ''|*[!a-zA-Z0-9_-]*) echo "Invalid approval id" >&2; return 1;; esac
  echo "Operator approval required: $link" >&2
  if [ "${FLATS_GATE_DISPOSABLE_OPERATOR:-0}" = 1 ]; then
    case "$FLATS_URL" in http://127.0.0.1:*) ;; *) echo "Disposable operator requires a loopback host" >&2; return 1;; esac
    # The Local-only caller has verified its own PID/listener and temp data.
    # This is a test operator intervention, never an agent deciding its action.
    echo "OPERATOR INTERVENTION (disposable Local host): approve $id" >&2
    curl -fsS --max-time 60 -X POST -H 'X-Flats-Console: 1' \
      -H 'Sec-Fetch-Site: same-origin' -H "Origin: $FLATS_URL" \
      "$FLATS_URL/console/api/approvals/$id/approve" >/dev/null || return 1
  fi
  timeout=${FLATS_GATE_APPROVAL_TIMEOUT:-600}
  case "$timeout" in ''|*[!0-9]*) echo "Approval timeout must be seconds" >&2; return 1;; esac
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -le "$deadline" ]; do
    receipt=$(curl -fsS --max-time 30 "$FLATS_URL/api/approvals/$id") || return 1
    state=$(gate_json status <<<"$receipt") || return 1
    case "$state" in
      approved)
        if python3 -c 'import json,sys; sys.exit(0 if (json.load(sys.stdin).get("result_data") or {}).get("cleanup_pending") else 1)' <<<"$receipt"; then
          echo "Deletion applied but cleanup is pending; inspect the receipt and repair storage before restarting. Do not repeat deletion." >&2
          echo "$receipt" >&2
          return 1
        fi
        printf '%s\n' "$receipt"; return 0;;
      failed|rejected) echo "Approval $id ended $state: $receipt" >&2; return 1;;
      pending|applying) sleep 1;;
      *) echo "Unknown approval state: $receipt" >&2; return 1;;
    esac
  done
  echo "Timed out waiting for operator decision for $id; request remains pending at $link" >&2
  return 1
}

gate_create_saved() {
  local slug=$1 saved=$2 revision hash receipt
  revision=$(gate_json draft.revision <<<"$saved") || return 1
  hash=$(gate_json draft.hash <<<"$saved") || return 1
  receipt=$(gate_request create-version "$slug" --revision "$revision" --hash "$hash") || return 1
  gate_json result_data.version <<<"$receipt"
}

gate_save_and_deploy() {
  local slug=$1 dir=$2 saved version
  saved=$("$F" --json save-draft "$slug" "$dir") || return 1
  version=$(gate_create_saved "$slug" "$saved") || return 1
  gate_request deploy --flat "$slug" --version "$version" >/dev/null
}
