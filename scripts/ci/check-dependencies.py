#!/usr/bin/env python3
"""Inventory exact selected external-tool installs and check both Go module graphs."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--evidence-dir", type=Path, required=True)
args = parser.parse_args()
args.evidence_dir = args.evidence_dir.resolve()
args.evidence_dir.mkdir(parents=True, exist_ok=True)
bootstrap = [sys.executable, str(ROOT / "tools/bootstrap.py")]
result = subprocess.run(bootstrap + ["print-env"], cwd=ROOT, check=True,
                        capture_output=True, text=True, timeout=900)
tools = json.loads(result.stdout)
sources = tools["ASSET_SOURCES"]
if not isinstance(sources, list) or not 1 <= len(sources) <= 32:
    raise ValueError("missing or unbounded selected tool sources")
lock = json.loads((ROOT / "build/toolchains.lock.json").read_text())
pins = {(asset["id"], asset["version"], asset["sha256"]) for asset in lock["assets"]}
seen = set()
policy = bootstrap + ["exec", "go", "--", "-C", "tools", "run", "./cmd/pm-dependency-check",
                      "--root", str(ROOT), "--versions", str(ROOT / "tools/versions.json"),
                      "--toolchains", str(ROOT / "build/toolchains.lock.json")]
sbom_paths = []
# Keep Syft's default directory package catalogers, but inventory every installed
# file. Keep unversioned source-root records; never drop rows to obtain a pass.
# The explicit empty config isolates scans from ambient user configuration.
with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", encoding="utf-8") as syft_config:
    syft_config.write("{}\n")
    syft_config.flush()
    scan_environment = {
        key: value for key, value in os.environ.items() if not key.startswith("SYFT_")
    }
    scan_environment.update(
        SYFT_CHECK_FOR_APP_UPDATE="false",
        SYFT_COMPLIANCE_MISSING_VERSION="stub",
        SYFT_CONFIG=syft_config.name,
        SYFT_FILE_METADATA_DIGESTS="sha256",
        SYFT_FILE_METADATA_SELECTION="all",
    )
    for index, source in enumerate(sources):
        identity = (source["id"], source["version"], source["sha256"])
        if identity not in pins or identity in seen:
            raise ValueError("unlocked or duplicated selected tool identity")
        seen.add(identity)
        path = Path(source["path"])
        if not path.is_absolute() or not path.is_dir():
            raise ValueError("selected tool install root is not an available directory")
        if source["id"] == "bazel":
            # Bazel's pinned self-extracting distribution owns the embedded JDK
            # and build tools. Scanning its launcher alone misses that runtime.
            expanded = subprocess.run(
                bootstrap + ["exec", "bazelisk", "--", "--batch", "info", "install_base"],
                cwd=ROOT, check=True, capture_output=True, text=True, timeout=180,
            )
            path = Path(expanded.stdout.strip())
            if not path.is_absolute() or not path.is_dir():
                raise ValueError("Bazel did not return an available absolute install base")
        destination = args.evidence_dir / f"tool-{index}.cdx.json"
        subprocess.run(bootstrap + ["exec", "syft", "--", str(path),
                                   "--from", "dir",
                                   "--source-name", source["id"], "--source-version", source["version"],
                                   "--parallelism", "2",
                                   "-o", f"cyclonedx-json={destination}"], cwd=ROOT, check=True,
                       env=scan_environment, timeout=600)
        sbom_paths.append(str(destination))
        policy.extend(["--sbom", str(destination)])
policy_environment = dict(os.environ, CGO_ENABLED="0")
subprocess.run(policy, cwd=ROOT, check=True, env=policy_environment, timeout=600)
test_environment = dict(
    policy_environment,
    PM_SBOMS=os.pathsep.join(sbom_paths),
)
subprocess.run(
    bootstrap + ["exec", "go", "--", "-C", "tools", "test", ".",
                 "-run", "^TestResolvedDependencyPolicy$", "-count=1", "-timeout", "5m"],
    cwd=ROOT,
    check=True,
    env=test_environment,
    timeout=600,
)
