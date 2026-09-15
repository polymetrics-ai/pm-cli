#!/usr/bin/env python3
"""Resolve and execute the repository's exact, digest-pinned build tools."""

from __future__ import annotations

import argparse
import contextlib
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


def _executable_relatives(asset: Mapping[str, object]) -> list[PurePosixPath]:
    executable = _safe_archive_path(str(asset["executable"]), "locked executable")
    paths = [executable]
    if asset["id"] == "clang":
        paths.append(executable.with_name("clang++"))
    return paths


def _verify_install(root: Path, asset: Mapping[str, object]) -> dict[PurePosixPath, Path]:
    marker_path = root / ".pm-bootstrap.json"
    marker = _require_object(_read_json(marker_path), "bootstrap cache marker")
    _require_keys(marker, {"schema_version", "asset", "executables"}, set(), "cache marker")
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
    executable_hashes = _require_object(marker["executables"], "cache marker executables")
    expected_relatives = _executable_relatives(asset)
    if set(executable_hashes) != {str(path) for path in expected_relatives}:
        raise BootstrapError(f"cached executable set mismatch: {root}")
    resolved_root = root.resolve(strict=True)
    result: dict[PurePosixPath, Path] = {}
    for relative in expected_relatives:
        executable = root.joinpath(*relative.parts)
        try:
            resolved = executable.resolve(strict=True)
        except OSError as exc:
            raise BootstrapError(f"cannot resolve cached executable {executable}: {exc}") from exc
        if not _inside(resolved_root, resolved) or not resolved.is_file():
            raise BootstrapError(f"cached executable escapes its install root: {executable}")
        if not os.access(executable, os.X_OK):
            raise BootstrapError(f"cached executable is not executable: {executable}")
        expected_digest = _require_string(
            executable_hashes[str(relative)], f"cached digest for {relative}"
        )
        if not HEX_SHA256.fullmatch(expected_digest):
            raise BootstrapError(f"invalid cached digest for {relative}")
        _verify_sha256(executable, expected_digest)
        result[relative] = executable.absolute()
    return result


def _install_asset(
    lock: Mapping[str, object], asset_id: str, selected_platform: str, cache: Path
) -> tuple[dict[str, object], dict[PurePosixPath, Path]]:
    asset = _find_asset(lock, asset_id, selected_platform)
    identity = f"{asset['id']}-{asset['version']}-{asset['platform']}-{asset['sha256']}"
    install_root = (
        cache
        / "tools"
        / str(asset["id"])
        / str(asset["version"])
        / str(asset["platform"])
        / str(asset["sha256"])
    )
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
            for relative in _executable_relatives(asset):
                executable = payload.joinpath(*relative.parts)
                try:
                    resolved = executable.resolve(strict=True)
                except OSError as exc:
                    raise BootstrapError(
                        f"locked executable is absent from {asset_id} archive: {relative}"
                    ) from exc
                if not _inside(payload.resolve(strict=True), resolved) or not resolved.is_file():
                    raise BootstrapError(f"locked executable is unsafe: {relative}")
                if not os.access(executable, os.X_OK):
                    raise BootstrapError(f"locked executable lacks execute permission: {relative}")
                executable_hashes[str(relative)] = _sha256_file(executable)
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
    root = (
        cache
        / "tools"
        / str(asset["id"])
        / str(asset["version"])
        / str(asset["platform"])
        / str(asset["sha256"])
    )
    try:
        return root.resolve(strict=True)
    except OSError as exc:
        raise BootstrapError(f"cannot resolve selected asset root {root}: {exc}") from exc


def _compiler_paths(
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> tuple[dict[str, object], Path, Path]:
    asset, installed = _install_asset(lock, "clang", selected_platform, cache)
    clang = _safe_archive_path(str(asset["executable"]), "locked clang executable")
    return asset, installed[clang], installed[clang.with_name("clang++")]


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
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> tuple[dict[str, object], Path, dict[str, str]]:
    bazelisk_asset, bazelisk = _resolve_tool(lock, "bazelisk", selected_platform, cache)
    bazel_asset, bazel = _resolve_tool(lock, "bazel", selected_platform, cache)
    _, go = _resolve_tool(lock, "go", selected_platform, cache)
    _, cc, cxx = _compiler_paths(lock, selected_platform, cache)
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
        "CC": str(cc),
        "CXX": str(cxx),
    }
    if selected_platform.startswith("darwin-"):
        environment["BAZELISK_HOME_DARWIN"] = str(bazelisk_home)
    else:
        environment["BAZELISK_HOME_LINUX"] = str(bazelisk_home)
    return bazelisk_asset, bazelisk, environment


def _print_environment(
    lock: Mapping[str, object], selected_platform: str, cache: Path
) -> None:
    go_asset, go = _resolve_tool(lock, "go", selected_platform, cache)
    clang_asset, cc, cxx = _compiler_paths(lock, selected_platform, cache)
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
        "CC": str(cc),
        "CXX": str(cxx),
        "BAZELISK": str(bazelisk),
        "SYFT": str(syft),
        "ASSET_ROOTS": [source["path"] for source in asset_sources],
        "ASSET_SOURCES": asset_sources,
        "native_environment": _native_environment(selected_platform),
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


def _native_environment(selected_platform: str) -> dict[str, str]:
    if not selected_platform.startswith("darwin-"):
        return {}
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


def _bazel_native_rc(cache: Path, sdk: str) -> Path:
    # rules_go's child compiler environment does not retain SDKROOT. Put the
    # selected SDK in native Bazel build options, not a second argument parser.
    content = "".join(
        f"build --{option}=-isysroot\nbuild --{option}={json.dumps(sdk)}\n"
        for option in ("copt", "host_copt", "linkopt", "host_linkopt")
    ).encode("utf-8")
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
    if tool in {"bazelisk", "clang", "clang++"} or (tool == "go" and environment.get("CGO_ENABLED") != "0"):
        environment.update(_native_environment(selected_platform))
    if tool == "bazelisk":
        _, executable, additions = _bazelisk_environment(lock, selected_platform, cache)
        environment.update(additions)
        environment.pop("BAZELISK_FORMAT_URL", None)
        argv = [
            str(executable),
            "--nohome_rc",
            "--nosystem_rc",
            f"--output_user_root={_bazel_output_root(cache, selected_platform)}",
            *arguments,
        ]
        if selected_platform.startswith("darwin-"):
            argv.insert(1, f"--bazelrc={_bazel_native_rc(cache, environment['SDKROOT'])}")
    elif tool in {"clang", "clang++"}:
        _, cc, cxx = _compiler_paths(lock, selected_platform, cache)
        executable = cc if tool == "clang" else cxx
        argv = [str(executable), *arguments]
    else:
        _, executable = _resolve_tool(lock, tool, selected_platform, cache)
        argv = [str(executable), *arguments]
        if tool == "go":
            environment["GOTOOLCHAIN"] = "local"
            environment["GOWORK"] = "off"
            environment["GOROOT"] = str(executable.parent.parent)
            environment["PM_GO"] = str(executable)
            environment["PATH"] = str(executable.parent) + os.pathsep + environment.get("PATH", "")
            if environment.get("CGO_ENABLED") != "0":
                _, cc, cxx = _compiler_paths(lock, selected_platform, cache)
                environment["CC"] = str(cc)
                environment["CXX"] = str(cxx)
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
