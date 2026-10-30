#!/bin/sh
# Git pre-push hook: refuse a push to main, then build, vet and test every
# package that needs no dependency stack. A failure stops the push.
set -eu
root=$(git rev-parse --show-toplevel)
cd "$root"
# shellcheck source=scripts/dev/hook-lib.sh disable=SC1091
. scripts/dev/hook-lib.sh

zero=0000000000000000000000000000000000000000
pushing=false
while read -r _local_ref local_sha remote_ref _remote_sha; do
  case "$remote_ref" in
    refs/heads/main)
      printf '%s\n' 'pre-push: do not push to main. Push a branch and open a pull request; only the owner merges.' >&2
      exit 1
      ;;
  esac
  [ "$local_sha" = "$zero" ] || pushing=true
done
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
