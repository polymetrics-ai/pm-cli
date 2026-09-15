#!/usr/bin/env python3
"""Exercise real bootstrap refusal without network or a system tool installation."""
from io import BytesIO
import hashlib
import json
import multiprocessing
import os
from pathlib import Path, PurePosixPath
import runpy
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
bootstrap = runpy.run_path(str(ROOT / "tools/bootstrap.py"))
BootstrapError = bootstrap["BootstrapError"]
capture_native_environment = bootstrap["capture_native_environment"]
verify_native_environment = bootstrap["verify_native_environment"]

NATIVE_PLATFORM = "linux-amd64"
NATIVE_GCC_INSTALL_DIR = "/usr/lib/gcc/x86_64-linux-gnu/13"
NATIVE_TREES = ("/usr/include", NATIVE_GCC_INSTALL_DIR)
NATIVE_NULL_GCC_TREES = ("/usr/include",)
NATIVE_FILES = (
    "/etc/os-release",
    "/usr/lib/libduckdb_static.a",
    "/usr/lib/x86_64-linux-gnu/libgcc_s.so.1",
    "/usr/lib/x86_64-linux-gnu/libstdc++.so.6",
)


def _native_environment_child(sender, action, root, profile, linker):
    try:
        if action == "capture":
            capture_native_environment(
                Path(root),
                NATIVE_PLATFORM,
                NATIVE_GCC_INSTALL_DIR,
                NATIVE_TREES,
                NATIVE_FILES,
                linker,
            )
        elif action == "verify":
            verify_native_environment(Path(root), profile)
        else:
            raise AssertionError(f"unknown native-environment action: {action}")
    except BootstrapError as exc:
        outcome = ("bootstrap-error", str(exc))
    except Exception as exc:
        outcome = ("unexpected-error", f"{type(exc).__name__}: {exc}")
    else:
        outcome = ("accepted", "")
    try:
        sender.send(outcome)
    finally:
        sender.close()


class BootstrapBoundaryTest(unittest.TestCase):
    @staticmethod
    def _locked_native_linker():
        lock = json.loads((ROOT / "build/toolchains.lock.json").read_text())
        asset = next(
            item
            for item in lock["assets"]
            if item["id"] == "clang" and item["platform"] == NATIVE_PLATFORM
        )
        linker = PurePosixPath(asset["executable"]).with_name("ld.lld")
        return {
            "asset_id": asset["id"],
            "executable": str(linker),
            "source_sha256": asset["sha256"],
        }

    @staticmethod
    def _write_native_fixture(root, include_gcc=True):
        contents = {
            "etc/os-release": b'ID=pm-test\nVERSION_ID="1"\n',
            "usr/include/pm/api.h": b"#define PM_API 1\n",
            "usr/include/pm/primary.h": b"#define PM_SELECTED 1\n",
            "usr/include/pm/alternate.h": b"#define PM_SELECTED 1\n",
            "usr/lib/libc.so": b"synthetic libc\n",
            "usr/lib/libduckdb_static.a": b"synthetic duckdb archive\n",
            "usr/lib/x86_64-linux-gnu/libgcc_s.so.1.0": b"synthetic libgcc_s.so\n",
            "usr/lib/x86_64-linux-gnu/libstdc++.so.6.0.33": b"synthetic libstdc++.so\n",
            "usr/lib/x86_64-linux-gnu/libstdc++.so.6.0.34": b"synthetic libstdc++.so\n",
        }
        if include_gcc:
            contents.update(
                {
                    "usr/lib/gcc/x86_64-linux-gnu/13/crtbegin.o": b"synthetic crtbegin\n",
                    "usr/lib/gcc/x86_64-linux-gnu/13/crtend.o": b"synthetic crtend\n",
                    "usr/lib/gcc/x86_64-linux-gnu/13/libgcc.a": b"synthetic libgcc\n",
                    "usr/lib/gcc/x86_64-linux-gnu/13/libgcc_s.so": b"synthetic libgcc_s\n",
                    "usr/lib/gcc/x86_64-linux-gnu/13/libstdc++.a": b"synthetic libstdc++\n",
                }
            )
        for relative, content in contents.items():
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        library_directory = root / "usr/lib/x86_64-linux-gnu"
        selected_libgcc = library_directory / "libgcc_s.so.1"
        selected_libgcc.symlink_to("libgcc_s.so.1.0")
        selected_libstdcpp = library_directory / "libstdc++.so.6"
        selected_libstdcpp.symlink_to("libstdc++.so.6.0.33")
        selected = root / "usr/include/pm/selected.h"
        selected.symlink_to("primary.h")
        paths = {
            "api_header": root / "usr/include/pm/api.h",
            "alternate_header": root / "usr/include/pm/alternate.h",
            "duckdb_archive": root / "usr/lib/libduckdb_static.a",
            "os_release": root / "etc/os-release",
            "primary_header": root / "usr/include/pm/primary.h",
            "selected_header": selected,
            "alternate_libstdcpp": library_directory / "libstdc++.so.6.0.34",
            "primary_libstdcpp": library_directory / "libstdc++.so.6.0.33",
            "selected_libstdcpp": selected_libstdcpp,
        }
        if include_gcc:
            paths["libgcc"] = root / "usr/lib/gcc/x86_64-linux-gnu/13/libgcc.a"
        return paths

    def _capture_native_fixture(
        self,
        root,
        gcc_install_dir=NATIVE_GCC_INSTALL_DIR,
        toolchain_lock=None,
    ):
        return capture_native_environment(
            root,
            NATIVE_PLATFORM,
            gcc_install_dir,
            NATIVE_TREES if gcc_install_dir is not None else NATIVE_NULL_GCC_TREES,
            NATIVE_FILES,
            self._locked_native_linker(),
            toolchain_lock=toolchain_lock,
        )

    def _assert_native_fixture_unchanged(self, root, profile):
        try:
            verify_native_environment(root, profile)
        except BootstrapError as exc:
            self.fail(f"freshly captured native fixture did not verify: {exc}")

    def _native_call_without_blocking(self, action, root, profile):
        context = multiprocessing.get_context("fork")
        receiver, sender = context.Pipe(duplex=False)
        process = context.Process(
            target=_native_environment_child,
            args=(
                sender,
                action,
                str(root),
                profile,
                self._locked_native_linker(),
            ),
        )
        process.start()
        sender.close()
        try:
            if not receiver.poll(3):
                self.fail(f"native-environment {action} blocked on a special file")
            try:
                outcome = receiver.recv()
            except EOFError:
                process.join(1)
                self.fail(
                    f"native-environment {action} exited without reporting an outcome "
                    f"(exit code {process.exitcode})"
                )
            process.join(1)
            if process.is_alive():
                self.fail(f"native-environment {action} did not exit after reporting")
            self.assertEqual(process.exitcode, 0)
            return outcome
        finally:
            if process.is_alive():
                process.terminate()
                process.join(2)
            receiver.close()

    def test_wrong_source_digest_prevents_execution(self):
        cache_parent = ROOT / ".cache"
        cache_parent.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="bootstrap-test-", dir=cache_parent) as directory:
            root = Path(directory)
            (root / "tools").mkdir()
            (root / "build").mkdir()
            (root / "tools/bootstrap.py").write_bytes((ROOT / "tools/bootstrap.py").read_bytes())
            lock = json.loads((ROOT / "build/toolchains.lock.json").read_text())
            platform = bootstrap["_selected_platform"]()
            asset = next(a for a in lock["assets"] if a["id"] == "go" and a["platform"] == platform)
            marker = root / "executed"
            # A synthetic SDK makes any unintended execution observable, not qualified.
            program = b'#!/bin/sh\nprintf executed > "$PM_BOOTSTRAP_TEST_MARKER"\n'
            archive = root / "sdk.tar.gz"
            with tarfile.open(archive, "w:gz") as output:
                entry = tarfile.TarInfo(asset["executable"])
                entry.mode = 0o755
                entry.size = len(program)
                output.addfile(entry, BytesIO(program))
            payload = archive.read_bytes()
            valid_digest = hashlib.sha256(payload).hexdigest()
            cache = root / "cache"
            downloads = cache / "downloads"
            downloads.mkdir(parents=True)
            command = [sys.executable, "-I", "-B", str(root / "tools/bootstrap.py"),
                       "exec", "go", "--", "version"]
            environment = dict(os.environ, PM_TOOLS_CACHE=str(cache), CGO_ENABLED="0",
                               PM_BOOTSTRAP_TEST_MARKER=str(marker))
            for digest, accepted in ((valid_digest, True), ("0" * 64, False)):
                with self.subTest(accepted=accepted):
                    marker.unlink(missing_ok=True)
                    asset["sha256"] = digest
                    (root / "build/toolchains.lock.json").write_text(json.dumps(lock))
                    (downloads / f"{digest}.blob").write_bytes(payload)
                    result = subprocess.run(command, cwd=root, env=environment,
                                            capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0 if accepted else 1,
                                     result.stdout + result.stderr)
                    self.assertEqual(marker.exists(), accepted)

    def test_archive_extraction_cannot_escape_its_destination(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = b"archive bytes"
            for kind in ("tar.gz", "zip"):
                for name, accepted in (("inside", True), ("../escaped", False)):
                    with self.subTest(kind=kind, name=name):
                        archive = root / f"input.{kind}"
                        if kind == "tar.gz":
                            with tarfile.open(archive, "w:gz") as output:
                                entry = tarfile.TarInfo(name)
                                entry.size = len(payload)
                                output.addfile(entry, BytesIO(payload))
                        else:
                            with zipfile.ZipFile(archive, "w") as output:
                                output.writestr(name, payload)
                        destination = root / f"extract-{kind}-{accepted}"
                        if accepted:
                            bootstrap["_extract_archive"](archive, kind, destination)
                            self.assertEqual((destination / name).read_bytes(), payload)
                        else:
                            with self.assertRaises(BootstrapError):
                                bootstrap["_extract_archive"](archive, kind, destination)
                            self.assertFalse((root / "escaped").exists())
                            self.assertFalse(destination.exists())

    def test_unsupported_platform_has_no_fallback(self):
        with self.assertRaises(BootstrapError):
            bootstrap["_normalize_platform"]("windows", "amd64")

    def test_native_environment_rejects_hashed_file_byte_drift(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)

            original_header = paths["api_header"].read_bytes()
            paths["api_header"].write_bytes(b"#define PM_API 2\n")
            self.assertNotEqual(paths["api_header"].read_bytes(), original_header)
            with self.assertRaises(
                BootstrapError, msg="changed header bytes were accepted"
            ):
                verify_native_environment(root, profile)

            paths["api_header"].write_bytes(original_header)
            self._assert_native_fixture_unchanged(root, profile)
            original_os_release = paths["os_release"].read_bytes()
            paths["os_release"].write_bytes(original_os_release + b"DRIFT=1\n")
            self.assertNotEqual(paths["os_release"].read_bytes(), original_os_release)
            with self.assertRaises(
                BootstrapError, msg="changed OS identity bytes were accepted"
            ):
                verify_native_environment(root, profile)

            paths["os_release"].write_bytes(original_os_release)
            self._assert_native_fixture_unchanged(root, profile)
            original_libgcc = paths["libgcc"].read_bytes()
            paths["libgcc"].write_bytes(original_libgcc + b"runtime drift\n")
            self.assertNotEqual(paths["libgcc"].read_bytes(), original_libgcc)
            with self.assertRaises(
                BootstrapError, msg="changed GNU runtime bytes were accepted"
            ):
                verify_native_environment(root, profile)

    def test_native_environment_null_gcc_profile_still_binds_sysroot(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root, include_gcc=False)
            gcc_directory = root / NATIVE_GCC_INSTALL_DIR.removeprefix("/")
            self.assertFalse(gcc_directory.exists())
            # Null records the host profile only; execution must verify compiler-rt.
            profile = self._capture_native_fixture(root, gcc_install_dir=None)
            self.assertIsNone(profile["gcc_install_dir"])
            self._assert_native_fixture_unchanged(root, profile)

            paths["api_header"].write_bytes(b"#define PM_API NULL_GCC_DRIFT\n")
            with self.assertRaises(
                BootstrapError, msg="null-GCC profile accepted changed sysroot bytes"
            ):
                verify_native_environment(root, profile)

    def test_native_environment_rejects_added_or_deleted_tree_members(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)

            added = root / "usr/include/pm/unreviewed.h"
            added.write_bytes(b"#define UNREVIEWED 1\n")
            self.assertTrue(added.is_file())
            with self.assertRaises(
                BootstrapError, msg="added header membership was accepted"
            ):
                verify_native_environment(root, profile)

            added.unlink()
            self._assert_native_fixture_unchanged(root, profile)
            paths["api_header"].unlink()
            self.assertFalse(paths["api_header"].exists())
            with self.assertRaises(
                BootstrapError, msg="deleted header membership was accepted"
            ):
                verify_native_environment(root, profile)

    def test_native_environment_rejects_symlink_target_drift(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)
            self.assertEqual(
                paths["primary_header"].read_bytes(),
                paths["alternate_header"].read_bytes(),
            )

            paths["selected_header"].unlink()
            paths["selected_header"].symlink_to("alternate.h")
            self.assertEqual(os.readlink(paths["selected_header"]), "alternate.h")
            with self.assertRaises(
                BootstrapError, msg="changed symlink target was accepted"
            ):
                verify_native_environment(root, profile)

            paths["selected_header"].unlink()
            paths["selected_header"].symlink_to("primary.h")
            self._assert_native_fixture_unchanged(root, profile)
            self.assertEqual(
                paths["primary_libstdcpp"].read_bytes(),
                paths["alternate_libstdcpp"].read_bytes(),
            )
            paths["selected_libstdcpp"].unlink()
            paths["selected_libstdcpp"].symlink_to("libstdc++.so.6.0.34")
            self.assertEqual(
                os.readlink(paths["selected_libstdcpp"]), "libstdc++.so.6.0.34"
            )
            with self.assertRaises(
                BootstrapError, msg="changed selected-library target was accepted"
            ):
                verify_native_environment(root, profile)

    def test_native_environment_rejects_symlink_escape(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)

            outside = root.parent / "outside.so"
            outside.write_bytes(paths["primary_libstdcpp"].read_bytes())
            escape_target = os.path.relpath(outside, paths["selected_libstdcpp"].parent)
            paths["selected_libstdcpp"].unlink()
            paths["selected_libstdcpp"].symlink_to(escape_target)
            self.assertEqual(
                paths["selected_libstdcpp"].resolve(strict=True),
                outside.resolve(strict=True),
            )
            with self.assertRaises(
                BootstrapError, msg="escaping symlink was accepted during verification"
            ):
                verify_native_environment(root, profile)
            with self.assertRaises(
                BootstrapError, msg="escaping symlink was accepted during capture"
            ):
                self._capture_native_fixture(root)

    def test_native_environment_rejects_special_files_without_opening_them(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            paths = self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)

            paths["duckdb_archive"].unlink()
            os.mkfifo(paths["duckdb_archive"])
            self.assertTrue(stat.S_ISFIFO(paths["duckdb_archive"].lstat().st_mode))
            for action in ("capture", "verify"):
                with self.subTest(action=action):
                    outcome, detail = self._native_call_without_blocking(
                        action, root, profile
                    )
                    self.assertEqual(
                        outcome,
                        "bootstrap-error",
                        f"native-environment {action} accepted a FIFO: {detail}",
                    )

    def test_native_environment_honors_supplied_toolchain_lock(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            self._write_native_fixture(root)
            reviewed_lock = json.loads(
                (ROOT / "build/toolchains.lock.json").read_text()
            )
            profile = self._capture_native_fixture(
                root, toolchain_lock=reviewed_lock
            )
            self._assert_native_fixture_unchanged(root, profile)

            conflicting_lock = json.loads(json.dumps(reviewed_lock))
            asset = next(
                item
                for item in conflicting_lock["assets"]
                if item["id"] == "clang" and item["platform"] == NATIVE_PLATFORM
            )
            asset["sha256"] = "0" * 64
            with self.assertRaises(
                BootstrapError, msg="capture ignored its supplied toolchain lock"
            ):
                self._capture_native_fixture(
                    root, toolchain_lock=conflicting_lock
                )

    def test_native_environment_rejects_malformed_profile_boundaries(self):
        with tempfile.TemporaryDirectory(prefix="native-environment-test-") as directory:
            root = Path(directory) / "root"
            self._write_native_fixture(root)
            profile = self._capture_native_fixture(root)
            self._assert_native_fixture_unchanged(root, profile)

            extra_key = json.loads(json.dumps(profile))
            extra_key["unreviewed"] = True
            broadened_scope = json.loads(json.dumps(profile))
            broadened_scope["qualification_scope"] = "immutable-image-and-release"
            unsupported_platform = json.loads(json.dumps(profile))
            unsupported_platform["platform"] = "linux-riscv64"
            ambient_linker = json.loads(json.dumps(profile))
            ambient_linker["linker"]["executable"] = "/usr/bin/ld"
            missing_gcc = json.loads(json.dumps(profile))
            missing_gcc["gcc_install_dir"] = "/usr/lib/gcc/x86_64-linux-gnu/missing"
            for case, malformed in (
                ("unknown profile key", extra_key),
                ("broadened qualification scope", broadened_scope),
                ("unsupported platform", unsupported_platform),
                ("ambient linker executable", ambient_linker),
                ("missing GCC runtime directory", missing_gcc),
            ):
                with self.subTest(case=case):
                    with self.assertRaises(
                        BootstrapError, msg=f"{case} was accepted"
                    ):
                        verify_native_environment(root, malformed)

            wrong_source = self._locked_native_linker()
            wrong_source["source_sha256"] = "0" * 64
            with self.assertRaises(
                BootstrapError, msg="unlocked linker source was accepted during capture"
            ):
                capture_native_environment(
                    root,
                    NATIVE_PLATFORM,
                    NATIVE_GCC_INSTALL_DIR,
                    NATIVE_TREES,
                    NATIVE_FILES,
                    wrong_source,
                )

if __name__ == "__main__":
    unittest.main()
