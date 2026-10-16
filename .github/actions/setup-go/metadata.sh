#!/usr/bin/env bash
# Inputs are supplied by action.yml; emit no secrets or source contents.
set -euo pipefail

case "$MODULE_SCOPE" in
  root) hash=$ROOT_HASH ;;
  netbird) hash=$NETBIRD_HASH ;;
  *) echo "unknown module scope: $MODULE_SCOPE" >&2; exit 1 ;;
esac
[ -n "$hash" ] || { echo "source module files missing" >&2; exit 1; }
toolchain=$(go env GOVERSION)
build=$(go env GOCACHE)
mod=$(go env GOMODCACHE)
# An immutable dependency-only key never picks up source-only rebuilds.
# Weekly generations limit upload churn (not total retained storage;
# GitHub's cache eviction policy still applies). Keep per-shard test
# result caches: sharing a writer would drop the other shards' results.
week=$(date -u +%G-%V)
{
  echo "toolchain=$toolchain"
  echo "week=$week"
  echo "build=$build"
  echo "mod=$mod"
  echo "hash=$hash"
  if [ "$GITHUB_EVENT_NAME" = push ] && [ "$GITHUB_REF" = refs/heads/main ]; then
    echo "save=true"
  fi
} >>"$GITHUB_OUTPUT"
