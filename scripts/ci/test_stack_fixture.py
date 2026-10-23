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

    def test_podman_compat_info_has_a_stable_rootless_identity(self):
        # Podman 4.9 compat /info: a new random UUID per call, fixed license string.
        root = "/home/ghrunner/.local/share/containers/storage"
        options = ["name=apparmor", "name=seccomp,profile=default", "name=rootless"]
        observed = []
        for daemon_id in ("0b5f6a50-8d1e-4c33-9a43-1f0e7c2b9d10", "7d2c1e44-5b6a-4f8e-b1c2-93a4d5e6f708"):
            info = {"id": daemon_id, "os": "linux", "architecture": "amd64", "server_version": "4.9.3",
                    "security_options": options, "product_license": "Apache-2.0", "root_dir": root}
            with mock.patch.object(FIXTURE, "run_capture", return_value=(0, json.dumps(info).encode(), b"")):
                observed.append(FIXTURE.inspect_daemon(
                    Path("/unused/docker"), Path("/unused"), "unix:///run/docker.sock", {}, ROOT,
                    {"device": 1, "inode": 2, "uid": 0, "mode": 0o660}))
        self.assertEqual(observed[0], observed[1])
        self.assertEqual(observed[0]["id"], "podman:" + root)
        self.assertTrue(observed[0]["rootless"])
        self.assertEqual(FIXTURE.podman_root(observed[0]["id"]), root)
        docker_id = "QJ4M:ABCD:EFGH"
        self.assertEqual(FIXTURE.engine_identity(docker_id, "Community Engine", "/var/lib/docker"), docker_id)
        self.assertIsNone(FIXTURE.podman_root(docker_id))
        uuid = "0b5f6a50-8d1e-4c33-9a43-1f0e7c2b9d10"
        for daemon_id, root_dir in (("not-a-uuid", root), (uuid, "relative"), (uuid, None)):
            with self.subTest(daemon_id=daemon_id, root_dir=root_dir), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.engine_identity(daemon_id, "Apache-2.0", root_dir)

    def stack_container(self, service, fixture_root, podman):
        project = "pmci-" + "4" * 20
        refs = FIXTURE.load_image_refs(ROOT)
        nano_cpus, memory, pids_limit, readonly, restart = FIXTURE.service_limits(service)
        network = f"{project}_stack-private"
        mounts = [{"Type": kind, "Source": source, "Destination": destination, "RW": read_write}
                  for source, destination, read_write, kind in FIXTURE.expected_mounts(service, fixture_root, ROOT)]
        tmpfs = None
        if service == "caddy":
            options = "size=32m,mode=1777,tmpcopyup,rw,rprivate,nosuid,nodev" if podman else "mode=1777,size=32m"
            tmpfs = {"/tmp": options}
        value = {
            "id": "a" * 64, "name": f"/{project}-{service}-1", "service": service, "project": project,
            "oneoff": "False", "config_files": ",".join(sorted(FIXTURE.expected_config_files(ROOT))),
            "working_dir": str(ROOT), "image": FIXTURE.image_for_service(service, refs),
            "image_id": "sha256:" + "b" * 64, "user": "0:0", "privileged": False,
            "network_mode": "bridge" if podman else network, "networks": {network: {}},
            "pid_mode": "private" if podman else "", "ipc_mode": "shareable" if podman else "private",
            "uts_mode": "private" if podman else "", "userns_mode": "", "readonly_rootfs": readonly,
            "auto_remove": False, "init": True, "nano_cpus": nano_cpus, "memory": memory,
            "pids_limit": pids_limit, "restart": restart, "cap_add": [], "security_opt": [],
            "devices": [], "device_requests": None, "mounts": mounts, "tmpfs": tmpfs,
            "port_bindings": FIXTURE.expected_ports(service, self.PORTS) or None,
        }
        return value, project, refs

    PORTS = {"PM_STACK_PUBLIC_PORT": "12005"}

    def test_podman_inspection_spellings_are_accepted_only_for_podman(self):
        fixture_root = Path("/private/fixture")
        root = "/home/ghrunner/.local/share/containers/storage"

        def validate(value, project, refs, engine_root):
            return FIXTURE.validate_stack_container(value, project, fixture_root, ROOT, self.PORTS, refs,
                                                    rootless=True, engine_root=engine_root)

        for service in ("seaweed-master", "caddy"):
            with self.subTest(service=service):
                value, project, refs = self.stack_container(service, fixture_root, podman=True)
                validate(value, project, refs, root)
                with self.assertRaises(FIXTURE.FixtureError):
                    validate(value, project, refs, None)
                docker, _, _ = self.stack_container(service, fixture_root, podman=False)
                validate(docker, project, refs, None)
                for field, unsafe in (("pid_mode", "host"), ("ipc_mode", "host"), ("uts_mode", "host"),
                                      ("userns_mode", "private"), ("network_mode", "host"),
                                      ("networks", {f"{project}_stack-private": {}, "podman": {}})):
                    with self.subTest(field=field), self.assertRaises(FIXTURE.FixtureError):
                        validate(dict(value, **{field: unsafe}), project, refs, root)
        value, project, refs = self.stack_container("caddy", fixture_root, podman=True)
        with self.assertRaises(FIXTURE.FixtureError):
            validate(dict(value, tmpfs={"/tmp": "size=32m,mode=1777,exec"}), project, refs, root)

    def test_podman_anonymous_volume_must_be_under_its_storage_root(self):
        name = "c" * 64
        root = "/home/ghrunner/.local/share/containers/storage/volumes"
        volume = {"Type": "volume", "Name": name, "Driver": "local", "RW": True,
                  "Destination": "/var/lib/postgresql/data", "Source": f"{root}/{name}/_data"}
        self.assertEqual(FIXTURE.actual_mounts([volume], allow_postgres_anonymous=True, volume_root=root), set())
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.actual_mounts([volume], allow_postgres_anonymous=True)
        moved = dict(volume, Source=f"/var/lib/docker/volumes/{name}/_data")
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.actual_mounts([moved], allow_postgres_anonymous=True, volume_root=root)

    def test_podman_cleanup_helper_lists_each_dropped_capability(self):
        fixture_root = Path("/private/fixture")
        project = "pmci-" + "5" * 20
        refs = FIXTURE.load_image_refs(ROOT)
        data = fixture_root / "state" / "data" / "postgres"
        value = {
            "id": "d" * 64, "name": f"/{project}-cleanup", "helper": "true", "helper_project": project,
            "helper_root": hashlib.sha256(str(fixture_root).encode()).hexdigest(),
            "project": None, "service": None, "image": refs["postgres"], "image_id": "sha256:" + "e" * 64,
            "user": "postgres", "privileged": False, "network_mode": "none", "readonly_rootfs": True,
            "nano_cpus": 250_000_000, "memory": 64 << 20, "pids_limit": 32, "port_bindings": None,
            "cap_add": [], "pid_mode": "private", "ipc_mode": "shareable", "uts_mode": "private",
            "userns_mode": "", "init": False, "auto_remove": False, "restart": "no", "devices": [],
            "device_requests": None, "tmpfs": {}, "security_opt": ["no-new-privileges"],
            "cap_drop": sorted(FIXTURE.PODMAN_DEFAULT_CAPABILITIES),
            "mounts": [{"Type": "bind", "Source": str(data), "Destination": "/var/lib/postgresql/data", "RW": True}],
        }

        def manifest(daemon_id):
            return types.SimpleNamespace(project=project, fixture_root=fixture_root, daemon={"id": daemon_id})

        podman = manifest("podman:/home/ghrunner/.local/share/containers/storage")
        FIXTURE.validate_helper_container(value, podman, refs)
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.validate_helper_container(value, manifest("QJ4M:ABCD:EFGH"), refs)
        retained = dict(value, cap_drop=sorted(FIXTURE.PODMAN_DEFAULT_CAPABILITIES - {"SETUID"}))
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.validate_helper_container(retained, podman, refs)
        docker = dict(value, cap_drop=["ALL"], pid_mode="", ipc_mode="private", uts_mode="")
        FIXTURE.validate_helper_container(docker, manifest("QJ4M:ABCD:EFGH"), refs)

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
