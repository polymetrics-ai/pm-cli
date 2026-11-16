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
# The time budget below starts here.
started=$(date +%s)
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

# Each step line shows the seconds since the script started.
step() {
  printf 'check-bdd: [+%ss] %s\n' "$(($(date +%s) - started))" "$1"
}

# The time budget. The workflow step has a frozen limit of 50 minutes
# (bdd.yml). The script keeps 3 minutes of it for the start and stop of the
# job container, so its own work ends within 47 minutes of its start, on the
# failure path too. go-test-report.py --deadline stops each run at an absolute
# time and still reports. --timeout 1140 also stops each run 19 minutes after
# its start, before the 20-minute command limit of with-stack-fixture.py
# (CHILD_TIMEOUT), so that the reporter, not the wrapper, ends the run. The allowances are upper bounds of the phases of
# with-stack-fixture.py: a fixture start (generator 60 s, `pm-stack up` 600 s
# and its 30-second Docker calls) and a fixture cleanup (drain 120 s, container
# stop 75 s, cleanup helper 60 s and its 30-second Docker calls). A run that
# reaches its deadline gets SIGTERM, then SIGKILL after 30 seconds (the stop
# grace). The prelude (tool builds and package selection) must take 8 minutes
# or less. Then, in minutes after the start:
#   stack run end      <= +15.5 (deadline +15 and the stop grace)
#   stack cleanup end  <= +21.5 (cleanup allowance)
#   Gherkin start end  <= +34.5 (finish and start allowances)
#   Gherkin run end    <= +40   (deadline +39.5 and the stop grace)
#   Gherkin cleanup    <= +46   (cleanup allowance)
#   toolchain check    <= +47   (finish allowance)
# When the prelude takes more than 3 minutes and the stack fixture start uses
# its whole allowance, the stack run starts after its deadline and stops at
# once. The end bound holds for a prelude of 8 minutes or less. The Gherkin run
# gets 5 minutes or more of test time when the prelude and the stack fixture
# start end by +15. See docs/development/ci.md.
# Each live fixture stays under the one manifest path that the frozen cleanup
# step of bdd.yml reads ($manifest below). See "Cleanup after an abrupt stop"
# in docs/development/ci.md.
budget=2820
start_allowance=720
cleanup_allowance=360
finish_allowance=60
stop_grace=30
gherkin_minimum=300
gherkin_deadline=$((started + budget - finish_allowance - cleanup_allowance - stop_grace))
stack_deadline=$((gherkin_deadline - gherkin_minimum - start_allowance - finish_allowance - cleanup_allowance - stop_grace))
step "budget: the stack run stops at +$((stack_deadline - started))s, the Gherkin run at +$((gherkin_deadline - started))s, the script ends by +${budget}s"

step 'build pm-stack, pm-stack-fixture and pm-api-generate from the candidate'
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain.json"
mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
# The internal/api tests in the stack run use the API generator at its default
# path .cache/bin/pm-api-generate when PM_API_GENERATE is not set
# (internal/api/protocol_test.go). The script builds that file from the
# candidate and unsets PM_API_GENERATE, so that no other generator applies.
unset PM_API_GENERATE
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate
python3 tools/bootstrap.py exec docker-compose -- version

# Select the stack run packages by rule from scripts/ci/stack-packages.txt.
# A selection failure fails the stack run, and the Gherkin run still runs.
step 'select the Go test packages'
# shellcheck source=scripts/ci/go-packages.sh disable=SC1091
. scripts/ci/go-packages.sh
stack_status=0
stack_ran=true
if module=$(python3 tools/bootstrap.py exec go -- list -m) &&
  CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- list ./... > "$RUNNER_TEMP/bdd-go-packages.txt" &&
  pm_stack_entries > "$RUNNER_TEMP/bdd-stack-entries.txt" &&
  pm_stack_run_packages < "$RUNNER_TEMP/bdd-stack-entries.txt" > "$RUNNER_TEMP/bdd-stack-packages.txt"; then
  sed "s#^$module#.#" "$RUNNER_TEMP/bdd-go-packages.txt" > "$RUNNER_TEMP/bdd-go-packages-relative.txt"
  sed "s#^\.#$module#" "$RUNNER_TEMP/bdd-stack-packages.txt" > "$RUNNER_TEMP/bdd-stack-expected.txt"
  # A stack list entry or an excluded package that `go list ./...` does not
  # print is stale.
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
else
  printf '%s\n' 'check-bdd: the package selection failed' >&2
  stack_status=1
fi
# shellcheck disable=SC2154
printf 'check-bdd: Gherkin run package: %s\n' "$pm_gherkin_package"
pm_excluded_packages | while IFS=' ' read -r package reason; do
  printf 'check-bdd: excluded from the Go test runs: %s. Reason: %s\n' "$package" "$reason"
done

# The stack run: Go developer tests of the stack packages, one package at a
# time (-p 1), because they share one schema, its roles and one Temporal
# history. A failed or skipped test fails the run. go-test-report.py --expect
# fails the run when a selected package has no result or more than one
# result, or when the stream is empty. The command uses no -run and no
# build-tag filter, and go-test-report.py refuses one. The -timeout bounds
# each test binary, and --deadline bounds the whole run.
if [ "$stack_status" -eq 0 ]; then
  step 'starting a fresh job-owned stack for the stack run (Go developer tests of the stack packages)'
  # Word splitting of the package list is intended; package paths have no spaces.
  # shellcheck disable=SC2046
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-stack.json" \
    --fail-on-skip --expect "$RUNNER_TEMP/bdd-stack-expected.txt" \
    --deadline "$stack_deadline" --timeout 1140 \
    --summary-file "$GITHUB_STEP_SUMMARY" \
    --title 'Go developer tests: stack run (bdd job). Developer checks, not acceptance proof.' -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 15m $(cat "$RUNNER_TEMP/bdd-stack-packages.txt") ||
    stack_status=$?
  step "stack run ended with exit $stack_status"
  case "$stack_status" in
    129 | 130 | 143)
      printf 'check-bdd: the stack run stopped on a signal (exit %s)\n' "$stack_status" >&2
      exit "$stack_status"
      ;;
  esac
else
  printf '%s\n' 'check-bdd: the stack run did not run: its package selection failed' >&2
  stack_ran=false
fi

# The Gherkin run starts on a fresh stack. The stack run's fixture is cleaned
# up when its command ends, and its manifest stays as a cleaned receipt. Move
# the receipt aside, so that the Gherkin fixture can own the manifest path that
# the workflow's cleanup step reads. If the stack fixture is not clean, the
# manifest stays for the cleanup step, and the Gherkin run cannot start.
gherkin_ready=true
if [ -e "$manifest" ]; then
  step 'check the stack run fixture cleanup and keep its receipt'
  if python3 scripts/ci/with-stack-fixture.py --cleanup "$manifest" &&
    mv "$manifest" "$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.stack-run-receipt.json"; then
    :
  else
    printf 'check-bdd: the Gherkin run did not run: the stack run fixture is not clean, so its manifest stays at %s for the cleanup step, and no fresh stack can start on that path\n' \
      "$manifest" >&2
    gherkin_ready=false
    [ "$stack_status" -ne 0 ] || stack_status=1
  fi
fi

# Scenarios get the job-owned stack. Strict godog fails on undefined or pending
# steps; a failed suite is never retried. The report lists each scenario and
# the godog totals in the log and in the job summary, also on success.
# --deadline bounds the whole run.
gherkin_status=1
if "$gherkin_ready"; then
  step 'starting a fresh job-owned stack for the Gherkin run (acceptance suite)'
  gherkin_status=0
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-bdd.json" \
    --scenarios --deadline "$gherkin_deadline" --timeout 1140 \
    --summary-file "$GITHUB_STEP_SUMMARY" -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 40m "$pm_gherkin_package/..." ||
    gherkin_status=$?
  step "Gherkin run ended with exit $gherkin_status"
fi

step 'compare the toolchain identity'
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain-after.json"
if "$stack_ran"; then stack_result="exit $stack_status"; else stack_result='did not run'; fi
if "$gherkin_ready"; then gherkin_result="exit $gherkin_status"; else gherkin_result='did not run'; fi
printf 'check-bdd: stack run (Go developer tests) %s; Gherkin run (acceptance suite) %s\n' \
  "$stack_result" "$gherkin_result"
if ! cmp -s "$RUNNER_TEMP/bdd-toolchain.json" "$RUNNER_TEMP/bdd-toolchain-after.json"; then
  printf '%s\n' 'BDD native/tool identity drifted during execution' >&2
  exit 1
fi
if [ "$stack_status" -ne 0 ] || [ "$gherkin_status" -ne 0 ]; then
  exit 1
fi
