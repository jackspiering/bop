#!/usr/bin/env bash
# Do not rebuild an existing release. Looks the image tag of this publish run
# up in the registry and, when it already exists, writes digest=<sha256:...> to
# $GITHUB_OUTPUT so the run re-signs and re-tags that digest instead of
# building a new one.
#
# A release tag (:X.Y.Z) is immutable: finding it on the first attempt fails
# the run. A main run looks for :sha-<short> and reuses it on any attempt.
# A "not found" answer means nothing was published yet and the build proceeds;
# any other failure stops the run.
#
# Inputs are the usual GITHUB_REF_TYPE, GITHUB_REF_NAME, GITHUB_SHA,
# GITHUB_RUN_ATTEMPT and GITHUB_OUTPUT, plus IMAGE. test/release/
# existing_release_test.go covers it with a fake docker on PATH.
set -euo pipefail

if [ "$GITHUB_REF_TYPE" = tag ]; then
  ref="${IMAGE}:${GITHUB_REF_NAME#v}"
else
  ref="${IMAGE}:sha-${GITHUB_SHA::7}"
fi
if out="$(docker buildx imagetools inspect "$ref" --format '{{.Manifest.Digest}}' 2>&1)"; then
  if ! [[ $out =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "::error::cannot read the digest of $ref: $out"
    exit 1
  fi
  if [ "$GITHUB_REF_TYPE" = tag ] && [ "$GITHUB_RUN_ATTEMPT" = 1 ]; then
    echo "::error::$ref already exists ($out); release tags are immutable, cut a new version"
    exit 1
  fi
  echo "::notice::$ref already exists; re-signing and re-tagging $out instead of rebuilding"
  echo "digest=$out" >> "$GITHUB_OUTPUT"
elif ! printf '%s\n' "$out" | grep -qF "$ref: not found"; then
  echo "::error::cannot check $ref: $out"
  exit 1
fi
