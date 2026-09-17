#!/bin/sh
# Verify and install a supplied PM release payload without network or service actions.
set -eu

usage() {
  code=${1:-2}
  printf '%s\n' 'usage: scripts/install.sh --manifest FILE --source DIR --prefix DIR [--check]' >&2
  exit "$code"
}

manifest=
source_root=
prefix=
check_only=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --manifest)
      [ "$#" -ge 2 ] || usage
      [ -z "$manifest" ] || usage
      manifest=$2
      shift 2
      ;;
    --source)
      [ "$#" -ge 2 ] || usage
      [ -z "$source_root" ] || usage
      source_root=$2
      shift 2
      ;;
    --prefix)
      [ "$#" -ge 2 ] || usage
      [ -z "$prefix" ] || usage
      prefix=$2
      shift 2
      ;;
    --check)
      [ "$check_only" -eq 0 ] || usage
      check_only=1
      shift
      ;;
    --help|-h)
      usage 0
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$manifest" ] || usage
[ -n "$source_root" ] || usage
[ -n "$prefix" ] || usage

exec python3 - "$manifest" "$source_root" "$prefix" "$check_only" <<'PY'
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import sys
import tempfile

MAX_METADATA_BYTES = 4 << 20
RELEASE_FIELDS = {
    "schema_version", "release_id", "backend_architecture", "protocol_major",
    "required_features", "artifacts",
}
ARTIFACT_FIELDS = {
    "role", "path", "version", "sha256", "media_type", "executable", "platforms",
}
MANIFEST_FIELDS = {
    "schema_version", "release_id", "arrangement", "platform", "release_pair", "files",
}
FILE_FIELDS = {"source", "destination", "sha256", "mode", "role"}
ROLES = {
    "cli", "api", "worker", "query_helper", "public_schema", "server_config_schema",
    "stack_launcher", "server_config_example", "license", "notice", "image_lock",
    "connector_bundle", "migration",
}
EXECUTABLE_ROLES = {"cli", "api", "worker", "query_helper", "stack_launcher"}
BACKEND_ROLES = {
    "api", "worker", "query_helper", "public_schema", "server_config_schema",
    "stack_launcher", "server_config_example",
}
CLIENT_ROLES = {"cli", "public_schema", "license", "notice"}
IDENTIFIER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
DIGEST = re.compile(r"^[0-9a-f]{64}$")
PLATFORM = re.compile(r"^(?:any|[a-z0-9]+-[a-z0-9_]+)$")
MEDIA_TYPE = re.compile(r"^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$")


def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON field: {key}")
        result[key] = value
    return result


def reject_constant(_):
    raise ValueError("non-standard JSON number")


def enforce_json_depth(value):
    stack = [(value, 1)]
    while stack:
        current, depth = stack.pop()
        if depth > 128:
            raise ValueError("JSON nesting exceeds the depth bound")
        if isinstance(current, dict):
            stack.extend((child, depth + 1) for child in current.values())
        elif isinstance(current, list):
            stack.extend((child, depth + 1) for child in current)




def regular_file(path, label):
    try:
        metadata = path.lstat()
    except OSError as error:
        raise ValueError(f"unavailable {label}") from error
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        raise ValueError(f"{label} is not a regular non-symlink file")


def load_json(path, label, expected_digest=None):
    regular_file(path, label)
    try:
        with path.open("rb") as source:
            data = source.read(MAX_METADATA_BYTES + 1)
    except OSError as error:
        raise ValueError(f"cannot read {label}") from error
    if len(data) > MAX_METADATA_BYTES:
        raise ValueError(f"{label} exceeds the metadata size bound")
    if expected_digest is not None and hashlib.sha256(data).hexdigest() != expected_digest:
        raise ValueError(f"{label} digest mismatch")
    try:
        value = json.loads(
            data,
            object_pairs_hook=reject_duplicates,
            parse_constant=reject_constant,
        )
    except (UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise ValueError(f"invalid {label}") from error
    enforce_json_depth(value)
    return value


def exact_fields(value, expected, label):
    if not isinstance(value, dict) or set(value) != expected:
        raise ValueError(f"{label} has missing or unknown fields")


def identifier(value, label):
    if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
        raise ValueError(f"invalid {label}")
    return value


def digest_value(value, label):
    if not isinstance(value, str) or not DIGEST.fullmatch(value):
        raise ValueError(f"invalid {label}")
    return value


def relative_path(value, label):
    if (
        not isinstance(value, str)
        or not value
        or value in {".", ".."}
        or len(value) > 4096
        or "\\" in value
    ):
        raise ValueError(f"invalid {label}")
    path = PurePosixPath(value)
    if (
        path.is_absolute()
        or path.as_posix() != value
        or any(part in {"", ".", ".."} for part in path.parts)
    ):
        raise ValueError(f"invalid {label}")
    return path


def unique_strings(value, label, pattern=IDENTIFIER, maximum=4096, item_maximum=128):
    if not isinstance(value, list) or len(value) > maximum:
        raise ValueError(f"invalid {label}")
    if any(not isinstance(item, str) or len(item) > item_maximum or not pattern.fullmatch(item) for item in value):
        raise ValueError(f"invalid {label}")
    if len(set(value)) != len(value):
        raise ValueError(f"duplicate {label}")
    return value


def sha256(path):
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1 << 20), b""):
                digest.update(block)
    except OSError as error:
        raise ValueError("cannot read release payload file") from error
    return digest.hexdigest()


def confined(root, relative, label):
    target = root.joinpath(*relative.parts)
    try:
        target.resolve(strict=False).relative_to(root)
    except (OSError, ValueError) as error:
        raise ValueError(f"{label} escapes its root") from error
    return target


def validate_release_pair(value):
    exact_fields(value, RELEASE_FIELDS, "release pair")
    if value["schema_version"] != "pm.release-pair/v1":
        raise ValueError("unsupported release pair schema")
    release_id = identifier(value["release_id"], "release pair release_id")
    if value["backend_architecture"] != "postgresql-temporal-s3":
        raise ValueError("unsupported backend architecture")
    if type(value["protocol_major"]) is not int or not 1 <= value["protocol_major"] <= 2147483647:
        raise ValueError("invalid release protocol major")
    unique_strings(value["required_features"], "required features")
    artifacts = value["artifacts"]
    if not isinstance(artifacts, list) or not 1 <= len(artifacts) <= 4096:
        raise ValueError("invalid release artifact inventory")
    by_path = {}
    for index, artifact in enumerate(artifacts):
        label = f"release artifact {index}"
        exact_fields(artifact, ARTIFACT_FIELDS, label)
        role = artifact["role"]
        if not isinstance(role, str) or role not in ROLES:
            raise ValueError(f"invalid {label} role")
        path = relative_path(artifact["path"], f"{label} path").as_posix()
        if path in by_path:
            raise ValueError("duplicate release artifact path")
        if not isinstance(artifact["version"], str) or not 1 <= len(artifact["version"]) <= 128:
            raise ValueError(f"invalid {label} version")
        digest_value(artifact["sha256"], f"{label} digest")
        if not isinstance(artifact["media_type"], str) or not 1 <= len(artifact["media_type"]) <= 255 or not MEDIA_TYPE.fullmatch(artifact["media_type"]):
            raise ValueError(f"invalid {label} media type")
        if type(artifact["executable"]) is not bool:
            raise ValueError(f"invalid {label} executable flag")
        platforms = unique_strings(artifact["platforms"], f"{label} platforms", PLATFORM, 32, 64)
        if not platforms:
            raise ValueError(f"missing {label} platforms")
        by_path[path] = artifact
    return release_id, by_path


def validate_manifest(value, release_id, release_artifacts):
    exact_fields(value, MANIFEST_FIELDS, "install manifest")
    if value["schema_version"] != "pm.install-manifest/v1":
        raise ValueError("unsupported install manifest schema")
    if identifier(value["release_id"], "install release_id") != release_id:
        raise ValueError("install and release identities differ")
    arrangement = value["arrangement"]
    if not isinstance(arrangement, str) or arrangement not in {"client-only", "backend-only", "full"}:
        raise ValueError("invalid package arrangement")
    target_platform = value["platform"]
    if not isinstance(target_platform, str) or len(target_platform) > 64 or PLATFORM.fullmatch(target_platform) is None:
        raise ValueError("invalid target platform")
    pair = value["release_pair"]
    exact_fields(pair, {"path", "sha256"}, "release pair reference")
    relative_path(pair["path"], "release pair reference path")
    digest_value(pair["sha256"], "release pair reference digest")
    files = value["files"]
    if not isinstance(files, list) or not 1 <= len(files) <= 4096:
        raise ValueError("invalid install file inventory")
    sources = set()
    destinations = set()
    roles = set()
    normalized = []
    for index, entry in enumerate(files):
        label = f"install file {index}"
        exact_fields(entry, FILE_FIELDS, label)
        source = relative_path(entry["source"], f"{label} source")
        destination = relative_path(entry["destination"], f"{label} destination")
        source_text = source.as_posix()
        destination_text = destination.as_posix()
        if source_text in sources or destination_text in destinations:
            raise ValueError("duplicate install source or destination")
        sources.add(source_text)
        destinations.add(destination_text)
        expected_digest = digest_value(entry["sha256"], f"{label} digest")
        mode = entry["mode"]
        if not isinstance(mode, str) or mode not in {"0644", "0755"}:
            raise ValueError(f"invalid {label} mode")
        role = entry["role"]
        if not isinstance(role, str) or role not in ROLES:
            raise ValueError(f"invalid {label} role")
        if arrangement == "client-only" and role not in CLIENT_ROLES:
            raise ValueError("client-only package contains a backend artifact")
        if arrangement == "backend-only" and role == "cli":
            raise ValueError("backend-only package contains a client artifact")
        if role in EXECUTABLE_ROLES and mode != "0755":
            raise ValueError(f"executable {label} does not use mode 0755")
        if role not in EXECUTABLE_ROLES and mode != "0644":
            raise ValueError(f"non-executable {label} does not use mode 0644")
        release_artifact = release_artifacts.get(source_text)
        if release_artifact is None or release_artifact["role"] != role or release_artifact["sha256"] != expected_digest:
            raise ValueError(f"{label} is not bound to the release pair")
        if release_artifact["executable"] != (mode == "0755"):
            raise ValueError(f"{label} executable metadata differs from the release pair")
        if "any" not in release_artifact["platforms"] and target_platform not in release_artifact["platforms"]:
            raise ValueError(f"{label} does not support the target platform")
        roles.add(role)
        normalized.append((source, destination, expected_digest, int(mode, 8)))
    for destination in destinations:
        if any(parent.as_posix() in destinations for parent in PurePosixPath(destination).parents):
            raise ValueError("install destinations have a file/directory conflict")
    required = {"cli"} if arrangement == "client-only" else set(BACKEND_ROLES)
    if arrangement == "full":
        required.add("cli")
    if not required <= roles:
        raise ValueError("install manifest omits required package roles")
    return arrangement, pair, normalized


def ensure_no_symlink_chain(root, target):
    current = root
    relative = target.relative_to(root)
    for part in relative.parts[:-1]:
        current = current / part
        if current.is_symlink():
            raise ValueError("path traverses a symlink")


manifest_arg = Path(sys.argv[1])
source_arg = Path(sys.argv[2])
prefix_arg = Path(sys.argv[3])
check_only = sys.argv[4] == "1"
if not source_arg.is_dir() or source_arg.is_symlink():
    raise SystemExit("install: source must be a non-symlink directory")
try:
    source_root = source_arg.resolve(strict=True)
    regular_file(manifest_arg, "install manifest")
    manifest_path = manifest_arg.resolve(strict=True)
    try:
        manifest_path.relative_to(source_root)
    except ValueError as error:
        raise ValueError("manifest must be inside the source directory") from error
    manifest = load_json(manifest_path, "install manifest")
    exact_fields(manifest, MANIFEST_FIELDS, "install manifest")
    pair_reference = manifest["release_pair"]
    exact_fields(pair_reference, {"path", "sha256"}, "release pair reference")
    pair_relative = relative_path(pair_reference["path"], "release pair reference path")
    pair_path = confined(source_root, pair_relative, "release pair")
    ensure_no_symlink_chain(source_root, pair_path)
    pair_digest = digest_value(pair_reference["sha256"], "release pair reference digest")
    pair = load_json(pair_path, "release pair", pair_digest)
    release_id, release_artifacts = validate_release_pair(pair)
    arrangement, _, files = validate_manifest(manifest, release_id, release_artifacts)
    for source_relative, _, expected_digest, _ in files:
        source_path = confined(source_root, source_relative, "release payload")
        ensure_no_symlink_chain(source_root, source_path)
        regular_file(source_path, "release payload file")
        if sha256(source_path) != expected_digest:
            raise ValueError("release payload digest mismatch")
except (OSError, ValueError) as error:
    raise SystemExit(f"install: {error}") from error

if prefix_arg.is_symlink():
    raise SystemExit("install: prefix must not be a symlink")
prefix = prefix_arg.resolve(strict=False)
if prefix.exists() and not prefix.is_dir():
    raise SystemExit("install: prefix must be a directory")

def prepare_install():
    install_plan = []
    for source_relative, destination_relative, expected_digest, mode in files:
        source_path = confined(source_root, source_relative, "release payload")
        destination = confined(prefix, destination_relative, "install destination")
        ensure_no_symlink_chain(prefix, destination)
        if destination.exists() or destination.is_symlink():
            regular_file(destination, "existing install destination")
            if sha256(destination) != expected_digest:
                raise ValueError("refusing to replace a different existing file")
        install_plan.append((source_path, destination, expected_digest, mode))
    return install_plan


def install_files(install_plan):
    os.umask(0o022)
    prefix.mkdir(parents=True, exist_ok=True)
    for source_path, destination, expected_digest, mode in install_plan:
        ensure_no_symlink_chain(prefix, destination)
        destination.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
        if destination.exists() or destination.is_symlink():
            regular_file(destination, "existing install destination")
            if sha256(destination) != expected_digest:
                raise ValueError("refusing to replace a different existing file")
            destination.chmod(mode)
            continue
        temporary_name = None
        try:
            with tempfile.NamedTemporaryFile(mode="wb", dir=destination.parent, prefix=".pm-install-", delete=False) as output:
                temporary_name = output.name
                with source_path.open("rb") as source:
                    shutil.copyfileobj(source, output, length=1 << 20)
                output.flush()
                os.fsync(output.fileno())
            if sha256(Path(temporary_name)) != expected_digest:
                raise ValueError("release payload changed during installation")
            os.chmod(temporary_name, mode)
            os.link(temporary_name, destination)
            os.unlink(temporary_name)
            temporary_name = None
        finally:
            if temporary_name is not None:
                try:
                    os.unlink(temporary_name)
                except FileNotFoundError:
                    pass


try:
    install_plan = prepare_install()
    if check_only:
        print(f"verified {arrangement} release {release_id} ({len(files)} files); no files or services changed")
        raise SystemExit(0)
    install_files(install_plan)
except (OSError, ValueError) as error:
    raise SystemExit(f"install: {error}") from error
print(f"installed {arrangement} release {release_id} ({len(files)} files); services were not started")
PY
