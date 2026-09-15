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
    validate_pr_issue_link,
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

    def test_rejects_missing_issue_linkage(self) -> None:
        with self.assertRaisesRegex(ValidationError, "issue linkage"):
            validate_pr_issue_link("Documentation-only update")

    def test_accepts_issue_linkage(self) -> None:
        validate_pr_issue_link("References #46 for architecture publication.")

    def test_accepts_exact_pmcli_issue_url(self) -> None:
        validate_pr_issue_link(
            "Tracks https://github.com/polymetrics-ai/pm-cli/issues/46?state=open."
        )

    def test_accepts_pmcli_issue_url_prose_and_markdown_boundaries(self) -> None:
        for body in (
            "Tracks https://github.com/polymetrics-ai/pm-cli/issues/46 for this work.",
            "[Architecture issue](https://github.com/polymetrics-ai/pm-cli/issues/46)",
        ):
            with self.subTest(body=body):
                validate_pr_issue_link(body)

    def test_rejects_wrong_issue_numbers(self) -> None:
        for body in (
            "References #460 for architecture publication.",
            "References #146 for architecture publication.",
            "Tracks https://github.com/polymetrics-ai/pm-cli/issues/460.",
        ):
            with self.subTest(body=body):
                with self.assertRaisesRegex(ValidationError, "issue linkage"):
                    validate_pr_issue_link(body)

    def test_rejects_foreign_issue_urls(self) -> None:
        for body in (
            "Tracks https://github.com/other/project/issues/46.",
            "Tracks https://github.com/polymetrics-ai/other/issues/46.",
        ):
            with self.subTest(body=body):
                with self.assertRaisesRegex(ValidationError, "issue linkage"):
                    validate_pr_issue_link(body)

    def test_pull_request_event_requires_issue_linkage(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            event = Path(temporary) / "event.json"
            event.write_text(json.dumps({"pull_request": {"body": "References #46."}}))
            validate_pull_request_event(event)
            event.write_text(json.dumps({"pull_request": {"body": "No issue reference."}}))
            with self.assertRaisesRegex(ValidationError, "issue linkage"):
                validate_pull_request_event(event)



if __name__ == "__main__":
    unittest.main()
