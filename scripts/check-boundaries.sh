#!/bin/sh
# Inspect actual selected imports, not source-text matches or declared requires.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 - "$repo_root" <<'PY'
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(sys.argv[1])
bootstrap = root / "tools/bootstrap.py"
decoder = json.JSONDecoder()
module_path = "github.com/polymetrics-ai/pm-cli"


def within(path, parent):
    return path == parent or path.startswith(parent + "/")


def tracked_go_symlinks():
    # Mode 120000 is a symbolic link in the git index. A tracked Go file must
    # be a regular file, so that its bytes are the reviewed bytes.
    result = subprocess.run(["git", "ls-files", "-s", "-z", "--", "*.go"], cwd=root,
                            check=True, capture_output=True, text=True, timeout=60)
    links = []
    for entry in result.stdout.split("\0"):
        if entry:
            meta, path = entry.split("\t", 1)
            if meta.split()[0] == "120000":
                links.append(path)
    return links


def packages(module, cgo):
    args = [sys.executable, str(bootstrap), "exec", "go", "--"]
    if module == "tools":
        args += ["-C", "tools"]
    args += ["list", "-deps", "-json", "./..."]
    result = subprocess.run(args, cwd=root, check=True, capture_output=True,
                            text=True, timeout=300,
                            env=dict(os.environ, CGO_ENABLED=cgo, GOWORK="off"))
    text = result.stdout
    values = []
    offset = 0
    while offset < len(text):
        if text[offset].isspace():
            offset += 1
            continue
        value, offset = decoder.raw_decode(text, offset)
        values.append(value)
    selected = [p for p in values if p.get("Module", {}).get("Main")]
    if not selected:
        raise SystemExit(f"No actual {module} packages selected")
    return selected


counts = {}
# ADR475-12: only the restricted analytical child links the native engine. The
# probe qualifies the pinned native identity; internal/analytics owns the engine
# wrapper and cmd/pm-query is the child binary that runs it. Every other
# package, including internal/warehouse, must stay free of native imports.
native_owners = {
    "github.com/polymetrics-ai/pm-cli/build/nativeprobe",
    "github.com/polymetrics-ai/pm-cli/internal/analytics",
    "github.com/polymetrics-ai/pm-cli/cmd/pm-query",
}
# The store is a lower layer. No non-test package under internal/store may
# depend on internal/app, internal/auth or their subpackages, directly or
# through another package. Deps holds the non-test dependencies only, so a
# store test file does not trigger the rule.
store_root = f"{module_path}/internal/store"
consumer_roots = (f"{module_path}/internal/app", f"{module_path}/internal/auth")
store_violations = []
hidden_store_files = []
go_links = tracked_go_symlinks()
if go_links:
    for path in go_links:
        print(f"Tracked Go file is a symbolic link: {path}", file=sys.stderr)
    sys.exit(1)
for module, cgo in (("application", "1"), ("tools", "0")):
    selected = packages(module, cgo)
    counts[module] = len(selected)
    for package in selected:
        path = package["ImportPath"]
        if within(path, store_root):
            store_violations += [(path, p) for p in package.get("Deps", [])
                                 if any(within(p, c) for c in consumer_roots)]
            # A build constraint or an OS or architecture suffix can hide a
            # file from this target, so its imports are not in Deps.
            hidden_store_files += [(path, f) for f in package.get("IgnoredGoFiles", [])
                                   if not f.endswith("_test.go")]
        native = [p for p in package.get("Deps", [])
                  if p.startswith("github.com/duckdb/")]
        allowed = path in native_owners
        if native and not allowed:
            raise SystemExit(f"Native analytics outside qualification child: {path}: {native}")
        if package.get("CgoFiles") and not allowed:
            raise SystemExit(f"Direct cgo outside qualification child: {path}")
if store_violations or hidden_store_files:
    for store, consumer in store_violations:
        print(f"Store depends on a consumer package: {store} -> {consumer}", file=sys.stderr)
    for store, name in hidden_store_files:
        print(f"Store file is outside the checked build: {store}: {name}", file=sys.stderr)
    sys.exit(1)
print(json.dumps({"native_import_boundary": "PASS", "store_dependency_boundary": "PASS",
                  "store_build_coverage": "PASS", "go_file_symlinks": "PASS",
                  "actual_packages": counts}))
PY
