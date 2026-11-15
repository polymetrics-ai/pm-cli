#!/bin/sh
# Runs the Gherkin acceptance suite inside the pinned CI image. This is the
# behavior gate. The workflow starts it through scripts/ci/in-ci-image.sh.
#
# Before the Gherkin run, the script runs the stack run: the Go developer tests
# of the stack packages (scripts/ci/go-packages.sh) against their own job-owned
# stack. The Gherkin run then starts a fresh stack. Both runs always run and
# report; the job fails when either run fails. Go developer tests are developer
# checks, never acceptance proof of PM behavior. See docs/development/ci.md.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_RUN_ID:?Actual workflow run identity required}"
: "${GITHUB_RUN_ATTEMPT:?Actual workflow attempt required}"
: "${PMCLI_CACHE_DIR:?Persistent CI cache directory required}"
: "${GITHUB_STEP_SUMMARY:?GitHub job summary file required}"
# The job container mounts RUNNER_TEMP at its host path, so the runner's
# summary file is writable only when it is inside RUNNER_TEMP.
case "$GITHUB_STEP_SUMMARY" in
  "$RUNNER_TEMP"/*) ;;
  *)
    printf '%s\n' 'GITHUB_STEP_SUMMARY is outside RUNNER_TEMP' >&2
    exit 1
    ;;
esac
export PM_TOOLS_CACHE="$PMCLI_CACHE_DIR/pm-tools"
export GOCACHE="$PMCLI_CACHE_DIR/go-build"
export GOMODCACHE="$PMCLI_CACHE_DIR/go-mod"
mkdir -p "$PM_TOOLS_CACHE/tmp"
export TMPDIR="$PM_TOOLS_CACHE/tmp"
export GOTMPDIR="$TMPDIR"
export PM_REPO_ROOT="$repo_root"
export GOWORK=off
export GOMAXPROCS=2
export GOMEMLIMIT=768MiB
export GOFLAGS=-p=2
manifest="$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"
printf 'BDD candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
printf 'Runner: %s\n' "${RUNNER_NAME:?Actual runner identity required}"

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain.json"
mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
# The internal/api tests in the stack run need the API generator.
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate
python3 tools/bootstrap.py exec docker-compose -- version

# Select the stack run packages by rule from scripts/ci/stack-packages.txt.
# shellcheck source=scripts/ci/go-packages.sh disable=SC1091
. scripts/ci/go-packages.sh
module=$(python3 tools/bootstrap.py exec go -- list -m)
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- list ./... > "$RUNNER_TEMP/bdd-go-packages.txt"
sed "s#^$module#.#" "$RUNNER_TEMP/bdd-go-packages.txt" > "$RUNNER_TEMP/bdd-go-packages-relative.txt"
pm_stack_entries > "$RUNNER_TEMP/bdd-stack-entries.txt"
pm_stack_run_packages < "$RUNNER_TEMP/bdd-stack-entries.txt" > "$RUNNER_TEMP/bdd-stack-packages.txt"
stack_status=0
# A stack list entry or an excluded package that `go list ./...` does not
# print is stale. It fails the stack run, and the Gherkin run still runs.
{
  cat "$RUNNER_TEMP/bdd-stack-entries.txt"
  pm_excluded_packages | cut -d ' ' -f 1
} > "$RUNNER_TEMP/bdd-listed-packages.txt"
pm_drop_paths "$RUNNER_TEMP/bdd-go-packages-relative.txt" \
  < "$RUNNER_TEMP/bdd-listed-packages.txt" > "$RUNNER_TEMP/bdd-stale-packages.txt"
# go-packages.sh sets pm_stack_list and pm_gherkin_package.
# shellcheck disable=SC2154
while IFS= read -r stale; do
  printf 'check-bdd: %s names a package that go list ./... does not print: %s\n' \
    "$pm_stack_list" "$stale" >&2
  stack_status=1
done < "$RUNNER_TEMP/bdd-stale-packages.txt"
if [ ! -s "$RUNNER_TEMP/bdd-stack-packages.txt" ]; then
  printf '%s\n' 'check-bdd: the stack run selected no package' >&2
  stack_status=1
fi
printf 'check-bdd: stack run packages (%s):\n' "$(wc -l < "$RUNNER_TEMP/bdd-stack-packages.txt" | tr -d ' ')"
sed 's/^/  /' "$RUNNER_TEMP/bdd-stack-packages.txt"
# shellcheck disable=SC2154
printf 'check-bdd: Gherkin run package: %s\n' "$pm_gherkin_package"
pm_excluded_packages | while IFS=' ' read -r package reason; do
  printf 'check-bdd: excluded from the Go test runs: %s. Reason: %s\n' "$package" "$reason"
done

# The stack run: Go developer tests of the stack packages, one package at a
# time (-p 1), because they share one schema, its roles and one Temporal
# history. A failed or skipped test fails the run. The -timeout bounds each
# test binary; with-stack-fixture.py stops its child after 20 minutes.
if [ "$stack_status" -eq 0 ]; then
  # Word splitting of the package list is intended; package paths have no spaces.
  # shellcheck disable=SC2046
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-stack.json" \
    --fail-on-skip --summary-file "$GITHUB_STEP_SUMMARY" \
    --title 'Go developer tests: stack run (bdd job). Developer checks, not acceptance proof.' -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 15m $(cat "$RUNNER_TEMP/bdd-stack-packages.txt") ||
    stack_status=$?
  case "$stack_status" in
    129 | 130 | 143)
      printf 'check-bdd: the stack run stopped on a signal (exit %s)\n' "$stack_status" >&2
      exit "$stack_status"
      ;;
  esac
else
  printf '%s\n' 'check-bdd: the stack run does not start, because its package selection failed' >&2
fi

# The Gherkin run starts on a fresh stack. The stack run's fixture is cleaned
# up when its command ends, and its manifest stays as a cleaned receipt. Move
# the receipt aside, so that the Gherkin fixture can own the manifest path that
# the workflow's cleanup step reads.
gherkin_ready=true
if [ -e "$manifest" ]; then
  if python3 scripts/ci/with-stack-fixture.py --cleanup "$manifest"; then
    mv "$manifest" "$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.stack-run-receipt.json"
  else
    printf 'check-bdd: the stack run fixture cleanup failed; its manifest stays at %s for the cleanup step, and the Gherkin run cannot start a fresh stack\n' \
      "$manifest" >&2
    gherkin_ready=false
  fi
fi

# Scenarios get the job-owned stack. Strict godog fails on undefined or pending
# steps; a failed suite is never retried. The report lists each scenario and
# the godog totals in the log and in the job summary, also on success.
gherkin_status=1
if "$gherkin_ready"; then
  gherkin_status=0
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-bdd.json" \
    --scenarios --summary-file "$GITHUB_STEP_SUMMARY" -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 40m "$pm_gherkin_package/..." ||
    gherkin_status=$?
fi

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain-after.json"
printf 'check-bdd: stack run (Go developer tests) exit %s; Gherkin run (acceptance suite) exit %s\n' \
  "$stack_status" "$gherkin_status"
if ! cmp -s "$RUNNER_TEMP/bdd-toolchain.json" "$RUNNER_TEMP/bdd-toolchain-after.json"; then
  printf '%s\n' 'BDD native/tool identity drifted during execution' >&2
  exit 1
fi
if [ "$stack_status" -ne 0 ] || [ "$gherkin_status" -ne 0 ]; then
  exit 1
fi
