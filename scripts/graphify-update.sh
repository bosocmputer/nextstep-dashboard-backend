#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXPECTED_VERSION="${GRAPHIFY_VERSION:-0.8.35}"
OUTPUT="$ROOT/graphify-out"
SNAPSHOT="$(mktemp -d "${TMPDIR:-/tmp}/nextstep-graphify.XXXXXX")"
trap 'rm -rf "$SNAPSHOT"' EXIT

command -v graphify >/dev/null 2>&1 || {
  printf 'graphify is not installed; expected graphifyy %s\n' "$EXPECTED_VERSION" >&2
  exit 1
}

ACTUAL_VERSION="$(graphify --version 2>/dev/null | awk '{print $2}')"
if [[ "$ACTUAL_VERSION" != "$EXPECTED_VERSION" ]]; then
  printf 'expected graphify %s, found %s\n' "$EXPECTED_VERSION" "${ACTUAL_VERSION:-unknown}" >&2
  exit 1
fi

cd "$ROOT"
git rev-parse --verify HEAD >/dev/null
git archive --format=tar HEAD | tar -xf - -C "$SNAPSHOT"
cp "$ROOT/.graphifyignore" "$SNAPSHOT/.graphifyignore"

if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
  printf 'note: uncommitted tracked changes are excluded; graph is built from HEAD\n' >&2
fi

graphify update "$SNAPSHOT" --no-cluster
rm -rf "$OUTPUT"
cp -R "$SNAPSHOT/graphify-out" "$OUTPUT"

"$ROOT/scripts/graphify-preflight.sh"
