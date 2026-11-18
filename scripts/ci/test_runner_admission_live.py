#!/usr/bin/env python3
"""Negative checks of the live admission policy (L-e1 P2). Developer checks, not acceptance proof.

One valid event and environment. Each case changes one thing and must be
refused, unless it is marked as accepted. The root files live in a
temporary root, and the owner is the user of the test (read_root_file
parameters).
"""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
SHA = "a" * 40
OWNER = "karthik-sivadas"
REPOSITORY = "polymetrics-ai/pm-cli"
REFUSAL = "PMCLI live admission rejected: event outside approved policy.\n"
# The owner of the test root files. Some tests patch os.getuid.
USER = os.getuid()


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts/ci" / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


LIVE = load("runner_admission_live", "runner_admission_live.py")
SHARED = load("runner_admission", "runner_admission.py")


def valid():
    event = {"repository": {"full_name": REPOSITORY}, "sender": {"login": OWNER},
             "ref": "refs/heads/main", "workflow": ".github/workflows/live-w2.yml"}
    env = {"GITHUB_REPOSITORY": REPOSITORY, "GITHUB_ACTOR": OWNER, "GITHUB_TRIGGERING_ACTOR": OWNER,
           "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
           "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/live-w2.yml@refs/heads/main",
           "GITHUB_RUN_ATTEMPT": "1", "GITHUB_SHA": SHA, "GITHUB_WORKFLOW_SHA": SHA}
    return event, env


def make_root(directory, approved=SHA.encode() + b"\n", kind=b"w2\n"):
    """A root with etc/pm-live/approved-sha and run-kind (directories 0755, files 0644)."""
    root = Path(directory).resolve()
    folder = root / "etc" / "pm-live"
    folder.mkdir(parents=True)
    for path in (root, root / "etc", folder):
        path.chmod(0o755)
    for name, data in (("approved-sha", approved), ("run-kind", kind)):
        if data is not None:
            (folder / name).write_bytes(data)
            (folder / name).chmod(0o644)
    return root


@contextlib.contextmanager
def foreign_owner(path):
    """fstat reports another owner for PATH only (a test cannot chown)."""
    inode, real = os.stat(path, follow_symlinks=False).st_ino, os.fstat

    def fstat(descriptor):
        status = real(descriptor)
        if status.st_ino != inode:
            return status
        values = list(status)
        values[4] = status.st_uid + 1
        return os.stat_result(values)

    with mock.patch.object(LIVE.os, "fstat", fstat):
        yield


class LiveAdmission(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = make_root(self.directory.name)

    def tearDown(self):
        self.directory.cleanup()

    def check(self, event, env, root=None):
        LIVE.validate(event, env, root or self.root, USER)

    def refused(self, name, mutate, root=None):
        event, env = valid()
        mutate(event, env)
        with self.subTest(case=name), self.assertRaises(ValueError):
            self.check(event, env, root)

    def test_the_valid_event_is_accepted(self):
        self.check(*valid())
        for kind in (b"rehearsal\n", b"w2\n"):
            with tempfile.TemporaryDirectory() as directory:
                self.check(*valid(), root=make_root(directory, kind=kind))

    def test_runner_identity(self):
        for configured, uid in (("", 1002), ("1002x", 1002), ("١٠", 10), ("1004", 1002),
                                ("0", 0), (None, 1002)):
            with self.subTest(configured=configured), \
                    mock.patch.object(LIVE.os, "getuid", return_value=uid), self.assertRaises(ValueError):
                LIVE.require_runner_identity({} if configured is None else {"PMCLI_RUNNER_UID": configured})
        with mock.patch.object(LIVE.os, "getuid", return_value=1002):
            LIVE.require_runner_identity({"PMCLI_RUNNER_UID": "1002"})

    def test_repository_and_actor(self):
        self.refused("environment repository", lambda e, v: v.update(GITHUB_REPOSITORY="other/repo"))
        self.refused("payload repository", lambda e, v: e["repository"].update(full_name="other/repo"))
        self.refused("actor", lambda e, v: v.update(GITHUB_ACTOR="outsider"))
        self.refused("triggering actor", lambda e, v: v.update(GITHUB_TRIGGERING_ACTOR="outsider"))
        self.refused("sender", lambda e, v: e["sender"].update(login="outsider"))

    def test_event_and_inputs(self):
        for kind in ("push", "pull_request", "schedule", "repository_dispatch", "workflow_run"):
            self.refused(kind, lambda e, v, kind=kind: v.update(GITHUB_EVENT_NAME=kind))
        for inputs in ({"a": ""}, {"": ""}, [], "", 0, False):
            self.refused(f"inputs {inputs!r}", lambda e, v, inputs=inputs: e.update(inputs=inputs))
        for inputs in (None, {}):
            event, env = valid()
            event["inputs"] = inputs
            self.check(event, env)

    def test_ref_and_workflow(self):
        for name, environment, payload in (
                ("environment other", "refs/heads/other", "refs/heads/main"),
                ("payload other", "refs/heads/main", "refs/heads/other"),
                ("both other", "refs/heads/other", "refs/heads/other"),
                ("mainx", "refs/heads/mainx", "refs/heads/mainx"),
                ("main/x", "refs/heads/main/x", "refs/heads/main/x"),
                ("pull", "refs/pull/1/merge", "refs/pull/1/merge"),
                ("tag", "refs/tags/v1", "refs/tags/v1")):
            self.refused(name, lambda e, v, a=environment, b=payload: (v.update(GITHUB_REF=a), e.update(ref=b)))
        prefix = f"{REPOSITORY}/.github/workflows/"
        for workflow in (prefix + "bdd.yml@refs/heads/main", prefix + "live-w2.yml@refs/heads/other",
                         prefix + "live-w2.yml@refs/heads/main-old",
                         "other/repo/.github/workflows/live-w2.yml@refs/heads/main"):
            self.refused(workflow, lambda e, v, w=workflow: v.update(GITHUB_WORKFLOW_REF=w))

    def test_attempt_and_sha(self):
        self.refused("attempt 2", lambda e, v: v.update(GITHUB_RUN_ATTEMPT="2"))
        self.refused("attempt missing", lambda e, v: v.pop("GITHUB_RUN_ATTEMPT"))
        self.refused("attempt 01", lambda e, v: v.update(GITHUB_RUN_ATTEMPT="01"))
        last = "a" * 39 + "b"
        self.refused("last character", lambda e, v: v.update(GITHUB_SHA=last, GITHUB_WORKFLOW_SHA=last))
        self.refused("workflow SHA", lambda e, v: v.update(GITHUB_WORKFLOW_SHA="b" * 40))
        self.refused("upper case", lambda e, v: v.update(GITHUB_SHA="A" * 40, GITHUB_WORKFLOW_SHA="A" * 40))
        for length in (39, 41):
            self.refused(f"{length} characters",
                         lambda e, v, s="a" * length: v.update(GITHUB_SHA=s, GITHUB_WORKFLOW_SHA=s))

    def test_run_kind_values(self):
        for data in (b"w2 \n", b"W2\n", b"rehearsal\nw2\n", b"none\n"):
            with tempfile.TemporaryDirectory() as directory:
                self.refused(repr(data), lambda e, v: None, make_root(directory, kind=data))

    def test_root_file_faults(self):
        for name in ("approved-sha", "run-kind"):
            good = SHA.encode() + b"\n" if name == "approved-sha" else b"w2\n"
            contents = {"missing": None, "empty": b"", "two lines": good + good, "no final break": good[:-1],
                        "carriage return": good[:-1] + b"\r\n", "NUL": good[:-1] + b"\0\n",
                        "trailing data": good + b"x", "wrong format": b"x\n", "65 bytes": b"a" * 64 + b"\n"}
            for case, data in contents.items():
                with tempfile.TemporaryDirectory() as directory:
                    self.refused(f"{name} {case}", lambda e, v: None,
                                 make_root(directory, **{"approved" if name == "approved-sha" else "kind": data}))
            for case in ("file link", "parent link", "FIFO", "directory", "file owner", "file group-writable",
                         "file world-writable", "parent owner", "parent group-writable", "parent world-writable"):
                with tempfile.TemporaryDirectory() as directory:
                    root = make_root(directory)
                    folder, file = root / "etc" / "pm-live", root / "etc" / "pm-live" / name
                    with self.fault(case, root, folder, file):
                        self.refused(f"{name} {case}", lambda e, v: None, root)

    @contextlib.contextmanager
    def fault(self, case, root, folder, file):
        if case == "file link":
            (folder / "real").write_bytes(file.read_bytes())
            file.unlink()
            file.symlink_to(folder / "real")
        elif case == "parent link":
            folder.rename(root / "etc" / "real")
            folder.symlink_to(root / "etc" / "real")
        elif case in ("FIFO", "directory"):
            file.unlink()
            os.mkfifo(file) if case == "FIFO" else file.mkdir()
        elif case.endswith("writable"):
            target = file if case.startswith("file") else folder
            target.chmod(target.stat().st_mode | (0o020 if "group" in case else 0o002))
        if case.endswith("owner"):
            with foreign_owner(file if case.startswith("file") else folder):
                yield
        else:
            yield

    def test_the_cases_of_the_surviving_mutations(self):
        # crrao I4 at 481df57: each case catches one mutation that survived his
        # mutation run (his section 3 table).
        self.refused("environment mainx, payload main", lambda e, v: v.update(GITHUB_REF="refs/heads/mainx"))
        upper = "A" * 40
        with tempfile.TemporaryDirectory() as directory:
            self.refused("upper-case SHA in the environment and the file",
                         lambda e, v: v.update(GITHUB_SHA=upper, GITHUB_WORKFLOW_SHA=upper),
                         make_root(directory, approved=upper.encode() + b"\n"))
        for name, data in (("kind", b"w2x"), ("approved", SHA.encode() + b"0")):
            with tempfile.TemporaryDirectory() as directory:
                self.refused(f"{name}: no final line break, valid less one byte", lambda e, v: None,
                             make_root(directory, **{name: data}))

    def test_shared_policy_refuses_the_live_event(self):
        # R2-1: the valid live event and environment, given to the shared
        # policy of the CI runners, must be refused.
        with self.assertRaises(ValueError):
            SHARED.validate(*valid())

    def test_each_named_check_is_called(self):
        # For each named check, one case that only that check refuses.
        cases = {
            "require_repository": lambda e, v: v.update(GITHUB_REPOSITORY="other/repo"),
            "require_actor": lambda e, v: v.update(GITHUB_ACTOR="outsider"),
            "require_event": lambda e, v: e.update(inputs={"a": ""}),
            "require_live_ref": lambda e, v: v.update(GITHUB_REF="refs/heads/other"),
            "require_workflow": lambda e, v: v.update(
                GITHUB_WORKFLOW_REF=f"{REPOSITORY}/.github/workflows/bdd.yml@refs/heads/main"),
            "require_first_attempt": lambda e, v: v.update(GITHUB_RUN_ATTEMPT="2"),
            "require_approved_sha": lambda e, v: v.update(GITHUB_SHA="b" * 40, GITHUB_WORKFLOW_SHA="b" * 40),
            "require_run_kind": "root",
        }
        names = sorted(name for name in vars(LIVE) if name.startswith("require_")
                       and name not in ("require_owned", "require_runner_identity"))
        self.assertEqual(sorted(cases), names)
        for name, mutate in cases.items():
            raised = []

            def spy(original, label):
                def wrapper(*arguments):
                    try:
                        return original(*arguments)
                    except ValueError:
                        raised.append(label)
                        raise
                return wrapper

            event, env = valid()
            root = self.root
            with tempfile.TemporaryDirectory() as directory:
                if mutate == "root":
                    root = make_root(directory, kind=b"none\n")
                else:
                    mutate(event, env)
                with contextlib.ExitStack() as stack:
                    for label in cases:
                        stack.enter_context(mock.patch.object(LIVE, label, spy(getattr(LIVE, label), label)))
                    with self.subTest(check=name), self.assertRaises(ValueError):
                        LIVE.validate(event, env, root, USER)
                self.assertEqual(raised, [name])
        with mock.patch.object(LIVE.os, "getuid", return_value=1002), self.entry(json.dumps(valid()[0]),
                                                                                   valid()[1]) as run:
            self.assertEqual(run("1004"), (1, "", REFUSAL))

    @contextlib.contextmanager
    def entry(self, raw, env):
        """Run main() on the event text RAW. The result is (status, stdout, stderr)."""
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "event.json"
            path.write_text(raw)

            def run(uid="1002"):
                environment = dict(env, GITHUB_EVENT_PATH=str(path), PMCLI_RUNNER_UID=uid)
                with mock.patch.dict(os.environ, environment, clear=True), \
                        mock.patch("sys.stdout", new_callable=io.StringIO) as out, \
                        mock.patch("sys.stderr", new_callable=io.StringIO) as err:
                    status = LIVE.main(self.root, USER)
                return status, out.getvalue(), err.getvalue()
            yield run

    def test_bounds_and_wiring(self):
        event, env = valid()
        secret = "sentinel-event-value"
        with mock.patch.object(LIVE.os, "getuid", return_value=1002):
            with self.entry(json.dumps(event), env) as run:
                self.assertEqual(run(), (0, "PMCLI live admission accepted.\n", ""))
            for name, raw in (("size", " " * (LIVE.MAX_EVENT_BYTES + 1)),
                              ("malformed", '{"repository": "' + secret),
                              ("wrong type", json.dumps(dict(event, repository=secret))),
                              ("sender type", json.dumps(dict(event, sender={"login": 7}))),
                              ("list", json.dumps([secret]))):
                with self.subTest(case=name), self.entry(raw, env) as run:
                    self.assertEqual(run(), (1, "", REFUSAL))

    def test_wrapper_refuses_with_the_fixed_line(self):
        # The wrapper runs the copy of the policy next to it. The test copy
        # calls this Python, because /usr/bin/timeout is not on every host.
        wrapper = (ROOT / "scripts/ci/runner-admission-live.sh").read_text()
        line = 'exec /usr/bin/timeout 10 /usr/bin/python3 -I -B "$hooks/runner_admission_live.py"\n'
        self.assertIn(line, wrapper)
        event, env = valid()
        with tempfile.TemporaryDirectory() as directory:
            hooks = Path(directory)
            (hooks / "runner-admission-live.sh").write_text(
                wrapper.replace(line, f'exec "{sys.executable}" -I -B "$hooks/runner_admission_live.py"\n'))
            (hooks / "runner_admission_live.py").write_text(
                (ROOT / "scripts/ci/runner_admission_live.py").read_text())
            (hooks / "event.json").write_text(json.dumps(dict(event, secret="sentinel-event-value")))
            result = subprocess.run(["/bin/sh", str(hooks / "runner-admission-live.sh")],
                                    env=dict(env, PATH="/usr/bin:/bin", GITHUB_EVENT_PATH=str(hooks / "event.json"),
                                             PMCLI_RUNNER_UID=str(USER)),
                                    capture_output=True, text=True, timeout=30)
        # The real /etc/pm-live does not hold this approved SHA, or the UID is 0.
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", REFUSAL))


if __name__ == "__main__":
    unittest.main()
