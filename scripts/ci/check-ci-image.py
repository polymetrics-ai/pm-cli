#!/usr/bin/env python3
"""Check the keyring layer of the CI job image (#323, D1-9).

The `image` check of scripts/ci/check-docs.sh and scripts/ci/check-bdd.sh run
this script inside the pinned job image (scripts/ci/in-ci-image.sh). Outside a
container it fails, because the host is not the image. It starts no bus and no
keyring service. It runs only dpkg-query and `--version` of the two daemons.

The check fails when one of these items is false:
- Each package of build/ci/keyring-packages.txt is installed at its version.
- dbus-x11 is not installed. No file or link named dbus-launch is in a
  binary directory, in a directory of the runtime PATH, or anywhere in the
  whole-root walk below.
- /usr/bin/dbus-daemon and /usr/bin/gnome-keyring-daemon are executable
  regular files of the packages dbus-daemon and gnome-keyring.
- No D-Bus service file names org.freedesktop.secrets or starts
  gnome-keyring-daemon. The walk of the service directories follows directory
  symlinks, reads each resolved directory once, and fails on a directory that
  it cannot read.
- On the root file system of the image (one device, as `find -xdev`), no file
  has a file capability (the security.capability extended attribute), and the
  setuid and setgid regular files are exactly the entries of
  build/ci/privileged-files.txt, with their mode and owner. A directory that
  the job user can search but the walk cannot list fails, because the job user
  can run a file in it by its name. Each directory that the job user cannot
  search is a recorded closed directory, and each recorded closed directory
  denies search to others by its mode. The access tests use the effective
  access of the job user (owner, group, other and ACLs), and the mode test does
  not depend on the user ID of the job. Each difference is printed.
- `dbus-daemon --version` and `gnome-keyring-daemon --version` exit with code 0
  within VERSION_TIMEOUT seconds, and the first line of their output is
  exactly the version line with the upstream version of the recorded package.

scripts/ci/test_ci_image.py tests the fail paths with fixtures. That test is a
developer check, not acceptance proof.
"""
import errno
import os
from pathlib import Path
import stat
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
RECORD = ROOT / "build/ci/keyring-packages.txt"
PRIVILEGED = ROOT / "build/ci/privileged-files.txt"
BINARIES = {"/usr/bin/dbus-daemon": "dbus-daemon",
            "/usr/bin/gnome-keyring-daemon": "gnome-keyring"}
ABSENT_PACKAGES = ("dbus-x11",)
ABSENT_FILE = "dbus-launch"
# The image PATH (build/ci/Dockerfile ENV PATH) and /usr/libexec. The runtime
# PATH is added when the check runs.
BINARY_DIRECTORIES = ("/opt/pmcli/venv/bin", "/opt/pmcli/bin", "/usr/local/sbin", "/usr/local/bin",
                      "/usr/sbin", "/usr/bin", "/sbin", "/bin", "/usr/libexec")
SERVICE_DIRECTORIES = ("/usr/share/dbus-1", "/usr/local/share/dbus-1", "/etc/dbus-1")
SECRETS_NAME = "org.freedesktop.secrets"
KEYRING_DAEMON = "gnome-keyring-daemon"
CAPABILITY = "security.capability"
COMMAND_ENVIRONMENT = {"PATH": "/usr/bin:/bin", "LANG": "C"}
# The first line that `--version` prints, from the format strings in the
# binaries of the snapshot packages: "D-Bus Message Bus Daemon %s" in
# dbus-daemon 1.14.10-4ubuntu4.1 and "gnome-keyring-daemon: %s" in
# gnome-keyring 46.1-2ubuntu0.2.
VERSION_LINES = {"dbus-daemon": "D-Bus Message Bus Daemon {}",
                 "gnome-keyring": "gnome-keyring-daemon: {}"}
VERSION_TIMEOUT = 10

failures = []


def fail(message):
    failures.append(message)
    print(f"check-ci-image: FAIL {message}", flush=True)


def passed(start, message):
    """Print a PASS line only when no failure was recorded since start."""
    if len(failures) == start:
        print(f"check-ci-image: PASS {message}", flush=True)


def query(*arguments):
    result = subprocess.run(["dpkg-query", *arguments], capture_output=True, text=True,
                            env=COMMAND_ENVIRONMENT, timeout=60, check=False)
    return result.returncode, result.stdout


def content_lines(path):
    for number, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if line and not line.startswith("#"):
            yield number, line.split()


def read_record(path=RECORD):
    packages = {}
    for number, fields in content_lines(path):
        if len(fields) != 2 or fields[0] in packages:
            raise SystemExit(f"check-ci-image: {path.name}:{number}: need one unique 'name version' pair")
        packages[fields[0]] = fields[1]
    if not set(BINARIES.values()) <= set(packages):
        raise SystemExit(f"check-ci-image: {path.name} does not record the daemon packages")
    return packages


def read_privileged(path=PRIVILEGED):
    """Return the recorded setuid and setgid files and the closed directories.

    A `setid MODE UID:GID PATH` line records a regular file with its octal mode
    (with the setuid and setgid bits) and its numeric owner. A `closed PATH`
    line records a directory whose mode denies search to others.
    """
    files, closed = {}, set()
    for number, fields in content_lines(path):
        if fields[0] == "setid" and len(fields) == 4 and fields[3] not in files:
            files[fields[3]] = f"{fields[1]} {fields[2]}"
        elif fields[0] == "closed" and len(fields) == 2 and fields[1] not in closed:
            closed.add(fields[1])
        else:
            raise SystemExit(f"check-ci-image: {path.name}:{number}: need 'setid MODE UID:GID PATH' or 'closed PATH', each path once")
    return files, closed


def installed_packages():
    code, output = query("--show", "--showformat=${Package}\t${Version}\t${db:Status-Abbrev}\n")
    if code != 0:
        raise SystemExit("check-ci-image: dpkg-query --show failed")
    packages = {}
    for line in output.splitlines():
        name, version, status = line.split("\t")
        # The second letter is the package state: n is not installed, c is
        # configuration files only. Any other state leaves package files.
        if status[1:2] not in ("n", "c"):
            packages[name] = (version, status.strip())
    return packages


def launcher_directories():
    """Return BINARY_DIRECTORIES and each directory of the runtime PATH."""
    runtime = [entry for entry in os.environ.get("PATH", "").split(":") if entry]
    return tuple(dict.fromkeys((*BINARY_DIRECTORIES, *runtime)))


def check_packages(record, installed, binary_directories=None):
    start = len(failures)
    if binary_directories is None:
        binary_directories = launcher_directories()
    for name, version in record.items():
        have = installed.get(name)
        if have is None:
            fail(f"package {name} is not installed")
        elif have != (version, "ii"):
            fail(f"package {name} is {have[0]} ({have[1]}), the record needs {version} (ii)")
    for name in ABSENT_PACKAGES:
        if name in installed:
            fail(f"package {name} is installed")
    for directory in binary_directories:
        path = os.path.join(directory, ABSENT_FILE)
        if os.path.lexists(path):
            fail(f"{path} exists")
    passed(start, f"{len(record)} recorded packages checked; {', '.join(ABSENT_PACKAGES)} and {ABSENT_FILE} checked absent")


def check_binaries():
    start = len(failures)
    for path, package in BINARIES.items():
        try:
            information = os.lstat(path)
        except FileNotFoundError:
            fail(f"{path} does not exist")
            continue
        if not stat.S_ISREG(information.st_mode) or not information.st_mode & 0o111:
            fail(f"{path} is not an executable regular file")
            continue
        code, output = query("--search", path)
        owners = output.split(":", 1)[0].split(", ") if code == 0 else []
        if owners != [package]:
            fail(f"{path} belongs to {owners or 'no package'}, not {package}")
    passed(start, "/usr/bin/dbus-daemon and /usr/bin/gnome-keyring-daemon are executable regular files of their packages")


def check_activation(directories=SERVICE_DIRECTORIES):
    start, count = len(failures), 0
    visited = set()

    def unreadable(error):
        fail(f"{error.filename} cannot be read: {error.strerror}")

    for directory in directories:
        # A service root that does not exist holds no service file. A link
        # that points nowhere is not skipped: the walk reports it.
        if not os.path.lexists(directory):
            continue
        for current, subdirectories, names in os.walk(directory, onerror=unreadable, followlinks=True):
            # Follow directory symlinks, so a symlinked service directory is
            # read. Read each resolved directory once, so a symlink loop ends.
            resolved = os.path.realpath(current)
            if resolved in visited:
                subdirectories[:] = []
                continue
            visited.add(resolved)
            for name in names:
                if not name.endswith(".service"):
                    continue
                path = os.path.join(current, name)
                count += 1
                try:
                    lines = Path(path).read_text(errors="replace").splitlines()
                except OSError as error:
                    fail(f"{path} cannot be read: {error.strerror}")
                    continue
                for line in lines:
                    key, _, value = line.partition("=")
                    key, value = key.strip(), value.strip()
                    if key == "Name" and value == SECRETS_NAME:
                        fail(f"{path} activates {SECRETS_NAME}")
                    if key == "Exec" and KEYRING_DAEMON in value:
                        fail(f"{path} starts {KEYRING_DAEMON}")
    passed(start, f"{count} D-Bus service files checked: none activates {SECRETS_NAME} or {KEYRING_DAEMON}")


def read_capability(path):
    """Return True when path has a file capability. Raise OSError when unknown."""
    try:
        os.getxattr(path, CAPABILITY, follow_symlinks=False)
    except OSError as error:
        if error.errno == errno.ENODATA:
            return False
        raise
    return True


def can_access(path, mode):
    """Test the effective access of the job user: owner, group, other and ACLs."""
    return os.access(path, mode, effective_ids=os.access in os.supports_effective_ids)


def scan_privileges(root="/", capability=read_capability):
    """Walk the file system of root on its own device, as `find -xdev`.

    Return (setid, closed, searchable, launchers, count): the setuid and
    setgid regular files as {path: "MODE UID:GID"}, the directories that the
    job user cannot search, the directories that the job user can search but
    the walk cannot list, each file, link or directory named dbus-launch, and
    the number of regular files. A file with a capability, or with an
    unknown capability state, is a failure. Paths are absolute in the image, so
    root can be a fixture directory.
    """
    device = os.lstat(root).st_dev
    setid, closed, searchable, launchers, count = {}, set(), set(), set(), 0

    def image_path(path):
        relative = os.path.relpath(path, root)
        return "/" if relative == "." else "/" + relative

    def unreadable(error):
        if can_access(error.filename, os.X_OK):
            searchable.add(image_path(error.filename))
        else:
            closed.add(image_path(error.filename))

    for current, directories, names in os.walk(root, onerror=unreadable):
        for name in (*directories, *names):
            if name == ABSENT_FILE:
                launchers.add(image_path(os.path.join(current, name)))
        kept = []
        for name in directories:
            path = os.path.join(current, name)
            information = os.lstat(path)
            if information.st_dev != device:
                continue
            if not stat.S_ISLNK(information.st_mode) and not can_access(path, os.R_OK | os.X_OK):
                # The job user can run a file in a directory that it can search,
                # also when it cannot list the directory.
                if can_access(path, os.X_OK):
                    searchable.add(image_path(path))
                else:
                    closed.add(image_path(path))
                continue
            kept.append(name)
        directories[:] = kept
        for name in names:
            path = os.path.join(current, name)
            information = os.lstat(path)
            # No device test here: on overlay file systems a regular file can
            # report the device of its lower layer. So the walk also scans a
            # regular file that is a mount point, for example /etc/hosts or a
            # scanner binary that the developer entry mounts. The directory
            # test above stops only at a directory on another device. A
            # directory mount on the same device is scanned.
            if not stat.S_ISREG(information.st_mode):
                continue
            count += 1
            if information.st_mode & (stat.S_ISUID | stat.S_ISGID):
                setid[image_path(path)] = f"{stat.S_IMODE(information.st_mode):04o} {information.st_uid}:{information.st_gid}"
            try:
                has_capability = capability(path)
            except OSError as error:
                fail(f"{image_path(path)}: file capabilities cannot be read: {error.strerror}")
                continue
            if has_capability:
                fail(f"{image_path(path)} has a file capability")
    return setid, closed, searchable, launchers, count


def check_privileges(recorded, recorded_closed, root="/", capability=read_capability):
    start = len(failures)
    setid, closed, searchable, launchers, count = scan_privileges(root, capability)
    for path in sorted(launchers):
        fail(f"{path} exists")
    for path in sorted(searchable):
        fail(f"{path}: the job user can search it, but the walk cannot list it, "
             "so a privileged file in it is not checked")
    for path in sorted(set(setid) | set(recorded)):
        if path not in recorded:
            fail(f"{path} is setuid or setgid ({setid[path]}) and not in {PRIVILEGED.name}")
        elif path not in setid:
            fail(f"{path} is in {PRIVILEGED.name} ({recorded[path]}) but is not a setuid or setgid file")
        elif setid[path] != recorded[path]:
            fail(f"{path} is {setid[path]}, {PRIVILEGED.name} records {recorded[path]}")
    for path in sorted(closed - recorded_closed):
        fail(f"closed directory {path}: the walk cannot read it, and {PRIVILEGED.name} does not record it")
    for path in sorted(recorded_closed):
        try:
            information = os.lstat(os.path.join(root, path.lstrip("/")))
        except FileNotFoundError:
            fail(f"closed directory {path}: recorded in {PRIVILEGED.name}, but it does not exist")
            continue
        if not stat.S_ISDIR(information.st_mode) or information.st_mode & 0o001:
            fail(f"closed directory {path}: recorded in {PRIVILEGED.name}, but it is not a directory "
                 f"that denies search to others ({stat.filemode(information.st_mode)})")
    passed(start, f"{count} regular files on the root file system checked: no file capability; no {ABSENT_FILE}; "
                  f"{len(setid)} setuid or setgid files match {PRIVILEGED.name}; "
                  f"{len(closed)} directories the job user cannot search, all recorded closed")


def check_versions(record):
    start = len(failures)
    found = []
    for path, package in BINARIES.items():
        upstream = record[package].split(":", 1)[-1].rsplit("-", 1)[0]
        expected = VERSION_LINES[package].format(upstream)
        try:
            result = subprocess.run([path, "--version"], capture_output=True, text=True,
                                    stdin=subprocess.DEVNULL, env=COMMAND_ENVIRONMENT,
                                    timeout=VERSION_TIMEOUT, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            fail(f"{path} --version did not finish: {error}")
            continue
        lines = result.stdout.splitlines()
        first = lines[0].strip() if lines else ""
        if result.returncode != 0 or first != expected:
            fail(f"{path} --version exited with {result.returncode} and printed {first!r}, "
                 f"not exactly {expected!r}")
            continue
        found.append(f"{path} --version prints {upstream}")
    passed(start, "; ".join(found))


def main():
    if not (sys.platform == "linux" and os.path.exists("/run/.containerenv")):
        raise SystemExit("check-ci-image: run this check inside the CI image "
                         "(sh scripts/ci/in-ci-image.sh dev -- sh scripts/ci/check-docs.sh image)")
    record = read_record()
    recorded, recorded_closed = read_privileged()
    check_packages(record, installed_packages())
    check_binaries()
    check_activation()
    check_privileges(recorded, recorded_closed)
    check_versions(record)
    if failures:
        raise SystemExit(f"check-ci-image: {len(failures)} failures in the keyring layer")
    print("check-ci-image: the keyring layer PASS", flush=True)


if __name__ == "__main__":
    main()
