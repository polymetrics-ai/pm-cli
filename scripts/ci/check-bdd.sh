#!/bin/sh
# Runs the Gherkin acceptance suite inside the pinned CI image. This is the
# behavior gate. The workflow starts it through scripts/ci/in-ci-image.sh.
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
python3 tools/bootstrap.py exec docker-compose -- version

# Scenarios get the job-owned stack. Strict godog fails on undefined or pending
# steps; a failed suite is never retried. The report lists each scenario and
# the godog totals in the log and in the job summary, also on success.
python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
  python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-bdd.json" \
  --scenarios --summary-file "$GITHUB_STEP_SUMMARY" -- \
  env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
  test -json -p 1 -count=1 -timeout 40m ./tests/acceptance/...

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bdd-toolchain-after.json"
if ! cmp -s "$RUNNER_TEMP/bdd-toolchain.json" "$RUNNER_TEMP/bdd-toolchain-after.json"; then
  printf '%s\n' 'BDD native/tool identity drifted during execution' >&2
  exit 1
fi
