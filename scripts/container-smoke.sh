#!/bin/sh
# End-to-end check of the container image on a Linux Docker host: first start
# with a generated operator credential, console unlock, approved static and
# JavaScript server deploys, restart persistence and the healthcheck. The
# container runs hardened as docs/container.md suggests: read-only root, no
# capabilities, no new privileges.
#
#   FLATS_RELEASE_TARGETS=linux/amd64 scripts/build-release.sh v0.0.0-ci dist
#   docker build -t flats:ci .
#   scripts/container-smoke.sh flats:ci v0.0.0-ci
#
# The container uses host networking, as docs/container.md recommends on
# Linux, so curl on this host reaches its loopback listeners. Ports default to
# 17878/17879 (FLATS_SMOKE_PORT, FLATS_SMOKE_LOCAL_PORT) to avoid a host's own
# Flats service. It removes its container and volumes on exit.

set -eu

[ $# -eq 2 ] || {
	echo "usage: $0 <image> <version>" >&2
	exit 2
}
image=$1
version=$2
port=${FLATS_SMOKE_PORT:-17878}
local_port=${FLATS_SMOKE_LOCAL_PORT:-17879}
url=http://127.0.0.1:$port
name=flats-smoke-$$
data=$name-data
operator=$name-operator
work=$(mktemp -d "${TMPDIR:-/tmp}/flats-container-smoke.XXXXXX")

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	docker volume rm -f "$data" "$operator" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

step() {
	printf '\n== %s\n' "$*"
}

fail() {
	printf 'container-smoke: FAIL: %s\n' "$*" >&2
	docker logs "$name" >&2 2>&1 || true
	exit 1
}

wait_for() {
	what=$1
	shift
	for _ in $(seq 1 60); do
		"$@" && return 0
		sleep 1
	done
	fail "timed out waiting for $what"
}

healthy() {
	curl -fsS --max-time 3 -o /dev/null "$url/api/status" 2>/dev/null
}

health_status() {
	docker inspect -f '{{.State.Health.Status}}' "$name"
}

docker_healthy() {
	[ "$(health_status)" = healthy ]
}

console() {
	curl -fsS --max-time 10 -b "$work/cookies" -c "$work/cookies" \
		-H 'X-Flats-Console: 1' -H "Origin: $url" -H 'Sec-Fetch-Site: same-origin' "$@"
}

operator_configured() {
	console "$url/console/api/operator/session" | grep -q '"configured": *true'
}

# get FLAT prints the body of the flat's root page.
get() {
	curl -fsS --max-time 5 -H "Host: $1.localhost:$local_port" "http://127.0.0.1:$local_port/" 2>/dev/null
}

static_served() {
	get hello | grep -qF 'Hello from a Flats container'
}

counter_served() {
	get counter | grep -q '"hits"'
}

hits() {
	get counter | sed -n 's/.*"hits": *\([0-9]*\).*/\1/p'
}

# deploy_approved FLAT deploys /tmp/FLAT in the container and approves the
# publish request through the unlocked console.
deploy_approved() {
	docker exec "$name" flats deploy "/tmp/$1" --flat "$1" --json >"$work/deploy" && status=0 || status=$?
	[ "$status" -eq 3 ] || fail "deploy of $1 did not wait for approval (exit $status): $(cat "$work/deploy")"
	docker exec "$name" flats approvals --status pending --json >"$work/approvals"
	id=$(sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' "$work/approvals" | head -n 1)
	[ -n "$id" ] || fail "no pending approval for $1: $(cat "$work/approvals")"
	console -X POST "$url/console/api/approvals/$id/approve" >/dev/null || fail "approving $id for $1 failed"
}

start() {
	docker run -d --name "$name" --network host \
		--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
		-e FLATS_URL="$url" \
		-v "$data:/data" -v "$operator:/run/flats-operator" \
		"$image" serve --listen "127.0.0.1:$port" --local-addr "127.0.0.1:$local_port" >/dev/null
}

step "image runs flats subcommands"
docker run --rm "$image" version | grep -qF "flats $version" || fail "image does not run flats $version"
docker run --rm "$image" flats version | grep -qF "flats $version" || fail "leading flats argument was not accepted"
[ "$(docker run --rm --entrypoint id "$image" -u)" = 65532 ] || fail "image does not run as uid 65532"

step "first start creates the operator credential"
start
wait_for "the host" healthy
operator_configured || fail "host has no operator credential"
docker exec "$name" sh -c 'stat -c "%a %u %s" /run/flats-operator/credential' >"$work/stat"
[ "$(cat "$work/stat")" = "600 65532 65" ] || fail "unexpected credential file: $(cat "$work/stat")"
docker exec "$name" cat /run/flats-operator/credential >"$work/credential"
cred=$(tr -d '\n' <"$work/credential")
docker logs "$name" >"$work/logs" 2>&1
grep -qF 'created operator credential' "$work/logs" || fail "first start did not report the new credential"
if grep -qF "$cred" "$work/logs"; then
	fail "the credential was printed to the container log"
fi
wait_for "a healthy healthcheck" docker_healthy

step "unlock the console"
printf '{"credential":"%s"}' "$cred" |
	console -H 'Content-Type: application/json' --data-binary @- "$url/console/api/operator/session" >/dev/null ||
	fail "console unlock with the generated credential failed"
console "$url/console/api/operator/session" | grep -q '"authorized": *true' || fail "console session is not authorized"

step "approved static deploy"
docker exec "$name" sh -c '
	mkdir -p /tmp/hello &&
	printf "<h1>Hello from a Flats container</h1>\n" >/tmp/hello/index.html'
deploy_approved hello
wait_for "the static flat" static_served

step "approved JavaScript server deploy with SQLite"
docker exec "$name" sh -c '
	mkdir -p /tmp/counter &&
	printf "{\"kind\":\"server\"}\n" >/tmp/counter/flats.json &&
	cat >/tmp/counter/server.js <<EOF
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    env.DB.exec("INSERT INTO hits VALUES (1)");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
};
EOF'
deploy_approved counter
wait_for "the server flat" counter_served
before=$(hits)
[ -n "$before" ] || fail "server flat returned no hit count"

step "restart keeps data, credential and flats"
docker restart "$name" >/dev/null
wait_for "the restarted host" healthy
wait_for "the static flat after restart" static_served
after=$(hits)
if [ -z "$after" ] || [ "$after" -le "$before" ]; then
	fail "server flat data did not survive the restart ($before then $after hits)"
fi
docker exec "$name" cat /run/flats-operator/credential | cmp -s - "$work/credential" || fail "the credential changed on restart"
operator_configured || fail "restarted host has no operator credential"
[ "$(docker logs "$name" 2>&1 | grep -c 'created operator credential')" -eq 1 ] || fail "restart created another credential"

step "SIGTERM stops the host cleanly"
docker stop -t 20 "$name" >/dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" = 0 ] || fail "host exited with $(docker inspect -f '{{.State.ExitCode}}' "$name") on SIGTERM"

printf '\ncontainer-smoke: PASS\n'
