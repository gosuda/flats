#!/bin/sh
# End-to-end check of install.sh against locally built release archives:
# fresh service install, upgrade restart, crash recovery and uninstall.
#
#   scripts/build-release.sh v0.0.0-ci dist
#   scripts/install-smoke.sh dist v0.0.0-ci
#
# It installs the real per-user service with default ports and paths, so run
# it only on a disposable machine such as a CI runner.

set -eu

[ $# -eq 2 ] || {
	echo "usage: $0 <release-dir> <version>" >&2
	exit 2
}
FLATS_DOWNLOAD_BASE=$(cd "$1" && pwd)
export FLATS_DOWNLOAD_BASE
version=$2
root=$(cd "$(dirname "$0")/.." && pwd)
flats=$HOME/.local/bin/flats
url=http://127.0.0.1:7878

step() {
	printf '\n== %s\n' "$*"
}

fail() {
	printf 'install-smoke: FAIL: %s\n' "$*" >&2
	exit 1
}

main_pid() {
	if [ "$(uname -s)" = Darwin ]; then
		launchctl print "gui/$(id -u)/dev.flats.serve" 2>/dev/null | awk '$1 == "pid" { print $3 }'
	else
		systemctl --user show -p MainPID --value flats.service
	fi
}

healthy() {
	curl -fsS --max-time 3 -o /dev/null "$url/api/status"
}

service_file() {
	if [ "$(uname -s)" = Darwin ]; then
		echo "$HOME/Library/LaunchAgents/dev.flats.serve.plist"
	else
		echo "$HOME/.config/systemd/user/flats.service"
	fi
}

operator_configured() {
	curl -fsS -H 'X-Flats-Console: 1' -H "Origin: $url" -H 'Sec-Fetch-Site: same-origin' \
		"$url/console/api/operator/session" | grep -q '"configured": *true'
}

step "fresh install"
sh "$root/install.sh"
"$flats" version | grep -qF "flats $version" || fail "installed binary is not $version"
healthy || fail "service is not answering"
operator_configured || fail "service has no operator credential"
cred=${XDG_CONFIG_HOME:-$HOME/.config}/flats-operator/credential
[ "$(wc -c <"$cred" | tr -d ' ')" -eq 65 ] || fail "unexpected credential size"
grep -q -- '--config' "$(service_file)" || fail "service does not run from config.json"
"$flats" config validate || fail "config.json does not validate"

step "rerun restarts the service"
pid=$(main_pid)
if [ -z "$pid" ] || [ "$pid" = 0 ]; then
	fail "no service pid"
fi
sh "$root/install.sh"
new=$(main_pid)
if [ -z "$new" ] || [ "$new" = "$pid" ]; then
	fail "service was not restarted (pid $pid -> $new)"
fi
operator_configured || fail "restarted service lost its operator credential"
grep -q -- '--config' "$(service_file)" || fail "rerun dropped --config from the service"

step "service recovers from a crash"
kill -9 "$new"
i=0
until p=$(main_pid) && [ -n "$p" ] && [ "$p" != 0 ] && [ "$p" != "$new" ] && healthy; do
	i=$((i + 1))
	[ $i -lt 60 ] || fail "service did not come back after kill -9"
	sleep 1
done
echo "restarted as pid $p after ${i}s"

step "a stopped service stays stopped on upgrade"
if [ "$(uname -s)" = Darwin ]; then
	launchctl bootout "gui/$(id -u)/dev.flats.serve"
else
	systemctl --user stop flats.service
fi
i=0
while healthy 2>/dev/null; do
	i=$((i + 1))
	[ $i -lt 20 ] || fail "service still answers after it was stopped"
	sleep 1
done
out=$(sh "$root/install.sh" 2>&1) || fail "installer failed on a stopped service: $out"
printf '%s\n' "$out"
printf '%s\n' "$out" | grep -q 'Start the service' || fail "installer did not print how to start the service"
sleep 3
! healthy 2>/dev/null || fail "installer started a service the user stopped"

step "uninstall"
"$flats" uninstall
i=0
while healthy 2>/dev/null; do
	i=$((i + 1))
	[ $i -lt 20 ] || fail "service still answers after uninstall"
	sleep 1
done
echo "install-smoke: ok"
