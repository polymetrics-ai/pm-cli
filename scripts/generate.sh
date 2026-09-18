#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
generator=${PM_API_GENERATE:-"$repo_root/.cache/bin/pm-api-generate"}
connector_generator=${PM_CONNECTOR_GENERATE:-"$repo_root/.cache/bin/pmgen"}

if [ ! -x "$generator" ]; then
  printf '%s\n' "pm API generator is unavailable or not executable: $generator" >&2
  exit 1
fi

check=0
for argument in "$@"; do
  case "$argument" in
    --check) check=1 ;;
  esac
done

# --check also proves that every connectors/<id> root still reproduces its
# runtime bundle, fidelity report, seven lane inventories and seven lane
# wrappers byte-for-byte. The connector generator derives the complete output
# set in memory and writes nothing in this mode; regenerate it explicitly with
# .cache/bin/pmgen connector --root REPO (omit --check to write).
if [ "$check" -eq 1 ]; then
  if [ ! -x "$connector_generator" ]; then
    printf '%s\n' "connector generator is unavailable or not executable: $connector_generator" >&2
    printf '%s\n' "rebuild it with 'make build' (or 'make check', which builds before checking)" >&2
    exit 1
  fi
  if ! "$connector_generator" connector --root "$repo_root" --check; then
    printf '%s\n' "connector generation check failed for $repo_root" >&2
    printf '%s\n' "rebuild .cache/bin/pmgen with 'make build' when the pinned tool is stale" >&2
    exit 1
  fi
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
