#!/bin/sh
# Entrypoint of the Flats container image (see docs/container.md).
#
# With no arguments, flags only, or `serve` and flags, it runs `flats serve`
# from FLATS_CONFIG (/data/config.json in the image), first creating that file
# with the defaults when it does not exist yet. Change settings with
# `flats config set` while the host is stopped.
#
# Any other arguments run that flats subcommand, for example `version`,
# `status` or `config show`; a leading `flats` is accepted and dropped.

set -eu

if [ "${1:-}" = flats ]; then
	shift
fi
case ${1:-} in
'' | -*) set -- serve "$@" ;;
esac

if [ "$1" = serve ] && [ -n "${FLATS_CONFIG:-}" ] && [ ! -e "$FLATS_CONFIG" ]; then
	flats config init --config "$FLATS_CONFIG" --data "${FLATS_DATA:-/data}"
fi
exec flats "$@"
