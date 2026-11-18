#!/bin/sh
# The admission hook of the live runner (D2-A). The owner installs this
# reviewed wrapper and runner_admission_live.py together in /etc/pm-live/hooks
# (owner root, mode 0644); see "Live runner (D2-A)" in docs/development/ci.md.
set -eu
hooks=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
exec /usr/bin/timeout 10 /usr/bin/python3 -I -B "$hooks/runner_admission_live.py"
