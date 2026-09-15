#!/usr/bin/env python3
"""Behavioral regression tests for the architecture documentation validator."""

from __future__ import annotations

import json
import hashlib
import shutil
import tempfile
import unittest
from pathlib import Path

from validate_architecture_docs import (
    ValidationError,
    validate_manifest,
    validate_pr_issue_links,
    validate_pull_request_event,
    validate_schema_identifier,
    validate_tree,
)


REPOSITORY = Path(__file__).resolve().parents[1]
CONTEXT = Path("docs/architecture/pm-server")


class ArchitectureDocumentationValidatorTests(unittest.TestCase):
    def copy_repository(self) -> tuple[tempfile.TemporaryDirectory[str], Path]:
        temporary = tempfile.TemporaryDirectory()
        root = Path(temporary.name)
        shutil.copytree(REPOSITORY / "docs", root / "docs")
        return temporary, root

    def validate_event_payload(self, payload: object) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            event = Path(temporary) / "event.json"
            event.write_text(json.dumps(payload))
            validate_pull_request_event(event)

    def test_current_architecture_tree_is_valid(self) -> None:
        validate_tree(REPOSITORY)

    def test_rejects_literal_space_in_schema_identifier(self) -> None:
        schema_path = REPOSITORY / CONTEXT / "schemas/architecture-delta.schema.json"
        schema = {"$id": "urn:pm:remote design:architecture-delta"}
        with self.assertRaisesRegex(ValidationError, "invalid PM architecture schema URI identifier"):
            validate_schema_identifier(schema_path, schema)

    def test_rejects_tampered_manifest_payload(self) -> None:
        temporary, root = self.copy_repository()
        self.addCleanup(temporary.cleanup)
        manifest_path = root / CONTEXT / "IMPORT-MANIFEST.json"
        manifest = json.loads(manifest_path.read_text())
        for row in manifest["files"]:
            if row["target"].endswith("source-ledger.schema.json"):
                row["transformation"] += "; URI %20 disclosure fixture"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        readme = root / CONTEXT / "README.md"
        readme.write_text(readme.read_text() + "\ntampered\n")
        with self.assertRaisesRegex(ValidationError, "payload digest mismatch"):
            validate_manifest(root)

    def test_rejects_malformed_inventoried_json_after_digest_refresh(self) -> None:
        temporary, root = self.copy_repository()
        self.addCleanup(temporary.cleanup)
        payload = root / CONTEXT / "implementation/PACKET-CONTRACTS.json"
        payload.write_text("{ malformed JSON\\n")
        manifest_path = root / CONTEXT / "IMPORT-MANIFEST.json"
        manifest = json.loads(manifest_path.read_text())
        for row in manifest["files"]:
            if row["target"].endswith("source-ledger.schema.json"):
                row["transformation"] += "; URI %20 disclosure fixture"
            if row["target"].endswith("implementation/PACKET-CONTRACTS.json"):
                row["sha256"] = hashlib.sha256(payload.read_bytes()).hexdigest()
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        with self.assertRaisesRegex(ValidationError, "invalid JSON"):
            validate_manifest(root)

    def test_publication_pull_request_accepts_issue_46(self) -> None:
        for body in (
            "Refs #46",
            "Refs https://github.com/polymetrics-ai/pm-cli/issues/46?state=open.",
        ):
            with self.subTest(body=body):
                validate_pr_issue_links(47, body)

    def test_publication_pull_request_rejects_other_issue(self) -> None:
        with self.assertRaises(ValidationError):
            validate_pr_issue_links(47, "Refs #2")

    def test_later_pull_request_accepts_actual_issue_without_46(self) -> None:
        for body in (
            "Refs #2",
            "Refs https://github.com/polymetrics-ai/pm-cli/issues/2?state=open.",
            "[Tracked issue](https://github.com/polymetrics-ai/pm-cli/issues/2)",
        ):
            with self.subTest(body=body):
                validate_pr_issue_links(48, body)

    def test_grouped_pull_request_accepts_multiple_issue_references(self) -> None:
        validate_pr_issue_links(
            48,
            "Closes #2\nCloses https://github.com/polymetrics-ai/pm-cli/issues/3",
        )

    def test_rejects_missing_or_malformed_issue_references(self) -> None:
        for body in (
            "Documentation-only update",
            "Refs 2",
            "Refs #0",
            "Refs #02",
            "Refs #-2",
            "Refs ##2",
            "Refs #2suffix",
        ):
            with self.subTest(body=body):
                with self.assertRaises(ValidationError):
                    validate_pr_issue_links(48, body)

    def test_rejects_foreign_or_non_issue_url_false_matches(self) -> None:
        for body in (
            "Refs https://github.com/other/project/issues/2",
            "Refs https://github.com/polymetrics-ai/other/issues/2",
            "Refs https://github.com/other/project/issues/9#2",
            "Refs https://github.com/polymetrics-ai/pm-cli/pull/2",
            "Refs https://github.com/polymetrics-ai/pm-cli/issues/2/comments",
            "Refs http://github.com/polymetrics-ai/pm-cli/issues/2",
            "Refs https://github.com.evil.example/polymetrics-ai/pm-cli/issues/2",
        ):
            with self.subTest(body=body):
                with self.assertRaises(ValidationError):
                    validate_pr_issue_links(48, body)

    def test_pull_request_events_apply_identity_specific_issue_policy(self) -> None:
        for payload in (
            {"number": 47, "pull_request": {"number": 47, "body": "Refs #46"}},
            {"number": 48, "pull_request": {"number": 48, "body": "Refs #2"}},
            {
                "number": 49,
                "pull_request": {
                    "number": 49,
                    "body": "Closes #2\nCloses #3",
                },
            },
        ):
            with self.subTest(payload=payload):
                self.validate_event_payload(payload)

        with self.assertRaises(ValidationError):
            self.validate_event_payload(
                {"number": 47, "pull_request": {"number": 47, "body": "Refs #2"}}
            )

    def test_pull_request_event_rejects_malformed_json(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            event = Path(temporary) / "event.json"
            event.write_text("{")
            with self.assertRaises(ValidationError):
                validate_pull_request_event(event)

    def test_pull_request_event_rejects_invalid_identity_and_shape(self) -> None:
        invalid_payloads = (
            None,
            [],
            {},
            {"number": 47, "pull_request": None},
            {"pull_request": {"body": "Refs #46"}},
            {"pull_request": {"number": 47, "body": "Refs #46"}},
            {"number": 47, "pull_request": {"body": "Refs #46"}},
            {
                "number": "47",
                "pull_request": {"number": 47, "body": "Refs #46"},
            },
            {
                "number": 47,
                "pull_request": {"number": "47", "body": "Refs #46"},
            },
            {
                "number": True,
                "pull_request": {"number": True, "body": "Refs #46"},
            },
            {"number": 0, "pull_request": {"number": 0, "body": "Refs #46"}},
            {"number": 48, "pull_request": {"number": 47, "body": "Refs #2"}},
            {"number": 48, "pull_request": {"number": 48, "body": None}},
        )
        for payload in invalid_payloads:
            with self.subTest(payload=payload):
                with self.assertRaises(ValidationError):
                    self.validate_event_payload(payload)



if __name__ == "__main__":
    unittest.main()
