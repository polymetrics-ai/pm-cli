#!/usr/bin/env python3
"""Run `go test -json`, keep the event stream and print a short summary.

Usage: go-test-report.py --output FILE -- COMMAND...

COMMAND must print `go test -json` events. The summary lists each package
with its result and elapsed time (input for shard balancing) and repeats the
output of failed tests and packages. The exit status is the command's status,
or 1 when the stream records a failure the command did not report.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("missing go test command")
    with args.output.open("wb") as stream:
        status = subprocess.run(command, stdout=stream, check=False).returncode
    packages, failures, output = {}, [], {}
    for line in args.output.read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            print(line)
            continue
        if not isinstance(event, dict):
            continue
        action = event.get("Action")
        key = (event.get("Package") or event.get("ImportPath") or "", event.get("Test"))
        if action in ("output", "build-output"):
            output.setdefault(key, []).append(event.get("Output", ""))
        elif action in ("pass", "fail", "skip") and key[1] is None:
            packages[key[0]] = (action, float(event.get("Elapsed") or 0))
        if action in ("fail", "build-fail"):
            failures.append(key)
    for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
        print(f"{action.upper():4} {elapsed:8.1f}s {package}")
    for key in failures:
        name = key[0] + (f" {key[1]}" if key[1] else "")
        print(f"::group::FAIL {name}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
    print(f"go test: {len(packages)} packages, {len(failures)} failures, exit {status}")
    return status or (1 if failures else 0)


if __name__ == "__main__":
    sys.exit(main())
