#!/bin/sh
# Install this reviewed wrapper outside the mutable checkout, owned by root.
set -eu
exec /usr/bin/timeout 10 /opt/pmcli-tools/venv/bin/python3 -I -B \
  /opt/pmcli-tools/hooks/runner_admission.py
