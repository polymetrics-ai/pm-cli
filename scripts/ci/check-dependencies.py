#!/usr/bin/env python3
"""Inventory selected tool installs and Bazel module sources, then check both Go graphs."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON object key: {key}")
        result[key] = value
    return result


ROOT = Path(__file__).resolve().parents[2]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--evidence-dir", type=Path, required=True)
args = parser.parse_args()
args.evidence_dir = args.evidence_dir.resolve()
args.evidence_dir.mkdir(parents=True, exist_ok=True)
bootstrap = [sys.executable, str(ROOT / "tools/bootstrap.py")]
result = subprocess.run(bootstrap + ["print-env"], cwd=ROOT, check=True,
                        capture_output=True, text=True, timeout=900)
tools = json.loads(result.stdout, object_pairs_hook=unique_object)
sources = tools["ASSET_SOURCES"]
if not isinstance(sources, list) or not 1 <= len(sources) <= 32:
    raise ValueError("missing or unbounded selected tool sources")
lock = json.loads((ROOT / "build/toolchains.lock.json").read_text(),
                  object_pairs_hook=unique_object)
pins = {(asset["id"], asset["version"], asset["sha256"]) for asset in lock["assets"]}
seen = set()
policy = bootstrap + ["exec", "go", "--", "-C", "tools", "run", "./cmd/pm-dependency-check",
                      "--root", str(ROOT), "--versions", str(ROOT / "tools/versions.json"),
                      "--toolchains", str(ROOT / "build/toolchains.lock.json")]
modules = lock["modules"]
if not isinstance(modules, list) or not 1 <= len(modules) <= 32:
    raise ValueError("missing or unbounded selected Bazel modules")
locked_modules = {}
for module in modules:
    key = f"{module['name']}@{module['version']}"
    if key in locked_modules:
        raise ValueError(f"duplicate selected Bazel module identity: {key}")
    locked_modules[key] = module

# Ask Bazel for the resolved repository rules rather than inferring cache names.
# The returned canonical repositories are the exact source trees selected by
# MODULE.bazel and MODULE.bazel.lock.
repository_metadata = subprocess.run(
    bootstrap + ["exec", "bazelisk", "--", "--batch", "mod", "show_repo",
                 *locked_modules, "--output=streamed_jsonproto", "--lockfile_mode=error"],
    cwd=ROOT, check=True, capture_output=True, text=True, timeout=180,
)
if len(repository_metadata.stdout.encode()) > 4 * 1024 * 1024:
    raise ValueError("Bazel repository metadata exceeds the bounded response size")
repositories = {}
canonical_names = set()
for line in repository_metadata.stdout.splitlines():
    if not line.strip():
        continue
    row = json.loads(line, object_pairs_hook=unique_object)
    key = row.get("moduleKey")
    if key not in locked_modules or key in repositories:
        raise ValueError(f"unexpected or duplicated selected Bazel repository: {key}")
    if row.get("repoRuleName") != "http_archive":
        raise ValueError(f"selected Bazel module {key} is not backed by an archive")
    canonical = row.get("canonicalName")
    if (not isinstance(canonical, str) or not canonical or canonical in {".", ".."}
            or "/" in canonical or "\\" in canonical or canonical in canonical_names):
        raise ValueError(f"invalid or duplicated canonical Bazel repository for {key}")
    attributes = {}
    for attribute in row.get("attribute", []):
        name = attribute.get("name")
        if not isinstance(name, str) or not name or name in attributes:
            raise ValueError(f"invalid or duplicated Bazel repository attribute for {key}")
        attributes[name] = attribute
    module = locked_modules[key]
    integrity = attributes.get("integrity", {}).get("stringValue")
    urls = attributes.get("urls", {}).get("stringListValue")
    if integrity != module["integrity"] or urls != [module["url"]]:
        raise ValueError(f"resolved Bazel source metadata differs from lock for {key}")
    canonical_names.add(canonical)
    repositories[key] = canonical
if set(repositories) != set(locked_modules):
    raise ValueError("Bazel omitted selected module repository metadata")

fetch = bootstrap + ["exec", "bazelisk", "--", "--batch", "fetch",
                     "--lockfile_mode=error"]
fetch.extend(f"--repo=@@{repositories[key]}" for key in locked_modules)
subprocess.run(fetch, cwd=ROOT, check=True, timeout=600)
output_base_result = subprocess.run(
    bootstrap + ["exec", "bazelisk", "--", "--batch", "info", "output_base"],
    cwd=ROOT, check=True, capture_output=True, text=True, timeout=180,
)
output_base = Path(output_base_result.stdout.strip())
external_root = output_base / "external"
if not output_base.is_absolute() or not external_root.is_dir():
    raise ValueError("Bazel did not return an available absolute external repository root")
module_sources = []
for key, module in locked_modules.items():
    selected_path = external_root / repositories[key]
    if not selected_path.is_dir():
        raise ValueError(f"selected Bazel module source is unavailable: {key}")
    path = selected_path.resolve(strict=True)
    if not path.is_dir():
        raise ValueError(f"selected Bazel module source is not a directory: {key}")
    module_sources.append({
        "path": path,
        "selected_path": selected_path,
        "name": module["name"],
        "version": module["version"],
        "integrity": module["integrity"],
        "url": module["url"],
    })
sbom_paths = []
# Keep Syft's default directory package catalogers, but inventory every selected
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
    # Scan only the source target of each canonical link in Bazel's current
    # output base. The external-repository root and content cache may contain
    # historical entries and are deliberately never scanned wholesale.
    # Package rows come only from Syft's recognition of these selected source
    # trees; never synthesize them from lock or Bazel graph metadata.
    for index, source in enumerate(module_sources):
        path = source["path"]
        print(
            f"Inventorying selected Bazel module {source['name']}@{source['version']} "
            f"from {source['selected_path']} -> {path} "
            f"(source {source['integrity']} {source['url']})",
            file=sys.stderr,
            flush=True,
        )
        destination = args.evidence_dir / f"bazel-module-{index}.cdx.json"
        subprocess.run(
            bootstrap + ["exec", "syft", "--", str(path),
                         "--from", "dir",
                         "--source-name", source["name"],
                         "--source-version", source["version"],
                         "--parallelism", "2",
                         "-o", f"cyclonedx-json={destination}"],
            cwd=ROOT,
            check=True,
            env=scan_environment,
            timeout=600,
        )
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
