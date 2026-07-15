#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GRAPH_PATH="${GRAPHIFY_GRAPH_PATH:-$ROOT/graphify-out/graph.json}"
BUDGET="${GRAPHIFY_QUERY_BUDGET:-1200}"

if [[ $# -lt 1 ]]; then
  printf 'usage: scripts/graphify-query.sh "focused question or symbol"\n' >&2
  exit 2
fi
if [[ ! "$BUDGET" =~ ^[0-9]+$ ]] || (( BUDGET < 200 || BUDGET > 2000 )); then
  printf 'GRAPHIFY_QUERY_BUDGET must be between 200 and 2000\n' >&2
  exit 2
fi
if [[ ! -f "$GRAPH_PATH" ]]; then
  printf '%s does not exist; run scripts/graphify-update.sh first\n' "$GRAPH_PATH" >&2
  exit 1
fi

graphify query "$*" --graph "$GRAPH_PATH" --budget "$BUDGET"
