#!/usr/bin/env python3
"""Run `go test -json`, keep the event stream and print a short summary.

Usage: go-test-report.py --output FILE [--scenarios] [--title TEXT]
                         [--fail-on-skip] [--summary-file FILE] -- COMMAND...

COMMAND must print `go test -json` events. The summary lists each package
with its result and elapsed time (input for shard balancing) and repeats the
output of failed tests and packages. It also lists each skipped test with its
output. A package-level skip means that the package has no test files. It is
not a test skip. The exit status is the command's status, or 1 when the
stream records a failure the command did not report.

With --fail-on-skip, a skipped test (a test or subtest that calls t.Skip) is a
failure: the exit status is 1, and the log and the summary name the test.

With --title, --summary-file appends a Markdown section with that title: the
totals, each package with its result and elapsed time, and each failed or
skipped test.

With --scenarios, the summary also lists each godog scenario with its result
and repeats the godog totals (`N scenarios (...)`, `N steps (...)`), also when
the suite passes. A passing run without godog totals, or with a scenario count
that differs from the godog total, fails. --summary-file appends the same
report as Markdown to FILE (for example $GITHUB_STEP_SUMMARY).
--summary-file requires --title, --scenarios or both.
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


def package_result(action):
    """Name a package result. A package-level skip means no test files."""
    return "SKIP (no test files)" if action == "skip" else action.upper()


def test_name(key):
    return key[0] + (f" {key[1]}" if key[1] else "")


def package_markdown(title, packages, failures, skips, fail_on_skip, status):
    failed = bool(status or failures or (fail_on_skip and skips))
    lines = [f"### {title}", "", f"Result: {'FAILED' if failed else 'PASSED'}.  "]
    counts = {}
    for action, _ in packages.values():
        counts[action] = counts.get(action, 0) + 1
    lines.append(
        f"{len(packages)} packages: {counts.get('pass', 0)} passed, {counts.get('fail', 0)} failed, "
        f"{counts.get('skip', 0)} with no test files. {len(failures)} failures. "
        f"{len(skips)} skipped tests{' (each one is a failure)' if fail_on_skip and skips else ''}. "
        f"Exit {status}.  ")
    lines.append("")
    if packages:
        lines += ["| Result | Time | Package |", "| --- | --- | --- |"]
        for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
            lines.append(f"| {package_result(action)} | {elapsed:.1f}s | `{package}` |")
        lines.append("")
    for heading, keys in (("Failed", failures), ("Skipped tests", skips)):
        if keys:
            lines += [f"{heading}:", ""]
            lines += [f"- `{test_name(key)}`" for key in keys]
            lines.append("")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenarios", action="store_true",
                        help="list each godog scenario and the godog totals")
    parser.add_argument("--title",
                        help="the title of the package section in the summary file")
    parser.add_argument("--fail-on-skip", action="store_true",
                        help="a skipped test is a failure")
    parser.add_argument("--summary-file", type=Path,
                        help="append the report as Markdown (requires --title or --scenarios)")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("missing go test command")
    if args.summary_file and not (args.scenarios or args.title):
        parser.error("--summary-file requires --title or --scenarios")
    with args.output.open("wb") as stream:
        status = subprocess.run(command, stdout=stream, check=False).returncode
    packages, failures, output = {}, [], {}
    order, results, skips = [], {}, []
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
        if action == "skip" and key[1] is not None:
            skips.append(key)
        if action in ("fail", "build-fail"):
            failures.append(key)
    if args.title:
        print(args.title)
    for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
        print(f"{action.upper():4} {elapsed:8.1f}s {package}" + (" (no test files)" if action == "skip" else ""))
    for key in failures:
        print(f"::group::FAIL {test_name(key)}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
    for key in skips:
        print(f"::group::SKIP {test_name(key)}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
        if args.fail_on_skip:
            print(f"go test: skipped test fails this run: {test_name(key)}")
    skip_failure = bool(args.fail_on_skip and skips)
    problem = None
    if args.title and args.summary_file:
        with args.summary_file.open("a", encoding="utf-8") as summary:
            summary.write(package_markdown(args.title, packages, failures, skips,
                                           args.fail_on_skip, status))
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
    print(f"go test: {len(packages)} packages, {len(failures)} failures, "
          f"{len(skips)} skipped tests, exit {status}")
    return status or (1 if failures or problem or skip_failure else 0)


if __name__ == "__main__":
    sys.exit(main())
