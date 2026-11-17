#!/usr/bin/env python3
"""Developer tests of the fail paths of scripts/ci/check-ci-image.py (#323).

They use fixture directories and stubs, so they run on any host. They are
developer checks, not acceptance proof. The `image` check runs the real check
inside the job image.
"""
import contextlib
import errno
import importlib.util
import io
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("check_ci_image", ROOT / "scripts/ci/check-ci-image.py")
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)


MISSING = object()


def no_capability(path):
    return False


class CheckCiImage(unittest.TestCase):
    def setUp(self):
        check.failures.clear()
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.output = io.StringIO()
        self.restore = []
        self.saved = {}
        # The fixtures run on any host, so they do not read /proc. Each test
        # that needs the effective capabilities sets them.
        self.setattr_check("effective_capabilities", lambda: 0)

    def tearDown(self):
        for path in self.restore:
            path.chmod(0o700)
        for name, value in self.saved.items():
            if value is MISSING:
                delattr(check, name)
            else:
                setattr(check, name, value)
        self.directory.cleanup()

    def setattr_check(self, name, value):
        self.saved.setdefault(name, getattr(check, name, MISSING))
        setattr(check, name, value)

    def lock(self, path, mode):
        """Set a restrictive directory mode, and restore it before cleanup."""
        self.restore.append(path)
        path.chmod(mode)

    def run_check(self, function, *args, **kwargs):
        with contextlib.redirect_stdout(self.output):
            function(*args, **kwargs)
        return list(check.failures), self.output.getvalue()

    def file(self, relative, mode=0o644):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("fixture\n")
        path.chmod(mode)
        return path

    def assert_failed_without_pass(self, failures, output, text):
        self.assertTrue(any(text in failure for failure in failures), failures)
        self.assertNotIn("PASS", output)

    def test_the_repository_records_parse(self):
        record = check.read_record()
        self.assertEqual(len(record), 72)
        files, closed = check.read_privileged()
        self.assertTrue(files)
        self.assertIn("/root", closed)

    def test_a_clean_fixture_passes(self):
        self.file("usr/bin/tool", 0o755)
        failures, output = self.run_check(check.check_privileges, {}, set(),
                                          root=str(self.root), capability=no_capability)
        self.assertEqual(failures, [])
        self.assertIn("PASS 1 regular files", output)

    def test_an_unrecorded_setuid_file_fails(self):
        # The file of a package outside the record, for example an extra
        # package that a later Dockerfile change adds.
        path = self.file("usr/bin/extra", 0o755)
        path.chmod(0o4755)
        self.assertTrue(path.stat().st_mode & stat.S_ISUID, "the fixture needs a setuid bit")
        failures, output = self.run_check(check.check_privileges, {}, set(),
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "/usr/bin/extra is setuid or setgid (4755")

    def test_a_recorded_setuid_file_with_another_mode_or_missing_fails(self):
        self.file("usr/bin/su", 0o755)
        recorded = {"/usr/bin/su": f"4755 {os.getuid()}:{os.getgid()}"}
        failures, output = self.run_check(check.check_privileges, recorded, set(),
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "/usr/bin/su is in privileged-files.txt")

    def test_a_capability_fails(self):
        self.file("usr/bin/daemon", 0o755)
        failures, output = self.run_check(check.check_privileges, {}, set(), root=str(self.root),
                                          capability=lambda path: path.endswith("/daemon"))
        self.assert_failed_without_pass(failures, output, "/usr/bin/daemon has a file capability")

    def test_an_unreadable_capability_state_fails(self):
        self.file("usr/bin/daemon", 0o755)

        def broken(path):
            raise OSError(errno.ENOTSUP, os.strerror(errno.ENOTSUP), path)

        failures, output = self.run_check(check.check_privileges, {}, set(),
                                          root=str(self.root), capability=broken)
        self.assert_failed_without_pass(failures, output, "/usr/bin/daemon: file capabilities cannot be read")

    def test_the_capability_read_accepts_only_enodata(self):
        def raises(number):
            def getxattr(path, name, follow_symlinks=True):
                raise OSError(number, os.strerror(number), path)
            return getxattr

        original = getattr(os, "getxattr", None)
        try:
            os.getxattr = raises(errno.ENODATA)
            self.assertFalse(check.read_capability("/usr/bin/daemon"))
            os.getxattr = raises(errno.EACCES)
            with self.assertRaises(OSError):
                check.read_capability("/usr/bin/daemon")
            os.getxattr = lambda path, name, follow_symlinks=True: b"\x01"
            self.assertTrue(check.read_capability("/usr/bin/daemon"))
        finally:
            if original is None:
                del os.getxattr
            else:
                os.getxattr = original

    def test_closed_directories_must_match_the_record(self):
        self.assertNotEqual(os.geteuid(), 0, "run the developer tests as a normal user")
        closed = self.root / "root"
        closed.mkdir()
        closed.chmod(0o700 & ~0o500)
        try:
            failures, output = self.run_check(check.check_privileges, {}, {"/var/cache/ldconfig"},
                                              root=str(self.root), capability=no_capability)
        finally:
            closed.chmod(0o700)
        self.assert_failed_without_pass(failures, output, "closed directory /root: the walk cannot read it")
        self.assertTrue(any("closed directory /var/cache/ldconfig: recorded in privileged-files.txt, "
                            "but it does not exist" in failure for failure in failures), failures)

    def test_a_recorded_closed_directory_open_to_others_fails(self):
        (self.root / "home/ubuntu").mkdir(parents=True)
        (self.root / "home/ubuntu").chmod(0o755)
        failures, output = self.run_check(check.check_privileges, {}, {"/home/ubuntu"},
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "closed directory /home/ubuntu: recorded")

    def test_an_activation_file_fails(self):
        services = self.root / "dbus-1/services"
        services.mkdir(parents=True)
        (services / "a.service").write_text("[D-BUS Service]\nName = org.freedesktop.secrets\nExec=/usr/bin/other\n")
        (services / "b.service").write_text("[D-BUS Service]\nName=org.gnome.keyring\nExec=/usr/bin/gnome-keyring-daemon --start\n")
        failures, output = self.run_check(check.check_activation, (str(self.root / "dbus-1"),))
        self.assert_failed_without_pass(failures, output, "a.service activates org.freedesktop.secrets")
        self.assertTrue(any("b.service starts gnome-keyring-daemon" in failure for failure in failures))

    def test_package_differences_fail(self):
        record = {"dbus-daemon": "1.14.10-4ubuntu4.1", "gnome-keyring": "46.1-2ubuntu0.2"}
        installed = {"gnome-keyring": ("46.1-2ubuntu0.1", "ii"), "dbus-x11": ("1.14.10-4ubuntu4.1", "ii")}
        launch = self.file("usr/bin/dbus-launch", 0o755)
        failures, output = self.run_check(check.check_packages, record, installed,
                                          binary_directories=(str(launch.parent),))
        self.assert_failed_without_pass(failures, output, "package dbus-daemon is not installed")
        self.assertTrue(any("package gnome-keyring is 46.1-2ubuntu0.1" in failure for failure in failures))
        self.assertTrue(any("package dbus-x11 is installed" in failure for failure in failures))
        self.assertTrue(any(failure.endswith("/usr/bin/dbus-launch exists") for failure in failures))


    # Review #pr-333 brahmagupta F1 at 3786ed9: a directory that the job user
    # can search but the walk cannot list hides files that the job user can run.
    def test_a_search_only_directory_fails_also_when_recorded(self):
        self.assertNotEqual(os.geteuid(), 0, "run the developer tests as a normal user")
        extra = self.file("root/hidden/extra", 0o755)
        extra.chmod(0o4755)
        self.lock(self.root / "root", 0o311)
        failures, output = self.run_check(check.check_privileges, {}, {"/root"},
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "/root: the job user can search it")

    def test_a_recorded_closed_directory_that_others_can_search_fails(self):
        recorded = self.root / "var/cache/ldconfig"
        recorded.mkdir(parents=True)
        recorded.chmod(0o701)
        failures, output = self.run_check(check.check_privileges, {}, {"/var/cache/ldconfig"},
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "denies search to others")

    # Review #pr-333 brahmagupta F2 at 3786ed9: dbus-launch in a directory on
    # the image PATH that the fixed directory list did not name.
    def test_dbus_launch_anywhere_in_the_walk_fails(self):
        self.file("opt/pmcli/venv/bin/dbus-launch", 0o755)
        failures, output = self.run_check(check.check_privileges, {}, set(),
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "/opt/pmcli/venv/bin/dbus-launch exists")

    # Review #pr-333 brahmagupta F1 at 9d73f24, security cross-review r1
    # items 11 and 12, and the T3-4 residual of the security delta at 90cfd64:
    # one check of the launcher search. It covers each entry of ENV PATH in
    # the Dockerfile and each runtime PATH entry. An empty entry or an unset
    # PATH fails with fixed text. It calls only the PATH check, not the
    # privilege walk.
    def test_the_launcher_search_covers_the_image_and_runtime_path(self):
        dockerfile = (ROOT / "build/ci/Dockerfile").read_text()
        line = next(line for line in dockerfile.splitlines() if line.startswith("ENV PATH="))
        image_path = line.split("=", 1)[1].split()[0].split(":")
        self.assertNotIn("", image_path, "the image PATH must have no empty entry")
        extra = self.root / "extra-bin"
        extra.mkdir()
        saved_path = os.environ.get("PATH")
        if saved_path is None:
            self.addCleanup(os.environ.pop, "PATH", None)
        else:
            self.addCleanup(os.environ.__setitem__, "PATH", saved_path)

        def run(path):
            check.failures.clear()
            self.output = io.StringIO()
            if path is None:
                os.environ.pop("PATH", None)
            else:
                os.environ["PATH"] = path
            return self.run_check(check.check_packages, {}, {})

        os.environ["PATH"] = f"{extra}:/usr/bin"
        directories = check.launcher_directories()
        missing = [entry for entry in (*image_path, str(extra)) if entry not in directories]
        self.assertEqual(missing, [], "the launcher search must cover these directories")
        failures, output = run(f"{extra}:/usr/bin")
        self.assertEqual(failures, [])
        self.assertIn("PASS", output)
        (extra / "dbus-launch").write_text("fixture\n")
        failures, output = run(f"{extra}:/usr/bin")
        self.assertEqual(failures, [f"{extra}/dbus-launch exists"])
        self.assertNotIn("PASS", output)
        (extra / "dbus-launch").unlink()
        for path in (f":{extra}:/usr/bin", f"{extra}:/usr/bin:", f"{extra}::/usr/bin", ""):
            with self.subTest(path=path):
                failures, output = run(path)
                self.assertEqual(failures, ["the runtime PATH has an empty entry"])
                self.assertNotIn("PASS", output)
        with self.subTest(path="unset"):
            failures, output = run(None)
            self.assertEqual(failures, ["the runtime PATH is not set"])
            self.assertNotIn("PASS", output)

    # Security cross-review r1 item 6: a check that fails stops the run before
    # any daemon starts.
    def test_a_failed_check_stops_before_any_daemon_runs(self):
        marker = self.root / "daemon-started"
        daemon = self.root / "bin/dbus-daemon"
        daemon.parent.mkdir(parents=True)
        daemon.write_text(f"#!/bin/sh\n: > '{marker}'\nprintf '%s\\n' 'D-Bus Message Bus Daemon 1.14.10'\n")
        daemon.chmod(0o755)
        self.setattr_check("BINARIES", {str(daemon): "dbus-daemon"})
        self.setattr_check("read_record", lambda: {"dbus-daemon": "1.14.10-4ubuntu4.1"})
        self.setattr_check("read_privileged", lambda: ({}, set()))
        self.setattr_check("installed_packages", lambda: {})
        self.setattr_check("check_packages", lambda record, installed: None)
        self.setattr_check("check_binaries", lambda: check.fail(f"{daemon} has mode -rwsr-xr-x"))
        self.setattr_check("check_activation", lambda: None)
        self.setattr_check("check_privileges", lambda recorded, recorded_closed: None)
        real_exists, real_platform = os.path.exists, sys.platform
        self.addCleanup(setattr, os.path, "exists", real_exists)
        self.addCleanup(setattr, sys, "platform", real_platform)
        os.path.exists = lambda path: path == "/run/.containerenv" or real_exists(path)
        sys.platform = "linux"
        with self.assertRaises(SystemExit) as stopped:
            self.run_check(check.main)
        self.assertNotEqual(stopped.exception.code, 0)
        self.assertFalse(marker.exists(), "the rejected daemon ran")

    # Security cross-review r1 item 8: a version failure prints fixed text,
    # never the daemon output.
    def test_version_output_is_not_printed(self):
        marker = "PMCI-MARKER-0001"
        daemon = self.root / "bin/dbus-daemon"
        daemon.parent.mkdir(parents=True)
        daemon.write_text(f"#!/bin/sh\nprintf '%s\\n' '{marker}'\nprintf '%s\\n' '{marker}' >&2\n")
        daemon.chmod(0o755)
        self.setattr_check("BINARIES", {str(daemon): "dbus-daemon"})
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            failures, output = self.run_check(check.check_versions, {"dbus-daemon": "1.14.10-4ubuntu4.1"})
        self.assert_failed_without_pass(failures, output, "is not exactly")
        for text in (output, errors.getvalue(), *failures):
            self.assertNotIn(marker, text)

    # Security cross-review r1 item 9: the job user must not be able to
    # restore access to a closed directory.
    def test_a_closed_directory_that_the_job_user_owns_fails(self):
        self.assertNotEqual(os.geteuid(), 0, "run the developer tests as a normal user")
        hidden = self.file("var/lib/private/extra", 0o755)
        hidden.chmod(0o4755)
        self.lock(self.root / "var/lib/private", 0o000)
        failures, output = self.run_check(check.check_privileges, {}, {"/var/lib/private"},
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output,
                                        "/var/lib/private: the job user owns it, so it can restore access")

    # Security cross-review r1 item 9 and N2 of the security delta at
    # 90cfd64: when a closed directory exists, any non-zero CapEff fails.
    def test_a_nonzero_capeff_with_a_closed_directory_fails(self):
        closed = self.root / "root"
        closed.mkdir()
        self.lock(closed, 0o000)
        for mask, text in ((1 << 3, "CapEff 0000000000000008"), (1 << 21, "CapEff 0000000000200000")):
            with self.subTest(mask=hex(mask)):
                check.failures.clear()
                self.output = io.StringIO()
                self.setattr_check("effective_capabilities", lambda mask=mask: mask)
                failures, output = self.run_check(check.check_privileges, {}, {"/root"},
                                                  root=str(self.root), capability=no_capability)
                self.assert_failed_without_pass(failures, output, text)

    def test_a_nonzero_capeff_without_a_closed_directory_passes(self):
        self.file("usr/bin/tool", 0o755)
        self.setattr_check("effective_capabilities", lambda: 1 << 21)
        failures, output = self.run_check(check.check_privileges, {}, set(),
                                          root=str(self.root), capability=no_capability)
        self.assertEqual(failures, [])
        self.assertIn("PASS", output)

    def test_an_unreadable_capeff_with_a_closed_directory_fails(self):
        closed = self.root / "root"
        closed.mkdir()
        self.lock(closed, 0o000)

        def unreadable():
            raise OSError(errno.ENOENT, os.strerror(errno.ENOENT), "/proc/self/status")

        self.setattr_check("effective_capabilities", unreadable)
        failures, output = self.run_check(check.check_privileges, {}, {"/root"},
                                          root=str(self.root), capability=no_capability)
        self.assert_failed_without_pass(failures, output, "cannot be read from /proc/self/status")

    def test_the_effective_capabilities_are_read_from_capeff(self):
        status = self.root / "status"
        read = self.saved["effective_capabilities"]  # the real reader; setUp stubs it
        status.write_text("Name:\tpython3\nCapEff:\t0000000000200008\n")
        self.assertEqual(read(str(status)), (1 << 21) | (1 << 3))
        status.write_text("CapEff:\t0000000000000000\n")
        self.assertEqual(read(str(status)), 0)
        status.write_text("Name:\tpython3\n")
        with self.assertRaises(ValueError):
            read(str(status))

    # Review #pr-333 brahmagupta F3 at 3786ed9: a symlinked service directory
    # and an unreadable service directory.
    def test_a_symlinked_service_directory_is_read(self):
        target = self.root / "elsewhere"
        target.mkdir()
        (target / "a.service").write_text("[D-BUS Service]\nName=org.freedesktop.secrets\n")
        (self.root / "dbus-1").mkdir()
        (self.root / "dbus-1/services").symlink_to(target)
        (target / "loop").symlink_to(target)
        failures, output = self.run_check(check.check_activation, (str(self.root / "dbus-1"),))
        self.assert_failed_without_pass(failures, output, "a.service activates org.freedesktop.secrets")
        self.assertEqual(len(failures), 1, failures)

    def test_an_unreadable_service_directory_fails(self):
        services = self.root / "dbus-1/services"
        services.mkdir(parents=True)
        self.lock(services, 0o000)
        failures, output = self.run_check(check.check_activation, (str(self.root / "dbus-1"),))
        self.assert_failed_without_pass(failures, output, "dbus-1/services cannot be read")

    def test_a_missing_service_root_is_not_a_failure(self):
        failures, output = self.run_check(check.check_activation, (str(self.root / "absent"),))
        self.assertEqual(failures, [])
        self.assertIn("PASS 0 D-Bus service files", output)

    # Review #pr-333 brahmagupta F4 at 3786ed9: the reported version must be
    # exact, and a failed or stopped command fails.
    def fake_daemon(self, text, code=0, wait=False):
        path = self.root / "bin/dbus-daemon"
        path.parent.mkdir(parents=True, exist_ok=True)
        body = f"printf '%s\\n' '{text}'\n"
        if wait:
            fifo = self.root / "never-written"
            os.mkfifo(fifo)
            body += f"exec cat '{fifo}'\n"
        path.write_text(f"#!/bin/sh\n{body}exit {code}\n")
        path.chmod(0o755)
        self.setattr_check("BINARIES", {str(path): "dbus-daemon"})
        return {"dbus-daemon": "1.14.10-4ubuntu4.1"}

    def test_the_exact_version_passes(self):
        record = self.fake_daemon("D-Bus Message Bus Daemon 1.14.10")
        failures, output = self.run_check(check.check_versions, record)
        self.assertEqual(failures, [])
        self.assertIn("PASS", output)

    def test_a_longer_version_with_the_same_prefix_fails(self):
        record = self.fake_daemon("D-Bus Message Bus Daemon 1.14.100")
        failures, output = self.run_check(check.check_versions, record)
        self.assert_failed_without_pass(failures, output, "--version")

    def test_a_wrong_version_fails(self):
        record = self.fake_daemon("D-Bus Message Bus Daemon 1.14.9")
        failures, output = self.run_check(check.check_versions, record)
        self.assert_failed_without_pass(failures, output, "--version")

    def test_a_nonzero_exit_fails(self):
        record = self.fake_daemon("D-Bus Message Bus Daemon 1.14.10", code=3)
        failures, output = self.run_check(check.check_versions, record)
        self.assert_failed_without_pass(failures, output, "exited with 3")

    def test_a_version_command_that_does_not_end_fails(self):
        record = self.fake_daemon("D-Bus Message Bus Daemon 1.14.10", wait=True)
        if hasattr(check, "VERSION_TIMEOUT"):
            self.setattr_check("VERSION_TIMEOUT", 1)
        failures, output = self.run_check(check.check_versions, record)
        self.assert_failed_without_pass(failures, output, "did not finish")


if __name__ == "__main__":
    unittest.main()
