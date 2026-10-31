#!/usr/bin/env python3
"""Install the pinned scanner and check staged changes or outgoing commits."""

import hashlib
import io
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tarfile
import tempfile
import urllib.request


VERSION = "8.30.1"
# Official release archives: https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1
# The binary digest also protects scans from a different executable on PATH.
RELEASES = {
    ("Linux", "x86_64"): (
        "linux_x64",
        "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb",
        "88f91962aa2f93ac6ab281d553b9e125f5197bbbce38f9f2437f7299c32e5509",
    ),
    ("Darwin", "arm64"): (
        "darwin_arm64",
        "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5",
        "ba52fb1bfabbcde42f032afad3d6e0b19dff8ed105229a16e7caa338bbc0e84f",
    ),
}


class ScanError(Exception):
    """A missing prerequisite or failed scan blocks the Git operation."""


def release():
    system, machine = platform.system(), platform.machine()
    # An Intel Python can run through Rosetta on an Apple Silicon host.
    if system == "Darwin" and machine == "x86_64":
        arm = subprocess.run(["/usr/sbin/sysctl", "-n", "hw.optional.arm64"], capture_output=True, text=True)
        if arm.returncode == 0 and arm.stdout.strip() == "1":
            machine = "arm64"
    result = RELEASES.get((system, machine))
    if result is None:
        raise ScanError("this platform has no reviewed Gitleaks pin")
    return result


def binary_path():
    path = Path(os.environ.get("PMCLI_GITLEAKS", str(Path.home() / ".local/bin/gitleaks"))).resolve()
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ScanError("Gitleaks is unavailable. Run python3 scripts/dev/secret_scan.py install")
    if hashlib.sha256(path.read_bytes()).hexdigest() != release()[2]:
        raise ScanError("Gitleaks has the wrong checksum. Run python3 scripts/dev/secret_scan.py install")
    return path


def install():
    target, archive_digest, binary_digest = release()
    url = f"https://github.com/gitleaks/gitleaks/releases/download/v{VERSION}/gitleaks_{VERSION}_{target}.tar.gz"
    with urllib.request.urlopen(url, timeout=60) as response:
        archive = response.read(32 * 1024 * 1024 + 1)
    if hashlib.sha256(archive).hexdigest() != archive_digest:
        raise ScanError("the Gitleaks archive has the wrong checksum")
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
        member = bundle.getmember("gitleaks")
        if not member.isfile():
            raise ScanError("the Gitleaks archive has no regular binary")
        with bundle.extractfile(member) as source:
            data = source.read()
    if hashlib.sha256(data).hexdigest() != binary_digest:
        raise ScanError("the Gitleaks binary has the wrong checksum")
    destination = Path.home() / ".local/bin/gitleaks"
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as temporary:
        temporary.write(data)
        temporary_path = Path(temporary.name)
    try:
        temporary_path.chmod(0o755)
        temporary_path.replace(destination)
    finally:
        temporary_path.unlink(missing_ok=True)
    print(f"Installed Gitleaks {VERSION} at {destination}")


def git(*args, check=True):
    result = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    if check and result.returncode:
        # Do not copy Git diagnostics. A ref or path can contain sensitive text.
        raise ScanError("Git could not identify the content to scan")
    return result


def scan(binary, mode, source, *args):
    # Only this reviewed policy can extend the pinned built-in rules. Local
    # config and ignore comments/files cannot silently remove a finding.
    config = Path(__file__).resolve().with_name("gitleaks.toml")
    command = [
        str(binary), mode, "--config", str(config),
        "--gitleaks-ignore-path", os.devnull, "--ignore-gitleaks-allow",
        "--redact=100", "--no-banner", "--no-color", "--log-level=info", "--timeout=600",
        *args, ".",
    ]
    result = subprocess.run(command, cwd=source, stdout=sys.stderr, check=False, timeout=620)
    if result.returncode:
        raise ScanError("the secret scan failed. Resolve the redacted findings or scanner error")


def staged(binary):
    # Gitleaks' Git mode respects -diff attributes and textconv. Materialize
    # raw changed index blobs instead, with their paths and without symlinks.
    result = subprocess.run(
        ["git", "diff", "--cached", "--name-only", "--diff-filter=ACMRT", "-z"],
        capture_output=True, check=False,
    )
    if result.returncode:
        raise ScanError("Git could not identify the staged content")
    with tempfile.TemporaryDirectory(prefix="pm-secret-index-") as directory:
        for name in result.stdout.split(b"\0"):
            if not name:
                continue
            path = Path(os.fsdecode(name))
            if path.is_absolute() or ".." in path.parts:
                raise ScanError("Git returned an invalid staged path")
            destination = Path(directory) / path
            destination.parent.mkdir(parents=True, exist_ok=True)
            with destination.open("wb") as target:
                blob = subprocess.run(["git", "cat-file", "blob", b":" + name],
                                      stdout=target, stderr=subprocess.DEVNULL, check=False)
            if blob.returncode:
                raise ScanError("Git could not read a staged blob")
        scan(binary, "dir", directory)


def push(binary):
    updates = []
    for line in sys.stdin:
        fields = line.split()
        if len(fields) != 4:
            raise ScanError("the pre-push input is invalid")
        _, local_sha, remote_ref, remote_sha = fields
        if remote_ref == "refs/heads/main":
            raise ScanError("do not push to main. Push a branch and open a pull request")
        if not all(re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", sha) for sha in (local_sha, remote_sha)):
            raise ScanError("the pre-push object identity is invalid")
        if set(local_sha) != {"0"}:
            updates.append((local_sha, remote_sha))
    if updates and git("rev-parse", "--is-shallow-repository").stdout.strip() != "false":
        raise ScanError("push scanning needs complete history. Fetch the missing history first")
    grafts = Path(git("rev-parse", "--git-path", "info/grafts").stdout.strip())
    if updates and grafts.exists() and grafts.stat().st_size:
        raise ScanError("push scanning does not accept grafted history")
    for local_sha, remote_sha in updates:
        local_commit = git("rev-parse", "--verify", f"{local_sha}^{{commit}}").stdout.strip()
        remote_commit = git("rev-parse", "--verify", f"{remote_sha}^{{commit}}", check=False)
        # New branches and unavailable remote bases scan all local reachable
        # history. This includes a secret added and removed before the push.
        revision = local_commit
        if set(remote_sha) != {"0"} and remote_commit.returncode == 0:
            revision = f"{remote_commit.stdout.strip()}..{local_commit}"
        # Explicit merge diffs include secrets introduced by merge resolution.
        scan(binary, "git", ".", f"--log-opts=--full-history --text --no-textconv --no-ext-diff -m {revision}")
    print("true" if updates else "false")


def main():
    # Local replacement refs must not hide the original objects sent by push.
    os.environ["GIT_NO_REPLACE_OBJECTS"] = "1"
    os.environ.pop("GIT_GRAFT_FILE", None)
    if len(sys.argv) != 2 or sys.argv[1] not in {"install", "binary", "staged", "push"}:
        raise ScanError("usage: secret_scan.py install | binary | staged | push")
    action = sys.argv[1]
    if action == "install":
        install()
        return
    binary = binary_path()
    if action == "binary":
        print(binary)
    elif action == "staged":
        git("rev-parse", "--show-toplevel")
        staged(binary)
    else:
        push(binary)


if __name__ == "__main__":
    try:
        main()
    except (ScanError, OSError, ValueError, KeyError, tarfile.TarError, subprocess.TimeoutExpired) as error:
        # The scanner itself emits only fully redacted findings. Unexpected
        # system errors stay generic because paths can contain sensitive text.
        message = str(error) if isinstance(error, ScanError) else "the scanner could not complete"
        print(f"secret scan: {message}", file=sys.stderr)
        sys.exit(1)
