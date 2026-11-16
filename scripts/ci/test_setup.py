#!/usr/bin/env python3
"""Exercise the public setup checker against isolated, real file inventories."""
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest


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
                lambda s: s.replace("    timeout-minutes: 60\n",
                                    "    timeout-minutes: 60\n    container: ubuntu:24.04\n")),
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
            return s[s.index("  checks:\n"):s.index("\n  # The Bazel gate")] + "\n"

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

    def test_bazel_job_is_frozen(self):
        def bazel_block(s):
            return s[s.index("  bazel:\n"):]

        cases = {
            "missing bazel job": (lambda s: s.split("\n  # The Bazel gate", 1)[0] + "\n",
                                  "unapproved job membership"),
            "renamed bazel job": (lambda s: s.replace("\n  bazel:\n", "\n  bazel-build:\n"),
                                  "unapproved job membership"),
            "changed bazel step": (lambda s: s.replace(
                "sh scripts/ci/check-bazel.sh", "sh scripts/ci/check-bazel.sh --keep_going"),
                "unapproved bazel executable step"),
            "bazel outside the CI image": (lambda s: s.replace(
                "run: sh scripts/ci/in-ci-image.sh run bazel -- ", "run: "),
                "unapproved bazel executable step"),
            "bazel step timeout changed": (lambda s: s[:s.index("  bazel:\n")] + bazel_block(s).replace(
                "        timeout-minutes: 50\n", "        timeout-minutes: 55\n", 1),
                "unapproved bazel executable step"),
            "bazel job timeout changed": (lambda s: s[:s.index("  bazel:\n")] + bazel_block(s).replace(
                "    timeout-minutes: 60\n", "    timeout-minutes: 90\n", 1),
                "bazel job timeout changed"),
            "bazel waits for bdd": (lambda s: s.replace(
                "  bazel:\n    if:", "  bazel:\n    needs: bdd\n    if:"),
                "unapproved bazel job capability"),
            "bazel continues on error": (lambda s: s.replace(
                "      - name: Run the Bazel build and test\n",
                "      - name: Run the Bazel build and test\n        continue-on-error: true\n"),
                "unapproved bazel executable step"),
            "bazel cleanup skipped on failure": (lambda s: s[:s.index("  bazel:\n")] + bazel_block(s).replace(
                "        if: always()", "        if: success()", 1),
                "unapproved bazel cleanup step"),
            "bazel cleanup selects another manifest": (lambda s: s.replace(
                "pmcli-bazel-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json", "pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"),
                "unapproved bazel cleanup step"),
            "bazel on another runner": (lambda s: s[:s.index("  bazel:\n")] + bazel_block(s).replace(
                "pm-cli-vps", "unrelated-runner", 1),
                "bazel runner routing changed"),
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


def process_exits_within(pid, seconds):
    """Wait one time for the exit event of process PID, for at most SECONDS.

    Return True when the process has exited, also as a zombie that no parent
    has reaped yet, or when no process has the ID. Return False when the time
    limit ends first. Linux uses a pidfd, macOS a kqueue process filter. A
    system with neither has no exit event, and the caller fails.
    """
    if hasattr(os, "pidfd_open"):
        try:
            descriptor = os.pidfd_open(pid)
        except ProcessLookupError:
            return True
        try:
            poller = select.poll()
            poller.register(descriptor, select.POLLIN)
            return bool(poller.poll(int(seconds * 1000)))
        finally:
            os.close(descriptor)
    if hasattr(select, "kqueue"):
        queue = select.kqueue()
        try:
            change = select.kevent(pid, filter=select.KQ_FILTER_PROC,
                                   flags=select.KQ_EV_ADD | select.KQ_EV_ONESHOT,
                                   fflags=select.KQ_NOTE_EXIT)
            try:
                queue.control([change], 0, 0)
            except ProcessLookupError:
                return True
            events = queue.control(None, 1, seconds)
            return any(event.fflags & select.KQ_NOTE_EXIT for event in events)
        finally:
            queue.close()
    raise AssertionError("this system has neither pidfd_open nor kqueue: no process exit event")


def go_event(action, package, test=None, **extra):
    event = {"Action": action, "Package": package}
    if test:
        event["Test"] = test
    event.update(extra)
    return json.dumps(event)


class GoTestReport(unittest.TestCase):
    """Developer checks of scripts/ci/go-test-report.py with synthetic events."""

    def report(self, events, *options, exit_status=0, raw=None, expect=None, command=None,
               env=None):
        with tempfile.TemporaryDirectory() as directory:
            stream = Path(directory) / "events.json"
            stream.write_text(raw if raw is not None else "\n".join(events) + ("\n" if events else ""))
            summary = Path(directory) / "summary.md"
            fake = command or [sys.executable, "-c",
                               "import sys; sys.stdout.write(open(sys.argv[1]).read()); sys.exit(int(sys.argv[2]))",
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
                command = [sys.executable, "-c", "raise SystemExit(0)", "-count=1", *options]
                result, _ = self.report([], expect=["m/a"], command=command, env=env)
                self.assertEqual(result.returncode, 2)
                self.assertIn("refused: the command selects or filters tests", result.stderr)
        command = [sys.executable, "-c", "raise SystemExit(0)"]
        result, _ = self.report([], expect=["m/a"], command=command)
        self.assertEqual(result.returncode, 2)
        self.assertIn("refused: the command does not contain -count=1", result.stderr)

    def test_deadline_stops_the_command_and_still_reports(self):
        script = ("import json, sys, time\n"
                  "print(json.dumps({'Action': 'pass', 'Package': 'm/a', 'Elapsed': 0.1}), flush=True)\n"
                  "time.sleep(60)\n")
        command = [sys.executable, "-c", script, "-count=1"]
        started = time.monotonic()
        result, summary = self.report([], "--deadline", str(int(time.time()) + 2), "--title", "Run",
                                      "--summary-file", "SUMMARY", expect=["m/a", "m/b"], command=command)
        self.assertLess(time.monotonic() - started, 30)
        self.assertEqual(result.returncode, 124)
        self.assertIn("go test: the command did not end before its deadline and was stopped (exit 124)",
                      result.stdout)
        self.assertIn("PASS      0.1s m/a", result.stdout)
        self.assertIn("go test: no package result for m/b", result.stdout)
        self.assertIn("Result: FAILED.", summary)

    def test_timeout_stops_the_command_and_still_reports(self):
        script = ("import json, time\n"
                  "print(json.dumps({'Action': 'pass', 'Package': 'm/a', 'Elapsed': 0.1}), flush=True)\n"
                  "time.sleep(60)\n")
        command = [sys.executable, "-c", script, "-count=1"]
        started = time.monotonic()
        result, _ = self.report([], "--timeout", "2", "--deadline", str(int(time.time()) + 600),
                                expect=["m/a"], command=command)
        self.assertLess(time.monotonic() - started, 30)
        self.assertEqual(result.returncode, 124)
        self.assertIn("go test: the command did not end before its deadline", result.stdout)
        self.assertIn("go test: intended set: 1 packages from go list; package results: 1", result.stdout)

    def test_no_test_process_outlives_the_command(self):
        with tempfile.TemporaryDirectory() as directory:
            pidfile = Path(directory) / "pid"
            # The background process has its own standard streams, so the
            # captured pipes of the reporter end when the reporter exits, not
            # when this process exits.
            script = (f'sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "{pidfile}"; '
                      "echo '{\"Action\":\"pass\",\"Package\":\"m/a\"}'")
            command = ["/bin/sh", "-c", script, "sh", "-count=1"]
            result, _ = self.report([], expect=["m/a"], command=command)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            pid = int(pidfile.read_text())
            # One wait for the exit event, with one time limit. The reporter
            # sends SIGKILL to the process group before it exits.
            if not process_exits_within(pid, 5):
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                self.fail("the background process of the command is still alive")

    def test_command_stopped_by_a_signal_fails(self):
        command = [sys.executable, "-c", "import os, signal; os.kill(os.getpid(), signal.SIGTERM)",
                   "-count=1"]
        result, _ = self.report([], expect=["m/a"], command=command)
        self.assertEqual(result.returncode, 143)

    def test_summary_file_needs_a_title_or_scenarios(self):
        result, _ = self.report([], "--summary-file", "SUMMARY")
        self.assertEqual(result.returncode, 2)
        self.assertIn("--summary-file requires --title or --scenarios", result.stderr)


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
