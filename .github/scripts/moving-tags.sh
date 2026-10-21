#!/usr/bin/env bash
# Decide which moving tags (:latest, :X.Y, :edge) this publish run may update,
# so that they only ever move forward. Prints latest=, minor= and edge= lines
# for $GITHUB_OUTPUT.
#
# Reads only local refs: the caller fetches tags (tag runs) or origin/main
# (main runs) first. Inputs are the usual GITHUB_REF_TYPE, GITHUB_REF_NAME and
# GITHUB_SHA. test/release/moving_tags_test.go covers it.
set -euo pipefail

latest=false
minor=false
edge=false

if [ "$GITHUB_REF_TYPE" = tag ]; then
  case "$GITHUB_REF_NAME" in
    *-*) ;; # pre-release: exact version only
    *)
      stable=$(git tag -l 'v*' | grep -E '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' | sort -V || true)
      ver=${GITHUB_REF_NAME#v}
      maj=${ver%%.*}
      rest=${ver#*.}
      min=${rest%%.*}
      if [ "$(printf '%s\n' "$stable" | tail -n1)" = "$GITHUB_REF_NAME" ]; then
        latest=true
      fi
      if [ "$(printf '%s\n' "$stable" | grep -E "^v${maj}\.${min}\.[0-9]+$" | tail -n1)" = "$GITHUB_REF_NAME" ]; then
        minor=true
      fi
      ;;
  esac
else
  if [ "$(git rev-parse origin/main)" = "$GITHUB_SHA" ]; then
    edge=true
  fi
fi

printf 'latest=%s\nminor=%s\nedge=%s\n' "$latest" "$minor" "$edge"
