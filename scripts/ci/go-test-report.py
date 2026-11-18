#!/usr/bin/env python3
"""Run `go test -json`, keep the event stream and print a short summary.

Usage: go-test-report.py --output FILE [--scenarios] [--title TEXT]
                         [--fail-on-skip] [--expect FILE] [--deadline TIME]
                         [--timeout SECONDS] [--summary-file FILE]
                         [--live --raw-output FILE] -- COMMAND...

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
intended set and the number of package results.

With --expect, a guard checks the command words and the GOFLAGS environment
variable before the command runs:
  - A word that starts with "-" is an option. The guard reads the forms -name,
    --name and -test.name, with "=value" or, in the command, with the value as
    the next word.
  - It accepts only the go test options of the CI commands and -race (for
    #312): -json, -p, -count, -timeout and -race. It refuses each other
    option, for example -exec, -overlay, -toolexec (crrao N1), or a filter
    such as -run, -skip, -list, -short, -failfast or -tags. It also refuses
    -args.
  - It splits GOFLAGS at the characters that go splits it at (space, tab,
    newline, carriage return). go also accepts quoted fields there, so the
    guard refuses a GOFLAGS value with a quote character. It also refuses a
    GOFLAGS field that is not an option, and a GOFLAGS option with a value
    that has no "=value" form, which go also refuses.
  - The command and GOFLAGS together must set the test count exactly once, to 1.
Such a command is refused before it runs. The guard does not read other
sources of go options, for example a GOFLAGS value in the go environment file
(go env -w).

With --deadline TIME (Unix time in seconds) or --timeout SECONDS, the command
stops at TIME or SECONDS after its start, whichever comes first: the script
sends SIGTERM to its process group, then SIGKILL after 30 seconds. The exit
status is then 124, and the report still lists the results in the stream.
The script runs COMMAND in its own process group and forwards SIGTERM, SIGINT
and SIGHUP to it. A signal that the script itself receives fails the run: the
script waits up to 30 seconds for COMMAND to stop, then sends SIGKILL, prints
"the reporter received SIG... and stopped the command" and exits with 128 plus
the signal number, also when COMMAND exits 0. When COMMAND exits, the script sends SIGKILL to the rest of
that process group, so that no test process of the run stays alive. It sends
that SIGKILL before it reaps COMMAND. Until the reap, the system keeps the
process ID of COMMAND, which is also the ID of its process group, for COMMAND,
so no new process or process group can get that ID. After that SIGKILL, the
script sends no more signals to the group: a signal that the script receives
later is only recorded, and it still fails the run. The script waits for the
exit of COMMAND with a pidfd (Linux) or a kqueue process filter (macOS), which
do not reap it. After the last SIGKILL and before the reap, the script lists
the live members of the group (Linux: /proc, other systems: ps). Linux
reports success for a group signal when one member gets it. So a live member
that the script may not signal (permission denied) did not get the SIGKILL,
and it fails the run with a fixed message. A member list that cannot be read
also fails the run. Each wait after the last SIGKILL has a limit of 30 seconds,
because the system can refuse that SIGKILL. When COMMAND has not ended by
then, the script does not reap it, so its ID stays reserved, and the run fails
with a cleanup problem.

With --title, --summary-file appends a Markdown section with that title: the
result, the totals, each package with its result and elapsed time, each failed
or skipped test and each problem that --expect or --deadline found.

With --scenarios, the summary also lists each godog scenario with its result
and repeats the godog totals (`N scenarios (...)`, `N steps (...)`), also when
the suite passes. A passing run without godog totals, or with a scenario count
that differs from the godog total, fails. The godog totals also fail the run
(issue #350), and the log and the summary name each cause:
  - the godog scenario total is 0 ("0 scenarios ran");
  - the godog step total has skipped steps.
Only these totals, and the skip events of Go tests (--fail-on-skip), fail the
run. When the step total has skipped steps, the log and the summary also name
each skipped step ("skipped step: SCENARIO / STEP") and each scenario whose
steps are all skipped ("skipped scenario: SCENARIO"). godog v0.16.0 prints a
skipped step in cyan. It also prints the template steps of a Scenario Outline
in cyan, whatever their result, so the names come only from scenarios that
are not outlines.
Without --title, the scenario section of the summary also lists each test
with no result, each skipped test and each problem. --summary-file appends
the same report as Markdown to FILE (for example $GITHUB_STEP_SUMMARY).
--summary-file requires --title, --scenarios or both.

--live is the mode of the live runner (D2-A, L-e1 P5a). The log and the
summary file then get no text from the test stream: no package, test,
subtest or scenario name, no output line and no error text. They get only
counts, the godog totals as numbers, the result words PASS, FAIL, SKIP and
"no result", and ordinals ("test 4 of 9: FAIL", "scenario 2 of 3: FAIL").
The full report and the standard error of COMMAND go to the --raw-output
FILE, which the script creates with mode 0600 and refuses when it exists.
--live requires --scenarios, --expect and --raw-output, and --raw-output
requires --live. The other rules do not change. The live log holds no stream
text by design: this is prevention, not detection.
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
# A step line of the godog pretty format: the keyword and the text, each in
# the color of the step status. Table rows start with "|" and do not match.
# In a Scenario Outline, godog prints the template steps in cyan whatever
# their result, so OUTLINE marks the output of an outline.
STEP = re.compile(r"^\s*\x1b?\[(3[0-7])m([^\s\x1b]+)\x1b?\[0m \x1b?\[\1m([^\x1b]*?)\x1b?\[0m")
SKIPPED_COLOR = "36"
SKIPPED_STEPS = re.compile(r"\b(\d+) skipped\b")
OUTLINE = re.compile(r"^\s*Scenario Outline:")


def scenario_report(order, results, output):
    """Return the scenario rows, the godog total lines and the steps.

    STEPS has one (name, step count, skipped steps) item for each row. The
    step lines of a Scenario Outline are not step results, so an outline has
    no counted steps.
    """
    totals, suite, steps = [], None, []
    for key in order:
        for text in output.get(key, []):
            line = ANSI.sub("", text).strip()
            if TOTAL.match(line):
                totals.append(line)
                suite = key
    rows = []
    if suite is None:
        return rows, totals, steps
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
        count, skipped = 0, []
        outline = any(OUTLINE.match(ANSI.sub("", text)) for text in output.get(key, []))
        for text in ([] if outline else output.get(key, [])):
            match = STEP.match(text)
            if match:
                count += 1
                if match.group(1) == SKIPPED_COLOR:
                    skipped.append(f"{match.group(2)} {match.group(3)}")
        rows.append((results[key], name, location))
        steps.append((name, count, skipped))
    return rows, totals, steps


def scenario_causes(rows, totals, steps):
    """Name each cause of issue #350 that the godog totals show.

    Only the totals decide: 0 scenarios, or skipped steps in the step total.
    The names of skipped steps and scenarios only explain a step total with
    skipped steps. They never fail a run on their own.
    """
    causes, skipped_total = [], 0
    for line in totals:
        match = TOTAL.match(line)
        if match and (match.group(3) == "scenarios" or
                      (match.group(2) or "").startswith("scenario") and match.group(1) == "0"):
            causes.append("0 scenarios ran")
        elif match and (match.group(2) or "").startswith("step"):
            skipped = SKIPPED_STEPS.search(line)
            if skipped and int(skipped.group(1)):
                skipped_total += int(skipped.group(1))
                causes.append(f"the godog step total has {skipped.group(1)} skipped steps")
    if not skipped_total:
        return causes
    for (action, _, _), (name, count, skipped) in zip(rows, steps):
        if count and len(skipped) == count:
            causes.append(f"skipped scenario: {name}")
        else:
            causes += [f"skipped step: {name} / {step}" for step in skipped]
    return causes


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


def markdown(rows, totals, problem, unfinished=(), problems=(), skips=(), fail_on_skip=False):
    """The scenario section. A caller without --title also passes the tests
    with no result, the skipped tests and the problems, because no package
    section lists them."""
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
    skipped = "Skipped tests (each one fails this run)" if fail_on_skip else "Skipped tests"
    for heading, items in (("Tests with no result", [f"`{test_name(key)}`" for key in unfinished]),
                           (skipped, [f"`{test_name(key)}`" for key in skips]),
                           ("Problems", list(problems))):
        if items:
            lines += [f"{heading}:", ""]
            lines += [f"- {item}" for item in items]
            lines.append("")
    return "\n".join(lines) + "\n"



# Options that select or filter tests. A run with --expect refuses them, so
# that each listed package runs all of its tests. go test and the test binary
# accept each one as -name, --name or -test.name, with "=value" or with the
# value as the next word.
SELECTION = frozenset({"run", "skip", "list", "short", "failfast", "tags"})
# The only go test options that a run with --expect accepts: the options of
# the CI commands (the unit run in check-docs.sh, the stack and Gherkin runs
# in check-bdd.sh), of GOFLAGS=-p=2, and -race, which #312 adds to the stack
# run. True marks an option with a value. Any other option, for example
# -exec, -overlay or -toolexec, can change what runs, so it is refused.
ALLOWED = {"json": False, "p": True, "count": True, "timeout": True, "race": False}
# The characters at which go splits GOFLAGS (cmd/internal/quoted).
GOFLAGS_SPACE = re.compile(r"[ \t\n\r]+")
DEADLINE_STATUS = 124
KILL_GRACE = 30


def package_result(action):
    """Name a package result. A package-level skip means no test files."""
    return "SKIP (no test files)" if action == "skip" else action.upper()


def test_name(key):
    return key[0] + (f" {key[1]}" if key[1] else "")


def option_name(word):
    """Return (name, value) of an option word, or (None, None).

    The name has no leading dashes and no "test." prefix. The value is None
    when the word has no "=value" part.
    """
    if not word.startswith("-") or word.strip("-") == "":
        return None, None
    name, equals, value = word.lstrip("-").partition("=")
    if name.startswith("test."):
        name = name[len("test."):]
    return name, (value if equals else None)


def filter_problem(command):
    """Describe a refused option or a wrong test count in COMMAND or GOFLAGS.

    The command and GOFLAGS together may use only the ALLOWED options, and
    must set the test count exactly once, to 1. Return None when they pass.
    """
    counts = []
    goflags = os.environ.get("GOFLAGS", "")
    # go reads a field in quotes as one word without the quotes. The guard
    # does not copy that, so it refuses each quoted form.
    if "'" in goflags or '"' in goflags:
        return "GOFLAGS has a quote character, which the guard refuses"
    goflags_words = [word for word in GOFLAGS_SPACE.split(goflags) if word]
    for word in goflags_words:
        if option_name(word)[0] is None:
            return f"GOFLAGS has the field {word!r}, which is not an option"
    for words, in_goflags in ((list(command), False), (goflags_words, True)):
        index = 0
        while index < len(words):
            word = words[index]
            if word in ("-args", "--args"):
                return f"the command passes options to the test binary with {word}"
            name, value = option_name(word)
            if name is None:
                index += 1
                continue
            if name in SELECTION:
                return f"the command selects or filters tests with {word.split('=')[0]}"
            if name not in ALLOWED:
                return (f"the command uses the option {word.split('=')[0]}, which is not "
                        f"in the allow list (-json, -p, -count, -timeout, -race)")
            if ALLOWED[name] and value is None and in_goflags:
                return f"GOFLAGS has {word} with no =value"
            if ALLOWED[name] and value is None:
                index += 1
                if index >= len(words):
                    return f"the command has {word} with no value"
                value = words[index]
            if name == "count":
                counts.append(value)
            index += 1
    if not counts:
        return "the command does not contain -count=1"
    if len(counts) > 1:
        return f"the command sets the test count {len(counts)} times; it must set -count=1 once"
    if counts[0] != "1":
        return f"the command sets the test count to {counts[0]}; it must be -count=1"
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


class _ReceivedSignal(Exception):
    """The script itself received SIGTERM, SIGINT or SIGHUP."""


# The process list of Linux. The developer checks replace it.
PROC = Path("/proc")
# The limits of one ps run: its listing, and its collection after a kill.
LIST_TIMEOUT = 10
LIST_KILL_GRACE = 5
# ps starts with this reference, so that a check that replaces
# subprocess.Popen to watch the command does not see the ps process.
PROCESS_LIST_POPEN = subprocess.Popen


def live_group_members(group):
    """Return the IDs of the processes in GROUP that have not exited.

    Return None when the process list cannot be read. Linux reads
    /proc/<pid>/stat, other systems read ps. A process that joins the group
    after the call is not in the list. Only a confirmed disappearance
    (ENOENT or ESRCH) counts as an exited process. Any other failure to read
    a process record makes the list unknown, so the run fails. Each wait for
    ps has a limit, also after a kill: a ps that does not end gives an
    unknown list, with no further wait.
    """
    live = []
    if sys.platform.startswith("linux"):
        try:
            entries = [entry for entry in PROC.iterdir() if entry.name.isdigit()]
        except OSError:
            return None
        for entry in entries:
            try:
                text = (entry / "stat").read_text()
            except (FileNotFoundError, ProcessLookupError):
                # The process exited after the directory listing.
                continue
            except OSError:
                # A record that cannot be read is no proof of an exit.
                return None
            # The fields after the command name: state, parent, group.
            fields = text[text.rfind(")") + 1:].split()
            if len(fields) >= 3 and fields[2] == str(group) and fields[0] not in ("Z", "X"):
                live.append(int(entry.name))
        return live
    try:
        lister = PROCESS_LIST_POPEN(["/bin/ps", "-A", "-o", "pid=,pgid=,stat="], stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    except OSError:
        return None
    try:
        listing, _ = lister.communicate(timeout=LIST_TIMEOUT)
    except subprocess.TimeoutExpired:
        lister.kill()
        try:
            lister.communicate(timeout=LIST_KILL_GRACE)
        except subprocess.TimeoutExpired:
            # ps did not end after SIGKILL. The script does not wait for it.
            pass
        return None
    if lister.returncode != 0:
        return None
    for line in listing.splitlines():
        fields = line.split()
        if len(fields) >= 3 and fields[1] == str(group) and not fields[2].startswith("Z"):
            live.append(int(fields[0]))
    return live


def unsignalled_members(group):
    """Count the live members of GROUP that this script may not signal.

    Call it after the last SIGKILL to GROUP and before the reap. Linux
    reports success for a group signal when one member gets it, so a member
    without permission can stay alive with no error (crrao R1). A member
    that the script may signal got the SIGKILL. The check sends signal 0,
    which delivers nothing. The leader of GROUP is not counted. Return None
    when the member list cannot be read.
    """
    members = live_group_members(group)
    if members is None:
        return None
    count = 0
    for pid in members:
        if pid == group:
            continue
        try:
            os.kill(pid, 0)
        except PermissionError:
            count += 1
        except ProcessLookupError:
            pass
    return count


def run_command(command, output, deadline, limit, errors=None):
    """Run COMMAND with stdout in OUTPUT, and stderr in ERRORS when it is given.

    Return (status, timed_out, received, cleanup). RECEIVED is the first
    signal that the script itself received, or None. A received signal forces
    the status 128 plus its number, also when COMMAND then exits 0. CLEANUP
    names a signal to the process group of COMMAND that the system refused
    (EPERM), or is None. A refused signal fails the run: a test process can
    still run. On Linux each refusal fails the run. On Darwin, the system
    also refuses a signal to a group whose only member is COMMAND after it
    has exited and before the script reaps it. Only then, and only when ps
    shows no member of the group that has not exited, the refusal does not
    fail the run. CLEANUP also names a COMMAND that did not end KILL_GRACE
    seconds after the last SIGKILL. The script does not reap such a COMMAND,
    and the run fails. After the last SIGKILL and before the reap, CLEANUP
    also names live members of the group that the script may not signal,
    and a member list that cannot be read (crrao R1). They fail the run.
    """
    # forward is False after the last signal to the group, before the reap.
    state = {"process": None, "watch": None, "waiting": False, "forward": True}
    received = []
    denied = []
    # alive is True when COMMAND has not ended KILL_GRACE seconds after the
    # last SIGKILL (for example, the system refused that SIGKILL). The script
    # then does not reap it, so its ID stays reserved, and the run fails.
    alive = []
    # unsignalled holds the result of unsignalled_members() after the last
    # SIGKILL: a count, or None when the member list cannot be read.
    unsignalled = []

    def refusal_is_harmless(group):
        watch = state["watch"]
        if sys.platform != "darwin" or watch is None or not watch.exited:
            return False
        return live_group_members(group) == []

    def forward(signum, _frame):
        process = state["process"]
        if process is None or not state["forward"]:
            return
        try:
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass
        except PermissionError:
            if not refusal_is_harmless(process.pid):
                denied.append(signum)

    def cleanup():
        parts = []
        if denied:
            names = ", ".join(sorted({signal.Signals(number).name for number in denied}))
            parts.append(f"the system refused to send {names} to the process group of the command "
                         f"(permission denied), so a test process can still run")
        if alive:
            parts.append(f"the command did not end {KILL_GRACE} seconds after the last SIGKILL, so "
                         f"the reporter did not reap it and a test process can still run")
        if unsignalled and unsignalled[0] is None:
            parts.append("the reporter could not list the process group of the command after its "
                         "last SIGKILL, so a test process can still run")
        elif unsignalled and unsignalled[0]:
            parts.append(f"after the last SIGKILL, {unsignalled[0]} processes of the process group "
                         f"of the command remain that the reporter may not signal (permission "
                         f"denied), so a test process can still run")
        return ". ".join(parts) or None

    def reap(process, watch):
        """Reap COMMAND only when it has ended. Never wait without a limit."""
        if watch is not None:
            ended = watch.wait(KILL_GRACE)
        else:
            try:
                return process.wait(timeout=KILL_GRACE)
            except subprocess.TimeoutExpired:
                ended = False
        if not ended:
            alive.append(True)
            return 1
        return process.wait()

    def early_status():
        # A received signal forces 128 plus its number on each path.
        return 128 + received[0] if received else 1

    def on_signal(signum, frame):
        # Only a signal that this script receives comes here. The deadline
        # and the final kill call forward() directly. The signal is always
        # recorded, also after forwarding ended, so that it fails the run.
        received.append(signum)
        forward(signum, frame)
        if state["waiting"] and len(received) == 1:
            raise _ReceivedSignal()

    # The handlers are in place before the command starts, so a signal that
    # comes before the wait is recorded too.
    previous = {number: signal.signal(number, on_signal)
                for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)}
    try:
        with output.open("wb") as stream:
            try:
                process = subprocess.Popen(command, stdout=stream, stderr=errors, start_new_session=True)
            except OSError as exc:
                print(f"go test: cannot start the command: {exc}")
                return early_status(), False, received[0] if received else None, None
            state["process"] = process
            if limit is not None:
                relative = time.time() + limit
                deadline = relative if deadline is None else min(deadline, relative)
            try:
                watch = state["watch"] = ExitWatch(process.pid)
            except OSError as exc:
                print(f"go test: cannot watch the command: {exc}")
                forward(signal.SIGKILL, None)
                state["forward"] = False
                unsignalled.append(unsignalled_members(process.pid))
                reap(process, None)
                return early_status(), False, received[0] if received else None, cleanup()
            timed_out = False
            try:
                try:
                    state["waiting"] = True
                    if received:
                        raise _ReceivedSignal()
                    timeout = None if deadline is None else max(0.0, deadline - time.time())
                    if not watch.wait(timeout):
                        timed_out = True
                        forward(signal.SIGTERM, None)
                        if not watch.wait(KILL_GRACE):
                            forward(signal.SIGKILL, None)
                except _ReceivedSignal:
                    # The handler forwarded the signal to the group. Give the
                    # command KILL_GRACE seconds to stop, then SIGKILL.
                    forward(received[0], None)
                    if not watch.wait(KILL_GRACE):
                        forward(signal.SIGKILL, None)
            finally:
                state["waiting"] = False
                # A test process that outlives the command must not keep
                # running. The command is not reaped yet, so no new process
                # or process group can have its ID. This is the last signal
                # to the group: forwarding ends before the reap, because
                # after the reap the ID can name a new group.
                forward(signal.SIGKILL, None)
                state["forward"] = False
                # The group ID still names this group. List its live members
                # before the reap (crrao R1).
                unsignalled.append(unsignalled_members(process.pid))
                # The wait for the end is bounded: a refused SIGKILL can leave
                # the command alive.
                status = reap(process, watch)
                watch.close()
            if timed_out:
                status = DEADLINE_STATUS
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)
    if status < 0:
        status = 128 - status
    if received:
        status = 128 + received[0]
    if (denied or alive or (unsignalled and unsignalled[0] != 0)) and status == 0:
        status = 1
    return status, timed_out, received[0] if received else None, cleanup()


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


RESULT_WORDS = re.compile(r"(\d+) (passed|failed|skipped|undefined|pending|ambiguous)\b")


def live_report(packages, order, results, unfinished, rows, totals, problems, failures, skips, status):
    """The lines of --live: counts, result words and ordinals only.

    No line holds text from the test stream. The godog totals are rebuilt
    from their numbers and fixed words. Each problem is counted by kind.
    """
    counts = {}
    for action, _ in packages.values():
        counts[action] = counts.get(action, 0) + 1
    lines = [f"go test (live): {len(packages)} package results: {counts.get('pass', 0)} PASS, "
             f"{counts.get('fail', 0)} FAIL, {counts.get('skip', 0)} with no test files"]
    tests = [key for key in order if key[1] is not None]
    words = {"fail": "FAIL", "skip": "SKIP"}
    for number, key in enumerate(tests, 1):
        word = "no result" if key in unfinished else words.get(results.get(key))
        if word:
            lines.append(f"test {number} of {len(tests)}: {word}")
    for number, (action, _, _) in enumerate(rows, 1):
        lines.append(f"scenario {number} of {len(rows)}: {action.upper()}")
    for line in totals:
        match = TOTAL.match(line)
        kind = "scenarios" if (match.group(3) or match.group(2)).startswith("scenario") else "steps"
        parts = ", ".join(f"{count} {word}" for count, word in RESULT_WORDS.findall(line))
        lines.append(f"godog {kind}: {match.group(1) or 0}" + (f" ({parts})" if parts else ""))
    lines.append(f"go test (live): {len(problems)} problems; the private raw report names them")
    lines.append(f"go test: {len(packages)} packages, {len(failures) + len(unfinished)} failures, "
                 f"{len(skips)} skipped tests, exit {status}")
    return lines


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
    parser.add_argument("--live", action="store_true",
                        help="print no stream text; the full report goes to --raw-output")
    parser.add_argument("--raw-output", type=Path,
                        help="the private file of the full report (mode 0600, must not exist)")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.live and not (args.scenarios and args.expect and args.raw_output):
        parser.error("--live requires --scenarios, --expect and --raw-output")
    if args.raw_output and not args.live:
        parser.error("--raw-output requires --live")
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
    public, raw = sys.stdout, None
    if args.live:
        try:
            raw = os.fdopen(os.open(args.raw_output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                    0o600), "w", encoding="utf-8")
        except OSError:
            parser.error("--raw-output must name a new file")
        # From here on, every report line goes to the private raw file.
        sys.stdout = raw
    status, timed_out, received, cleanup = run_command(command, args.output, args.deadline, args.timeout, raw)
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
    if cleanup is not None:
        problems.append(cleanup)
    if received is not None:
        problems.append(f"the reporter received {signal.Signals(received).name} and stopped the "
                        f"command (exit {status})")
    if timed_out:
        problems.append(f"the command did not end before its deadline and was stopped (exit {status})")
    if expected is not None:
        problems += expect_problems(expected, counts, events, malformed)
        intended = (f"intended set: {len(set(expected))} packages from go list; "
                    f"package results: {sum(counts.values())}")
    if args.scenarios:
        rows, totals, steps = scenario_report(order, results, output)
        problems += scenario_causes(rows, totals, steps)
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
        with summary_stream(args.summary_file, raw) as summary:
            summary.write(package_markdown(args.title, packages, failures, skips,
                                           args.fail_on_skip, status, problems,
                                           intended if expected is not None else None,
                                           unfinished))
    problem = None
    if args.scenarios:
        problem = scenario_problem(rows, totals)
        print("Scenarios:")
        for action, name, location in rows:
            print(f"{action.upper():4} {name}" + (f" ({location})" if location else ""))
        for line in totals:
            print(line)
        if problem:
            print(f"go test: scenario summary error: {problem}")
        if args.summary_file:
            with summary_stream(args.summary_file, raw) as summary:
                # Without --title, no package section names the tests with no
                # result or the problems, so this section names them.
                summary.write(markdown(rows, totals, problem,
                                       () if args.title else unfinished,
                                       () if args.title else problems,
                                       () if args.title else skips,
                                       args.fail_on_skip))
    print(f"go test: {len(packages)} packages, {len(failures) + len(unfinished)} failures, "
          f"{len(skips)} skipped tests, exit {status}")
    if raw is not None:
        sys.stdout = public
        raw.close()
        lines = live_report(packages, order, results, unfinished, rows, totals,
                            problems + ([problem] if problem else []), failures, skips, status)
        print("\n".join(lines))
        if args.summary_file:
            with args.summary_file.open("a", encoding="utf-8") as summary:
                summary.write("### Live run (no test names or output)\n\n" + "".join(f"- {line}\n" for line in lines))
    return status or (1 if failures or problem or problems or skip_failure else 0)


class summary_stream:
    """The summary file, or in --live mode the raw file, which stays open."""

    def __init__(self, path, raw):
        self.path, self.raw, self.file = path, raw, None

    def __enter__(self):
        if self.raw is not None:
            return self.raw
        self.file = self.path.open("a", encoding="utf-8")
        return self.file

    def __exit__(self, *exc):
        if self.file is not None:
            self.file.close()
        return False


if __name__ == "__main__":
    sys.exit(main())
