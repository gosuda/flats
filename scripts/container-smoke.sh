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
# Requires Python 3 on the Docker host for exact JSON approval receipts.

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
# A binary from before internal/buildinfo, such as an older commit image built
# by the Commit images workflow, reports the stamp itself and no system.build
# in /api/status; its version --json has no release field.
if "$flats" version --json | tr -d ' \n' | grep -qF '"release":'; then
	reported=$(expected_version "$version" "$flats")
	release=false
	[ "$reported" = "$version" ] && release=true
else
	reported=$version release=
fi

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

# json_field FIELD FILE reads only the named field from a CLI/API receipt.
json_field() {
	python3 -c 'import json,sys
value=json.load(open(sys.argv[2]))
for key in sys.argv[1].split("."): value=value[key]
print(value)' "$1" "$2"
}

# approval_applied checks exactly the requested approval, never a pending-list
# candidate. The disposable test operator is separate from the CLI client.
approval_applied() {
	curl -fsS --max-time 5 "$url/api/approvals/$1" >"$work/approval-result" || return 1
	decision_status=$(json_field status "$work/approval-result") || fail "invalid approval receipt"
	case "$decision_status" in
	approved) return 0;;
	pending|applying) return 1;;
	*) fail "approval $1 ended $decision_status: $(cat "$work/approval-result")";;
	esac
}

# approve_requested LABEL REQUEST approves only the ID captured by that request.
# This intervention is allowed only against this smoke test's own Docker data.
approve_requested() {
	[ "$(json_field status "$2")" = pending_approval ] || fail "request $1 is not pending"
	request_id=$(json_field approval.id "$2") || fail "missing approval ID for $1"
	case "$request_id" in ''|*[!a-zA-Z0-9_-]*) fail "invalid approval ID for $1";; esac
	echo "OPERATOR INTERVENTION (disposable container): approve $request_id for $1" >&2
	console -X POST "$url/console/api/approvals/$request_id/approve" >/dev/null || fail "approving $request_id for $1 failed"
	wait_for "approval $request_id" approval_applied "$request_id"
}

# deploy_approved FLAT saves its Draft, freezes that exact receipt for an
# approved version, then separately requests and approves that version's deploy.
deploy_approved() {
	"$flats" --json save-draft "$1" "$work/$1" >"$work/save" || fail "saving Draft $1 failed: $(cat "$work/save")"
	draft_revision=$(json_field draft.revision "$work/save") || fail "missing Draft revision"
	draft_hash=$(json_field draft.hash "$work/save") || fail "missing Draft hash"
	"$flats" --json create-version "$1" --revision "$draft_revision" --hash "$draft_hash" >"$work/create" && status=0 || status=$?
	[ "$status" -eq 3 ] || fail "version creation of $1 did not wait for approval (exit $status): $(cat "$work/create")"
	approve_requested "creating $1 version" "$work/create"
	created_version=$(json_field result_data.version "$work/approval-result") || fail "creation receipt lacks version"
	case "$created_version" in ''|*[!0-9]*) fail "invalid created version: $created_version";; esac
	[ "$created_version" -gt 0 ] || fail "created version must be positive"
	"$flats" --json deploy --flat "$1" --version "$created_version" >"$work/deploy" && status=0 || status=$?
	[ "$status" -eq 3 ] || fail "deploy of $1 version $created_version did not wait for approval (exit $status): $(cat "$work/deploy")"
	approve_requested "deploying $1 version $created_version" "$work/deploy"
}

# Documents run immediately. Their explicit whole-bundle save has no approval.
deploy_document() {
	"$flats" --json save-document "$1" "$work/$1" >"$work/deploy" && status=0 || status=$?
	[ "$status" -eq 0 ] || fail "save of document $1 failed (exit $status): $(cat "$work/deploy")"
	[ "$(json_field flat.type "$work/deploy")" = docs ] || fail "document save returned wrong type"
	[ "$(json_field flat.running "$work/deploy")" = True ] || fail "saved document is not running"
	"$flats" approvals --status pending --json >"$work/approvals"
	! grep -q '"id"' "$work/approvals" || fail "save of document $1 requested approval: $(cat "$work/approvals")"
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
	if [ -n "$release" ]; then
		curl -fsS --max-time 5 "$url/api/status" | tr -d ' \n' | grep -qF "\"build\":{\"version\":\"$reported\",\"release\":$release" ||
			fail "the host does not report build $reported in /api/status"
	fi
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

	step "$mode: document deploy (no approval) under the writable data volume"
	deploy_document document
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
