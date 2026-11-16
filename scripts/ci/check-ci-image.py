#!/usr/bin/env python3
"""Check the keyring layer of the CI job image (#323, D1-9).

The `image` check of scripts/ci/check-docs.sh runs this script inside the
pinned job image (scripts/ci/in-ci-image.sh). Outside a container it fails,
because the host is not the image. It starts no bus and no keyring daemon.

The check fails when one of these items is false:
- Each package of build/ci/keyring-packages.txt is installed at its version.
- dbus-x11 is not installed, and no dbus-launch file is on a binary path.
- /usr/bin/dbus-daemon and /usr/bin/gnome-keyring-daemon are executable
  regular files of the packages dbus-daemon and gnome-keyring.
- No D-Bus service file names org.freedesktop.secrets or starts
  gnome-keyring-daemon.
- No regular file of a recorded package has a setuid bit, a setgid bit or a
  file capability (the security.capability extended attribute).
- `dbus-daemon --version` and `gnome-keyring-daemon --version` exit with code 0
  and print the upstream version of the recorded package.
"""
import errno
import os
from pathlib import Path
import stat
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
RECORD = ROOT / "build/ci/keyring-packages.txt"
BINARIES = {"/usr/bin/dbus-daemon": "dbus-daemon",
            "/usr/bin/gnome-keyring-daemon": "gnome-keyring"}
ABSENT_PACKAGES = ("dbus-x11",)
ABSENT_FILE = "dbus-launch"
BINARY_DIRECTORIES = ("/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin",
                      "/bin", "/sbin", "/usr/libexec", "/opt/pmcli/bin")
SERVICE_DIRECTORIES = ("/usr/share/dbus-1", "/usr/local/share/dbus-1", "/etc/dbus-1")
SECRETS_NAME = "org.freedesktop.secrets"
KEYRING_DAEMON = "gnome-keyring-daemon"
COMMAND_ENVIRONMENT = {"PATH": "/usr/bin:/bin", "LANG": "C"}

failures = []


def fail(message):
    failures.append(message)
    print(f"check-ci-image: FAIL {message}", flush=True)


def passed(message):
    print(f"check-ci-image: PASS {message}", flush=True)


def query(*arguments):
    result = subprocess.run(["dpkg-query", *arguments], capture_output=True, text=True,
                            env=COMMAND_ENVIRONMENT, timeout=60, check=False)
    return result.returncode, result.stdout


def read_record():
    packages = {}
    for number, line in enumerate(RECORD.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        fields = line.split()
        if len(fields) != 2 or fields[0] in packages:
            raise SystemExit(f"check-ci-image: {RECORD.name}:{number}: need one unique 'name version' pair")
        packages[fields[0]] = fields[1]
    if not set(BINARIES.values()) <= set(packages):
        raise SystemExit(f"check-ci-image: {RECORD.name} does not record the daemon packages")
    return packages


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


def check_packages(record, installed):
    for name, version in record.items():
        have = installed.get(name)
        if have is None:
            fail(f"package {name} is not installed")
        elif have != (version, "ii"):
            fail(f"package {name} is {have[0]} ({have[1]}), the record needs {version} (ii)")
    for name in ABSENT_PACKAGES:
        if name in installed:
            fail(f"package {name} is installed")
    found = [os.path.join(directory, ABSENT_FILE) for directory in BINARY_DIRECTORIES
             if os.path.lexists(os.path.join(directory, ABSENT_FILE))]
    for path in found:
        fail(f"{path} exists")
    passed(f"{len(record)} recorded packages checked; {', '.join(ABSENT_PACKAGES)} and {ABSENT_FILE} checked absent")


def check_binaries():
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
            continue
        passed(f"{path} is an executable regular file of {package}")


def check_activation():
    count = 0
    for directory in SERVICE_DIRECTORIES:
        for current, _, names in os.walk(directory):
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
    passed(f"{count} D-Bus service files checked: none activates {SECRETS_NAME} or {KEYRING_DAEMON}")


def check_privileges(record):
    count = 0
    for package in record:
        code, output = query("--listfiles", package)
        if code != 0:
            fail(f"dpkg-query --listfiles {package} failed")
            continue
        for path in output.splitlines():
            try:
                information = os.lstat(path)
            except FileNotFoundError:
                continue
            if not stat.S_ISREG(information.st_mode):
                continue
            count += 1
            if information.st_mode & (stat.S_ISUID | stat.S_ISGID):
                fail(f"{path} of {package} has mode {stat.filemode(information.st_mode)}")
            try:
                os.getxattr(path, "security.capability", follow_symlinks=False)
            except OSError as error:
                if error.errno != errno.ENODATA:
                    fail(f"{path} of {package}: file capabilities cannot be read: {error.strerror}")
            else:
                fail(f"{path} of {package} has a file capability")
    passed(f"{count} regular files of the recorded packages checked: no setuid, setgid or file capability")


def check_versions(record):
    for path, package in BINARIES.items():
        upstream = record[package].split(":", 1)[-1].rsplit("-", 1)[0]
        try:
            result = subprocess.run([path, "--version"], capture_output=True, text=True,
                                    stdin=subprocess.DEVNULL, env=COMMAND_ENVIRONMENT,
                                    timeout=10, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            fail(f"{path} --version did not finish: {error}")
            continue
        if result.returncode != 0 or upstream not in result.stdout + result.stderr:
            fail(f"{path} --version exited with {result.returncode} and did not print {upstream}")
            continue
        passed(f"{path} --version prints {upstream}")


def main():
    if not (sys.platform == "linux" and os.path.exists("/run/.containerenv")):
        raise SystemExit("check-ci-image: run this check inside the CI image "
                         "(sh scripts/ci/in-ci-image.sh dev -- sh scripts/ci/check-docs.sh image)")
    record = read_record()
    check_packages(record, installed_packages())
    check_binaries()
    check_activation()
    check_privileges(record)
    check_versions(record)
    if failures:
        raise SystemExit(f"check-ci-image: {len(failures)} failures in the keyring layer")
    print("check-ci-image: the keyring layer PASS", flush=True)


if __name__ == "__main__":
    main()
