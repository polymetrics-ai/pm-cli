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

# --- functions that scripts/ci/test_setup.py runs: begin ---
# Job cancellation. The job container's init process passes SIGHUP, SIGINT
# and SIGTERM to this script. The script sets no trap for them, so such a
# signal stops it at once, also while a command runs: sh in the CI image is
# dash, which keeps the default action. A trap would run only after that
# command ends. So when the script reaches pm_stack_ended, the job is not
# cancelled.
#
# pm_stack_ended STATUS reports the end of the stack run. A status of 129, 130
# or 143 means that the stack run command ended on a signal that this script
# did not get. That is a stack run failure: the receipt check and the Gherkin
# run still run, and the job fails.
pm_stack_ended() {
  step "stack run ended with exit $1"
  case "$1" in
    129 | 130 | 143)
      printf 'check-bdd: the stack run command ended on a signal (exit %s), and this script got no signal, so the job is not cancelled. This is a stack run failure; the receipt check and the Gherkin run still run\n' "$1" >&2
      ;;
  esac
}
# --- functions that scripts/ci/test_setup.py runs: end ---

# The time budget. The workflow step has a frozen limit of 90 minutes
# (bdd.yml). The script keeps 3 minutes of it for the start and stop of the
# job container, so its own work ends within 87 minutes of its start, on the
# failure path too. Owner decision of 2026-11-17: the stack go test command
# stops one hour after the start (it stopped after 20 minutes before).
#
# The two deadlines stop only the go test command of each run, inside
# go-test-report.py (--deadline). The fixture start before that command and
# the fixture cleanup after it are outside the deadlines. A stopped command
# gets SIGTERM, then SIGKILL after 30 seconds (the stop grace), and the run
# still reports. --timeout also stops each go test command after its start:
# 3600 s (60 minutes) for the stack run and 1140 s (19 minutes) for the
# Gherkin run. Each one plus the 30-second stop grace ends before the
# 61-minute command limit of with-stack-fixture.py (CHILD_TIMEOUT), so that
# the reporter, not the wrapper, ends the run. The stack --timeout cannot stop
# the stack command before its deadline, because that command starts after
# the script.
#
# The allowances are upper bounds of the phases of with-stack-fixture.py: a
# fixture start (generator 60 s, `pm-stack up` 600 s and its 30-second Docker
# calls) and a fixture cleanup (drain 120 s, container stop 75 s, cleanup
# helper 60 s and its 30-second Docker calls). The prelude (tool builds and
# package selection) must take 8 minutes or less. Then, in minutes after the
# start, on the longest path:
#   stack go test stop  <= +60   (stack deadline)
#   stack run end       <= +60.5 (stop grace)
#   stack cleanup end   <= +66.5 (cleanup allowance)
#   receipt check end   <= +67.5 (finish allowance)
#   Gherkin start end   <= +79.5 (start allowance; this is the Gherkin deadline)
#   Gherkin run end     <= +80   (stop grace)
#   Gherkin cleanup end <= +86   (cleanup allowance)
#   toolchain check end <= +87   (finish allowance)
# If the prelude and the stack fixture start end after +60, the stack go test
# command stops at once. The end bound holds for a prelude of 8 minutes or less.
#
# gherkin_minimum is 0. A Gherkin reserve counts only on the path where the
# stack run reaches its deadline and each fixture phase uses its whole
# allowance, but it took that time from the stack run on every run. On that
# one path (stack run at its deadline, stack cleanup at its full 6 minutes and
# Gherkin fixture start at its full 12 minutes), the Gherkin run can get no
# test time. It still reports that it stopped at its deadline, and the job
# fails (E4 narrowing). On a normal run, the Gherkin go test command keeps its
# own 19-minute limit (--timeout 1140). See docs/development/ci.md.
# Each live fixture stays under the one manifest path that the frozen cleanup
# step of bdd.yml reads ($manifest below). See "Cleanup after an abrupt stop"
# in docs/development/ci.md.
budget=5220
start_allowance=720
cleanup_allowance=360
finish_allowance=60
stop_grace=30
gherkin_minimum=0
gherkin_deadline=$((started + budget - finish_allowance - cleanup_allowance - stop_grace))
stack_deadline=$((gherkin_deadline - gherkin_minimum - start_allowance - finish_allowance - cleanup_allowance - stop_grace))
step "budget: the stack go test command stops at +$((stack_deadline - started))s, the Gherkin go test command at +$((gherkin_deadline - started))s, the script ends by +${budget}s. Fixture starts and cleanups are outside these deadlines."

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
# The intended set of the Gherkin run: the packages that its go test pattern
# selects, from go list in the same environment, before the run. The Gherkin
# run passes it to go-test-report.py --expect, so a package with no result, a
# second result or an unexpected result fails the run. A failed or empty
# selection means that the Gherkin run does not run, and the job fails.
gherkin_selected=false
# go-packages.sh sets pm_gherkin_package.
# shellcheck disable=SC2154
if CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- list "$pm_gherkin_package/..." \
  > "$RUNNER_TEMP/bdd-gherkin-expected.txt" && [ -s "$RUNNER_TEMP/bdd-gherkin-expected.txt" ]; then
  gherkin_selected=true
  printf 'check-bdd: Gherkin run packages (%s):\n' "$(wc -l < "$RUNNER_TEMP/bdd-gherkin-expected.txt" | tr -d ' ')"
  sed 's/^/  /' "$RUNNER_TEMP/bdd-gherkin-expected.txt"
else
  printf '%s\n' 'check-bdd: the Gherkin run package selection failed or selected no package' >&2
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
    --deadline "$stack_deadline" --timeout 3600 \
    --summary-file "$GITHUB_STEP_SUMMARY" \
    --title 'Go developer tests: stack run (bdd job). Developer checks, not acceptance proof.' -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 15m $(cat "$RUNNER_TEMP/bdd-stack-packages.txt") ||
    stack_status=$?
  pm_stack_ended "$stack_status"
else
  printf '%s\n' 'check-bdd: the stack run did not run: its package selection failed' >&2
  stack_ran=false
fi

# The Gherkin run starts on a fresh stack. The stack run's fixture is cleaned
# up when its command ends, and its manifest stays as a cleaned receipt. Move
# the receipt aside, so that the Gherkin fixture can own the manifest path that
# the workflow's cleanup step reads. --check-receipt only reads the manifest
# and checks the phase "cleaned"; it makes no Docker call and does not repeat
# the cleanup. If the stack fixture is not clean, the manifest stays for the
# cleanup step, and the Gherkin run does not start.
gherkin_ready=true
if [ -e "$manifest" ]; then
  step 'check the stack run fixture cleanup and keep its receipt'
  if python3 scripts/ci/with-stack-fixture.py --check-receipt "$manifest" &&
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
# the godog totals in the log and in the job summary, also on success. The run
# fails on 0 scenarios, a skipped step or scenario (--scenarios) and a skipped
# Go test or subtest (--fail-on-skip), and the log and the summary name each
# cause (issue #350).
# --expect checks one package result for each package of the intended set.
# --deadline bounds the whole run.
gherkin_status=1
gherkin_ran=false
if ! "$gherkin_selected"; then
  printf '%s\n' 'check-bdd: the Gherkin run did not run: its package selection failed' >&2
elif "$gherkin_ready"; then
  step 'starting a fresh job-owned stack for the Gherkin run (acceptance suite)'
  gherkin_status=0
  gherkin_ran=true
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-bdd.json" \
    --scenarios --fail-on-skip --expect "$RUNNER_TEMP/bdd-gherkin-expected.txt" \
    --deadline "$gherkin_deadline" --timeout 1140 \
    --summary-file "$GITHUB_STEP_SUMMARY" -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 40m "$pm_gherkin_package/..." ||
    gherkin_status=$?
  step "Gherkin run ended with exit $gherkin_status"
fi

step 'compare the toolchain identity'
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain-after.json"
if "$stack_ran"; then stack_result="exit $stack_status"; else stack_result='did not run'; fi
if "$gherkin_ran"; then gherkin_result="exit $gherkin_status"; else gherkin_result='did not run'; fi
printf 'check-bdd: stack run (Go developer tests) %s; Gherkin run (acceptance suite) %s\n' \
  "$stack_result" "$gherkin_result"
if ! cmp -s "$RUNNER_TEMP/bdd-toolchain.json" "$RUNNER_TEMP/bdd-toolchain-after.json"; then
  printf '%s\n' 'BDD native/tool identity drifted during execution' >&2
  exit 1
fi
if [ "$stack_status" -ne 0 ] || [ "$gherkin_status" -ne 0 ]; then
  exit 1
fi
