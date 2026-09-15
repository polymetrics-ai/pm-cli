#!/usr/bin/env python3
"""Exercise real bootstrap refusal without network or a system tool installation."""
from io import BytesIO
import hashlib
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
bootstrap = runpy.run_path(str(ROOT / "tools/bootstrap.py"))
BootstrapError = bootstrap["BootstrapError"]


class BootstrapBoundaryTest(unittest.TestCase):
    def test_wrong_source_digest_prevents_execution(self):
        cache_parent = ROOT / ".cache"
        cache_parent.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="bootstrap-test-", dir=cache_parent) as directory:
            root = Path(directory)
            (root / "tools").mkdir()
            (root / "build").mkdir()
            (root / "tools/bootstrap.py").write_bytes((ROOT / "tools/bootstrap.py").read_bytes())
            lock = json.loads((ROOT / "build/toolchains.lock.json").read_text())
            platform = bootstrap["_selected_platform"]()
            asset = next(a for a in lock["assets"] if a["id"] == "go" and a["platform"] == platform)
            marker = root / "executed"
            # A synthetic SDK makes any unintended execution observable, not qualified.
            program = b'#!/bin/sh\nprintf executed > "$PM_BOOTSTRAP_TEST_MARKER"\n'
            archive = root / "sdk.tar.gz"
            with tarfile.open(archive, "w:gz") as output:
                entry = tarfile.TarInfo(asset["executable"])
                entry.mode = 0o755
                entry.size = len(program)
                output.addfile(entry, BytesIO(program))
            payload = archive.read_bytes()
            valid_digest = hashlib.sha256(payload).hexdigest()
            cache = root / "cache"
            downloads = cache / "downloads"
            downloads.mkdir(parents=True)
            command = [sys.executable, "-I", "-B", str(root / "tools/bootstrap.py"),
                       "exec", "go", "--", "version"]
            environment = dict(os.environ, PM_TOOLS_CACHE=str(cache), CGO_ENABLED="0",
                               PM_BOOTSTRAP_TEST_MARKER=str(marker))
            for digest, accepted in ((valid_digest, True), ("0" * 64, False)):
                with self.subTest(accepted=accepted):
                    marker.unlink(missing_ok=True)
                    asset["sha256"] = digest
                    (root / "build/toolchains.lock.json").write_text(json.dumps(lock))
                    (downloads / f"{digest}.blob").write_bytes(payload)
                    result = subprocess.run(command, cwd=root, env=environment,
                                            capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0 if accepted else 1,
                                     result.stdout + result.stderr)
                    self.assertEqual(marker.exists(), accepted)

    def test_archive_extraction_cannot_escape_its_destination(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = b"archive bytes"
            for kind in ("tar.gz", "zip"):
                for name, accepted in (("inside", True), ("../escaped", False)):
                    with self.subTest(kind=kind, name=name):
                        archive = root / f"input.{kind}"
                        if kind == "tar.gz":
                            with tarfile.open(archive, "w:gz") as output:
                                entry = tarfile.TarInfo(name)
                                entry.size = len(payload)
                                output.addfile(entry, BytesIO(payload))
                        else:
                            with zipfile.ZipFile(archive, "w") as output:
                                output.writestr(name, payload)
                        destination = root / f"extract-{kind}-{accepted}"
                        if accepted:
                            bootstrap["_extract_archive"](archive, kind, destination)
                            self.assertEqual((destination / name).read_bytes(), payload)
                        else:
                            with self.assertRaises(BootstrapError):
                                bootstrap["_extract_archive"](archive, kind, destination)
                            self.assertFalse((root / "escaped").exists())
                            self.assertFalse(destination.exists())

    def test_unsupported_platform_has_no_fallback(self):
        with self.assertRaises(BootstrapError):
            bootstrap["_normalize_platform"]("windows", "amd64")


if __name__ == "__main__":
    unittest.main()
