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
        # The leader stops on SIGTERM. Its descendant ignores SIGTERM and holds
        # the process output open. Both block on a FIFO that the test holds
        # open and never writes. The group SIGKILL after the grace must stop
        # the descendant. The leader is not reaped before the call, so its
        # process group ID names no other group (issue #286).
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
        # E6 lines 357-394 at 8fc2bd3: terminate_process_group called
        # process.poll(), which can reap the exited leader, and then signalled
        # the group by the leader's process ID. After the reap, that ID can
        # name another group. Here the leader has exited and is not reaped,
        # and a member of its group blocks on a FIFO that the test holds open
        # and never writes. Each killpg must come before the reap.
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

    def exited_leader(self, directory, descendant):
        """Start a leader that exits at once and is not reaped.

        With DESCENDANT, the leader first starts a member of its group that
        opens the FIFO "hold", writes its process ID to "pid" and then
        acknowledges on the FIFO "ack". The leader exits only after that
        acknowledgement. The member blocks on "hold" until the test closes it.
        Return (process, held member or None, the FIFO ends to close).
        """
        hold, ack, pid_file = (Path(directory) / name for name in ("hold", "ack", "pid"))
        os.mkfifo(hold)
        os.mkfifo(ack)
        ends = [os.open(hold, os.O_RDWR), os.open(ack, os.O_RDWR)]
        script = "exit 0"
        if descendant:
            script = ("sh -c 'exec 3<\"$0\"; echo $$ > \"$2\"; echo opened > \"$1\"; exec cat <&3 > /dev/null' "
                      "\"$0\" \"$1\" \"$2\" & read ack < \"$1\"")
        process = subprocess.Popen(["/bin/sh", "-c", script, str(hold), str(ack), str(pid_file)],
                                   stdin=subprocess.DEVNULL, start_new_session=True)
        # One bounded wait for the exit of the leader, without a reap.
        watch = FIXTURE.ExitWatch(process.pid)
        try:
            self.assertTrue(watch.wait(30), "the leader did not exit")
        finally:
            watch.close()
        member = FIXTURE.ExitWatch(int(pid_file.read_text())) if descendant else None
        return process, member, ends

    def test_group_signal_refusal_needs_an_empty_group(self):
        # brahmagupta F3 at 3b6e71f: the wrapper took any refused group signal
        # as harmless once the leader had exited. Now only Darwin, with a
        # member list that shows no live member, makes it harmless. killpg is
        # refused in each case, so no real signal goes to the group.
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
                process, member, ends = self.exited_leader(directory, descendant)
                try:
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
                finally:
                    if process.returncode is None:
                        process.wait()
                    for end in ends:
                        os.close(end)
                    if member is not None:
                        # One wait for the exit of the held member, which reads
                        # the end of the closed FIFO.
                        try:
                            self.assertTrue(member.wait(30), "the group member did not exit")
                        finally:
                            member.close()

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
    ENVIRONMENT = {"PM_STACK_TEST_CONFIG": "/unused/server.json",
                   "PM_STACK_TEST_ROLLOVER_NAMESPACE": "unused-rollover"}
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
        # The fake clock. With create=True, the patch also applies to a
        # wrapper that has no CLOCK, so that the check fails there on its
        # assertions.
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
        # Each call ends after 25 s, inside its own 30 s limit. Together the
        # calls pass 720 s in the discovery after `pm-stack up`.
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
        # docker info 30, the generator 60, 13 discovery calls 30, `pm-stack
        # up` with 345 s left, then discovery calls with 30 s until 20 s are left.
        self.assertEqual(timeouts[:2], [30, 60])
        self.assertEqual(timeouts[2:15], [30] * 13)
        self.assertEqual(start[15][0][-3:-2], ["up"])
        self.assertEqual(timeouts[15], 345)
        self.assertEqual(timeouts[16:], [30] * 12 + [20])
        # The 20 s call is stopped at the start deadline. The cleanup in the
        # run step then reaches its own deadline and stops one more call.
        self.assertEqual(len(fixture.stopped), 2)

    def test_phase_deadline_ends_the_cleanup_in_the_run_step(self):
        # The start and the command are quick. Each cleanup call ends after
        # 25 s. Together they pass the 360 s of the cleanup phase.
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
        # --cleanup in the workflow cleanup step: 300 s. The 12th call gets the
        # 25 s that are left. Then no time is left, and no 13th call starts.
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
        # One call that reaches its own 30 s limit gets the per-call message,
        # not the phase message: in the start phase and in --cleanup.
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

    def test_phase_deadline_limits_match_the_bdd_budget(self):
        # check-bdd.sh plans with the same start and cleanup times, plus the
        # wait after the SIGKILL of a stopped call, which the phase does not
        # limit.
        script = (ROOT / "scripts/ci/check-bdd.sh").read_text()
        grace = FIXTURE.PROCESS_TERM_GRACE
        self.assertIn(f"\nstart_allowance={FIXTURE.START_PHASE_LIMIT + grace}\n", script)
        self.assertIn(f"\ncleanup_allowance={FIXTURE.CLEANUP_PHASE_LIMIT + grace}\n", script)
        self.assertEqual(FIXTURE.CLEANUP_STEP_PHASE_LIMIT, 300)


if __name__ == "__main__":
    unittest.main()
