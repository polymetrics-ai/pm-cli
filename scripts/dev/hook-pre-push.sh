#!/bin/sh
# Git pre-push hook: refuse a push to main, then build every package, test
# every package that needs no dependency stack, and run the static checks one
# time for the push. The static checks include `vet` (go vet ./...), so the
# hook does not run go vet itself. A failure stops the push.
set -eu
root=$(git rev-parse --show-toplevel)
cd "$root"
# shellcheck source=scripts/dev/hook-lib.sh disable=SC1091
. scripts/dev/hook-lib.sh
pm_in_image scripts/dev/hook-pre-push.sh "$@"

pushing=$(python3 -I -B scripts/dev/secret_scan.py push)
"$pushing" || exit 0

printf '%s\n' 'pre-push: go build ./...'
CGO_ENABLED=1 pgo build ./...
module=$(pgo list -m)
unit=$(CGO_ENABLED=1 pgo list ./... | sed "s#^$module#.#" | drop_stack_packages)
printf '%s\n' 'pre-push: go test (packages that need no stack)'
# shellcheck disable=SC2086
CGO_ENABLED=1 pgo test -count=1 -timeout 20m $unit
if [ "$(uname -s)" = Darwin ]; then
  pm_select_python pre-push || exit 1
fi
# The hook ran the packages of the `unit` check above, so pm_push_checks
# leaves `unit` out on every OS.
checks=$(pm_push_checks "$(uname -s)")
# An empty list would make check-docs.sh run every check, `unit` too.
if [ -z "$checks" ]; then
  printf '%s\n' 'pre-push: check-docs.sh --list gave no check' >&2
  exit 1
fi
# Word splitting of the check names is intended.
# shellcheck disable=SC2086
set -- $checks
printf '%s\n' 'pre-push: check-docs.sh (the checks job)'
sh scripts/ci/check-docs.sh "$@"
