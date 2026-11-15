#!/bin/sh
# Static repository checks and the Go unit run for the `checks` CI job and the
# git hooks. They run no behavior test: the Gherkin suite (`bdd`) is the only
# behavior gate. The `unit` check runs Go developer tests. They are developer
# checks, never acceptance proof of PM behavior.
#
#   check-docs.sh            run every check below
#   check-docs.sh NAME...    run only the named checks (the git hooks use this)
#   check-docs.sh --list     print the check names, one per line
#
# PMCLI_PYTHON names the Python interpreter for the checks (default python3).
# The macOS git hooks set it. CI ignores it and uses python3.
# Checks: setup python generate gofmt boundaries tools unit.
# `unit` runs `go test` of every root-module package outside the stack list
# (scripts/ci/go-packages.sh). The pre-push hook runs the same packages itself
# and does not select `unit`.
# CI and the Linux git hooks run this script inside the pinned job image
# (scripts/ci/in-ci-image.sh). See docs/development/ci.md.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
all='setup python generate gofmt boundaries tools unit'
if [ "$#" -eq 1 ] && [ "$1" = --list ]; then
  # shellcheck disable=SC2086
  printf '%s\n' $all
  exit 0
fi
if [ "$#" -eq 0 ]; then
  # Word splitting of the check list is intended.
  # shellcheck disable=SC2086
  set -- $all
fi
for name in "$@"; do
  case " $all " in
    *" $name "*) ;;
    *)
      printf 'usage: check-docs.sh [%s]...\n' "$(printf '%s' "$all" | tr ' ' '|')" >&2
      exit 2
      ;;
  esac
done

py=${PMCLI_PYTHON:-python3}
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  py=python3
  # Share the Go caches and resource bounds of the bdd job on the same runner.
  : "${PMCLI_CACHE_DIR:?Persistent CI cache directory required}"
  export PM_TOOLS_CACHE="$PMCLI_CACHE_DIR/pm-tools"
  export GOCACHE="$PMCLI_CACHE_DIR/go-build"
  export GOMODCACHE="$PMCLI_CACHE_DIR/go-mod"
  export GOMAXPROCS=2
  export GOMEMLIMIT=768MiB
  export GOFLAGS=-p=2
  mkdir -p "$PM_TOOLS_CACHE/tmp"
  export TMPDIR="$PM_TOOLS_CACHE/tmp"
  export GOTMPDIR="$TMPDIR"
  printf 'Checks candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
fi
export PM_REPO_ROOT="$repo_root"
export GOWORK=off
work=$(mktemp -d "${TMPDIR:-/tmp}/pmcli-checks-XXXXXX")
trap 'rm -rf "$work"' EXIT
started=$(date +%s)

pgo() {
  "$py" tools/bootstrap.py exec go -- "$@"
}

# Each step line shows the seconds since the script started.
step() {
  printf 'check-docs: [+%ss] %s\n' "$(($(date +%s) - started))" "$1"
}

# The tools come from the candidate. A warm cache does not guarantee that a
# previous build left them in .cache/bin.
build_tools() {
  step "build $*"
  mkdir -p .cache/bin
  for name in "$@"; do
    case "$name" in
      pm-api-generate) CGO_ENABLED=0 pgo -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate ;;
      pmgen) CGO_ENABLED=0 pgo build -o .cache/bin/pmgen ./cmd/pmgen ;;
    esac
  done
}

check_setup() {
  step 'check-setup.py'
  "$py" -I -B scripts/ci/check-setup.py --root . --policy scripts/ci/setup-policy.json
}

check_python() {
  # The developer container mounts the verified host scanner. A CI job installs
  # the pinned, checksummed scanner into its cache when it is missing.
  if ! "$py" -I -B scripts/dev/secret_scan.py binary >/dev/null 2>&1; then
    if [ "${GITHUB_ACTIONS:-}" != true ]; then
      "$py" -I -B scripts/dev/secret_scan.py binary
      exit 1
    fi
    "$py" -I -B scripts/dev/secret_scan.py install
  fi
  for test in scripts/ci/test_setup.py scripts/ci/test_runner_admission.py \
    scripts/ci/test_stack_fixture.py tools/test_bootstrap.py scripts/dev/test_secret_scan.py; do
    step "$test"
    "$py" -I -B "$test"
  done
}

check_generate() {
  step 'generate.sh --check (API and connectors)'
  sh scripts/generate.sh --check
}

check_gofmt() {
  step 'gofmt -l (every tracked Go file)'
  gofmt=$(pgo env GOROOT)/bin/gofmt
  git ls-files -z -- '*.go' > "$work/go-files"
  unformatted=$(xargs -0 -r "$gofmt" -l < "$work/go-files")
  if [ -n "$unformatted" ]; then
    printf 'check-docs: these files need gofmt:\n%s\n' "$unformatted" >&2
    return 1
  fi
}

check_boundaries() {
  step 'check-boundaries.sh'
  sh scripts/check-boundaries.sh
}

check_tools() {
  step 'go test (tools module)'
  CGO_ENABLED=0 pgo -C tools test -count=1 -timeout 10m ./...
}

# The unit run: Go developer tests of every root-module package that is not in
# scripts/ci/stack-packages.txt and not excluded. The packages are selected by
# rule, so a new package runs here with no list edit. A failed or skipped test
# fails the check. A package with no test files is not a test skip. The report
# lists each package with its result and time, in the log and, in CI, in the
# job summary. go-test-report.py --expect fails the check when a selected
# package has no result or more than one result, or when the stream is empty.
# The command uses no -run and no build-tag filter, and go-test-report.py
# refuses one. The -timeout bounds each test binary. --timeout stops the
# whole run after 10 minutes and still reports, inside the 17-minute step
# limit of the `checks` job.
check_unit() {
  step 'go test (unit run: root-module packages outside the stack list)'
  # shellcheck source=scripts/ci/go-packages.sh disable=SC1091
  . scripts/ci/go-packages.sh
  module=$(pgo list -m)
  CGO_ENABLED=1 pgo list ./... > "$work/go-packages"
  sed "s#^$module#.#" "$work/go-packages" > "$work/go-packages-relative"
  pm_drop_stack_packages < "$work/go-packages-relative" > "$work/unit-packages"
  if [ ! -s "$work/unit-packages" ]; then
    printf '%s\n' 'check-docs: the unit run selected no package' >&2
    return 1
  fi
  printf 'check-docs: unit run packages (%s):\n' "$(wc -l < "$work/unit-packages" | tr -d ' ')"
  sed 's/^/  /' "$work/unit-packages"
  sed "s#^\.#$module#" "$work/unit-packages" > "$work/unit-expected"
  set -- --output "$work/go-test-unit.json" --fail-on-skip \
    --expect "$work/unit-expected" --timeout 600 \
    --title 'Go developer tests: unit run (checks job). Developer checks, not acceptance proof.'
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    set -- "$@" --summary-file "${GITHUB_STEP_SUMMARY:?GitHub job summary file required}"
  fi
  # Word splitting of the package list is intended; package paths have no spaces.
  # shellcheck disable=SC2046
  "$py" -I -B scripts/ci/go-test-report.py "$@" -- \
    env CGO_ENABLED=1 "$py" tools/bootstrap.py exec go -- \
    test -json -count=1 -timeout 10m $(cat "$work/unit-packages")
}

tools=
for name in "$@"; do
  case "$name" in
    generate) tools="$tools pm-api-generate pmgen" ;;
  esac
done
# shellcheck disable=SC2086
[ -z "$tools" ] || build_tools $tools
for name in "$@"; do
  "check_$name"
done
step 'passed'
