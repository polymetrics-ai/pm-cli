#!/usr/bin/env python3
"""Resolve and execute the repository's exact, digest-pinned build tools."""

from __future__ import annotations

import argparse
import contextlib
from dataclasses import dataclass
try:
    import fcntl
except ImportError:  # pragma: no cover - reached only on a refused non-POSIX host
    fcntl = None  # type: ignore[assignment]
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform as host_platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from typing import BinaryIO, Iterator, Mapping, Sequence
import urllib.parse
import urllib.request
import zipfile


ROOT = Path(__file__).resolve().parents[1]
LOCK_PATH = ROOT / "build" / "toolchains.lock.json"
DEFAULT_CACHE = ROOT / ".cache" / "pm-tools"
SUPPORTED_PLATFORMS = {
    ("darwin", "arm64"): "darwin-arm64",
    ("darwin", "aarch64"): "darwin-arm64",
    ("linux", "amd64"): "linux-amd64",
    ("linux", "x86_64"): "linux-amd64",
    ("linux", "arm64"): "linux-arm64",
    ("linux", "aarch64"): "linux-arm64",
}
REQUIRED_ASSETS = {"bazel", "bazelisk", "clang", "go", "syft"}
REQUIRED_MODULES = {"gazelle", "rules_go"}
SUPPORTED_KINDS = {"binary", "tar.gz", "tar.xz", "zip"}
MAX_LOCK_BYTES = 4 * 1024 * 1024
MAX_DOWNLOAD_BYTES = 2_300_000_000
MAX_ARCHIVE_ENTRIES = 200_000
MAX_ARCHIVE_MEMBER_BYTES = 4_000_000_000
MAX_ARCHIVE_TOTAL_BYTES = 12_000_000_000
CHUNK_BYTES = 1024 * 1024
BAZEL_BASE_URL = "https://github.com/bazelbuild/bazel/releases/download"
BAZEL_VERSION = "9.2.0"
EXPECTED_ASSET_VERSIONS = {
    "bazel": "9.2.0",
    "bazelisk": "1.29.0",
    "clang": "21.1.8",
    "go": "1.27.1",
    "syft": "1.51.1",
}
EXPECTED_MODULE_VERSIONS = {
    "gazelle": "0.54.0",
    "rules_go": "0.63.0",
}
HEX_SHA256 = re.compile(r"[0-9a-f]{64}\Z")
SAFE_ID = re.compile(r"[a-z][a-z0-9-]*\Z")
SAFE_VERSION = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]*\Z")
NATIVE_ENVIRONMENT_PATH = ROOT / "build" / "native-environment.json"
NATIVE_ENVIRONMENT_SCHEMA = "pm.native-environment/v1"
NATIVE_QUALIFICATION_SCOPE = "bound-development-host-not-immutable-image-or-release"
LINUX_NATIVE_PLATFORMS = {"linux-amd64", "linux-arm64"}
MAX_NATIVE_PROFILE_BYTES = 1024 * 1024
MAX_NATIVE_TREES = 32
MAX_NATIVE_FILES = 128
MAX_NATIVE_TREE_ENTRIES = 1_000_000
MAX_NATIVE_TREE_BYTES = 32_000_000_000
MAX_NATIVE_FILE_BYTES = 4_000_000_000
MAX_NATIVE_SYMLINK_TRAVERSALS = 64
MAX_NATIVE_SYMLINK_BYTES = 4096
NATIVE_OS_IDENTITY_PATHS = {"/etc/os-release", "/usr/lib/os-release"}
# Application archives remain pinned by go.sum and the derived Bazel go_deps
# repositories. They are not host sysroot files and must not be required here.
NATIVE_REQUIRED_FILE_NAMES = {
    "libgcc_s.so.1",
    "libstdc++.so.6",
}
LINUX_NATIVE_GO_TARGETS = {
    "linux-amd64": ("linux", "amd64", "x86_64-unknown-linux-gnu"),
    "linux-arm64": ("linux", "arm64", "aarch64-unknown-linux-gnu"),
}
LINUX_NATIVE_CGO_OVERRIDES = {
    "CGO_CFLAGS",
    "CGO_CFLAGS_ALLOW",
    "CGO_CFLAGS_DISALLOW",
    "CGO_CPPFLAGS",
    "CGO_CXXFLAGS",
    "CGO_FFLAGS",
    "CGO_LDFLAGS",
    "CGO_LDFLAGS_ALLOW",
    "CGO_LDFLAGS_DISALLOW",
}
LINUX_NATIVE_AMBIENT_OVERRIDES = {
    "CCC_OVERRIDE_OPTIONS",
    "CC_FOR_TARGET",
    "C_INCLUDE_PATH",
    "CFLAGS",
    "COMPILER_PATH",
    "CPATH",
    "CPLUS_INCLUDE_PATH",
    "CPPFLAGS",
    "CXXFLAGS",
    "CXX_FOR_TARGET",
    "GCC_EXEC_PREFIX",
    "LD",
    "LDFLAGS",
    "LD_LIBRARY_PATH",
    "LIBRARY_PATH",
    "OBJC_INCLUDE_PATH",
    "PKG_CONFIG_LIBDIR",
    "GOARCH",
    "GOENV",
    "GOOS",
    "PKG_CONFIG_PATH",
    "PKG_CONFIG_SYSROOT_DIR",
    "SDKROOT",
    "SYSROOT",
    "CC",
    "CXX",
    "CGO_CFLAGS_ALLOW",
    "CGO_CFLAGS_DISALLOW",
    "CGO_CFLAGS",
    "CGO_CPPFLAGS",
    "CGO_FFLAGS",
    "CGO_CXXFLAGS",
    "CGO_LDFLAGS",
    "CGO_LDFLAGS_ALLOW",
    "CGO_LDFLAGS_DISALLOW",
}


class BootstrapError(RuntimeError):
    """A closed failure while selecting, fetching, or verifying a tool."""


def _reject_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise BootstrapError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def _read_json(path: Path, *, max_bytes: int = MAX_LOCK_BYTES) -> object:
    try:
        size = path.stat().st_size
    except OSError as exc:
        raise BootstrapError(f"cannot stat {path}: {exc}") from exc
    if size > max_bytes:
        raise BootstrapError(f"JSON file is too large: {path}")
    try:
        text = path.read_text(encoding="utf-8")
        return json.loads(text, object_pairs_hook=_reject_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise BootstrapError(f"cannot read strict JSON from {path}: {exc}") from exc


def _require_object(value: object, context: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise BootstrapError(f"{context} must be an object")
    return value


def _require_string(value: object, context: str) -> str:
    if not isinstance(value, str) or not value:
        raise BootstrapError(f"{context} must be a non-empty string")
    return value


def _require_keys(
    value: Mapping[str, object], required: set[str], optional: set[str], context: str
) -> None:
    keys = set(value)
    missing = required - keys
    extra = keys - required - optional
    if missing:
        raise BootstrapError(f"{context} is missing keys: {', '.join(sorted(missing))}")
    if extra:
        raise BootstrapError(f"{context} has unknown keys: {', '.join(sorted(extra))}")


def _require_https(url: str, context: str) -> None:
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not parsed.netloc or parsed.username or parsed.password:
        raise BootstrapError(f"{context} must be an HTTPS URL without credentials")


def _safe_archive_path(name: str, context: str = "archive path") -> PurePosixPath:
    if not name or "\x00" in name or "\\" in name:
        raise BootstrapError(f"unsafe {context}: {name!r}")
    candidate = name[:-1] if name.endswith("/") else name
    if not candidate:
        raise BootstrapError(f"unsafe {context}: {name!r}")
    raw_parts = candidate.split("/")
    if any(part in {"", ".", ".."} for part in raw_parts):
        raise BootstrapError(f"unsafe {context}: {name!r}")
    path = PurePosixPath(candidate)
    if path.is_absolute() or (path.parts and path.parts[0].endswith(":")):
        raise BootstrapError(f"unsafe {context}: {name!r}")
    return path


def _validate_lock(raw: object) -> dict[str, object]:
    lock = _require_object(raw, "toolchain lock")
    _require_keys(
        lock,
        {"schema_version", "go_version", "assets", "modules", "images"},
        set(),
        "toolchain lock",
    )
    if lock["schema_version"] != "pm.toolchains/v1":
        raise BootstrapError("unsupported toolchain lock schema")
    if lock["go_version"] != "1.27.1":
        raise BootstrapError("toolchain lock must select Go 1.27.1")

    assets = lock["assets"]
    if not isinstance(assets, list):
        raise BootstrapError("toolchain lock assets must be an array")
    seen_assets: set[tuple[str, str]] = set()
    seen_asset_ids: set[str] = set()
    for index, item in enumerate(assets):
        context = f"assets[{index}]"
        asset = _require_object(item, context)
        _require_keys(
            asset,
            {"id", "version", "platform", "url", "sha256", "kind", "executable"},
            {"attestation"},
            context,
        )
        asset_id = _require_string(asset["id"], f"{context}.id")
        version = _require_string(asset["version"], f"{context}.version")
        selected_platform = _require_string(asset["platform"], f"{context}.platform")
        url = _require_string(asset["url"], f"{context}.url")
        digest = _require_string(asset["sha256"], f"{context}.sha256")
        kind = _require_string(asset["kind"], f"{context}.kind")
        executable = _require_string(asset["executable"], f"{context}.executable")
        if not SAFE_ID.fullmatch(asset_id) or asset_id not in REQUIRED_ASSETS:
            raise BootstrapError(f"unsupported {context}.id: {asset_id}")
        if not SAFE_VERSION.fullmatch(version):
            raise BootstrapError(f"unsafe {context}.version: {version}")
        if version != EXPECTED_ASSET_VERSIONS[asset_id]:
            raise BootstrapError(
                f"{context}.version must be {EXPECTED_ASSET_VERSIONS[asset_id]}"
            )
        if selected_platform not in set(SUPPORTED_PLATFORMS.values()):
            raise BootstrapError(f"unsupported {context}.platform: {selected_platform}")
        _require_https(url, f"{context}.url")
        if not HEX_SHA256.fullmatch(digest):
            raise BootstrapError(f"{context}.sha256 must be lowercase hexadecimal SHA-256")
        if kind not in SUPPORTED_KINDS:
            raise BootstrapError(f"unsupported {context}.kind: {kind}")
        _safe_archive_path(executable, f"{context}.executable")
        identity = (asset_id, selected_platform)
        if identity in seen_assets:
            raise BootstrapError(f"duplicate asset identity: {asset_id}/{selected_platform}")
        seen_assets.add(identity)
        seen_asset_ids.add(asset_id)
        attestation = asset.get("attestation")
        if attestation is not None:
            record = _require_object(attestation, f"{context}.attestation")
            _require_keys(record, {"url", "sha256"}, set(), f"{context}.attestation")
            attestation_url = _require_string(record["url"], f"{context}.attestation.url")
            attestation_digest = _require_string(
                record["sha256"], f"{context}.attestation.sha256"
            )
            _require_https(attestation_url, f"{context}.attestation.url")
            if not HEX_SHA256.fullmatch(attestation_digest):
                raise BootstrapError(
                    f"{context}.attestation.sha256 must be lowercase hexadecimal SHA-256"
                )
    expected_assets = {
        (asset_id, selected_platform)
        for asset_id in REQUIRED_ASSETS
        for selected_platform in {"darwin-arm64", "linux-amd64", "linux-arm64"}
    }
    if seen_assets != expected_assets:
        missing = sorted(expected_assets - seen_assets)
        extra = sorted(seen_assets - expected_assets)
        raise BootstrapError(f"toolchain lock asset matrix mismatch; missing={missing}, extra={extra}")
    if seen_asset_ids != REQUIRED_ASSETS:
        raise BootstrapError("toolchain lock does not contain the required tool identities")

    modules = lock["modules"]
    if not isinstance(modules, list):
        raise BootstrapError("toolchain lock modules must be an array")
    seen_modules: set[str] = set()
    for index, item in enumerate(modules):
        context = f"modules[{index}]"
        module = _require_object(item, context)
        _require_keys(module, {"name", "version", "integrity", "url"}, set(), context)
        name = _require_string(module["name"], f"{context}.name")
        version = _require_string(module["version"], f"{context}.version")
        if name not in EXPECTED_MODULE_VERSIONS:
            raise BootstrapError(f"unsupported Bazel module identity: {name}")
        if version != EXPECTED_MODULE_VERSIONS[name]:
            raise BootstrapError(
                f"{context}.version must be {EXPECTED_MODULE_VERSIONS[name]}"
            )
        integrity = _require_string(module["integrity"], f"{context}.integrity")
        url = _require_string(module["url"], f"{context}.url")
        if name in seen_modules:
            raise BootstrapError(f"duplicate module identity: {name}")
        if not integrity.startswith("sha256-"):
            raise BootstrapError(f"{context}.integrity must be an SRI SHA-256 value")
        _require_https(url, f"{context}.url")
        seen_modules.add(name)
    if seen_modules != REQUIRED_MODULES:
        raise BootstrapError("toolchain lock must contain exactly rules_go and Gazelle modules")

    images = lock["images"]
    if not isinstance(images, list):
        raise BootstrapError("toolchain lock images must be an array")
    seen_images: set[str] = set()
    for index, item in enumerate(images):
        context = f"images[{index}]"
        image = _require_object(item, context)
        _require_keys(image, {"name", "version", "index_digest", "source_url"}, set(), context)
        name = _require_string(image["name"], f"{context}.name")
        _require_string(image["version"], f"{context}.version")
        digest = _require_string(image["index_digest"], f"{context}.index_digest")
        source_url = _require_string(image["source_url"], f"{context}.source_url")
        if name in seen_images:
            raise BootstrapError(f"duplicate image identity: {name}")
        if not digest.startswith("sha256:") or not HEX_SHA256.fullmatch(digest[7:]):
            raise BootstrapError(f"{context}.index_digest must be a SHA-256 image digest")
        _require_https(source_url, f"{context}.source_url")
        seen_images.add(name)
    return lock


def _load_lock(path: Path = LOCK_PATH) -> dict[str, object]:
    return _validate_lock(_read_json(path))


def _normalize_platform(system: str, machine: str) -> str:
    key = (system.strip().lower(), machine.strip().lower())
    try:
        return SUPPORTED_PLATFORMS[key]
    except KeyError as exc:
        raise BootstrapError(
            f"unsupported target platform: operating_system={system!r}, architecture={machine!r}"
        ) from exc


def _selected_platform() -> str:
    return _normalize_platform(host_platform.system(), host_platform.machine())


def _cache_root() -> Path:
    configured = os.environ.get("PM_TOOLS_CACHE")
    cache = Path(configured).expanduser() if configured else DEFAULT_CACHE
    try:
        cache = cache.resolve(strict=False)
    except OSError as exc:
        raise BootstrapError(f"cannot resolve tools cache path {cache}: {exc}") from exc
    temporary_roots = {Path("/tmp"), Path("/private/tmp")}
    if any(cache == root or root in cache.parents for root in temporary_roots):
        raise BootstrapError("PM tools cache must be persistent and cannot be under /tmp")
    try:
        cache.mkdir(parents=True, exist_ok=True, mode=0o700)
    except OSError as exc:
        raise BootstrapError(f"cannot create tools cache {cache}: {exc}") from exc
    if not cache.is_dir():
        raise BootstrapError(f"tools cache is not a directory: {cache}")
    return cache


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(CHUNK_BYTES), b""):
                digest.update(chunk)
    except OSError as exc:
        raise BootstrapError(f"cannot hash {path}: {exc}") from exc
    return digest.hexdigest()


def _verify_sha256(path: Path, expected: str) -> None:
    actual = _sha256_file(path)
    if actual != expected:
        raise BootstrapError(f"SHA-256 mismatch for {path}: expected {expected}, got {actual}")


def _copy_bounded(
    source: BinaryIO,
    destination: BinaryIO,
    *,
    limit: int,
    expected: int | None = None,
) -> int:
    copied = 0
    while True:
        chunk = source.read(CHUNK_BYTES)
        if not chunk:
            break
        copied += len(chunk)
        if copied > limit:
            raise BootstrapError(f"stream exceeds the {limit}-byte safety limit")
        destination.write(chunk)
    if expected is not None and copied != expected:
        raise BootstrapError(f"stream length mismatch: expected {expected}, got {copied}")
    return copied


def _inside(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
        return True
    except ValueError:
        return False


def _require_bounded_integer(value: object, context: str, maximum: int) -> int:
    if type(value) is not int or value < 0 or value > maximum:
        raise BootstrapError(f"{context} must be an integer from 0 through {maximum}")
    return value


def _native_path(
    value: object, context: str, *, allow_root: bool = False
) -> PurePosixPath:
    text = _require_string(value, context)
    if "\x00" in text or "\\" in text or not text.startswith("/"):
        raise BootstrapError(f"{context} must be an absolute POSIX path")
    if text != "/" and (
        text.endswith("/")
        or any(part in {"", ".", ".."} for part in text.split("/")[1:])
    ):
        raise BootstrapError(f"{context} must be a canonical absolute POSIX path")
    path = PurePosixPath(text)
    if str(path) != text or (path == PurePosixPath("/") and not allow_root):
        raise BootstrapError(f"{context} must be a canonical absolute POSIX path")
    return path


@contextlib.contextmanager
def _native_root(root: Path) -> Iterator[Path]:
    if not hasattr(os, "O_DIRECTORY"):
        raise BootstrapError("safe native root opening is unavailable on this host")
    flags = os.O_RDONLY | os.O_DIRECTORY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    try:
        descriptor = os.open(root, flags)
    except OSError as exc:
        raise BootstrapError(f"cannot open native environment root {root}: {exc}") from exc
    try:
        status = os.fstat(descriptor)
        if not stat.S_ISDIR(status.st_mode):
            raise BootstrapError(f"native environment root is not a directory: {root}")
        proc_descriptor = Path(f"/proc/self/fd/{descriptor}")
        opened = (
            proc_descriptor
            if proc_descriptor.exists()
            else root.expanduser().absolute()
        )
        yield opened
        try:
            final_status = os.stat(root)
        except OSError as exc:
            raise BootstrapError(
                f"cannot re-open native environment root {root}: {exc}"
            ) from exc
        if (
            final_status.st_dev != status.st_dev
            or final_status.st_ino != status.st_ino
        ):
            raise BootstrapError(
                f"native environment root changed during capture: {root}"
            )
    finally:
        os.close(descriptor)


def _native_link_target_parts(
    parent: Sequence[str], target: str, context: str
) -> list[str]:
    if not target or "\x00" in target:
        raise BootstrapError(f"{context} has an empty or invalid symlink target")
    encoded = os.fsencode(target)
    if len(encoded) > MAX_NATIVE_SYMLINK_BYTES:
        raise BootstrapError(f"{context} symlink target exceeds the safety limit")
    parts = [] if target.startswith("/") else list(parent)
    for part in target.split("/"):
        if part in {"", "."}:
            continue
        if part == "..":
            if not parts:
                raise BootstrapError(f"{context} symlink target escapes the supplied root")
            parts.pop()
        else:
            parts.append(part)
    return parts


def _resolve_native_path(
    root: Path, path: PurePosixPath, context: str
) -> tuple[Path, os.stat_result, list[tuple[str, str]]]:
    remaining = list(path.parts[1:])
    resolved_parts: list[str] = []
    links: list[tuple[str, str]] = []
    while remaining:
        part = remaining.pop(0)
        candidate = root.joinpath(*resolved_parts, part)
        try:
            status = os.lstat(candidate)
        except OSError as exc:
            raise BootstrapError(f"cannot inspect {context} {path}: {exc}") from exc
        if stat.S_ISLNK(status.st_mode):
            if len(links) >= MAX_NATIVE_SYMLINK_TRAVERSALS:
                raise BootstrapError(f"{context} {path} exceeds the symlink traversal limit")
            try:
                target = os.readlink(candidate)
            except OSError as exc:
                raise BootstrapError(f"cannot read {context} symlink {candidate}: {exc}") from exc
            location = "/" + "/".join((*resolved_parts, part))
            links.append((location, target))
            remaining = (
                _native_link_target_parts(
                    resolved_parts, target, f"{context} {location}"
                )
                + remaining
            )
            resolved_parts = []
            continue
        if remaining and not stat.S_ISDIR(status.st_mode):
            raise BootstrapError(f"{context} path component is not a directory: {candidate}")
        resolved_parts.append(part)

    resolved = root.joinpath(*resolved_parts)
    try:
        status = os.lstat(resolved)
    except OSError as exc:
        raise BootstrapError(f"cannot inspect {context} {path}: {exc}") from exc
    if stat.S_ISLNK(status.st_mode):
        raise BootstrapError(f"unresolved symlink in {context}: {path}")
    return resolved, status, links


def _fingerprint_field(digest: object, value: bytes) -> None:
    if len(value) >= 1 << 64:
        raise BootstrapError("native fingerprint field exceeds the safety limit")
    digest.update(len(value).to_bytes(8, "big"))  # type: ignore[attr-defined]
    digest.update(value)  # type: ignore[attr-defined]


def _fingerprint_links(
    digest: object, links: Sequence[tuple[str, str]]
) -> None:
    for location, target in links:
        _fingerprint_field(digest, b"L")
        _fingerprint_field(digest, os.fsencode(location))
        _fingerprint_field(digest, os.fsencode(target))


def _hash_native_regular_file(
    path: Path,
    expected_status: os.stat_result,
    digest: object,
    *,
    limit: int,
    context: str,
) -> int:
    if not stat.S_ISREG(expected_status.st_mode):
        raise BootstrapError(f"{context} is not a regular file: {path}")
    if expected_status.st_size < 0 or expected_status.st_size > limit:
        raise BootstrapError(f"{context} exceeds the {limit}-byte safety limit: {path}")
    if not hasattr(os, "O_NOFOLLOW"):
        raise BootstrapError("safe native file opening is unavailable on this host")
    flags = os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise BootstrapError(f"cannot open {context} {path}: {exc}") from exc
    try:
        opened_status = os.fstat(descriptor)
        expected_identity = (
            expected_status.st_dev,
            expected_status.st_ino,
            expected_status.st_size,
        )
        opened_identity = (
            opened_status.st_dev,
            opened_status.st_ino,
            opened_status.st_size,
        )
        if not stat.S_ISREG(opened_status.st_mode) or opened_identity != expected_identity:
            raise BootstrapError(f"{context} changed before it could be opened: {path}")
        _fingerprint_field(digest, opened_status.st_size.to_bytes(8, "big"))
        total = 0
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            while True:
                chunk = stream.read(CHUNK_BYTES)
                if not chunk:
                    break
                total += len(chunk)
                if total > limit:
                    raise BootstrapError(f"{context} exceeds the safety limit: {path}")
                digest.update(chunk)  # type: ignore[attr-defined]
        final_status = os.fstat(descriptor)
        final_identity = (
            final_status.st_dev,
            final_status.st_ino,
            final_status.st_size,
            final_status.st_mtime_ns,
        )
        initial_identity = (
            opened_status.st_dev,
            opened_status.st_ino,
            opened_status.st_size,
            opened_status.st_mtime_ns,
        )
        if total != opened_status.st_size or final_identity != initial_identity:
            raise BootstrapError(f"{context} changed while it was being read: {path}")
        return total
    except OSError as exc:
        raise BootstrapError(f"cannot hash {context} {path}: {exc}") from exc
    finally:
        os.close(descriptor)


def _scan_native_directory(
    path: Path, expected_status: os.stat_result, context: str
) -> list[tuple[str, os.stat_result, str | None]]:
    if not stat.S_ISDIR(expected_status.st_mode):
        raise BootstrapError(f"{context} is not a directory: {path}")
    if not hasattr(os, "O_DIRECTORY") or not hasattr(os, "O_NOFOLLOW"):
        raise BootstrapError("safe native directory traversal is unavailable on this host")
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise BootstrapError(f"cannot open {context} {path}: {exc}") from exc
    try:
        opened_status = os.fstat(descriptor)
        if (
            not stat.S_ISDIR(opened_status.st_mode)
            or opened_status.st_dev != expected_status.st_dev
            or opened_status.st_ino != expected_status.st_ino
        ):
            raise BootstrapError(f"{context} changed before traversal: {path}")
        entries: list[tuple[str, os.stat_result, str | None]] = []
        with os.scandir(descriptor) as iterator:
            for entry in iterator:
                if len(entries) >= MAX_NATIVE_TREE_ENTRIES:
                    raise BootstrapError(f"{context} contains too many entries")
                status = entry.stat(follow_symlinks=False)
                target = (
                    os.readlink(entry.name, dir_fd=descriptor)
                    if stat.S_ISLNK(status.st_mode)
                    else None
                )
                entries.append((entry.name, status, target))
        entries.sort(key=lambda entry: os.fsencode(entry[0]))
        return entries
    except OSError as exc:
        raise BootstrapError(f"cannot traverse {context} {path}: {exc}") from exc
    finally:
        os.close(descriptor)


def _fingerprint_native_file(root: Path, path: PurePosixPath) -> dict[str, object]:
    resolved, status, links = _resolve_native_path(root, path, "native file")
    digest = hashlib.sha256()
    digest.update(b"pm.native-file/v1\0")
    _fingerprint_field(digest, os.fsencode(str(path)))
    _fingerprint_links(digest, links)
    _fingerprint_field(digest, b"F")
    byte_count = _hash_native_regular_file(
        resolved,
        status,
        digest,
        limit=MAX_NATIVE_FILE_BYTES,
        context=f"native file {path}",
    )
    final_resolved, final_status, final_links = _resolve_native_path(
        root, path, "native file"
    )
    if (
        final_resolved != resolved
        or final_links != links
        or final_status.st_dev != status.st_dev
        or final_status.st_ino != status.st_ino
    ):
        raise BootstrapError(f"native file changed during capture: {path}")
    return {"path": str(path), "sha256": digest.hexdigest(), "byte_count": byte_count}


def _fingerprint_native_tree(
    root: Path,
    path: PurePosixPath,
    admitted_trees: Sequence[Path],
    admitted_files: set[Path],
) -> dict[str, object]:
    resolved, status, root_links = _resolve_native_path(root, path, "native tree")
    if not stat.S_ISDIR(status.st_mode):
        raise BootstrapError(f"native tree is not a directory: {path}")
    digest = hashlib.sha256()
    digest.update(b"pm.native-tree/v1\0")
    _fingerprint_field(digest, os.fsencode(str(path)))
    _fingerprint_links(digest, root_links)
    entry_count = 0
    total_bytes = 0
    symlink_traversals = len(root_links)
    pending: list[tuple[Path, tuple[str, ...], os.stat_result]] = [
        (resolved, (), status)
    ]
    while pending:
        directory, relative_parent, directory_status = pending.pop()
        children: list[tuple[Path, tuple[str, ...], os.stat_result]] = []
        for name, child_status, target in _scan_native_directory(
            directory, directory_status, f"native tree {path}"
        ):
            entry_count += 1
            if entry_count > MAX_NATIVE_TREE_ENTRIES:
                raise BootstrapError(f"native tree has too many entries: {path}")
            relative_parts = (*relative_parent, name)
            relative = b"/".join(os.fsencode(part) for part in relative_parts)
            child = directory / name
            if stat.S_ISDIR(child_status.st_mode):
                _fingerprint_field(digest, b"D")
                _fingerprint_field(digest, relative)
                children.append((child, relative_parts, child_status))
            elif stat.S_ISREG(child_status.st_mode):
                _fingerprint_field(digest, b"F")
                _fingerprint_field(digest, relative)
                remaining = MAX_NATIVE_TREE_BYTES - total_bytes
                size = _hash_native_regular_file(
                    child,
                    child_status,
                    digest,
                    limit=remaining,
                    context=f"native tree file {path}/{os.fsdecode(relative)}",
                )
                total_bytes += size
            elif stat.S_ISLNK(child_status.st_mode):
                if target is None:
                    raise BootstrapError(f"cannot read native tree symlink: {child}")
                _fingerprint_field(digest, b"L")
                _fingerprint_field(digest, relative)
                _fingerprint_field(digest, os.fsencode(target))
                logical = PurePosixPath(str(path), *relative_parts)
                target_path, target_status, traversed = _resolve_native_path(
                    root, logical, "native tree symlink"
                )
                _fingerprint_links(digest, traversed)
                if target_path not in admitted_files and not any(
                    target_path.is_relative_to(tree) for tree in admitted_trees
                ):
                    raise BootstrapError(
                        f"native tree symlink target is not profiled: {logical}"
                    )
                symlink_traversals += len(traversed)
                if symlink_traversals > MAX_NATIVE_TREE_ENTRIES:
                    raise BootstrapError(
                        f"native tree exceeds the symlink traversal limit: {path}"
                    )
                if not (
                    stat.S_ISREG(target_status.st_mode)
                    or stat.S_ISDIR(target_status.st_mode)
                ):
                    raise BootstrapError(
                        f"native tree symlink targets a special file: {logical}"
                    )
            else:
                raise BootstrapError(f"native tree contains a special file: {child}")
        pending.extend(reversed(children))
    final_resolved, final_status, final_links = _resolve_native_path(
        root, path, "native tree"
    )
    if (
        final_resolved != resolved
        or final_links != root_links
        or final_status.st_dev != status.st_dev
        or final_status.st_ino != status.st_ino
    ):
        raise BootstrapError(f"native tree changed during capture: {path}")
    return {
        "path": str(path),
        "sha256": digest.hexdigest(),
        "entry_count": entry_count,
        "total_bytes": total_bytes,
    }


def _validate_native_linker(
    linker: object,
    platform: str | None = None,
    *,
    toolchain_lock: Mapping[str, object] | None = None,
) -> dict[str, object]:
    record = _require_object(linker, "native environment linker")
    _require_keys(
        record,
        {"asset_id", "executable", "source_sha256"},
        set(),
        "native environment linker",
    )
    asset_id = _require_string(record["asset_id"], "native environment linker.asset_id")
    executable_text = _require_string(
        record["executable"], "native environment linker.executable"
    )
    source_sha256 = _require_string(
        record["source_sha256"], "native environment linker.source_sha256"
    )
    executable = _safe_archive_path(
        executable_text, "native environment linker executable"
    )
    if asset_id != "clang":
        raise BootstrapError("native environment linker asset_id must be clang")
    if executable.name != "ld.lld" or executable.parent.name != "bin":
        raise BootstrapError(
            "native environment linker must be the selected Clang archive's bin/ld.lld"
        )
    if not HEX_SHA256.fullmatch(source_sha256):
        raise BootstrapError(
            "native environment linker.source_sha256 must be lowercase hexadecimal SHA-256"
        )
    normalized = {
        "asset_id": asset_id,
        "executable": executable_text,
        "source_sha256": source_sha256,
    }
    if platform is not None:
        lock = (
            _load_lock()
            if toolchain_lock is None
            else _validate_lock(dict(toolchain_lock))
        )
        asset = _find_asset(lock, "clang", platform)
        expected = {
            "asset_id": "clang",
            "executable": str(_clang_linker_relative(asset)),
            "source_sha256": asset["sha256"],
        }
        if normalized != expected:
            raise BootstrapError(
                "native environment linker does not match the locked Clang source"
            )
    return normalized


def _validate_native_environment_profile(
    profile: object,
    *,
    toolchain_lock: Mapping[str, object] | None = None,
) -> dict[str, object]:
    value = _require_object(profile, "native environment profile")
    _require_keys(
        value,
        {
            "schema_version",
            "qualification_scope",
            "platform",
            "sysroot",
            "gcc_install_dir",
            "trees",
            "files",
            "linker",
        },
        set(),
        "native environment profile",
    )
    if value["schema_version"] != NATIVE_ENVIRONMENT_SCHEMA:
        raise BootstrapError("unsupported native environment profile schema")
    if value["qualification_scope"] != NATIVE_QUALIFICATION_SCOPE:
        raise BootstrapError("unsupported native environment qualification scope")
    platform = _require_string(value["platform"], "native environment platform")
    if platform not in LINUX_NATIVE_PLATFORMS:
        raise BootstrapError(f"unsupported native environment platform: {platform}")
    if value["sysroot"] != "/":
        raise BootstrapError("native environment sysroot must be /")
    raw_gcc = value["gcc_install_dir"]
    gcc_install_dir: str | None
    if raw_gcc is None:
        gcc_install_dir = None
    else:
        gcc_install_dir = str(
            _native_path(raw_gcc, "native environment gcc_install_dir")
        )

    raw_trees = value["trees"]
    if not isinstance(raw_trees, list) or not 1 <= len(raw_trees) <= MAX_NATIVE_TREES:
        raise BootstrapError(
            f"native environment trees must contain 1 through {MAX_NATIVE_TREES} records"
        )
    trees: list[dict[str, object]] = []
    for index, item in enumerate(raw_trees):
        context = f"native environment trees[{index}]"
        record = _require_object(item, context)
        _require_keys(
            record,
            {"path", "sha256", "entry_count", "total_bytes"},
            set(),
            context,
        )
        tree_path = str(_native_path(record["path"], f"{context}.path"))
        digest = _require_string(record["sha256"], f"{context}.sha256")
        if not HEX_SHA256.fullmatch(digest):
            raise BootstrapError(f"{context}.sha256 must be lowercase hexadecimal SHA-256")
        entry_count = _require_bounded_integer(
            record["entry_count"], f"{context}.entry_count", MAX_NATIVE_TREE_ENTRIES
        )
        total_bytes = _require_bounded_integer(
            record["total_bytes"], f"{context}.total_bytes", MAX_NATIVE_TREE_BYTES
        )
        trees.append(
            {
                "path": tree_path,
                "sha256": digest,
                "entry_count": entry_count,
                "total_bytes": total_bytes,
            }
        )
    tree_paths = [str(record["path"]) for record in trees]
    if tree_paths != sorted(set(tree_paths)):
        raise BootstrapError("native environment trees must be uniquely sorted by path")

    raw_files = value["files"]
    if not isinstance(raw_files, list) or not 1 <= len(raw_files) <= MAX_NATIVE_FILES:
        raise BootstrapError(
            f"native environment files must contain 1 through {MAX_NATIVE_FILES} records"
        )
    files: list[dict[str, object]] = []
    for index, item in enumerate(raw_files):
        context = f"native environment files[{index}]"
        record = _require_object(item, context)
        _require_keys(record, {"path", "sha256", "byte_count"}, set(), context)
        file_path = str(_native_path(record["path"], f"{context}.path"))
        digest = _require_string(record["sha256"], f"{context}.sha256")
        if not HEX_SHA256.fullmatch(digest):
            raise BootstrapError(f"{context}.sha256 must be lowercase hexadecimal SHA-256")
        byte_count = _require_bounded_integer(
            record["byte_count"], f"{context}.byte_count", MAX_NATIVE_FILE_BYTES
        )
        files.append(
            {"path": file_path, "sha256": digest, "byte_count": byte_count}
        )
    file_paths = [str(record["path"]) for record in files]
    if file_paths != sorted(set(file_paths)):
        raise BootstrapError("native environment files must be uniquely sorted by path")
    if not NATIVE_OS_IDENTITY_PATHS.intersection(file_paths):
        raise BootstrapError(
            "native environment files must include /etc/os-release or /usr/lib/os-release"
        )
    names = {PurePosixPath(path).name for path in file_paths}
    missing_names = sorted(NATIVE_REQUIRED_FILE_NAMES - names)
    if missing_names:
        raise BootstrapError(
            "native environment files are missing required native inputs: "
            + ", ".join(missing_names)
        )
    if gcc_install_dir is not None:
        gcc_path = PurePosixPath(gcc_install_dir)
        if not any(
            gcc_path == PurePosixPath(tree)
            or PurePosixPath(tree) in gcc_path.parents
            for tree in tree_paths
        ):
            raise BootstrapError(
                "native environment gcc_install_dir must be covered by a tree"
            )

    return {
        "schema_version": NATIVE_ENVIRONMENT_SCHEMA,
        "qualification_scope": NATIVE_QUALIFICATION_SCOPE,
        "platform": platform,
        "sysroot": "/",
        "gcc_install_dir": gcc_install_dir,
        "trees": trees,
        "files": files,
        "linker": _validate_native_linker(
            value["linker"], platform, toolchain_lock=toolchain_lock
        ),
    }

def capture_native_environment(
    root: Path,
    platform: str,
    gcc_install_dir: str | None,
    trees: Sequence[str],
    files: Sequence[str],
    linker: Mapping[str, object],
    *,
    toolchain_lock: Mapping[str, object] | None = None,
) -> dict[str, object]:
    """Capture a bounded development-host native identity below ``root``."""
    if platform not in LINUX_NATIVE_PLATFORMS:
        raise BootstrapError(f"unsupported native environment platform: {platform}")
    capture_lock = (
        _load_lock()
        if toolchain_lock is None
        else _validate_lock(dict(toolchain_lock))
    )
    if isinstance(trees, (str, bytes)) or not 1 <= len(trees) <= MAX_NATIVE_TREES:
        raise BootstrapError(
            f"native environment trees must contain 1 through {MAX_NATIVE_TREES} paths"
        )
    if isinstance(files, (str, bytes)) or not 1 <= len(files) <= MAX_NATIVE_FILES:
        raise BootstrapError(
            f"native environment files must contain 1 through {MAX_NATIVE_FILES} paths"
        )
    tree_paths = sorted(
        str(_native_path(path, "native environment tree path")) for path in trees
    )
    file_paths = sorted(
        str(_native_path(path, "native environment file path")) for path in files
    )
    if len(tree_paths) != len(set(tree_paths)):
        raise BootstrapError("native environment tree paths must be unique")
    if len(file_paths) != len(set(file_paths)):
        raise BootstrapError("native environment file paths must be unique")

    normalized_gcc: str | None
    normalized_linker = _validate_native_linker(
        linker, platform, toolchain_lock=capture_lock
    )
    if gcc_install_dir is None:
        normalized_gcc = None
    else:
        normalized_gcc = str(
            _native_path(gcc_install_dir, "native environment gcc_install_dir")
        )

    with _native_root(root) as opened_root:
        if normalized_gcc is not None:
            gcc_path, gcc_status, _ = _resolve_native_path(
                opened_root,
                PurePosixPath(normalized_gcc),
                "native environment gcc_install_dir",
            )
            if not stat.S_ISDIR(gcc_status.st_mode):
                raise BootstrapError(
                    f"native environment gcc_install_dir is not a directory: {gcc_path}"
                )

        admitted_paths = {
            path: _resolve_native_path(
                opened_root, PurePosixPath(path), "native admitted path"
            )[0]
            for path in (*tree_paths, *file_paths)
        }
        admitted_trees = tuple(admitted_paths[path] for path in tree_paths)
        admitted_files = {admitted_paths[path] for path in file_paths}
        profile = {
            "schema_version": NATIVE_ENVIRONMENT_SCHEMA,
            "qualification_scope": NATIVE_QUALIFICATION_SCOPE,
            "platform": platform,
            "sysroot": "/",
            "gcc_install_dir": normalized_gcc,
            "trees": [
                _fingerprint_native_tree(
                    opened_root,
                    PurePosixPath(path),
                    admitted_trees,
                    admitted_files,
                )
                for path in tree_paths
            ],
            "files": [
                _fingerprint_native_file(opened_root, PurePosixPath(path))
                for path in file_paths
            ],
            "linker": normalized_linker,
        }
        for path, target in admitted_paths.items():
            if _resolve_native_path(
                opened_root, PurePosixPath(path), "native admitted path"
            )[0] != target:
                raise BootstrapError(f"native admitted target changed during capture: {path}")
    return _validate_native_environment_profile(
        profile, toolchain_lock=capture_lock
    )


def verify_native_environment(
    root: Path, profile: Mapping[str, object]
) -> None:
    """Refuse when a closed native profile differs from the supplied root."""
    expected = _validate_native_environment_profile(profile)
    observed = capture_native_environment(
        root,
        str(expected["platform"]),
        expected["gcc_install_dir"],  # type: ignore[arg-type]
        [str(record["path"]) for record in expected["trees"]],  # type: ignore[index]
        [str(record["path"]) for record in expected["files"]],  # type: ignore[index]
        expected["linker"],  # type: ignore[arg-type]
    )
    if observed == expected:
        return
    for group in ("trees", "files"):
        expected_records = expected[group]  # type: ignore[assignment]
        observed_records = observed[group]  # type: ignore[assignment]
        for expected_record, observed_record in zip(
            expected_records, observed_records  # type: ignore[arg-type]
        ):
            if expected_record != observed_record:
                raise BootstrapError(
                    f"native environment drift for {expected_record['path']}"
                )
    raise BootstrapError("native environment drift")


def _destination(root: Path, relative: PurePosixPath) -> Path:
    destination = root.joinpath(*relative.parts)
    resolved_parent = destination.parent.resolve(strict=False)
    resolved_root = root.resolve(strict=True)
    if not _inside(resolved_root, resolved_parent):
        raise BootstrapError(f"archive path escapes extraction root: {relative}")
    return destination


def _create_output_file(path: Path, mode: int) -> BinaryIO:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, mode)
    except OSError as exc:
        raise BootstrapError(f"cannot create extracted file {path}: {exc}") from exc
    return os.fdopen(descriptor, "wb")


def _validate_link_target(root: Path, destination: Path, linkname: str) -> None:
    if not linkname or "\x00" in linkname or "\\" in linkname:
        raise BootstrapError(f"unsafe archive link target: {linkname!r}")
    target = PurePosixPath(linkname)
    if target.is_absolute() or (target.parts and target.parts[0].endswith(":")):
        raise BootstrapError(f"unsafe archive link target: {linkname!r}")
    resolved = (destination.parent / Path(*target.parts)).resolve(strict=False)
    if not _inside(root.resolve(strict=True), resolved):
        raise BootstrapError(f"archive link target escapes extraction root: {linkname!r}")


def _extract_tar(archive_path: Path, kind: str, destination: Path) -> None:
    mode = "r:gz" if kind == "tar.gz" else "r:xz"
    with tarfile.open(archive_path, mode=mode, errorlevel=2) as archive:
        members = archive.getmembers()
        if len(members) > MAX_ARCHIVE_ENTRIES:
            raise BootstrapError("archive has too many entries")
        seen: set[str] = set()
        total_size = 0
        prepared: list[tuple[tarfile.TarInfo, PurePosixPath]] = []
        for member in members:
            relative = _safe_archive_path(member.name)
            collision_key = str(relative).casefold()
            if collision_key in seen:
                raise BootstrapError(f"archive contains a duplicate path: {relative}")
            seen.add(collision_key)
            if member.isfile():
                if member.size < 0 or member.size > MAX_ARCHIVE_MEMBER_BYTES:
                    raise BootstrapError(f"archive member is too large: {relative}")
                total_size += member.size
                if total_size > MAX_ARCHIVE_TOTAL_BYTES:
                    raise BootstrapError("archive expands beyond the total-size safety limit")
            elif not (member.isdir() or member.issym() or member.islnk()):
                raise BootstrapError(f"unsupported archive entry type: {relative}")
            prepared.append((member, relative))

        directory_modes: list[tuple[Path, int]] = []
        hardlinks: list[tuple[Path, PurePosixPath]] = []
        symlinks: list[tuple[Path, str]] = []
        for member, relative in prepared:
            output = _destination(destination, relative)
            if member.isdir():
                if output.exists():
                    if output.is_symlink() or not output.is_dir():
                        raise BootstrapError(f"archive directory collides with a file: {relative}")
                else:
                    output.mkdir(parents=True, mode=0o700)
                directory_modes.append((output, member.mode & 0o777))
                continue
            output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            if member.isfile():
                source = archive.extractfile(member)
                if source is None:
                    raise BootstrapError(f"cannot read archive member: {relative}")
                file_mode = member.mode & 0o777 or 0o600
                with source, _create_output_file(output, file_mode) as target:
                    _copy_bounded(
                        source,
                        target,
                        limit=member.size,
                        expected=member.size,
                    )
            elif member.islnk():
                hardlinks.append((output, _safe_archive_path(member.linkname, "hardlink target")))
            else:
                _validate_link_target(destination, output, member.linkname)
                symlinks.append((output, member.linkname))

        pending = hardlinks
        while pending:
            remaining: list[tuple[Path, PurePosixPath]] = []
            progress = False
            for output, relative_target in pending:
                target = _destination(destination, relative_target)
                if not target.exists():
                    remaining.append((output, relative_target))
                    continue
                if not target.is_file() or target.is_symlink():
                    raise BootstrapError(f"hardlink target is not a regular file: {relative_target}")
                try:
                    os.link(target, output, follow_symlinks=False)
                except OSError as exc:
                    raise BootstrapError(f"cannot create archive hardlink {output}: {exc}") from exc
                progress = True
            if not progress:
                raise BootstrapError("archive contains an unresolved hardlink target")
            pending = remaining

        for output, linkname in symlinks:
            try:
                os.symlink(linkname, output)
            except OSError as exc:
                raise BootstrapError(f"cannot create archive symlink {output}: {exc}") from exc
        for directory, mode_bits in sorted(
            directory_modes, key=lambda entry: len(entry[0].parts), reverse=True
        ):
            try:
                directory.chmod(mode_bits or 0o755)
            except OSError as exc:
                raise BootstrapError(f"cannot set extracted directory mode for {directory}: {exc}") from exc


def _extract_zip(archive_path: Path, destination: Path) -> None:
    with zipfile.ZipFile(archive_path, mode="r") as archive:
        entries = archive.infolist()
        if len(entries) > MAX_ARCHIVE_ENTRIES:
            raise BootstrapError("archive has too many entries")
        seen: set[str] = set()
        total_size = 0
        prepared: list[tuple[zipfile.ZipInfo, PurePosixPath, bool, int]] = []
        for entry in entries:
            relative = _safe_archive_path(entry.filename)
            collision_key = str(relative).casefold()
            if collision_key in seen:
                raise BootstrapError(f"archive contains a duplicate path: {relative}")
            seen.add(collision_key)
            if entry.flag_bits & 0x1:
                raise BootstrapError(f"encrypted zip entry is unsupported: {relative}")
            unix_mode = (entry.external_attr >> 16) & 0xFFFF
            file_type = stat.S_IFMT(unix_mode)
            is_directory = entry.is_dir()
            if file_type == stat.S_IFLNK:
                raise BootstrapError(f"zip symlink is unsupported: {relative}")
            if file_type not in {0, stat.S_IFREG, stat.S_IFDIR}:
                raise BootstrapError(f"unsupported zip entry type: {relative}")
            if not is_directory:
                if entry.file_size < 0 or entry.file_size > MAX_ARCHIVE_MEMBER_BYTES:
                    raise BootstrapError(f"archive member is too large: {relative}")
                total_size += entry.file_size
                if total_size > MAX_ARCHIVE_TOTAL_BYTES:
                    raise BootstrapError("archive expands beyond the total-size safety limit")
            prepared.append((entry, relative, is_directory, unix_mode & 0o777))

        directory_modes: list[tuple[Path, int]] = []
        for entry, relative, is_directory, mode_bits in prepared:
            output = _destination(destination, relative)
            if is_directory:
                if output.exists():
                    if output.is_symlink() or not output.is_dir():
                        raise BootstrapError(f"archive directory collides with a file: {relative}")
                else:
                    output.mkdir(parents=True, mode=0o700)
                directory_modes.append((output, mode_bits))
                continue
            output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with archive.open(entry, mode="r") as source, _create_output_file(
                output, mode_bits or 0o600
            ) as target:
                _copy_bounded(
                    source,
                    target,
                    limit=entry.file_size,
                    expected=entry.file_size,
                )
        for directory, mode_bits in sorted(
            directory_modes, key=lambda entry: len(entry[0].parts), reverse=True
        ):
            directory.chmod(mode_bits or 0o755)


def _extract_archive(archive_path: Path, kind: str, destination: Path) -> None:
    if kind not in {"tar.gz", "tar.xz", "zip"}:
        raise BootstrapError(f"unsupported archive kind: {kind}")
    if destination.exists():
        raise BootstrapError(f"extraction destination already exists: {destination}")
    destination.mkdir(parents=True, mode=0o700)
    try:
        if kind == "zip":
            _extract_zip(archive_path, destination)
        else:
            _extract_tar(archive_path, kind, destination)
    except (BootstrapError, OSError, tarfile.TarError, zipfile.BadZipFile) as exc:
        shutil.rmtree(destination, ignore_errors=True)
        if isinstance(exc, BootstrapError):
            raise
        raise BootstrapError(f"cannot safely extract {archive_path}: {exc}") from exc


@contextlib.contextmanager
def _file_lock(path: Path) -> Iterator[None]:
    if fcntl is None:
        raise BootstrapError("cache locking is unavailable on this host")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    except OSError as exc:
        raise BootstrapError(f"cannot open cache lock {path}: {exc}") from exc
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        yield
    except OSError as exc:
        raise BootstrapError(f"cannot lock cache file {path}: {exc}") from exc
    finally:
        os.close(descriptor)


def _atomic_write(path: Path, content: bytes, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.chmod(mode)
        os.replace(temporary, path)
    except OSError as exc:
        temporary.unlink(missing_ok=True)
        raise BootstrapError(f"cannot atomically write {path}: {exc}") from exc


def _download(asset: Mapping[str, object], cache: Path) -> Path:
    digest = str(asset["sha256"])
    destination = cache / "downloads" / f"{digest}.blob"
    if destination.exists():
        if not destination.is_file() or destination.is_symlink():
            raise BootstrapError(f"cached download is not a regular file: {destination}")
        _verify_sha256(destination, digest)
        return destination
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{digest}.", dir=destination.parent)
    temporary = Path(temporary_name)
    output = os.fdopen(descriptor, "wb")
    try:
        request = urllib.request.Request(
            str(asset["url"]),
            headers={"User-Agent": "pm-cli-bootstrap/1"},
            method="GET",
        )
        with output:
            with urllib.request.urlopen(request, timeout=60) as response:
                final_url = response.geturl()
                _require_https(final_url, "download redirect")
                if response.headers.get("Content-Encoding"):
                    raise BootstrapError("encoded HTTP downloads are unsupported")
                raw_length = response.headers.get("Content-Length")
                expected_length: int | None = None
                if raw_length is not None:
                    try:
                        expected_length = int(raw_length)
                    except ValueError as exc:
                        raise BootstrapError("invalid download Content-Length") from exc
                    if expected_length < 0 or expected_length > MAX_DOWNLOAD_BYTES:
                        raise BootstrapError("download exceeds the size safety limit")
                _copy_bounded(
                    response,
                    output,
                    limit=expected_length if expected_length is not None else MAX_DOWNLOAD_BYTES,
                    expected=expected_length,
                )
                output.flush()
                os.fsync(output.fileno())
        _verify_sha256(temporary, digest)
        temporary.chmod(0o444)
        try:
            os.rename(temporary, destination)
        except FileExistsError:
            temporary.unlink(missing_ok=True)
            _verify_sha256(destination, digest)
    except Exception as exc:
        output.close()
        temporary.unlink(missing_ok=True)
        if isinstance(exc, BootstrapError):
            raise
        raise BootstrapError(f"cannot download locked asset from {asset['url']}: {exc}") from exc
    return destination


def _find_asset(
    lock: Mapping[str, object], asset_id: str, selected_platform: str
) -> dict[str, object]:
    matches = [
        item
        for item in lock["assets"]  # type: ignore[index]
        if isinstance(item, dict)
        and item.get("id") == asset_id
        and item.get("platform") == selected_platform
    ]
    if len(matches) != 1:
        raise BootstrapError(
            f"expected one locked {asset_id} asset for {selected_platform}, found {len(matches)}"
        )
    return matches[0]


def _clang_linker_relative(asset: Mapping[str, object]) -> PurePosixPath:
    executable = _safe_archive_path(str(asset["executable"]), "locked clang executable")
    return executable.with_name("ld.lld")


def _compiler_runtime_relatives(
    asset: Mapping[str, object],
) -> list[PurePosixPath]:
    platform = str(asset["platform"])
    if asset["id"] != "clang" or platform not in LINUX_NATIVE_PLATFORMS:
        return []
    target_triple = LINUX_NATIVE_GO_TARGETS[platform][2]
    executable = _safe_archive_path(str(asset["executable"]), "locked clang executable")
    resource_directory = (
        executable.parent.parent
        / "lib"
        / "clang"
        / EXPECTED_ASSET_VERSIONS["clang"].split(".", 1)[0]
        / "lib"
        / target_triple
    )
    return [
        resource_directory / "libclang_rt.builtins.a",
        resource_directory / "clang_rt.crtbegin.o",
        resource_directory / "clang_rt.crtend.o",
    ]


def _executable_relatives(asset: Mapping[str, object]) -> list[PurePosixPath]:
    executable = _safe_archive_path(str(asset["executable"]), "locked executable")
    paths = [executable]
    if asset["id"] == "clang":
        paths.append(executable.with_name("clang++"))
        if str(asset["platform"]) in LINUX_NATIVE_PLATFORMS:
            paths.append(_clang_linker_relative(asset))
    return paths


def _verify_cached_members(
    root: Path,
    resolved_root: Path,
    relatives: Sequence[PurePosixPath],
    hashes: Mapping[str, object],
    *,
    executable: bool,
    context: str,
) -> dict[PurePosixPath, Path]:
    if set(hashes) != {str(path) for path in relatives}:
        raise BootstrapError(f"cached {context} set mismatch: {root}")
    result: dict[PurePosixPath, Path] = {}
    for relative in relatives:
        member = root.joinpath(*relative.parts)
        try:
            resolved = member.resolve(strict=True)
        except OSError as exc:
            raise BootstrapError(f"cannot resolve cached {context} {member}: {exc}") from exc
        if not _inside(resolved_root, resolved) or not resolved.is_file():
            raise BootstrapError(f"cached {context} escapes its install root: {member}")
        if executable and not os.access(member, os.X_OK):
            raise BootstrapError(f"cached {context} is not executable: {member}")
        expected_digest = _require_string(
            hashes[str(relative)], f"cached digest for {relative}"
        )
        if not HEX_SHA256.fullmatch(expected_digest):
            raise BootstrapError(f"invalid cached digest for {relative}")
        _verify_sha256(member, expected_digest)
        result[relative] = member.absolute()
    return result


def _verify_install(root: Path, asset: Mapping[str, object]) -> dict[PurePosixPath, Path]:
    marker_path = root / ".pm-bootstrap.json"
    marker = _require_object(_read_json(marker_path), "bootstrap cache marker")
    runtime_relatives = _compiler_runtime_relatives(asset)
    _require_keys(
        marker,
        {"schema_version", "asset", "executables"},
        {"runtime_files"} if runtime_relatives else set(),
        "cache marker",
    )
    if marker["schema_version"] != "pm.bootstrap-cache/v1":
        raise BootstrapError(f"unsupported cache marker schema: {marker_path}")
    identity = _require_object(marker["asset"], "cache marker asset")
    _require_keys(identity, {"id", "version", "platform", "sha256"}, set(), "cache marker asset")
    expected_identity = {
        "id": asset["id"],
        "version": asset["version"],
        "platform": asset["platform"],
        "sha256": asset["sha256"],
    }
    if identity != expected_identity:
        raise BootstrapError(f"cached install identity mismatch: {root}")
    resolved_root = root.resolve(strict=True)
    executable_hashes = _require_object(marker["executables"], "cache marker executables")
    result = _verify_cached_members(
        root,
        resolved_root,
        _executable_relatives(asset),
        executable_hashes,
        executable=True,
        context="executable",
    )
    if runtime_relatives:
        runtime_hashes = _require_object(
            marker.get("runtime_files"), "cache marker compiler runtime files"
        )
        result.update(
            _verify_cached_members(
                root,
                resolved_root,
                runtime_relatives,
                runtime_hashes,
                executable=False,
                context="compiler runtime file",
            )
        )
    return result


def _asset_install_root(asset: Mapping[str, object], cache: Path) -> Path:
    root = (
        cache
        / "tools"
        / str(asset["id"])
        / str(asset["version"])
        / str(asset["platform"])
        / str(asset["sha256"])
    )
    if (
        asset["id"] == "clang"
        and str(asset["platform"]) in LINUX_NATIVE_PLATFORMS
    ):
        return root / "native-v1"
    return root


def _install_asset(
    lock: Mapping[str, object], asset_id: str, selected_platform: str, cache: Path
) -> tuple[dict[str, object], dict[PurePosixPath, Path]]:
    asset = _find_asset(lock, asset_id, selected_platform)
    identity = f"{asset['id']}-{asset['version']}-{asset['platform']}-{asset['sha256']}"
    install_root = _asset_install_root(asset, cache)
    with _file_lock(cache / "locks" / f"{identity}.lock"):
        download = _download(asset, cache)
        if install_root.exists():
            if not install_root.is_dir() or install_root.is_symlink():
                raise BootstrapError(f"cached install is not a directory: {install_root}")
            return asset, _verify_install(install_root, asset)

        install_root.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        staging = Path(tempfile.mkdtemp(prefix=".install-", dir=install_root.parent))
        payload = staging / "payload"
        try:
            if asset["kind"] == "binary":
                payload.mkdir(mode=0o700)
                relative = _executable_relatives(asset)[0]
                executable = payload.joinpath(*relative.parts)
                executable.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                try:
                    os.link(download, executable, follow_symlinks=False)
                except OSError as exc:
                    raise BootstrapError(f"cannot install locked binary {asset_id}: {exc}") from exc
                executable.chmod(0o555)
            else:
                _extract_archive(download, str(asset["kind"]), payload)

            executable_hashes: dict[str, str] = {}
            resolved_payload = payload.resolve(strict=True)
            for relative in _executable_relatives(asset):
                executable = payload.joinpath(*relative.parts)
                try:
                    resolved = executable.resolve(strict=True)
                except OSError as exc:
                    raise BootstrapError(
                        f"locked executable is absent from {asset_id} archive: {relative}"
                    ) from exc
                if not _inside(resolved_payload, resolved) or not resolved.is_file():
                    raise BootstrapError(f"locked executable is unsafe: {relative}")
                if not os.access(executable, os.X_OK):
                    raise BootstrapError(f"locked executable lacks execute permission: {relative}")
                executable_hashes[str(relative)] = _sha256_file(executable)
            runtime_hashes: dict[str, str] = {}
            for relative in _compiler_runtime_relatives(asset):
                runtime = payload.joinpath(*relative.parts)
                try:
                    resolved = runtime.resolve(strict=True)
                except OSError as exc:
                    raise BootstrapError(
                        f"compiler runtime is absent from {asset_id} archive: {relative}"
                    ) from exc
                if not _inside(resolved_payload, resolved) or not resolved.is_file():
                    raise BootstrapError(f"compiler runtime is unsafe: {relative}")
                runtime_hashes[str(relative)] = _sha256_file(runtime)
            marker = {
                "schema_version": "pm.bootstrap-cache/v1",
                "asset": {
                    "id": asset["id"],
                    "version": asset["version"],
                    "platform": asset["platform"],
                    "sha256": asset["sha256"],
                },
                "executables": executable_hashes,
            }
            if runtime_hashes:
                marker["runtime_files"] = runtime_hashes
            _atomic_write(
                payload / ".pm-bootstrap.json",
                (json.dumps(marker, sort_keys=True, separators=(",", ":")) + "\n").encode(),
                0o444,
            )
            os.rename(payload, install_root)
        finally:
            shutil.rmtree(staging, ignore_errors=True)
        return asset, _verify_install(install_root, asset)


def _resolve_tool(
    lock: Mapping[str, object], asset_id: str, selected_platform: str, cache: Path
) -> tuple[dict[str, object], Path]:
    asset, installed = _install_asset(lock, asset_id, selected_platform, cache)
    relative = _safe_archive_path(str(asset["executable"]), "locked executable")
    return asset, installed[relative]

def _selected_asset_root(
    asset: Mapping[str, object], cache: Path
) -> Path:
    root = _asset_install_root(asset, cache)
    try:
        return root.resolve(strict=True)
    except OSError as exc:
        raise BootstrapError(f"cannot resolve selected asset root {root}: {exc}") from exc


def _compiler_paths(
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> tuple[
    dict[str, object],
    Path,
    Path,
    Path | None,
    tuple[Path, ...],
]:
    asset, installed = _install_asset(lock, "clang", selected_platform, cache)
    clang = _safe_archive_path(str(asset["executable"]), "locked clang executable")
    linker = (
        installed[_clang_linker_relative(asset)]
        if selected_platform in LINUX_NATIVE_PLATFORMS
        else None
    )
    runtimes = tuple(installed[path] for path in _compiler_runtime_relatives(asset))
    return (
        asset,
        installed[clang],
        installed[clang.with_name("clang++")],
        linker,
        runtimes,
    )


def _load_native_environment_profile() -> dict[str, object]:
    try:
        status = os.lstat(NATIVE_ENVIRONMENT_PATH)
    except OSError as exc:
        raise BootstrapError(
            "Linux native execution is unqualified: committed "
            f"{NATIVE_ENVIRONMENT_PATH.relative_to(ROOT)} is unavailable: {exc}"
        ) from exc
    if (
        not stat.S_ISREG(status.st_mode)
        or status.st_size < 0
        or status.st_size > MAX_NATIVE_PROFILE_BYTES
    ):
        raise BootstrapError(
            "Linux native execution is unqualified: committed native environment "
            "profile is not a bounded regular file"
        )
    if not hasattr(os, "O_NOFOLLOW"):
        raise BootstrapError("safe native profile opening is unavailable on this host")
    flags = os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    try:
        descriptor = os.open(NATIVE_ENVIRONMENT_PATH, flags)
    except OSError as exc:
        raise BootstrapError(
            f"cannot open committed native environment profile: {exc}"
        ) from exc
    try:
        opened_status = os.fstat(descriptor)
        if (
            not stat.S_ISREG(opened_status.st_mode)
            or opened_status.st_dev != status.st_dev
            or opened_status.st_ino != status.st_ino
            or opened_status.st_size != status.st_size
        ):
            raise BootstrapError(
                "committed native environment profile changed before it was opened"
            )
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(
                descriptor,
                min(CHUNK_BYTES, MAX_NATIVE_PROFILE_BYTES + 1 - total),
            )
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > MAX_NATIVE_PROFILE_BYTES:
                raise BootstrapError(
                    "committed native environment profile is too large"
                )
        final_status = os.fstat(descriptor)
        if (
            total != opened_status.st_size
            or final_status.st_dev != opened_status.st_dev
            or final_status.st_ino != opened_status.st_ino
            or final_status.st_size != opened_status.st_size
            or final_status.st_mtime_ns != opened_status.st_mtime_ns
        ):
            raise BootstrapError(
                "committed native environment profile changed while it was read"
            )
    except OSError as exc:
        raise BootstrapError(
            f"cannot read committed native environment profile: {exc}"
        ) from exc
    finally:
        os.close(descriptor)
    try:
        profile = json.loads(
            b"".join(chunks).decode("utf-8"),
            object_pairs_hook=_reject_duplicate_keys,
        )
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise BootstrapError(
            f"cannot read strict native environment JSON: {exc}"
        ) from exc
    return _validate_native_environment_profile(profile)


def _verified_linux_profile(
    lock: Mapping[str, object], selected_platform: str
) -> dict[str, object]:
    profile = _load_native_environment_profile()
    if profile["platform"] != selected_platform:
        raise BootstrapError(
            "Linux native execution is unqualified for this host: "
            f"profile={profile['platform']}, host={selected_platform}"
        )
    clang_asset = _find_asset(lock, "clang", selected_platform)
    expected_linker = {
        "asset_id": "clang",
        "executable": str(_clang_linker_relative(clang_asset)),
        "source_sha256": clang_asset["sha256"],
    }
    if profile["linker"] != expected_linker:
        raise BootstrapError(
            "native environment linker does not match the selected Clang source"
        )
    return profile


def _native_profile_record(
    profile: Mapping[str, object], name: str
) -> dict[str, object]:
    matches = [
        record
        for record in profile["files"]  # type: ignore[index]
        if PurePosixPath(str(record["path"])).name == name
    ]
    if len(matches) != 1:
        raise BootstrapError(
            f"native environment must select exactly one {name}, found {len(matches)}"
        )
    return matches[0]  # type: ignore[return-value]


def _native_profile_sha256(profile: Mapping[str, object]) -> str:
    canonical = json.dumps(
        profile, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()




def _prepare_native_linker_inputs(
    profile: Mapping[str, object],
    profile_sha256: str,
    cache: Path,
) -> tuple[Path, dict[str, object]]:
    targets = {
        "libgcc_s.so": _native_profile_record(profile, "libgcc_s.so.1"),
        "libstdc++.so": _native_profile_record(profile, "libstdc++.so.6"),
    }
    scripts = {
        name: (
            'INPUT ( "'
            + str(record["path"]).replace('"', '\\"')
            + '" )\n'
        ).encode("utf-8")
        for name, record in targets.items()
    }
    declaration = {
        "schema_version": "pm.native-derived-linker-inputs/v1",
        "native_environment_sha256": profile_sha256,
        "targets": targets,
        "scripts": {
            name: hashlib.sha256(content).hexdigest()
            for name, content in scripts.items()
        },
    }
    declaration_bytes = json.dumps(
        declaration, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")
    identity = hashlib.sha256(declaration_bytes).hexdigest()
    linker_root = cache / "native-linker"
    directory = linker_root / identity
    with _file_lock(cache / "locks" / f"native-linker-{identity}.lock"):
        if os.path.lexists(linker_root):
            if not linker_root.is_dir() or linker_root.is_symlink():
                raise BootstrapError(
                    f"derived native linker root is not a directory: {linker_root}"
                )
        else:
            try:
                linker_root.mkdir(mode=0o700)
            except OSError as exc:
                raise BootstrapError(
                    f"cannot create derived native linker root {linker_root}: {exc}"
                ) from exc
        if os.path.lexists(directory):
            if not directory.is_dir() or directory.is_symlink():
                raise BootstrapError(
                    f"derived native linker input is not a directory: {directory}"
                )
        else:
            try:
                directory.mkdir(mode=0o700)
            except OSError as exc:
                raise BootstrapError(
                    f"cannot create derived native linker input {directory}: {exc}"
                ) from exc
        for name, content in scripts.items():
            path = directory / name
            if os.path.lexists(path):
                try:
                    status = os.lstat(path)
                except OSError as exc:
                    raise BootstrapError(
                        f"cannot inspect derived native linker input {path}: {exc}"
                    ) from exc
                if not stat.S_ISREG(status.st_mode):
                    raise BootstrapError(
                        f"derived native linker input is not a regular file: {path}"
                    )
                try:
                    observed = path.read_bytes()
                except OSError as exc:
                    raise BootstrapError(
                        f"cannot read derived native linker input {path}: {exc}"
                    ) from exc
                if observed != content:
                    raise BootstrapError(
                        f"derived native linker input content mismatch: {path}"
                    )
            else:
                _atomic_write(path, content, 0o444)
        try:
            with os.scandir(directory) as entries:
                members = {entry.name for entry in entries}
        except OSError as exc:
            raise BootstrapError(
                f"cannot inspect derived native linker inputs {directory}: {exc}"
            ) from exc
        if members != set(scripts):
            raise BootstrapError(
                f"derived native linker input set mismatch: {directory}"
            )
    return directory, {
        **declaration,
        "identity_sha256": identity,
        "path": str(directory),
    }


@dataclass(frozen=True)
class _NativeSelection:
    asset: dict[str, object]
    cc: Path
    cxx: Path
    linker: Path | None
    environment: dict[str, str]
    driver_compile_options: tuple[str, ...]
    driver_link_options: tuple[str, ...]
    bazel_compile_options: tuple[str, ...]
    bazel_link_options: tuple[str, ...]
    identity: dict[str, object] | None


def _native_selection(
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> _NativeSelection:
    profile = (
        _verified_linux_profile(lock, selected_platform)
        if selected_platform in LINUX_NATIVE_PLATFORMS
        else None
    )
    asset, cc, cxx, linker, compiler_runtimes = _compiler_paths(
        lock, selected_platform, cache
    )
    if profile is None:
        environment = _darwin_native_environment()
        environment.update(CC=str(cc), CXX=str(cxx))
        sdk = environment["SDKROOT"]
        bazel_options = ("-isysroot", sdk)
        return _NativeSelection(
            asset=asset,
            cc=cc,
            cxx=cxx,
            linker=None,
            environment=environment,
            driver_compile_options=(),
            driver_link_options=(),
            bazel_compile_options=bazel_options,
            bazel_link_options=bazel_options,
            identity=None,
        )

    if linker is None:
        raise BootstrapError("selected Linux Clang archive has no verified ld.lld")
    expected_linker = _selected_asset_root(asset, cache).joinpath(
        *_safe_archive_path(
            str(profile["linker"]["executable"]),  # type: ignore[index]
            "native environment linker executable",
        ).parts
    )
    if linker.resolve(strict=True) != expected_linker.resolve(strict=True):
        raise BootstrapError("resolved linker is outside the selected Clang source")
    verify_native_environment(Path("/"), profile)
    profile_sha256 = _native_profile_sha256(profile)
    linker_inputs, linker_input_identity = _prepare_native_linker_inputs(
        profile, profile_sha256, cache
    )
    target_goos, target_goarch, target_triple = LINUX_NATIVE_GO_TARGETS[
        selected_platform
    ]
    resource_include = (
        cc.parent.parent
        / "lib"
        / "clang"
        / EXPECTED_ASSET_VERSIONS["clang"].split(".", 1)[0]
        / "include"
    )
    if not resource_include.is_dir() or not resource_include.resolve(
        strict=True
    ).is_relative_to(_selected_asset_root(asset, cache).resolve(strict=True)):
        raise BootstrapError("selected Clang source has no confined builtin headers")
    system_includes = (
        Path("/usr/include") / target_triple.replace("-unknown-", "-"),
        Path("/usr/include"),
    )
    for include in system_includes:
        if not include.is_dir() or not any(
            include.is_relative_to(Path(record["path"]))
            for record in profile["trees"]
        ):
            raise BootstrapError(f"native include directory is not profiled: {include}")
    compile_options = [
        "--no-default-config",
        f"-DPM_NATIVE_ENVIRONMENT_SHA256={profile_sha256}",
        f"--target={target_triple}",
        "--sysroot=/",
        "-nostdinc",
        "-isystem",
        str(resource_include),
        "-isystem",
        str(system_includes[0]),
        "-isystem",
        str(system_includes[1]),
    ]
    gcc_install_dir = profile["gcc_install_dir"]
    if gcc_install_dir is None:
        if len(compiler_runtimes) != 3:
            raise BootstrapError(
                "selected Clang source lacks the required compiler-rt runtime closure"
            )
        compile_options.append(f"--gcc-toolchain={cc.parent.parent}")
        runtime_options = [
            "--rtlib=compiler-rt",
            "--unwindlib=libgcc",
            "-nostdlib++",
        ]
        compiler_runtime = "selected-clang-compiler-rt"
    else:
        compile_options.append(f"--gcc-install-dir={gcc_install_dir}")
        runtime_options = ["--rtlib=libgcc", "--unwindlib=libgcc"]
        compiler_runtime = "profiled-gcc"

    link_options = [
        *compile_options,
        f"-fuse-ld={linker}",
        f"-L{linker_inputs}",
        *runtime_options,
        "-lgcc_s",
    ]
    environment = {
        "CC": str(cc),
        "CXX": str(cxx),
        "LD": str(linker),
        "GOENV": "off",
        "GOOS": target_goos,
        "GOARCH": target_goarch,
        "CGO_CFLAGS": shlex.join(compile_options),
        "CGO_CPPFLAGS": shlex.join(compile_options),
        "CGO_CXXFLAGS": shlex.join(compile_options),
        "CGO_LDFLAGS": shlex.join(link_options),
    }
    identity = {
        "profile": profile,
        "profile_sha256": profile_sha256,
        "compiler_runtime": compiler_runtime,
        "compiler_runtime_files": [str(path) for path in compiler_runtimes],
        "compiler": str(cc),
        "linker": str(linker),
        "derived_linker_inputs": linker_input_identity,
        "cleared_environment": sorted(LINUX_NATIVE_AMBIENT_OVERRIDES),
    }
    return _NativeSelection(
        asset=asset,
        cc=cc,
        cxx=cxx,
        linker=linker,
        environment=environment,
        driver_compile_options=tuple(compile_options),
        driver_link_options=tuple(link_options),
        bazel_compile_options=tuple(compile_options),
        bazel_link_options=tuple(link_options),
        identity=identity,
    )


def _clang_driver_options(
    arguments: Sequence[str], native: _NativeSelection
) -> tuple[str, ...]:
    non_linking = {
        "--help",
        "--version",
        "-E",
        "-M",
        "-MM",
        "-S",
        "-c",
        "-dumpmachine",
        "-fsyntax-only",
        "-print-resource-dir",
        "-print-target-triple",
    }
    if any(argument in non_linking for argument in arguments):
        return native.driver_compile_options
    return native.driver_link_options


def _apply_native_selection(
    environment: dict[str, str],
    selected_platform: str,
    selection: _NativeSelection,
) -> None:
    if selected_platform in LINUX_NATIVE_PLATFORMS:
        for name in LINUX_NATIVE_AMBIENT_OVERRIDES:
            environment.pop(name, None)
    environment.update(selection.environment)


def _reject_unadmitted_linux_native_requests(
    environment: Mapping[str, str],
    selected_platform: str,
    tool: str,
) -> None:
    if tool not in {"go", "bazelisk"}:
        return
    cgo_overrides = sorted(
        name
        for name in LINUX_NATIVE_CGO_OVERRIDES
        if environment.get(name, "").strip()
    )
    if cgo_overrides:
        raise BootstrapError(
            "unadmitted Linux native CGO flag overrides are not allowed: "
            + ", ".join(cgo_overrides)
        )
    target_goos, target_goarch, _ = LINUX_NATIVE_GO_TARGETS[selected_platform]
    requested_goos = environment.get("GOOS", "").strip()
    requested_goarch = environment.get("GOARCH", "").strip()
    if requested_goos and requested_goos != target_goos:
        raise BootstrapError(
            f"unqualified Linux native GOOS requested: {requested_goos}"
        )
    if requested_goarch and requested_goarch != target_goarch:
        raise BootstrapError(
            f"unqualified Linux native GOARCH requested: {requested_goarch}"
        )




def _bazel_path_segment(selected_platform: str) -> str:
    suffix = {
        "darwin-arm64": "darwin-arm64",
        "linux-amd64": "linux-x86_64",
        "linux-arm64": "linux-arm64",
    }[selected_platform]
    return f"bazel-{BAZEL_VERSION}-{suffix}"


def _bazel_cache_source_name() -> str:
    return re.sub(r"[^A-Za-z0-9]", "-", BAZEL_BASE_URL)


def _prepare_bazelisk_cache(
    bazel: Path, bazel_digest: str, selected_platform: str, cache: Path
) -> Path:
    home = cache / "bazelisk"
    cas_binary = home / "downloads" / "sha256" / bazel_digest / "bin" / "bazel"
    mapping = (
        home
        / "downloads"
        / "metadata"
        / _bazel_cache_source_name()
        / _bazel_path_segment(selected_platform)
    )
    with _file_lock(cache / "locks" / "bazelisk-cas.lock"):
        if cas_binary.exists():
            if not cas_binary.is_file() or cas_binary.is_symlink():
                raise BootstrapError(f"Bazelisk CAS entry is not a regular file: {cas_binary}")
            _verify_sha256(cas_binary, bazel_digest)
        else:
            cas_binary.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            try:
                os.link(bazel, cas_binary, follow_symlinks=False)
            except OSError as exc:
                raise BootstrapError(f"cannot populate Bazelisk CAS: {exc}") from exc
            cas_binary.chmod(0o555)
            _verify_sha256(cas_binary, bazel_digest)
        if mapping.exists():
            try:
                mapped_digest = mapping.read_text(encoding="ascii")
            except (OSError, UnicodeError) as exc:
                raise BootstrapError(f"cannot read Bazelisk digest mapping: {exc}") from exc
            if mapped_digest != bazel_digest:
                raise BootstrapError(f"Bazelisk digest mapping mismatch: {mapping}")
        else:
            _atomic_write(mapping, bazel_digest.encode("ascii"), 0o444)
    return home


def _bazelisk_environment(
    lock: Mapping[str, object],
    selected_platform: str,
    cache: Path,
    native: _NativeSelection,
) -> tuple[dict[str, object], Path, dict[str, str]]:
    bazelisk_asset, bazelisk = _resolve_tool(lock, "bazelisk", selected_platform, cache)
    bazel_asset, bazel = _resolve_tool(lock, "bazel", selected_platform, cache)
    _, go = _resolve_tool(lock, "go", selected_platform, cache)
    bazel_digest = str(bazel_asset["sha256"])
    bazelisk_home = _prepare_bazelisk_cache(
        bazel, bazel_digest, selected_platform, cache
    )
    environment = {
        "BAZELISK_BASE_URL": BAZEL_BASE_URL,
        "BAZELISK_HOME": str(bazelisk_home),
        "BAZELISK_NOJDK": "0",
        "BAZELISK_SKIP_WRAPPER": "true",
        "BAZELISK_VERIFY_SHA256": bazel_digest,
        "USE_BAZEL_FALLBACK_VERSION": f"error:{BAZEL_VERSION}",
        "USE_BAZEL_VERSION": BAZEL_VERSION,
        "GOTOOLCHAIN": "local",
        "GOWORK": "off",
        "PM_GO": str(go),
        "PM_REPO_ROOT": str(ROOT),
        **native.environment,
    }
    if selected_platform.startswith("darwin-"):
        environment["BAZELISK_HOME_DARWIN"] = str(bazelisk_home)
    else:
        environment["BAZELISK_HOME_LINUX"] = str(bazelisk_home)
    return bazelisk_asset, bazelisk, environment


def _print_environment(
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> None:
    native = _native_selection(lock, selected_platform, cache)
    go_asset, go = _resolve_tool(lock, "go", selected_platform, cache)
    clang_asset = native.asset
    bazelisk_asset, bazelisk = _resolve_tool(lock, "bazelisk", selected_platform, cache)
    bazel_asset, bazel = _resolve_tool(lock, "bazel", selected_platform, cache)
    _prepare_bazelisk_cache(
        bazel, str(bazel_asset["sha256"]), selected_platform, cache
    )
    syft_asset, syft = _resolve_tool(lock, "syft", selected_platform, cache)
    selected = [
        bazel_asset,
        bazelisk_asset,
        clang_asset,
        go_asset,
        syft_asset,
    ]
    asset_sources = [
        {
            "path": str(_selected_asset_root(asset, cache)),
            "id": asset["id"],
            "version": asset["version"],
            "sha256": asset["sha256"],
        }
        for asset in selected
    ]
    result = {
        "schema_version": "pm.bootstrap-env/v1",
        "platform": selected_platform,
        "cache": str(cache),
        "GO": str(go),
        "CC": str(native.cc),
        "CXX": str(native.cxx),
        "LD": str(native.linker) if native.linker is not None else None,
        "BAZELISK": str(bazelisk),
        "SYFT": str(syft),
        "ASSET_ROOTS": [source["path"] for source in asset_sources],
        "ASSET_SOURCES": asset_sources,
        "native_environment": native.environment,
        "native_identity": native.identity,
        "native_compile_options": list(native.bazel_compile_options),
        "native_link_options": list(native.bazel_link_options),
        "identities": {
            "go": {"version": go_asset["version"], "sha256": go_asset["sha256"]},
            "clang": {
                "version": clang_asset["version"],
                "sha256": clang_asset["sha256"],
            },
            "bazelisk": {
                "version": bazelisk_asset["version"],
                "sha256": bazelisk_asset["sha256"],
            },
            "bazel": {
                "version": bazel_asset["version"],
                "sha256": bazel_asset["sha256"],
            },
            "syft": {"version": syft_asset["version"], "sha256": syft_asset["sha256"]},
        },
    }
    print(json.dumps(result, indent=2, sort_keys=True))


def _darwin_native_environment() -> dict[str, str]:
    try:
        result = subprocess.run(
            ["/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-path"],
            check=True, capture_output=True, text=True, timeout=15,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise BootstrapError(f"cannot resolve the required Apple SDK: {exc}") from exc
    sdk = Path(result.stdout.strip())
    if not sdk.is_absolute() or not sdk.is_dir():
        raise BootstrapError("xcrun did not return an available absolute Apple SDK path")
    deployment_target = host_platform.mac_ver()[0]
    if not re.fullmatch(r"[0-9]+\.[0-9]+(?:\.[0-9]+)?", deployment_target):
        raise BootstrapError("cannot resolve the current macOS deployment target")
    # LLVM 21 emits DWARF5, which the Apple linker cannot preserve here.
    # Produce supported debug information; do not disable symbols or warnings.
    return {
        "SDKROOT": str(sdk),
        "MACOSX_DEPLOYMENT_TARGET": deployment_target,
        "CGO_CFLAGS": os.environ.get("CGO_CFLAGS", "-O2 -g") + " -gdwarf-4",
        "CGO_CXXFLAGS": os.environ.get("CGO_CXXFLAGS", "-O2 -g") + " -gdwarf-4",
    }


def _bazel_output_root(cache: Path, selected_platform: str) -> Path:
    # Bazel 9 rejects its repository-contents cache inside the source workspace.
    if not _inside(ROOT, cache):
        return cache / "bazel"
    user_cache = Path.home() / ("Library/Caches" if selected_platform.startswith("darwin-") else ".cache")
    return user_cache / "pm-cli" / "bazel"


def _bazel_native_rc(cache: Path, native: _NativeSelection) -> Path:
    # Child compiler actions do not retain every repository environment value.
    # Bind the same admitted native inputs directly into Bazel build options.
    lines: list[str] = []
    for option in ("copt", "host_copt"):
        for argument in native.bazel_compile_options:
            lines.append(f"build --{option}={json.dumps(argument)}\n")
    for option in ("linkopt", "host_linkopt"):
        for argument in native.bazel_link_options:
            lines.append(f"build --{option}={json.dumps(argument)}\n")
    content = "".join(lines).encode("utf-8")
    path = cache / "bazel-native" / f"{hashlib.sha256(content).hexdigest()}.bazelrc"
    _atomic_write(path, content, 0o444)
    return path


def _exec_tool(
    tool: str,
    arguments: Sequence[str],
    lock: Mapping[str, object],
    selected_platform: str,
    cache: Path,
) -> None:
    environment = os.environ.copy()
    native_required = tool in {"bazelisk", "clang", "clang++"} or (
        tool == "go" and environment.get("CGO_ENABLED") != "0"
    )
    if native_required and selected_platform in LINUX_NATIVE_PLATFORMS:
        _reject_unadmitted_linux_native_requests(
            environment, selected_platform, tool
        )
    native = (
        _native_selection(lock, selected_platform, cache)
        if native_required
        else None
    )
    if native is not None:
        _apply_native_selection(environment, selected_platform, native)
    if tool == "bazelisk":
        if native is None:
            raise BootstrapError("Bazel native selection was not prepared")
        _, executable, additions = _bazelisk_environment(
            lock, selected_platform, cache, native
        )
        environment.update(additions)
        environment.pop("BAZELISK_FORMAT_URL", None)
        argv = [
            str(executable),
            f"--bazelrc={_bazel_native_rc(cache, native)}",
            "--nohome_rc",
            "--nosystem_rc",
            f"--output_user_root={_bazel_output_root(cache, selected_platform)}",
            *arguments,
        ]
    elif tool in {"clang", "clang++"}:
        if native is None:
            raise BootstrapError("Clang native selection was not prepared")
        executable = native.cc if tool == "clang" else native.cxx
        argv = [
            str(executable),
            *arguments,
            *_clang_driver_options(arguments, native),
        ]
    else:
        _, executable = _resolve_tool(lock, tool, selected_platform, cache)
        argv = [str(executable), *arguments]
        if tool == "go":
            environment["GOTOOLCHAIN"] = "local"
            environment["GOWORK"] = "off"
            environment["GOROOT"] = str(executable.parent.parent)
            environment["PM_GO"] = str(executable)
            environment["PATH"] = str(executable.parent) + os.pathsep + environment.get("PATH", "")
        elif tool == "syft":
            environment["SYFT_CHECK_FOR_APP_UPDATE"] = "false"
    try:
        os.execve(executable, argv, environment)
    except OSError as exc:
        raise BootstrapError(f"cannot execute locked {tool} binary {executable}: {exc}") from exc


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Fetch, verify, and run exact project-local build tools."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    execute = subparsers.add_parser(
        "exec", help="replace this process with a locked tool"
    )
    execute.add_argument("tool", choices=("go", "bazelisk", "clang", "clang++", "syft"))
    execute.add_argument("arguments", nargs=argparse.REMAINDER, metavar="-- ARGS")
    subparsers.add_parser("print-env", help="print resolved paths and identities as JSON")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    raw_arguments = list(sys.argv[1:] if argv is None else argv)
    if raw_arguments[:1] == ["exec"]:
        if len(raw_arguments) < 3 or raw_arguments[2] != "--":
            parser.error("exec requires a literal -- before tool arguments")
        raw_arguments = raw_arguments[:2] + raw_arguments[3:]
    args = parser.parse_args(raw_arguments)
    tool_arguments = args.arguments if args.command == "exec" else []
    try:
        lock = _load_lock()
        selected_platform = _selected_platform()
        cache = _cache_root()
        if args.command == "print-env":
            _print_environment(lock, selected_platform, cache)
        else:
            _exec_tool(args.tool, tool_arguments, lock, selected_platform, cache)
    except BootstrapError as exc:
        print(f"bootstrap: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
