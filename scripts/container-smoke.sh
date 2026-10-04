#!/bin/sh
# End-to-end check of the container image on a Linux Docker host, in both
# documented network modes: host networking, and ports published on
# 127.0.0.1 from a dedicated bridge network. Each mode starts from empty data
# (config.json created on first start), approves static and JavaScript server
# deploys and a built-in docs flat through the console API, restarts the container and checks that
# config, flats and data survive, then stops it with SIGTERM. Containers run
# hardened as docs/container.md suggests: read-only root, no capabilities, no
# new privileges.
#
#   FLATS_RELEASE_TARGETS=linux/amd64 scripts/build-release.sh v0.0.0-ci dist
#   docker build -t flats:ci .
#   scripts/container-smoke.sh flats:ci v0.0.0-ci
#
# Ports default to 17878/17879 (FLATS_SMOKE_PORT, FLATS_SMOKE_LOCAL_PORT) to
# avoid a host's own Flats service. It removes its containers, volumes and
# network on exit.

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
prefix=flats-smoke-$$
network=$prefix-net
name=
work=$(mktemp -d "${TMPDIR:-/tmp}/flats-container-smoke.XXXXXX")

cleanup() {
	for mode in host published; do
		docker rm -f "$prefix-$mode" >/dev/null 2>&1 || true
		docker volume rm -f "$prefix-$mode-data" >/dev/null 2>&1 || true
	done
	docker network rm "$network" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

step() {
	printf '\n== %s\n' "$*"
}

fail() {
	printf 'container-smoke: FAIL: %s\n' "$*" >&2
	[ -z "$name" ] || docker logs "$name" >&2 2>&1 || true
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

docker_healthy() {
	[ "$(docker inspect -f '{{.State.Health.Status}}' "$name")" = healthy ]
}

# console sends a request the way the console page does.
console() {
	curl -fsS --max-time 10 -H 'X-Flats-Console: 1' -H "Origin: $url" -H 'Sec-Fetch-Site: same-origin' "$@"
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
# publish request through the console API.
deploy_approved() {
	docker exec "$name" flats deploy "/tmp/$1" --flat "$1" --json >"$work/deploy" && status=0 || status=$?
	[ "$status" -eq 3 ] || fail "deploy of $1 did not wait for approval (exit $status): $(cat "$work/deploy")"
	docker exec "$name" flats approvals --status pending --json >"$work/approvals"
	id=$(sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' "$work/approvals" | head -n 1)
	[ -n "$id" ] || fail "no pending approval for $1: $(cat "$work/approvals")"
	console -X POST "$url/console/api/approvals/$id/approve" >/dev/null || fail "approving $id for $1 failed"
}

# smoke MODE runs the whole check with host networking (host) or published
# loopback ports on a dedicated network (published).
smoke() {
	mode=$1
	name=$prefix-$mode
	data=$name-data
	case $mode in
	host)
		set -- --network host
		serve="--listen 127.0.0.1:$port --local-addr 127.0.0.1:$local_port"
		;;
	published)
		docker network create "$network" >/dev/null
		set -- --network "$network" -p "127.0.0.1:$port:$port" -p "127.0.0.1:$local_port:$local_port"
		serve="--listen 0.0.0.0:$port --local-addr 0.0.0.0:$local_port"
		;;
	esac

	step "$mode: first start creates config.json"
	# shellcheck disable=SC2086 # serve is a word list.
	docker run -d --name "$name" "$@" \
		--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
		-e FLATS_URL="$url" -v "$data:/data" \
		"$image" $serve >/dev/null
	wait_for "the host" healthy
	docker exec "$name" flats config show >"$work/config" || fail "config show failed"
	instance=$(sed -n 's/.*"instance_id": *"\([^"]*\)".*/\1/p' "$work/config" | head -n 1)
	[ -n "$instance" ] || fail "config.json was not created: $(cat "$work/config")"
	wait_for "a healthy healthcheck" docker_healthy

	step "$mode: approved static deploy"
	docker exec "$name" sh -c '
		mkdir -p /tmp/hello &&
		printf "<h1>Hello from a Flats container</h1>\n" >/tmp/hello/index.html'
	deploy_approved hello
	wait_for "the static flat" static_served

	step "$mode: approved JavaScript server deploy with SQLite"
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

	step "$mode: approved document deploy under the writable data volume"
	docker exec "$name" sh -c '
		mkdir -p /tmp/document &&
		printf "# Container document\n\nPersistent Markdown.\n" >/tmp/document/index.md'
	deploy_approved document
	get document | grep -q 'Container document' || fail "built-in docs flat was not served"
	docker exec "$name" test -d /data/runtime/docs || fail "docs modules are not under /data"

	step "$mode: restart keeps config, flats and data"
	docker restart "$name" >/dev/null
	wait_for "the restarted host" healthy
	wait_for "the static flat after restart" static_served
	get document | grep -q 'Container document' || fail "docs flat did not survive restart"
	after=$(hits)
	if [ -z "$after" ] || [ "$after" -le "$before" ]; then
		fail "server flat data did not survive the restart ($before then $after hits)"
	fi
	echo "server flat hits: $before before restart, $after after"
	docker exec "$name" flats config show | grep -qF "$instance" || fail "config.json changed on restart"

	step "$mode: SIGTERM stops the host cleanly"
	docker stop -t 20 "$name" >/dev/null
	code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
	[ "$code" = 0 ] || fail "host exited with $code on SIGTERM"
	docker rm "$name" >/dev/null
	name=
}

step "image runs flats subcommands"
docker run --rm "$image" version | grep -qF "flats $version" || fail "image does not run flats $version"
docker run --rm "$image" flats version | grep -qF "flats $version" || fail "leading flats argument was not accepted"
[ "$(docker run --rm --entrypoint id "$image" -u)" = 65532 ] || fail "image does not run as uid 65532"

smoke host
smoke published

printf '\ncontainer-smoke: PASS\n'
