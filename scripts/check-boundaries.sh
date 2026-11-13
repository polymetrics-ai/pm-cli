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
for module, cgo in (("application", "1"), ("tools", "0")):
    selected = packages(module, cgo)
    counts[module] = len(selected)
    for package in selected:
        path = package["ImportPath"]
        if within(path, store_root):
            store_violations += [(path, p) for p in package.get("Deps", [])
                                 if any(within(p, c) for c in consumer_roots)]
        native = [p for p in package.get("Deps", [])
                  if p.startswith("github.com/duckdb/")]
        allowed = path in native_owners
        if native and not allowed:
            raise SystemExit(f"Native analytics outside qualification child: {path}: {native}")
        if package.get("CgoFiles") and not allowed:
            raise SystemExit(f"Direct cgo outside qualification child: {path}")
if store_violations:
    for store, consumer in store_violations:
        print(f"Store depends on a consumer package: {store} -> {consumer}", file=sys.stderr)
    sys.exit(1)
print(json.dumps({"native_import_boundary": "PASS", "store_dependency_boundary": "PASS",
                  "actual_packages": counts}))
PY
