#!/bin/sh
# Git pre-commit hook: fast checks on the staged change. A failure stops the
# commit. See docs/development/ci.md.
set -eu
root=$(git rev-parse --show-toplevel)
cd "$root"
# shellcheck source=scripts/dev/hook-lib.sh disable=SC1091
. scripts/dev/hook-lib.sh

pm_in_image scripts/dev/hook-pre-commit.sh "$@"
python3 -I -B scripts/dev/secret_scan.py staged

staged=$(git diff --cached --name-only --diff-filter=ACMR)
[ -n "$staged" ] || exit 0

go_files=$(printf '%s\n' "$staged" | grep '\.go$' | grep -v '/testdata/' || true)
[ -n "$go_files" ] || exit 0

goroot=$(pgo env GOROOT)
# Word splitting of the file list is intended; paths have no spaces.
# shellcheck disable=SC2086
unformatted=$("$goroot/bin/gofmt" -l $go_files)
if [ -n "$unformatted" ]; then
  printf 'pre-commit: these files need gofmt:\n%s\n' "$unformatted" >&2
  exit 1
fi

app=$(printf '%s\n' "$go_files" | grep -v '^tools/' | while IFS= read -r file; do dirname "$file"; done | sort -u | sed 's#^#./#' || true)
tools=$(printf '%s\n' "$go_files" | grep '^tools/' | sed 's#^tools/##' | while IFS= read -r file; do dirname "$file"; done | sort -u | sed 's#^#./#' || true)

# shellcheck disable=SC2086
if [ -n "$app" ]; then
  printf 'pre-commit: go vet %s\n' "$(printf '%s ' $app)"
  CGO_ENABLED=1 pgo vet $app
  unit=$(printf '%s\n' "$app" | drop_stack_packages)
  if [ -n "$unit" ]; then
    printf 'pre-commit: go test %s\n' "$(printf '%s ' $unit)"
    CGO_ENABLED=1 pgo test -count=1 -timeout 10m $unit
  fi
  if [ -x .cache/bin/staticcheck ]; then
    # Advisory: the tree has existing Staticcheck findings, so this never blocks.
    PATH="$goroot/bin:$PATH" CGO_ENABLED=1 .cache/bin/staticcheck $app ||
      printf '%s\n' 'pre-commit: Staticcheck findings above are advisory.' >&2
  fi
fi
# shellcheck disable=SC2086
if [ -n "$tools" ]; then
  CGO_ENABLED=0 pgo -C tools vet $tools
  CGO_ENABLED=0 pgo -C tools test -count=1 -timeout 10m $tools
fi
