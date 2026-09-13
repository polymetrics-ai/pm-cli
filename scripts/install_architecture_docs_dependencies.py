#!/usr/bin/env python3
"""Verify and unpack pinned documentation-CI wheels into job-owned storage."""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import stat
import zipfile
from pathlib import Path, PurePosixPath


REPOSITORY = Path(__file__).resolve().parents[1]
VENDOR_ROOT = REPOSITORY / "third_party/python/architecture-docs"
MANIFEST_PATH = VENDOR_ROOT / "dependency-manifest.json"


class DependencyError(ValueError):
    """A pinned documentation dependency is missing or unsafe."""


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_manifest() -> dict:
    data = json.loads(MANIFEST_PATH.read_text())
    if data.get("schema_version") != "pm.documentation-ci.dependencies/v1":
        raise DependencyError("unexpected dependency manifest schema")
    wheels = data.get("wheels")
    if not isinstance(wheels, list) or not wheels:
        raise DependencyError("dependency manifest has no wheels")
    return data


def verified_wheels(manifest: dict) -> list[Path]:
    verified: list[Path] = []
    for wheel in manifest["wheels"]:
        filename = wheel.get("filename")
        expected = wheel.get("sha256")
        if not isinstance(filename, str) or PurePosixPath(filename).name != filename:
            raise DependencyError(f"unsafe wheel filename: {filename!r}")
        if not isinstance(expected, str) or len(expected) != 64:
            raise DependencyError(f"invalid wheel digest: {filename}")
        path = VENDOR_ROOT / "wheels" / filename
        if not path.is_file() or path.is_symlink():
            raise DependencyError(f"missing regular wheel: {filename}")
        if digest(path) != expected:
            raise DependencyError(f"wheel digest mismatch: {filename}")
        verified.append(path)
    return verified


def safe_extract(wheel: Path, destination: Path) -> None:
    destination_root = destination.resolve()
    with zipfile.ZipFile(wheel) as archive:
        for member in archive.infolist():
            relative = PurePosixPath(member.filename)
            if relative.is_absolute() or ".." in relative.parts:
                raise DependencyError(f"escaping wheel member: {wheel.name}:{member.filename}")
            mode = member.external_attr >> 16
            if stat.S_ISLNK(mode):
                raise DependencyError(f"symlinked wheel member: {wheel.name}:{member.filename}")
            target = destination.joinpath(*relative.parts)
            try:
                target.resolve(strict=False).relative_to(destination_root)
            except ValueError as error:
                raise DependencyError(f"wheel member escapes destination: {member.filename}") from error
            if member.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(member) as source, target.open("wb") as output:
                shutil.copyfileobj(source, output)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--destination", type=Path, help="job-owned disposable destination")
    parser.add_argument("--verify-only", action="store_true", help="verify vendored wheel hashes without extraction")
    args = parser.parse_args()
    if args.verify_only == (args.destination is not None):
        parser.error("provide exactly one of --verify-only or --destination")
    try:
        wheels = verified_wheels(load_manifest())
        if args.destination is not None:
            destination = args.destination.resolve()
            destination.mkdir(parents=True, exist_ok=True)
            for wheel in wheels:
                safe_extract(wheel, destination)
    except (DependencyError, OSError, zipfile.BadZipFile) as error:
        parser.error(str(error))
    print(f"verified {len(wheels)} pinned documentation wheels")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
