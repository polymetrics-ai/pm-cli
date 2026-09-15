#!/usr/bin/env python3
"""Run the native boundary, then refuse disabled cgo and an incompatible target."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
cache = ROOT / ".cache"
cache.mkdir(exist_ok=True)
with tempfile.TemporaryDirectory(prefix="native-qualification-", dir=cache) as directory:
    executable = Path(directory) / "nativeprobe"
    command = [sys.executable, str(ROOT / "tools/bootstrap.py"), "exec", "go", "--",
               "build", "-mod=readonly", "-o", str(executable), "./build/nativeprobe"]
    environment = dict(os.environ, CGO_ENABLED="1", GOWORK="off")
    subprocess.run(command, cwd=ROOT, env=environment, check=True, timeout=600)
    subprocess.run([str(executable)], cwd=ROOT, env=environment, check=True, timeout=120)
    compiler_probe = Path(directory) / "compiler-selection"
    compiler_probe.mkdir()
    lock = json.loads((ROOT / "build/toolchains.lock.json").read_text())
    compiler_probe.joinpath("go.mod").write_text(
        f"module pmcli-native-compiler-probe\n\ngo {lock['go_version']}\n"
    )
    compiler_probe.joinpath("main.go").write_text(
        'package main\n/*\nint native_value(void);\n*/\nimport "C"\n'
        'import "fmt"\nfunc main() { fmt.Println(int(C.native_value())) }\n'
    )
    compiler_probe.joinpath("native.cc").write_text(
        'extern "C" int native_value(void) { return 42; }\n'
    )
    compiler_result = subprocess.run(
        [sys.executable, str(ROOT / "tools/bootstrap.py"), "exec", "go", "--",
         "-C", str(compiler_probe), "run", "."],
        cwd=ROOT,
        env=dict(environment, CC="/usr/bin/false", CXX="/usr/bin/false"),
        text=True, capture_output=True, check=True, timeout=600,
    )
    if compiler_result.stdout.strip() != "42":
        raise SystemExit("native compiler selection returned an incorrect C++ result")
    print("Pinned C and C++ child compiler selection PASS", flush=True)
    # Both controls use the now-resolved identical modules without network lookup.
    environment.update(GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
    if sys.platform == "linux":
        # The admitted host profile rejects this target before invoking Go or
        # Clang. Do not claim that a cross-target compiler ran.
        incompatible = ("unqualified 32-bit native Go target", {"GOARCH": "386"})
    else:
        incompatible = ("incompatible 32-bit C target against 64-bit Go",
                        {"CGO_CFLAGS": "--target=i386-unknown-linux-gnu",
                         "CGO_CXXFLAGS": "--target=i386-unknown-linux-gnu"})
    controls = (("cgo disabled", {"CGO_ENABLED": "0"}), incompatible)
    for name, changed in controls:
        print(f"Requiring native build refusal: {name}", flush=True)
        result = subprocess.run(command, cwd=ROOT, env=dict(environment, **changed),
                                check=False, timeout=180)
        if result.returncode == 0:
            raise SystemExit(f"native qualification failed: accepted {name}")
        if result.returncode != 1:
            raise SystemExit(f"native control did not reach a normal build refusal: {name}, exit {result.returncode}")
        print(f"Native refusal PASS: {name}", flush=True)
