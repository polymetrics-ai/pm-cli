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


def payload(ref="refs/heads/main", workflow="bdd.yml", kind="push"):
    event = {
        "repository": {"full_name": REPOSITORY},
        "sender": {"login": "karthik-sivadas"},
        "ref": ref, "after": SHA, "deleted": False, "forced": False,
    }
    env = {
        "GITHUB_REPOSITORY": REPOSITORY,
        "GITHUB_ACTOR": "karthik-sivadas",
        "GITHUB_TRIGGERING_ACTOR": "karthik-sivadas",
        "GITHUB_EVENT_NAME": kind,
        "GITHUB_SHA": SHA,
        "GITHUB_WORKFLOW_SHA": SHA,
        "GITHUB_REF": ref,
        "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/{workflow}@{ref}",
    }
    return event, env


class AdmissionPolicy(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location(
            "runner_admission", ROOT / "scripts/ci/runner_admission.py"
        )
        cls.policy = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.policy)

    def test_branch_pushes_are_accepted(self):
        for ref in ("refs/heads/main", "refs/heads/agent/72-godog-harness",
                    "refs/heads/ci/dev-loop", "refs/heads/feature_x.y"):
            with self.subTest(ref=ref):
                self.policy.validate(*payload(ref))
        event, env = payload("refs/heads/agent/72-godog-harness")
        event["forced"] = True
        self.policy.validate(event, env)

    def test_only_the_bdd_workflow_on_push_is_admitted(self):
        for workflow in ("check.yml", "governance.yml", "agent-pr.yml", "other.yml"):
            with self.subTest(workflow=workflow), self.assertRaises(ValueError):
                self.policy.validate(*payload(workflow=workflow))
        for kind in ("pull_request", "pull_request_target", "workflow_run", "workflow_dispatch"):
            with self.subTest(kind=kind), self.assertRaises(ValueError):
                self.policy.validate(*payload(kind=kind))

    def test_push_trust_boundaries_fail_closed(self):
        changes = [
            ("actor", lambda e, v: v.update(GITHUB_ACTOR="outsider")),
            ("rerun actor", lambda e, v: v.update(GITHUB_TRIGGERING_ACTOR="outsider")),
            ("sender", lambda e, v: e["sender"].update(login="outsider")),
            ("environment repo", lambda e, v: v.update(GITHUB_REPOSITORY="other/repo")),
            ("payload repo", lambda e, v: e["repository"].update(full_name="other/repo")),
            ("forced main", lambda e, v: e.update(forced=True)),
            ("deleted", lambda e, v: e.update(deleted=True)),
            ("after mismatch", lambda e, v: e.update(after="b" * 40)),
            ("ref mismatch", lambda e, v: v.update(GITHUB_REF="refs/heads/other")),
            ("tag", lambda e, v: (e.update(ref="refs/tags/v1"), v.update(GITHUB_REF="refs/tags/v1"))),
            ("pull ref", lambda e, v: (e.update(ref="refs/pull/1/merge"), v.update(GITHUB_REF="refs/pull/1/merge"))),
            ("dot-dot ref", lambda e, v: (e.update(ref="refs/heads/a..b"), v.update(GITHUB_REF="refs/heads/a..b"))),
            ("workflow ref branch", lambda e, v: v.update(GITHUB_WORKFLOW_REF=f"{REPOSITORY}/.github/workflows/bdd.yml@refs/heads/other")),
            ("SHA", lambda e, v: v.update(GITHUB_SHA="not-a-sha")),
            ("workflow SHA mismatch", lambda e, v: v.update(GITHUB_WORKFLOW_SHA="b" * 40)),
            ("workflow SHA absent", lambda e, v: v.pop("GITHUB_WORKFLOW_SHA")),
        ]
        for name, mutate in changes:
            with self.subTest(case=name):
                event, env = payload()
                mutate(event, env)
                with self.assertRaises(ValueError):
                    self.policy.validate(event, env)

    def test_malformed_payload_is_nonzero_without_traceback(self):
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
                with mock.patch.dict(os.environ, dict(env, GITHUB_EVENT_PATH=str(path),
                                                      PMCLI_RUNNER_UID="1002"), clear=True), \
                        mock.patch.object(self.policy.os, "getuid", return_value=1002), \
                        mock.patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                        mock.patch("sys.stdout", new_callable=io.StringIO):
                    self.assertEqual(self.policy.main(), expected)
                    self.assertNotIn("sentinel-sensitive-value", stderr.getvalue())
                    self.assertNotIn("Traceback", stderr.getvalue())


    def test_runner_identity_is_the_configured_unprivileged_uid(self):
        for uid, configured, accepted in ((1002, "1002", True), (1004, "1002", False),
                                          (0, "0", False), (1002, "", False), (1002, "1002x", False)):
            with self.subTest(uid=uid, configured=configured), \
                    mock.patch.object(self.policy.os, "getuid", return_value=uid):
                if accepted:
                    self.policy.require_runner_identity({"PMCLI_RUNNER_UID": configured})
                else:
                    with self.assertRaises(ValueError):
                        self.policy.require_runner_identity({"PMCLI_RUNNER_UID": configured})


class WorkflowPolicy(unittest.TestCase):
    def load(self, name):
        return yaml.load((ROOT / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)

    def test_bdd_runs_on_owner_pushes_only(self):
        workflow = self.load("bdd.yml")
        self.assertEqual(workflow["on"], {"push": {"branches": ["**"]}})
        self.assertEqual(workflow["concurrency"]["cancel-in-progress"],
                         "${{ github.ref != 'refs/heads/main' }}")
        job = workflow["jobs"]["bdd"]
        self.assertIn("github.event_name == 'push'", job["if"])
        self.assertIn("github.actor == 'karthik-sivadas'", job["if"])
        self.assertIn("github.event.forced == false", job["if"])
        self.assertEqual(job["steps"][0]["with"]["ref"], "${{ github.sha }}")

    def test_agent_pr_never_runs_pushed_code(self):
        workflow = self.load("agent-pr.yml")
        self.assertEqual(workflow["on"], {"push": {"branches": ["agent/**"]}})
        job = workflow["jobs"]["draft-pr"]
        self.assertEqual(job["runs-on"], "ubuntu-24.04")
        self.assertIn("github.actor == 'karthik-sivadas'", job["if"])
        self.assertTrue(all("uses" not in step for step in job["steps"]))
        self.assertEqual(workflow["permissions"], {"contents": "read", "pull-requests": "write"})


if __name__ == "__main__":
    unittest.main()
