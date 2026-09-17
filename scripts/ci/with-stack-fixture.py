#!/usr/bin/env python3
"""Run one command against an owned, disposable PM dependency stack."""

from __future__ import annotations

import copy
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any, Mapping, Sequence
from urllib.parse import urlsplit


SCHEMA_VERSION = "pm.stack-fixture-ownership/v1"
PROJECT_PREFIX = "pmci-"
PARENT_PREFIX = "pm-stack-job-"
TOKEN_PATTERN = re.compile(r"[0-9a-f]{20}\Z")
PROJECT_PATTERN = re.compile(r"[a-z0-9][a-z0-9_-]{2,62}\Z")
TEMPORAL_NAMESPACE_PATTERN = re.compile(r"[a-z0-9][a-z0-9_-]{2,254}\Z")
MANIFEST_NAME_PATTERN = re.compile(r"pmcli-foundation-([0-9]+)-([0-9]+)\.json\Z")
CONTAINER_ID_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
SAFE_DAEMON_VALUE = re.compile(r"[A-Za-z0-9_.:+/=-]{1,256}\Z")
SAFE_ONEOFF_SUFFIX = re.compile(r"[a-z0-9]{6,64}\Z")

MAX_JSON_BYTES = 1 << 20
MAX_GENERATOR_OUTPUT = 64 << 10
DOCKER_TIMEOUT = 30
GENERATOR_TIMEOUT = 60
STARTUP_TIMEOUT = 10 * 60
CHILD_TIMEOUT = 20 * 60
DRAIN_TIMEOUT = 2 * 60
STOP_TIMEOUT = 75
HELPER_TIMEOUT = 60
PROCESS_TERM_GRACE = 10

ENVIRONMENT_KEYS = frozenset(
    {
        "PM_STACK_ROOT",
        "PM_STACK_SECRETS_DIR",
        "PM_STACK_STATE_DIR",
        "PM_STACK_PROJECT",
        "PM_STACK_TEMPORAL_DYNAMIC_CONFIG",
        "PM_STACK_TEST_CONFIG",
        "PM_STACK_TEST_ROLLOVER_NAMESPACE",
        "PM_STACK_TEST_S3_PRIMARY_ENDPOINT",
        "PM_STACK_TEST_S3_SECONDARY_ENDPOINT",
        "PM_STORE_TEST_SECRETS_DIR",
        "PM_STACK_POSTGRES_PORT",
        "PM_STACK_TEMPORAL_PORT",
        "PM_STACK_S3_PRIMARY_PORT",
        "PM_STACK_S3_SECONDARY_PORT",
        "PM_STACK_PUBLIC_PORT",
    }
)
PORT_KEYS = (
    "PM_STACK_POSTGRES_PORT",
    "PM_STACK_TEMPORAL_PORT",
    "PM_STACK_S3_PRIMARY_PORT",
    "PM_STACK_S3_SECONDARY_PORT",
    "PM_STACK_PUBLIC_PORT",
)
PERSISTENT_SERVICES = frozenset(
    {
        "postgres",
        "temporal",
        "seaweed-master",
        "seaweed-volume",
        "seaweed-filer",
        "seaweed-s3-a",
        "seaweed-s3-b",
        "caddy",
    }
)
ONEOFF_SERVICES = frozenset({"postgres-bootstrap", "temporal-schema"})
ALL_SERVICES = PERSISTENT_SERVICES | ONEOFF_SERVICES

EXPECTED_IMAGES: dict[str, dict[str, str]] = {
    "caddy": {
        "repository": "docker.io/library/caddy",
        "tag": "2.11.4",
        "index_digest": "sha256:13ba145cba2f3e28fa801994876e4c086d1b95d5aa2a520a734765ffb6b12017",
    },
    "postgres": {
        "repository": "docker.io/library/postgres",
        "tag": "16.15",
        "index_digest": "sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94",
    },
    "seaweedfs": {
        "repository": "docker.io/chrislusf/seaweedfs",
        "tag": "4.46",
        "index_digest": "sha256:08d516132314207d10c8e37cbffc1f32b147d870169688734cc61c6231625b62",
    },
    "temporal-admin-tools": {
        "repository": "docker.io/temporalio/admin-tools",
        "tag": "1.31.2",
        "index_digest": "sha256:dbc5fcd6ee8f0f4d808bf765af9a87dea9d8a283abfdcfbd2fc148496ba66107",
    },
    "temporal-server": {
        "repository": "docker.io/temporalio/server",
        "tag": "1.31.2",
        "index_digest": "sha256:b5ecdb8282bededae2a10c36e8d862e27d0bc2d247fc73c5416025997ab4a1da",
    },
}
EXPECTED_COMPOSE_DIGESTS = {
    "darwin-arm64": "998735c9b6fe68a4f05895e6ea73d71ad06f9fc7046383ad89e47346781b6af5",
    "linux-amd64": "db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576",
    "linux-arm64": "732e3a84c1a0f67256ce80bc2598a24546b10ca05f9faa97efceb1171ece2ef7",
}
PLATFORM_KEYS = {
    ("darwin", "arm64"): "darwin-arm64",
    ("darwin", "aarch64"): "darwin-arm64",
    ("linux", "amd64"): "linux-amd64",
    ("linux", "x86_64"): "linux-amd64",
    ("linux", "arm64"): "linux-arm64",
    ("linux", "aarch64"): "linux-arm64",
}

CONTAINER_FORMAT = (
    '{"id":{{json .Id}},"name":{{json .Name}},"image":{{json .Config.Image}},'
    '"image_id":{{json .Image}},"user":{{json .Config.User}},'
    '"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
    '"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
    '"config_files":{{json (index .Config.Labels "com.docker.compose.project.config_files")}},'
    '"working_dir":{{json (index .Config.Labels "com.docker.compose.project.working_dir")}},'
    '"oneoff":{{json (index .Config.Labels "com.docker.compose.oneoff")}},'
    '"helper":{{json (index .Config.Labels "ai.polymetrics.pm-stack-fixture.cleanup")}},'
    '"helper_project":{{json (index .Config.Labels "ai.polymetrics.pm-stack-fixture.project")}},'
    '"helper_root":{{json (index .Config.Labels "ai.polymetrics.pm-stack-fixture.root")}},'
    '"state":{{json .State.Status}},"exit_code":{{json .State.ExitCode}},'
    '"privileged":{{json .HostConfig.Privileged}},'
    '"network_mode":{{json .HostConfig.NetworkMode}},'
    '"pid_mode":{{json .HostConfig.PidMode}},"ipc_mode":{{json .HostConfig.IpcMode}},'
    '"readonly_rootfs":{{json .HostConfig.ReadonlyRootfs}},'
    '"init":{{json (index .HostConfig "Init")}},"auto_remove":{{json .HostConfig.AutoRemove}},'
    '"nano_cpus":{{json .HostConfig.NanoCpus}},'
    '"memory":{{json .HostConfig.Memory}},"pids_limit":{{json .HostConfig.PidsLimit}},'
    '"restart":{{json .HostConfig.RestartPolicy.Name}},'
    '"cap_add":{{json .HostConfig.CapAdd}},"cap_drop":{{json .HostConfig.CapDrop}},'
    '"security_opt":{{json .HostConfig.SecurityOpt}},'
    '"devices":{{json .HostConfig.Devices}},"device_requests":{{json .HostConfig.DeviceRequests}},'
    '"uts_mode":{{json .HostConfig.UTSMode}},"userns_mode":{{json .HostConfig.UsernsMode}},'
    '"port_bindings":{{json .HostConfig.PortBindings}},'
    '"mounts":{{json .Mounts}},"tmpfs":{{json (index .HostConfig "Tmpfs")}}}'
)
NETWORK_FORMAT = (
    '{"id":{{json .Id}},"name":{{json .Name}},"driver":{{json .Driver}},'
    '"scope":{{json .Scope}},"internal":{{json .Internal}},'
    '"attachable":{{json .Attachable}},"ingress":{{json .Ingress}},'
    '"project":{{json (index .Labels "com.docker.compose.project")}},'
    '"network":{{json (index .Labels "com.docker.compose.network")}},'
    '"owner":{{json (index .Labels "ai.polymetrics.pm-stack")}},'
    '"containers":{{json .Containers}}}'
)
DAEMON_FORMAT = (
    '{"id":{{json .ID}},"os":{{json .OSType}},'
    '"architecture":{{json .Architecture}},"server_version":{{json .ServerVersion}},'
    '"security_options":{{json .SecurityOptions}}}'
)


class FixtureError(Exception):
    """A safe, user-reportable fixture failure."""


class Cancellation(Exception):
    def __init__(self, signum: int) -> None:
        super().__init__(f"cancelled by signal {signum}")
        self.signum = signum


class CommandFailure(FixtureError):
    def __init__(self, stage: str, returncode: int | None = None, timed_out: bool = False) -> None:
        if timed_out:
            message = f"{stage} timed out"
        elif returncode is None:
            message = f"{stage} failed"
        else:
            message = f"{stage} failed with exit status {returncode}"
        super().__init__(message)
        self.stage = stage
        self.returncode = returncode
        self.timed_out = timed_out


class SignalController:
    def __init__(self) -> None:
        self.first_signal: int | None = None
        self.cleaning = False
        self._raised = False
        self._previous: dict[int, Any] = {}

    def install(self) -> None:
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            self._previous[signum] = signal.getsignal(signum)
            signal.signal(signum, self._handle)

    def restore(self) -> None:
        for signum, previous in self._previous.items():
            signal.signal(signum, previous)

    def enter_cleanup(self) -> None:
        self.cleaning = True

    def _handle(self, signum: int, _frame: Any) -> None:
        if self.first_signal is None:
            self.first_signal = signum
        if not self.cleaning and not self._raised:
            self._raised = True
            raise Cancellation(signum)


SIGNALS = SignalController()


def safe_error(message: str) -> None:
    print(f"with-stack-fixture: {message}", file=sys.stderr)


def open_regular_file(path: Path) -> int:
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0)
    nofollow = getattr(os, "O_NOFOLLOW", None)
    if nofollow is None:
        raise FixtureError("platform cannot safely open fixture files without following symlinks")
    try:
        descriptor = os.open(path, flags | nofollow)
        status = os.fstat(descriptor)
    except OSError as exc:
        if "descriptor" in locals():
            os.close(descriptor)
        raise FixtureError(f"cannot open required file {path.name}: {exc.strerror or exc}") from exc
    if not stat.S_ISREG(status.st_mode):
        os.close(descriptor)
        raise FixtureError(f"required file {path.name} is not regular")
    return descriptor


def read_limited(path: Path, limit: int = MAX_JSON_BYTES) -> bytes:
    descriptor = open_regular_file(path)
    try:
        with os.fdopen(descriptor, "rb") as stream:
            value = stream.read(limit + 1)
    except OSError as exc:
        raise FixtureError(f"cannot read required file {path.name}: {exc.strerror or exc}") from exc
    if len(value) > limit:
        raise FixtureError(f"required file {path.name} exceeds its size limit")
    return value


def decode_json_bytes(value: bytes, context: str) -> Any:
    try:
        return json.loads(value.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise FixtureError(f"{context} is not valid UTF-8 JSON") from exc


def load_json_file(path: Path, context: str) -> Any:
    return decode_json_bytes(read_limited(path), context)


def sha256_file(path: Path, limit: int | None = None) -> str:
    digest = hashlib.sha256()
    total = 0
    descriptor = open_regular_file(path)
    try:
        with os.fdopen(descriptor, "rb") as stream:
            for chunk in iter(lambda: stream.read(1 << 20), b""):
                total += len(chunk)
                if limit is not None and total > limit:
                    raise FixtureError(f"required file {path.name} exceeds its size limit")
                digest.update(chunk)
    except OSError as exc:
        raise FixtureError(f"cannot hash required file {path.name}: {exc.strerror or exc}") from exc
    return digest.hexdigest()


def fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def path_identity(path: Path) -> dict[str, int]:
    try:
        status = path.lstat()
    except OSError as exc:
        raise FixtureError(f"cannot inspect owned path {path}: {exc.strerror or exc}") from exc
    if not stat.S_ISDIR(status.st_mode) or path.is_symlink():
        raise FixtureError(f"owned path is not a real directory: {path}")
    return {
        "device": status.st_dev,
        "inode": status.st_ino,
        "uid": status.st_uid,
        "mode": stat.S_IMODE(status.st_mode),
    }


def require_identity(path: Path, expected: Mapping[str, Any], context: str) -> None:
    actual = path_identity(path)
    if actual != expected:
        raise FixtureError(f"{context} identity changed")
    if actual["uid"] != os.getuid() or actual["mode"] & 0o077:
        raise FixtureError(f"{context} is not private to the current job user")


def regular_executable(path: Path, context: str) -> Path:
    try:
        status = path.lstat()
    except OSError as exc:
        raise FixtureError(f"missing {context}: {path}") from exc
    if path.is_symlink() or not stat.S_ISREG(status.st_mode) or not os.access(path, os.X_OK):
        raise FixtureError(f"{context} must be an executable regular file: {path}")
    return path


def terminate_process_group(process: subprocess.Popen[bytes]) -> None:
    def signal_group(signum: int) -> bool:
        try:
            os.killpg(process.pid, signum)
            return True
        except ProcessLookupError:
            return False
        except PermissionError as denied:
            # Darwin can report EPERM while the owned leader becomes a zombie.
            # Reap that exact child before retrying; never waive a live denial.
            try:
                process.wait(timeout=PROCESS_TERM_GRACE)
            except subprocess.TimeoutExpired:
                raise denied
            try:
                os.killpg(process.pid, signum)
                return True
            except ProcessLookupError:
                return False

    process.poll()
    signal_group(signal.SIGTERM)
    deadline = time.monotonic() + PROCESS_TERM_GRACE
    while True:
        process.poll()
        if not signal_group(0):
            break
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            # A reaped leader does not prove its descendants have stopped.
            signal_group(signal.SIGKILL)
            break
        time.sleep(min(0.05, remaining))
    try:
        process.wait(timeout=PROCESS_TERM_GRACE)
    except subprocess.TimeoutExpired as exc:
        raise FixtureError("owned process group did not terminate") from exc


def run_capture(
    arguments: Sequence[str],
    *,
    env: Mapping[str, str],
    cwd: Path,
    timeout: int,
    stage: str,
    max_output: int = MAX_JSON_BYTES,
    check: bool = True,
) -> tuple[int, bytes, bytes]:
    try:
        process = subprocess.Popen(
            list(arguments),
            cwd=cwd,
            env=dict(env),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )
    except OSError as exc:
        raise FixtureError(f"cannot start {stage}: {exc.strerror or exc}") from exc
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        terminate_process_group(process)
        raise CommandFailure(stage, timed_out=True) from exc
    except Cancellation:
        terminate_process_group(process)
        raise
    if len(stdout) > max_output or len(stderr) > max_output:
        raise FixtureError(f"{stage} produced excessive output")
    if check and process.returncode != 0:
        raise CommandFailure(stage, process.returncode)
    return process.returncode, stdout, stderr


def run_quiet(
    arguments: Sequence[str],
    *,
    env: Mapping[str, str],
    cwd: Path,
    timeout: int,
    stage: str,
    check: bool = True,
) -> int:
    try:
        process = subprocess.Popen(
            list(arguments),
            cwd=cwd,
            env=dict(env),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
    except OSError as exc:
        raise FixtureError(f"cannot start {stage}: {exc.strerror or exc}") from exc
    try:
        returncode = process.wait(timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        terminate_process_group(process)
        raise CommandFailure(stage, timed_out=True) from exc
    except Cancellation:
        terminate_process_group(process)
        raise
    if check and returncode != 0:
        raise CommandFailure(stage, returncode)
    return returncode


def run_child(arguments: Sequence[str], *, env: Mapping[str, str], cwd: Path) -> int:
    try:
        process = subprocess.Popen(
            list(arguments),
            cwd=cwd,
            env=dict(env),
            start_new_session=True,
        )
    except OSError as exc:
        raise FixtureError(f"cannot start requested command: {exc.strerror or exc}") from exc
    try:
        returncode = process.wait(timeout=CHILD_TIMEOUT)
    except subprocess.TimeoutExpired:
        terminate_process_group(process)
        safe_error("requested command timed out")
        return 124
    except Cancellation:
        terminate_process_group(process)
        raise
    if returncode < 0:
        return 128 + -returncode
    return returncode


def sanitized_environment() -> dict[str, str]:
    environment: dict[str, str] = {}
    for name, value in os.environ.items():
        if name.startswith("DOCKER_") or name.startswith("COMPOSE_"):
            continue
        if name.startswith("PM_STACK_") or name == "PM_STORE_TEST_SECRETS_DIR":
            continue
        environment[name] = value
    environment["COMPOSE_DISABLE_ENV_FILE"] = "true"
    environment["COMPOSE_ENV_FILES"] = ""
    return environment


def resolve_runner_temp() -> Path:
    raw = os.environ.get("RUNNER_TEMP", "")
    if not raw or not Path(raw).is_absolute():
        raise FixtureError("RUNNER_TEMP must name an existing absolute directory")
    try:
        resolved = Path(raw).resolve(strict=True)
    except OSError as exc:
        raise FixtureError("RUNNER_TEMP is unavailable") from exc
    if not resolved.is_dir():
        raise FixtureError("RUNNER_TEMP must name an existing directory")
    return resolved


def validate_manifest_path(raw: str, runner_temp: Path) -> tuple[Path, str, str]:
    candidate = Path(raw)
    if not candidate.is_absolute():
        raise FixtureError("manifest path must be absolute")
    try:
        parent = candidate.parent.resolve(strict=True)
    except OSError as exc:
        raise FixtureError("manifest parent is unavailable") from exc
    if parent != runner_temp:
        raise FixtureError("manifest must be a direct child of RUNNER_TEMP")
    candidate = runner_temp / candidate.name
    match = MANIFEST_NAME_PATTERN.fullmatch(candidate.name)
    if match is None:
        raise FixtureError("manifest name must bind a numeric run ID and attempt")
    run_id, run_attempt = match.groups()
    environment_run = os.environ.get("GITHUB_RUN_ID")
    environment_attempt = os.environ.get("GITHUB_RUN_ATTEMPT")
    if (environment_run is None) != (environment_attempt is None):
        raise FixtureError("GitHub run identity is incomplete")
    if environment_run is not None:
        if not environment_run.isdigit() or not environment_attempt or not environment_attempt.isdigit():
            raise FixtureError("GitHub run identity is invalid")
        if (run_id, run_attempt) != (environment_run, environment_attempt):
            raise FixtureError("manifest does not belong to this GitHub run attempt")
    return candidate, run_id, run_attempt


def choose_docker_socket() -> tuple[Path, dict[str, int]]:
    system = platform.system().lower()
    if system == "linux":
        selected = Path("/var/run/docker.sock")
    elif system == "darwin":
        selected = Path.home() / ".docker" / "run" / "docker.sock"
    else:
        raise FixtureError("only local Unix Docker on Linux or Docker Desktop is supported")
    try:
        resolved = selected.resolve(strict=True)
        status = resolved.stat()
    except OSError as exc:
        raise FixtureError("the selected local Docker Unix socket is unavailable") from exc
    if not stat.S_ISSOCK(status.st_mode):
        raise FixtureError("the selected Docker endpoint is not a Unix socket")
    return resolved, {
        "device": status.st_dev,
        "inode": status.st_ino,
        "uid": status.st_uid,
        "mode": stat.S_IMODE(status.st_mode),
    }


def create_docker_config(path: Path) -> None:
    expected = {"auths": {"https://index.docker.io/v1/": {}}}
    if os.path.lexists(path):
        identity = path_identity(path)
        if identity["uid"] != os.getuid() or identity["mode"] & 0o077:
            raise FixtureError("Docker configuration directory is not private")
    else:
        path.mkdir(mode=0o700)
    config = path / "config.json"
    encoded = (json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n").encode()
    if os.path.lexists(config):
        status = config.lstat()
        if config.is_symlink() or not stat.S_ISREG(status.st_mode):
            raise FixtureError("Docker configuration is not a regular file")
        if status.st_uid != os.getuid() or stat.S_IMODE(status.st_mode) & 0o077:
            raise FixtureError("Docker configuration is not private")
        if decode_json_bytes(read_limited(config, 4096), "Docker configuration") != expected:
            raise FixtureError("Docker configuration contains an unapproved credential source")
        return
    descriptor = os.open(config, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
    except Exception:
        try:
            config.unlink()
        except OSError:
            pass
        raise
    fsync_directory(path)


def docker_environment(base: Mapping[str, str], config: Path, endpoint: str) -> dict[str, str]:
    environment = dict(base)
    environment["DOCKER_CONFIG"] = str(config)
    environment["DOCKER_HOST"] = endpoint
    environment["COMPOSE_DISABLE_ENV_FILE"] = "true"
    environment["COMPOSE_ENV_FILES"] = ""
    return environment


def docker_command(docker: Path, config: Path, endpoint: str, *arguments: str) -> list[str]:
    return [str(docker), "--config", str(config), "--host", endpoint, *arguments]


def parse_single_json(stdout: bytes, context: str) -> Any:
    if not stdout.strip():
        raise FixtureError(f"{context} returned no JSON")
    return decode_json_bytes(stdout, context)


def inspect_daemon(
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
    socket_identity: Mapping[str, int],
) -> dict[str, Any]:
    _, stdout, _ = run_capture(
        docker_command(docker, config, endpoint, "info", "--format", DAEMON_FORMAT),
        env=env,
        cwd=cwd,
        timeout=DOCKER_TIMEOUT,
        stage="inspect Docker engine",
    )
    value = parse_single_json(stdout, "Docker engine inspection")
    if not isinstance(value, dict) or set(value) != {"id", "os", "architecture", "server_version", "security_options"}:
        raise FixtureError("Docker engine inspection returned an unexpected shape")
    value["rootless"] = rootless_engine(value.pop("security_options"))
    for name in ("id", "os", "architecture", "server_version"):
        if not isinstance(value[name], str) or SAFE_DAEMON_VALUE.fullmatch(value[name]) is None:
            raise FixtureError("Docker engine inspection returned an unsafe identity")
    if value["os"] != "linux":
        raise FixtureError("the selected Docker engine is not a Linux engine")
    value["endpoint"] = endpoint
    value["socket"] = dict(socket_identity)
    return value


def rootless_engine(options: Any) -> bool:
    if not isinstance(options, list) or any(not isinstance(option, str) for option in options):
        raise FixtureError("Docker security options are invalid")
    names = {option.split(",", 1)[0] for option in options}
    if "name=rootless" in names:
        return True
    if "name=userns" in names:
        raise FixtureError("rootful Docker userns-remap lacks an admitted bind-mount ownership mapping")
    return False


def verify_daemon_identity(
    expected: Mapping[str, Any],
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
) -> None:
    selected_socket, selected_identity = choose_docker_socket()
    if endpoint != "unix://" + str(selected_socket) or expected.get("socket") != selected_identity:
        raise FixtureError("recorded Docker endpoint is not this host's selected local socket")
    socket_path = Path(endpoint.removeprefix("unix://"))
    try:
        status = socket_path.stat()
    except OSError as exc:
        raise FixtureError("recorded Docker socket is unavailable") from exc
    socket_identity = {
        "device": status.st_dev,
        "inode": status.st_ino,
        "uid": status.st_uid,
        "mode": stat.S_IMODE(status.st_mode),
    }
    actual = inspect_daemon(docker, config, endpoint, env, cwd, socket_identity)
    if actual != expected:
        raise FixtureError("Docker daemon ownership changed")


def normalized_platform() -> str:
    key = (platform.system().strip().lower(), platform.machine().strip().lower())
    try:
        return PLATFORM_KEYS[key]
    except KeyError as exc:
        raise FixtureError("host platform has no admitted Docker Compose asset") from exc


def validate_compose_cache(repo_root: Path, environment: Mapping[str, str]) -> Path:
    lock = load_json_file(repo_root / "build" / "toolchains.lock.json", "toolchain lock")
    if not isinstance(lock, dict) or not isinstance(lock.get("assets"), list):
        raise FixtureError("toolchain lock is invalid")
    selected_platform = normalized_platform()
    matches = [
        asset
        for asset in lock["assets"]
        if isinstance(asset, dict)
        and asset.get("id") == "docker-compose"
        and asset.get("platform") == selected_platform
    ]
    if len(matches) != 1:
        raise FixtureError("the admitted Docker Compose asset is unavailable")
    asset = matches[0]
    expected_digest = EXPECTED_COMPOSE_DIGESTS[selected_platform]
    expected_identity = {
        "id": "docker-compose",
        "version": "5.5.1",
        "platform": selected_platform,
        "sha256": expected_digest,
    }
    if any(asset.get(name) != value for name, value in expected_identity.items()):
        raise FixtureError("Docker Compose lock identity changed")
    if asset.get("kind") != "binary" or asset.get("executable") != "docker-compose":
        raise FixtureError("Docker Compose lock shape changed")
    configured_cache = environment.get("PM_TOOLS_CACHE")
    cache = Path(configured_cache).expanduser() if configured_cache else repo_root / ".cache" / "pm-tools"
    try:
        cache = cache.resolve(strict=True)
    except OSError as exc:
        raise FixtureError("the preinstalled PM tools cache is unavailable") from exc
    if not cache.is_dir() or cache.is_symlink():
        raise FixtureError("the preinstalled PM tools cache is unsafe")
    if cache == Path("/tmp") or Path("/tmp") in cache.parents or cache == Path("/private/tmp") or Path("/private/tmp") in cache.parents:
        raise FixtureError("the preinstalled PM tools cache cannot be temporary")
    download = cache / "downloads" / f"{expected_digest}.blob"
    install = cache / "tools" / "docker-compose" / "5.5.1" / selected_platform / expected_digest
    executable = install / "docker-compose"
    marker = install / ".pm-bootstrap.json"
    for path, context in ((download, "Compose download"), (executable, "Compose executable"), (marker, "Compose cache marker")):
        try:
            status = path.lstat()
        except OSError as exc:
            raise FixtureError(f"preinstalled {context} is unavailable") from exc
        if path.is_symlink() or not stat.S_ISREG(status.st_mode):
            raise FixtureError(f"preinstalled {context} is unsafe")
    if sha256_file(download) != expected_digest:
        raise FixtureError("preinstalled Compose download digest changed")
    marker_value = load_json_file(marker, "Compose cache marker")
    if not isinstance(marker_value, dict) or set(marker_value) != {"schema_version", "asset", "executables"}:
        raise FixtureError("Compose cache marker shape changed")
    if marker_value.get("schema_version") != "pm.bootstrap-cache/v1" or marker_value.get("asset") != expected_identity:
        raise FixtureError("Compose cache marker identity changed")
    executables = marker_value.get("executables")
    if not isinstance(executables, dict) or set(executables) != {"docker-compose"}:
        raise FixtureError("Compose cache executable set changed")
    executable_digest = executables.get("docker-compose")
    if not isinstance(executable_digest, str) or re.fullmatch(r"[0-9a-f]{64}", executable_digest) is None:
        raise FixtureError("Compose cache executable digest is invalid")
    if sha256_file(executable) != executable_digest or not os.access(executable, os.X_OK):
        raise FixtureError("preinstalled Compose executable changed")
    return executable


def load_image_refs(repo_root: Path) -> dict[str, str]:
    lock = load_json_file(repo_root / "deploy" / "images.lock.json", "deployment image lock")
    if not isinstance(lock, dict) or set(lock) != {"schema_version", "source", "images"}:
        raise FixtureError("deployment image lock shape changed")
    if lock["schema_version"] != 1 or lock["source"] != "build/toolchains.lock.json":
        raise FixtureError("deployment image lock identity changed")
    if lock["images"] != EXPECTED_IMAGES:
        raise FixtureError("deployment image lock no longer matches the admitted image set")
    return {
        name: specification["repository"] + "@" + specification["index_digest"]
        for name, specification in EXPECTED_IMAGES.items()
    }


def validate_private_tree(root: Path) -> None:
    try:
        root_status = root.lstat()
    except OSError as exc:
        raise FixtureError("generated fixture root is unavailable") from exc
    if root.is_symlink() or not stat.S_ISDIR(root_status.st_mode):
        raise FixtureError("generated fixture root is not a real directory")
    if root_status.st_uid != os.getuid() or stat.S_IMODE(root_status.st_mode) & 0o077:
        raise FixtureError("generated fixture root is not private")
    for current, directories, files in os.walk(root, topdown=True, followlinks=False):
        current_path = Path(current)
        for name in [*directories, *files]:
            path = current_path / name
            status = path.lstat()
            if stat.S_ISLNK(status.st_mode):
                raise FixtureError("generated fixture contains a symbolic link")
            if status.st_uid != os.getuid():
                raise FixtureError("generated fixture ownership is unsafe")
            if stat.S_ISDIR(status.st_mode):
                if stat.S_IMODE(status.st_mode) & 0o077:
                    raise FixtureError("generated fixture directory is not private")
            elif stat.S_ISREG(status.st_mode):
                if stat.S_IMODE(status.st_mode) & 0o077 or status.st_size > MAX_JSON_BYTES:
                    raise FixtureError("generated fixture file is not private and bounded")
            else:
                raise FixtureError("generated fixture contains a special file")


def require_exact_path(value: str, expected: Path, name: str) -> None:
    if not Path(value).is_absolute() or value != str(expected):
        raise FixtureError(f"generated {name} path is not the expected owned path")


def parse_loopback_endpoint(value: str, expected_port: str, name: str) -> None:
    try:
        endpoint = urlsplit(value)
        port = endpoint.port
    except ValueError as exc:
        raise FixtureError(f"generated {name} endpoint is invalid") from exc
    if (
        endpoint.scheme != "https"
        or endpoint.hostname != "127.0.0.1"
        or str(port) != expected_port
        or endpoint.username is not None
        or endpoint.password is not None
        or endpoint.path not in ("", "/")
        or endpoint.query
        or endpoint.fragment
    ):
        raise FixtureError(f"generated {name} endpoint is not the selected loopback endpoint")


def expected_temporal_profile(base: bytes, namespace: str) -> bytes:
    if not base:
        raise FixtureError("repository Temporal dynamic configuration is empty")
    if not base.endswith(b"\n"):
        base += b"\n"
    return base + (
        "limit.historyCount.suggestContinueAsNew:\n"
        "  - value: 64\n"
        "    constraints:\n"
        f"      namespace: {json.dumps(namespace)}\n"
    ).encode()


def validate_generated_environment(
    stdout: bytes,
    fixture_root: Path,
    repo_root: Path,
    project: str,
) -> tuple[dict[str, str], str, str, str]:
    if len(stdout) > MAX_GENERATOR_OUTPUT:
        raise FixtureError("fixture generator output exceeds its limit")
    value = parse_single_json(stdout, "fixture generator")
    if not isinstance(value, dict) or set(value) != ENVIRONMENT_KEYS:
        raise FixtureError("fixture generator returned an unexpected environment map")
    environment: dict[str, str] = {}
    for name, item in value.items():
        if not isinstance(name, str) or not isinstance(item, str):
            raise FixtureError("fixture environment must be a flat string map")
        if not item or len(item) > 4096 or any(character in item for character in ("\x00", "\r", "\n")):
            raise FixtureError(f"fixture environment value {name} is unsafe")
        environment[name] = item
    if environment["PM_STACK_PROJECT"] != project:
        raise FixtureError("fixture project identity changed")
    require_exact_path(environment["PM_STACK_ROOT"], repo_root, "repository root")
    require_exact_path(environment["PM_STACK_SECRETS_DIR"], fixture_root / "secrets", "secret root")
    require_exact_path(environment["PM_STACK_STATE_DIR"], fixture_root / "state", "state root")
    require_exact_path(
        environment["PM_STACK_TEMPORAL_DYNAMIC_CONFIG"],
        fixture_root / "temporal-dynamicconfig.yaml",
        "Temporal dynamic configuration",
    )
    require_exact_path(environment["PM_STACK_TEST_CONFIG"], fixture_root / "server.json", "configuration")
    require_exact_path(
        environment["PM_STORE_TEST_SECRETS_DIR"],
        fixture_root / "secrets" / "operator" / "postgres",
        "store secret root",
    )
    rollover_namespace = environment["PM_STACK_TEST_ROLLOVER_NAMESPACE"]
    if (
        rollover_namespace != project + "-rollover"
        or TEMPORAL_NAMESPACE_PATTERN.fullmatch(rollover_namespace) is None
    ):
        raise FixtureError("fixture rollover namespace is not the exact auxiliary namespace")
    seen_ports: set[int] = set()
    for name in PORT_KEYS:
        value_string = environment[name]
        try:
            port = int(value_string)
        except ValueError as exc:
            raise FixtureError(f"fixture port {name} is invalid") from exc
        if not 1024 <= port <= 65535 or str(port) != value_string or port in seen_ports:
            raise FixtureError("fixture ports must be distinct canonical non-privileged ports")
        seen_ports.add(port)
    parse_loopback_endpoint(
        environment["PM_STACK_TEST_S3_PRIMARY_ENDPOINT"],
        environment["PM_STACK_S3_PRIMARY_PORT"],
        "primary S3",
    )
    parse_loopback_endpoint(
        environment["PM_STACK_TEST_S3_SECONDARY_ENDPOINT"],
        environment["PM_STACK_S3_SECONDARY_PORT"],
        "secondary S3",
    )
    environment_path = fixture_root / "environment.json"
    environment_bytes = read_limited(environment_path)
    if decode_json_bytes(environment_bytes, "fixture environment file") != environment:
        raise FixtureError("fixture environment file disagrees with generator output")
    dynamic_config_path = fixture_root / "temporal-dynamicconfig.yaml"
    dynamic_config_bytes = read_limited(dynamic_config_path)
    base_dynamic_config = read_limited(repo_root / "deploy" / "temporal" / "dynamicconfig.yaml")
    if dynamic_config_bytes != expected_temporal_profile(base_dynamic_config, rollover_namespace):
        raise FixtureError("generated Temporal profile changed primary settings or auxiliary constraint")
    config_path = fixture_root / "server.json"
    configuration = load_json_file(config_path, "fixture server configuration")
    template = load_json_file(repo_root / "deploy" / "server.example.json", "server configuration template")
    if not isinstance(template, dict):
        raise FixtureError("server configuration template is invalid")
    expected = copy.deepcopy(template)
    try:
        expected["deployment_id"] = project
        expected["public_origin"] = "https://127.0.0.1:" + environment["PM_STACK_PUBLIC_PORT"]
        expected["postgres"]["endpoint_ref"] = "secret://operator/postgres"
        expected["temporal"]["endpoint"] = "127.0.0.1:" + environment["PM_STACK_TEMPORAL_PORT"]
        expected["temporal"]["namespace"] = project
        expected["temporal"]["client_identity_ref"] = "secret://operator/temporal"
        expected["artifacts"]["endpoint"] = expected["public_origin"]
        expected["artifacts"]["bucket"] = project + "-artifacts"
        expected["artifacts"]["credential_ref"] = "secret://operator/artifacts"
    except (KeyError, TypeError) as exc:
        raise FixtureError("server configuration template shape changed") from exc
    if configuration != expected:
        raise FixtureError("generated configuration is not the bounded template projection")
    validate_private_tree(fixture_root)
    return (
        environment,
        hashlib.sha256(environment_bytes).hexdigest(),
        sha256_file(config_path, MAX_JSON_BYTES),
        sha256_file(dynamic_config_path, MAX_JSON_BYTES),
    )


def reserve_ports(environment: Mapping[str, str]) -> list[socket.socket]:
    reservations: list[socket.socket] = []
    try:
        for name in PORT_KEYS:
            reservation = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            reservation.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 0)
            reservation.bind(("127.0.0.1", int(environment[name])))
            reservation.listen(1)
            reservations.append(reservation)
    except OSError as exc:
        for reservation in reservations:
            reservation.close()
        raise FixtureError("a selected fixed fixture port is no longer available") from exc
    return reservations


def close_reservations(reservations: list[socket.socket]) -> None:
    while reservations:
        reservations.pop().close()


class OwnershipManifest:
    def __init__(
        self,
        path: Path,
        runner_temp: Path,
        repo_root: Path,
        run_id: str,
        run_attempt: str,
        document: dict[str, Any],
    ) -> None:
        self.path = path
        self.runner_temp = runner_temp
        self.repo_root = repo_root
        self.run_id = run_id
        self.run_attempt = run_attempt
        self.document = document

    @classmethod
    def create(
        cls,
        path: Path,
        runner_temp: Path,
        repo_root: Path,
        run_id: str,
        run_attempt: str,
        token: str,
        parent: Path,
        fixture_root: Path,
        project: str,
        daemon: Mapping[str, Any],
    ) -> "OwnershipManifest":
        document: dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "revision": 0,
            "phase": "prepared",
            "docker_mutation_authorized": False,
            "fixture_deletion_authorized": False,
            "ownership": {
                "token": token,
                "run_id": run_id,
                "run_attempt": run_attempt,
                "runner_temp": str(runner_temp),
                "manifest": str(path),
                "repo_root": str(repo_root),
                "parent": str(parent),
                "fixture_root": str(fixture_root),
                "project": project,
                "parent_identity": path_identity(parent),
                "daemon": dict(daemon),
            },
            "fixture": None,
            "resources": {"containers": [], "network": None, "helper": None},
            "diagnostic": None,
            "updated_at_ns": time.time_ns(),
        }
        manifest = cls(path, runner_temp, repo_root, run_id, run_attempt, document)
        manifest._validate_document(document)
        manifest._write_exclusive(document)
        return manifest

    @classmethod
    def load(
        cls,
        path: Path,
        runner_temp: Path,
        repo_root: Path,
        run_id: str,
        run_attempt: str,
    ) -> "OwnershipManifest":
        if path.is_symlink():
            raise FixtureError("ownership manifest must not be a symbolic link")
        try:
            status = path.lstat()
        except OSError as exc:
            raise FixtureError("ownership manifest is unavailable") from exc
        if not stat.S_ISREG(status.st_mode) or status.st_uid != os.getuid() or stat.S_IMODE(status.st_mode) & 0o077:
            raise FixtureError("ownership manifest is not a private job-owned regular file")
        value = load_json_file(path, "ownership manifest")
        if not isinstance(value, dict):
            raise FixtureError("ownership manifest has an invalid shape")
        manifest = cls(path, runner_temp, repo_root, run_id, run_attempt, value)
        manifest._validate_document(value)
        return manifest

    @property
    def ownership(self) -> dict[str, Any]:
        value = self.document["ownership"]
        if not isinstance(value, dict):
            raise FixtureError("ownership manifest is invalid")
        return value

    @property
    def parent(self) -> Path:
        return Path(self.ownership["parent"])

    @property
    def fixture_root(self) -> Path:
        return Path(self.ownership["fixture_root"])

    @property
    def project(self) -> str:
        return self.ownership["project"]

    @property
    def token(self) -> str:
        return self.ownership["token"]

    @property
    def daemon(self) -> dict[str, Any]:
        return self.ownership["daemon"]

    def _validate_document(self, document: Mapping[str, Any]) -> None:
        expected_keys = {
            "schema_version",
            "revision",
            "phase",
            "docker_mutation_authorized",
            "fixture_deletion_authorized",
            "ownership",
            "fixture",
            "resources",
            "diagnostic",
            "updated_at_ns",
        }
        if set(document) != expected_keys or document.get("schema_version") != SCHEMA_VERSION:
            raise FixtureError("ownership manifest schema is invalid")
        if not isinstance(document.get("revision"), int) or document["revision"] < 0:
            raise FixtureError("ownership manifest revision is invalid")
        if document.get("phase") not in {
            "prepared",
            "generated",
            "starting",
            "running",
            "cleaning",
            "cleanup_failed",
            "cleaned",
        }:
            raise FixtureError("ownership manifest phase is invalid")
        if not isinstance(document.get("docker_mutation_authorized"), bool):
            raise FixtureError("ownership manifest mutation boundary is invalid")
        deletion_authorized = document.get("fixture_deletion_authorized")
        if not isinstance(deletion_authorized, bool) or (
            deletion_authorized and not document["docker_mutation_authorized"]
        ):
            raise FixtureError("ownership manifest deletion boundary is invalid")
        ownership = document.get("ownership")
        if not isinstance(ownership, dict) or set(ownership) != {
            "token",
            "run_id",
            "run_attempt",
            "runner_temp",
            "manifest",
            "repo_root",
            "parent",
            "fixture_root",
            "project",
            "parent_identity",
            "daemon",
        }:
            raise FixtureError("ownership manifest identity is invalid")
        token = ownership.get("token")
        if not isinstance(token, str) or TOKEN_PATTERN.fullmatch(token) is None:
            raise FixtureError("ownership manifest token is invalid")
        expected_parent = self.runner_temp / f"{PARENT_PREFIX}{token}"
        expected_root = expected_parent / "fixture"
        expected_project = PROJECT_PREFIX + token
        if (
            ownership.get("run_id") != self.run_id
            or ownership.get("run_attempt") != self.run_attempt
            or ownership.get("runner_temp") != str(self.runner_temp)
            or ownership.get("manifest") != str(self.path)
            or ownership.get("repo_root") != str(self.repo_root)
            or ownership.get("parent") != str(expected_parent)
            or ownership.get("fixture_root") != str(expected_root)
            or ownership.get("project") != expected_project
            or PROJECT_PATTERN.fullmatch(expected_project) is None
        ):
            raise FixtureError("ownership manifest does not belong to this exact job and fixture")
        parent_identity = ownership.get("parent_identity")
        if not isinstance(parent_identity, dict) or set(parent_identity) != {"device", "inode", "uid", "mode"}:
            raise FixtureError("ownership manifest parent identity is invalid")
        if any(not isinstance(value, int) for value in parent_identity.values()):
            raise FixtureError("ownership manifest parent identity is invalid")
        daemon = ownership.get("daemon")
        if not isinstance(daemon, dict) or set(daemon) != {
            "id",
            "os",
            "architecture",
            "server_version",
            "rootless",
            "endpoint",
            "socket",
        }:
            raise FixtureError("ownership manifest daemon identity is invalid")
        if type(daemon["rootless"]) is not bool:
            raise FixtureError("ownership manifest daemon user namespace is invalid")
        for name in ("id", "os", "architecture", "server_version", "endpoint"):
            if not isinstance(daemon.get(name), str) or SAFE_DAEMON_VALUE.fullmatch(daemon[name]) is None:
                raise FixtureError("ownership manifest daemon identity is unsafe")
        if daemon["os"] != "linux" or not daemon["endpoint"].startswith("unix:///"):
            raise FixtureError("ownership manifest daemon is not the admitted local Linux engine")
        socket_value = daemon.get("socket")
        if not isinstance(socket_value, dict) or set(socket_value) != {"device", "inode", "uid", "mode"}:
            raise FixtureError("ownership manifest socket identity is invalid")
        if any(not isinstance(value, int) for value in socket_value.values()):
            raise FixtureError("ownership manifest socket identity is invalid")
        resources = document.get("resources")
        if not isinstance(resources, dict) or set(resources) != {"containers", "network", "helper"}:
            raise FixtureError("ownership manifest resource record is invalid")
        if not isinstance(resources["containers"], list):
            raise FixtureError("ownership manifest container record is invalid")
        if document["phase"] == "cleaned" and (
            resources != {"containers": [], "network": None, "helper": None}
            or document.get("diagnostic") is not None
        ):
            raise FixtureError("cleaned ownership receipt contains live-resource authority")
        if not isinstance(document.get("updated_at_ns"), int):
            raise FixtureError("ownership manifest timestamp is invalid")
        fixture = document.get("fixture")
        authorized = document["docker_mutation_authorized"]
        if authorized and not isinstance(fixture, dict):
            raise FixtureError("authorized ownership manifest lacks fixture identity")
        if document["phase"] in {"starting", "running"} and not authorized:
            raise FixtureError("ownership manifest phase precedes Docker authorization")

    def _write_exclusive(self, document: Mapping[str, Any]) -> None:
        encoded = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n").encode()
        try:
            descriptor = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError as exc:
            raise FixtureError("ownership manifest already exists") from exc
        try:
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(stream.fileno())
            fsync_directory(self.path.parent)
        except Exception:
            try:
                self.path.unlink()
            except OSError:
                pass
            raise

    def update(
        self,
        phase: str,
        *,
        fixture: Mapping[str, Any] | None = None,
        resources: Mapping[str, Any] | None = None,
        diagnostic: Mapping[str, Any] | None = None,
        authorize_docker: bool = False,
        authorize_deletion: bool = False,
    ) -> None:
        current = OwnershipManifest.load(
            self.path,
            self.runner_temp,
            self.repo_root,
            self.run_id,
            self.run_attempt,
        ).document
        if current["ownership"] != self.document["ownership"]:
            raise FixtureError("ownership manifest immutable identity changed")
        if self.document["fixture"] is not None and current["fixture"] != self.document["fixture"]:
            raise FixtureError("ownership manifest fixture identity changed")
        if (
            current["fixture"] is not None
            and fixture is not None
            and dict(fixture) != current["fixture"]
        ):
            raise FixtureError("ownership manifest fixture identity is immutable")
        if current["phase"] == "cleaned" and phase != "cleaned":
            raise FixtureError("cleaned ownership receipt cannot authorize more work")
        updated = copy.deepcopy(current)
        updated["revision"] += 1
        updated["phase"] = phase
        updated["updated_at_ns"] = time.time_ns()
        if fixture is not None:
            updated["fixture"] = dict(fixture)
        if resources is not None:
            updated["resources"] = dict(resources)
        updated["diagnostic"] = dict(diagnostic) if diagnostic is not None else None
        if authorize_docker:
            updated["docker_mutation_authorized"] = True
        if authorize_deletion:
            updated["fixture_deletion_authorized"] = True
        if current["docker_mutation_authorized"] and not updated["docker_mutation_authorized"]:
            raise FixtureError("ownership manifest cannot revoke Docker mutation history")
        if current["fixture_deletion_authorized"] and not updated["fixture_deletion_authorized"]:
            raise FixtureError("ownership manifest cannot revoke fixture deletion history")
        self._validate_document(updated)
        encoded = (json.dumps(updated, sort_keys=True, separators=(",", ":")) + "\n").encode()
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{self.path.name}.",
            dir=self.path.parent,
        )
        temporary = Path(temporary_name)
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.path)
            fsync_directory(self.path.parent)
        finally:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
        self.document = updated


def docker_json(
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
    arguments: Sequence[str],
    context: str,
) -> Any:
    _, stdout, _ = run_capture(
        docker_command(docker, config, endpoint, *arguments),
        env=env,
        cwd=cwd,
        timeout=DOCKER_TIMEOUT,
        stage=context,
    )
    return parse_single_json(stdout, context)


def docker_lines(
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
    arguments: Sequence[str],
    context: str,
) -> list[str]:
    _, stdout, _ = run_capture(
        docker_command(docker, config, endpoint, *arguments),
        env=env,
        cwd=cwd,
        timeout=DOCKER_TIMEOUT,
        stage=context,
    )
    try:
        text = stdout.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise FixtureError(f"{context} returned invalid text") from exc
    values = [line.strip() for line in text.splitlines() if line.strip()]
    if any(CONTAINER_ID_PATTERN.fullmatch(value) is None for value in values):
        raise FixtureError(f"{context} returned an invalid resource identity")
    return values


def expected_config_files(repo_root: Path) -> set[str]:
    return {
        str(repo_root / "deploy" / "compose.yaml"),
        str(repo_root / "deploy" / "compose.managed.yaml"),
    }


def image_for_service(service: str, image_refs: Mapping[str, str]) -> str:
    if service in {"postgres", "postgres-bootstrap"}:
        return image_refs["postgres"]
    if service == "temporal-schema":
        return image_refs["temporal-admin-tools"]
    if service == "temporal":
        return image_refs["temporal-server"]
    if service.startswith("seaweed-"):
        return image_refs["seaweedfs"]
    if service == "caddy":
        return image_refs["caddy"]
    raise FixtureError("unknown fixture service")


def service_limits(service: str) -> tuple[int, int, int, bool, str]:
    values = {
        "postgres": (1_000_000_000, 768 << 20, 256, False, "unless-stopped"),
        "postgres-bootstrap": (500_000_000, 256 << 20, 128, False, "no"),
        "temporal-schema": (500_000_000, 384 << 20, 128, False, "no"),
        "temporal": (1_000_000_000, 1 << 30, 512, False, "unless-stopped"),
        "seaweed-master": (500_000_000, 256 << 20, 128, False, "unless-stopped"),
        "seaweed-volume": (750_000_000, 384 << 20, 192, False, "unless-stopped"),
        "seaweed-filer": (750_000_000, 512 << 20, 192, False, "unless-stopped"),
        "seaweed-s3-a": (500_000_000, 384 << 20, 160, False, "unless-stopped"),
        "seaweed-s3-b": (500_000_000, 384 << 20, 160, False, "unless-stopped"),
        "caddy": (500_000_000, 256 << 20, 128, True, "unless-stopped"),
    }
    return values[service]


def mount(source: Path | None, destination: str, read_write: bool, kind: str = "bind") -> tuple[str, str, bool, str]:
    return (str(source) if source is not None else "", destination, read_write, kind)


def expected_mounts(service: str, fixture_root: Path, repo_root: Path) -> set[tuple[str, str, bool, str]]:
    state = fixture_root / "state"
    data = state / "data"
    postgres = fixture_root / "secrets" / "operator" / "postgres"
    temporal = fixture_root / "secrets" / "operator" / "temporal"
    artifacts = fixture_root / "secrets" / "operator" / "artifacts"
    artifact_tls = {
        mount(artifacts / "ca.pem", "/run/secrets/artifact-ca.pem", False),
        mount(artifacts / "server.pem", "/run/secrets/artifact-server.pem", False),
        mount(artifacts / "server-key.pem", "/run/secrets/artifact-server-key.pem", False),
    }
    if service == "postgres":
        names = {
            "superuser-password",
            "pm-app-migrator-password",
            "pm-app-runtime-password",
            "temporal-migrator-password",
            "temporal-runtime-password",
            "temporal-visibility-migrator-password",
            "temporal-visibility-runtime-password",
            "objects-migrator-password",
            "objects-runtime-password",
            "denied-password",
            "server.pem",
            "server-key.pem",
        }
        return {mount(data / "postgres", "/var/lib/postgresql/data", True)} | {
            mount(postgres / name, f"/run/pm-stack-secrets/{name}", False) for name in names
        }
    if service == "postgres-bootstrap":
        names = {
            "superuser-password",
            "pm-app-migrator-password",
            "pm-app-runtime-password",
            "temporal-migrator-password",
            "temporal-runtime-password",
            "temporal-visibility-migrator-password",
            "temporal-visibility-runtime-password",
            "objects-migrator-password",
            "objects-runtime-password",
            "denied-password",
            "ca.pem",
        }
        return {mount(state / "bootstrap.sql", "/etc/pm-stack/bootstrap.sql", False)} | {
            mount(postgres / name, f"/run/pm-stack-secrets/{name}", False) for name in names
        }
    if service == "temporal-schema":
        return {
            mount(repo_root / "deploy" / "temporal" / "schema-init.sh", "/etc/pm-stack/schema-init.sh", False),
            mount(postgres / "temporal-migrator-password", "/run/pm-stack-secrets/temporal-migrator-password", False),
            mount(
                postgres / "temporal-visibility-migrator-password",
                "/run/pm-stack-secrets/temporal-visibility-migrator-password",
                False,
            ),
            mount(postgres / "ca.pem", "/run/secrets/postgres-ca.pem", False),
        }
    if service == "temporal":
        return {
            mount(state / "temporal.yaml", "/etc/temporal/pm-stack.yaml", False),
            mount(fixture_root / "temporal-dynamicconfig.yaml", "/etc/temporal/dynamicconfig.yaml", False),
            mount(postgres / "ca.pem", "/run/secrets/postgres-ca.pem", False),
            mount(temporal / "ca.pem", "/run/secrets/temporal-ca.pem", False),
            mount(temporal / "server.pem", "/run/secrets/temporal-server.pem", False),
            mount(temporal / "server-key.pem", "/run/secrets/temporal-server-key.pem", False),
            mount(temporal / "client.pem", "/run/secrets/temporal-client.pem", False),
            mount(temporal / "client-key.pem", "/run/secrets/temporal-client-key.pem", False),
        }
    if service == "seaweed-master":
        return {
            mount(data / service, "/data", True),
            mount(state / "security.toml", "/etc/seaweedfs/security.toml", False),
        } | artifact_tls
    if service == "seaweed-volume":
        return {
            mount(data / service, "/data", True),
            mount(state / "security.toml", "/etc/seaweedfs/security.toml", False),
        } | artifact_tls
    if service == "seaweed-filer":
        return {
            mount(data / service, "/data", True),
            mount(state / "filer.toml", "/etc/seaweedfs/filer.toml", False),
            mount(state / "security.toml", "/etc/seaweedfs/security.toml", False),
            mount(postgres / "ca.pem", "/run/secrets/postgres-ca.pem", False),
        } | artifact_tls
    if service in {"seaweed-s3-a", "seaweed-s3-b"}:
        return {
            mount(data / service, "/data", True),
            mount(state / "s3.json", "/etc/seaweedfs/s3.json", False),
            mount(state / "security.toml", "/etc/seaweedfs/security.toml", False),
        } | artifact_tls
    if service == "caddy":
        return {
            mount(repo_root / "deploy" / "caddy" / "Caddyfile", "/etc/caddy/Caddyfile", False),
        } | artifact_tls
    raise FixtureError("unknown fixture service mounts")


def actual_mounts(
    value: Any,
    *,
    allow_postgres_anonymous: bool = False,
) -> set[tuple[str, str, bool, str]]:
    if not isinstance(value, list):
        raise FixtureError("container mount inspection is invalid")
    result: set[tuple[str, str, bool, str]] = set()
    anonymous_volumes = 0
    for item in value:
        if not isinstance(item, dict):
            raise FixtureError("container mount inspection is invalid")
        kind = item.get("Type")
        source = item.get("Source", "")
        destination = item.get("Destination")
        read_write = item.get("RW")
        if kind == "volume" and allow_postgres_anonymous:
            name = item.get("Name")
            if (
                anonymous_volumes
                or not isinstance(name, str)
                or re.fullmatch(r"[0-9a-f]{64}", name) is None
                or item.get("Driver") != "local"
                or source != f"/var/lib/docker/volumes/{name}/_data"
                or destination != "/var/lib/postgresql/data"
                or read_write is not True
            ):
                raise FixtureError("one-off container has an unsafe anonymous volume")
            anonymous_volumes += 1
            continue
        if (
            kind not in {"bind", "tmpfs"}
            or not isinstance(source, str)
            or not isinstance(destination, str)
            or not isinstance(read_write, bool)
        ):
            raise FixtureError("container has an unsafe mount")
        if kind == "tmpfs":
            source = ""
        result.add((source, destination, read_write, kind))
    if len(result) + anonymous_volumes != len(value):
        raise FixtureError("container has duplicate mounts")
    return result


def expected_ports(service: str, environment: Mapping[str, str]) -> dict[str, list[dict[str, str]]]:
    mapping = {
        "postgres": ("5432/tcp", "PM_STACK_POSTGRES_PORT"),
        "temporal": ("7233/tcp", "PM_STACK_TEMPORAL_PORT"),
        "seaweed-s3-a": ("8333/tcp", "PM_STACK_S3_PRIMARY_PORT"),
        "seaweed-s3-b": ("8333/tcp", "PM_STACK_S3_SECONDARY_PORT"),
        "caddy": ("9443/tcp", "PM_STACK_PUBLIC_PORT"),
    }
    if service not in mapping:
        return {}
    container_port, environment_name = mapping[service]
    return {container_port: [{"HostIp": "127.0.0.1", "HostPort": environment[environment_name]}]}


def bool_label(value: Any) -> bool | None:
    if not isinstance(value, str):
        return None
    if value.lower() == "true":
        return True
    if value.lower() == "false":
        return False
    return None


def validate_stack_container(
    value: Any,
    project: str,
    fixture_root: Path,
    repo_root: Path,
    environment: Mapping[str, str],
    image_refs: Mapping[str, str],
    rootless: bool = False,
) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise FixtureError("container inspection is invalid")
    container_id = value.get("id")
    service = value.get("service")
    if not isinstance(container_id, str) or CONTAINER_ID_PATTERN.fullmatch(container_id) is None:
        raise FixtureError("container identity is invalid")
    if service not in ALL_SERVICES:
        raise FixtureError("project has a container with an unknown service identity")
    if value.get("project") != project:
        raise FixtureError("container project ownership mismatched")
    oneoff = bool_label(value.get("oneoff"))
    expected_oneoff = service in ONEOFF_SERVICES
    if oneoff is not expected_oneoff:
        raise FixtureError("container one-off ownership mismatched")
    expected_name = f"/{project}-{service}-1"
    name = value.get("name")
    if expected_oneoff:
        prefix = f"/{project}-{service}-run-"
        suffix = name[len(prefix) :] if isinstance(name, str) and name.startswith(prefix) else ""
        if SAFE_ONEOFF_SUFFIX.fullmatch(suffix) is None:
            raise FixtureError("one-off container name is unsafe")
    elif name != expected_name:
        raise FixtureError("container name does not match its owned service")
    config_files = value.get("config_files")
    if (
        not isinstance(config_files, str)
        or len(config_files.split(",")) != 2
        or set(config_files.split(",")) != expected_config_files(repo_root)
    ):
        raise FixtureError("container Compose configuration ownership mismatched")
    if value.get("working_dir") != str(repo_root):
        raise FixtureError("container Compose working-directory ownership mismatched")
    if value.get("image") != image_for_service(service, image_refs):
        raise FixtureError("container image is not the admitted pinned image")
    image_id = value.get("image_id")
    if not isinstance(image_id, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None:
        raise FixtureError("container image content identity is invalid")
    nano_cpus, memory, pids_limit, readonly, restart = service_limits(service)
    mapped_user = "0:0" if rootless else f"{os.getuid()}:{os.getgid()}"
    expected_user = "" if service in {"postgres", "postgres-bootstrap"} else mapped_user
    if (
        value.get("user") != expected_user
        or value.get("privileged") is not False
        or value.get("network_mode") != f"{project}_stack-private"
        or value.get("pid_mode") not in ("", None)
        or value.get("ipc_mode") not in ("", "private", None)
        or value.get("uts_mode") not in ("", None)
        or value.get("userns_mode") not in ("", None)
        or value.get("readonly_rootfs") is not readonly
        or value.get("auto_remove") is not expected_oneoff
        or value.get("init") is not True
        or value.get("nano_cpus") != nano_cpus
        or value.get("memory") != memory
        or value.get("pids_limit") != pids_limit
        or value.get("restart") != restart
        or value.get("cap_add") not in (None, [])
        or value.get("security_opt") not in (None, [])
        or value.get("devices") not in (None, [])
        or value.get("device_requests") not in (None, [])
    ):
        raise FixtureError("container privileges or resource bounds changed")
    wanted_ports = expected_ports(service, environment)
    allowed_ports = (wanted_ports,) if wanted_ports else ({}, None)
    if value.get("port_bindings") not in allowed_ports:
        raise FixtureError("container published ports changed")
    if actual_mounts(
        value.get("mounts"),
        allow_postgres_anonymous=service == "postgres-bootstrap",
    ) != expected_mounts(service, fixture_root, repo_root):
        raise FixtureError("container mounts changed or escaped the owned fixture")
    tmpfs = value.get("tmpfs")
    if service == "caddy":
        if not isinstance(tmpfs, dict) or set(tmpfs) != {"/tmp"}:
            raise FixtureError("Caddy tmpfs bound changed")
        options = tmpfs["/tmp"]
        if not isinstance(options, str):
            raise FixtureError("Caddy tmpfs options are invalid")
        option_set = set(options.split(","))
        if option_set not in (
            {"mode=1777", "size=33554432"},
            {"mode=1777", "size=32m"},
        ):
            raise FixtureError("Caddy tmpfs options changed")
    elif tmpfs not in (None, {}):
        raise FixtureError("container has an unexpected tmpfs")
    return {
        "id": container_id,
        "name": name,
        "service": service,
        "state": value.get("state"),
        "image_id": image_id,
        "oneoff": expected_oneoff,
    }


def validate_helper_container(
    value: Any,
    manifest: OwnershipManifest,
    image_refs: Mapping[str, str],
) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise FixtureError("cleanup helper inspection is invalid")
    container_id = value.get("id")
    if not isinstance(container_id, str) or CONTAINER_ID_PATTERN.fullmatch(container_id) is None:
        raise FixtureError("cleanup helper identity is invalid")
    postgres_data = manifest.fixture_root / "state" / "data" / "postgres"
    root_digest = hashlib.sha256(str(manifest.fixture_root).encode()).hexdigest()
    if (
        value.get("name") != f"/{manifest.project}-cleanup"
        or bool_label(value.get("helper")) is not True
        or value.get("helper_project") != manifest.project
        or value.get("helper_root") != root_digest
        or value.get("project") is not None
        or value.get("service") is not None
        or value.get("image") != image_refs["postgres"]
        or value.get("user") != "postgres"
        or value.get("privileged") is not False
        or value.get("network_mode") != "none"
        or value.get("readonly_rootfs") is not True
        or value.get("nano_cpus") != 250_000_000
        or value.get("memory") != 64 << 20
        or value.get("pids_limit") != 32
        or value.get("port_bindings") not in (None, {})
        or value.get("cap_add") not in (None, [])
        or value.get("pid_mode") not in ("", None)
        or value.get("ipc_mode") not in ("", "private", None)
        or value.get("uts_mode") not in ("", None)
        or value.get("userns_mode") not in ("", None)
        or value.get("init") not in (False, None)
        or value.get("auto_remove") is not False
        or value.get("restart") != "no"
        or value.get("devices") not in (None, [])
        or value.get("device_requests") not in (None, [])
        or value.get("tmpfs") not in (None, {})
    ):
        raise FixtureError("cleanup helper ownership or isolation changed")
    cap_drop = value.get("cap_drop")
    if not isinstance(cap_drop, list) or {str(item).upper() for item in cap_drop} != {"ALL"}:
        raise FixtureError("cleanup helper capabilities changed")
    security_opt = value.get("security_opt")
    if not isinstance(security_opt, list) or not any(str(item).startswith("no-new-privileges") for item in security_opt):
        raise FixtureError("cleanup helper privilege boundary changed")
    if actual_mounts(value.get("mounts")) != {mount(postgres_data, "/var/lib/postgresql/data", True)}:
        raise FixtureError("cleanup helper mount escaped the owned PostgreSQL data")
    image_id = value.get("image_id")
    if not isinstance(image_id, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None:
        raise FixtureError("cleanup helper image identity is invalid")
    return {
        "id": container_id,
        "name": value.get("name"),
        "state": value.get("state"),
        "image_id": image_id,
    }


def inspect_container(
    container_id: str,
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
) -> Any:
    return docker_json(
        docker,
        config,
        endpoint,
        env,
        cwd,
        ["container", "inspect", "--format", CONTAINER_FORMAT, container_id],
        "inspect owned container",
    )


def inspect_network(
    network_id: str,
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
) -> Any:
    return docker_json(
        docker,
        config,
        endpoint,
        env,
        cwd,
        ["network", "inspect", "--format", NETWORK_FORMAT, network_id],
        "inspect owned network",
    )


def list_container_ids(
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
    filters: Sequence[str],
) -> list[str]:
    arguments = ["container", "ls", "--all", "--quiet", "--no-trunc"]
    for value in filters:
        arguments.extend(("--filter", value))
    return docker_lines(docker, config, endpoint, env, cwd, arguments, "list fixture containers")


def list_network_ids(
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    cwd: Path,
    filters: Sequence[str],
) -> list[str]:
    arguments = ["network", "ls", "--quiet", "--no-trunc"]
    for value in filters:
        arguments.extend(("--filter", value))
    return docker_lines(docker, config, endpoint, env, cwd, arguments, "list fixture networks")


def discover_resources(
    manifest: OwnershipManifest,
    environment: Mapping[str, str],
    image_refs: Mapping[str, str],
    docker: Path,
    config: Path,
    endpoint: str,
    docker_env: Mapping[str, str],
    repo_root: Path,
) -> dict[str, Any]:
    container_ids = set(
        list_container_ids(
            docker,
            config,
            endpoint,
            docker_env,
            repo_root,
            [f"label=com.docker.compose.project={manifest.project}"],
        )
    )
    for service in PERSISTENT_SERVICES:
        container_ids.update(
            list_container_ids(
                docker,
                config,
                endpoint,
                docker_env,
                repo_root,
                [f"name=^/{re.escape(manifest.project + '-' + service + '-1')}$"],
            )
        )
    containers = [
        validate_stack_container(
            inspect_container(container_id, docker, config, endpoint, docker_env, repo_root),
            manifest.project,
            manifest.fixture_root,
            repo_root,
            environment,
            image_refs,
            rootless=manifest.daemon["rootless"],
        )
        for container_id in sorted(container_ids)
    ]
    helper_ids = list_container_ids(
        docker,
        config,
        endpoint,
        docker_env,
        repo_root,
        [f"name=^/{re.escape(manifest.project + '-cleanup')}$"],
    )
    if len(helper_ids) > 1:
        raise FixtureError("multiple cleanup helpers claim the fixture identity")
    helper = None
    if helper_ids:
        helper = validate_helper_container(
            inspect_container(helper_ids[0], docker, config, endpoint, docker_env, repo_root),
            manifest,
            image_refs,
        )
    network_ids = set(
        list_network_ids(
            docker,
            config,
            endpoint,
            docker_env,
            repo_root,
            [f"label=com.docker.compose.project={manifest.project}"],
        )
    )
    network_ids.update(
        list_network_ids(
            docker,
            config,
            endpoint,
            docker_env,
            repo_root,
            [f"name=^{re.escape(manifest.project + '_stack-private')}$"],
        )
    )
    if len(network_ids) > 1:
        raise FixtureError("multiple networks claim the fixture identity")
    network_record = None
    if network_ids:
        network_value = inspect_network(next(iter(network_ids)), docker, config, endpoint, docker_env, repo_root)
        if not isinstance(network_value, dict):
            raise FixtureError("network inspection is invalid")
        network_id = network_value.get("id")
        attached = network_value.get("containers")
        if (
            not isinstance(network_id, str)
            or CONTAINER_ID_PATTERN.fullmatch(network_id) is None
            or network_value.get("name") != f"{manifest.project}_stack-private"
            or network_value.get("driver") != "bridge"
            or network_value.get("scope") != "local"
            or network_value.get("internal") is not False
            or network_value.get("attachable") is not False
            or network_value.get("ingress") is not False
            or network_value.get("project") != manifest.project
            or network_value.get("network") != "stack-private"
            or bool_label(network_value.get("owner")) is not True
            or not isinstance(attached, dict)
        ):
            raise FixtureError("fixture network ownership or isolation changed")
        known_ids = {container["id"] for container in containers}
        # Stopped and not-yet-started owned containers need not be attached.
        if not set(attached).issubset(known_ids):
            raise FixtureError("fixture network has an unknown attached container")
        network_record = {"id": network_id, "name": network_value["name"], "containers": sorted(attached)}
    volume_ids = docker_lines(
        docker,
        config,
        endpoint,
        docker_env,
        repo_root,
        ["volume", "ls", "--quiet", "--filter", f"label=com.docker.compose.project={manifest.project}"],
        "list fixture volumes",
    )
    if volume_ids:
        raise FixtureError("fixture unexpectedly owns a named volume")
    return {"containers": containers, "network": network_record, "helper": helper}


def require_empty_before_start(
    manifest: OwnershipManifest,
    environment: Mapping[str, str],
    image_refs: Mapping[str, str],
    docker: Path,
    config: Path,
    endpoint: str,
    docker_env: Mapping[str, str],
    repo_root: Path,
) -> None:
    resources = discover_resources(
        manifest,
        environment,
        image_refs,
        docker,
        config,
        endpoint,
        docker_env,
        repo_root,
    )
    if resources["containers"] or resources["network"] is not None or resources["helper"] is not None:
        raise FixtureError("fixture project or resource name collided before startup")


def resource_manifest(resources: Mapping[str, Any]) -> dict[str, Any]:
    containers = [
        {
            "id": item["id"],
            "name": item["name"],
            "service": item["service"],
            "image_id": item["image_id"],
            "oneoff": item["oneoff"],
        }
        for item in resources["containers"]
    ]
    helper = resources.get("helper")
    helper_record = None
    if helper is not None:
        helper_record = {"id": helper["id"], "name": helper["name"], "image_id": helper["image_id"]}
    network = resources.get("network")
    network_record = None
    if network is not None:
        network_record = {"id": network["id"], "name": network["name"]}
    return {"containers": containers, "network": network_record, "helper": helper_record}


def verify_running_resources(resources: Mapping[str, Any]) -> None:
    containers = resources["containers"]
    services = [container["service"] for container in containers]
    if set(services) != PERSISTENT_SERVICES or len(services) != len(PERSISTENT_SERVICES):
        raise FixtureError("started fixture does not have exactly one container for every selected service")
    if any(container["state"] != "running" or container["oneoff"] for container in containers):
        raise FixtureError("started fixture has an unexpected container state")
    network = resources.get("network")
    if network is None or set(network["containers"]) != {container["id"] for container in containers}:
        raise FixtureError("started fixture network membership is incomplete")
    if resources.get("helper") is not None:
        raise FixtureError("cleanup helper exists during fixture execution")


def fixture_environment(base: Mapping[str, str], generated: Mapping[str, str], config: Path, endpoint: str) -> dict[str, str]:
    environment = dict(base)
    environment.update(generated)
    environment["DOCKER_CONFIG"] = str(config)
    environment["DOCKER_HOST"] = endpoint
    environment["COMPOSE_DISABLE_ENV_FILE"] = "true"
    environment["COMPOSE_ENV_FILES"] = ""
    return environment


def fixture_record(
    fixture_root: Path,
    environment: Mapping[str, str],
    environment_digest: str,
    config_digest: str,
    dynamic_config_digest: str,
) -> dict[str, Any]:
    return {
        "root_identity": path_identity(fixture_root),
        "environment": dict(environment),
        "environment_sha256": environment_digest,
        "config_sha256": config_digest,
        "rollover_namespace": environment["PM_STACK_TEST_ROLLOVER_NAMESPACE"],
        "temporal_dynamic_config_sha256": dynamic_config_digest,
    }


def validate_fixture_record(manifest: OwnershipManifest) -> dict[str, str]:
    fixture = manifest.document.get("fixture")
    if not isinstance(fixture, dict) or set(fixture) != {
        "root_identity",
        "environment",
        "environment_sha256",
        "config_sha256",
        "rollover_namespace",
        "temporal_dynamic_config_sha256",
    }:
        raise FixtureError("ownership manifest fixture identity is invalid")
    environment = fixture.get("environment")
    if not isinstance(environment, dict) or set(environment) != ENVIRONMENT_KEYS:
        raise FixtureError("ownership manifest environment is invalid")
    for name, value in environment.items():
        if not isinstance(name, str) or not isinstance(value, str) or not value or "\x00" in value:
            raise FixtureError("ownership manifest environment is unsafe")
    if environment["PM_STACK_PROJECT"] != manifest.project:
        raise FixtureError("ownership manifest project disagrees with its environment")
    require_exact_path(environment["PM_STACK_ROOT"], manifest.repo_root, "repository root")
    require_exact_path(environment["PM_STACK_SECRETS_DIR"], manifest.fixture_root / "secrets", "secret root")
    require_exact_path(environment["PM_STACK_STATE_DIR"], manifest.fixture_root / "state", "state root")
    require_exact_path(
        environment["PM_STACK_TEMPORAL_DYNAMIC_CONFIG"],
        manifest.fixture_root / "temporal-dynamicconfig.yaml",
        "Temporal dynamic configuration",
    )
    require_exact_path(environment["PM_STACK_TEST_CONFIG"], manifest.fixture_root / "server.json", "configuration")
    require_exact_path(
        environment["PM_STORE_TEST_SECRETS_DIR"],
        manifest.fixture_root / "secrets" / "operator" / "postgres",
        "store secret root",
    )
    rollover_namespace = environment["PM_STACK_TEST_ROLLOVER_NAMESPACE"]
    if (
        fixture["rollover_namespace"] != rollover_namespace
        or rollover_namespace != manifest.project + "-rollover"
        or TEMPORAL_NAMESPACE_PATTERN.fullmatch(rollover_namespace) is None
    ):
        raise FixtureError("ownership manifest rollover namespace is invalid")
    seen_ports: set[int] = set()
    for name in PORT_KEYS:
        try:
            port = int(environment[name])
        except ValueError as exc:
            raise FixtureError("ownership manifest contains an invalid fixture port") from exc
        if not 1024 <= port <= 65535 or str(port) != environment[name] or port in seen_ports:
            raise FixtureError("ownership manifest fixture ports are unsafe")
        seen_ports.add(port)
    parse_loopback_endpoint(
        environment["PM_STACK_TEST_S3_PRIMARY_ENDPOINT"],
        environment["PM_STACK_S3_PRIMARY_PORT"],
        "primary S3",
    )
    parse_loopback_endpoint(
        environment["PM_STACK_TEST_S3_SECONDARY_ENDPOINT"],
        environment["PM_STACK_S3_SECONDARY_PORT"],
        "secondary S3",
    )
    root_identity = fixture["root_identity"]
    if (
        not isinstance(root_identity, dict)
        or set(root_identity) != {"device", "inode", "uid", "mode"}
        or any(not isinstance(value, int) for value in root_identity.values())
    ):
        raise FixtureError("ownership manifest root identity is invalid")
    for name in ("environment_sha256", "config_sha256", "temporal_dynamic_config_sha256"):
        if not isinstance(fixture[name], str) or re.fullmatch(r"[0-9a-f]{64}", fixture[name]) is None:
            raise FixtureError("ownership manifest fixture digest is invalid")
    return environment


def cleanup_helper_name(manifest: OwnershipManifest) -> str:
    return f"{manifest.project}-cleanup"


def remove_exact_container(
    container_id: str,
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    repo_root: Path,
    errors: list[str],
    *,
    remove_volumes: bool = False,
) -> None:
    arguments = ["container", "rm"]
    if remove_volumes:
        arguments.append("--volumes")
    arguments.append(container_id)
    returncode = run_quiet(
        docker_command(docker, config, endpoint, *arguments),
        env=env,
        cwd=repo_root,
        timeout=DOCKER_TIMEOUT,
        stage="remove owned container",
        check=False,
    )
    if returncode != 0:
        errors.append("container-remove")


def stop_exact_containers(
    containers: Sequence[Mapping[str, Any]],
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    repo_root: Path,
    errors: list[str],
) -> None:
    running = [item["id"] for item in containers if item.get("state") in {"running", "paused", "restarting"}]
    if not running:
        return
    returncode = run_quiet(
        docker_command(docker, config, endpoint, "container", "stop", "--time", "45", *running),
        env=env,
        cwd=repo_root,
        timeout=STOP_TIMEOUT,
        stage="stop owned containers",
        check=False,
    )
    if returncode != 0:
        errors.append("container-stop")


def remove_existing_helper(
    helper: Mapping[str, Any] | None,
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    repo_root: Path,
    errors: list[str],
) -> None:
    if helper is None:
        return
    if helper.get("state") in {"running", "paused", "restarting"}:
        returncode = run_quiet(
            docker_command(docker, config, endpoint, "container", "stop", "--time", "10", helper["id"]),
            env=env,
            cwd=repo_root,
            timeout=DOCKER_TIMEOUT,
            stage="stop cleanup helper",
            check=False,
        )
        if returncode != 0:
            errors.append("helper-stop")
    remove_exact_container(helper["id"], docker, config, endpoint, env, repo_root, errors)


def postgres_data_has_entries(path: Path, fixture_root: Path) -> bool:
    if not os.path.lexists(path):
        return False
    try:
        relative = path.relative_to(fixture_root)
    except ValueError as exc:
        raise FixtureError("owned PostgreSQL data path escaped the fixture root") from exc
    current = fixture_root
    for component in relative.parts:
        current = current / component
        try:
            status = current.lstat()
        except OSError as exc:
            raise FixtureError("cannot inspect owned PostgreSQL data path") from exc
        if current.is_symlink() or not stat.S_ISDIR(status.st_mode):
            raise FixtureError("owned PostgreSQL data path is unsafe")
    try:
        with os.scandir(path) as entries:
            return next(entries, None) is not None
    except OSError as exc:
        if exc.errno in {13, 1}:
            return True
        raise FixtureError("cannot inspect owned PostgreSQL data path") from exc


def run_postgres_cleanup_helper(
    manifest: OwnershipManifest,
    image_refs: Mapping[str, str],
    docker: Path,
    config: Path,
    endpoint: str,
    env: Mapping[str, str],
    repo_root: Path,
    errors: list[str],
) -> None:
    postgres_data = manifest.fixture_root / "state" / "data" / "postgres"
    if not postgres_data_has_entries(postgres_data, manifest.fixture_root):
        return
    root_digest = hashlib.sha256(str(manifest.fixture_root).encode()).hexdigest()
    create_arguments = docker_command(
        docker,
        config,
        endpoint,
        "container",
        "create",
        "--name",
        cleanup_helper_name(manifest),
        "--label",
        "ai.polymetrics.pm-stack-fixture.cleanup=true",
        "--label",
        f"ai.polymetrics.pm-stack-fixture.project={manifest.project}",
        "--label",
        f"ai.polymetrics.pm-stack-fixture.root={root_digest}",
        "--network",
        "none",
        "--read-only",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--pids-limit",
        "32",
        "--memory",
        str(64 << 20),
        "--cpus",
        "0.25",
        "--user",
        "postgres",
        "--mount",
        f"type=bind,src={postgres_data},dst=/var/lib/postgresql/data",
        "--entrypoint",
        "/bin/sh",
        "--pull",
        "never",
        image_refs["postgres"],
        "-eu",
        "-c",
        "find /var/lib/postgresql/data -mindepth 1 -delete",
    )
    returncode, stdout, _ = run_capture(
        create_arguments,
        env=env,
        cwd=repo_root,
        timeout=DOCKER_TIMEOUT,
        stage="create bounded PostgreSQL cleanup helper",
        check=False,
    )
    if returncode != 0:
        errors.append("helper-create")
        return
    try:
        helper_id = stdout.decode("ascii").strip()
    except UnicodeDecodeError:
        errors.append("helper-create-identity")
        return
    if CONTAINER_ID_PATTERN.fullmatch(helper_id) is None:
        errors.append("helper-create-identity")
        return
    try:
        validate_helper_container(
            inspect_container(helper_id, docker, config, endpoint, env, repo_root),
            manifest,
            image_refs,
        )
    except FixtureError:
        errors.append("helper-ownership")
        return
    start_code = run_quiet(
        docker_command(docker, config, endpoint, "container", "start", "--attach", helper_id),
        env=env,
        cwd=repo_root,
        timeout=HELPER_TIMEOUT,
        stage="run bounded PostgreSQL cleanup helper",
        check=False,
    )
    if start_code != 0:
        errors.append("helper-run")
    remove_exact_container(helper_id, docker, config, endpoint, env, repo_root, errors)
    if start_code == 0:
        try:
            # The Linux runner can unlink this empty directory through its
            # owned parent without traversing PostgreSQL's uid-999 mode-0700 root.
            postgres_data.rmdir()
        except OSError:
            errors.append("postgres-directory-remove")


def remove_owned_parent(manifest: OwnershipManifest) -> None:
    parent = manifest.parent
    if not os.path.lexists(parent):
        return
    require_identity(parent, manifest.ownership["parent_identity"], "fixture parent")
    try:
        shutil.rmtree(parent)
    except OSError as exc:
        raise FixtureError("owned fixture files could not be removed") from exc
    if os.path.lexists(parent):
        raise FixtureError("owned fixture parent remains after cleanup")

def safe_diagnostic(code: str, resources: Mapping[str, Any] | None = None, errors: Sequence[str] = ()) -> dict[str, Any]:
    value: dict[str, Any] = {"code": code, "errors": sorted(set(errors))}
    if resources is not None:
        value["resources"] = resource_manifest(resources)
    return value

def verify_immutable_fixture_files(manifest: OwnershipManifest, environment: Mapping[str, str]) -> None:
    fixture = manifest.document["fixture"]
    environment_path = manifest.fixture_root / "environment.json"
    config_path = manifest.fixture_root / "server.json"
    dynamic_config_path = manifest.fixture_root / "temporal-dynamicconfig.yaml"
    if (
        sha256_file(environment_path, MAX_JSON_BYTES) != fixture["environment_sha256"]
        or decode_json_bytes(read_limited(environment_path), "fixture environment file") != environment
        or sha256_file(config_path, MAX_JSON_BYTES) != fixture["config_sha256"]
        or sha256_file(dynamic_config_path, MAX_JSON_BYTES) != fixture["temporal_dynamic_config_sha256"]
    ):
        raise FixtureError("fixture files changed after ownership was recorded")


def cleanup_fixture(
    manifest: OwnershipManifest,
    docker: Path,
    image_refs: Mapping[str, str],
    base_environment: Mapping[str, str],
) -> bool:
    if manifest.document["phase"] == "cleaned":
        return True
    parent_exists = os.path.lexists(manifest.parent)
    if parent_exists:
        try:
            require_identity(manifest.parent, manifest.ownership["parent_identity"], "fixture parent")
        except FixtureError:
            try:
                manifest.update("cleanup_failed", diagnostic=safe_diagnostic("parent-identity-mismatch"))
            except FixtureError:
                pass
            return False
    authorized = manifest.document["docker_mutation_authorized"]
    if not authorized:
        try:
            remove_owned_parent(manifest)
            manifest.update(
                "cleaned",
                resources={"containers": [], "network": None, "helper": None},
                diagnostic=None,
            )
            return True
        except FixtureError:
            try:
                manifest.update("cleanup_failed", diagnostic=safe_diagnostic("local-cleanup-failed"))
            except FixtureError:
                pass
            return False
    try:
        environment = validate_fixture_record(manifest)
    except FixtureError:
        try:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("fixture-record-mismatch"))
        except FixtureError:
            pass
        return False
    if parent_exists and os.path.lexists(manifest.fixture_root):
        fixture = manifest.document["fixture"]
        try:
            require_identity(manifest.fixture_root, fixture["root_identity"], "fixture root")
        except FixtureError:
            try:
                manifest.update("cleanup_failed", diagnostic=safe_diagnostic("root-identity-mismatch"))
            except FixtureError:
                pass
            return False
    if (
        parent_exists
        and os.path.lexists(manifest.fixture_root)
        and not manifest.document["fixture_deletion_authorized"]
    ):
        try:
            verify_immutable_fixture_files(manifest, environment)
        except FixtureError:
            try:
                manifest.update("cleanup_failed", diagnostic=safe_diagnostic("fixture-file-mismatch"))
            except FixtureError:
                pass
            return False
    temporary_config: tempfile.TemporaryDirectory[str] | None = None
    docker_config = manifest.parent / "docker-config"
    try:
        if os.path.lexists(docker_config):
            create_docker_config(docker_config)
        else:
            temporary_config = tempfile.TemporaryDirectory(prefix="pm-stack-cleanup-config-", dir=manifest.runner_temp)
            docker_config = Path(temporary_config.name)
            os.chmod(docker_config, 0o700)
            create_docker_config(docker_config)
        endpoint = manifest.daemon["endpoint"]
        docker_env = docker_environment(base_environment, docker_config, endpoint)
        verify_daemon_identity(manifest.daemon, docker, docker_config, endpoint, docker_env, manifest.repo_root)
        manifest.update("cleaning", diagnostic=None)
        try:
            resources = discover_resources(
                manifest,
                environment,
                image_refs,
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
            )
        except FixtureError:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("resource-ownership-mismatch"))
            return False
        manifest.update("cleaning", resources=resource_manifest(resources), diagnostic=None)
        errors: list[str] = []
        remove_existing_helper(
            resources.get("helper"),
            docker,
            docker_config,
            endpoint,
            docker_env,
            manifest.repo_root,
            errors,
        )
        stack_containers = resources["containers"]
        if stack_containers:
            pm_stack = manifest.repo_root / ".cache" / "bin" / "pm-stack"
            try:
                regular_executable(pm_stack, "pm-stack operator")
                validate_compose_cache(manifest.repo_root, base_environment)
                stack_env = fixture_environment(base_environment, environment, docker_config, endpoint)
                drain_code = run_quiet(
                    [
                        str(pm_stack),
                        "drain",
                        "--dependency",
                        "all",
                        "--config",
                        environment["PM_STACK_TEST_CONFIG"],
                    ],
                    env=stack_env,
                    cwd=manifest.repo_root,
                    timeout=DRAIN_TIMEOUT,
                    stage="drain owned fixture",
                    check=False,
                )
                if drain_code != 0:
                    errors.append("operator-drain")
            except FixtureError:
                errors.append("operator-drain")
            try:
                resources = discover_resources(
                    manifest,
                    environment,
                    image_refs,
                    docker,
                    docker_config,
                    endpoint,
                    docker_env,
                    manifest.repo_root,
                )
            except FixtureError:
                manifest.update("cleanup_failed", diagnostic=safe_diagnostic("post-drain-ownership-mismatch", errors=errors))
                return False
            stop_exact_containers(
                resources["containers"],
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
                errors,
            )
            for container in resources["containers"]:
                if (
                    container["oneoff"]
                    and container["state"] in {"running", "paused", "restarting", "removing"}
                ):
                    # AutoRemove owns removal once an active Compose one-off is stopped.
                    continue
                remove_exact_container(
                    container["id"],
                    docker,
                    docker_config,
                    endpoint,
                    docker_env,
                    manifest.repo_root,
                    errors,
                    remove_volumes=container["oneoff"],
                )
        try:
            remaining = discover_resources(
                manifest,
                environment,
                image_refs,
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
            )
        except FixtureError:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("post-container-ownership-mismatch", errors=errors))
            return False
        if remaining["containers"] or remaining["helper"] is not None:
            manifest.update(
                "cleanup_failed",
                resources=resource_manifest(remaining),
                diagnostic=safe_diagnostic("owned-containers-remain", remaining, errors),
            )
            return False
        if remaining["network"] is not None:
            network = remaining["network"]
            if network["containers"]:
                manifest.update(
                    "cleanup_failed",
                    resources=resource_manifest(remaining),
                    diagnostic=safe_diagnostic("owned-network-not-empty", remaining, errors),
                )
                return False
            network_code = run_quiet(
                docker_command(docker, docker_config, endpoint, "network", "rm", network["id"]),
                env=docker_env,
                cwd=manifest.repo_root,
                timeout=DOCKER_TIMEOUT,
                stage="remove owned network",
                check=False,
            )
            if network_code != 0:
                errors.append("network-remove")
        try:
            final_resources = discover_resources(
                manifest,
                environment,
                image_refs,
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
            )
        except FixtureError:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("final-resource-ownership-mismatch", errors=errors))
            return False
        if final_resources["containers"] or final_resources["network"] is not None or final_resources["helper"] is not None:
            manifest.update(
                "cleanup_failed",
                resources=resource_manifest(final_resources),
                diagnostic=safe_diagnostic("owned-resources-remain", final_resources, errors),
            )
            return False
        if os.path.lexists(manifest.parent):
            run_postgres_cleanup_helper(
                manifest,
                image_refs,
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
                errors,
            )
        try:
            post_helper = discover_resources(
                manifest,
                environment,
                image_refs,
                docker,
                docker_config,
                endpoint,
                docker_env,
                manifest.repo_root,
            )
        except FixtureError:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("post-helper-ownership-mismatch", errors=errors))
            return False
        if post_helper["containers"] or post_helper["network"] is not None or post_helper["helper"] is not None:
            manifest.update(
                "cleanup_failed",
                resources=resource_manifest(post_helper),
                diagnostic=safe_diagnostic("cleanup-helper-remains", post_helper, errors),
            )
            return False
        if errors:
            manifest.update(
                "cleanup_failed",
                resources=resource_manifest(post_helper),
                diagnostic=safe_diagnostic("cleanup-command-failed", post_helper, errors),
            )
            return False
        manifest.update(
            "cleaning",
            resources={"containers": [], "network": None, "helper": None},
            diagnostic=None,
            authorize_deletion=True,
        )
        remove_owned_parent(manifest)
        manifest.update(
            "cleaned",
            resources={"containers": [], "network": None, "helper": None},
            diagnostic=None,
        )
        return True
    except FixtureError:
        try:
            manifest.update("cleanup_failed", diagnostic=safe_diagnostic("cleanup-failed"))
        except FixtureError:
            pass
        return False
    finally:
        if temporary_config is not None:
            temporary_config.cleanup()


def parse_cli(arguments: Sequence[str]) -> tuple[str, str, list[str]]:
    if list(arguments) in (["-h"], ["--help"]):
        print(
            "usage: with-stack-fixture.py --manifest ABSOLUTE_PATH -- COMMAND [ARGS...]\n"
            "       with-stack-fixture.py --cleanup ABSOLUTE_PATH"
        )
        raise SystemExit(0)
    if len(arguments) == 2 and arguments[0] == "--cleanup":
        return "cleanup", arguments[1], []
    if len(arguments) >= 4 and arguments[0] == "--manifest" and arguments[2] == "--":
        command = list(arguments[3:])
        if not command or not command[0]:
            raise FixtureError("requested command is required after --")
        return "run", arguments[1], command
    raise FixtureError(
        "usage: with-stack-fixture.py --manifest ABSOLUTE_PATH -- COMMAND [ARGS...] "
        "or with-stack-fixture.py --cleanup ABSOLUTE_PATH"
    )

def resolve_repo_root() -> Path:
    try:
        repo_root = Path(__file__).resolve(strict=True).parents[2]
        current = Path.cwd().resolve(strict=True)
    except OSError as exc:
        raise FixtureError("cannot resolve the assigned checkout") from exc
    if current != repo_root:
        raise FixtureError("wrapper must run from the assigned checkout root")
    return repo_root


def resolve_docker(environment: Mapping[str, str]) -> Path:
    path_value = environment.get("PATH", "")
    selected = shutil.which("docker", path=path_value)
    if selected is None:
        raise FixtureError("the existing Docker CLI is unavailable")
    try:
        resolved = Path(selected).resolve(strict=True)
        status = resolved.stat()
    except OSError as exc:
        raise FixtureError("the existing Docker CLI is unavailable") from exc
    if not stat.S_ISREG(status.st_mode) or not os.access(resolved, os.X_OK):
        raise FixtureError("the existing Docker CLI is not an executable regular file")
    return resolved


def create_private_parent(parent: Path) -> dict[str, int]:
    try:
        parent.mkdir(mode=0o700)
    except FileExistsError as exc:
        raise FixtureError("unique fixture parent collided before ownership was recorded") from exc
    except OSError as exc:
        raise FixtureError("cannot create private fixture parent") from exc
    identity = path_identity(parent)
    if identity["uid"] != os.getuid() or identity["mode"] & 0o077:
        try:
            parent.rmdir()
        except OSError:
            pass
        raise FixtureError("fixture parent is not private to the current job user")
    return identity


def cleanup_mode(
    manifest_path: Path,
    runner_temp: Path,
    repo_root: Path,
    run_id: str,
    run_attempt: str,
    base_environment: Mapping[str, str],
) -> int:
    if not os.path.lexists(manifest_path):
        return 0
    manifest = OwnershipManifest.load(manifest_path, runner_temp, repo_root, run_id, run_attempt)
    if manifest.document["phase"] == "cleaned":
        return 0
    docker = resolve_docker(base_environment)
    image_refs = load_image_refs(repo_root)
    return 0 if cleanup_fixture(manifest, docker, image_refs, base_environment) else 1


def run_mode(
    manifest_path: Path,
    runner_temp: Path,
    repo_root: Path,
    run_id: str,
    run_attempt: str,
    command: Sequence[str],
    base_environment: Mapping[str, str],
) -> int:
    pm_stack = regular_executable(repo_root / ".cache" / "bin" / "pm-stack", "pm-stack operator")
    generator = regular_executable(repo_root / ".cache" / "bin" / "pm-stack-fixture", "fixture generator")
    docker = resolve_docker(base_environment)
    validate_compose_cache(repo_root, base_environment)
    image_refs = load_image_refs(repo_root)
    socket_path, socket_identity = choose_docker_socket()
    endpoint = "unix://" + str(socket_path)
    token = secrets.token_hex(10)
    project = PROJECT_PREFIX + token
    parent = runner_temp / f"{PARENT_PREFIX}{token}"
    fixture_root = parent / "fixture"
    docker_config = parent / "docker-config"
    manifest: OwnershipManifest | None = None
    created_parent_identity: dict[str, int] | None = None
    reservations: list[socket.socket] = []
    result = 1
    try:
        created_parent_identity = create_private_parent(parent)
        create_docker_config(docker_config)
        docker_env = docker_environment(base_environment, docker_config, endpoint)
        daemon = inspect_daemon(docker, docker_config, endpoint, docker_env, repo_root, socket_identity)
        manifest = OwnershipManifest.create(
            manifest_path,
            runner_temp,
            repo_root,
            run_id,
            run_attempt,
            token,
            parent,
            fixture_root,
            project,
            daemon,
        )
        generator_code, stdout, _ = run_capture(
            [
                str(generator),
                "--root",
                str(fixture_root),
                "--project",
                project,
                "--repo-root",
                str(repo_root),
            ],
            env=base_environment,
            cwd=repo_root,
            timeout=GENERATOR_TIMEOUT,
            stage="generate private stack fixture",
            max_output=MAX_GENERATOR_OUTPUT,
            check=False,
        )
        if generator_code != 0:
            raise CommandFailure("generate private stack fixture", generator_code)
        environment, environment_digest, config_digest, dynamic_config_digest = validate_generated_environment(
            stdout,
            fixture_root,
            repo_root,
            project,
        )
        record = fixture_record(
            fixture_root,
            environment,
            environment_digest,
            config_digest,
            dynamic_config_digest,
        )
        manifest.update("generated", fixture=record)
        reservations = reserve_ports(environment)
        require_empty_before_start(
            manifest,
            environment,
            image_refs,
            docker,
            docker_config,
            endpoint,
            docker_env,
            repo_root,
        )
        manifest.update("starting", fixture=record, authorize_docker=True)
        close_reservations(reservations)
        stack_env = fixture_environment(base_environment, environment, docker_config, endpoint)
        run_quiet(
            [str(pm_stack), "up", "--config", environment["PM_STACK_TEST_CONFIG"]],
            env=stack_env,
            cwd=repo_root,
            timeout=STARTUP_TIMEOUT,
            stage="start owned fixture",
        )
        resources = discover_resources(
            manifest,
            environment,
            image_refs,
            docker,
            docker_config,
            endpoint,
            docker_env,
            repo_root,
        )
        verify_running_resources(resources)
        manifest.update("running", resources=resource_manifest(resources), diagnostic=None)
        result = run_child(command, env=stack_env, cwd=repo_root)
    except Cancellation as cancelled:
        result = 128 + cancelled.signum
    except FixtureError as exc:
        safe_error(str(exc))
        result = 1
    finally:
        SIGNALS.enter_cleanup()
        close_reservations(reservations)
        if manifest is None:
            if created_parent_identity is not None:
                try:
                    require_identity(parent, created_parent_identity, "new fixture parent")
                    shutil.rmtree(parent)
                except (FixtureError, OSError):
                    safe_error("pre-manifest fixture cleanup refused or failed")
                    result = 1
        else:
            cleaned = cleanup_fixture(manifest, docker, image_refs, base_environment)
            if not cleaned:
                safe_error(f"cleanup incomplete; safe ownership diagnostic retained at {manifest.path}")
                if result == 0:
                    result = 1
        if SIGNALS.first_signal is not None and result == 0:
            result = 128 + SIGNALS.first_signal
    return result


def main(arguments: Sequence[str] | None = None) -> int:
    try:
        mode, raw_manifest, command = parse_cli(sys.argv[1:] if arguments is None else arguments)
        runner_temp = resolve_runner_temp()
        repo_root = resolve_repo_root()
        manifest_path, run_id, run_attempt = validate_manifest_path(raw_manifest, runner_temp)
        base_environment = sanitized_environment()
        SIGNALS.install()
        try:
            if mode == "cleanup":
                SIGNALS.enter_cleanup()
                result = cleanup_mode(
                    manifest_path,
                    runner_temp,
                    repo_root,
                    run_id,
                    run_attempt,
                    base_environment,
                )
                if SIGNALS.first_signal is not None and result == 0:
                    return 128 + SIGNALS.first_signal
                return result
            return run_mode(
                manifest_path,
                runner_temp,
                repo_root,
                run_id,
                run_attempt,
                command,
                base_environment,
            )
        finally:
            SIGNALS.restore()
    except FixtureError as exc:
        safe_error(str(exc))
        return 1
    except OSError:
        safe_error("local operating-system operation failed")
        return 1
    except Cancellation as cancelled:
        return 128 + cancelled.signum


if __name__ == "__main__":
    raise SystemExit(main())
