#!/bin/sh
# Install this reviewed wrapper and runner_admission.py together in the runner
# user's hook directory (outside the checkout); see docs/development/ci.md.
set -eu
hooks=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
exec /usr/bin/timeout 10 /usr/bin/python3 -I -B "$hooks/runner_admission.py"
