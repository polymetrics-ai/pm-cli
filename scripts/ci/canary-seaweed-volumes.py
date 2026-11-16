#!/usr/bin/env python3
"""DIAGNOSTIC ONLY (issue #310 canary). Never merge.

Print how many writable volumes the SeaweedFS master holds for the artifact
bucket of the fixture project, at one named moment of the stack run.

The script reads the master's own status (/dir/status) inside the master
container, with `docker exec`. It prints counts and the UTC time only: never a
collection or bucket name, a path, a host name or an address. It always exits
0, so it never changes the exit status of the stack run.

Each line also names the runner and the host state, so that a later slow run
can be tied to a host incident or not (INC-20261116-1): an 8-character
SHA-256 prefix of the runner name, the same for the runner name without its
trailing "-<n>" index (the machine), the host load averages, and the host I/O
and CPU pressure (Linux PSI, avg10). It never prints the runner name.

Usage: canary-seaweed-volumes.py ready|after_first_write|end
"""
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys

MOMENTS = ("ready", "after_first_write", "end")
LIMIT_SECONDS = 30
# The master binds its HTTP port to its service address. Inside its own
# container, both names below reach it. Neither name is printed.
STATUS_URLS = ("http://seaweed-master:9333/dir/status", "http://127.0.0.1:9333/dir/status")


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def short_hash(text):
    return hashlib.sha256(text.encode()).hexdigest()[:8] if text else "-"


def pressure(path, kind):
    """The avg10 value of one PSI line kind ("some" or "full"), or "-"."""
    try:
        with open(path, encoding="ascii") as handle:
            for line in handle:
                fields = line.split()
                if fields and fields[0] == kind:
                    for field in fields[1:]:
                        name, _, value = field.partition("=")
                        if name == "avg10" and re.fullmatch(r"\d+\.\d+", value):
                            return value
    except OSError:
        pass
    return "-"


def host_state():
    runner = os.environ.get("RUNNER_NAME", "")
    try:
        with open("/proc/loadavg", encoding="ascii") as handle:
            loads = handle.read().split()[:3]
        if len(loads) != 3 or not all(re.fullmatch(r"\d+\.\d+", value) for value in loads):
            loads = ["-", "-", "-"]
    except OSError:
        loads = ["-", "-", "-"]
    return (f"runner={short_hash(runner)} machine={short_hash(re.sub(r'-[0-9]+$', '', runner))} "
            f"load1={loads[0]} load5={loads[1]} load15={loads[2]} "
            f"io_some_avg10={pressure('/proc/pressure/io', 'some')} "
            f"io_full_avg10={pressure('/proc/pressure/io', 'full')} "
            f"cpu_some_avg10={pressure('/proc/pressure/cpu', 'some')}")


def report(moment, fields):
    print(f"pm310-volumes: moment={moment} utc={now()} {fields} {host_state()}", flush=True)


def docker(arguments):
    try:
        done = subprocess.run(
            ["docker", *arguments], capture_output=True, text=True,
            errors="replace", timeout=LIMIT_SECONDS, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if done.returncode != 0:
        return None
    return done.stdout


def bucket_name(project):
    """The artifact bucket of the fixture: the configured name, or the
    fixture's own rule (project + "-artifacts")."""
    path = os.environ.get("PM_STACK_TEST_CONFIG", "")
    try:
        with open(path, encoding="utf-8") as handle:
            bucket = json.load(handle)["artifacts"]["bucket"]
        if isinstance(bucket, str) and bucket:
            return bucket
    except (OSError, ValueError, KeyError, TypeError):
        pass
    return project + "-artifacts"


def main(arguments):
    moment = arguments[0] if arguments and arguments[0] in MOMENTS else "unknown"
    project = os.environ.get("PM_STACK_PROJECT", "")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project):
        report(moment, "status=unavailable reason=no_fixture_project")
        return 0
    bucket = bucket_name(project)
    ids = docker([
        "ps", "--quiet",
        "--filter", f"label=com.docker.compose.project={project}",
        "--filter", "label=com.docker.compose.service=seaweed-master",
    ])
    containers = [line.strip() for line in (ids or "").splitlines()
                  if re.fullmatch(r"[0-9a-f]{12,64}", line.strip())]
    if len(containers) != 1:
        report(moment, f"status=unavailable reason=master_containers_{len(containers)}")
        return 0
    status = None
    for url in STATUS_URLS:
        for client in (["wget", "-q", "-T", "10", "-O", "-", url], ["curl", "-fsS", "-m", "10", url]):
            output = docker(["exec", containers[0], *client])
            if output:
                try:
                    status = json.loads(output)
                    break
                except ValueError:
                    continue
        if status is not None:
            break
    if not isinstance(status, dict) or not isinstance(status.get("Topology"), dict):
        report(moment, "status=unavailable reason=no_master_status")
        return 0
    topology = status["Topology"]
    bucket_layouts = bucket_writable = default_writable = other_layouts = 0
    for layout in topology.get("Layouts") or []:
        if not isinstance(layout, dict):
            continue
        writables = layout.get("writables") or []
        count = len(writables) if isinstance(writables, list) else 0
        collection = layout.get("collection")
        if collection == bucket:
            bucket_layouts += 1
            bucket_writable += count
        elif collection == "":
            default_writable += count
        else:
            other_layouts += 1
    free = topology.get("Free")
    maximum = topology.get("Max")
    report(moment, f"status=read bucket_layouts={bucket_layouts} bucket_writable={bucket_writable} "
                   f"default_writable={default_writable} other_layouts={other_layouts} "
                   f"max={maximum if isinstance(maximum, int) else '-'} "
                   f"free={free if isinstance(free, int) else '-'}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except Exception as error:  # A diagnostic must never fail the stack run.
        print(f"pm310-volumes: stopped with {type(error).__name__}", flush=True)
        sys.exit(0)
