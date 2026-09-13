#!/usr/bin/env python3
"""Validate the PM architecture documentation's executable data contracts."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path, PurePosixPath
from typing import Any

from jsonschema import Draft202012Validator

CONTEXT_PREFIX = "docs/architecture/pm-server/"
CONTEXT_DIRECTORY = PurePosixPath(CONTEXT_PREFIX)
MANIFEST_NAME = "IMPORT-MANIFEST.json"
# The published schemas intentionally share this fixed URN namespace. This
# lexical check is separate from jsonschema's optional format validation.
SCHEMA_ID = re.compile(r"urn:pm:remote%20design:[A-Za-z0-9][A-Za-z0-9._-]*\Z")
MARKDOWN_LINK = re.compile(r"\[[^\]]+\]\(([^)]+)\)")
LOCAL_ISSUE_REFERENCE = re.compile(r"(?<![A-Za-z0-9_])#46(?![A-Za-z0-9_])")
PMCLI_ISSUE_URL = re.compile(
    r"""https://github\.com/polymetrics-ai/pm-cli/issues/46(?=$|[\s/?#.,;:!?)\]}>\"'])"""
)

# source-ledger.schema.json is historical and deliberately does not validate
# reference/SOURCE-LEDGER.json, the sanitized pm.context.sources/v1 projection.
SCHEMA_DOCUMENTS = {
    "architecture-delta.schema.json": "architecture/ARCHITECTURE-DELTA.json",
    "client-parity.schema.json": "architecture/CLIENT-PARITY.json",
    "contracts.schema.json": "architecture/CONTRACTS.json",
    "file-ownership.schema.json": "implementation/FILE-OWNERSHIP.json",
    "foundation-mappings.schema.json": "architecture/FOUNDATION-MAPPINGS.json",
    "foundations.schema.json": "architecture/FOUNDATIONS.json",
    "milestones.schema.json": "implementation/MILESTONES.json",
    "state-ownership.schema.json": "architecture/STATE-OWNERSHIP.json",
    "technology.schema.json": "architecture/TECHNOLOGY.json",
    "validation-matrix.schema.json": "implementation/VALIDATION-MATRIX.json",
}


class ValidationError(ValueError):
    """An observable architecture-documentation contract failure."""


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise ValidationError(f"invalid JSON at {path}: {error}") from error


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValidationError(message)


def context_root(repository: Path) -> Path:
    root = repository / CONTEXT_DIRECTORY
    require(root.is_dir() and not root.is_symlink(), f"missing regular context root: {root}")
    return root


def target_path(repository: Path, target: str) -> Path:
    relative = PurePosixPath(target)
    require(not relative.is_absolute(), f"absolute manifest target: {target}")
    require(".." not in relative.parts, f"escaping manifest target: {target}")
    require(target.startswith(CONTEXT_PREFIX), f"out-of-scope manifest target: {target}")
    path = repository.joinpath(*relative.parts)
    try:
        path.resolve(strict=True).relative_to(context_root(repository).resolve())
    except (OSError, ValueError) as error:
        raise ValidationError(f"manifest target escapes context root: {target}") from error
    return path


def validate_manifest(repository: Path) -> None:
    root = context_root(repository)
    manifest_path = root / MANIFEST_NAME
    require(manifest_path.is_file() and not manifest_path.is_symlink(), "missing regular compact manifest")
    manifest = load_json(manifest_path)
    require(manifest.get("schema_version") == "pm.context.import/v1", "unexpected compact manifest schema")
    require(manifest.get("target_prefix") == CONTEXT_PREFIX, "unexpected compact manifest target prefix")
    rows = manifest.get("files")
    require(isinstance(rows, list) and rows, "compact manifest has no file rows")

    expected: set[str] = set()
    for row in rows:
        require(isinstance(row, dict), "compact manifest row is not an object")
        target = row.get("target")
        expected_hash = row.get("sha256")
        source_hash = row.get("source_sha256")
        transformation = row.get("transformation")
        require(isinstance(target, str), "compact manifest row has no target")
        require(target not in expected, f"duplicate compact manifest target: {target}")
        require(isinstance(expected_hash, str) and re.fullmatch(r"[0-9a-f]{64}", expected_hash) is not None,
                f"invalid payload digest for {target}")
        require(source_hash is None or (
            isinstance(source_hash, str) and re.fullmatch(r"[0-9a-f]{64}", source_hash) is not None
        ), f"invalid source digest for {target}")
        require(isinstance(transformation, str) and transformation.strip(), f"missing transformation for {target}")
        path = target_path(repository, target)
        require(path.is_file() and not path.is_symlink(), f"missing regular payload: {target}")
        require(digest(path) == expected_hash, f"payload digest mismatch: {target}")
        if target.endswith(".json"):
            load_json(path)
        if source_hash is not None and source_hash != expected_hash and target.endswith(".schema.json"):
            require("URI" in transformation and "%20" in transformation,
                    f"changed schema transformation omits URI escaping disclosure: {target}")
        expected.add(target)

    for path in root.rglob("*"):
        require(not path.is_symlink(), f"symlinked context content: {path.relative_to(repository)}")
    actual = {
        path.relative_to(repository).as_posix()
        for path in root.rglob("*")
        if path.is_file()
    }
    expected.add(f"{CONTEXT_PREFIX}{MANIFEST_NAME}")
    require(actual == expected, "compact manifest file set differs from context tree")


def validate_schema_identifier(schema_path: Path, schema: dict[str, Any]) -> None:
    identifier = schema.get("$id")
    require(isinstance(identifier, str) and SCHEMA_ID.fullmatch(identifier) is not None,
            f"invalid PM architecture schema URI identifier in {schema_path}: {identifier!r}")


def validate_schema_documents(repository: Path) -> None:
    root = context_root(repository)
    schemas = root / "schemas"
    for schema_name, document_name in SCHEMA_DOCUMENTS.items():
        schema_path = schemas / schema_name
        schema = load_json(schema_path)
        require(isinstance(schema, dict), f"schema is not an object: {schema_path}")
        validate_schema_identifier(schema_path, schema)
        try:
            Draft202012Validator.check_schema(schema)
        except Exception as error:  # jsonschema supplies several schema-specific subclasses.
            raise ValidationError(f"invalid Draft 2020-12 schema {schema_path}: {error}") from error
        document_path = root / document_name
        document = load_json(document_path)
        errors = sorted(Draft202012Validator(schema).iter_errors(document), key=lambda error: list(error.path))
        if errors:
            raise ValidationError(f"schema validation failed for {document_path}: {errors[0].message}")

    historical_path = schemas / "source-ledger.schema.json"
    historical_schema = load_json(historical_path)
    require(isinstance(historical_schema, dict), "historical source ledger schema is not an object")
    validate_schema_identifier(historical_path, historical_schema)
    Draft202012Validator.check_schema(historical_schema)
    description = historical_schema.get("description")
    require(isinstance(description, str) and "does not apply" in description,
            "historical source ledger non-applicability disclosure is missing")
    projection = load_json(root / "reference/SOURCE-LEDGER.json")
    errors = list(Draft202012Validator(historical_schema).iter_errors(projection))
    require(errors, "historical source ledger schema unexpectedly validates sanitized projection")


def validate_readme_links(repository: Path) -> None:
    root = context_root(repository)
    readme = (root / "README.md").read_text()
    for raw_target in MARKDOWN_LINK.findall(readme):
        target = raw_target.strip()
        if target.startswith(("https://", "http://", "mailto:", "#")):
            continue
        path_part = target.split("#", 1)[0]
        if not path_part:
            continue
        relative = PurePosixPath(path_part)
        require(not relative.is_absolute() and ".." not in relative.parts,
                f"README link escapes context: {target}")
        candidate = root.joinpath(*relative.parts)
        require(candidate.is_file() and not candidate.is_symlink(), f"broken README-local link: {target}")


def validate_pr_issue_link(body: str) -> None:
    require(LOCAL_ISSUE_REFERENCE.search(body) is not None or PMCLI_ISSUE_URL.search(body) is not None,
            "pull request body lacks architecture-publication issue linkage to #46")


def validate_pull_request_event(event_path: Path) -> None:
    event = load_json(event_path)
    pull_request = event.get("pull_request") if isinstance(event, dict) else None
    require(isinstance(pull_request, dict), "workflow event is not a pull request")
    body = pull_request.get("body")
    require(isinstance(body, str), "pull request body is unavailable")
    validate_pr_issue_link(body)


def validate_tree(repository: Path) -> None:
    validate_manifest(repository)
    validate_schema_documents(repository)
    validate_readme_links(repository)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd(), help="repository root")
    parser.add_argument("--pr-event", type=Path, help="GitHub pull_request event JSON")
    args = parser.parse_args()
    repository = args.root.resolve()
    try:
        validate_tree(repository)
        if args.pr_event is not None:
            validate_pull_request_event(args.pr_event)
    except ValidationError as error:
        parser.error(str(error))
    print("architecture documentation contracts passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
