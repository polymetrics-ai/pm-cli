#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
generator=${PM_API_GENERATE:-"$repo_root/.cache/bin/pm-api-generate"}

if [ ! -x "$generator" ]; then
  printf '%s\n' "pm API generator is unavailable or not executable: $generator" >&2
  exit 1
fi

# Bazel supplies the selected executable; ordinary invocations resolve the same
# pinned tool through bootstrap rather than requiring a global Go installation.
if [ -n "${PM_GO:-}" ]; then
  go_root=$(unset GOROOT; GOTOOLCHAIN=local GOWORK=off "$PM_GO" env GOROOT)
else
  go_root=$(CGO_ENABLED=0 python3 "$repo_root/tools/bootstrap.py" exec go -- env GOROOT)
fi
export GOROOT="$go_root"
export PM_GO="$go_root/bin/go"
export GOTOOLCHAIN=local
export GOWORK=off
export PATH="$go_root/bin:${PATH:-}"

exec "$generator" --root "$repo_root" "$@"
