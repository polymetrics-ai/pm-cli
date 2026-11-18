#!/usr/bin/env python3
"""Admit only the owner's first live run of the approved commit (D2-A).

The policy of the pm-live runner. The shared runner_admission.py refuses
every live event, and this file does not change it. Each rule is one named
check, and validate() calls each one. read_root_file() reads a root-owned
file in /etc/pm-live with one openat(O_NOFOLLOW) for each path component.
Its ROOT and OWNER parameters exist for the developer checks. Production
calls pass "/" and 0, and no environment variable changes them.
"""
import json
import os
from pathlib import Path
import re
import stat
import sys

REPOSITORY = "polymetrics-ai/pm-cli"
OWNER = "karthik-sivadas"
LIVE_REF = "refs/heads/main"
LIVE_WORKFLOW = f"{REPOSITORY}/.github/workflows/live-w2.yml@{LIVE_REF}"
RUN_KINDS = ("rehearsal", "w2")
MAX_EVENT_BYTES = 4 * 1024 * 1024
MAX_ROOT_FILE_BYTES = 64
ROOT_DIRECTORY = ("etc", "pm-live")
OPEN_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)


def require(condition):
    if not condition:
        raise ValueError("event outside approved policy")


def require_runner_identity(env):
    """The runner .env names its own unprivileged UID; root is never admitted."""
    expected = env.get("PMCLI_RUNNER_UID", "")
    require(expected.isdigit() and expected.isascii())
    require(os.getuid() != 0 and os.getuid() == int(expected))


def require_repository(event, env):
    require(env.get("GITHUB_REPOSITORY") == REPOSITORY)
    require(event["repository"]["full_name"] == REPOSITORY)


def require_actor(event, env):
    require(env.get("GITHUB_ACTOR") == OWNER)
    require(env.get("GITHUB_TRIGGERING_ACTOR") == OWNER)
    require(event["sender"]["login"] == OWNER)


def require_event(event, env):
    """A workflow_dispatch with no inputs: absent, JSON null or an empty object."""
    require(env.get("GITHUB_EVENT_NAME") == "workflow_dispatch")
    inputs = event.get("inputs")
    require(inputs is None or (type(inputs) is dict and not inputs))


def require_live_ref(event, env):
    """The one ref rule of the owner decision of 2026-11-17."""
    require(env.get("GITHUB_REF") == LIVE_REF)
    require(event["ref"] == LIVE_REF)


def require_workflow(env):
    require(env.get("GITHUB_WORKFLOW_REF") == LIVE_WORKFLOW)


def require_first_attempt(env):
    """A re-run could run an old approved SHA again (crrao m2)."""
    require(env.get("GITHUB_RUN_ATTEMPT") == "1")


def require_approved_sha(env, root, owner):
    sha = env.get("GITHUB_SHA")
    require(isinstance(sha, str) and re.fullmatch(r"[0-9a-f]{40}", sha))
    require(env.get("GITHUB_WORKFLOW_SHA") == sha)
    require(read_root_file("approved-sha", root, owner) == sha)


def require_run_kind(root, owner):
    require(read_root_file("run-kind", root, owner) in RUN_KINDS)


def require_owned(descriptor, owner, kind):
    """The open object has the type KIND, belongs to OWNER and is not group- or world-writable."""
    status = os.fstat(descriptor)
    require(kind(status.st_mode) and status.st_uid == owner)
    require(not status.st_mode & (stat.S_IWGRP | stat.S_IWOTH))


def read_root_file(name, root="/", owner=0):
    """Return the one-line value of ROOT/etc/pm-live/NAME.

    Each directory from ROOT on and the file are opened with O_NOFOLLOW, one
    component at a time, so a symbolic link anywhere refuses. Each must
    belong to OWNER and not be group- or world-writable. The file must be a
    regular file of at most 64 bytes: the value and exactly one final line
    break, with no carriage return and no NUL. An empty value refuses.
    """
    directory = os.open(root, OPEN_FLAGS | os.O_DIRECTORY)
    try:
        require_owned(directory, owner, stat.S_ISDIR)
        for part in ROOT_DIRECTORY:
            child = os.open(part, OPEN_FLAGS | os.O_DIRECTORY, dir_fd=directory)
            os.close(directory)
            directory = child
            require_owned(directory, owner, stat.S_ISDIR)
        # O_NONBLOCK: the open of a FIFO must not wait for a writer.
        handle = os.open(name, OPEN_FLAGS | os.O_NONBLOCK, dir_fd=directory)
        try:
            require_owned(handle, owner, stat.S_ISREG)
            require(os.fstat(handle).st_size <= MAX_ROOT_FILE_BYTES)
            data = os.read(handle, MAX_ROOT_FILE_BYTES + 1)
        finally:
            os.close(handle)
    finally:
        os.close(directory)
    require(len(data) <= MAX_ROOT_FILE_BYTES and data.endswith(b"\n") and data.count(b"\n") == 1)
    require(b"\r" not in data and b"\0" not in data)
    value = data[:-1].decode("ascii")
    require(value)
    return value


def validate(event, env, root="/", owner=0):
    """Raise ValueError for any event outside the live runner policy."""
    try:
        require(isinstance(event, dict))
        require_repository(event, env)
        require_actor(event, env)
        require_event(event, env)
        require_live_ref(event, env)
        require_workflow(env)
        require_first_attempt(env)
        require_approved_sha(env, root, owner)
        require_run_kind(root, owner)
    except (KeyError, TypeError, AttributeError, OSError, UnicodeError) as error:
        raise ValueError("malformed event") from error


def main(root="/", owner=0):
    try:
        require_runner_identity(os.environ)
        with Path(os.environ["GITHUB_EVENT_PATH"]).open("rb") as stream:
            raw = stream.read(MAX_EVENT_BYTES + 1)
        require(len(raw) <= MAX_EVENT_BYTES)
        validate(json.loads(raw), os.environ, root, owner)
    except (OSError, ValueError, KeyError, RecursionError):
        # GitHub fails the job on a nonzero hook exit. Never print event
        # values, file contents or exception tracebacks here.
        print("PMCLI live admission rejected: event outside approved policy.", file=sys.stderr)
        return 1
    print("PMCLI live admission accepted.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
