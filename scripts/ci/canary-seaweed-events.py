#!/usr/bin/env python3
"""DIAGNOSTIC ONLY (issue #310 canary). Never merge.

After the stack run, and before the fixture cleanup, print the volume events
of the SeaweedFS master, volume and filer services of the fixture project.

Each printed event holds fixed fields only: the UTC time that the container
engine recorded for the log line, the service, an event kind from the fixed
list below, the volume ID (digits only), and whether the line names the
artifact bucket (bucket=yes or bucket=no, only for kinds whose text names a
collection). The script prints no log text, so no printed line holds an
address, a host name, a path, a collection or a bucket name. It never changes
the exit status of the stack run: it always exits 0.

For the artifact bucket, it also prints the growth time from the master's and
the volume server's own log times: from the master's growth request to the
volume server's new volume, and to the master's registration of that volume.

The patterns match the messages that SeaweedFS 4.46 writes at the default
verbosity (weed/server/master_grpc_server_volume.go,
weed/server/master_grpc_server_assign.go, weed/topology/volume_growth.go and
weed/storage/store.go).
"""
import datetime
import json
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
# The glog fork of SeaweedFS 4.46 (weed/glog/glog.go, formatHeader) writes
# "Lmmdd hh:mm:ss.uuuuuu file:line message": no thread ID and no "]", although
# its comment shows both. The pattern also accepts the classic glog form.
GLOG = re.compile(r"^([IWEF])\d{4} \d{2}:\d{2}:\d{2}\.\d+ +(?:\d+ +)?[^\s:]+:\d+\]? (.*)$")
# The kinds whose message names a collection.
NAMES_COLLECTION = ("grow_request", "grow_request_failed", "volume_added")
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


def classify(message, bucket):
    """Return the event kind, the volume ID and the bucket mark of one glog
    message, or None."""
    match = GLOG.match(message)
    if not match:
        return None
    level, text = match.group(1), match.group(2)
    for kind, pattern in KINDS:
        found = pattern.search(text)
        if found:
            volume = next((group for group in found.groups() if group), "-")
            mark = ("yes" if bucket and bucket in text else "no") if kind in NAMES_COLLECTION else "-"
            return kind, volume, mark
    if level in LEVEL_KINDS:
        return LEVEL_KINDS[level], "-", "-"
    return None


def bucket_name(project):
    """The artifact bucket of the fixture: the configured name, or the
    fixture's own rule (project + "-artifacts")."""
    try:
        with open(os.environ.get("PM_STACK_TEST_CONFIG", ""), encoding="utf-8") as handle:
            bucket = json.load(handle)["artifacts"]["bucket"]
        if isinstance(bucket, str) and bucket:
            return bucket
    except (OSError, ValueError, KeyError, TypeError):
        pass
    return project + "-artifacts"


def milliseconds(start, end):
    if start is None or end is None:
        return "-"
    return f"{(end - start).total_seconds() * 1000:.1f}"


def growth(all_events):
    """Print the growth time of the first artifact bucket volume.

    The master's "volume grow" line prints its request with a pointer, so it
    does not name the collection. The volume server's "adds volume" line
    names it. So the bucket's first volume is the first volume that the
    volume server adds for the bucket. Its request is the last growth request
    of the master at or before that time, and its registration is the
    master's "Registered Volume" line with the same volume ID."""
    added = next((event for event in all_events
                  if event[1] == "seaweed-volume" and event[2] == "volume_added" and event[4] == "yes"), None)
    request = registered = None
    if added is not None:
        for event in all_events:
            if (event[1] == "seaweed-master" and event[2] == "grow_request" and event[0] is not None
                    and added[0] is not None and event[0] <= added[0]):
                request = event
        registered = next((event for event in all_events
                           if event[1] == "seaweed-master" and event[2] == "volume_registered"
                           and event[3] == added[3]), None)
    def stamp(event):
        return event[0].strftime("%Y-%m-%dT%H:%M:%S.%fZ") if event and event[0] else "-"
    print(f"pm310-growth: bucket_volume={added[3] if added else '-'} "
          f"request_utc={stamp(request)} volume_added_utc={stamp(added)} "
          f"volume_registered_utc={stamp(registered)} "
          f"request_to_added_ms={milliseconds(request and request[0], added and added[0])} "
          f"request_to_registered_ms={milliseconds(request and request[0], registered and registered[0])}")


def main():
    project = os.environ.get("PM_STACK_PROJECT", "")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project):
        print("pm310-seaweed: no valid fixture project in the environment")
        return 0
    print(f"pm310-seaweed: receipt at {datetime.datetime.now(datetime.timezone.utc).isoformat()}")
    bucket = bucket_name(project)
    all_events = []
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
                result = classify(stamped.group(5), bucket)
                if result is None:
                    continue
                moment = utc(*stamped.group(1, 2, 3, 4))
                kind, volume, mark = result
                counts[kind] = counts.get(kind, 0) + 1
                events.append((moment, kind, volume, mark))
            events.sort(key=lambda event: event[0] or datetime.datetime.min.replace(tzinfo=datetime.timezone.utc))
            for moment, kind, volume, mark in events[:MAX_EVENTS]:
                stamp = moment.strftime("%Y-%m-%dT%H:%M:%S.%fZ") if moment else "-"
                print(f"pm310-seaweed: utc={stamp} service={service} container={ordinal} kind={kind} "
                      f"volume={volume} bucket={mark}")
                all_events.append((moment, service, kind, volume, mark))
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
    all_events.sort(key=lambda event: event[0] or datetime.datetime.min.replace(tzinfo=datetime.timezone.utc))
    growth(all_events)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:  # A diagnostic must never fail the stack run.
        print(f"pm310-seaweed: stopped with {type(error).__name__}")
        sys.exit(0)
