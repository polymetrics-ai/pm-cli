#!/usr/bin/env python3
"""Measure one cold-output-cache and one warm Bazel build of identical targets."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time


ROOT = Path(__file__).resolve().parents[2]
bootstrap = [sys.executable, str(ROOT / "tools/bootstrap.py"), "exec", "bazelisk", "--"]
resolved = subprocess.run(bootstrap + ["--batch", "info", "output_base"], cwd=ROOT,
                          check=True, capture_output=True, text=True, timeout=180)
base = Path(resolved.stdout.strip())
if not base.is_absolute() or not base.parent.is_dir():
    raise ValueError("Bazel did not return an available absolute output base")
cache = base.parent.resolve()
if cache.is_relative_to(ROOT):
    raise ValueError("benchmark output cache must be outside the source workspace")
results = []
with tempfile.TemporaryDirectory(prefix="foundation-benchmark-", dir=cache) as directory:
    output = Path(directory) / "output"
    command = bootstrap + ["--batch", f"--output_base={output}", "build", "//...",
               f"--symlink_prefix={directory}/links/"]
    for state in ("cold-output-cache", "warm-output-cache"):
        print(f"Measuring {state}", flush=True)
        start = time.monotonic()
        result = subprocess.run(command, cwd=ROOT, check=False, timeout=1200)
        elapsed = time.monotonic() - start
        results.append({"state": state, "seconds": elapsed, "exit": result.returncode})
        if result.returncode:
            print(json.dumps({"measurements": results, "complete": False}))
            raise SystemExit(result.returncode)
print(json.dumps({
    "measurements": results,
    "complete": True,
    "targets": "//...",
    "conditions": "Same source, targets and output base; fresh Bazel output/action cache for first build. Tool and repository-download caches may already be warm. One sample each, not a statistical speedup claim.",
}))
