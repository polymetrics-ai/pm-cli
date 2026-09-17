#!/usr/bin/env python3
"""Admit trusted PMCLI events without changing runner or sibling processes."""
import json
import os
from pathlib import Path
import re
import sys

REPOSITORY = "polymetrics-ai/pm-cli"
OWNER = "karthik-sivadas"
WORKFLOWS = ("check.yml", "architecture-documentation.yml")
MAX_EVENT_BYTES = 4 * 1024 * 1024


def require(condition):
    if not condition:
        raise ValueError("event outside approved policy")


def validate(event, env):
    """Raise ValueError for any event outside the dedicated runner policy."""
    try:
        require(env.get("GITHUB_REPOSITORY") == REPOSITORY)
        require(event["repository"]["full_name"] == REPOSITORY)
        require(env.get("GITHUB_ACTOR") == OWNER)
        require(env.get("GITHUB_TRIGGERING_ACTOR") == OWNER)
        require(event["sender"]["login"] == OWNER)
        sha = env.get("GITHUB_SHA", "")
        require(isinstance(sha, str) and re.fullmatch(r"[0-9a-f]{40}", sha))
        kind = env.get("GITHUB_EVENT_NAME")
        if kind == "pull_request":
            pr = event["pull_request"]
            number = event["number"]
            require(type(number) is int and number > 0)
            require(type(pr["number"]) is int and pr["number"] == number)
            require(event["action"] in ("opened", "synchronize", "reopened", "edited"))
            require(pr["state"] == "open" and pr["merged"] is False)
            require(pr["merge_commit_sha"] == sha)
            require(pr["head"]["repo"]["full_name"] == REPOSITORY)
            require(pr["base"]["repo"]["full_name"] == REPOSITORY)
            require(pr["base"]["ref"] == "main")
            ref = f"refs/pull/{number}/merge"
        elif kind == "push":
            ref = "refs/heads/main"
            require(event["ref"] == ref and event["after"] == sha)
            require(event["deleted"] is False and event["forced"] is False)
        else:
            raise ValueError("event outside approved policy")
        require(env.get("GITHUB_REF") == ref)
        require(env.get("GITHUB_WORKFLOW_REF") in {
            f"{REPOSITORY}/.github/workflows/{workflow}@{ref}"
            for workflow in WORKFLOWS
        })
    except (KeyError, TypeError, AttributeError) as error:
        raise ValueError("malformed event") from error


def main():
    try:
        require(os.getuid() == 2001)
        with Path(os.environ["GITHUB_EVENT_PATH"]).open("rb") as stream:
            raw = stream.read(MAX_EVENT_BYTES + 1)
        require(len(raw) <= MAX_EVENT_BYTES)
        validate(json.loads(raw), os.environ)
    except (OSError, ValueError, KeyError, RecursionError):
        # GitHub fails the job on nonzero hook exit; the listener stays alive.
        # Never print event values, credentials, or exception tracebacks here.
        print("PMCLI admission rejected: event outside approved policy.", file=sys.stderr)
        return 1
    print("PMCLI admission accepted.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
