#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
generator=${PM_API_GENERATE:-"$repo_root/.cache/bin/pm-api-generate"}

if [ ! -x "$generator" ]; then
  printf '%s\n' "pm API generator is unavailable or not executable: $generator" >&2
  exit 1
fi

exec "$generator" --root "$repo_root" "$@"
