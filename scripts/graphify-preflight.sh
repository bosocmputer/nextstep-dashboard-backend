#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXPECTED_VERSION="${GRAPHIFY_VERSION:-0.8.35}"
GRAPH_PATH="${GRAPHIFY_GRAPH_PATH:-$ROOT/graphify-out/graph.json}"
MAX_GRAPH_BYTES="${GRAPHIFY_MAX_GRAPH_BYTES:-20000000}"

fail() {
  printf 'graphify preflight failed: %s\n' "$1" >&2
  exit 1
}

command -v graphify >/dev/null 2>&1 || fail 'graphify is not installed'
command -v jq >/dev/null 2>&1 || fail 'jq is required'
ACTUAL_VERSION="$(graphify --version 2>/dev/null | awk '{print $2}')"
[[ "$ACTUAL_VERSION" == "$EXPECTED_VERSION" ]] || fail "expected $EXPECTED_VERSION, found ${ACTUAL_VERSION:-unknown}"
[[ -f "$GRAPH_PATH" ]] || fail "$GRAPH_PATH does not exist"

GRAPH_BYTES="$(wc -c < "$GRAPH_PATH" | tr -d ' ')"
[[ "$GRAPH_BYTES" -le "$MAX_GRAPH_BYTES" ]] || fail "$GRAPH_PATH is larger than $MAX_GRAPH_BYTES bytes"
jq -e '(.nodes | type == "array") and (.links | type == "array")' "$GRAPH_PATH" >/dev/null || fail 'graph schema is invalid'

cd "$ROOT"
git check-ignore -q graphify-out/graph.json || fail 'graphify-out is not ignored by Git'
[[ -z "$(git ls-files graphify-out)" ]] || fail 'graphify-out contains tracked files'

if jq -r '.nodes[].source_file // empty' "$GRAPH_PATH" | grep -Eiq '(^|/)(\.env($|\.)|backups?|node_modules|dist|coverage|screenshots?|artifacts?|tmp|vendor)(/|$)'; then
  fail 'graph references excluded runtime or sensitive paths'
fi
if grep -Eq '(sk-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|-----BEGIN ((RSA|OPENSSH|EC) )?PRIVATE KEY-----)' "$GRAPH_PATH"; then
  fail 'graph appears to contain token-like or private-key material'
fi

NODES="$(jq '.nodes | length' "$GRAPH_PATH")"
LINKS="$(jq '.links | length' "$GRAPH_PATH")"
printf 'graphify preflight ok: version=%s nodes=%s links=%s bytes=%s\n' "$ACTUAL_VERSION" "$NODES" "$LINKS" "$GRAPH_BYTES"
