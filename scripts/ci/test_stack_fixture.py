#!/usr/bin/env python3
"""Behavioral checks for fixture ownership and process-group cancellation."""

import base64
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import selectors
import signal
import socket
import ssl
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


# The teardown check and the FIFO reader check of test_setup.py.
SETUP_SPEC = importlib.util.spec_from_file_location("test_setup", ROOT / "scripts/ci/test_setup.py")
SETUP = importlib.util.module_from_spec(SETUP_SPEC)
SETUP_SPEC.loader.exec_module(SETUP)
processes_holding, no_fifo_reader = SETUP.processes_holding, SETUP.no_fifo_reader


class ExitedLeader:
    """A process group whose leader exited and is not reaped (F1 at 52a87ec).

    The leader and, with DESCENDANT, one group member are children of the
    test, so each Popen keeps its identity until the reap. The member opens
    "hold" and acknowledges on "ack". The leader then exits, and the member
    blocks on "hold" until release(). The test holds both FIFOs open, so no
    open blocks. Each end and process is registered when it is acquired. A
    failed setup releases the FIFOs, then kills and collects each process.
    """

    def __init__(self, directory, descendant):
        self.ends, self.processes, self.member = [], [], None
        try:
            hold, ack = Path(directory) / "hold", Path(directory) / "ack"
            for path in (hold, ack):
                os.mkfifo(path)
                self.ends.append(os.open(path, os.O_RDWR))
            scripts = ['read ack < "$1"', 'exec 3<"$0"; echo opened > "$1"; exec cat <&3 > /dev/null']
            for script in (scripts if descendant else ["exit 0"]):
                group = self.processes[0].pid if self.processes else 0
                self.processes.append(subprocess.Popen(["/bin/sh", "-c", script, str(hold), str(ack)],
                                                       stdin=subprocess.DEVNULL, process_group=group))
            self.leader, self.member = self.processes[0], (self.processes[1] if descendant else None)
            # One bounded wait for the exit of the leader, without a reap.
            watch = FIXTURE.ExitWatch(self.leader.pid)
            try:
                if not watch.wait(30):
                    raise AssertionError("the leader did not exit")
            finally:
                watch.close()
        except BaseException:
            self.close()
            raise

    def release(self):
        while self.ends:
            os.close(self.ends.pop())

    def close(self):
        self.release()
        for process in self.processes:
            if process.returncode is None:
                # The process is not reaped, so its ID still names it.
                process.kill()
                process.wait(timeout=30)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


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

    def receipt_manifest(self, runner_temp, phase):
        """A real ownership manifest of job 90001, attempt 1, in PHASE."""
        token = "3" * 20
        parent = runner_temp / (FIXTURE.PARENT_PREFIX + token)
        parent.mkdir(mode=0o700)
        path = runner_temp / "pmcli-bdd-90001-1.json"
        daemon = {"id": "engine", "os": "linux", "architecture": "x86_64", "server_version": "5.0.0",
                  "rootless": True, "endpoint": "unix:///run/podman/podman.sock",
                  "socket": {"device": 1, "inode": 2, "uid": os.getuid(), "mode": 0o140600}}
        manifest = FIXTURE.OwnershipManifest.create(
            path, runner_temp, ROOT, "90001", "1", token, parent, parent / "fixture",
            FIXTURE.PROJECT_PREFIX + token, daemon)
        if phase != "prepared":
            manifest.update(phase)
        return path

    def test_receipt_check_only_reads_the_manifest(self):
        # The receipt check makes no Docker call and changes no file. Only a
        # manifest in the phase "cleaned" passes.
        for phase, expected in (("cleaned", 0), ("cleanup_failed", 1), ("cleaning", 1), ("prepared", 1)):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as temporary:
                runner_temp = Path(temporary).resolve()
                path = self.receipt_manifest(runner_temp, phase)
                before = path.read_bytes()
                errors = io.StringIO()
                with contextlib.ExitStack() as stack:
                    for name in ("resolve_docker", "load_image_refs", "cleanup_fixture",
                                 "docker_json", "docker_lines"):
                        stack.enter_context(mock.patch.object(
                            FIXTURE, name, side_effect=AssertionError(f"{name} called")))
                    stack.enter_context(mock.patch.object(
                        FIXTURE.subprocess, "Popen", side_effect=AssertionError("a process started")))
                    stack.enter_context(contextlib.redirect_stderr(errors))
                    status = FIXTURE.receipt_mode(path, runner_temp, ROOT, "90001", "1")
                self.assertEqual(status, expected, errors.getvalue())
                self.assertEqual(path.read_bytes(), before)
                if expected:
                    self.assertIn(f"ownership manifest phase is {phase}, not cleaned", errors.getvalue())

    def test_receipt_check_fails_without_a_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            runner_temp = Path(temporary).resolve()
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                status = FIXTURE.receipt_mode(runner_temp / "pmcli-bdd-90001-1.json",
                                              runner_temp, ROOT, "90001", "1")
            self.assertEqual(status, 1)
            self.assertIn("ownership manifest is missing", errors.getvalue())

    def test_receipt_check_option(self):
        self.assertEqual(FIXTURE.parse_cli(["--check-receipt", "/tmp/m.json"]),
                         ("receipt", "/tmp/m.json", []))

    def test_manifest_name_binds_the_job_and_run_attempt(self):
        with tempfile.TemporaryDirectory() as temporary:
            runner_temp = Path(temporary).resolve()
            env = {"GITHUB_RUN_ID": "90001", "GITHUB_RUN_ATTEMPT": "2"}
            with mock.patch.dict(os.environ, env):
                for name in ("pmcli-foundation-90001-2.json", "pmcli-bdd-90001-2.json"):
                    with self.subTest(name=name):
                        path, run_id, attempt = FIXTURE.validate_manifest_path(
                            str(runner_temp / name), runner_temp)
                        self.assertEqual((path.name, run_id, attempt), (name, "90001", "2"))
                for name in ("pmcli-other-90001-2.json", "pmcli-bdd-90001.json",
                             "pmcli-bdd-90001-3.json", "pmcli-bdd-x-2.json"):
                    with self.subTest(name=name), self.assertRaises(FIXTURE.FixtureError):
                        FIXTURE.validate_manifest_path(str(runner_temp / name), runner_temp)

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
                "PM_STACK_IDENTITY_PROVIDER_HOST": "127.31.41.59",
                "PM_STACK_IDENTITY_PROVIDER_PORT": "12006",
                "PM_STACK_IDENTITY_PROVIDER_ISSUER": "https://127.31.41.59:12006/realms/pm-test",
                "PM_STACK_IDENTITY_PROVIDER_DIR": str(fixture_root / "identity-provider"),
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
        # The leader stops on SIGTERM. Its descendant ignores SIGTERM, holds
        # the output open and blocks on a held FIFO. The group SIGKILL after
        # the grace must stop it, before the leader's reap (issue #286).
        descendant = (
            "import signal,sys; "
            "signal.signal(signal.SIGTERM,signal.SIG_IGN); "
            "print('ready',flush=True); open(sys.argv[1]).read()"
        )
        leader = (
            "import subprocess,sys; "
            f"subprocess.Popen([sys.executable,'-c',{descendant!r},sys.argv[1]]); open(sys.argv[1]).read()"
        )
        with tempfile.TemporaryDirectory() as directory:
            hold = Path(directory) / "hold"
            os.mkfifo(hold)
            hold_end = os.open(hold, os.O_RDWR)
            process = subprocess.Popen(
                [sys.executable, "-I", "-B", "-c", leader, str(hold)],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
            )
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ)
                    self.assertTrue(selector.select(timeout=5), "descendant did not become ready")
                self.assertEqual(process.stdout.readline(), b"ready\n")
                with mock.patch.object(FIXTURE, "PROCESS_TERM_GRACE", 0.1):
                    FIXTURE.terminate_process_group(process)
                self.assertEqual(process.returncode, -signal.SIGTERM)
                try:
                    process.communicate(timeout=2)
                except subprocess.TimeoutExpired:
                    self.fail("cancellation left a live descendant holding the process output open")
            finally:
                if process.returncode is None:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except (ProcessLookupError, PermissionError):
                        pass
                process.communicate(timeout=5)
                os.close(hold_end)

    def test_group_signal_precedes_any_reap(self):
        # E6 at 8fc2bd3: a reap before the group signal lets the leader's ID
        # name another group. The leader exited and is not reaped, and a group
        # member blocks on a held FIFO. Each killpg must come before the reap.
        events = []
        real_killpg = os.killpg

        class RecordingPopen(subprocess.Popen):
            def poll(self):
                before = self.returncode
                status = super().poll()
                if before is None and status is not None:
                    events.append(("reaped", self.pid))
                return status

            def wait(self, timeout=None):
                before = self.returncode
                status = super().wait(timeout)
                if before is None:
                    events.append(("reaped", self.pid))
                return status

        def recording_killpg(group, signum):
            events.append(("killpg", group, signum))
            return real_killpg(group, signum)

        with tempfile.TemporaryDirectory() as directory:
            hold = Path(directory) / "hold"
            os.mkfifo(hold)
            hold_end = os.open(hold, os.O_RDWR)
            process = RecordingPopen(
                ["/bin/sh", "-c", 'cat < "$0" > /dev/null 2>&1 &', str(hold)],
                stdout=subprocess.PIPE, start_new_session=True)
            try:
                # One bounded wait: end of file on the leader's output means
                # that the leader has exited. Nothing has reaped it.
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ)
                    self.assertTrue(selector.select(timeout=30), "the leader did not exit")
                self.assertEqual(os.read(process.stdout.fileno(), 64), b"")
                self.assertIsNone(process.returncode)
                with mock.patch.object(FIXTURE.os, "killpg", recording_killpg):
                    FIXTURE.terminate_process_group(process)
            finally:
                if process.returncode is None:
                    process.kill()
                    process.wait()
                process.stdout.close()
                os.close(hold_end)
        reaped = [index for index, event in enumerate(events) if event[0] == "reaped"]
        kills = [index for index, event in enumerate(events) if event[0] == "killpg"]
        self.assertEqual(len(reaped), 1, events)
        self.assertIn(("killpg", process.pid, signal.SIGTERM), events)
        self.assertIn(("killpg", process.pid, signal.SIGKILL), events)
        self.assertEqual([index for index in kills if index > reaped[0]], [],
                         f"killpg after the leader was reaped: {events}")

    def test_group_signal_refusal_needs_an_empty_group(self):
        # brahmagupta F3 at 3b6e71f: only Darwin, with no live member, makes a
        # refused group signal harmless. killpg is refused in each case.
        def refused(group, signum):
            raise PermissionError(1, "Operation not permitted")

        cases = [
            ("zombie-only group on Darwin", False, "darwin", [], None),
            ("zombie-only group on Linux", False, "linux", None, "refused to send SIGKILL, SIGTERM"),
            ("live member, native platform", True, None, None, "refused to send SIGKILL, SIGTERM"),
            ("unreadable member list on Darwin", False, "darwin", "unknown", "refused to send SIGKILL, SIGTERM"),
        ]
        for name, descendant, system, members, message in cases:
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                with ExitedLeader(directory, descendant) as group:
                    process = group.leader
                    with contextlib.ExitStack() as stack:
                        stack.enter_context(mock.patch.object(FIXTURE.os, "killpg", refused))
                        if system:
                            stack.enter_context(mock.patch.object(FIXTURE.sys, "platform", system))
                        if members is not None:
                            stack.enter_context(mock.patch.object(
                                FIXTURE, "live_group_members", create=True,
                                return_value=None if members == "unknown" else members))
                        if message is None:
                            FIXTURE.terminate_process_group(process)
                        else:
                            with self.assertRaisesRegex(FIXTURE.FixtureError, message):
                                FIXTURE.terminate_process_group(process)
                    self.assertIsNotNone(process.returncode, "the exited leader was not reaped")
                    group.release()
                    if group.member is not None:
                        # One wait for the member, which reads the end of the
                        # closed FIFO and exits by itself.
                        self.assertEqual(group.member.wait(timeout=30), 0, "the group member did not exit")
                self.assertEqual(processes_holding(directory), [])

    def test_exited_leader_setup_failure_releases_everything(self):
        # brahmagupta F1 at 52a87ec: a failed setup left the FIFO ends and the
        # processes. The exit watch fails after both processes started.
        started = []

        class Recorded(subprocess.Popen):
            def __init__(self, *arguments, **options):
                super().__init__(*arguments, **options)
                started.append(self)

        def no_watch(pid):
            raise OSError("no exit watch")

        with tempfile.TemporaryDirectory() as directory:
            try:
                with mock.patch.object(subprocess, "Popen", Recorded), \
                        mock.patch.object(FIXTURE, "ExitWatch", no_watch), \
                        self.assertRaisesRegex(OSError, "no exit watch"):
                    ExitedLeader(directory, True)
                self.assertEqual(len(started), 2, started)
                self.assertEqual([process.returncode is None for process in started], [False, False],
                                 "an owned process was not stopped and collected")
                self.assertTrue(all(no_fifo_reader(Path(directory) / name) for name in ("hold", "ack")))
                self.assertEqual(processes_holding(directory), [])
            finally:
                for process in started:
                    if process.returncode is None:
                        process.kill()
                        process.wait(timeout=30)

    def test_group_signal_refuses_a_reaped_leader(self):
        # The process group ID of a reaped leader can name another group, so
        # no signal goes to it.
        process = subprocess.Popen([sys.executable, "-I", "-B", "-c", "pass"], start_new_session=True)
        process.wait(timeout=30)
        with mock.patch.object(FIXTURE.os, "killpg", side_effect=AssertionError("killpg called")):
            with self.assertRaisesRegex(FIXTURE.FixtureError, "reaped before its process group was stopped"):
                FIXTURE.terminate_process_group(process)


class PhaseFixture:
    """Fake processes on a fake monotonic clock, for the phase deadline checks.

    Each process ends after DURATION(arguments) seconds of fake time, or never
    when DURATION returns None. A wait with a smaller timeout moves the clock by
    the timeout and raises TimeoutExpired. No real process starts and no test
    waits: the clock moves only when a fake process is waited for.
    """

    DAEMON_INFO = {"id": "QJ4M:ABCD:EFGH", "os": "linux", "architecture": "x86_64",
                   "server_version": "5.0.0", "security_options": ["name=rootless"],
                   "product_license": "Community Engine", "root_dir": "/var/lib/docker"}
    SOCKET = {"device": 1, "inode": 2, "uid": os.getuid(), "mode": 0o140600}
    # L-a's job identity provider is part of every fixture start, so the
    # environment carries its values.
    ENVIRONMENT = {"PM_STACK_TEST_CONFIG": "/unused/server.json",
                   "PM_STACK_TEST_ROLLOVER_NAMESPACE": "unused-rollover",
                   "PM_STACK_IDENTITY_PROVIDER_HOST": "127.0.0.2",
                   "PM_STACK_IDENTITY_PROVIDER_PORT": "13006",
                   "PM_STACK_IDENTITY_PROVIDER_ISSUER": "https://127.0.0.2:13006/realms/pm-test",
                   "PM_STACK_IDENTITY_PROVIDER_DIR": "/unused/identity-provider"}
    RECORD = {"root_identity": {"device": 1, "inode": 3, "uid": os.getuid(), "mode": 0o700},
              "environment": ENVIRONMENT, "environment_sha256": "a" * 64, "config_sha256": "b" * 64,
              "rollover_namespace": "unused-rollover", "temporal_dynamic_config_sha256": "c" * 64}
    COMMAND = "requested-command"

    def __init__(self, duration):
        self.duration = duration
        self.now = 1000.0
        self.calls = []
        self.command_started = False
        self.stopped = []

    def clock(self):
        return self.now

    def popen(self, arguments, **_options):
        fixture = self
        arguments = list(arguments)
        if arguments == [self.COMMAND]:
            self.command_started = True

        class Process:
            pid = None
            returncode = None

            def finish(self, timeout):
                fixture.calls.append((arguments, timeout))
                seconds = fixture.duration(arguments)
                if seconds is None or (timeout is not None and seconds > timeout):
                    fixture.now += timeout
                    raise subprocess.TimeoutExpired(arguments, timeout)
                fixture.now += seconds
                self.returncode = 0

            def communicate(self, timeout=None):
                self.finish(timeout)
                info = json.dumps(fixture.DAEMON_INFO).encode()
                return (info if "info" in arguments else b""), b""

            def wait(self, timeout=None):
                self.finish(timeout)
                return 0

            def poll(self):
                return self.returncode

        return Process()

    def stop(self, process, *_arguments, **_options):
        self.stopped.append(process)
        process.returncode = -signal.SIGKILL

    def patches(self, stack, token):
        values = {
            "resolve_docker": Path("/unused/docker"),
            "validate_compose_cache": None,
            "load_image_refs": {},
            "choose_docker_socket": (Path("/unused/docker.sock"), self.SOCKET),
            "validate_generated_environment": (self.ENVIRONMENT, "a" * 64, "b" * 64, "c" * 64),
            "fixture_record": self.RECORD,
            "reserve_ports": [],
            "verify_running_resources": None,
            # test_phase_deadline_covers_the_identity_provider_checks drives
            # the identity check itself under the fake clock.
            "verify_identity_provider": None,
            "validate_fixture_record": self.ENVIRONMENT,
            "verify_immutable_fixture_files": None,
            "verify_daemon_identity": None,
        }
        for name, value in values.items():
            stack.enter_context(mock.patch.object(FIXTURE, name, return_value=value))
        stack.enter_context(mock.patch.object(FIXTURE, "regular_executable", side_effect=lambda path, _: path))
        stack.enter_context(mock.patch.object(FIXTURE.secrets, "token_hex", return_value=token))
        stack.enter_context(mock.patch.object(FIXTURE, "SIGNALS", FIXTURE.SignalController()))
        stack.enter_context(mock.patch.object(FIXTURE, "terminate_process_group", side_effect=self.stop))
        # The fake clock. create=True also patches a wrapper with no CLOCK.
        stack.enter_context(mock.patch.object(FIXTURE, "CLOCK", self.clock, create=True))
        stack.enter_context(mock.patch.object(FIXTURE.subprocess, "Popen", side_effect=self.popen))

    def run(self, runner_temp, token="6" * 20):
        """Run run_mode. Return its result, its stderr and the manifest path."""
        path = runner_temp / "pmcli-bdd-90001-1.json"
        errors = io.StringIO()
        with contextlib.ExitStack() as stack:
            self.patches(stack, token)
            stack.enter_context(contextlib.redirect_stderr(errors))
            result = FIXTURE.run_mode(path, runner_temp, ROOT, "90001", "1", [self.COMMAND], {})
        return result, errors.getvalue(), path

    def cleanup_step(self, runner_temp, token="7" * 20):
        """Run cleanup_mode on a live fixture manifest. Return its result, stderr and path."""
        parent = runner_temp / (FIXTURE.PARENT_PREFIX + token)
        parent.mkdir(mode=0o700)
        path = runner_temp / "pmcli-bdd-90001-1.json"
        daemon = dict(json.loads(json.dumps(self.DAEMON_INFO)), rootless=True,
                      endpoint="unix:///unused/docker.sock", socket=self.SOCKET)
        for name in ("security_options", "product_license", "root_dir"):
            daemon.pop(name)
        manifest = FIXTURE.OwnershipManifest.create(
            path, runner_temp, ROOT, "90001", "1", token, parent, parent / "fixture",
            FIXTURE.PROJECT_PREFIX + token, daemon)
        manifest.update("generated", fixture=self.RECORD)
        manifest.update("starting", fixture=self.RECORD, authorize_docker=True)
        manifest.update("running")
        errors = io.StringIO()
        with contextlib.ExitStack() as stack:
            self.patches(stack, token)
            stack.enter_context(contextlib.redirect_stderr(errors))
            result = FIXTURE.cleanup_mode(path, runner_temp, ROOT, "90001", "1", {})
        return result, errors.getvalue(), path


class PhaseDeadlineTests(unittest.TestCase):
    """Issue #286: one deadline for each whole fixture phase (design v3, 4.5)."""

    START = "the fixture start phase reached its deadline (720 s)"

    @staticmethod
    def manifest(path):
        return json.loads(path.read_text())

    def test_phase_deadline_ends_the_start_phase(self):
        # Each call ends after 25 s (own limit 30 s); together they pass 720 s.
        fixture = PhaseFixture(lambda _arguments: 25)
        with tempfile.TemporaryDirectory() as temporary:
            result, errors, path = fixture.run(Path(temporary).resolve())
            self.assertEqual(result, 1, errors)
            self.assertIn(self.START, errors)
            self.assertNotIn("reached its own limit", errors)
            self.assertFalse(fixture.command_started, "a command started on the fixture")
            self.assertTrue(path.exists(), "the manifest did not stay")
            self.assertNotEqual(self.manifest(path)["phase"], "running")

    def test_phase_deadline_reduces_the_call_timeouts(self):
        fixture = PhaseFixture(lambda _arguments: 25)
        with tempfile.TemporaryDirectory() as temporary:
            fixture.run(Path(temporary).resolve())
        start = fixture.calls[:fixture.calls.index(next(call for call in fixture.calls
                                                          if call[1] != 30 and call[1] != 60
                                                          and "up" not in call[0])) + 1]
        timeouts = [timeout for _, timeout in start]
        # docker info 30, the generator 60, 15 discovery calls 30 (L-a adds the
        # keycloak container and the identity-private network), `pm-stack up`
        # with 720 - 17 * 25 = 295 s left, then discovery calls with 30 s until
        # 20 s are left. Each value is min(its own limit, the time left).
        self.assertEqual(timeouts[:2], [30, 60])
        self.assertEqual(timeouts[2:17], [30] * 15)
        self.assertEqual(start[17][0][-3:-2], ["up"])
        self.assertEqual(timeouts[17], 295)
        self.assertEqual(timeouts[18:], [30] * 10 + [20])
        # The start deadline stops the 20 s call; the cleanup deadline one more.
        self.assertEqual(len(fixture.stopped), 2)

    def test_phase_deadline_ends_the_cleanup_in_the_run_step(self):
        # Each cleanup call ends after 25 s; together they pass the 360 s.
        fixture = PhaseFixture(lambda _arguments: 25 if fixture.command_started else 1)
        with tempfile.TemporaryDirectory() as temporary:
            result, errors, path = fixture.run(Path(temporary).resolve())
            self.assertEqual(result, 1, errors)
            self.assertTrue(fixture.command_started)
            self.assertIn("the fixture cleanup phase reached its deadline (360 s)", errors)
            self.assertNotIn("reached its own limit", errors)
            document = self.manifest(path)
            self.assertEqual(document["phase"], "cleanup_failed")
            self.assertEqual(document["diagnostic"]["code"], "cleanup-phase-deadline")
        cleanup = fixture.calls[fixture.calls.index(([PhaseFixture.COMMAND], FIXTURE.CHILD_TIMEOUT)) + 1:]
        self.assertEqual([timeout for _, timeout in cleanup], [30] * 14 + [10])

    def test_phase_deadline_ends_the_cleanup_step(self):
        # --cleanup: 300 s. The 12th call gets the last 25 s; no 13th call starts.
        fixture = PhaseFixture(lambda _arguments: 25)
        with tempfile.TemporaryDirectory() as temporary:
            result, errors, path = fixture.cleanup_step(Path(temporary).resolve())
            self.assertEqual(result, 1, errors)
            self.assertIn("the fixture cleanup phase reached its deadline (300 s)", errors)
            document = self.manifest(path)
            self.assertEqual(document["phase"], "cleanup_failed")
            self.assertEqual(document["diagnostic"]["code"], "cleanup-phase-deadline")
        self.assertEqual([timeout for _, timeout in fixture.calls], [30] * 11 + [25])

    def test_phase_deadline_differs_from_a_slow_call(self):
        # A call at its own 30 s limit gets the per-call message, in both phases.
        def first_never_ends(arguments):
            return None if len(fixture.calls) == 1 else 1

        fixture = PhaseFixture(first_never_ends)
        with tempfile.TemporaryDirectory() as temporary:
            result, errors, _ = fixture.run(Path(temporary).resolve())
        self.assertEqual(result, 1, errors)
        self.assertIn("inspect Docker engine timed out: the call reached its own limit of 30 s", errors)
        self.assertNotIn("phase reached its deadline", errors)
        self.assertFalse(fixture.command_started)
        fixture = PhaseFixture(first_never_ends)
        with tempfile.TemporaryDirectory() as temporary:
            result, errors, path = fixture.cleanup_step(Path(temporary).resolve())
            self.assertEqual(result, 1, errors)
            self.assertIn("list fixture containers timed out: the call reached its own limit of 30 s", errors)
            self.assertNotIn("phase reached its deadline", errors)
            self.assertEqual(self.manifest(path)["phase"], "cleanup_failed")

    def test_phase_deadline_covers_the_identity_provider_checks(self):
        # #370's rule on L-a's start path: each engine call of the identity
        # check and each identity probe gets min(its own limit, the time left),
        # a phase with no time left starts no call, and a probe that the phase
        # deadline stopped raises PhaseDeadline, not FixtureError.
        host, port = "127.0.0.2", 13006
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve() / "fixture"
            directory = root / FIXTURE.IDENTITY_DIRECTORY
            directory.mkdir(parents=True)
            (directory / "admin-client-id").write_text("pm-test-admin")
            (directory / "admin-client-secret").write_text("a" * 64)
            environment = {name: str(14000 + index) for index, name in enumerate(FIXTURE.PORT_KEYS)}
            environment.update({
                "PM_STACK_IDENTITY_PROVIDER_HOST": host,
                "PM_STACK_IDENTITY_PROVIDER_PORT": str(port),
                "PM_STACK_IDENTITY_PROVIDER_ISSUER": f"https://{host}:{port}/realms/{FIXTURE.IDENTITY_REALM}",
                "PM_STACK_IDENTITY_PROVIDER_DIR": str(directory),
            })
            resources = {"containers": [{"id": "f" * 64, "service": FIXTURE.IDENTITY_SERVICE}]}
            context = ssl.create_default_context()

            def in_phase(left, action, socket_error):
                """Run action in the start phase with LEFT seconds left. Return its result and the timeouts."""
                fixture = PhaseFixture(lambda _arguments: 1)
                sockets = []

                def connect(_address, timeout=None, *_args, **_kwargs):
                    sockets.append(timeout)
                    raise socket_error

                with contextlib.ExitStack() as stack:
                    stack.enter_context(mock.patch.object(FIXTURE, "CLOCK", fixture.clock, create=True))
                    stack.enter_context(mock.patch.object(FIXTURE.subprocess, "Popen", side_effect=fixture.popen))
                    stack.enter_context(mock.patch.object(FIXTURE, "terminate_process_group", side_effect=fixture.stop))
                    stack.enter_context(mock.patch.object(FIXTURE.socket, "create_connection", side_effect=connect))
                    with FIXTURE.Phase("start", FIXTURE.START_PHASE_LIMIT) as phase:
                        fixture.now = phase.end - left
                        try:
                            outcome = action()
                        except (FIXTURE.FixtureError, FIXTURE.PhaseDeadline) as exc:
                            outcome = exc
                return outcome, [timeout for _, timeout in fixture.calls], sockets

            check = lambda: FIXTURE.verify_identity_provider(  # noqa: E731
                environment, resources, root, Path("/unused/pm-stack"), {}, True, Path("/unused/docker"),
                Path("/unused/docker-config"), "unix:///unused/docker.sock", {}, ROOT)
            probes = {
                "plain HTTP": lambda: FIXTURE.plain_http_refusal(host, port, FIXTURE.IDENTITY_REALM),
                "HTTPS request": lambda: FIXTURE.identity_request(host, port, context, "GET", "/"),
                "TLS name": lambda: FIXTURE.identity_tls_name_probe(host, port, context),
                "first loopback": lambda: FIXTURE.identity_first_loopback_probe(port),
            }

            # (i) The two engine calls: min(30, left). Each call takes 1 s.
            outcome, engine, sockets = in_phase(15, check, ConnectionRefusedError())
            self.assertEqual(engine, [15, 14])
            self.assertIsInstance(outcome, FIXTURE.FixtureError)  # the fake log has no ready line
            self.assertEqual(sockets, [])
            # (i) Each probe: min(10, left).
            for left, want in ((15, 10), (7, 7)):
                for name, probe in probes.items():
                    with self.subTest(probe=name, left=left):
                        _, _, sockets = in_phase(left, probe, ConnectionRefusedError())
                        self.assertEqual(sockets, [want])
            # (ii) No time left: the first engine call raises PhaseDeadline, and no probe opens a socket.
            outcome, engine, sockets = in_phase(0, check, ConnectionRefusedError())
            self.assertIsInstance(outcome, FIXTURE.PhaseDeadline)
            self.assertEqual((engine, sockets), ([], []))
            # (iii) A probe timeout that the phase set raises PhaseDeadline.
            for name, probe in probes.items():
                with self.subTest(probe=name, timeout="set by the phase"):
                    outcome, _, sockets = in_phase(7, probe, socket.timeout())
                    self.assertEqual(sockets, [7])
                    self.assertIsInstance(outcome, FIXTURE.PhaseDeadline)
                with self.subTest(probe=name, timeout="the probe's own limit"):
                    outcome, _, _ = in_phase(15, probe, socket.timeout())
                    self.assertNotIsInstance(outcome, FIXTURE.PhaseDeadline)

    def test_phase_deadline_limits_match_the_bdd_budget(self):
        # check-bdd.sh adds the post-SIGKILL wait (PROCESS_TERM_GRACE, 10 s), outside the phase deadline.
        script = (ROOT / "scripts/ci/check-bdd.sh").read_text()
        grace = FIXTURE.PROCESS_TERM_GRACE
        self.assertIn(f"\nstart_allowance={FIXTURE.START_PHASE_LIMIT + grace}\n", script)
        self.assertIn(f"\ncleanup_allowance={FIXTURE.CLEANUP_PHASE_LIMIT + grace}\n", script)
        self.assertEqual(FIXTURE.CLEANUP_STEP_PHASE_LIMIT, 300)


class IdentityProviderTests(unittest.TestCase):
    """The job Keycloak: its environment, binding, hardening, networks and probes."""

    # Synthetic test vectors. HOST stands for the per-job second loopback
    # address; 192.0.2.10 is from the RFC 5737 documentation range.
    PROJECT = "pmci-" + "6" * 20
    HOST = "127.31.41.59"
    ROOT = Path("/private/fixture")
    PODMAN_ROOT = "/home/ghrunner/.local/share/containers/storage"
    ENV = {
        "PM_STACK_POSTGRES_PORT": "12001",
        "PM_STACK_TEMPORAL_PORT": "12002",
        "PM_STACK_S3_PRIMARY_PORT": "12003",
        "PM_STACK_S3_SECONDARY_PORT": "12004",
        "PM_STACK_PUBLIC_PORT": "12005",
        "PM_STACK_IDENTITY_PROVIDER_HOST": HOST,
        "PM_STACK_IDENTITY_PROVIDER_PORT": "12006",
        "PM_STACK_IDENTITY_PROVIDER_ISSUER": f"https://{HOST}:12006/realms/pm-test",
        "PM_STACK_IDENTITY_PROVIDER_DIR": "/private/fixture/identity-provider",
    }
    PORTS = ENV

    def test_identity_environment_refuses_wildcard_non_loopback_and_first_loopback(self):
        self.assertEqual(FIXTURE.identity_endpoint(self.ENV, self.ROOT), (self.HOST, 12006))
        host = "PM_STACK_IDENTITY_PROVIDER_HOST"
        for name, value, refusal in (
            (host, "0.0.0.0", "wildcard"),
            (host, "::", "wildcard"),
            (host, "192.0.2.10", "non-loopback"),
            (host, "127.0.0.1", "first loopback"),
            (host, "localhost", "IPv4"),
            (host, "::1", "IPv4"),
            (host, "127.0.0.02", "IPv4"),
            ("PM_STACK_IDENTITY_PROVIDER_PORT", "12005", "port"),
            ("PM_STACK_IDENTITY_PROVIDER_PORT", "80", "port"),
            ("PM_STACK_IDENTITY_PROVIDER_PORT", "012006", "port"),
            ("PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://localhost:12006/realms/pm-test", "issuer"),
            ("PM_STACK_IDENTITY_PROVIDER_ISSUER", f"http://{self.HOST}:12006/realms/pm-test", "issuer"),
            ("PM_STACK_IDENTITY_PROVIDER_ISSUER", f"https://{self.HOST}:12006/realms/master", "issuer"),
            ("PM_STACK_IDENTITY_PROVIDER_DIR", "/private/fixture/other", "identity provider"),
        ):
            with self.subTest(name=name, value=value):
                with self.assertRaisesRegex(FIXTURE.FixtureError, refusal):
                    FIXTURE.identity_endpoint(dict(self.ENV, **{name: value}), self.ROOT)

    def keycloak(self, podman=True, rootless=True):
        refs = FIXTURE.load_image_refs(ROOT)
        nano_cpus, memory, pids_limit, readonly, restart = FIXTURE.service_limits("keycloak")
        network = f"{self.PROJECT}_identity-private"
        mounts = [
            {"Type": kind, "Source": source, "Destination": destination, "RW": read_write}
            for source, destination, read_write, kind in FIXTURE.expected_mounts("keycloak", self.ROOT, ROOT)
        ]
        if podman:
            tmpfs = {
                "/tmp": "size=64m,mode=1777,tmpcopyup,rw,rprivate,nosuid,nodev,noexec",
                "/opt/keycloak/data": "size=128m,mode=1777,tmpcopyup,rw,rprivate,nosuid,nodev,noexec",
                "/opt/keycloak/lib/quarkus": "size=64m,mode=0770,tmpcopyup,rw,rprivate,nosuid,nodev,noexec",
            }
        else:
            tmpfs = {"/tmp": "mode=1777,size=64m", "/opt/keycloak/data": "mode=1777,size=128m",
                     "/opt/keycloak/lib/quarkus": "mode=0770,size=64m,tmpcopyup"}
        value = {
            "id": "f" * 64, "name": f"/{self.PROJECT}-keycloak-1", "service": "keycloak",
            "project": self.PROJECT, "oneoff": "False",
            "config_files": ",".join(sorted(FIXTURE.expected_config_files(ROOT))),
            "working_dir": str(ROOT), "image": refs["keycloak"], "image_id": "sha256:" + "b" * 64,
            "user": "1000:0" if rootless else f"{os.getuid()}:{os.getgid()}", "privileged": False,
            "network_mode": "bridge" if podman else network, "networks": {network: {}},
            "pid_mode": "private" if podman else "", "ipc_mode": "shareable" if podman else "private",
            "uts_mode": "private" if podman else "", "userns_mode": "", "readonly_rootfs": readonly,
            "auto_remove": False, "init": True, "nano_cpus": nano_cpus, "memory": memory,
            "pids_limit": pids_limit, "restart": restart, "cap_add": [],
            "cap_drop": sorted(FIXTURE.PODMAN_DEFAULT_CAPABILITIES) if podman else ["ALL"],
            "security_opt": ["no-new-privileges"] if podman else ["no-new-privileges:true"],
            "devices": [], "device_requests": None, "mounts": mounts, "tmpfs": tmpfs,
            "port_bindings": {"8443/tcp": [{"HostIp": self.HOST, "HostPort": "12006"}]},
        }
        return value, refs

    def validate(self, value, refs, podman=True, rootless=True):
        return FIXTURE.validate_stack_container(
            value, self.PROJECT, self.ROOT, ROOT, self.ENV, refs,
            rootless=rootless, engine_root=self.PODMAN_ROOT if podman else None,
        )

    def test_keycloak_binds_only_the_second_loopback_address(self):
        for podman in (True, False):
            value, refs = self.keycloak(podman=podman)
            self.assertEqual(self.validate(value, refs, podman=podman)["service"], "keycloak")
        value, refs = self.keycloak()
        for host_ip in ("127.0.0.1", "0.0.0.0", "", "::", "192.0.2.10"):
            with self.subTest(host_ip=host_ip), self.assertRaises(FIXTURE.FixtureError):
                self.validate(dict(value, port_bindings={"8443/tcp": [{"HostIp": host_ip, "HostPort": "12006"}]}), refs)
        extra = {"8443/tcp": [{"HostIp": self.HOST, "HostPort": "12006"}],
                 "8080/tcp": [{"HostIp": self.HOST, "HostPort": "12007"}]}
        with self.assertRaises(FIXTURE.FixtureError):
            self.validate(dict(value, port_bindings=extra), refs)
        caddy, project, _ = FixtureOwnershipTests.stack_container(self, "caddy", self.ROOT, podman=True)
        caddy = dict(caddy, project=self.PROJECT, name=f"/{self.PROJECT}-caddy-1",
                     networks={f"{self.PROJECT}_stack-private": {}},
                     port_bindings={"9443/tcp": [{"HostIp": self.HOST, "HostPort": "12005"}]})
        with self.assertRaises(FIXTURE.FixtureError):
            self.validate(caddy, refs)

    def test_keycloak_container_must_stay_hardened(self):
        value, refs = self.keycloak()
        tmpfs = dict(value["tmpfs"])
        for field, unsafe in (
            ("user", "0:0"),
            ("user", ""),
            ("cap_drop", []),
            ("cap_drop", sorted(FIXTURE.PODMAN_DEFAULT_CAPABILITIES - {"NET_BIND_SERVICE"})),
            ("cap_add", ["NET_ADMIN"]),
            ("security_opt", []),
            ("security_opt", ["no-new-privileges", "seccomp=unconfined"]),
            ("readonly_rootfs", False),
            ("tmpfs", {"/tmp": tmpfs["/tmp"]}),
            ("tmpfs", dict(tmpfs, **{"/opt/keycloak/data": "size=128m,mode=1777,exec"})),
            ("tmpfs", {name: options for name, options in tmpfs.items() if name != "/opt/keycloak/lib/quarkus"}),
            ("tmpfs", dict(tmpfs, **{"/opt/keycloak/lib/quarkus": "size=64m,mode=0770,notmpcopyup"})),
            ("memory", 4 << 30),
            ("pids_limit", 4096),
            ("restart", "unless-stopped"),
            ("network_mode", "host"),
            ("networks", {f"{self.PROJECT}_stack-private": {}}),
            ("networks", {f"{self.PROJECT}_identity-private": {}, f"{self.PROJECT}_stack-private": {}}),
        ):
            with self.subTest(field=field, unsafe=unsafe), self.assertRaises(FIXTURE.FixtureError):
                self.validate(dict(value, **{field: unsafe}), refs)
        docker, refs = self.keycloak(podman=False)
        with self.assertRaises(FIXTURE.FixtureError):
            self.validate(dict(docker, cap_drop=sorted(FIXTURE.PODMAN_DEFAULT_CAPABILITIES)), refs, podman=False)

    def test_keycloak_realm_import_is_a_directory_mount(self):
        # Review #316 F2: the provider mounts the import directory, not the
        # import file. A file bind mount keeps a removed file visible in the
        # container.
        imports = self.ROOT / FIXTURE.IDENTITY_DIRECTORY / "import"
        mounts = FIXTURE.expected_mounts("keycloak", self.ROOT, ROOT)
        self.assertIn((str(imports), "/opt/keycloak/data/import", False, "bind"), mounts)
        self.assertEqual([item for item in mounts if item[1].startswith("/opt/keycloak/data/import/")], [])
        value, refs = self.keycloak(podman=False)
        file_mount = [
            dict(item, Source=str(imports / "realm.json"), Destination="/opt/keycloak/data/import/realm.json")
            if item["Destination"] == "/opt/keycloak/data/import" else item
            for item in value["mounts"]
        ]
        with self.assertRaises(FIXTURE.FixtureError):
            self.validate(dict(value, mounts=file_mount), refs, podman=False)

    def test_realm_import_removal_checks_both_views(self):
        # Review #316 F2: after the ready line the job removes the realm
        # import, then needs an empty import directory on the job host and in
        # the running provider.
        def remove(imports, container_count, extra=None):
            imports.mkdir(mode=0o750)
            (imports / "realm.json").write_text("{}\n")
            if extra is not None:
                (imports / extra).write_text("{}\n")
            calls = []

            def run_capture(arguments, **options):
                calls.append((list(arguments), options))
                if isinstance(container_count, Exception):
                    raise container_count
                return 0, container_count, b""

            with mock.patch.object(FIXTURE, "run_capture", side_effect=run_capture):
                try:
                    FIXTURE.remove_realm_import(
                        imports, "f" * 64, Path("/unused/docker"), Path("/unused/docker-config"),
                        "unix:///unused/docker.sock", {}, ROOT)
                    outcome = None
                except FIXTURE.FixtureError as exc:
                    outcome = exc
            return outcome, calls

        with tempfile.TemporaryDirectory() as temporary:
            outcome, calls = remove(Path(temporary) / "empty", b"0\n")
            self.assertIsNone(outcome)
            self.assertEqual(os.listdir(Path(temporary) / "empty"), [])
            self.assertEqual(len(calls), 1)
            arguments, options = calls[0]
            self.assertEqual(arguments[-8:], ["container", "exec", "f" * 64, "/bin/bash", "-c",
                                              FIXTURE.IDENTITY_IMPORT_COUNT, "bash", "/opt/keycloak/data/import"])
            self.assertEqual((options["timeout"], options["max_output"]), (FIXTURE.DOCKER_TIMEOUT, 64))

            outcome, calls = remove(Path(temporary) / "host", b"0\n", extra="second.json")
            self.assertEqual(str(outcome), "identity provider realm import remains on the job host")
            self.assertEqual(calls, [])

            outcome, _ = remove(Path(temporary) / "container", b"1\n")
            self.assertEqual(str(outcome), "identity provider realm import remains in the container")

            outcome, _ = remove(Path(temporary) / "unreadable", b"")
            self.assertEqual(str(outcome), "identity provider realm import remains in the container")

            outcome, _ = remove(Path(temporary) / "failed", FIXTURE.CommandFailure("list the import", 126))
            self.assertIsInstance(outcome, FIXTURE.CommandFailure)

    def test_realm_import_count_counts_every_entry(self):
        # Review #316 F2: the container check runs this text with bash
        # builtins only. A real bash runs it here against a local directory.
        def count(directory):
            return subprocess.run(["/bin/bash", "-c", FIXTURE.IDENTITY_IMPORT_COUNT, "bash", str(directory)],
                                  capture_output=True, timeout=30, check=False)

        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / "import"
            directory.mkdir()
            self.assertEqual(count(directory).stdout, b"0\n")
            (directory / "realm.json").write_text("{}\n")
            self.assertEqual(count(directory).stdout, b"1\n")
            (directory / ".hidden").write_text("x")
            self.assertEqual(count(directory).stdout, b"2\n")
            self.assertNotEqual(count(Path(temporary) / "absent").returncode, 0)
            self.assertEqual(count(Path(temporary) / "absent").stdout, b"")

    def test_keycloak_no_new_privileges_must_be_enabled(self):
        # Review #316 F1: only an enabled no-new-privileges record passes. A
        # false value, another suffix or a conflicting entry is refused.
        for podman in (True, False):
            value, refs = self.keycloak(podman=podman)
            for accepted in (["no-new-privileges"], ["no-new-privileges:true"], ["no-new-privileges=true"]):
                with self.subTest(podman=podman, accepted=accepted):
                    self.validate(dict(value, security_opt=accepted), refs, podman=podman)
            for unsafe in (
                ["no-new-privileges:false"],
                ["no-new-privileges=false"],
                ["no-new-privileges:true", "no-new-privileges:false"],
                ["no-new-privileges=true", "no-new-privileges=false"],
                ["no-new-privileges:yes"],
                ["no-new-privileges-disabled"],
                ["no-new-privileges", None],
            ):
                with self.subTest(podman=podman, unsafe=unsafe), self.assertRaises(FIXTURE.FixtureError):
                    self.validate(dict(value, security_opt=unsafe), refs, podman=podman)

    def test_keycloak_build_output_must_name_copy_up(self):
        # Review #316 F6: a build output tmpfs record without tmpcopyup is
        # refused on both engines, and so is an explicit contrary option.
        for podman, missing in (
            (True, "size=64m,mode=0770,rw,rprivate,nosuid,nodev,noexec"),
            (False, "mode=0770,size=64m"),
        ):
            value, refs = self.keycloak(podman=podman)
            self.validate(value, refs, podman=podman)
            for options in (missing, missing + ",notmpcopyup"):
                tmpfs = dict(value["tmpfs"], **{"/opt/keycloak/lib/quarkus": options})
                with self.subTest(podman=podman, options=options), self.assertRaises(FIXTURE.FixtureError):
                    self.validate(dict(value, tmpfs=tmpfs), refs, podman=podman)

    def test_rootful_keycloak_runs_as_the_job_account(self):
        value, refs = self.keycloak(podman=False, rootless=False)
        self.validate(value, refs, podman=False, rootless=False)
        with self.assertRaises(FIXTURE.FixtureError):
            self.validate(dict(value, user="1000:0"), refs, podman=False, rootless=False)

    def test_private_tree_admits_group_read_only_for_identity_mounts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "fixture"
            identity = root / "identity-provider"
            imports = identity / "import"
            imports.mkdir(parents=True)
            os.chmod(root, 0o700)
            os.chmod(identity, 0o700)
            os.chmod(imports, 0o750)
            modes = {"ca.pem": 0o600, "server.pem": 0o640, "server-key.pem": 0o640, "import/realm.json": 0o640,
                     "admin-client-id": 0o600, "admin-client-secret": 0o600}
            for name, mode in modes.items():
                (identity / name).write_text("synthetic\n")
                os.chmod(identity / name, mode)
            (root / "server.json").write_text("{}\n")
            os.chmod(root / "server.json", 0o600)
            FIXTURE.validate_private_tree(root)
            FIXTURE.validate_identity_files(root)
            for path, mode in ((imports / "realm.json", 0o644), (identity / "admin-client-secret", 0o640),
                               (root / "server.json", 0o640), (imports, 0o755), (imports, 0o770),
                               (imports, 0o700), (identity, 0o750)):
                with self.subTest(path=path.name, mode=oct(mode)):
                    original = stat_mode(path)
                    os.chmod(path, mode)
                    try:
                        with self.assertRaises(FIXTURE.FixtureError):
                            FIXTURE.validate_private_tree(root)
                            FIXTURE.validate_identity_files(root)
                    finally:
                        os.chmod(path, original)
            # Review #316 F2: the realm import only in its own directory, and
            # nothing else in that directory.
            for extra in (identity / "realm.json", imports / "admin-client-secret", imports / "extra.json"):
                with self.subTest(extra=str(extra.relative_to(identity))):
                    extra.write_text("x")
                    os.chmod(extra, 0o600)
                    try:
                        with self.assertRaises(FIXTURE.FixtureError):
                            FIXTURE.validate_identity_files(root)
                    finally:
                        extra.unlink()
            FIXTURE.validate_identity_files(root)
            (identity / "extra").write_text("x")
            os.chmod(identity / "extra", 0o600)
            with self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.validate_identity_files(root)

    READY = (
        b"2026-11-16 12:00:00,000 INFO  [io.quarkus] (main) Profile prod activated.\n"
        b"2026-11-16 12:00:00,001 INFO  [io.quarkus] (main) Keycloak 26.8.1 on JVM (powered by Quarkus 3.27.1) "
        b"started in 23.456s. Listening on: https://0.0.0.0:8443\n"
    )

    def test_keycloak_ready_line_requires_production_https(self):
        self.assertEqual(FIXTURE.keycloak_start_summary(self.READY), "23.456")
        for unsafe in (
            self.READY.replace(b"Profile prod", b"Profile dev"),
            self.READY.replace(b"https://0.0.0.0:8443", b"http://0.0.0.0:8080 and https://0.0.0.0:8443"),
            self.READY.replace(b"8443\n", b"8443. Management interface listening on https://0.0.0.0:9000.\n"),
            self.READY.replace(b"26.8.1", b"26.8.0"),
            self.READY + b"Running the server in development mode. DO NOT use this configuration in production.\n",
            b"",
        ):
            with self.subTest(log=unsafe[-90:]), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.keycloak_start_summary(unsafe)

    def test_failed_start_names_only_fixed_codes(self):
        noise = b" Container pmci-x-keycloak-1  Started\npm-stack: job identity provider readiness: /private/a\n"
        for code, step in (("identity-provider-ready/connection-refused", None), ("stack-ready/status-503", None),
                           ("identity-provider-start", None), ("render/" + self.HOST, "unknown"), ("a" * 64, "unknown")):
            with self.subTest(code=code[-40:]):
                output = noise + b"pm-stack: failed step " + code.encode() + b"\n"
                self.assertEqual(FIXTURE.stack_failed_step(output), step or code)
        self.assertEqual(FIXTURE.stack_failed_step(noise), "unknown")
        refusal = b"The '--optimized' flag was used for first ever server start. Please don't use this flag"
        self.assertTrue(FIXTURE.PROVIDER_LOG_MARKERS["build-option"].search(refusal))
        self.assertIsNone(FIXTURE.PROVIDER_LOG_MARKERS["build-option"].search(b"just add --optimized to the command"))
        log = self.READY.replace(b"Profile prod", b"ERROR [org.keycloak.quarkus.runtime.cli.ExecutionExceptionHandler] "
                                 b"(main) ERROR: /opt/keycloak/lib/quarkus: Read-only file system\nProfile prod")
        state = {"status": "exited", "exit": 1, "oom": False}
        with mock.patch.object(FIXTURE, "list_container_ids", return_value=["c" * 64]), \
                mock.patch.object(FIXTURE, "docker_json", return_value=state), \
                mock.patch.object(FIXTURE, "run_capture", return_value=(0, log, b"")):
            described = FIXTURE.identity_provider_state(self.PROJECT, Path("/unused"), Path("/unused"), "", {}, ROOT)
        self.assertEqual(described, "provider=exited provider-exit=1 provider-oom=no "
                                    "provider-log=ready-line,start-error,read-only-file-system")
        for ids, state in (([], state), (["c" * 64] * 2, state), (["c" * 64], {"status": ["exited"], "exit": 1})):
            with self.subTest(ids=len(ids), state=state), \
                    mock.patch.object(FIXTURE, "list_container_ids", return_value=ids), \
                    mock.patch.object(FIXTURE, "docker_json", return_value=state):
                described = FIXTURE.identity_provider_state(self.PROJECT, Path("/unused"), Path("/unused"), "", {}, ROOT)
                self.assertEqual(described, "provider=absent" if not ids else "provider=unknown")

    def test_secret_scan_finds_raw_and_encoded_forms(self):
        secret = FIXTURE.secrets.token_hex(32)
        FIXTURE.require_secret_absent(b'{"Cmd":["start","--optimized"]}', secret, "container record")
        raw = secret.encode()
        for form in (raw, base64.b64encode(raw), base64.urlsafe_b64encode(raw).rstrip(b"="),
                     base64.b64encode(b"pm-test-admin:" + raw)):
            with self.subTest(form=form[:12]), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.require_secret_absent(b"prefix " + form + b" suffix", secret, "provider log", "pm-test-admin")

    def test_secret_scan_report_names_its_forms_and_snapshots(self):
        # Review #316 F8: the success line names the snapshots and the forms
        # that the scan checked, and claims no more. The scan refuses each
        # form that the line names.
        secret = FIXTURE.secrets.token_hex(32)
        report = FIXTURE.secret_scan_report("pm-test-admin")
        forms = FIXTURE.secret_forms(secret, "pm-test-admin")
        self.assertEqual(len(forms), 7)
        for name, form in forms.items():
            with self.subTest(form=name):
                self.assertIn(name, report)
                with self.assertRaises(FIXTURE.FixtureError):
                    FIXTURE.require_secret_absent(b"prefix " + form + b" suffix", secret, "provider log", "pm-test-admin")
        self.assertIn("both read before the HTTPS probes and the admin token request", report)
        self.assertIn("is not detected", report)
        self.assertNotIn("no form", report)

    def test_admin_token_claims_must_be_a_json_object(self):
        # Review #316 F5: a JWT payload of valid JSON that is not an object
        # gives one fixed FixtureError, not an AttributeError.
        issuer = "https://127.0.0.2:13006/realms/pm-test"

        def token(payload):
            encoded = base64.urlsafe_b64encode(json.dumps(payload).encode()).rstrip(b"=").decode()
            return "e30." + encoded + ".c2ln"

        def roles(payload):
            answer = (200, {"access_token": token(payload)})
            with mock.patch.object(FIXTURE, "identity_request", return_value=answer):
                return FIXTURE.admin_client_roles(
                    "127.0.0.2", 13006, issuer, ssl.create_default_context(), "pm-test-admin", "a" * 64)

        for payload in ([], [1, 2], None, "claims", 5, True):
            with self.subTest(payload=payload), self.assertRaisesRegex(
                FIXTURE.FixtureError, "^identity provider admin token claims are not a JSON object$"
            ):
                roles(payload)
        valid = {"iss": issuer, "azp": "pm-test-admin",
                 "resource_access": {"realm-management": {"roles": ["manage-users", "manage-clients"]}}}
        self.assertEqual(roles(valid), ["manage-clients", "manage-users"])

    def test_owned_networks_keep_the_identity_provider_alone(self):
        containers = [{"id": "1" * 64, "service": "postgres"}, {"id": "2" * 64, "service": "keycloak"}]

        def network(name, attached):
            return {"id": "9" * 64, "name": f"{self.PROJECT}_{name}", "driver": "bridge", "scope": "local",
                    "internal": False, "attachable": False, "ingress": False, "project": self.PROJECT,
                    "network": name, "owner": "true", "containers": {item: {} for item in attached}}

        record = FIXTURE.validate_owned_network(network("identity-private", ["2" * 64]), self.PROJECT, containers)
        self.assertEqual(record["network"], "identity-private")
        FIXTURE.validate_owned_network(network("stack-private", ["1" * 64]), self.PROJECT, containers)
        for name, attached in (("identity-private", ["1" * 64]), ("stack-private", ["2" * 64]),
                               ("other-private", []), ("identity-private", ["3" * 64])):
            with self.subTest(name=name, attached=attached), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.validate_owned_network(network(name, attached), self.PROJECT, containers)

    def test_running_fixture_needs_both_owned_networks(self):
        containers = [{"id": f"{index:064x}", "service": service, "state": "running", "oneoff": False}
                      for index, service in enumerate(sorted(FIXTURE.PERSISTENT_SERVICES), start=1)]
        provider = next(item["id"] for item in containers if item["service"] == "keycloak")
        stack = sorted(item["id"] for item in containers if item["service"] != "keycloak")
        networks = [{"id": "a" * 64, "name": "x", "network": "stack-private", "containers": stack},
                    {"id": "b" * 64, "name": "y", "network": "identity-private", "containers": [provider]}]
        FIXTURE.verify_running_resources({"containers": containers, "networks": networks, "helper": None})
        for broken in (networks[:1], [networks[0], dict(networks[1], containers=[])]):
            with self.subTest(networks=len(broken)), self.assertRaises(FIXTURE.FixtureError):
                FIXTURE.verify_running_resources({"containers": containers, "networks": broken, "helper": None})
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.verify_running_resources({"containers": containers[1:], "networks": networks, "helper": None})

    def serve_once(self, reply):
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        listener.settimeout(5)

        def answer():
            try:
                connection, _ = listener.accept()
            except OSError:
                return
            with connection:
                connection.settimeout(5)
                try:
                    connection.recv(1024)
                except OSError:
                    pass
                if reply:
                    connection.sendall(reply)

        thread = threading.Thread(target=answer)
        thread.start()
        return listener, thread

    def test_identity_request_ends_a_slow_answer_at_one_deadline(self):
        # Review #316 F3: an answer that keeps making progress, one byte for
        # each second of a fake clock, never reaches a per-operation socket
        # timeout. The request must still end at one absolute deadline and
        # close its TLS socket. A fast answer still passes.
        body = b'{"issuer": "' + b"a" * 4096 + b'"}'
        answer = (b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n"
                  % len(body)) + body

        class Answer(io.RawIOBase):
            """The http.client reader of a socket: it reads through recv_into."""

            def __init__(self, connection):
                super().__init__()
                self.connection = connection

            def readable(self):
                return True

            def readinto(self, buffer):
                return self.connection.recv_into(buffer)

        class Clock:
            now = 1000.0

            def __call__(self):
                return self.now

        class TLS:
            """A TLS socket whose answer gives STEP bytes for each SECONDS of the clock."""

            def __init__(self, clock, step, seconds):
                self.clock, self.step, self.seconds = clock, step, seconds
                self.offset, self.request, self.closed = 0, b"", False

            def settimeout(self, _value):
                pass

            def setsockopt(self, *_arguments):
                pass

            def send(self, data):
                self.request += bytes(data)
                return len(data)

            def sendall(self, data):
                self.request += bytes(data)

            def recv_into(self, buffer, *_arguments):
                piece = answer[self.offset:self.offset + min(self.step, len(buffer))]
                self.offset += len(piece)
                buffer[:len(piece)] = piece
                self.clock.now += self.seconds if piece else 0
                return len(piece)

            def makefile(self, _mode, *_arguments, **_options):
                return io.BufferedReader(Answer(self))

            def close(self):
                self.closed = True

        class Raw:
            def settimeout(self, _value):
                pass

            def setsockopt(self, *_arguments):
                pass

            def close(self):
                pass

        def request(step, seconds, phase=None):
            clock = Clock()
            tls = TLS(clock, step, seconds)
            context = mock.Mock()
            context.wrap_socket.return_value = tls
            with contextlib.ExitStack() as stack:
                stack.enter_context(mock.patch.object(FIXTURE, "CLOCK", clock, create=True))
                stack.enter_context(mock.patch.object(FIXTURE.socket, "create_connection", return_value=Raw()))
                if phase is not None:
                    stack.enter_context(FIXTURE.Phase("start", phase))
                try:
                    outcome = FIXTURE.identity_request("127.0.0.2", 13006, context, "GET", "/")
                except (FIXTURE.FixtureError, FIXTURE.PhaseDeadline) as exc:
                    outcome = exc
            return outcome, clock.now - 1000.0, tls

        outcome, _, tls = request(len(answer), 0)
        self.assertEqual(outcome, (200, {"issuer": "a" * 4096}))
        self.assertTrue(tls.closed)
        self.assertTrue(tls.request.startswith(b"GET / HTTP/1.1\r\nHost: 127.0.0.2:13006\r\n"))

        outcome, elapsed, tls = request(1, 1)
        self.assertIsInstance(outcome, FIXTURE.FixtureError)
        self.assertNotIsInstance(outcome, FIXTURE.PhaseDeadline)
        self.assertIn("did not end within its limit of 10 s", str(outcome))
        self.assertLessEqual(elapsed, FIXTURE.IDENTITY_PROBE_TIMEOUT)
        self.assertLess(tls.offset, len(answer))
        self.assertTrue(tls.closed)

        # The phase deadline sets the limit when less time is left in it.
        outcome, elapsed, tls = request(1, 1, phase=7)
        self.assertIsInstance(outcome, FIXTURE.PhaseDeadline)
        self.assertLessEqual(elapsed, 7)
        self.assertTrue(tls.closed)

    def test_plain_http_probe_needs_no_http_answer(self):
        for reply, refused in ((b"", True), (b"\x15\x03\x03\x00\x02\x02\x50", True),
                               (b"HTTP/1.1 400 Bad Request\r\n\r\n", True),
                               (b"HTTP/1.1 200 OK\r\n\r\n{}", False), (b"HTTP/1.1 302 Found\r\n\r\n", False)):
            with self.subTest(reply=reply[:16]):
                listener, thread = self.serve_once(reply)
                try:
                    port = listener.getsockname()[1]
                    if refused:
                        self.assertIsInstance(FIXTURE.plain_http_refusal("127.0.0.1", port, "pm-test"), str)
                    else:
                        with self.assertRaises(FIXTURE.FixtureError):
                            FIXTURE.plain_http_refusal("127.0.0.1", port, "pm-test")
                finally:
                    thread.join(timeout=10)
                    listener.close()
                self.assertFalse(thread.is_alive())

    def test_memory_file_system_is_read_from_the_longest_mount(self):
        mountinfo = (
            "22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n"
            "30 22 0:25 / /runner/_temp rw,nosuid - tmpfs tmpfs rw,size=1024k\n"
            "31 22 0:26 / /runner/_temp2 rw - xfs /dev/sdb rw\n"
        )
        self.assertEqual(FIXTURE.filesystem_type(Path("/runner/_temp/job/realm.json"), mountinfo), "tmpfs")
        self.assertEqual(FIXTURE.filesystem_type(Path("/runner/_temp2/job/realm.json"), mountinfo), "xfs")
        self.assertEqual(FIXTURE.filesystem_type(Path("/home/runner/realm.json"), mountinfo), "ext4")
        self.assertEqual(FIXTURE.filesystem_type(Path("/home/runner/realm.json"), ""), "unknown")


def stat_mode(path):
    return os.stat(path).st_mode & 0o777


if __name__ == "__main__":
    unittest.main()
