#!/usr/bin/env python3
"""Behavioral checks for fixture ownership and process-group cancellation."""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import selectors
import signal
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("stack_fixture", ROOT / "scripts/ci/with-stack-fixture.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)


class FixtureOwnershipTests(unittest.TestCase):
    def test_rootless_mapping_requires_observed_daemon_option(self):
        self.assertTrue(FIXTURE.rootless_engine(["name=seccomp,profile=builtin", "name=rootless"]))
        self.assertFalse(FIXTURE.rootless_engine(["name=seccomp,profile=builtin"]))
        self.assertFalse(FIXTURE.rootless_engine(["name=not-rootless"]))
        for options in (None, {}, [False], ["name=userns"]):
            with self.subTest(options=options), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.rootless_engine(options)

    def run_with_preflight(self, runner_temp, token):
        with contextlib.ExitStack() as stack:
            for name, value in (
                ("regular_executable", ROOT / ".cache/bin/unused"),
                ("resolve_docker", Path("/unused/docker")),
                ("validate_compose_cache", None),
                ("load_image_refs", {}),
                ("choose_docker_socket", (Path("/unused/docker.sock"), {})),
            ):
                stack.enter_context(mock.patch.object(FIXTURE, name, return_value=value))
            stack.enter_context(mock.patch.object(FIXTURE.secrets, "token_hex", return_value=token))
            stack.enter_context(mock.patch.object(FIXTURE, "SIGNALS", FIXTURE.SignalController()))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            return FIXTURE.run_mode(
                runner_temp / "pmcli-foundation-90001-1.json",
                runner_temp, ROOT, "90001", "1", ["never-execute"], {},
            )

    def test_colliding_parent_is_not_removed(self):
        with tempfile.TemporaryDirectory() as temporary:
            runner_temp = Path(temporary).resolve()
            token = "1" * 20
            existing = runner_temp / (FIXTURE.PARENT_PREFIX + token)
            existing.mkdir(mode=0o700)
            marker = existing / "unrelated-data"
            marker.write_bytes(b"must survive refusal")
            self.assertNotEqual(self.run_with_preflight(runner_temp, token), 0)
            self.assertEqual(marker.read_bytes(), b"must survive refusal")

    def test_replaced_parent_is_not_removed_after_preflight_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            runner_temp = Path(temporary).resolve()
            token = "2" * 20
            parent = runner_temp / (FIXTURE.PARENT_PREFIX + token)
            marker = parent / "replacement-data"

            def replace_parent(*_arguments):
                parent.rename(runner_temp / "original-owned-parent")
                parent.mkdir(mode=0o700)
                marker.write_bytes(b"different directory identity")
                raise FIXTURE.FixtureError("injected daemon failure")

            with mock.patch.object(FIXTURE, "inspect_daemon", side_effect=replace_parent):
                self.assertNotEqual(self.run_with_preflight(runner_temp, token), 0)
            self.assertEqual(marker.read_bytes(), b"different directory identity")

    def test_temporal_profile_identity_and_digest_are_immutable(self):
        with tempfile.TemporaryDirectory() as temporary:
            parent = Path(temporary).resolve()
            repo_root = parent / "repo"
            fixture_root = parent / "fixture"
            repo_root.mkdir(mode=0o700)
            fixture_root.mkdir(mode=0o700)
            project = "pmci-" + "3" * 20
            environment = {
                "PM_STACK_ROOT": str(repo_root),
                "PM_STACK_SECRETS_DIR": str(fixture_root / "secrets"),
                "PM_STACK_STATE_DIR": str(fixture_root / "state"),
                "PM_STACK_PROJECT": project,
                "PM_STACK_TEMPORAL_DYNAMIC_CONFIG": str(fixture_root / "temporal-dynamicconfig.yaml"),
                "PM_STACK_TEST_CONFIG": str(fixture_root / "server.json"),
                "PM_STACK_TEST_ROLLOVER_NAMESPACE": project + "-rollover",
                "PM_STACK_TEST_S3_PRIMARY_ENDPOINT": "https://127.0.0.1:12003",
                "PM_STACK_TEST_S3_SECONDARY_ENDPOINT": "https://127.0.0.1:12004",
                "PM_STORE_TEST_SECRETS_DIR": str(fixture_root / "secrets/operator/postgres"),
                "PM_STACK_POSTGRES_PORT": "12001",
                "PM_STACK_TEMPORAL_PORT": "12002",
                "PM_STACK_S3_PRIMARY_PORT": "12003",
                "PM_STACK_S3_SECONDARY_PORT": "12004",
                "PM_STACK_PUBLIC_PORT": "12005",
            }
            environment_bytes = (json.dumps(environment, sort_keys=True) + "\n").encode()
            config_bytes = b'{"schema_version":"1"}\n'
            profile_bytes = b"system.enableDeployments:\n  - value: true\n"
            (fixture_root / "environment.json").write_bytes(environment_bytes)
            (fixture_root / "server.json").write_bytes(config_bytes)
            profile_path = fixture_root / "temporal-dynamicconfig.yaml"
            profile_path.write_bytes(profile_bytes)
            record = FIXTURE.fixture_record(
                fixture_root,
                environment,
                hashlib.sha256(environment_bytes).hexdigest(),
                hashlib.sha256(config_bytes).hexdigest(),
                hashlib.sha256(profile_bytes).hexdigest(),
            )
            manifest = types.SimpleNamespace(
                project=project,
                repo_root=repo_root,
                fixture_root=fixture_root,
                document={"fixture": record},
            )
            self.assertEqual(FIXTURE.validate_fixture_record(manifest), environment)
            FIXTURE.verify_immutable_fixture_files(manifest, environment)
            profile_path.write_bytes(profile_bytes + b"tampered: true\n")
            with self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.verify_immutable_fixture_files(manifest, environment)

    def test_temporal_mount_uses_owned_profile_not_repository_default(self):
        fixture_root = Path("/private/fixture")
        repo_root = Path("/private/repository")
        mounts = FIXTURE.expected_mounts("temporal", fixture_root, repo_root)
        self.assertIn(
            (
                str(fixture_root / "temporal-dynamicconfig.yaml"),
                "/etc/temporal/dynamicconfig.yaml",
                False,
                "bind",
            ),
            mounts,
        )
        self.assertNotIn(
            (
                str(repo_root / "deploy/temporal/dynamicconfig.yaml"),
                "/etc/temporal/dynamicconfig.yaml",
                False,
                "bind",
            ),
            mounts,
        )

    def test_bounded_reader_refuses_fifo_without_blocking(self):
        if not hasattr(os, "mkfifo"):
            self.skipTest("FIFO is unavailable")
        with tempfile.TemporaryDirectory() as temporary:
            fifo = Path(temporary) / "profile"
            os.mkfifo(fifo, 0o600)
            result = []

            def read_fifo():
                try:
                    FIXTURE.read_limited(fifo)
                except Exception as exc:  # behavior under test is the exact safe refusal type below
                    result.append(exc)

            reader = threading.Thread(target=read_fifo, daemon=True)
            reader.start()
            reader.join(timeout=2)
            self.assertFalse(reader.is_alive(), "bounded reader blocked opening a FIFO")
            self.assertEqual(len(result), 1)
            self.assertIsInstance(result[0], FIXTURE.FixtureError)

    def test_cancellation_reaps_a_leader_exiting_during_group_signal(self):
        process = subprocess.Popen(
            [sys.executable, "-I", "-B", "-c", "import time;print('ready',flush=True);time.sleep(30)"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
        )
        signal_group = os.killpg
        raced = False

        def exit_before_signal(group, signum):
            nonlocal raced
            if not raced and group == process.pid and signum == signal.SIGTERM:
                raced = True
                process.terminate()
                process.stdout.read()
            return signal_group(group, signum)

        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                self.assertTrue(selector.select(timeout=5), "leader did not become ready")
            self.assertEqual(process.stdout.readline(), b"ready\n")
            with mock.patch.object(os, "killpg", side_effect=exit_before_signal):
                FIXTURE.terminate_process_group(process)
            self.assertEqual(process.wait(timeout=2), -signal.SIGTERM)
        finally:
            if process.poll() is None:
                process.kill()
            process.communicate(timeout=5)

    def test_cancellation_stops_descendant_that_ignores_term(self):
        descendant = (
            "import signal,time; "
            "signal.signal(signal.SIGTERM,signal.SIG_IGN); "
            "print('ready',flush=True); time.sleep(30)"
        )
        leader = (
            "import subprocess,sys,time; "
            f"subprocess.Popen([sys.executable,'-c',{descendant!r}]); time.sleep(30)"
        )
        process = subprocess.Popen(
            [sys.executable, "-I", "-B", "-c", leader],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
        )
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                self.assertTrue(selector.select(timeout=5), "descendant did not become ready")
            self.assertEqual(process.stdout.readline(), b"ready\n")
            process.terminate()
            process.wait(timeout=2)
            with mock.patch.object(FIXTURE, "PROCESS_TERM_GRACE", 0.1):
                FIXTURE.terminate_process_group(process)
            try:
                process.communicate(timeout=2)
            except subprocess.TimeoutExpired:
                self.fail("cancellation left a live descendant holding the process output open")
        finally:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
