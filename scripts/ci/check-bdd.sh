#!/bin/sh
# SPIKE ONLY. Branch t3/72-login-spike (pushed as arch/72-login-spike). Never
# merge this file. It replaces the Gherkin run of the bdd job with the #72
# login spike (tests/loginspike). The spike prints PASS or FAIL lines and never
# prints a password, unlock secret or token. The workflow starts it through
# scripts/ci/in-ci-image.sh, as it starts the Gherkin run on main.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_RUN_ID:?Actual workflow run identity required}"
: "${GITHUB_RUN_ATTEMPT:?Actual workflow attempt required}"
: "${PMCLI_CACHE_DIR:?Persistent CI cache directory required}"
export PM_TOOLS_CACHE="$PMCLI_CACHE_DIR/pm-tools"
export GOCACHE="$PMCLI_CACHE_DIR/go-build"
export GOMODCACHE="$PMCLI_CACHE_DIR/go-mod"
mkdir -p "$PM_TOOLS_CACHE/tmp"
export TMPDIR="$PM_TOOLS_CACHE/tmp"
export GOTMPDIR="$TMPDIR"
export PM_REPO_ROOT="$repo_root"
export GOWORK=off
export GOMAXPROCS=2
export GOFLAGS=-p=2
manifest="$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"
started=$(date +%s)
step() {
  printf 'login-spike-script: [+%ss] %s\n' "$(($(date +%s) - started))" "$1"
}
printf 'Login spike candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
printf 'Runner: %s\n' "${RUNNER_NAME:?Actual runner identity required}"

step 'build pm-stack and pm-stack-fixture (the job-owned stack)'
mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
python3 tools/bootstrap.py exec docker-compose -- version

step 'build pm, pm-server and the spike from the candidate (CGO off)'
bin="$RUNNER_TEMP/pm-login-spike-bin-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
rm -rf "$bin"
mkdir -m 700 "$bin"
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o "$bin/pm" ./cmd/pm
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o "$bin/pm-server" ./cmd/pm-server
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o "$bin/loginspike" ./tests/loginspike

step 'start the job-owned stack and run the login spike'
status=0
python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
  "$bin/loginspike" run --pm "$bin/pm" --pm-server "$bin/pm-server" || status=$?
rm -rf "$bin"
step "login spike ended with exit $status"
exit "$status"
