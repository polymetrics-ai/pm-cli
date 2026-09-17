#!/usr/bin/env python3
"""Focused event-policy and workflow regressions; no live services or credentials."""
import copy
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

import yaml

ROOT = Path(__file__).resolve().parents[2]
REPOSITORY = "polymetrics-ai/pm-cli"
SHA = "a" * 40


def payload(kind="pull_request", workflow="check.yml"):
    event = {
        "repository": {"full_name": REPOSITORY},
        "sender": {"login": "karthik-sivadas"},
    }
    env = {
        "GITHUB_REPOSITORY": REPOSITORY,
        "GITHUB_ACTOR": "karthik-sivadas",
        "GITHUB_TRIGGERING_ACTOR": "karthik-sivadas",
        "GITHUB_EVENT_NAME": kind,
        "GITHUB_SHA": SHA,
    }
    if kind == "pull_request":
        event.update(action="synchronize", number=52, pull_request={
            "number": 52, "state": "open", "merged": False,
            "head": {"repo": {"full_name": REPOSITORY}},
            "base": {"ref": "main", "repo": {"full_name": REPOSITORY}},
        })
        env["GITHUB_REF"] = "refs/pull/52/merge"
    else:
        event.update(ref="refs/heads/main", after=SHA, deleted=False, forced=False)
        env["GITHUB_REF"] = "refs/heads/main"
    env["GITHUB_WORKFLOW_REF"] = (
        f"{REPOSITORY}/.github/workflows/{workflow}@{env['GITHUB_REF']}"
    )
    return event, env


class AdmissionPolicy(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location(
            "runner_admission", ROOT / "scripts/ci/runner_admission.py"
        )
        cls.policy = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.policy)

    def test_open_pr_and_main_push_are_accepted(self):
        for kind in ("pull_request", "push"):
            for workflow in ("check.yml", "architecture-documentation.yml"):
                with self.subTest(kind=kind, workflow=workflow):
                    event, env = payload(kind, workflow)
                    self.policy.validate(event, env)

    def test_closed_pr_metadata_replay_is_rejected(self):
        event, env = payload()
        event["action"] = "edited"
        event["pull_request"].update(state="closed", merged=True)
        env["GITHUB_REF"] = "refs/heads/main"
        env["GITHUB_WORKFLOW_REF"] = f"{REPOSITORY}/.github/workflows/check.yml@refs/heads/main"
        with self.assertRaises(ValueError):
            self.policy.validate(event, env)

    def test_pr_trust_boundaries_fail_closed(self):
        event, env = payload()
        changes = [
            ("actor", lambda e, v: v.update(GITHUB_ACTOR="outsider")),
            ("rerun actor", lambda e, v: v.update(GITHUB_TRIGGERING_ACTOR="outsider")),
            ("sender", lambda e, v: e["sender"].update(login="outsider")),
            ("environment repo", lambda e, v: v.update(GITHUB_REPOSITORY="other/repo")),
            ("payload repo", lambda e, v: e["repository"].update(full_name="other/repo")),
            ("fork", lambda e, v: e["pull_request"]["head"]["repo"].update(full_name="other/repo")),
            ("base repo", lambda e, v: e["pull_request"]["base"]["repo"].update(full_name="other/repo")),
            ("base branch", lambda e, v: e["pull_request"]["base"].update(ref="release")),
            ("closed", lambda e, v: e["pull_request"].update(state="closed")),
            ("merged", lambda e, v: e["pull_request"].update(merged=True)),
            ("number mismatch", lambda e, v: e.update(number=53)),
            ("boolean number", lambda e, v: e.update(number=True)),
            ("action", lambda e, v: e.update(action="closed")),
            ("event", lambda e, v: v.update(GITHUB_EVENT_NAME="pull_request_target")),
            ("workflow", lambda e, v: v.update(GITHUB_WORKFLOW_REF=f"{REPOSITORY}/.github/workflows/other.yml@refs/pull/52/merge")),
            ("workflow ref", lambda e, v: v.update(GITHUB_WORKFLOW_REF=f"{REPOSITORY}/.github/workflows/check.yml@refs/heads/main")),
            ("ref", lambda e, v: v.update(GITHUB_REF="refs/pull/53/merge")),
            ("SHA", lambda e, v: v.update(GITHUB_SHA="not-a-sha")),
        ]
        for name, mutate in changes:
            with self.subTest(case=name):
                candidate, environment = copy.deepcopy(event), env.copy()
                mutate(candidate, environment)
                with self.assertRaises(ValueError):
                    self.policy.validate(candidate, environment)

    def test_push_only_accepts_exact_non_deleted_main_sha(self):
        for field, value in (("ref", "refs/heads/other"), ("after", "b" * 40),
                             ("deleted", True), ("forced", True)):
            with self.subTest(field=field):
                event, env = payload("push")
                event[field] = value
                with self.assertRaises(ValueError):
                    self.policy.validate(event, env)

    def test_malformed_payload_is_nonzero_without_traceback_or_process_control(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "event.json"
            path.write_text('{"repository": "sentinel-sensitive-value"')
            _, env = payload()
            result = subprocess.run(
                [sys.executable, "-I", "-B", str(ROOT / "scripts/ci/runner_admission.py")],
                env=dict(os.environ, **env, GITHUB_EVENT_PATH=str(path)),
                capture_output=True, text=True, timeout=5,
            )
            self.assertEqual(result.returncode, 1)
            self.assertNotIn("Traceback", result.stderr)
            self.assertNotIn("sentinel-sensitive-value", result.stderr)
        for name in ("runner_admission.py", "runner-admission.sh"):
            source = (ROOT / "scripts/ci" / name).read_text()
            for forbidden in ("pkill", "killall", "os.kill", "systemctl", "subprocess"):
                self.assertNotIn(forbidden, source)

    def test_entrypoint_accepts_and_rejects_without_exposing_payload(self):
        event, env = payload()
        for raw, expected in (
            (json.dumps(event), 0),
            ('{"secret": "sentinel-sensitive-value"', 1),
            ("null", 1),
            (json.dumps({"secret": "sentinel-sensitive-value"}), 1),
            (" " * (self.policy.MAX_EVENT_BYTES + 1), 1),
        ):
            with self.subTest(expected=expected), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "event.json"
                path.write_text(raw)
                with mock.patch.dict(os.environ, dict(env, GITHUB_EVENT_PATH=str(path)), clear=True), \
                        mock.patch.object(self.policy.os, "getuid", return_value=2001), \
                        mock.patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                        mock.patch("sys.stdout", new_callable=io.StringIO):
                    self.assertEqual(self.policy.main(), expected)
                    self.assertNotIn("sentinel-sensitive-value", stderr.getvalue())
                    self.assertNotIn("Traceback", stderr.getvalue())


class WorkflowPolicy(unittest.TestCase):
    def test_main_push_and_open_pr_are_separate(self):
        for name in ("check.yml", "architecture-documentation.yml"):
            with self.subTest(workflow=name):
                workflow = yaml.load((ROOT / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)
                self.assertEqual(workflow["on"].get("push"), {"branches": ["main"]})
                self.assertIn("github.event_name", workflow["concurrency"]["group"])
                self.assertIn("github.sha", workflow["concurrency"]["group"])
                self.assertEqual(workflow["concurrency"]["cancel-in-progress"], "${{ github.event_name == 'pull_request' }}")
                for job in workflow["jobs"].values():
                    self.assertIn("github.event.pull_request.state == 'open'", job["if"])
                    self.assertIn("github.event_name == 'push'", job["if"])
                    self.assertIn("github.ref == 'refs/heads/main'", job["if"])
                    self.assertIn("github.actor == 'karthik-sivadas'", job["if"])
                    self.assertEqual(job["steps"][0]["with"]["ref"], "${{ github.sha }}")


if __name__ == "__main__":
    unittest.main()
