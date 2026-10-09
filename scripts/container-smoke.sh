#!/bin/sh
# End-to-end check of the container image on a Linux Docker host, in both
# documented network modes: host networking, and ports published on
# 127.0.0.1 from a dedicated bridge network. Each mode starts on an empty
# volume, where `flats serve` bootstraps config.json and the database by
# itself, deploys static and JavaScript server flats with the host's flats
# CLI and a built-in docs flat, and approves them through the console API.
# It restarts the container and checks that config, flats and data survive,
# then stops it with SIGTERM. Containers run hardened as docs/container.md
# suggests: read-only root, no capabilities, no new privileges.
#
#   FLATS_RELEASE_TARGETS=linux/amd64 scripts/build-release.sh v0.0.0-ci dist
#   docker build -t flats:ci .
#   tar -xzf dist/flats_linux_amd64.tar.gz -C /tmp flats
#   scripts/container-smoke.sh flats:ci v0.0.0-ci /tmp/flats
#
# Ports default to 17878/17879 (FLATS_SMOKE_PORT, FLATS_SMOKE_LOCAL_PORT) to
# avoid a host's own Flats service. It removes its containers, volumes and
# network on exit.

set -eu

[ $# -eq 3 ] || {
	echo "usage: $0 <image> <version> <host flats binary>" >&2
	exit 2
}
image=$1
version=$2
flats=$3
port=${FLATS_SMOKE_PORT:-17878}
local_port=${FLATS_SMOKE_LOCAL_PORT:-17879}
url=http://127.0.0.1:$port
FLATS_URL=$url
export FLATS_URL
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

# expected_version prints the version a binary built with the stamp $1
# reports. A v0.0.0-… stamp is a development build (a main commit image is
# v0.0.0-sha-<commit>), which reports the commit it was built from, read here
# from the binary $2's own build information.
expected_version() {
	case $1 in
	v0.0.0-*)
		info=$(go version -m "$2") || fail "cannot read the build information of $2"
		rev=$(printf '%s\n' "$info" | sed -n 's/.*vcs\.revision=\([0-9a-f]*\).*/\1/p')
		[ -n "$rev" ] || fail "$2 records no commit"
		dirty=
		printf '%s\n' "$info" | grep -q 'vcs\.modified=true' && dirty=-dirty
		printf '%.7s%s\n' "$rev" "$dirty"
		;;
	*) printf '%s\n' "$1" ;;
	esac
}
reported=$(expected_version "$version" "$flats")
release=false
[ "$reported" = "$version" ] && release=true

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

# deploy_approved FLAT deploys $work/FLAT with the host CLI and approves the
# publish request through the console API.
deploy_approved() {
	"$flats" deploy "$work/$1" --flat "$1" --json >"$work/deploy" && status=0 || status=$?
	[ "$status" -eq 3 ] || fail "deploy of $1 did not wait for approval (exit $status): $(cat "$work/deploy")"
	"$flats" approvals --status pending --json >"$work/approvals"
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

	step "$mode: first start bootstraps an empty volume"
	# shellcheck disable=SC2086 # serve is a word list.
	docker run -d --name "$name" "$@" \
		--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
		-e FLATS_URL="$url" -v "$data:/data" \
		"$image" serve $serve >/dev/null
	wait_for "the host" healthy
	curl -fsS --max-time 5 "$url/api/status" | tr -d ' \n' | grep -qF "\"build\":{\"version\":\"$reported\",\"release\":$release" ||
		fail "the host does not report build $reported in /api/status"
	docker logs "$name" 2>&1 | grep -qF 'initialized a new host in /data' || fail "first start did not report the bootstrap"
	docker exec "$name" flats config show >"$work/config" || fail "config show failed"
	instance=$(sed -n 's/.*"instance_id": *"\([^"]*\)".*/\1/p' "$work/config" | head -n 1)
	[ -n "$instance" ] || fail "config.json was not created: $(cat "$work/config")"
	if grep -qF "$port" "$work/config"; then
		fail "per-run listen flags were stored in config.json"
	fi
	wait_for "a healthy healthcheck" docker_healthy

	step "$mode: approved static deploy"
	deploy_approved hello
	wait_for "the static flat" static_served

	step "$mode: approved JavaScript server deploy with SQLite"
	deploy_approved counter
	wait_for "the server flat" counter_served
	before=$(hits)
	[ -n "$before" ] || fail "server flat returned no hit count"

	step "$mode: approved document deploy under the writable data volume"
	deploy_approved document
	get document | grep -q 'Container document' || fail "built-in docs flat was not served"
	# Distroless has no shell or test utility. Inspect the generated modules
	# through Docker rather than adding tools to the hardened image.
	runtime_copy=$work/$mode-docs-runtime
	docker cp "$name:/data/runtime/docs" "$runtime_copy" || fail "docs modules are not under /data"
	find "$runtime_copy" -type f -name server.js | grep -q . || fail "docs server module was not materialized"
	find "$runtime_copy" -type f -name content.js | grep -q . || fail "docs content module was not materialized"

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
	[ "$(docker logs "$name" 2>&1 | grep -c 'initialized a new host')" -eq 1 ] || fail "restart bootstrapped again"

	step "$mode: SIGTERM stops the host cleanly"
	docker stop -t 20 "$name" >/dev/null
	code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
	[ "$code" = 0 ] || fail "host exited with $code on SIGTERM"
	docker rm "$name" >/dev/null
	name=
}

mkdir -p "$work/hello" "$work/counter" "$work/document"
printf '# Container document\n\nPersistent Markdown.\n' >"$work/document/index.md"
printf '{"type":"docs"}\n' >"$work/document/flats.json"
printf '<h1>Hello from a Flats container</h1>\n' >"$work/hello/index.html"
printf '{"kind":"server"}\n' >"$work/counter/flats.json"
cat >"$work/counter/server.js" <<'EOF'
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    env.DB.exec("INSERT INTO hits VALUES (1)");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
};
EOF

step "image runs flats subcommands as nonroot"
docker run --rm "$image" version | grep -qF "flats $reported " || fail "image does not run flats $reported"
[ "$(docker image inspect -f '{{.Config.User}}' "$image")" = 65532:65532 ] || fail "image does not run as uid 65532"

smoke host
smoke published

printf '\ncontainer-smoke: PASS\n'
