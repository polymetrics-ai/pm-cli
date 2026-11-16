#!/bin/sh
# Runs the Bazel build and test of every target (//...) inside the pinned CI
# image. The workflow starts it through scripts/ci/in-ci-image.sh. The `bazel`
# job is a blocking check. See "The bazel check" in docs/development/ci.md.
#
# `bazel test //...` runs against one job-owned dependency stack, because the
# physical test targets need it. Test results never come from a cache
# (--cache_test_results=no). The script uses no target filter, no tag filter
# and no --keep_going.
#
# Cache layout under the Bazel output user root ($PM_TOOLS_CACHE/bazel, set by
# tools/bootstrap.py):
#   install/<md5>                 install base, shared by all jobs
#   cache/repos/v1                repository download cache, shared; each entry
#                                 is named by its SHA-256
#   contents-<class>              repository contents cache, one per class
#   base-<class>-<slot>           output base, one per class and slot
# The class is `main` for a push to refs/heads/main and `branch` for every
# other ref. A main job never reads a branch output base or branch repository
# contents. The job holds a lock on its slot for its whole run, so two jobs on
# one host never share an output base. All jobs run as one host user, so this
# split stops accidental reuse only. It cannot stop a job that writes to the
# main paths on purpose (see "Trust boundary without root" in ci.md).
set -eu
started=$(date +%s)
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_RUN_ID:?Actual workflow run identity required}"
: "${GITHUB_RUN_ATTEMPT:?Actual workflow attempt required}"
: "${GITHUB_REF:?Actual workflow ref required}"
: "${PMCLI_CACHE_DIR:?Persistent CI cache directory required}"
export PM_TOOLS_CACHE="$PMCLI_CACHE_DIR/pm-tools"
export GOCACHE="$PMCLI_CACHE_DIR/go-build"
export GOMODCACHE="$PMCLI_CACHE_DIR/go-mod"
mkdir -p "$PM_TOOLS_CACHE/tmp"
# Native link intermediates belong on the persistent cache disk, not /tmp.
export TMPDIR="$PM_TOOLS_CACHE/tmp"
export GOTMPDIR="$TMPDIR"
export PM_REPO_ROOT="$repo_root"
export GOWORK=off
export GOMAXPROCS=2
export GOMEMLIMIT=768MiB
export GOFLAGS=-p=2
manifest="$RUNNER_TEMP/pmcli-bazel-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"
printf 'Bazel candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
printf 'Runner: %s\n' "${RUNNER_NAME:?Actual runner identity required}"

# Each step line shows the seconds since the script started.
step() {
  printf 'check-bazel: [+%ss] %s\n' "$(($(date +%s) - started))" "$1"
}

# Select the cache class. The script compares the ref and never runs it.
case "$GITHUB_REF" in
  refs/heads/main) class=main ;;
  *) class=branch ;;
esac

# Take the first free slot of this class. The lock stays on file descriptor 9
# until the script exits. The process exit also releases it after a
# cancellation. The slot count is the number of runner slots on one host, so
# each host keeps at most two output bases for each class.
command -v flock >/dev/null 2>&1 || {
  printf '%s\n' 'check-bazel: flock is unavailable in the CI image' >&2
  exit 1
}
bazel_root="$PM_TOOLS_CACHE/bazel"
mkdir -p "$PM_TOOLS_CACHE/locks" "$bazel_root"
slots=2
slot=
n=0
while [ "$n" -lt "$slots" ]; do
  exec 9>"$PM_TOOLS_CACHE/locks/bazel-$class-$n.lock"
  if flock -n 9; then
    slot=$n
    break
  fi
  n=$((n + 1))
done
if [ -z "$slot" ]; then
  exec 9>&-
  printf 'check-bazel: no free Bazel slot for class %s: %s/locks/bazel-%s-[0-%s].lock are held\n' \
    "$class" "$PM_TOOLS_CACHE" "$class" "$((slots - 1))" >&2
  exit 1
fi
output_base="$bazel_root/base-$class-$slot"
repository_cache="$bazel_root/cache/repos/v1"
contents_cache="$bazel_root/contents-$class"
step "cache class $class, slot $slot: output base $output_base, repository contents cache $contents_cache, shared repository cache $repository_cache"

step 'prepare the pinned tools and record the toolchain identity'
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bazel-toolchain.json"

# //tests/client-parity needs the browser dependencies at loading time
# (glob of node_modules, allow_empty = False) and the built browser SDK under
# .cache/browser-sdk as test data. `bazel build //...` fails without them.
step 'install and build the browser SDK and the client-parity dependencies'
sh scripts/browser.sh install
sh scripts/browser.sh build

# with-stack-fixture.py starts the stack with these two binaries and the
# pinned Compose.
step 'build pm-stack and pm-stack-fixture from the candidate'
mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
python3 tools/bootstrap.py exec docker-compose -- version

# Each phase records its exit status. The toolchain comparison and the
# closing line run on every path, and the script exits nonzero when any part
# failed. The test phase runs only after a passing build.
step 'bazel build //...'
build_status=0
python3 tools/bootstrap.py exec bazelisk -- --output_base="$output_base" --batch \
  build --repository_cache="$repository_cache" --repo_contents_cache="$contents_cache" \
  //... || build_status=$?
step "bazel build //... ended with exit $build_status"

# Physical targets declare exclusive execution for their shared schema, roles
# and history. with-stack-fixture.py stops the command 20 minutes after its
# start (CHILD_TIMEOUT).
test_ran=false
test_status=0
if [ "$build_status" -eq 0 ]; then
  test_ran=true
  step 'starting a fresh job-owned stack for bazel test //...'
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 tools/bootstrap.py exec bazelisk -- --output_base="$output_base" --batch \
    test --repository_cache="$repository_cache" --repo_contents_cache="$contents_cache" \
    --test_output=errors --cache_test_results=no \
    //... || test_status=$?
  step "bazel test //... ended with exit $test_status"
else
  printf '%s\n' 'check-bazel: bazel test //... did not run: bazel build //... failed' >&2
fi

# A cancelled job stops on a signal. Stop at once, as check-bdd.sh does.
for status in "$build_status" "$test_status"; do
  case "$status" in
    129 | 130 | 143)
      printf 'check-bazel: a Bazel phase stopped on a signal (exit %s)\n' "$status" >&2
      exit "$status"
      ;;
  esac
done

step 'compare the toolchain identity'
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/bazel-toolchain-after.json"
if "$test_ran"; then test_result="exit $test_status"; else test_result='did not run'; fi
printf 'check-bazel: bazel build exit %s; bazel test %s\n' "$build_status" "$test_result"
if ! cmp -s "$RUNNER_TEMP/bazel-toolchain.json" "$RUNNER_TEMP/bazel-toolchain-after.json"; then
  printf '%s\n' 'Bazel native/tool identity drifted during execution' >&2
  exit 1
fi
step 'the toolchain identity is unchanged'
if [ "$build_status" -ne 0 ] || [ "$test_status" -ne 0 ]; then
  exit 1
fi
