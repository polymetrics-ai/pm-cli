#!/bin/sh
# Git pre-push hook: refuse a push to main, then build, vet and test every
# package that needs no dependency stack, and run the static checks one time
# for the push. A failure stops the push.
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
printf '%s\n' 'pre-push: go vet ./...'
CGO_ENABLED=1 pgo vet ./...
module=$(pgo list -m)
unit=$(CGO_ENABLED=1 pgo list ./... | sed "s#^$module#.#" | drop_stack_packages)
printf '%s\n' 'pre-push: go test (packages that need no stack)'
# shellcheck disable=SC2086
CGO_ENABLED=1 pgo test -count=1 -timeout 20m $unit
set --
if [ "$(uname -s)" = Darwin ]; then
  # The host runs every check except docs. The names come from the script.
  pm_select_python pre-push || exit 1
  checks=$(sh scripts/ci/check-docs.sh --list)
  # Word splitting of the check names is intended.
  # shellcheck disable=SC2046,SC2086
  set -- $(pm_host_checks pre-push Darwin $checks)
fi
printf '%s\n' 'pre-push: check-docs.sh (the checks job)'
sh scripts/ci/check-docs.sh "$@"
