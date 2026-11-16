#!/usr/bin/env python3
"""DIAGNOSTIC ONLY (issue #310 canary). Never merge.

After the stack run, and before the fixture cleanup, print the volume events
of the SeaweedFS master, volume and filer services of the fixture project.

Each printed event holds four fixed fields only: the UTC time that the
container engine recorded for the log line, the service, an event kind from
the fixed list below, and the volume ID (digits only). The script prints no
log text, so no printed line holds an address, a host name, a path, a
collection or a bucket name. It never changes the exit status of the stack
run: it always exits 0.

The patterns match the messages that SeaweedFS 4.46 writes at the default
verbosity (weed/server/master_grpc_server_volume.go,
weed/server/master_grpc_server_assign.go, weed/topology/volume_growth.go and
weed/storage/store.go).
"""
import datetime
import os
import re
import subprocess
import sys

SERVICES = ("seaweed-master", "seaweed-volume", "seaweed-filer")
LIMIT_SECONDS = 30
MAX_EVENTS = 300

# The first pattern that matches a log message gives the event kind. A
# pattern group, if it matched, is the volume ID.
KINDS = (
    ("grow_request_failed", re.compile(r"^volume grow request (?:for dc:\S* rack:\S* )?failed")),
    ("grow_request", re.compile(r"^volume grow ")),
    ("grow_result", re.compile(r"^create \d+ volume, created \d+")),
    ("grow_waits_for_warmup", re.compile(r"^wait for volume servers to join back")),
    ("volume_created", re.compile(r"^Created Volume (\d+) on ")),
    ("volume_registered", re.compile(r"^Registered Volume (\d+) on ")),
    ("allocate_failed", re.compile(r"^Failed to assign volume (\d+) on ")),
    ("allocate_cleanup_failed", re.compile(r"^Failed to clean up volume (\d+) on ")),
    ("assign_failed", re.compile(r"^assign ")),
    ("volume_added", re.compile(r"^In dir .* adds volume:(\d+) ")),
    ("volume_added_on_disk", re.compile(r"^add volume (\d+) on disk ID ")),
    ("volume_deleted", re.compile(r"^(?:DeleteVolume (\d+) |volume (\d+) is deleted)")),
    ("leader_event", re.compile(r"(?i)\bleader\b")),
    ("assign_event", re.compile(r"(?i)\bassign")),
)
LEVEL_KINDS = {"W": "warning", "E": "error", "F": "fatal"}
CPU_FIELDS = ("usage_usec", "nr_periods", "nr_throttled", "throttled_usec")
# glog: Lmmdd hh:mm:ss.uuuuuu threadid file:line] message
GLOG = re.compile(r"^([IWEF])\d{4} \d{2}:\d{2}:\d{2}\.\d+\s+\d+\s+\S+:\d+\]\s?(.*)$")
STAMP = re.compile(r"^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})\s(.*)$")


def utc(date, clock, fraction, zone):
    """Return the engine time of one log line in UTC, or None."""
    micro = (fraction or "0")[:6].ljust(6, "0")
    offset = "+00:00" if zone == "Z" else zone
    try:
        moment = datetime.datetime.fromisoformat(f"{date}T{clock}.{micro}{offset}")
    except ValueError:
        return None
    return moment.astimezone(datetime.timezone.utc)


def docker(arguments):
    """Run one docker call with a time limit. Return its output, or None."""
    try:
        done = subprocess.run(
            ["docker", *arguments], capture_output=True, text=True,
            errors="replace", timeout=LIMIT_SECONDS, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if done.returncode != 0:
        return None
    return done.stdout + done.stderr


def classify(message):
    """Return the event kind and the volume ID of one glog message, or None."""
    match = GLOG.match(message)
    if not match:
        return None
    level, text = match.group(1), match.group(2)
    for kind, pattern in KINDS:
        found = pattern.search(text)
        if found:
            volume = next((group for group in found.groups() if group), "-")
            return kind, volume
    if level in LEVEL_KINDS:
        return LEVEL_KINDS[level], "-"
    return None


def main():
    project = os.environ.get("PM_STACK_PROJECT", "")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project):
        print("pm310-seaweed: no valid fixture project in the environment")
        return 0
    print(f"pm310-seaweed: receipt at {datetime.datetime.now(datetime.timezone.utc).isoformat()}")
    for service in SERVICES:
        ids = docker([
            "ps", "--all", "--quiet",
            "--filter", f"label=com.docker.compose.project={project}",
            "--filter", f"label=com.docker.compose.service={service}",
        ])
        if ids is None:
            print(f"pm310-seaweed: service={service} docker ps failed")
            continue
        containers = [line.strip() for line in ids.splitlines() if re.fullmatch(r"[0-9a-f]{12,64}", line.strip())]
        print(f"pm310-seaweed: service={service} containers={len(containers)}")
        for ordinal, container in enumerate(containers, start=1):
            output = docker(["logs", "--timestamps", container])
            if output is None:
                print(f"pm310-seaweed: service={service} container={ordinal} docker logs failed")
                continue
            events, counts, lines = [], {}, 0
            for line in output.splitlines():
                lines += 1
                stamped = STAMP.match(line)
                if not stamped:
                    continue
                result = classify(stamped.group(5))
                if result is None:
                    continue
                moment = utc(*stamped.group(1, 2, 3, 4))
                kind, volume = result
                counts[kind] = counts.get(kind, 0) + 1
                events.append((moment, kind, volume))
            events.sort(key=lambda event: event[0] or datetime.datetime.min.replace(tzinfo=datetime.timezone.utc))
            for moment, kind, volume in events[:MAX_EVENTS]:
                stamp = moment.strftime("%Y-%m-%dT%H:%M:%S.%fZ") if moment else "-"
                print(f"pm310-seaweed: utc={stamp} service={service} container={ordinal} kind={kind} volume={volume}")
            summary = " ".join(f"{kind}={count}" for kind, count in sorted(counts.items())) or "none"
            print(f"pm310-seaweed-summary: service={service} container={ordinal} lines={lines} "
                  f"events={len(events)} printed={min(len(events), MAX_EVENTS)} {summary}")
            # The CPU throttling totals of the container's cgroup since it
            # started. Only the named counters print, as integers.
            stat = docker(["exec", container, "cat", "/sys/fs/cgroup/cpu.stat"])
            if stat is None:
                print(f"pm310-seaweed-cpu: service={service} container={ordinal} cpu.stat unavailable")
                continue
            fields = {}
            for line in stat.splitlines():
                name, _, value = line.partition(" ")
                if name in CPU_FIELDS and re.fullmatch(r"\d+", value.strip()):
                    fields[name] = value.strip()
            print(f"pm310-seaweed-cpu: service={service} container={ordinal} " +
                  " ".join(f"{name}={fields.get(name, '-')}" for name in CPU_FIELDS))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:  # A diagnostic must never fail the stack run.
        print(f"pm310-seaweed: stopped with {type(error).__name__}")
        sys.exit(0)
