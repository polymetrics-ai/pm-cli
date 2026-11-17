#!/usr/bin/env python3
"""Run `go test -json`, keep the event stream and print a short summary.

Usage: go-test-report.py --output FILE [--scenarios] [--title TEXT]
                         [--fail-on-skip] [--expect FILE] [--deadline TIME]
                         [--timeout SECONDS] [--summary-file FILE] -- COMMAND...

COMMAND must print `go test -json` events. The summary lists each package
with its result and elapsed time (input for shard balancing) and repeats the
output of failed tests and packages. It also lists each skipped test with its
output. A package-level skip means that the package has no test files. It is
not a test skip. The exit status is the command's status, or 1 when the
stream records a failure the command did not report. A command that a signal
stops gives 128 plus the signal number.

A test or subtest that started (a `run` event) and has no result (no `pass`,
`fail` or `skip` event) is a failure, also in a package that passes. An
example is a test whose goroutine panics: the test binary exits, and the
panic output belongs to that test. The log and the summary print the test
as `FAIL (no result)` with its output, and the problem list names it. A test
that pauses and continues (`pause`, `cont`) and then gets a result is not
such a test.

With --fail-on-skip, a skipped test (a test or subtest that calls t.Skip) is a
failure: the exit status is 1, and the log and the summary name the test.

With --expect FILE, FILE lists one import path on each line. Each listed
package must have exactly one package result in the stream. A missing,
duplicated or unexpected package result, an empty stream or a line that is not
a JSON event fails the run. The log and the summary give the size of the
intended set and the number of package results. The command must contain
-count=1 and must not
select or filter tests (-run, -skip, -list, -short, -failfast, -tags), also not
through GOFLAGS. Such a command is refused before it runs.

With --deadline TIME (Unix time in seconds) or --timeout SECONDS, the command
stops at TIME or SECONDS after its start, whichever comes first: the script
sends SIGTERM to its process group, then SIGKILL after 30 seconds. The exit
status is then 124, and the report still lists the results in the stream.
The script runs COMMAND in its own process group and forwards SIGTERM, SIGINT
and SIGHUP to it. When COMMAND exits, the script sends SIGKILL to the rest of
that process group, so that no test process of the run stays alive. It sends
that SIGKILL before it reaps COMMAND: until then, the process ID of COMMAND,
which is also the ID of its process group, names no other process. The script
waits for the exit of COMMAND with a pidfd (Linux) or a kqueue process filter
(macOS), which do not reap it.

With --title, --summary-file appends a Markdown section with that title: the
result, the totals, each package with its result and elapsed time, each failed
or skipped test and each problem that --expect or --deadline found.

With --scenarios, the summary also lists each godog scenario with its result
and repeats the godog totals (`N scenarios (...)`, `N steps (...)`), also when
the suite passes. A passing run without godog totals, or with a scenario count
that differs from the godog total, fails. --summary-file appends the same
report as Markdown to FILE (for example $GITHUB_STEP_SUMMARY).
--summary-file requires --title, --scenarios or both.
"""
import argparse
import json
import os
from pathlib import Path
import re
import select
import signal
import subprocess
import sys
import time

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


def markdown(rows, totals, problem, unfinished=(), problems=()):
    """The scenario section. A caller without --title also passes the tests
    with no result and the problems, because no package section lists them."""
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
    for heading, items in (("Tests with no result", [f"`{test_name(key)}`" for key in unfinished]),
                           ("Problems", list(problems))):
        if items:
            lines += [f"{heading}:", ""]
            lines += [f"- {item}" for item in items]
            lines.append("")
    return "\n".join(lines) + "\n"



# Options that select or filter tests. A run with --expect refuses them, so
# that each listed package runs all of its tests.
FILTER = re.compile(r"^--?(run|skip|list|short|failfast|tags)(=.*)?$")
DEADLINE_STATUS = 124
KILL_GRACE = 30


def package_result(action):
    """Name a package result. A package-level skip means no test files."""
    return "SKIP (no test files)" if action == "skip" else action.upper()


def test_name(key):
    return key[0] + (f" {key[1]}" if key[1] else "")


def filter_problem(command):
    """Describe a test filter in COMMAND or GOFLAGS, or return None."""
    words = list(command) + os.environ.get("GOFLAGS", "").split()
    for word in words:
        if FILTER.match(word):
            return f"the command selects or filters tests with {word.split('=')[0]}"
    if "-count=1" not in command:
        return "the command does not contain -count=1"
    return None


class ExitWatch:
    """Waits for the exit of one child process without reaping it.

    While the child is not reaped, its process ID, and so the ID of its
    process group, names no other process. Linux uses a pidfd, macOS a
    kqueue process filter.
    """

    def __init__(self, pid):
        self.descriptor = self.queue = None
        self.exited = False
        if hasattr(os, "pidfd_open"):
            self.descriptor = os.pidfd_open(pid)
        elif hasattr(select, "kqueue"):
            self.queue = select.kqueue()
            change = select.kevent(pid, filter=select.KQ_FILTER_PROC,
                                   flags=select.KQ_EV_ADD | select.KQ_EV_ONESHOT,
                                   fflags=select.KQ_NOTE_EXIT)
            try:
                self.queue.control([change], 0, 0)
            except ProcessLookupError:
                # The child has already exited. This script has not reaped it.
                self.exited = True
        else:
            raise OSError("no process exit event: the system has neither pidfd_open nor kqueue")

    def wait(self, timeout):
        """Wait for the exit for at most TIMEOUT seconds (None: no limit)."""
        if not self.exited:
            if self.descriptor is not None:
                poller = select.poll()
                poller.register(self.descriptor, select.POLLIN)
                ready = poller.poll(None if timeout is None else int(timeout * 1000))
            else:
                ready = self.queue.control(None, 1, timeout)
            self.exited = bool(ready)
        return self.exited

    def close(self):
        if self.descriptor is not None:
            os.close(self.descriptor)
        if self.queue is not None:
            self.queue.close()


def run_command(command, output, deadline, limit):
    """Run COMMAND with stdout in OUTPUT. Return (status, timed_out)."""
    with output.open("wb") as stream:
        try:
            process = subprocess.Popen(command, stdout=stream, start_new_session=True)
        except OSError as exc:
            print(f"go test: cannot start the command: {exc}")
            return 1, False
        if limit is not None:
            relative = time.time() + limit
            deadline = relative if deadline is None else min(deadline, relative)

        watch = None

        def forward(signum, _frame):
            try:
                os.killpg(process.pid, signum)
            except ProcessLookupError:
                pass
            except PermissionError:
                # Darwin reports EPERM when the only member left in the group
                # is the command that has exited and is not reaped yet. Then
                # no process is left to signal. Any other EPERM is an error.
                if watch is None or not watch.exited:
                    raise

        try:
            watch = ExitWatch(process.pid)
        except OSError as exc:
            print(f"go test: cannot watch the command: {exc}")
            forward(signal.SIGKILL, None)
            process.wait()
            return 1, False
        previous = {number: signal.signal(number, forward)
                    for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)}
        timed_out = False
        try:
            timeout = None if deadline is None else max(0.0, deadline - time.time())
            if not watch.wait(timeout):
                timed_out = True
                forward(signal.SIGTERM, None)
                if not watch.wait(KILL_GRACE):
                    forward(signal.SIGKILL, None)
                    watch.wait(None)
        finally:
            for number, handler in previous.items():
                signal.signal(number, handler)
            # A test process that outlives the command must not keep running.
            # The command is not reaped yet, so its process group ID names no
            # other group. Reap it only after this signal.
            forward(signal.SIGKILL, None)
            status = process.wait()
            watch.close()
        if timed_out:
            status = DEADLINE_STATUS
    if status < 0:
        status = 128 - status
    return status, timed_out


def expect_problems(expected, counts, events, malformed):
    """Compare the package results with the expected import paths."""
    problems = []
    if events == 0:
        problems.append("the go test event stream is empty")
    if malformed:
        problems.append(f"the stream has {malformed} lines that are not JSON events")
    seen = set()
    for package in expected:
        if package in seen:
            problems.append(f"the expected package list names {package} more than once")
        seen.add(package)
        if counts.get(package, 0) == 0:
            problems.append(f"no package result for {package}")
    for package, count in sorted(counts.items()):
        if count > 1:
            problems.append(f"{count} package results for {package}")
        if package not in seen:
            problems.append(f"a package result for {package}, which is not in the expected list")
    return problems


def package_markdown(title, packages, failures, skips, fail_on_skip, status, problems,
                     intended=None, unfinished=()):
    failed = bool(status or failures or unfinished or problems or (fail_on_skip and skips))
    lines = [f"### {title}", "", f"Result: {'FAILED' if failed else 'PASSED'}.  "]
    if intended:
        lines.append(f"The {intended}.  ")
    counts = {}
    for action, _ in packages.values():
        counts[action] = counts.get(action, 0) + 1
    lines.append(
        f"{len(packages)} packages: {counts.get('pass', 0)} passed, {counts.get('fail', 0)} failed, "
        f"{counts.get('skip', 0)} with no test files. {len(failures) + len(unfinished)} failures. "
        f"{len(skips)} skipped tests{' (each one is a failure)' if fail_on_skip and skips else ''}. "
        f"Exit {status}.  ")
    lines.append("")
    if packages:
        lines += ["| Result | Time | Package |", "| --- | --- | --- |"]
        for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
            lines.append(f"| {package_result(action)} | {elapsed:.1f}s | `{package}` |")
        lines.append("")
    failed_items = [f"`{test_name(key)}`" for key in failures]
    failed_items += [f"`{test_name(key)}` (no result)" for key in unfinished]
    for heading, items in (("Failed", failed_items),
                           ("Skipped tests", [f"`{test_name(key)}`" for key in skips]),
                           ("Problems", problems)):
        if items:
            lines += [f"{heading}:", ""]
            lines += [f"- {item}" for item in items]
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
    parser.add_argument("--expect", type=Path,
                        help="a file of import paths that must each have exactly one package result")
    parser.add_argument("--deadline", type=int,
                        help="Unix time in seconds at which the command stops")
    parser.add_argument("--timeout", type=int,
                        help="seconds after its start at which the command stops")
    parser.add_argument("--summary-file", type=Path,
                        help="append the report as Markdown (requires --title or --scenarios)")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("missing go test command")
    if args.summary_file and not (args.scenarios or args.title):
        parser.error("--summary-file requires --title or --scenarios")
    expected = None
    if args.expect:
        expected = [line.strip() for line in args.expect.read_text().splitlines() if line.strip()]
        if not expected:
            parser.error(f"--expect {args.expect} lists no package")
        refused = filter_problem(command)
        if refused:
            parser.error(f"refused: {refused}")
    status, timed_out = run_command(command, args.output, args.deadline, args.timeout)
    packages, failures, output = {}, [], {}
    # started keeps the tests with a `run` event in first-seen order. A dict
    # keeps insertion order and checks membership in constant time.
    order, results, skips, started = [], {}, [], {}
    counts, events, malformed = {}, 0, 0
    for line in args.output.read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            print(line)
            malformed += 1
            continue
        if not isinstance(event, dict):
            malformed += 1
            continue
        events += 1
        action = event.get("Action")
        key = (event.get("Package") or event.get("ImportPath") or "", event.get("Test"))
        if key not in output:
            order.append(key)
            output[key] = []
        if action in ("output", "build-output"):
            output[key].append(event.get("Output", ""))
        elif action == "run" and key[1] is not None:
            started.setdefault(key, None)
        elif action in ("pass", "fail", "skip") and key[1] is None:
            packages[key[0]] = (action, float(event.get("Elapsed") or 0))
            counts[key[0]] = counts.get(key[0], 0) + 1
        if action in ("pass", "fail", "skip") and key[1] is not None:
            results[key] = action
        if action == "skip" and key[1] is not None:
            skips.append(key)
        if action in ("fail", "build-fail"):
            failures.append(key)
    # A test that started and has no result: for example, a goroutine of the
    # test panicked and the test binary exited. Its output holds the cause.
    unfinished = [key for key in started if key not in results]
    problems = [f"a test started and has no result: {test_name(key)}" for key in unfinished]
    if timed_out:
        problems.append(f"the command did not end before its deadline and was stopped (exit {status})")
    if expected is not None:
        problems += expect_problems(expected, counts, events, malformed)
        intended = (f"intended set: {len(set(expected))} packages from go list; "
                    f"package results: {sum(counts.values())}")
    if args.title:
        print(args.title)
    for package, (action, elapsed) in sorted(packages.items(), key=lambda item: -item[1][1]):
        print(f"{action.upper():4} {elapsed:8.1f}s {package}" + (" (no test files)" if action == "skip" else ""))
    for key in failures:
        print(f"::group::FAIL {test_name(key)}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
    for key in unfinished:
        print(f"::group::FAIL (no result) {test_name(key)}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
    for key in skips:
        print(f"::group::SKIP {test_name(key)}")
        sys.stdout.write("".join(output.get(key, [])))
        print("::endgroup::")
        if args.fail_on_skip:
            print(f"go test: skipped test fails this run: {test_name(key)}")
    if expected is not None:
        print(f"go test: {intended}")
    for problem in problems:
        print(f"go test: {problem}")
    skip_failure = bool(args.fail_on_skip and skips)
    if args.title and args.summary_file:
        with args.summary_file.open("a", encoding="utf-8") as summary:
            summary.write(package_markdown(args.title, packages, failures, skips,
                                           args.fail_on_skip, status, problems,
                                           intended if expected is not None else None,
                                           unfinished))
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
                # Without --title, no package section names the tests with no
                # result or the problems, so this section names them.
                summary.write(markdown(rows, totals, problem,
                                       () if args.title else unfinished,
                                       () if args.title else problems))
    print(f"go test: {len(packages)} packages, {len(failures) + len(unfinished)} failures, "
          f"{len(skips)} skipped tests, exit {status}")
    return status or (1 if failures or problem or problems or skip_failure else 0)


if __name__ == "__main__":
    sys.exit(main())
