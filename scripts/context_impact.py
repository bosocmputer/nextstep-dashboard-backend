#!/usr/bin/env python3
from __future__ import annotations

import argparse
import dataclasses
import fnmatch
import json
import os
import pathlib
import re
import subprocess
import sys
from typing import Any

SENSITIVE_CONTEXT_PATTERNS = {
    "UUID": re.compile(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b", re.I),
    "private key": re.compile(r"-----BEGIN (?:RSA |OPENSSH |EC )?PRIVATE KEY-----"),
    "token-like value": re.compile(
        r"(?:sk-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,})"
    ),
    "entry reference": re.compile(r"(?:deliveryRef|(?:^|[?&])ref)=[A-Za-z0-9_-]{20,}", re.M),
    "customer IP address": re.compile(r"(?<![A-Za-z0-9])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?::[0-9]+)?"),
    "customer dynamic endpoint": re.compile(r"https?://[^\s)]*(?:ddns|iszai)", re.I),
}


class ImpactError(RuntimeError):
    pass


@dataclasses.dataclass(frozen=True)
class Rule:
    id: str
    source_patterns: tuple[str, ...]
    exclude_patterns: tuple[str, ...]
    context_documents: tuple[str, ...]
    fallback: bool


@dataclasses.dataclass(frozen=True)
class GateResult:
    changed_paths: tuple[str, ...]
    impacted_rule_ids: tuple[str, ...]
    acknowledged_rule_ids: tuple[str, ...]


def sensitive_context_labels(text: str) -> tuple[str, ...]:
    return tuple(label for label, pattern in SENSITIVE_CONTEXT_PATTERNS.items() if pattern.search(text))


def _matches(path: str, patterns: tuple[str, ...]) -> bool:
    return any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns)


def _run_git(root: pathlib.Path, *args: str) -> str:
    process = subprocess.run(
        ("git", *args),
        cwd=root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if process.returncode != 0:
        detail = process.stderr.strip() or process.stdout.strip() or "git command failed"
        raise ImpactError(detail)
    return process.stdout


def _resolve_commit(root: pathlib.Path, label: str, revision: str) -> str:
    if not revision.strip():
        raise ImpactError(f"{label} revision is empty")
    try:
        return _run_git(root, "rev-parse", "--verify", f"{revision}^{{commit}}").strip()
    except ImpactError as error:
        raise ImpactError(f"invalid {label} revision: {revision}") from error


def load_rules(map_path: pathlib.Path) -> tuple[Rule, ...]:
    try:
        raw: Any = json.loads(map_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ImpactError(f"cannot load context map: {error}") from error
    if not isinstance(raw, dict) or raw.get("version") != 1 or not isinstance(raw.get("rules"), list):
        raise ImpactError("context map must contain version 1 and a rules array")

    rules: list[Rule] = []
    seen: set[str] = set()
    expected_keys = {"id", "sourcePatterns", "excludePatterns", "contextDocuments", "fallback"}
    for index, item in enumerate(raw["rules"]):
        if not isinstance(item, dict) or set(item) != expected_keys:
            raise ImpactError(f"context map rule {index} has invalid fields")
        rule_id = item["id"]
        if not isinstance(rule_id, str) or not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", rule_id):
            raise ImpactError(f"context map rule {index} has invalid id")
        if rule_id in seen:
            raise ImpactError(f"context map contains duplicate rule id: {rule_id}")
        seen.add(rule_id)
        for field in ("sourcePatterns", "excludePatterns", "contextDocuments"):
            if not isinstance(item[field], list) or not all(isinstance(value, str) and value for value in item[field]):
                raise ImpactError(f"context map rule {rule_id} has invalid {field}")
        if not item["sourcePatterns"] or not item["contextDocuments"] or not isinstance(item["fallback"], bool):
            raise ImpactError(f"context map rule {rule_id} is incomplete")
        rules.append(
            Rule(
                id=rule_id,
                source_patterns=tuple(item["sourcePatterns"]),
                exclude_patterns=tuple(item["excludePatterns"]),
                context_documents=tuple(item["contextDocuments"]),
                fallback=item["fallback"],
            )
        )
    if sum(rule.fallback for rule in rules) != 1:
        raise ImpactError("context map must contain exactly one fallback rule")
    return tuple(rules)


def _validate_pattern(pattern: str, *, rule_id: str, field: str) -> None:
    if pattern.startswith("/") or "\\" in pattern or ".." in pathlib.PurePosixPath(pattern).parts:
        raise ImpactError(f"context map rule {rule_id} has unsafe {field} pattern: {pattern}")


def validate_repository(
    *,
    root: pathlib.Path,
    map_path: pathlib.Path,
    marker_pairs: tuple[tuple[str, str, str], ...] = (),
) -> tuple[Rule, ...]:
    root = root.resolve()
    rules = load_rules(map_path)
    tracked = tuple(path for path in _run_git(root, "ls-files").splitlines() if path)
    for rule in rules:
        for field, patterns in (
            ("sourcePatterns", rule.source_patterns),
            ("excludePatterns", rule.exclude_patterns),
            ("contextDocuments", rule.context_documents),
        ):
            for pattern in patterns:
                _validate_pattern(pattern, rule_id=rule.id, field=field)
        for pattern in rule.source_patterns:
            if not any(fnmatch.fnmatchcase(path, pattern) for path in tracked):
                raise ImpactError(f"context map source pattern matches no tracked file: {rule.id}: {pattern}")
        for pattern in rule.context_documents:
            if not any(fnmatch.fnmatchcase(path, pattern) for path in tracked):
                raise ImpactError(f"context map document pattern matches no tracked file: {rule.id}: {pattern}")

    for document_path, start_marker, end_marker in marker_pairs:
        target = root / document_path
        if not target.is_file():
            raise ImpactError(f"generated context document is missing: {document_path}")
        content = target.read_text(encoding="utf-8")
        if content.count(start_marker) != 1 or content.count(end_marker) != 1:
            raise ImpactError(f"generated markers must appear exactly once: {document_path}")
        if content.index(start_marker) >= content.index(end_marker):
            raise ImpactError(f"generated markers are out of order: {document_path}")
    return rules


def _parse_acknowledgement(pr_body: str, known_ids: set[str]) -> tuple[set[str], str]:
    reviewed = re.findall(r"(?mi)^Context-Reviewed:\s*(.*?)\s*$", pr_body)
    reasons = re.findall(r"(?mi)^Context-Reason:\s*(.*?)\s*$", pr_body)
    if not reviewed and not reasons:
        return set(), ""
    if len(reviewed) != 1 or len(reasons) != 1:
        raise ImpactError("Context-Reviewed and Context-Reason must each appear exactly once")
    ids = {value.strip() for value in reviewed[0].split(",") if value.strip()}
    if not ids:
        raise ImpactError("Context-Reviewed must list at least one rule id")
    unknown = sorted(ids - known_ids)
    if unknown:
        raise ImpactError(f"unknown Context-Reviewed rule id: {', '.join(unknown)}")
    reason = reasons[0].strip()
    if len(reason) < 20:
        raise ImpactError("Context-Reason must explain the review in at least 20 characters")
    return ids, reason


def evaluate(*, root: pathlib.Path, map_path: pathlib.Path, base: str, head: str, pr_body: str) -> GateResult:
    root = root.resolve()
    base_sha = _resolve_commit(root, "base", base)
    head_sha = _resolve_commit(root, "head", head)
    try:
        _run_git(root, "merge-base", "--is-ancestor", base_sha, head_sha)
    except ImpactError as error:
        raise ImpactError("base revision is not an ancestor of head") from error

    rules = load_rules(map_path)
    known_ids = {rule.id for rule in rules}
    acknowledged, _ = _parse_acknowledgement(pr_body, known_ids)
    tree_paths = tuple(path for path in _run_git(root, "ls-tree", "-r", "--name-only", head_sha).splitlines() if path)
    for rule in rules:
        if not any(_matches(path, rule.context_documents) for path in tree_paths):
            raise ImpactError(f"mapped context document is missing for rule {rule.id}")

    changed = tuple(
        sorted(
            path
            for path in _run_git(root, "diff", "--name-only", "--diff-filter=ACMRD", base_sha, head_sha).splitlines()
            if path
        )
    )
    specific = tuple(rule for rule in rules if not rule.fallback)
    fallback = next(rule for rule in rules if rule.fallback)
    impacted: set[str] = set()
    for path in changed:
        matched_specific = False
        for rule in specific:
            if _matches(path, rule.source_patterns) and not _matches(path, rule.exclude_patterns):
                impacted.add(rule.id)
                matched_specific = True
        if not matched_specific and _matches(path, fallback.source_patterns) and not _matches(path, fallback.exclude_patterns):
            impacted.add(fallback.id)

    unsatisfied: list[str] = []
    for rule in rules:
        if rule.id not in impacted:
            continue
        document_changed = any(_matches(path, rule.context_documents) for path in changed)
        if not document_changed and rule.id not in acknowledged:
            unsatisfied.append(rule.id)
    if unsatisfied:
        joined = ", ".join(sorted(unsatisfied))
        raise ImpactError(
            "context review required for: "
            f"{joined}. Update a mapped knowledge note or add Context-Reviewed and Context-Reason to the PR body."
        )

    return GateResult(
        changed_paths=changed,
        impacted_rule_ids=tuple(sorted(impacted)),
        acknowledged_rule_ids=tuple(sorted(acknowledged)),
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Enforce source-to-context review coverage")
    parser.add_argument("--base", required=True)
    parser.add_argument("--head", required=True)
    parser.add_argument("--map", default="docs/knowledge/context-map.json")
    args = parser.parse_args(argv)
    root = pathlib.Path(__file__).resolve().parent.parent
    try:
        result = evaluate(
            root=root,
            map_path=(root / args.map).resolve(),
            base=args.base,
            head=args.head,
            pr_body=os.environ.get("CONTEXT_PR_BODY", ""),
        )
    except ImpactError as error:
        print(f"context impact failed: {error}", file=sys.stderr)
        return 1
    impacted = ", ".join(result.impacted_rule_ids) or "none"
    acknowledged = ", ".join(result.acknowledged_rule_ids) or "none"
    print(f"context impact ok: impacted={impacted} acknowledged={acknowledged}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
