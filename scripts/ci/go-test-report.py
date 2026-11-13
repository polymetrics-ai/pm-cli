#!/usr/bin/env python3
"""Run `go test -json`, keep the event stream and print a short summary.

Usage: go-test-report.py --output FILE [--scenarios] [--summary-file FILE] -- COMMAND...

COMMAND must print `go test -json` events. The summary lists each package
with its result and elapsed time (input for shard balancing) and repeats the
output of failed tests and packages. The exit status is the command's status,
or 1 when the stream records a failure the command did not report.

With --scenarios, the summary also lists each godog scenario with its result
and repeats the godog totals (`N scenarios (...)`, `N steps (...)`), also when
the suite passes. A passing run without godog totals, or with a scenario count
that differs from the godog total, fails. --summary-file appends the same
report as Markdown to FILE (for example $GITHUB_STEP_SUMMARY).
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys

# godog colors its output. The escape byte can be absent from the event text,
# so the color codes match with and without it.
ANSI = re.compile(r"\x1b?\[[0-9;]*m")
SCENARIO = re.compile(r"^\s*Scenario(?: Outline)?:\s*(.*?)\s+#\s+(\S+)\s*$")
TOTAL = re.compile(r"^(\d+) (scenarios?|steps?) \(.*\)$|^No (scenarios|steps)$")


def scenario_report(order, results, output):
    """Return the scenario rows, the godog total lines and their test."""
    totals, suite = [], None
    for key in order:
        for text in output.get(key, []):
            line = ANSI.sub("", text).strip()
            if TOTAL.match(line):
                totals.append(line)
                suite = key
    rows = []
    if suite is None:
        return rows, totals
    prefix = suite[1] + "/" if suite[1] else None
    for key in order:
        if prefix is None or key[0] != suite[0] or not key[1] or not key[1].startswith(prefix):
            continue
        if key not in results:
            continue
        name, location = key[1][len(prefix):].replace("_", " "), ""
        for text in output.get(key, []):
            match = SCENARIO.match(ANSI.sub("", text))
            if match:
                name, location = match.group(1), match.group(2)
                break
        rows.append((results[key], name, location))
    return rows, totals


def scenario_problem(rows, totals):
    """Describe a missing or inconsistent godog summary, or return None."""
    counts = []
    for line in totals:
        match = TOTAL.match(line)
        if match and match.group(2) and match.group(2).startswith("scenario"):
            counts.append(int(match.group(1)))
        elif match and match.group(3) == "scenarios":
            counts.append(0)
    if len(counts) != 1:
        return "the godog scenario total is missing" if not counts else "godog printed more than one scenario total"
    if counts[0] != len(rows):
        return f"godog reports {counts[0]} scenarios, the test stream has {len(rows)}"
    return None


def markdown(rows, totals, problem):
    lines = ["### Gherkin scenarios", ""]
    lines += [f"{line}  " for line in totals] or ["No godog totals were found.  "]
    if problem:
        lines.append(f"Summary error: {problem}.  ")
    lines.append("")
    if rows:
        lines += ["| Result | Scenario | Location |", "| --- | --- | --- |"]
        for action, name, location in rows:
            cell = name.replace("|", "\\|")
            lines.append(f"| {action.upper()} | {cell} | `{location}` |" if location
                         else f"| {action.upper()} | {cell} | |")
        lines.append("")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenarios", action="store_true",
                        help="list each godog scenario and the godog totals")
    parser.add_argument("--summary-file", type=Path,
                        help="append the scenario report as Markdown (requires --scenarios)")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("missing go test command")
    if args.summary_file and not args.scenarios:
        parser.error("--summary-file requires --scenarios")
    with args.output.open("wb") as stream:
        status = subprocess.run(command, stdout=stream, check=False).returncode
    packages, failures, output = {}, [], {}
    order, results = [], {}
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
        if key not in output:
            order.append(key)
            output[key] = []
        if action in ("output", "build-output"):
            output[key].append(event.get("Output", ""))
        elif action in ("pass", "fail", "skip") and key[1] is None:
            packages[key[0]] = (action, float(event.get("Elapsed") or 0))
        if action in ("pass", "fail", "skip") and key[1] is not None:
            results[key] = action
        if action in ("fail", "build-fail"):
            failures.append(key)
    for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
        print(f"{action.upper():4} {elapsed:8.1f}s {package}")
    for key in failures:
        name = key[0] + (f" {key[1]}" if key[1] else "")
        print(f"::group::FAIL {name}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
    problem = None
    if args.scenarios:
        rows, totals = scenario_report(order, results, output)
        problem = scenario_problem(rows, totals)
        print("Scenarios:")
        for action, name, location in rows:
            print(f"{action.upper():4} {name}" + (f" ({location})" if location else ""))
        for line in totals:
            print(line)
        if problem:
            print(f"go test: scenario summary error: {problem}")
        if args.summary_file:
            with args.summary_file.open("a", encoding="utf-8") as summary:
                summary.write(markdown(rows, totals, problem))
    print(f"go test: {len(packages)} packages, {len(failures)} failures, exit {status}")
    return status or (1 if failures or problem else 0)


if __name__ == "__main__":
    sys.exit(main())
