#!/bin/sh
# Runs one reviewed foundation shard inside the pinned CI image.
# Usage: check-foundation.sh build|stack-a|stack-b|governance
# The workflow starts it through scripts/ci/in-ci-image.sh (docs/development/ci.md).
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
shard=${1:-}
case "$shard" in
  build | stack-a | stack-b | governance) ;;
  *)
    printf '%s\n' 'usage: check-foundation.sh build|stack-a|stack-b|governance' >&2
    exit 2
    ;;
esac
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_EVENT_PATH:?Actual workflow event required}"
: "${GITHUB_EVENT_NAME:?Actual workflow event name required}"
: "${GITHUB_RUN_ID:?Actual workflow run identity required}"
: "${GITHUB_RUN_ATTEMPT:?Actual workflow attempt required}"
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
export SYFT_CHECK_FOR_APP_UPDATE=false
manifest="$RUNNER_TEMP/pmcli-foundation-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"
printf 'Foundation candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
printf 'Runner: %s\n' "${RUNNER_NAME:?Actual runner identity required}"
printf 'Shard: %s\n' "$shard"

# Physical packages share one fixture's schema, roles and Temporal history, so
# each stack shard runs them serially (-p 1) against its own job-owned fixture.
# The split follows the last measured package durations. Browser parity runs
# Playwright and stays in the non-blocking governance shard. Every other package
# runs without a fixture in the build shard; a new physical package fails there
# until it is added to a stack list.
stack_a='./internal/app ./internal/workflows ./internal/temporal ./tests/stack'
stack_b='./internal/activities ./internal/api ./internal/auth ./internal/cli
./internal/credentials ./internal/httpapi ./internal/objects ./internal/runtime
./internal/server ./internal/store ./internal/sync ./internal/testfixture
./internal/warehouse'
governance_only='./tests/client-parity'

go_test() {
  name=$1
  shift
  python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-$name.json" -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -count=1 -timeout 15m "$@"
}

build_stack_tools() {
  mkdir -p .cache/bin
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
  # internal/api's generated-contract test runs in a stack shard and needs the generator.
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate
  python3 tools/bootstrap.py exec docker-compose -- version
}

build_shard() {
  python3 -I -B scripts/ci/check-setup.py --root . --policy scripts/ci/setup-policy.json
  python3 -I -B scripts/ci/test_setup.py
  python3 -I -B scripts/ci/test_runner_admission.py
  python3 -I -B scripts/ci/test_stack_fixture.py
  python3 -I -B tools/test_bootstrap.py
  python3 tools/bootstrap.py exec go -- version
  python3 tools/bootstrap.py exec go -- env GOVERSION GOOS GOARCH CGO_ENABLED CC CXX
  python3 tools/bootstrap.py exec clang -- --version
  python3 tools/bootstrap.py exec clang -- -print-target-triple
  python3 tools/bootstrap.py exec clang -- -print-resource-dir
  python3 tools/bootstrap.py exec clang -- -print-prog-name=ld
  mkdir -p .cache/bin
  CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- build -o .cache/bin/nativeprobe ./build/nativeprobe
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-dependency-check ./cmd/pm-dependency-check
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pmgen ./cmd/pmgen
  build_stack_tools
  sh scripts/generate.sh --check
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/oapi-codegen github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/staticcheck honnef.co/go/tools/cmd/staticcheck
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/govulncheck golang.org/x/vuln/cmd/govulncheck
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools test . -run '^TestResolvedDependencyPolicy$' -count=1 -timeout 5m
  CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- build ./...
  CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- vet ./...
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools vet ./...
  .cache/bin/oapi-codegen -version
  .cache/bin/staticcheck -version
  .cache/bin/govulncheck -version
  python3 scripts/ci/check-native.py
  sh scripts/check-boundaries.sh
  sh scripts/browser.sh install
  sh scripts/browser.sh build
  python3 scripts/ci/check-image-pins.py
  for package in $stack_a $stack_b $governance_only; do
    printf 'github.com/polymetrics-ai/pm-cli/%s\n' "${package#./}"
  done > "$RUNNER_TEMP/foundation-stack-packages.txt"
  CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- list ./... > "$RUNNER_TEMP/foundation-packages.txt"
  grep -v -x -F -f "$RUNNER_TEMP/foundation-stack-packages.txt" \
    "$RUNNER_TEMP/foundation-packages.txt" > "$RUNNER_TEMP/foundation-unit-packages.txt"
  # Word splitting of the package list is intended.
  # shellcheck disable=SC2046
  go_test build $(cat "$RUNNER_TEMP/foundation-unit-packages.txt")
}

stack_shard() {
  name=$1
  shift
  build_stack_tools
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 scripts/ci/go-test-report.py --output "$RUNNER_TEMP/go-test-$name.json" -- \
    env CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- \
    test -json -p 1 -count=1 -timeout 15m "$@"
}

# Atlas, architecture documents, Bazel and supply-chain reports. Not required.
governance_shard() {
  python3 scripts/install_architecture_docs_dependencies.py \
    --destination "$RUNNER_TEMP/foundation-docs-deps"
  PYTHONPATH="$RUNNER_TEMP/foundation-docs-deps" python3 -B scripts/test_validate_architecture_docs.py
  PYTHONPATH="$RUNNER_TEMP/foundation-docs-deps" python3 -B scripts/validate_architecture_docs.py
  mkdir -p .cache/bin
  CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/atlas ./atlas
  .cache/bin/atlas --root "$repo_root" --check
  build_stack_tools
  sh scripts/browser.sh install
  python3 tools/bootstrap.py exec bazelisk -- --batch build //...
  # Physical targets declare exclusive execution for their shared schema/roles.
  python3 scripts/ci/with-stack-fixture.py --manifest "$manifest" -- \
    python3 tools/bootstrap.py exec bazelisk -- --batch test //... \
    --test_output=errors --cache_test_results=no
  python3 scripts/ci/check-dependencies.py --evidence-dir "$RUNNER_TEMP/foundation-sboms"
  python3 scripts/ci/benchmark.py
}

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/foundation-toolchain-$shard.json"
# Word splitting of the package lists is intended.
# shellcheck disable=SC2086
case "$shard" in
  build) build_shard ;;
  stack-a) stack_shard stack-a $stack_a ;;
  stack-b) stack_shard stack-b $stack_b ;;
  governance) governance_shard ;;
esac

# Re-open and re-hash the admitted native inputs after every shard action.
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/foundation-toolchain-$shard-after.json"
if ! cmp -s \
  "$RUNNER_TEMP/foundation-toolchain-$shard.json" \
  "$RUNNER_TEMP/foundation-toolchain-$shard-after.json"; then
  printf '%s\n' 'foundation native/tool identity drifted during execution' >&2
  exit 1
fi
