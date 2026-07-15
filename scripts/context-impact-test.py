from __future__ import annotations

import json
import pathlib
import subprocess
import tempfile
import unittest

import context_impact


class ContextImpactGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tempdir.name)
        self.run_command("git", "init", "-q")
        self.run_command("git", "config", "user.email", "context@example.invalid")
        self.run_command("git", "config", "user.name", "Context Test")
        self.write("internal/report/catalog.go", "package report\n")
        self.write("internal/worker/report_worker.go", "package worker\n")
        self.write("internal/report/catalog_test.go", "package report\n")
        self.write("docs/knowledge/reports.md", "# Reports\n")
        self.write("docs/knowledge/operations.md", "# Operations\n")
        self.write(
            "docs/knowledge/context-map.json",
            json.dumps(
                {
                    "version": 1,
                    "rules": [
                        {
                            "id": "report-pipeline",
                            "sourcePatterns": ["internal/report/catalog.go"],
                            "excludePatterns": ["**/*_test.go"],
                            "contextDocuments": ["docs/knowledge/reports.md"],
                            "fallback": False,
                        },
                        {
                            "id": "queue-sml",
                            "sourcePatterns": ["internal/worker/report_worker.go"],
                            "excludePatterns": ["**/*_test.go"],
                            "contextDocuments": ["docs/knowledge/operations.md"],
                            "fallback": False,
                        },
                        {
                            "id": "backend-source",
                            "sourcePatterns": ["internal/**"],
                            "excludePatterns": ["**/*_test.go"],
                            "contextDocuments": ["docs/knowledge/*.md"],
                            "fallback": True,
                        },
                    ],
                },
                indent=2,
            )
            + "\n",
        )
        self.run_command("git", "add", ".")
        self.run_command("git", "commit", "-qm", "base")
        self.base = self.rev("HEAD")

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def run_command(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(args, cwd=self.root, check=True, text=True, capture_output=True)

    def rev(self, ref: str) -> str:
        return self.run_command("git", "rev-parse", ref).stdout.strip()

    def write(self, path: str, content: str) -> None:
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content, encoding="utf-8")

    def commit(self, message: str) -> str:
        self.run_command("git", "add", ".")
        self.run_command("git", "commit", "-qm", message)
        return self.rev("HEAD")

    def gate(self, head: str, body: str = "") -> context_impact.GateResult:
        return context_impact.evaluate(
            root=self.root,
            map_path=self.root / "docs/knowledge/context-map.json",
            base=self.base,
            head=head,
            pr_body=body,
        )

    def test_source_change_with_mapped_document_passes(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        self.write("docs/knowledge/reports.md", "# Reports\n\nVersion 2\n")
        result = self.gate(self.commit("update report and note"))
        self.assertEqual(result.impacted_rule_ids, ("report-pipeline",))

    def test_valid_pr_acknowledgement_passes_without_document_change(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        head = self.commit("refactor report")
        result = self.gate(
            head,
            "Context-Reviewed: report-pipeline\n"
            "Context-Reason: Internal refactor only; report contracts and behavior are unchanged.",
        )
        self.assertEqual(result.acknowledged_rule_ids, ("report-pipeline",))

    def test_missing_document_and_acknowledgement_fails(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        with self.assertRaisesRegex(context_impact.ImpactError, "report-pipeline"):
            self.gate(self.commit("unreviewed report"))

    def test_test_only_change_does_not_trigger(self) -> None:
        self.write("internal/report/catalog_test.go", "package report\n\nfunc TestCatalog() {}\n")
        result = self.gate(self.commit("test only"))
        self.assertEqual(result.impacted_rule_ids, ())

    def test_docs_only_change_does_not_trigger(self) -> None:
        self.write("docs/knowledge/reports.md", "# Reports\n\nEditorial clarification.\n")
        result = self.gate(self.commit("docs only"))
        self.assertEqual(result.impacted_rule_ids, ())

    def test_new_source_file_uses_fallback_rule(self) -> None:
        self.write("internal/new_feature.go", "package internal\n")
        head = self.commit("new source")
        result = self.gate(
            head,
            "Context-Reviewed: backend-source\n"
            "Context-Reason: New internal source was reviewed and does not alter documented behavior.",
        )
        self.assertEqual(result.impacted_rule_ids, ("backend-source",))

    def test_every_impacted_rule_must_be_satisfied(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        self.write("internal/worker/report_worker.go", "package worker\n\nconst Lease = 1\n")
        head = self.commit("two subsystems")
        with self.assertRaisesRegex(context_impact.ImpactError, "queue-sml"):
            self.gate(
                head,
                "Context-Reviewed: report-pipeline\n"
                "Context-Reason: Report behavior was reviewed and remains unchanged after refactoring.",
            )

    def test_unknown_acknowledgement_id_fails_closed(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        head = self.commit("report refactor")
        with self.assertRaisesRegex(context_impact.ImpactError, "unknown"):
            self.gate(
                head,
                "Context-Reviewed: report-pipeline, unknown-rule\n"
                "Context-Reason: Internal refactor only; report contracts and behavior are unchanged.",
            )

    def test_short_acknowledgement_reason_fails_closed(self) -> None:
        self.write("internal/report/catalog.go", "package report\n\nconst Version = 2\n")
        head = self.commit("report refactor")
        with self.assertRaisesRegex(context_impact.ImpactError, "20 characters"):
            self.gate(head, "Context-Reviewed: report-pipeline\nContext-Reason: no change")

    def test_invalid_base_reference_fails_closed(self) -> None:
        with self.assertRaisesRegex(context_impact.ImpactError, "base"):
            context_impact.evaluate(
                root=self.root,
                map_path=self.root / "docs/knowledge/context-map.json",
                base="not-a-commit",
                head=self.base,
                pr_body="",
            )

    def test_unsafe_map_path_fails_validation(self) -> None:
        map_path = self.root / "docs/knowledge/context-map.json"
        payload = json.loads(map_path.read_text(encoding="utf-8"))
        payload["rules"][0]["sourcePatterns"] = ["../outside/**"]
        map_path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaisesRegex(context_impact.ImpactError, "unsafe"):
            context_impact.validate_repository(root=self.root, map_path=map_path)

    def test_missing_generated_marker_fails_validation(self) -> None:
        with self.assertRaisesRegex(context_impact.ImpactError, "markers"):
            context_impact.validate_repository(
                root=self.root,
                map_path=self.root / "docs/knowledge/context-map.json",
                marker_pairs=(("docs/knowledge/reports.md", "<!-- START -->", "<!-- END -->"),),
            )

    def test_sensitive_customer_context_is_detected(self) -> None:
        samples = {
            "UUID": "123e4567-e89b-42d3-a456-426614174000",
            "token-like value": "sk-abcdefghijklmnopqrstuvwxyz123456",
            "customer IP address": "10.20.30.40:8092",
            "customer dynamic endpoint": "http://customer.thddns.com:8080",
        }
        for expected, sample in samples.items():
            with self.subTest(expected=expected):
                self.assertIn(expected, context_impact.sensitive_context_labels(sample))


if __name__ == "__main__":
    unittest.main()
