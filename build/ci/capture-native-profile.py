#!/usr/bin/env python3
"""Capture build/native-environment.json from inside the pinned CI image.

Usage (on a host with Podman or Docker; writes the profile to stdout):

    sh scripts/ci/in-ci-image.sh run capture -- \
      python3 build/ci/capture-native-profile.py > build/native-environment.json

The profiled trees are the image's /usr/include and multiarch library
directory. Symlinks in those trees that leave them add their final target as a
profiled file, as bootstrap requires. Review and commit the result. CI never
refreshes the profile; tools/bootstrap.py verifies it before native builds.
"""
import json
import os
from pathlib import Path
import runpy
import sys

ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = runpy.run_path(str(ROOT / "tools/bootstrap.py"))
MULTIARCH = {"linux-amd64": "x86_64-linux-gnu", "linux-arm64": "aarch64-linux-gnu"}
LOADERS = {"linux-amd64": "/lib64/ld-linux-x86-64.so.2",
           "linux-arm64": "/lib/ld-linux-aarch64.so.1"}


def inside(path, roots):
    return any(path == root or path.startswith(root + "/") for root in roots)


def outside_targets(trees):
    """Return regular files that tree symlinks resolve to outside the trees."""
    targets = set()
    for tree in trees:
        for current, directories, files in os.walk(tree):
            for name in directories + files:
                path = os.path.join(current, name)
                if not os.path.islink(path):
                    continue
                target = os.path.realpath(path)
                if inside(target, trees):
                    continue
                if not os.path.isfile(target):
                    raise SystemExit(f"tree symlink leaves the trees for a non-file: {path}")
                targets.add(target)
    return targets


def main():
    platform = BOOTSTRAP["_selected_platform"]()
    if platform not in MULTIARCH:
        raise SystemExit(f"native profiles are Linux only, not {platform}")
    library = f"/usr/lib/{MULTIARCH[platform]}"
    trees = ["/usr/include", library]
    files = {"/etc/os-release", "/usr/lib/os-release", LOADERS[platform],
             f"{library}/libgcc_s.so.1", f"{library}/libstdc++.so.6"}
    files |= outside_targets(trees)
    lock = BOOTSTRAP["_load_lock"]()
    clang = BOOTSTRAP["_find_asset"](lock, "clang", platform)
    linker = {"asset_id": "clang",
              "executable": str(BOOTSTRAP["_clang_linker_relative"](clang)),
              "source_sha256": clang["sha256"]}
    profile = BOOTSTRAP["capture_native_environment"](
        Path("/"), platform, None, trees, sorted(files), linker)
    BOOTSTRAP["verify_native_environment"](Path("/"), profile)
    sys.stdout.write(json.dumps(profile, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
