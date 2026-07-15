#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

command -v python3 >/dev/null 2>&1 || {
  printf 'context verification requires python3\n' >&2
  exit 1
}

python3 - "$ROOT" <<'PY'
from __future__ import annotations

import csv
import pathlib
import re
import sys
import urllib.parse

root = pathlib.Path(sys.argv[1]).resolve()
agents = root / "AGENTS.md"
knowledge = root / "docs" / "knowledge"
errors: list[str] = []

if not agents.is_file():
    errors.append("AGENTS.md is missing")
elif agents.stat().st_size > 4096:
    errors.append(f"AGENTS.md is {agents.stat().st_size} bytes; limit is 4096")

notes = sorted(knowledge.rglob("*.md")) if knowledge.is_dir() else []
if not notes:
    errors.append("docs/knowledge contains no Markdown notes")

required = ("status:", "last_verified:", "source_of_truth:", "tags:")
uuid_pattern = re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b", re.I)
secret_patterns = {
    "private key": re.compile(r"-----BEGIN (?:RSA |OPENSSH |EC )?PRIVATE KEY-----"),
    "token-like value": re.compile(r"(?:sk-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,})"),
    "entry reference": re.compile(r"(?:deliveryRef|(?:^|[?&])ref)=[A-Za-z0-9_-]{20,}", re.M),
}

for context_file in [agents, *notes]:
    if not context_file.is_file():
        continue
    context_text = context_file.read_text(encoding="utf-8")
    relative = context_file.relative_to(root)
    if uuid_pattern.search(context_text):
        errors.append(f"{relative} contains a UUID; context must be tenant-neutral")
    for label, pattern in secret_patterns.items():
        if pattern.search(context_text):
            errors.append(f"{relative} contains {label}")

for note in notes:
    text = note.read_text(encoding="utf-8")
    lines = text.splitlines()
    relative = note.relative_to(root)
    limit = 150 if note.name == "00-project-map.md" else 400
    if len(lines) > limit:
        errors.append(f"{relative} has {len(lines)} lines; limit is {limit}")
    if not lines or lines[0] != "---":
        errors.append(f"{relative} is missing YAML frontmatter")
        continue
    try:
        closing = lines.index("---", 1)
    except ValueError:
        errors.append(f"{relative} has unclosed YAML frontmatter")
        continue
    frontmatter = lines[1:closing]
    for key in required:
        if not any(line.startswith(key) for line in frontmatter):
            errors.append(f"{relative} is missing {key[:-1]}")

    source_line = next((line for line in frontmatter if line.startswith("source_of_truth:")), "")
    match = re.fullmatch(r"source_of_truth:\s*\[(.*)\]\s*", source_line)
    if not match:
        errors.append(f"{relative} source_of_truth must be an inline YAML list")
    elif match.group(1).strip():
        for item in next(csv.reader([match.group(1)], skipinitialspace=True)):
            source = item.strip().strip("'\"")
            if source and not (root / source).exists():
                errors.append(f"{relative} references missing source {source}")

    for link in re.finditer(r"\[[^\]]+\]\(([^)]+)\)", text):
        target = link.group(1).strip().strip("<>").split("#", 1)[0]
        if not target or "://" in target or not target.lower().endswith(".md"):
            continue
        resolved = (note.parent / urllib.parse.unquote(target)).resolve()
        if not resolved.is_file():
            errors.append(f"{relative} contains broken link {target}")

if errors:
    print("context verification failed:", file=sys.stderr)
    for error in errors:
        print(f"- {error}", file=sys.stderr)
    raise SystemExit(1)

print(f"context verification ok: notes={len(notes)} agents_bytes={agents.stat().st_size}")
PY

if git ls-files --error-unmatch 'docs/knowledge/.obsidian/*' >/dev/null 2>&1; then
  printf 'context verification failed: Obsidian application state must remain local-only\n' >&2
  exit 1
fi
