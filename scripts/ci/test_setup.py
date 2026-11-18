#!/usr/bin/env python3
"""Exercise the public setup checker against isolated, real file inventories."""
import ast
import contextlib
import errno
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import select
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
CHECKER = ROOT / "scripts/ci/check-setup.py"
POLICY = ROOT / "scripts/ci/setup-policy.json"
TRACKER = "docs/agents/issue-tracker.md"
PROVENANCE = "docs/development/skill-provenance.json"
WORKFLOW = ".github/workflows/bdd.yml"
SKILL = ".agents/skills/pm-go/SKILL.md"
VENDORED = ".agents/skills/asd-ste100"


class SetupContracts(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        result = subprocess.run(
            ["git", "-C", str(ROOT), "ls-files", "-z"], check=True,
            capture_output=True, text=True, timeout=15,
        )
        cls.inputs = set(filter(None, result.stdout.split("\0")))
        cls.contents = {name: (ROOT / name).read_bytes() for name in cls.inputs}

    def fixture(self, directory):
        root = Path(directory) / "checkout"
        for name, content in self.contents.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        inventory = Path(directory) / "paths.json"
        inventory.write_text(json.dumps(sorted(self.inputs)))
        return root, inventory

    def check(self, root, inventory, env=None):
        return subprocess.run(
            [sys.executable, "-I", "-B", str(CHECKER), "--root", str(root),
             "--policy", str(POLICY), "--paths-file", str(inventory)],
            capture_output=True, text=True, timeout=90, env=env,
        )

    def test_current_setup_is_accepted(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            result = self.check(root, inventory)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            report = json.loads(result.stdout)
            self.assertEqual(set(report["provenance"]["project_skills"]), {
                ".agents/skills/pm-go/SKILL.md",
                ".agents/skills/pm-connector-contracts/SKILL.md",
                ".agents/skills/pm-durable-effects/SKILL.md",
                ".agents/skills/pm-release-recovery/SKILL.md",
            })
            self.assertEqual(report["provenance"]["vendored_skills"],
                             [VENDORED + "/SKILL.md"])

    def test_local_heading_fragments_are_resolved(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            target = root / "docs/agents/domain.md"
            target.write_text(target.read_text() + "\n## Repeated `heading`\n\nFirst.\n\n## Repeated `heading`\n\nSecond.\n")
            tracker = root / TRACKER
            original = tracker.read_text()
            for fragment, accepted in (("repeated-heading", True),
                                       ("repeated-heading-1", True),
                                       ("repeated-heading-2", False)):
                with self.subTest(fragment=fragment):
                    tracker.write_text(original + f"\n[local section](domain.md#{fragment})\n")
                    result = self.check(root, inventory)
                    self.assertEqual(result.returncode == 0, accepted,
                                     result.stdout + result.stderr)

    def test_invalid_setup_is_refused(self):
        def text(root, path, transform):
            target = root / path
            target.write_text(transform(target.read_text()))

        def json_change(root, path, change):
            value = json.loads((root / path).read_text())
            change(value)
            (root / path).write_text(json.dumps(value))

        def section(content, empty=False):
            before, rest = content.split("## Read the assigned issue", 1)
            _, after = rest.split("\n## ", 1)
            return before + ("## Read the assigned issue\n\n" if empty else "") + "## " + after

        def broken_tracker_link(root, _):
            text(root, TRACKER, lambda value: value + "\n[broken](missing-required-context.md)\n")

        def missing_membership(root, inventory):
            names = json.loads(inventory.read_text())
            names.remove(TRACKER)
            inventory.write_text(json.dumps(names))

        def symlink(root, _):
            path = root / "docs/agents/domain.md"
            path.unlink()
            path.symlink_to(root / TRACKER)

        def unrecorded_vendored_file(root, inventory):
            (root / VENDORED / "extra.md").write_text("# Extra\n")
            names = json.loads(inventory.read_text())
            names.append(VENDORED + "/extra.md")
            inventory.write_text(json.dumps(names))

        def missing_provenance(root, _):
            json_change(root, PROVENANCE,
                        lambda value: value["project_skills"][0]["references"].remove(TRACKER))

        cases = {
            "blank document": lambda r, _: (r / TRACKER).write_text(""),
            "wrong tracker title": lambda r, _: text(r, TRACKER, lambda s: s.replace(
                "# Issue tracker: PM CLI", "# Unrelated document", 1)),
            "missing section": lambda r, _: text(r, TRACKER, section),
            "empty section": lambda r, _: text(r, TRACKER, lambda s: section(s, True)),
            "missing routing link": lambda r, _: text(r, "AGENTS.md", lambda s: s.replace(
                "[issue/spec lookup](docs/agents/issue-tracker.md)", "issue/spec lookup")),
            "missing provenance membership": missing_provenance,
            "broken tracker link": broken_tracker_link,
            "missing disk file": lambda r, _: (r / TRACKER).unlink(),
            "missing tracked membership": missing_membership,
            "missing provenance": lambda r, _: (r / PROVENANCE).unlink(),
            "stale skill digest": lambda r, _: text(r, SKILL, lambda s: s + "\nChanged skill.\n"),
            "stale vendored file": lambda r, _: text(r, VENDORED + "/references/writing-rules.md",
                lambda s: s + "\nLocal edit.\n"),
            "unrecorded vendored file": unrecorded_vendored_file,
            "vendored skill unrecorded": lambda r, _: json_change(r, PROVENANCE,
                lambda v: v.update({"vendored_skills": []})),
            "floating vendored revision": lambda r, _: json_change(r, PROVENANCE,
                lambda v: v["vendored_skills"][0].update({"revision": "main"})),
            "missing frontmatter": lambda r, _: text(r, SKILL, lambda s: s.split("---\n", 2)[-1]),
            "duplicate frontmatter": lambda r, _: text(r, SKILL, lambda s: s.replace(
                "name: pm-go", "name: pm-go\nname: conflicting", 1)),
            "duplicate JSON key": lambda r, _: text(r, PROVENANCE, lambda s: s.replace(
                '"schema_version":', '"schema_version": "duplicate", "schema_version":', 1)),
            "unknown provenance key": lambda r, _: json_change(r, PROVENANCE,
                lambda v: v.update({"unknown": True})),
            "escaping local link": lambda r, _: text(r, TRACKER,
                lambda s: s + "\n[escape](../../../../outside)\n"),
            "symlinked local input": symlink,
            "floating action": lambda r, _: text(r, WORKFLOW, lambda s: s.replace(
                "3d3c42e5aac5ba805825da76410c181273ba90b1", "v7")),
            "excess token permission": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("contents: read", "contents: write")),
            "duplicate workflow key": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("contents: read", "contents: read\n  contents: write")),
            "pull request trigger": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("on:\n  push:", "on:\n  pull_request:\n  push:")),
            "unsafe workflow trigger": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("on:\n  push:", "on:\n  pull_request_target:\n  push:")),
            "narrowed branches": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace('branches: ["**"]', "branches: [main]")),
            "outsider admitted": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("github.actor == 'karthik-sivadas' && ", "")),
            "forced main admitted": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace(" && (github.ref != 'refs/heads/main' || github.event.forced == false)", "")),
            "unpinned event checkout": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("ref: ${{ github.sha }}", "ref: main")),
            "main runs cancel each other": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}",
                                    "cancel-in-progress: true")),
            "different runner": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("pm-cli-vps", "unrelated-runner")),
            "retired runner label": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("pm-cli-vps", "pm-cli-qual-20260911-r8")),
            "job container moves the workspace": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("    timeout-minutes: 100\n",
                                    "    timeout-minutes: 100\n    container: ubuntu:24.04\n")),
            "suite outside the CI image": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("run: sh scripts/ci/in-ci-image.sh run bdd -- ", "run: ")),
            "renamed required check": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("\n  bdd:\n", "\n  bdd-check:\n")),
            "extra job": lambda r, _: text(r, WORKFLOW,
                lambda s: s + "  lint:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: true\n"),
            "missing fixture cleanup": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace(
                    "      - name: Clean up the job container and dependency fixture\n"
                    "        if: always()\n", "      - name: Unrelated step\n        if: always()\n")),
            "cleanup skipped on failure": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("if: always()", "if: success()")),
            "cleanup selects another job": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace(
                    "pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json",
                    "pmcli-bdd-unrelated-job.json")),
            "invalid agent PR workflow": lambda r, _: text(r, ".github/workflows/agent-pr.yml",
                lambda s: s.replace("runs-on: ubuntu-24.04", "runs-on: [self-hosted, Linux, X64, undeclared-label]")),
            "missing required validator": lambda r, _: (r / "scripts/ci/check-setup.sh").unlink(),
            "invalid shell": lambda r, _: text(r, "scripts/ci/check-setup.sh",
                lambda s: s + "\nif then\n"),
        }
        for name, mutate in cases.items():
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                root, inventory = self.fixture(directory)
                mutate(root, inventory)
                result = self.check(root, inventory)
                self.assertNotEqual(result.returncode, 0, name + " was accepted")

    def test_checks_job_is_frozen(self):
        def checks_block(s):
            return s[s.index("  checks:\n"):]

        cases = {
            "unknown job": (lambda s: s + checks_block(s).replace("  checks:\n", "  lint:\n", 1),
                            "unapproved job membership"),
            "missing checks job": (lambda s: s.split("\n  # The static checks", 1)[0] + "\n",
                                   "unapproved job membership"),
            "changed checks step": (lambda s: s.replace(
                "sh scripts/ci/check-docs.sh", "sh scripts/ci/check-docs.sh gofmt"),
                "unapproved checks executable step"),
            "checks outside the CI image": (lambda s: s.replace(
                "run: sh scripts/ci/in-ci-image.sh run checks -- ", "run: "),
                "unapproved checks executable step"),
            "checks waits for bdd": (lambda s: s.replace(
                "  checks:\n    if:", "  checks:\n    needs: bdd\n    if:"),
                "unapproved checks job capability"),
            "checks cleanup skipped on failure": (lambda s: s.replace(
                "      - name: Clean up the job container\n        if: always()",
                "      - name: Clean up the job container\n        if: success()"),
                "unapproved checks cleanup step"),
        }
        for name, (mutate, message) in cases.items():
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                root, inventory = self.fixture(directory)
                path = root / WORKFLOW
                original = path.read_text()
                changed = mutate(original)
                self.assertNotEqual(changed, original, name + " did not change the workflow")
                path.write_text(changed)
                result = self.check(root, inventory)
                self.assertNotEqual(result.returncode, 0, name + " was accepted")
                self.assertIn(message, result.stderr, name + " failed for another reason")

    def test_missing_required_tool_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            result = self.check(root, inventory, dict(os.environ, PATH=directory))
            self.assertNotEqual(result.returncode, 0)


CHECK_DOCS = ROOT / "scripts/ci/check-docs.sh"
HOOK_LIB = ROOT / "scripts/dev/hook-lib.sh"
CHECK_NAMES = ["setup", "python", "generate", "gofmt", "boundaries", "tools", "unit"]


class HookCheckSelection(unittest.TestCase):
    """Developer checks of the host check selection in scripts/dev/hook-lib.sh."""

    def lib(self, script, *args, env=None):
        return subprocess.run(
            ["/bin/sh", "-c", '. "$0"; ' + script, str(HOOK_LIB), *args],
            capture_output=True, text=True, timeout=30, env=env,
        )

    def test_live_suites_and_files_are_registered(self):
        # L-e1 P4: the checks job runs the live suites, and the setup check
        # requires the live files.
        script = CHECK_DOCS.read_text()
        for suite in ("test_runner_admission_live.py", "test_live_launcher.py", "test_live_cleanup.py"):
            self.assertIn(f"scripts/ci/{suite}", script)
        required = set(json.loads(POLICY.read_text())["required_files"])
        self.assertLessEqual({f"scripts/ci/{name}" for name in (
            "runner_admission_live.py", "runner-admission-live.sh", "test_runner_admission_live.py",
            "test_live_launcher.py", "live-cleanup-check.sh", "live-runner-check.sh", "test_live_cleanup.py")},
            required)

    def test_check_names_come_from_the_script(self):
        result = subprocess.run(["/bin/sh", str(CHECK_DOCS), "--list"],
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.split(), CHECK_NAMES)
        self.assertEqual(result.stderr, "")

    def test_retired_check_names_are_refused(self):
        usage = "usage: check-docs.sh [" + "|".join(CHECK_NAMES) + "]...\n"
        for name in ("atlas", "docs"):
            with self.subTest(name=name):
                result = subprocess.run(["/bin/sh", str(CHECK_DOCS), name],
                                        capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, "")
                self.assertEqual(result.stderr, usage)

    def test_macos_hooks_run_every_check(self):
        cases = {
            ("pre-push", "Darwin", tuple(CHECK_NAMES)): CHECK_NAMES,
            ("pre-commit", "Darwin", ("setup",)): ["setup"],
            ("pre-push", "Linux", tuple(CHECK_NAMES)): CHECK_NAMES,
            ("pre-commit", "Linux", ("setup",)): ["setup"],
        }
        for (hook, system, names), expected in cases.items():
            with self.subTest(hook=hook, system=system, names=names):
                result = self.lib('pm_host_checks "$@"', hook, system, *names)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.split(), expected)
                self.assertEqual(result.stderr, "")

    def test_pre_push_runs_one_unit_run_on_each_os(self):
        expected = [name for name in CHECK_NAMES if name != "unit"]
        for system in ("Darwin", "Linux"):
            with self.subTest(system=system):
                result = subprocess.run(
                    ["/bin/sh", "-c", '. "$0"; pm_push_checks "$1"', str(HOOK_LIB), system],
                    capture_output=True, text=True, timeout=30, cwd=ROOT)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.split(), expected)

    def test_macos_python_selection(self):
        def fake(directory, name, passes, version="3.12.9"):
            path = Path(directory) / name
            path.write_text("#!/bin/sh\n" + (
                f'case "$*" in *platform*) echo {version} ;; esac\nexit 0\n'
                if passes else "exit 1\n"))
            path.chmod(0o755)

        script = 'pm_select_python pre-push && printf "selected=%s\\n" "$PMCLI_PYTHON"'
        with tempfile.TemporaryDirectory() as directory:
            fake(directory, "python3", passes=False)
            fake(directory, "python3.12", passes=True)
            fake(directory, "python3.11", passes=True, version="3.11.11")
            result = self.lib(script, env={"PATH": directory})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout,
                             "pre-push: check-docs.sh uses Python 3.12.9\nselected=python3.12\n")
        with tempfile.TemporaryDirectory() as directory:
            fake(directory, "python3", passes=False)
            fake(directory, "python3.11", passes=False)
            result = self.lib(script, env={"PATH": directory})
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, "")
            self.assertIn("pre-push: no Python 3.11 or later on PATH imports tomllib, yaml, "
                          "markdown_it and mdurl.", result.stderr)

    def test_python_probe_matches_the_host_check_modules(self):
        import importlib.util
        expected = sys.version_info >= (3, 11) and all(
            importlib.util.find_spec(name) for name in ("tomllib", "yaml", "markdown_it", "mdurl"))
        result = self.lib('"$1" -I -B -c "$pm_python_probe"', sys.executable)
        self.assertEqual(result.returncode == 0, expected, result.stderr)


REPORT = ROOT / "scripts/ci/go-test-report.py"
GO_PACKAGES = ROOT / "scripts/ci/go-packages.sh"
CHECK_BDD = ROOT / "scripts/ci/check-bdd.sh"


# The tests of test_stack_fixture.py that hold processes without a timed pause:
# the receipt tests of #278 and the tests that #286 adds or changes.
FIXTURE_TESTS_WITHOUT_PAUSE = ("test_receipt_check", "test_group_signal", "test_phase_deadline",
                               "PhaseFixture", "test_cancellation_stops_descendant", "ExitedLeader",
                               "test_exited_leader")


def gherkin_caller_options(values):
    """The go-test-report.py options of the Gherkin run in check-bdd.sh.

    VALUES maps each shell word of the caller, for example
    "$gherkin_deadline", to its test value. An unknown shell word fails, so
    that a new caller option needs a new test value.
    """
    text = CHECK_BDD.read_text().replace("\\\n", " ")
    line = next(line for line in text.splitlines()
                if '--output "$RUNNER_TEMP/go-test-bdd.json"' in line)
    words = shlex.split(line)
    start = words.index("--output") + 2
    options = words[start:words.index("--", start)]
    unknown = [word for word in options if "$" in word and word not in values]
    if unknown:
        raise AssertionError(f"no test value for the Gherkin caller words {unknown}")
    return [values.get(word, word) for word in options]


def processes_holding(text):
    """Return each live process whose command line holds TEXT.

    The teardown check of the tests that start processes: no process that
    names the temporary folder of a test case may stay after the case. Linux
    reads /proc. Other systems read ps. A process that ends during the
    listing is not listed.
    """
    if sys.platform.startswith("linux"):
        found = []
        for entry in Path("/proc").iterdir():
            if entry.name.isdigit():
                try:
                    arguments = (entry / "cmdline").read_bytes()
                except (FileNotFoundError, ProcessLookupError):
                    continue
                if text.encode() in arguments:
                    found.append(entry.name + " " + arguments.replace(b"\0", b" ").decode(errors="replace"))
        return found
    listing = subprocess.run(["/bin/ps", "-A", "-ww", "-o", "pid=,args="], capture_output=True,
                             text=True, timeout=10, check=True)
    return [line.strip() for line in listing.stdout.splitlines() if text in line]


def no_fifo_reader(path):
    """Return True when no process has the FIFO PATH open for reading (ENXIO)."""
    try:
        os.close(os.open(path, os.O_WRONLY | os.O_NONBLOCK))
    except OSError as error:
        return error.errno == errno.ENXIO
    return False


class HeldProcess:
    """The identity of one live process, for one bounded exit wait.

    Open it only while the process cannot have exited, so that its process ID
    names no other process. Linux holds a pidfd. macOS holds a kqueue process
    filter registration: it reports the exit of the process that had the ID at
    registration, but macOS has no signal through it, so kill() sends nothing
    there. A system with neither interface fails the caller.
    """

    def __init__(self, pid):
        self.descriptor = self.queue = None
        if hasattr(os, "pidfd_open"):
            self.descriptor = os.pidfd_open(pid)
        elif hasattr(select, "kqueue"):
            self.queue = select.kqueue()
            change = select.kevent(pid, filter=select.KQ_FILTER_PROC,
                                   flags=select.KQ_EV_ADD | select.KQ_EV_ONESHOT,
                                   fflags=select.KQ_NOTE_EXIT)
            try:
                self.queue.control([change], 0, 0)
            except BaseException:
                self.queue.close()
                raise
        else:
            raise AssertionError("this system has neither pidfd_open nor kqueue: no process identity")

    def exits_within(self, seconds):
        """Wait one time for the exit event, for at most SECONDS."""
        if self.descriptor is not None:
            poller = select.poll()
            poller.register(self.descriptor, select.POLLIN)
            return bool(poller.poll(int(seconds * 1000)))
        events = self.queue.control(None, 1, seconds)
        return any(event.fflags & select.KQ_NOTE_EXIT for event in events)

    def kill(self):
        """Send SIGKILL to the held process. Never signal a process ID.

        Return True when the signal went to the held process. Return False
        when it has exited, or on macOS, which has no identity-bound signal.
        """
        if self.descriptor is None:
            return False
        try:
            signal.pidfd_send_signal(self.descriptor, signal.SIGKILL)
        except ProcessLookupError:
            return False
        return True

    def close(self):
        if self.descriptor is not None:
            os.close(self.descriptor)
        if self.queue is not None:
            self.queue.close()


class FifoHold:
    """The ownership and the release of a process that reads a FIFO that the test holds.

    The command gets the arguments ARGS: the FIFO "hold" ($0), the FIFO
    "ack" ($1) and the file "pid" ($2). The reader opens "hold" first,
    writes its process ID to "pid" and then writes "opened" to "ack". The
    test holds both FIFOs open for reading and writing, so no open blocks,
    and it never writes "hold". So the reader blocks until the release.

    The release takes the identity of the reader (HeldProcess) while it still
    blocks, closes the FIFOs and waits once for its exit. It sends no signal.
    Use it as a context manager, so that the release also runs when an
    assertion fails. READER says whether the reader acknowledged, was alive
    at the release and then ended.

    FifoHold owns a reader only after its acknowledgement. Before that, the
    starter of the command owns it and must collect it before the release
    (brahmagupta F2 at 52a87ec, REFUSED_KILL_CHILD).
    """

    # The reader steps of a command, in their order.
    READER = 'exec 3<"$0"; echo $$ > "$2"; echo opened > "$1"'

    def __init__(self, directory):
        self.hold, self.ack, self.pid = (Path(directory) / name for name in ("hold", "ack", "pid"))
        os.mkfifo(self.hold)
        os.mkfifo(self.ack)
        self.ends = [os.open(self.hold, os.O_RDWR), os.open(self.ack, os.O_RDWR)]
        self.args = [str(self.hold), str(self.ack), str(self.pid)]
        self.reader = {"acknowledged": False, "alive": False, "ended": False}
        self.released = False

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.release()
        return False

    def release(self):
        if self.released:
            return self.reader
        self.released = True
        handle = None
        try:
            if self.pid.exists() and self.pid.read_text().strip():
                # The reader wrote its ID before its acknowledgement, and it
                # blocks on the open FIFO, so its ID names no other process.
                self.reader["acknowledged"] = True
                handle = HeldProcess(int(self.pid.read_text().strip()))
                self.reader["alive"] = not handle.exits_within(0)
        finally:
            # The reader reads the end of the FIFO and stops.
            for end in self.ends:
                os.close(end)
            if handle is not None:
                try:
                    # One wait for the exit event of the held reader.
                    self.reader["ended"] = handle.exits_within(30)
                finally:
                    handle.close()
        return self.reader


def gated_background_script(pid_fifo, gate_fifo, release_fifo, work, last):
    """The shell command of the process cleanup test.

    The command starts one background process with its own standard streams.
    That process blocks on GATE_FIFO before it does any work, and then runs
    WORK with exec, so its process ID does not change. The command hands the
    ID over PID_FIFO, blocks on RELEASE_FIFO and then runs LAST. The test
    opens the gate only after it holds the identity handle. So the
    background process cannot end, and no parent can reap it, before the
    handle exists.
    """
    return (f'( read gate < "{gate_fifo}"; exec {work} ) </dev/null >/dev/null 2>&1 & '
            f'echo $! > "{pid_fifo}"; read release < "{release_fifo}"; {last}')


def go_event(action, package, test=None, **extra):
    event = {"Action": action, "Package": package}
    if test:
        event["Test"] = test
    event.update(extra)
    return json.dumps(event)


class GoTestReport(unittest.TestCase):
    """Developer checks of scripts/ci/go-test-report.py with synthetic events."""

    # The fake commands are script files, not "python -c CODE": with --expect,
    # the reporter refuses each option that the CI commands do not use.
    @classmethod
    def setUpClass(cls):
        cls.programs = tempfile.TemporaryDirectory()

    @classmethod
    def tearDownClass(cls):
        cls.programs.cleanup()

    def program(self, code):
        """Return the command words that run the Python CODE from a file."""
        path = Path(self.programs.name) / f"program-{len(os.listdir(self.programs.name))}.py"
        path.write_text(code)
        return [sys.executable, str(path)]

    def report(self, events, *options, exit_status=0, raw=None, expect=None, command=None,
               env=None):
        with tempfile.TemporaryDirectory() as directory:
            stream = Path(directory) / "events.json"
            stream.write_text(raw if raw is not None else "\n".join(events) + ("\n" if events else ""))
            summary = Path(directory) / "summary.md"
            fake = command or [*self.program(
                "import sys; sys.stdout.write(open(sys.argv[1]).read()); sys.exit(int(sys.argv[2]))"),
                str(stream), str(exit_status), "-count=1"]
            if expect is not None:
                expected = Path(directory) / "expected.txt"
                expected.write_text("".join(f"{line}\n" for line in expect))
                options = ("--expect", str(expected), *options)
            options = [str(summary) if option == "SUMMARY" else option for option in options]
            result = subprocess.run(
                [sys.executable, "-I", "-B", str(REPORT), "--output", str(Path(directory) / "out.json"),
                 *options, "--", *fake],
                capture_output=True, text=True, timeout=120,
                env=dict(os.environ, **(env or {})))
            text = summary.read_text() if summary.exists() else ""
        return result, text

    def test_package_without_test_files_is_not_a_test_skip(self):
        events = [go_event("skip", "m/empty", Elapsed=0),
                  go_event("run", "m/full", "TestOK"), go_event("pass", "m/full", "TestOK"),
                  go_event("pass", "m/full", Elapsed=1.5)]
        result, summary = self.report(events, "--fail-on-skip", "--title", "Unit run",
                                      "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("SKIP      0.0s m/empty (no test files)", result.stdout)
        self.assertIn("PASS      1.5s m/full", result.stdout)
        self.assertIn("### Unit run", summary)
        self.assertIn("Result: PASSED.", summary)
        self.assertIn("| SKIP (no test files) | 0.0s | `m/empty` |", summary)
        self.assertIn("| PASS | 1.5s | `m/full` |", summary)

    def test_skipped_test_fails_only_with_fail_on_skip(self):
        events = [go_event("run", "m/p", "TestSkip"),
                  go_event("output", "m/p", "TestSkip", Output="    p_test.go:9: canary\n"),
                  go_event("skip", "m/p", "TestSkip"), go_event("pass", "m/p", Elapsed=0.2)]
        result, summary = self.report(events, "--fail-on-skip", "--title", "Stack run",
                                      "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 1)
        self.assertIn("go test: skipped test fails this run: m/p TestSkip", result.stdout)
        self.assertIn("p_test.go:9: canary", result.stdout)
        self.assertIn("Result: FAILED.", summary)
        self.assertIn("- `m/p TestSkip`", summary)
        result, _ = self.report(events)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("1 skipped tests", result.stdout)

    def test_failed_test_and_build_failure_fail(self):
        failed = [go_event("run", "m/p", "TestBad"), go_event("fail", "m/p", "TestBad"),
                  go_event("fail", "m/p", Elapsed=0.1)]
        result, summary = self.report(failed, "--title", "Unit run", "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 1)
        self.assertIn("::group::FAIL m/p TestBad", result.stdout)
        self.assertIn("- `m/p TestBad`", summary)
        broken = [json.dumps({"Action": "build-fail", "ImportPath": "m/q [m/q.test]"})]
        result, _ = self.report(broken)
        self.assertEqual(result.returncode, 1)
        self.assertIn("::group::FAIL m/q [m/q.test]", result.stdout)

    def test_test_without_result_prints_its_output_and_fails(self):
        # The shape of a real `go test -json` stream (Go 1.27.1) when a
        # goroutine of a test panics: output under the running test, no test
        # result, and a package fail.
        events = [go_event("start", "m/a"),
                  go_event("run", "m/a", "TestFirst"),
                  go_event("output", "m/a", "TestFirst", Output="=== RUN   TestFirst\n"),
                  go_event("output", "m/a", "TestFirst", Output="--- PASS: TestFirst (0.00s)\n"),
                  go_event("pass", "m/a", "TestFirst", Elapsed=0),
                  go_event("run", "m/a", "TestHelperGoroutinePanics"),
                  go_event("output", "m/a", "TestHelperGoroutinePanics",
                           Output="=== RUN   TestHelperGoroutinePanics\n"),
                  go_event("output", "m/a", "TestHelperGoroutinePanics",
                           Output="panic: send on closed channel\n"),
                  go_event("output", "m/a", "TestHelperGoroutinePanics",
                           Output="goroutine 37 [running]:\n"),
                  go_event("output", "m/a", "TestHelperGoroutinePanics",
                           Output="m/a.TestHelperGoroutinePanics.func1()\n"),
                  go_event("output", "m/a", Output="FAIL\tm/a\t0.583s\n"),
                  go_event("fail", "m/a", Elapsed=0.583)]
        result, summary = self.report(events, "--title", "Unit run", "--summary-file", "SUMMARY")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("::group::FAIL (no result) m/a TestHelperGoroutinePanics\n"
                      "=== RUN   TestHelperGoroutinePanics\n"
                      "panic: send on closed channel\n"
                      "goroutine 37 [running]:\n"
                      "m/a.TestHelperGoroutinePanics.func1()\n"
                      "::endgroup::\n", result.stdout)
        self.assertIn("go test: a test started and has no result: m/a TestHelperGoroutinePanics\n",
                      result.stdout)
        self.assertIn("go test: 1 packages, 2 failures, 0 skipped tests", result.stdout)
        self.assertNotIn("--- PASS: TestFirst", result.stdout)
        self.assertIn("- `m/a TestHelperGoroutinePanics` (no result)", summary)
        self.assertIn("- a test started and has no result: m/a TestHelperGoroutinePanics", summary)

    def test_test_without_result_fails_a_passing_package(self):
        events = [go_event("run", "m/p", "TestLost"),
                  go_event("run", "m/p", "TestLost/sub"),
                  go_event("output", "m/p", "TestLost/sub", Output="    p_test.go:4: last line\n"),
                  go_event("pass", "m/p", "TestLost/sub"),
                  go_event("pass", "m/p", Elapsed=0.3)]
        result, summary = self.report(events, "--title", "Stack run", "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("PASS      0.3s m/p", result.stdout)
        self.assertIn("::group::FAIL (no result) m/p TestLost\n", result.stdout)
        self.assertIn("go test: a test started and has no result: m/p TestLost\n", result.stdout)
        self.assertNotIn("no result: m/p TestLost/sub", result.stdout)
        self.assertIn("go test: 1 packages, 1 failures, 0 skipped tests, exit 0", result.stdout)
        self.assertIn("Result: FAILED.", summary)

    def test_gherkin_run_fails_without_a_package_result(self):
        # Valid scenario rows and godog totals, but the stream has no package
        # result for the Gherkin package. The Gherkin caller in check-bdd.sh
        # passes --expect, so the run fails and names the package.
        suite = [go_event("run", "m/acc", "TestFeatures"),
                 go_event("run", "m/acc", "TestFeatures/first"),
                 go_event("output", "m/acc", "TestFeatures/first",
                          Output="  Scenario: first # features/a.feature:3\n"),
                 go_event("pass", "m/acc", "TestFeatures/first"),
                 go_event("output", "m/acc", "TestFeatures", Output="1 scenarios (1 passed)\n"),
                 go_event("output", "m/acc", "TestFeatures", Output="3 steps (3 passed)\n"),
                 go_event("pass", "m/acc", "TestFeatures")]
        # The intended set comes from go list with the same pattern as the
        # go test command of the Gherkin run.
        script = CHECK_BDD.read_text()
        self.assertIn('exec go -- list "$pm_gherkin_package/..."', script)
        self.assertIn('-timeout 40m "$pm_gherkin_package/..."', script)
        for name, events, status in (("no package result", suite, 1),
                                     ("package result", suite + [go_event("pass", "m/acc", Elapsed=1.0)], 0)):
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                expected = Path(directory) / "expected.txt"
                expected.write_text("m/acc\n")
                options = gherkin_caller_options({
                    "$gherkin_deadline": str(int(time.time()) + 600),
                    "$GITHUB_STEP_SUMMARY": "SUMMARY",
                    "$RUNNER_TEMP/bdd-gherkin-expected.txt": str(expected)})
                result, summary = self.report(events, *options)
                self.assertEqual(result.returncode, status, result.stdout + result.stderr)
                self.assertIn("go test: intended set: 1 packages from go list; package results: "
                              + ("0" if status else "1"), result.stdout)
                self.assertIn("| PASS | first | `features/a.feature:3` |", summary)
                self.assertIn("1 scenarios (1 passed)", summary)
                if status:
                    self.assertIn("go test: no package result for m/acc", result.stdout)
                    self.assertIn("- no package result for m/acc\n", summary)
                else:
                    self.assertNotIn("Problems:", summary)

    def test_gherkin_summary_names_tests_with_no_result(self):
        # The Gherkin caller has --scenarios and no --title, so only the
        # scenario section of the job summary can name these tests.
        events = [go_event("run", "m/acc", "TestFeatures"),
                  go_event("run", "m/acc", "TestFeatures/first"),
                  go_event("output", "m/acc", "TestFeatures/first",
                           Output="  Scenario: first # features/a.feature:3\n"),
                  go_event("pass", "m/acc", "TestFeatures/first"),
                  go_event("run", "m/acc", "TestFeatures/second"),
                  go_event("output", "m/acc", "TestFeatures/second",
                           Output="  Scenario: second # features/a.feature:9\n"),
                  go_event("output", "m/acc", "TestFeatures/second", Output="panic: boom\n"),
                  go_event("output", "m/acc", Output="FAIL\tm/acc\t1.0s\n"),
                  go_event("fail", "m/acc", Elapsed=1.0)]
        with tempfile.TemporaryDirectory() as directory:
            expected = Path(directory) / "expected.txt"
            expected.write_text("m/acc\n")
            options = gherkin_caller_options({
                "$gherkin_deadline": str(int(time.time()) + 600),
                "$GITHUB_STEP_SUMMARY": "SUMMARY",
                "$RUNNER_TEMP/bdd-gherkin-expected.txt": str(expected)})
            self.assertNotIn("--title", options)
            result, summary = self.report(events, *options)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("### Gherkin scenarios", summary)
        self.assertIn("No godog totals were found.", summary)
        self.assertIn("Tests with no result:\n\n- `m/acc TestFeatures`\n- `m/acc TestFeatures/second`\n",
                      summary)
        self.assertIn("- a test started and has no result: m/acc TestFeatures/second\n", summary)
        self.assertIn("Summary error: the godog scenario total is missing.", summary)

    # Issue #350. The godog v0.16.0 pretty format prints each step keyword and
    # text in the color of the step status: green passed, cyan skipped. A data
    # table row is cyan too, but it starts with "|".
    @staticmethod
    def step_line(color, keyword, text):
        return f"    \x1b[{color}m{keyword}\x1b[0m \x1b[{color}m{text}\x1b[0m \x1b[1;30m# steps.go:9\x1b[0m\n"

    def gherkin_suite(self, scenarios, totals, extra=()):
        """A Gherkin run stream. SCENARIOS maps a name to (result, step lines)."""
        events = [go_event("run", "m/acc", "TestFeatures")]
        for line, (name, (result, steps)) in enumerate(scenarios.items(), 3):
            key = "TestFeatures/" + name
            events += [go_event("run", "m/acc", key),
                       go_event("output", "m/acc", key,
                                Output=f"  \x1b[1;37mScenario:\x1b[0m {name} \x1b[1;30m# a.feature:{line}\x1b[0m\n")]
            events += [go_event("output", "m/acc", key, Output=step) for step in steps]
            events.append(go_event(result, "m/acc", key))
        events += [go_event("output", "m/acc", "TestFeatures", Output=total + "\n") for total in totals]
        events += [go_event("pass", "m/acc", "TestFeatures"), *extra, go_event("pass", "m/acc", Elapsed=1.0)]
        return events

    def gherkin_report(self, events):
        """Run the reporter with the exact options of the Gherkin caller in check-bdd.sh."""
        with tempfile.TemporaryDirectory() as directory:
            expected = Path(directory) / "expected.txt"
            expected.write_text("m/acc\n")
            options = gherkin_caller_options({
                "$gherkin_deadline": str(int(time.time()) + 600),
                "$GITHUB_STEP_SUMMARY": "SUMMARY",
                "$RUNNER_TEMP/bdd-gherkin-expected.txt": str(expected)})
            return self.report(events, *options)

    def test_gherkin_skip_causes_come_from_the_totals(self):
        # C0 of #286b (run 44133272339) named skips while the totals had none:
        # godog prints outline template steps in cyan. The stream is the local
        # `PM_ACCEPTANCE_TAGS=@pmgen go test -json ./tests/acceptance/` output.
        real = (ROOT / "scripts/ci/testdata/godog-pmgen-outlines.jsonl").read_text().splitlines()

        def gherkin(events, raw=None):
            with tempfile.TemporaryDirectory() as directory:
                expected = Path(directory) / "expected.txt"
                expected.write_text(("github.com/polymetrics-ai/pm-cli/tests/acceptance" if raw else "m/acc") + "\n")
                options = gherkin_caller_options({
                    "$gherkin_deadline": str(int(time.time()) + 600),
                    "$GITHUB_STEP_SUMMARY": "SUMMARY",
                    "$RUNNER_TEMP/bdd-gherkin-expected.txt": str(expected)})
                return self.report(events, *options, raw=raw)

        with self.subTest(case="real outlines, no skip in the totals"):
            result, summary = gherkin([], "\n".join(real) + "\n")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("12 scenarios (12 passed)\n77 steps (77 passed)\n", result.stdout)
            self.assertNotIn("skipped", result.stdout.replace("0 skipped tests", ""))
            self.assertNotIn("Problems:", summary)
        with self.subTest(case="cyan step names alone do not fail a run"):
            events = self.gherkin_suite({"first": ("pass", [self.step_line(36, "Given", "a state")])},
                                        ["1 scenarios (\x1b[32m1 passed\x1b[0m)", "1 steps (\x1b[32m1 passed\x1b[0m)"])
            result, _ = gherkin(events)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertNotIn("skipped step", result.stdout)
        with self.subTest(case="real outlines and one real skipped step"):
            # One more scenario with one skipped step, before the godog totals,
            # and the totals changed to match. Only that step is named.
            package = "github.com/polymetrics-ai/pm-cli/tests/acceptance"
            key = "TestFeatures/Synthetic_skip"
            extra = [go_event("run", package, key),
                     go_event("output", package, key,
                              Output="  [1;37mScenario:[0m Synthetic skip [1;30m# features/synthetic.feature:3[0m\n"),
                     go_event("output", package, key, Output=self.step_line(32, "When", "a step passes")),
                     go_event("output", package, key, Output=self.step_line(36, "Then", "the step is skipped")),
                     go_event("pass", package, key)]
            at = next(index for index, line in enumerate(real) if "12 scenarios (" in line)
            changed = real[:at] + extra + real[at:]
            text = "\n".join(changed).replace("12 scenarios (", "13 scenarios (").replace(
                "12 passed", "13 passed").replace("77 steps (", "79 steps (").replace(
                "77 passed", "78 passed, \\u001b[36m1 skipped\\u001b[0m") + "\n"
            result, summary = gherkin([], text)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            causes = [line for line in result.stdout.splitlines() if "skipped step" in line or "skipped scenario" in line]
            self.assertEqual(causes, ["go test: the godog step total has 1 skipped steps",
                                      "go test: skipped step: Synthetic skip / Then the step is skipped"])
            self.assertIn("- skipped step: Synthetic skip / Then the step is skipped\n", summary)

    def test_gherkin_run_passes_with_a_cyan_table_row(self):
        table = "      | \x1b[36mname\x1b[0m | \x1b[36mvalue\x1b[0m |\n"
        events = self.gherkin_suite(
            {"first": ("pass", [self.step_line(32, "Given", "a table"), table,
                                self.step_line(32, "Then", "it passes")])},
            ["1 scenarios (\x1b[32m1 passed\x1b[0m)", "2 steps (\x1b[32m2 passed\x1b[0m)"])
        result, summary = self.gherkin_report(events)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("| PASS | first | `a.feature:3` |", summary)
        self.assertNotIn("Problems:", summary)

    def test_gherkin_run_fails_on_zero_scenarios(self):
        events = self.gherkin_suite({}, ["No scenarios", "No steps"])
        result, summary = self.gherkin_report(events)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("go test: 0 scenarios ran\n", result.stdout)
        self.assertIn("- 0 scenarios ran\n", summary)
        self.assertIn("No scenarios", summary)

    def test_gherkin_run_fails_on_a_skipped_step(self):
        events = self.gherkin_suite(
            {"first": ("pass", [self.step_line(32, "Given", "a state"),
                                self.step_line(36, "Then", "it is skipped")])},
            ["1 scenarios (\x1b[32m1 passed\x1b[0m)",
             "2 steps (\x1b[32m1 passed\x1b[0m, \x1b[36m1 skipped\x1b[0m)"])
        result, summary = self.gherkin_report(events)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        for cause in ("skipped step: first / Then it is skipped",
                      "the godog step total has 1 skipped steps"):
            self.assertIn(f"go test: {cause}\n", result.stdout)
            self.assertIn(f"- {cause}\n", summary)
        self.assertIn("1 scenarios (1 passed)", summary)

    def test_gherkin_run_fails_on_a_skipped_scenario(self):
        events = self.gherkin_suite(
            {"first": ("pass", [self.step_line(32, "Given", "a state")]),
             "second": ("pass", [self.step_line(36, "Given", "a state"), self.step_line(36, "Then", "more")]),
             "third": ("skip", [])},
            ["3 scenarios (\x1b[32m3 passed\x1b[0m)",
             "3 steps (\x1b[32m1 passed\x1b[0m, \x1b[36m2 skipped\x1b[0m)"])
        result, summary = self.gherkin_report(events)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        for cause in ("skipped scenario: second", "the godog step total has 2 skipped steps"):
            self.assertIn(f"go test: {cause}\n", result.stdout)
            self.assertIn(f"- {cause}\n", summary)
        self.assertNotIn("skipped scenario: first", result.stdout)
        # A Go test skip event of "third": --fail-on-skip names it, not godog.
        self.assertIn("go test: skipped test fails this run: m/acc TestFeatures/third\n", result.stdout)
        self.assertIn("- `m/acc TestFeatures/third`\n", summary)
        self.assertIn("3 scenarios (3 passed)", summary)

    def test_gherkin_run_fails_on_a_skipped_go_test(self):
        # A Gherkin package test calls t.Skip: the untitled summary names it.
        skipped = [go_event("run", "m/acc", "TestOther"),
                   go_event("output", "m/acc", "TestOther", Output="    other_test.go:5: canary\n"),
                   go_event("skip", "m/acc", "TestOther")]
        events = self.gherkin_suite({"first": ("pass", [self.step_line(32, "Given", "a state")])},
                                    ["1 scenarios (\x1b[32m1 passed\x1b[0m)", "1 steps (\x1b[32m1 passed\x1b[0m)"],
                                    skipped)
        result, summary = self.gherkin_report(events)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("go test: skipped test fails this run: m/acc TestOther\n", result.stdout)
        self.assertIn("Skipped tests (each one fails this run):\n\n- `m/acc TestOther`\n", summary)
        self.assertIn("1 scenarios (1 passed)", summary)
        self.assertIn("| PASS | first | `a.feature:3` |", summary)
        self.assertNotIn("Summary error", summary)

    def test_paused_parallel_tests_with_results_pass(self):
        events = [go_event("run", "m/p", "TestA"),
                  go_event("output", "m/p", "TestA", Output="=== RUN   TestA\n"),
                  go_event("pause", "m/p", "TestA"),
                  go_event("run", "m/p", "TestB"),
                  go_event("output", "m/p", "TestB", Output="=== RUN   TestB\n"),
                  go_event("pause", "m/p", "TestB"),
                  go_event("cont", "m/p", "TestA"),
                  go_event("cont", "m/p", "TestB"),
                  go_event("output", "m/p", "TestB", Output="--- PASS: TestB (0.01s)\n"),
                  go_event("pass", "m/p", "TestB", Elapsed=0.01),
                  go_event("output", "m/p", "TestA", Output="--- PASS: TestA (0.02s)\n"),
                  go_event("pass", "m/p", "TestA", Elapsed=0.02),
                  go_event("pass", "m/p", Elapsed=0.1)]
        result, summary = self.report(events, "--fail-on-skip", "--title", "Unit run",
                                      "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("no result", result.stdout)
        self.assertNotIn("=== RUN", result.stdout)
        self.assertIn("go test: 1 packages, 0 failures, 0 skipped tests, exit 0", result.stdout)
        self.assertIn("Result: PASSED.", summary)

    def passing(self, *packages):
        events = []
        for package in packages:
            events += [go_event("run", package, "TestOK"), go_event("pass", package, "TestOK"),
                       go_event("pass", package, Elapsed=0.1)]
        return events

    def test_expected_packages_pass_once_each(self):
        result, summary = self.report(self.passing("m/a", "m/b"), "--title", "Run",
                                      "--summary-file", "SUMMARY", expect=["m/a", "m/b"])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Result: PASSED.", summary)
        count = "intended set: 2 packages from go list; package results: 2"
        self.assertIn(f"go test: {count}", result.stdout)
        self.assertIn(f"The {count}.", summary)

    def test_missing_duplicated_and_unexpected_package_results_fail(self):
        cases = {
            "no package result for m/b": (self.passing("m/a"), ["m/a", "m/b"]),
            "2 package results for m/a": (self.passing("m/a", "m/a"), ["m/a"]),
            "a package result for m/c, which is not in the expected list":
                (self.passing("m/a", "m/c"), ["m/a"]),
        }
        for message, (events, expect) in cases.items():
            with self.subTest(message=message):
                result, summary = self.report(events, "--title", "Run", "--summary-file", "SUMMARY",
                                              expect=expect)
                self.assertEqual(result.returncode, 1)
                self.assertIn(f"go test: {message}", result.stdout)
                self.assertIn(f"- {message}", summary)
                self.assertIn("Result: FAILED.", summary)

    def test_empty_and_truncated_streams_fail(self):
        result, _ = self.report([], expect=["m/a"])
        self.assertEqual(result.returncode, 1)
        self.assertIn("go test: the go test event stream is empty", result.stdout)
        self.assertIn("go test: no package result for m/a", result.stdout)
        events = self.passing("m/a")
        truncated = "\n".join(events + [go_event("run", "m/b", "TestOK")]) + '\n{"Action":"pa'
        result, _ = self.report([], raw=truncated, expect=["m/a", "m/b"])
        self.assertEqual(result.returncode, 1)
        self.assertIn("go test: the stream has 1 lines that are not JSON events", result.stdout)
        self.assertIn("go test: no package result for m/b", result.stdout)

    def test_nonzero_command_status_fails_with_a_passing_stream(self):
        result, _ = self.report(self.passing("m/a"), expect=["m/a"], exit_status=3)
        self.assertEqual(result.returncode, 3)

    def test_test_filters_are_refused(self):
        for options, env in ((["-run=TestOK"], None), (["-tags", "x"], None), ([], {"GOFLAGS": "-p=2 -short"})):
            with self.subTest(options=options, env=env):
                command = [*self.program("raise SystemExit(0)"), "-count=1", *options]
                result, _ = self.report([], expect=["m/a"], command=command, env=env)
                self.assertEqual(result.returncode, 2)
                self.assertIn("refused: the command selects or filters tests", result.stderr)
        command = self.program("raise SystemExit(0)")
        result, _ = self.report([], expect=["m/a"], command=command)
        self.assertEqual(result.returncode, 2)
        self.assertIn("refused: the command does not contain -count=1", result.stderr)

    # A child that never ends by itself: it prints one event and then blocks
    # on a FIFO that the test holds open and never writes. When the test
    # closes the FIFO, a child that is still there reads its end and stops.
    HELD_CHILD = ("import json, sys\n"
                  "print(json.dumps({'Action': 'pass', 'Package': 'm/a', 'Elapsed': 0.1}), flush=True)\n"
                  "open(sys.argv[1]).read()\n")

    def held_child_report(self, *options, expect):
        with tempfile.TemporaryDirectory() as directory:
            hold = Path(directory) / "hold"
            os.mkfifo(hold)
            hold_end = os.open(hold, os.O_RDWR)
            try:
                command = [*self.program(self.HELD_CHILD), str(hold), "-count=1"]
                return self.report([], *options, expect=expect, command=command)
            finally:
                os.close(hold_end)

    def test_each_spelling_of_a_filter_or_a_count_is_refused(self):
        # The command already has -count=1. Each case adds one more option,
        # in the command or in GOFLAGS, and the reporter must refuse it
        # before the command runs.
        refused = [
            (["-test.run=X"], None), (["-run", "X"], None), (["--run=X"], None),
            (["-test.skip=Y"], None), (["-test.list=."], None), (["-test.short"], None),
            (["-test.failfast"], None), (["-tags", "x"], None),
            (["-count=0"], None), (["-count", "0"], None), (["-test.count=0"], None),
            (["-test.count=1"], None), (["-args", "-test.run=X"], None), (["-count"], None),
            ([], {"GOFLAGS": "-test.run=X"}), ([], {"GOFLAGS": "-count=0"}),
            ([], {"GOFLAGS": "-p=2 -test.count=2"}), ([], {"GOFLAGS": "-test.short"}),
        ]
        for options, env in refused:
            with self.subTest(options=options, env=env):
                command = [*self.program("raise SystemExit(0)"), "-count=1", *options]
                result, _ = self.report([], expect=["m/a"], command=command, env=env)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("refused: the command", result.stderr)
        # One count, equal to 1, with other options, passes the guard. The
        # fake command then prints no event, so the run fails later.
        for options, env in (([], {"GOFLAGS": "-p=2"}), (["-p", "1", "-timeout", "15m", "-json"], None)):
            with self.subTest(accepted=options, env=env):
                command = [*self.program("raise SystemExit(0)"), "-count=1", *options]
                result, _ = self.report([], expect=["m/a"], command=command, env=env)
                self.assertNotIn("refused:", result.stderr)
                self.assertIn("go test: the go test event stream is empty", result.stdout)

    def test_goflags_forms_that_go_reads_differently_are_refused(self):
        # Deferred item (b): go reads a quoted GOFLAGS field without its
        # quotes. The guard refuses each quoted form, each field that is not
        # an option, and a GOFLAGS -count with no =value. -p=2 and -race stay
        # accepted.
        program = [*self.program("raise SystemExit(0)"), "-json", "-count=1"]
        for goflags in ("'-run=^$'", '"-run=^$"', "-p=2 '-run=^$'", "-p=2\t'-short'", "-p=2 '-p=2'",
                        "-p=2 x", "-p 2", "-timeout 10m", "-count"):
            with self.subTest(goflags=goflags):
                result, _ = self.report([], expect=["m/a"], command=program, env={"GOFLAGS": goflags})
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("refused: GOFLAGS has", result.stderr)
        for goflags, extra in (("-p=2", []), ("-p=2 -race", []), ("-p=2\t-race", []), ("-p=2", ["-race"])):
            with self.subTest(accepted=goflags, extra=extra):
                result, _ = self.report([], expect=["m/a"], command=[*program, *extra], env={"GOFLAGS": goflags})
                self.assertNotIn("refused:", result.stderr)

    def test_only_the_ci_go_test_options_pass_the_guard(self):
        # crrao N1 at 958cd5e: `go test -exec /usr/bin/true` runs no test, and
        # each package passes. With --expect, the reporter accepts only the
        # options of the CI commands, and -race for #312.
        module = self.load_reporter()

        def command_words(path, marker):
            text = path.read_text().replace("\\\n", " ")
            line = next(line for line in text.splitlines() if marker in line)
            words = shlex.split(line.split("||")[0])
            # The command starts after the "--" that ends the reporter options.
            return words[words.index("--", words.index("scripts/ci/go-test-report.py")) + 1:]

        callers = {
            "unit run": command_words(CHECK_DOCS, '"$py" -I -B scripts/ci/go-test-report.py'),
            "stack run": command_words(CHECK_BDD, '--output "$RUNNER_TEMP/go-test-stack.json"'),
            "Gherkin run": command_words(CHECK_BDD, '--output "$RUNNER_TEMP/go-test-bdd.json"'),
        }
        for name, words in callers.items():
            self.assertIn("test", words)
            self.assertIn("-count=1", words)
            for goflags, extra in (("-p=2", []), ("-p=2", ["-race"]), ("-p=2 -race", [])):
                with self.subTest(caller=name, goflags=goflags, extra=extra), \
                        mock.patch.dict(os.environ, {"GOFLAGS": goflags}):
                    self.assertIsNone(module.filter_problem([*words, *extra]), words)
        base = [*self.program("raise SystemExit(0)"), "-json", "-count=1"]
        refused = []
        for option in ("exec", "overlay", "toolexec", "unknownoption"):
            refused += [([f"-{option}=/usr/bin/true"], None, "refused: the command uses the option"),
                        ([f"-{option}", "/usr/bin/true"], None, "refused: the command uses the option"),
                        ([f"--{option}=/usr/bin/true"], None, "refused: the command uses the option"),
                        ([f"-test.{option}=/usr/bin/true"], None, "refused: the command uses the option"),
                        ([], {"GOFLAGS": f"-p=2 -{option}=/usr/bin/true"}, "refused: the command uses the option")]
        # Every GOFLAGS option with a value needs "=value", as go reads it.
        for goflags in ("-p", "-timeout", "-p=2 -timeout"):
            refused.append(([], {"GOFLAGS": goflags}, "refused: GOFLAGS has"))
        for options, env, message in refused:
            with self.subTest(options=options, env=env):
                result, _ = self.report([], expect=["m/a"], command=[*base, *options], env=env)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn(message, result.stderr)

    def test_quoted_goflags_are_refused_for_a_real_go_package(self):
        # Deferred item (b), with a real Go package. go removes the quotes of
        # a GOFLAGS field, so '-run=^$' selects no test, and the package still
        # reports a pass. The reporter must refuse the run before go starts.
        # GOFLAGS=-p=2 must still run the real test. The go command comes from
        # the shared toolchain through tools/bootstrap.py, with GOPROXY=off,
        # GOTOOLCHAIN=local and no module to download.
        with tempfile.TemporaryDirectory() as directory:
            package = Path(directory) / "quoted"
            package.mkdir()
            (package / "go.mod").write_text("module example.com/quoted\n\ngo 1.22\n")
            (package / "quoted_test.go").write_text(
                'package quoted\n\nimport "testing"\n\nfunc TestRuns(t *testing.T) { t.Log("ran") }\n')
            expected = Path(directory) / "expected.txt"
            expected.write_text("example.com/quoted\n")
            go_test = [sys.executable, str(ROOT / "tools/bootstrap.py"), "exec", "go", "--",
                       "test", "-json", "-count=1", "./..."]

            def run(goflags):
                env = dict(os.environ, CGO_ENABLED="0", GOPROXY="off", GOWORK="off",
                           GOTOOLCHAIN="local", GOFLAGS=goflags)
                return subprocess.run(
                    [sys.executable, "-I", "-B", str(REPORT), "--output", str(Path(directory) / "out.json"),
                     "--expect", str(expected), "--", *go_test],
                    cwd=package, env=env, capture_output=True, text=True, timeout=600)

            for goflags in ("'-run=^$'", "-p=2 '-run=^$'", '"-run=^$"'):
                with self.subTest(goflags=goflags):
                    result = run(goflags)
                    self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                    self.assertIn("refused: GOFLAGS has a quote character", result.stderr)
                    self.assertNotIn("PASS", result.stdout)
            result = run("-p=2")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("go test: intended set: 1 packages from go list; package results: 1", result.stdout)
            events = [json.loads(line) for line in (Path(directory) / "out.json").read_text().splitlines()]
            self.assertIn({"pass"}, [{event["Action"]} for event in events if event.get("Test") == "TestRuns"])

    def test_deadline_stops_the_command_and_still_reports(self):
        started = time.monotonic()
        result, summary = self.held_child_report(
            "--deadline", str(int(time.time()) + 2), "--title", "Run", "--summary-file", "SUMMARY",
            expect=["m/a", "m/b"])
        self.assertLess(time.monotonic() - started, 30)
        self.assertEqual(result.returncode, 124)
        self.assertIn("go test: the command did not end before its deadline and was stopped (exit 124)",
                      result.stdout)
        self.assertIn("PASS      0.1s m/a", result.stdout)
        self.assertIn("go test: no package result for m/b", result.stdout)
        self.assertIn("Result: FAILED.", summary)

    def test_timeout_stops_the_command_and_still_reports(self):
        started = time.monotonic()
        result, _ = self.held_child_report("--timeout", "2", "--deadline", str(int(time.time()) + 600),
                                           expect=["m/a"])
        self.assertLess(time.monotonic() - started, 30)
        self.assertEqual(result.returncode, 124)
        self.assertIn("go test: the command did not end before its deadline", result.stdout)
        self.assertIn("go test: intended set: 1 packages from go list; package results: 1", result.stdout)

    def test_no_test_process_outlives_the_command(self):
        with tempfile.TemporaryDirectory() as directory:
            fifos = {name: Path(directory) / name for name in ("pid", "gate", "release", "hold")}
            for path in fifos.values():
                os.mkfifo(path)
            expected = Path(directory) / "expected.txt"
            expected.write_text("m/a\n")
            # After its gate, the background process blocks on a read of the
            # hold FIFO, which the test never writes. So it never ends by
            # itself. When the test closes the FIFO, it reads its end.
            script = gated_background_script(
                fifos["pid"], fifos["gate"], fifos["release"], 'cat < "%s"' % fifos["hold"],
                "echo '{\"Action\":\"pass\",\"Package\":\"m/a\"}'")
            command_file = Path(directory) / "command.sh"
            command_file.write_text(script + "\n")
            # The test holds each FIFO open for reading and writing, so no open
            # blocks and no write meets a closed FIFO.
            ends = {name: os.open(path, os.O_RDWR) for name, path in fifos.items()}
            handle = None
            try:
                reporter = subprocess.Popen(
                    [sys.executable, "-I", "-B", str(REPORT), "--output", str(Path(directory) / "out.json"),
                     "--expect", str(expected), "--", "/bin/sh", str(command_file), "-count=1"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    # One bounded read of the process ID.
                    ready, _, _ = select.select([ends["pid"]], [], [], 30)
                    self.assertTrue(ready, "the command did not hand over its background process ID")
                    pid = int(os.read(ends["pid"], 64).decode().strip())
                    # The background process waits at its gate, so it is alive
                    # and its ID names no other process.
                    handle = HeldProcess(pid)
                    self.assertIsNone(reporter.poll(), "the reporter ended before the handle was open")
                finally:
                    os.write(ends["gate"], b"go\n")
                    os.write(ends["release"], b"go\n")
                    stdout, stderr = reporter.communicate(timeout=60)
                self.assertEqual(reporter.returncode, 0, stdout + stderr)
                # One wait for the exit event, with one time limit. The reporter
                # sends SIGKILL to the process group before it exits.
                if not handle.exits_within(5):
                    handle.kill()
                    self.fail("the background process of the command is still alive")
            finally:
                if handle is not None:
                    handle.close()
                for end in ends.values():
                    os.close(end)

    def load_reporter(self):
        spec = importlib.util.spec_from_file_location("go_test_report", REPORT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_group_is_killed_before_the_command_is_reaped(self):
        # The command exits and leaves a group member that never ends by
        # itself: cat on a FIFO that the test holds open and never writes.
        # The reporter must send its process-group SIGKILL while the command
        # is not reaped yet, so that the group ID names no other group. The
        # check records each killpg call and the reap of the command.
        module = self.load_reporter()
        events = []
        real_killpg = os.killpg

        class RecordingPopen(subprocess.Popen):
            def wait(self, timeout=None):
                status = super().wait(timeout)
                events.append(("reaped", self.pid))
                return status

        def recording_killpg(group, signum):
            events.append(("killpg", group, signum))
            return real_killpg(group, signum)

        with tempfile.TemporaryDirectory() as directory:
            hold = Path(directory) / "hold"
            os.mkfifo(hold)
            hold_end = os.open(hold, os.O_RDWR)
            try:
                command = ["/bin/sh", "-c", 'cat < "$0" & echo \'{"Action":"pass","Package":"m/a"}\'',
                           str(hold)]
                with mock.patch.object(module.subprocess, "Popen", RecordingPopen), \
                        mock.patch.object(module.os, "killpg", recording_killpg):
                    status, timed_out, received, cleanup = module.run_command(
                        command, Path(directory) / "out.json", None, None)
            finally:
                os.close(hold_end)
        self.assertEqual((status, timed_out, received, cleanup), (0, False, None, None))
        reaped = [event for event in events if event[0] == "reaped"]
        self.assertEqual(len(reaped), 1, events)
        leader = reaped[0][1]
        reaped_at = events.index(("reaped", leader))
        kills = [index for index, event in enumerate(events) if event[0] == "killpg" and event[1] == leader]
        self.assertIn(("killpg", leader, signal.SIGKILL), events)
        self.assertEqual([index for index in kills if index > reaped_at], [],
                         f"killpg after the command was reaped: {events}")

    # The command that leaves a live member: the FifoHold reader. The command
    # exits only after its acknowledgement (brahmagupta F2 at 333e804).
    LIVE_MEMBER = ("sh -c '" + FifoHold.READER + "; exec cat <&3 > /dev/null' \"$0\" \"$1\" \"$2\" & "
                   "read ack < \"$1\"; echo '{\"Action\":\"pass\",\"Package\":\"m/a\"}'")

    def run_with_refused_group_signals(self, script):
        """Run run_command with a killpg that the system refuses (EPERM).

        SCRIPT is a /bin/sh command with the FifoHold arguments. Return
        (status, cleanup problem or None, reader) with the FifoHold reader
        result.
        """
        module = self.load_reporter()

        def refused_killpg(group, signum):
            raise PermissionError(1, "Operation not permitted")

        with tempfile.TemporaryDirectory() as directory, FifoHold(directory) as fifo:
            with mock.patch.object(module.os, "killpg", refused_killpg):
                result = module.run_command(["/bin/sh", "-c", script, *fifo.args],
                                            Path(directory) / "out.json", None, None)
            reader = fifo.release()
            self.assertEqual(processes_holding(directory), [])
        return result[0], (result[3] if len(result) > 3 else None), reader

    def test_refused_group_signal_with_a_live_member_fails_the_run(self):
        # security cross-review r2 N-R2-1: the command exits 0, a member of its
        # group still runs, and the system refuses the group signal. The exit
        # of the command does not make the refusal harmless, so the run fails
        # and names the refused signal, on every platform.
        status, cleanup, _ = self.run_with_refused_group_signals(self.LIVE_MEMBER)
        self.assertNotEqual(status, 0)
        self.assertIsNotNone(cleanup)
        self.assertIn("refused to send SIGKILL to the process group of the command", cleanup)

    def test_refused_signal_reader_opens_before_the_command_exits(self):
        # brahmagupta F2 at 333e804: the reader must open, write its ID and
        # acknowledge before the command exits and the test closes the FIFO.
        # The command text holds that order. The run shows the outcome only.
        script = self.LIVE_MEMBER
        steps = ['exec 3<"$0"', 'echo $$ > "$2"', 'echo opened > "$1"', 'exec cat <&3',
                 'read ack < "$1"', "echo '{"]
        self.assertEqual([step for step in steps if step in script], steps, script)
        self.assertEqual([script.index(step) for step in steps], sorted(script.index(step) for step in steps))
        _, _, reader = self.run_with_refused_group_signals(script)
        self.assertEqual(reader, {"acknowledged": True, "alive": True, "ended": True})

    def test_refused_group_signal_after_the_group_is_empty(self):
        # The command exits 0 and leaves no member. Only on Darwin does the
        # system refuse a signal to a group whose only member is the exited,
        # unreaped command; ps then shows no live member, so the run passes.
        # On Linux each refusal fails the run.
        status, cleanup, _ = self.run_with_refused_group_signals("echo '{\"Action\":\"pass\",\"Package\":\"m/a\"}'")
        if sys.platform == "darwin":
            self.assertEqual((status, cleanup), (0, None))
        else:
            self.assertNotEqual(status, 0)
            self.assertIn("refused to send SIGKILL", cleanup)

    # brahmagupta F1 at 333e804: run_command installs signal handlers, so it
    # runs in this child. The child prints the result and the reap state.
    REFUSED_KILL_CHILD = """\
import importlib.util, json, os, select, signal, subprocess, sys, time
from pathlib import Path
report, out, case, command, hold, ack, pid, ack_limit = sys.argv[1:9]
spec = importlib.util.spec_from_file_location("go_test_report", report)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.KILL_GRACE = 0
real_killpg = module.os.killpg
def refused(group, signum):
    raise PermissionError(1, "Operation not permitted")
module.os.killpg = refused
started = []
class Recording(subprocess.Popen):
    def __init__(self, *arguments, **options):
        super().__init__(*arguments, **options)
        started.append(self)
        # The child owns the command from its start. Its wait for the
        # acknowledgement has a limit. Without it, the child kills the group
        # of the unreaped command and collects it before the test releases.
        try:
            with open(ack) as acknowledgement:
                ready = select.select([acknowledgement], [], [], float(ack_limit))[0]
                if not (ready and acknowledgement.readline() == "opened\\n"):
                    raise TimeoutError("no acknowledgement")
        except BaseException:
            real_killpg(self.pid, signal.SIGKILL)
            try:
                self.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
            print(json.dumps({"acknowledged": False, "status": self.returncode}), flush=True)
            os._exit(3)
        if case == "received":
            signal.raise_signal(signal.SIGTERM)
module.subprocess.Popen = Recording
if case == "no exit watch":
    def no_watch(pid):
        raise OSError("no watch")
    module.ExitWatch = no_watch
deadline = time.time() - 1 if case == "deadline" else None
result = module.run_command(["/bin/sh", "-c", command, hold, ack, pid], Path(out), deadline, None)
print(json.dumps({"result": list(result), "reaped": started[0].returncode is not None}))
"""

    # The command of the refused-kill test reads the hold FIFO itself, after
    # the FifoHold reader steps.
    REFUSED_KILL_COMMAND = FifoHold.READER + "; read line <&3"

    def test_refused_kill_of_a_live_command_has_a_bounded_wait(self):
        # Each group signal is refused, and the command blocks on a held FIFO.
        # The reporter must not wait with no limit or reap the live command,
        # and must fail. FifoHold then releases the command (F4 at 3b6e71f).
        steps = ['exec 3<"$0"', 'echo $$ > "$2"', 'echo opened > "$1"', "read line <&3"]
        command = self.REFUSED_KILL_COMMAND
        for step in steps:
            self.assertIn(step, command, "the command must open, write its ID and acknowledge first")
        self.assertEqual([command.index(step) for step in steps], sorted(command.index(step) for step in steps))
        child_code = self.REFUSED_KILL_CHILD
        self.assertIn("acknowledgement.readline()", child_code, "the child must wait for the acknowledgement")
        self.assertLess(child_code.index("acknowledgement.readline()"), child_code.index("module.run_command("))
        self.assertLess(child_code.index("started.append(self)"), child_code.index("acknowledgement.readline()"),
                        "the child must own the command before it waits for the acknowledgement")
        expected = {"deadline": 124, "received": 128 + signal.SIGTERM, "no exit watch": 1}
        with tempfile.TemporaryDirectory() as directory:
            child = Path(directory) / "child.py"
            child.write_text(child_code)
            for case, status in expected.items():
                case_directory = Path(directory) / case.replace(" ", "-")
                case_directory.mkdir()
                with self.subTest(case=case), FifoHold(case_directory) as fifo:
                    # The live command inherits the standard error of the child,
                    # so it goes to a file, not to a pipe that the wait reads.
                    errors = case_directory / "errors"
                    try:
                        with errors.open("w") as error_file:
                            result = subprocess.run(
                                [sys.executable, "-I", "-B", str(child), str(REPORT),
                                 str(case_directory / "out.json"), case, command, *fifo.args, "5"],
                                stdout=subprocess.PIPE, stderr=error_file, text=True, timeout=20)
                    except subprocess.TimeoutExpired:
                        self.fail(f"{case}: the reporter waited with no limit after a refused SIGKILL")
                    self.assertEqual(result.returncode, 0, result.stdout + errors.read_text())
                    report = json.loads(result.stdout.splitlines()[-1])
                    self.assertEqual(report["result"][0], status, report)
                    self.assertFalse(report["reaped"], "the reporter reaped a live command")
                    self.assertIn("refused to send SIGKILL", report["result"][3])
                    self.assertIn("after the last SIGKILL, so the reporter did not reap it", report["result"][3])
                    self.assertEqual(fifo.release(), {"acknowledged": True, "alive": True, "ended": True})
                self.assertEqual(processes_holding(str(case_directory)), [])

    def test_teardown_check_finds_a_live_process(self):
        # The teardown check must see a live process that names the folder.
        # The process blocks on its standard input, which the test holds.
        with tempfile.TemporaryDirectory() as directory:
            process = subprocess.Popen(["/bin/sh", "-c", "read line", directory], stdin=subprocess.PIPE)
            try:
                self.assertEqual(len(processes_holding(directory)), 1)
            finally:
                process.stdin.write(b"go\n")
                process.stdin.close()
                self.assertEqual(process.wait(timeout=30), 0)
            self.assertEqual(processes_holding(directory), [])

    def test_refused_kill_child_collects_a_command_with_no_acknowledgement(self):
        # brahmagupta F2 at 52a87ec and process 5352: a command that never
        # acknowledges blocks on a FIFO with no writer. The child must collect
        # it. No reporter, command or FIFO reader may stay.
        with tempfile.TemporaryDirectory() as directory:
            child = Path(directory) / "child.py"
            child.write_text(self.REFUSED_KILL_CHILD)
            never = Path(directory) / "hold.never"
            os.mkfifo(never)
            with FifoHold(directory) as fifo:
                errors = Path(directory) / "errors"
                with errors.open("w") as error_file:
                    result = subprocess.run(
                        [sys.executable, "-I", "-B", str(child), str(REPORT), str(Path(directory) / "out.json"),
                         "deadline", 'read line < "$0.never"', *fifo.args, "1"],
                        stdout=subprocess.PIPE, stderr=error_file, text=True, timeout=20)
                self.assertEqual(result.returncode, 3, result.stdout + errors.read_text())
                self.assertEqual(json.loads(result.stdout.splitlines()[-1]),
                                 {"acknowledged": False, "status": -signal.SIGKILL})
                self.assertEqual(fifo.release(), {"acknowledged": False, "alive": False, "ended": False})
            self.assertEqual([path.name for path in (never, fifo.hold, fifo.ack) if not no_fifo_reader(path)], [])
            self.assertEqual(processes_holding(directory), [])

    def test_signal_after_the_reap_is_recorded_and_not_forwarded(self):
        # Deferred item (a): after the real reap of the command, the
        # reporter's handlers are still in place. A signal then must not reach
        # killpg, because the old process ID can name a new group, and it must
        # still fail the run. The check calls the installed SIGTERM handler
        # directly, right after the real reap, so it needs no timing. It covers
        # the normal path and the path where ExitWatch cannot start.
        module = self.load_reporter()
        command = [sys.executable, "-c",
                   "print('{\"Action\":\"pass\",\"Package\":\"m/a\"}', flush=True)", "-count=1"]
        for path in ("normal", "no exit watch"):
            events = []
            real_killpg = os.killpg

            class SignalAfterReap(subprocess.Popen):
                # Only the command of the reporter. The reporter can also
                # start ps (the Darwin path), which must not count here.
                def wait(self, timeout=None):
                    status = super().wait(timeout)
                    if list(self.args) == command:
                        events.append(("reaped", self.pid))
                        signal.getsignal(signal.SIGTERM)(signal.SIGTERM, None)
                    return status

            def recording_killpg(group, signum):
                reaped = any(event[0] == "reaped" for event in events)
                events.append(("killpg", group, signum))
                if not reaped:
                    real_killpg(group, signum)

            patches = [mock.patch.object(module.subprocess, "Popen", SignalAfterReap),
                       mock.patch.object(module.os, "killpg", recording_killpg)]
            if path == "no exit watch":
                patches.append(mock.patch.object(module, "ExitWatch", side_effect=OSError("no watch")))
            with self.subTest(path=path), tempfile.TemporaryDirectory() as directory, \
                    contextlib.ExitStack() as stack:
                for patch in patches:
                    stack.enter_context(patch)
                stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                result = module.run_command(command, Path(directory) / "out.json", None, None)
                status, received = result[0], result[2]
                reaped = [index for index, event in enumerate(events) if event[0] == "reaped"]
                self.assertEqual(len(reaped), 1, events)
                self.assertEqual([event for event in events[reaped[0]:] if event[0] == "killpg"], [],
                                 f"killpg after the reap: {events}")
                self.assertEqual(received, signal.SIGTERM)
                self.assertEqual(status, 128 + signal.SIGTERM)

    def test_member_list_is_unknown_for_an_unreadable_record(self):
        # brahmagupta F1 at 3b6e71f: only ENOENT or ESRCH proves an exit. The
        # fake /proc entry 4194002 fails to read in each case.
        module = self.load_reporter()
        group = 4194001
        real_read_text = Path.read_text
        cases = {"permission denied": PermissionError(13, "Permission denied"),
                 "input/output error": OSError(5, "Input/output error"),
                 "vanished (ENOENT)": FileNotFoundError(2, "No such file or directory"),
                 "vanished (ESRCH)": ProcessLookupError(3, "No such process")}
        for case, error in cases.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                proc = Path(directory)
                for pid in (group, 4194002):
                    (proc / str(pid)).mkdir()
                    (proc / str(pid) / "stat").write_text(f"{pid} (held name) S 1 {group} {group} 0\n")
                broken = proc / "4194002" / "stat"

                def read_text(path, *arguments, **options):
                    if path == broken:
                        raise error
                    return real_read_text(path, *arguments, **options)

                with mock.patch.object(module.sys, "platform", "linux"), \
                        mock.patch.object(module, "PROC", proc, create=True), \
                        mock.patch.object(type(proc), "read_text", read_text):
                    members = module.live_group_members(group)
                    unsignalled = module.unsignalled_members(group)
                if case.startswith("vanished"):
                    self.assertEqual(members, [group])
                    self.assertEqual(unsignalled, 0)
                else:
                    self.assertIsNone(members, case)
                    self.assertIsNone(unsignalled, case)

    def test_member_list_ps_shutdown_has_a_limit(self):
        # brahmagupta F2 at 3b6e71f: each wait for a ps that never ends has a
        # limit, and the list is unknown. The fake refuses a wait with no limit.
        module = self.load_reporter()
        events = []

        class StuckLister:
            def __init__(self, *arguments, **options):
                self.returncode = None

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                events.append("context exit")
                raise AssertionError("the context exit waits for ps with no limit")

            def communicate(self, timeout=None):
                events.append(("communicate", timeout))
                if timeout is None:
                    raise AssertionError("communicate waits for ps with no limit")
                raise subprocess.TimeoutExpired("ps", timeout)

            def wait(self, timeout=None):
                events.append(("wait", timeout))
                if timeout is None:
                    raise AssertionError("wait waits for ps with no limit")
                raise subprocess.TimeoutExpired("ps", timeout)

            def kill(self):
                events.append("kill")

        with mock.patch.object(module.sys, "platform", "darwin"), \
                mock.patch.object(module, "PROCESS_LIST_POPEN", StuckLister):
            self.assertIsNone(module.live_group_members(4194001))
            self.assertIsNone(module.unsignalled_members(4194001))
        self.assertIn("kill", events)
        self.assertNotIn("context exit", events)
        self.assertTrue(all(event == "kill" or event[1] is not None for event in events), events)

    def test_unsignalled_group_member_fails_the_run(self):
        # crrao R1: a real member blocks on a FIFO, and the real SIGKILL stops
        # it. A fake /proc (forced Linux path) also lists a member that gives
        # EPERM to signal 0. The run must fail with the fixed message. Only
        # Linux CI can show a real member that refuses a successful killpg.
        module = self.load_reporter()
        refusing = 4194000
        real_kill = os.kill
        message = {"refused": "remain that the reporter may not signal (permission denied)",
                   "unreadable": "could not list the process group of the command"}
        for case in ("refused", "signalled", "unreadable"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                proc = Path(directory) / "proc"
                hold = Path(directory) / "hold"
                os.mkfifo(hold)
                hold_end = os.open(hold, os.O_RDWR)

                class FakeProcPopen(subprocess.Popen):
                    def __init__(self, *arguments, **options):
                        super().__init__(*arguments, **options)
                        if case == "unreadable":
                            return
                        for pid, state in ((self.pid, "Z"), (refusing, "S")):
                            (proc / str(pid)).mkdir(parents=True)
                            (proc / str(pid) / "stat").write_text(
                                f"{pid} (held name) {state} 1 {self.pid} {self.pid} 0 -1 4194560\n")

                def kill(pid, signum):
                    if pid == refusing:
                        if case == "refused":
                            raise PermissionError(1, "Operation not permitted")
                        return None
                    return real_kill(pid, signum)

                command = ["/bin/sh", "-c", 'cat < "$0" > /dev/null 2>&1 & echo \'{"Action":"pass","Package":"m/a"}\'',
                           str(hold)]
                try:
                    with mock.patch.object(module.subprocess, "Popen", FakeProcPopen), \
                            mock.patch.object(module.sys, "platform", "linux"), \
                            mock.patch.object(module, "PROC", proc, create=True), \
                            mock.patch.object(module.os, "kill", side_effect=kill):
                        result = module.run_command(command, Path(directory) / "out.json", None, None)
                finally:
                    os.close(hold_end)
                self.assertEqual(result[0], 0 if case == "signalled" else 1, result)
                if case == "signalled":
                    self.assertIsNone(result[3])
                else:
                    self.assertIn(message[case], result[3])

    def test_signal_to_the_reporter_fails_the_run(self):
        # The child prints a complete, passing stream, exits 0 on SIGTERM and
        # else blocks on a FIFO that the test never writes. The test sends
        # SIGTERM to the reporter, not to the child. The reporter forwards it,
        # the child exits 0, and the run must still fail with 128 + 15.
        child = ("import json, os, signal, sys\n"
                 "signal.signal(signal.SIGTERM, lambda *_: os._exit(0))\n"
                 "for event in ({'Action': 'run', 'Package': 'm/a', 'Test': 'TestOK'},\n"
                 "              {'Action': 'pass', 'Package': 'm/a', 'Test': 'TestOK'},\n"
                 "              {'Action': 'pass', 'Package': 'm/a', 'Elapsed': 0.1}):\n"
                 "    print(json.dumps(event), flush=True)\n"
                 "with open(sys.argv[2], 'w') as ready:\n"
                 "    ready.write('ready\\n')\n"
                 "open(sys.argv[1]).read()\n")
        with tempfile.TemporaryDirectory() as directory:
            hold, ready = Path(directory) / "hold", Path(directory) / "ready"
            os.mkfifo(hold)
            os.mkfifo(ready)
            expected = Path(directory) / "expected.txt"
            expected.write_text("m/a\n")
            hold_end, ready_end = os.open(hold, os.O_RDWR), os.open(ready, os.O_RDWR)
            try:
                reporter = subprocess.Popen(
                    [sys.executable, "-I", "-B", str(REPORT), "--output", str(Path(directory) / "out.json"),
                     "--expect", str(expected), "--", *self.program(child), str(hold), str(ready),
                     "-count=1"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    # One bounded wait: the child has printed its stream and
                    # handles SIGTERM.
                    is_ready, _, _ = select.select([ready_end], [], [], 30)
                    self.assertTrue(is_ready, "the child did not report that it is ready")
                    reporter.send_signal(signal.SIGTERM)
                finally:
                    stdout, stderr = reporter.communicate(timeout=60)
            finally:
                os.close(hold_end)
                os.close(ready_end)
        self.assertEqual(reporter.returncode, 143, stdout + stderr)
        self.assertIn("go test: the reporter received SIGTERM and stopped the command (exit 143)\n", stdout)
        self.assertIn("go test: intended set: 1 packages from go list; package results: 1", stdout)

    def test_new_tests_hold_processes_without_a_timed_pause(self):
        # Hard rule 5: this file and the listed fixture tests hold a process on
        # a FIFO or a pipe, never on a timed pause. The word is split in two.
        token = re.compile(r"\b" + "sl" + "eep" + r"\b")
        texts = {"test_setup.py": Path(__file__).read_text()}
        fixture_tests = ROOT / "scripts/ci/test_stack_fixture.py"
        tree = ast.parse(fixture_tests.read_text())
        for node in ast.walk(tree):
            if isinstance(node, (ast.FunctionDef, ast.ClassDef)) and (
                    node.name.startswith(FIXTURE_TESTS_WITHOUT_PAUSE) or node.name == "receipt_manifest"):
                texts[f"test_stack_fixture.py {node.name}"] = ast.get_source_segment(
                    fixture_tests.read_text(), node)
        self.assertGreaterEqual(len(texts), 8, texts.keys())
        found = [f"{name}:{number}: {line.strip()}" for name, text in texts.items()
                 for number, line in enumerate(text.splitlines(), 1) if token.search(line)]
        self.assertEqual(found, [])

    def test_background_process_waits_for_its_gate(self):
        # The acquisition order of the process cleanup test. The background
        # process must not start its work before the test opens its gate, so
        # the test can hold its identity first. Here the work only writes
        # "done" to a FIFO and ends.
        with tempfile.TemporaryDirectory() as directory:
            fifos = {name: Path(directory) / name for name in ("pid", "gate", "release", "done")}
            for path in fifos.values():
                os.mkfifo(path)
            script = gated_background_script(
                fifos["pid"], fifos["gate"], fifos["release"],
                f"sh -c 'echo done > \"{fifos['done']}\"'", "true")
            ends = {name: os.open(path, os.O_RDWR) for name, path in fifos.items()}
            handle = None
            command = subprocess.Popen(["/bin/sh", "-c", script])
            try:
                ready, _, _ = select.select([ends["pid"]], [], [], 30)
                self.assertTrue(ready, "the command did not hand over its background process ID")
                pid = int(os.read(ends["pid"], 64).decode().strip())
                # One wait, with one time limit, for an event that must not
                # come: the work must not run before the gate opens.
                early, _, _ = select.select([ends["done"]], [], [], 2)
                self.assertFalse(early, "the background process did its work before its gate opened")
                handle = HeldProcess(pid)
                self.assertFalse(handle.exits_within(0), "the background process ended before its gate opened")
                os.write(ends["gate"], b"go\n")
                # The work runs only now: one wait for its event, then one wait
                # for the exit event of the same, held process.
                done, _, _ = select.select([ends["done"]], [], [], 30)
                self.assertTrue(done, "the background process did not do its work after its gate opened")
                self.assertTrue(handle.exits_within(5), "the held process did not end after its work")
            finally:
                os.write(ends["gate"], b"go\n")
                os.write(ends["release"], b"go\n")
                command.wait(timeout=30)
                if handle is not None:
                    handle.close()
                for end in ends.values():
                    os.close(end)

    def test_held_process_never_signals_by_process_id(self):
        # A deterministic identity check. The held process exits and is
        # reaped, so its process ID is free for another process. kill() must
        # not signal by process ID: os.kill fails the test if it runs.
        child = subprocess.Popen(["cat"], stdin=subprocess.PIPE)
        handle = HeldProcess(child.pid)
        try:
            child.stdin.close()
            self.assertEqual(child.wait(timeout=30), 0)
            self.assertTrue(handle.exits_within(5))
            with mock.patch.object(os, "kill", side_effect=AssertionError("a signal by process ID")):
                self.assertFalse(handle.kill())
        finally:
            handle.close()
        # A live held process: Linux kills it through the pidfd. macOS sends
        # nothing, and the test stops its own child.
        child = subprocess.Popen(["cat"], stdin=subprocess.PIPE)
        handle = HeldProcess(child.pid)
        try:
            with mock.patch.object(os, "kill", side_effect=AssertionError("a signal by process ID")):
                killed = handle.kill()
            if hasattr(os, "pidfd_open"):
                self.assertTrue(killed)
                self.assertEqual(child.wait(timeout=30), -signal.SIGKILL)
            else:
                self.assertFalse(killed)
                self.assertIsNone(child.poll())
        finally:
            handle.close()
            child.stdin.close()
            child.wait(timeout=30)

    def test_command_stopped_by_a_signal_fails(self):
        command = [*self.program("import os, signal; os.kill(os.getpid(), signal.SIGTERM)"), "-count=1"]
        result, _ = self.report([], expect=["m/a"], command=command)
        self.assertEqual(result.returncode, 143)

    # L-e1 P5a: a marker in names and output, with and without control characters.
    MARK = "LIVE-MARK-41c7"

    def live_report(self, events, *options, expect=("m/acc",), raw_text=None):
        """Run the reporter with --live. Return (result, summary, raw report, raw mode)."""
        with tempfile.TemporaryDirectory() as directory:
            stream = Path(directory) / "events.json"
            stream.write_text(raw_text if raw_text is not None else "\n".join(events) + "\n")
            expected = Path(directory) / "expected.txt"
            expected.write_text("".join(f"{package}\n" for package in expect))
            raw, summary = Path(directory) / "raw", Path(directory) / "summary.md"
            fake = self.program("import sys; sys.stdout.write(open(sys.argv[1]).read()); "
                                "sys.stderr.write(sys.argv[2])")
            result = subprocess.run(
                [sys.executable, "-I", "-B", str(REPORT), "--output", str(Path(directory) / "out.json"),
                 "--live", "--raw-output", str(raw), "--scenarios", "--expect", str(expected),
                 "--fail-on-skip", "--summary-file", str(summary), *options, "--", *fake, str(stream),
                 f"stderr {self.MARK}\n", "-count=1"],
                capture_output=True, text=True, timeout=120)
            text = summary.read_text() if summary.exists() else ""
            report = raw.read_text() if raw.exists() else ""
            mode = raw.stat().st_mode & 0o777 if raw.exists() else None
        return result, text, report, mode

    def test_live_mode_prints_no_stream_text(self):
        for decoration in ("", "\x1b[31m\r\x07\t"):
            mark = self.MARK + decoration
            scenario = f"{mark} scenario"
            steps = [f"    \x1b[31mWhen\x1b[0m \x1b[31m{mark} step\x1b[0m\n"]
            extra = [go_event("run", "m/acc", f"Test{mark}Fail"),
                     go_event("output", "m/acc", f"Test{mark}Fail", Output=f"error {mark}\n"),
                     go_event("fail", "m/acc", f"Test{mark}Fail"),
                     go_event("run", "m/acc", f"TestParent/{mark}sub"),
                     go_event("output", "m/acc", f"TestParent/{mark}sub", Output=f"skip {mark}\n"),
                     go_event("skip", "m/acc", f"TestParent/{mark}sub"),
                     go_event("run", "m/acc", f"Test{mark}Unfinished"),
                     go_event("output", "m/acc", f"Test{mark}Unfinished", Output=f"panic {mark}\n")]
            events = self.gherkin_suite({scenario: ("fail", steps)},
                                        ["1 scenarios (1 failed)", "1 steps (1 failed)"], extra)
            with self.subTest(decoration=repr(decoration)):
                result, summary, raw, mode = self.live_report(events)
                self.assertNotEqual(result.returncode, 0)
                for name, text in (("stdout", result.stdout), ("stderr", result.stderr), ("summary", summary)):
                    self.assertNotIn(self.MARK, text, name)
                self.assertIn(f"Test{self.MARK}", raw)
                self.assertIn(f"stderr {self.MARK}", raw)
                self.assertEqual(mode, 0o600)
                self.assertIn("scenario 1 of 1: FAIL\n", result.stdout)
                self.assertIn("godog scenarios: 1 (1 failed)\n", result.stdout)
                self.assertIn(": no result\n", result.stdout)
                self.assertIn(": SKIP\n", result.stdout)
                self.assertIn("- scenario 1 of 1: FAIL\n", summary)

    def test_live_mode_needs_its_options(self):
        with tempfile.TemporaryDirectory() as directory:
            existing = Path(directory) / "raw"
            existing.write_text("")
            out, new, missing = (str(Path(directory) / name) for name in ("o", "r", "x"))
            for options in (["--live", "--expect", missing, "--raw-output", new],
                            ["--live", "--scenarios", "--raw-output", new],
                            ["--live", "--scenarios", "--expect", missing],
                            ["--raw-output", new, "--scenarios", "--expect", missing]):
                with self.subTest(options=options):
                    result = subprocess.run([sys.executable, "-I", "-B", str(REPORT), "--output", out,
                                             *options, "--", "true"], capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 2, result.stderr)
            expected = Path(directory) / "expected"
            expected.write_text("m/acc\n")
            result = subprocess.run([sys.executable, "-I", "-B", str(REPORT), "--output", out, "--live",
                                     "--scenarios", "--expect", str(expected), "--raw-output", str(existing),
                                     "--", "true", "-count=1"], capture_output=True, text=True, timeout=30)
            self.assertEqual((result.returncode, existing.read_text()), (2, ""), result.stderr)

    def test_live_mode_fails_on_stream_faults(self):
        zero = self.gherkin_suite({}, ["No scenarios", "No steps"])
        passing = self.gherkin_suite({"a": ("pass", [])}, ["1 scenarios (1 passed)", "0 steps"])
        for name, events, expect, raw_text in (
                ("zero scenarios", zero, ("m/acc",), None),
                ("missing result", passing, ("m/acc", "m/other"), None),
                ("malformed stream", passing, ("m/acc",), "\n".join(passing) + f"\nnot json {self.MARK}\n")):
            with self.subTest(case=name):
                result, summary, raw, _ = self.live_report(events, expect=expect, raw_text=raw_text)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(self.MARK, result.stdout + summary)
                self.assertRegex(result.stdout, r"go test \(live\): [1-9]\d* problems")

    def test_summary_file_needs_a_title_or_scenarios(self):
        result, _ = self.report([], "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 2)
        self.assertIn("--summary-file requires --title or --scenarios", result.stderr)


def check_bdd_functions():
    """The function region of check-bdd.sh that this file runs."""
    text = CHECK_BDD.read_text()
    begin = "# --- functions that scripts/ci/test_setup.py runs: begin ---\n"
    end = "# --- functions that scripts/ci/test_setup.py runs: end ---\n"
    return text[text.index(begin):text.index(end) + len(end)]


class BddSignalPaths(unittest.TestCase):
    """Developer checks of the stack run signal paths in check-bdd.sh."""

    # CI runs check-bdd.sh with sh in the CI image, which is dash. bash as sh
    # (macOS /bin/sh) waits for the command after SIGINT, so the check runs
    # with dash on every host. It fails when dash is missing.
    DASH = "/bin/dash"

    def run_script(self, child):
        # The real function region of check-bdd.sh, then the stack run step as
        # check-bdd.sh runs it, with CHILD as the stack run command.
        self.assertTrue(os.access(self.DASH, os.X_OK), "dash is required for this check")
        script = ("step() { printf 'step: %s\\n' \"$1\"; }\n" + check_bdd_functions()
                  + "stack_status=0\n" + child + " || stack_status=$?\n"
                  + 'pm_stack_ended "$stack_status"\n'
                  + "printf '%s\\n' 'next: receipt check and Gherkin run'\n")
        return subprocess.run([self.DASH, "-c", script], capture_output=True, text=True, timeout=60)

    def test_child_signal_exit_is_a_stack_failure(self):
        for name, status in (("HUP", 129), ("INT", 130), ("TERM", 143)):
            with self.subTest(signal=name):
                result = self.run_script(f"sh -c 'kill -{name} $$'")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f"step: stack run ended with exit {status}\n", result.stdout)
                self.assertIn("next: receipt check and Gherkin run\n", result.stdout)
                self.assertIn(f"ended on a signal (exit {status})", result.stderr)

    def test_job_cancellation_stops_the_script(self):
        # The command sends the signal to the script itself, as the init
        # process does on a job cancellation. The script stops at once.
        self.assertNotRegex(CHECK_BDD.read_text(), r"(?m)^\s*trap\b",
                            "check-bdd.sh must keep the default action of HUP, INT and TERM")
        for name in ("HUP", "INT", "TERM"):
            with self.subTest(signal=name):
                result = self.run_script(f"sh -c 'kill -{name} \"$PPID\"'")
                self.assertEqual(result.returncode, -getattr(signal, "SIG" + name),
                                 result.stdout + result.stderr)
                self.assertNotIn("stack run ended", result.stdout)
                self.assertNotIn("next:", result.stdout)


class BddReceiptStep(unittest.TestCase):
    """The receipt step of check-bdd.sh only reads the stack run manifest."""

    def test_receipt_step_does_not_repeat_the_cleanup(self):
        commands = [line for line in CHECK_BDD.read_text().splitlines()
                    if not line.lstrip().startswith("#")]
        self.assertEqual([line for line in commands if "--cleanup" in line], [])
        self.assertEqual(
            [line.strip() for line in commands if "--check-receipt" in line],
            ['if python3 scripts/ci/with-stack-fixture.py --check-receipt "$manifest" &&'])


class BddTimeBudget(unittest.TestCase):
    """The time limits of the bdd job (owner decision of 2026-11-17: the stack
    go test command stops one hour after the start of check-bdd.sh)."""

    def module(self, name, path):
        spec = importlib.util.spec_from_file_location(name, path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_limits_fit_one_hour_of_stack_tests(self):
        script = CHECK_BDD.read_text()
        # The budget lines of check-bdd.sh, run with the script start at 0.
        lines = [line for line in script.splitlines()
                 if re.match(r"^(budget|start_allowance|cleanup_allowance|finish_allowance|stop_grace|"
                             r"gherkin_minimum|gherkin_deadline|stack_deadline)=", line)]
        self.assertEqual(len(lines), 8, lines)
        result = subprocess.run(["/bin/sh", "-c", "started=0\n" + "\n".join(lines)
                                 + '\necho "$budget $stack_deadline $gherkin_deadline $start_allowance'
                                 + ' $cleanup_allowance $finish_allowance $stop_grace $gherkin_minimum"'],
                                capture_output=True, text=True, timeout=30)
        (budget, stack_deadline, gherkin_deadline, start_allowance, cleanup_allowance,
         finish_allowance, stop_grace, gherkin_minimum) = map(int, result.stdout.split())
        text = script.replace("\\\n", " ")
        stack = re.search(r'--deadline "\$stack_deadline" --timeout (\d+) .* -timeout (\w+) \$\(cat', text)
        gherkin = re.search(r'--deadline "\$gherkin_deadline" --timeout (\d+) ', text)
        stack_timeout, gherkin_timeout = int(stack.group(1)), int(gherkin.group(1))
        fixture = self.module("stack_fixture", ROOT / "scripts/ci/with-stack-fixture.py")
        kill_grace = self.module("go_test_report", REPORT).KILL_GRACE
        workflow = (ROOT / WORKFLOW).read_text()
        bdd = workflow[workflow.index("\n  bdd:\n"):workflow.index("\n  checks:\n")]
        job = int(re.search(r"\n    timeout-minutes: (\d+)\n", bdd).group(1))
        step = int(re.search(r"- name: Run the Gherkin acceptance suite\n        timeout-minutes: (\d+)\n",
                             bdd).group(1))
        cleanup = int(re.search(r"- name: Clean up the job container and dependency fixture\n"
                                r"        if: always\(\)\n        timeout-minutes: (\d+)\n", bdd).group(1))
        # The allowances are the real worst cases. A stopped go test command
        # gets the TERM grace and then the bounded reap after SIGKILL. A phase
        # can end PROCESS_TERM_GRACE after its deadline, because the wait
        # after the SIGKILL of a stopped call has no phase limit.
        self.assertEqual(stop_grace, 2 * kill_grace)
        self.assertEqual(start_allowance, fixture.START_PHASE_LIMIT + fixture.PROCESS_TERM_GRACE)
        self.assertEqual(cleanup_allowance, fixture.CLEANUP_PHASE_LIMIT + fixture.PROCESS_TERM_GRACE)
        # The reporter, not the wrapper, ends each run.
        self.assertLess(stack_timeout + 2 * kill_grace, fixture.CHILD_TIMEOUT)
        self.assertLess(gherkin_timeout + 2 * kill_grace, fixture.CHILD_TIMEOUT)
        # The step keeps 3 minutes for the job container; the job keeps the
        # cleanup step and 4 minutes for the checkout and the runner.
        self.assertEqual(step * 60, budget + 3 * 60)
        self.assertEqual(job, step + cleanup + 4)
        # The longest path ends at the budget for a prelude end P of 2870 s
        # or less, and the 8-minute prelude of the budget is inside that.
        for prelude, end in ((480, budget), (2870, budget), (2871, budget + 1)):
            stack_start_end = prelude + start_allowance
            gherkin_start_end = (max(stack_deadline, stack_start_end) + stop_grace + cleanup_allowance
                                 + finish_allowance + start_allowance)
            gherkin_end = max(gherkin_deadline, gherkin_start_end) + stop_grace + cleanup_allowance
            self.assertEqual(gherkin_end + finish_allowance, end, prelude)
        self.assertEqual((budget, stack_deadline, gherkin_deadline, gherkin_minimum), (5220, 3600, 4775, 0))
        self.assertEqual((start_allowance, cleanup_allowance, finish_allowance, stop_grace), (730, 370, 15, 60))
        self.assertEqual((stack_timeout, stack.group(2), gherkin_timeout), (3590, "15m", 1140))
        self.assertEqual((fixture.CHILD_TIMEOUT, kill_grace, fixture.PROCESS_TERM_GRACE), (3660, 30, 10))
        self.assertEqual((fixture.START_PHASE_LIMIT, fixture.CLEANUP_PHASE_LIMIT), (720, 360))
        self.assertEqual((step, job, cleanup), (90, 100, 6))


class GoPackageSelection(unittest.TestCase):
    """Developer checks of the package rule in scripts/ci/go-packages.sh."""

    def sh(self, script, stdin="", cwd=ROOT):
        return subprocess.run(["/bin/sh", "-c", '. "$0"; ' + script, str(GO_PACKAGES)],
                              input=stdin, capture_output=True, text=True, timeout=30, cwd=cwd)

    def lines(self, script, stdin=""):
        result = self.sh(script, stdin)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.split()

    def test_each_package_has_exactly_one_place(self):
        entries = self.lines("pm_stack_entries")
        excluded = [line.split()[0] for line in self.lines_raw("pm_excluded_packages")]
        self.assertEqual(excluded, ["./tests/client-parity"])
        packages = sorted(set(entries) | set(excluded) | {
            "./cmd/pm", "./internal/compile", "./tests/starter", "./tests/acceptance"})
        unit = self.lines("pm_drop_stack_packages", "\n".join(packages) + "\n")
        stack = self.lines("pm_stack_entries | pm_stack_run_packages")
        gherkin = self.lines('printf "%s\\n" "$pm_gherkin_package"')
        self.assertEqual(gherkin, ["./tests/acceptance"])
        self.assertIn("./tests/starter", unit)
        places = unit + stack + gherkin + excluded
        self.assertEqual(sorted(places), packages)
        self.assertEqual(len(places), len(set(places)))

    def lines_raw(self, script):
        result = self.sh(script)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.splitlines()

    def test_hook_uses_the_same_rule(self):
        packages = "./cmd/pm\n./internal/store\n./tests/client-parity\n./tests/starter\n"
        result = subprocess.run(
            ["/bin/sh", "-c", '. "$0"; drop_stack_packages', str(HOOK_LIB)],
            input=packages, capture_output=True, text=True, timeout=30, cwd=ROOT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.split(), ["./cmd/pm", "./tests/starter"])

    def test_missing_stack_list_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.sh("pm_drop_stack_packages", "./cmd/pm\n", cwd=directory)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("scripts/ci/stack-packages.txt is missing", result.stderr)
        self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
