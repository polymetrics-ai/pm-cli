#!/bin/sh
# Runs only reviewed foundation source on the existing dedicated trusted runner.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_EVENT_PATH:?Actual pull-request event required}"
: "${GITHUB_RUN_ID:?Actual workflow run identity required}"
: "${GITHUB_RUN_ATTEMPT:?Actual workflow attempt required}"
export PATH="/opt/pmcli-tools/venv/bin:/opt/pmcli-tools/bin:$PATH"
export PM_TOOLS_CACHE=/cache/pm-tools
mkdir -p "$PM_TOOLS_CACHE/tmp"
# Native link intermediates belong on the existing owned disk, not 128MiB /tmp.
export TMPDIR="$PM_TOOLS_CACHE/tmp"
export GOTMPDIR="$TMPDIR"
export PM_REPO_ROOT="$repo_root"
export GOWORK=off
export GOMAXPROCS=2
export GOMEMLIMIT=768MiB
export GOFLAGS=-p=2
export SYFT_CHECK_FOR_APP_UPDATE=false
printf 'Foundation candidate: %s\n' "${GITHUB_SHA:?Actual candidate SHA required}"
printf 'Runner: %s\n' "${RUNNER_NAME:?Actual runner identity required}"

python3 scripts/install_architecture_docs_dependencies.py \
  --destination "$RUNNER_TEMP/foundation-docs-deps"
export PYTHONPATH="$RUNNER_TEMP/foundation-docs-deps"
# Invoke the existing entrypoints directly; the runner does not require make.
python3 -I -B scripts/ci/check-setup.py --root . --policy scripts/ci/setup-policy.json
python3 -I -B scripts/ci/test_setup.py
python3 -I -B scripts/ci/test_stack_fixture.py
python3 -I -B tools/test_bootstrap.py
python3 -B scripts/test_validate_architecture_docs.py
python3 -B scripts/validate_architecture_docs.py
python3 scripts/validate_architecture_docs.py --pr-event "$GITHUB_EVENT_PATH"

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/foundation-toolchain.json"
python3 tools/bootstrap.py exec go -- version
python3 tools/bootstrap.py exec go -- env GOVERSION GOOS GOARCH CGO_ENABLED CC CXX
python3 tools/bootstrap.py exec clang -- --version
python3 tools/bootstrap.py exec clang -- -print-target-triple
python3 tools/bootstrap.py exec clang -- -print-resource-dir
python3 tools/bootstrap.py exec clang -- -print-prog-name=ld
python3 tools/bootstrap.py exec bazelisk -- --batch version

# Fail on cheap source defects before builds, analysis, or shared services.
goroot=$(python3 tools/bootstrap.py exec go -- env GOROOT)
export GOROOT="$goroot"
export GOTOOLCHAIN=local
export PM_GO="$goroot/bin/go"
export PATH="$goroot/bin:$PATH"
unformatted=$(
  git ls-files -z -- '*.go' |
    xargs -0 "$goroot/bin/gofmt" -l
)
if [ -n "$unformatted" ]; then
  printf '%s\n' 'Go files require gofmt:' "$unformatted" >&2
  exit 1
fi

mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- build -o .cache/bin/nativeprobe ./build/nativeprobe
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-dependency-check ./cmd/pm-dependency-check
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/oapi-codegen github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/staticcheck honnef.co/go/tools/cmd/staticcheck
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/govulncheck golang.org/x/vuln/cmd/govulncheck
sh scripts/generate.sh --check

# Compilation and focused tests cover both Go modules without starting services.
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- test -mod=readonly -run '^$' -count=1 -timeout 10m ./...
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools test -mod=readonly -run '^$' -count=1 -timeout 10m ./...
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- test -race -mod=readonly -count=1 -timeout 10m ./internal/api ./internal/contract
CGO_ENABLED=0 PM_REPO_ROOT="$repo_root" python3 tools/bootstrap.py exec go -- -C tools test -mod=readonly -count=1 -timeout 5m ./...
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- test ./build/nativeprobe -count=1 -timeout 3m

# Tool versions record identity; the following commands perform the analysis.
.cache/bin/oapi-codegen -version
.cache/bin/staticcheck -version
.cache/bin/govulncheck -version
CGO_ENABLED=1 python3 tools/bootstrap.py exec go -- vet ./...
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools vet ./...
CGO_ENABLED=1 .cache/bin/staticcheck ./...
(
  cd tools
  CGO_ENABLED=0 ../.cache/bin/staticcheck ./...
)
CGO_ENABLED=1 .cache/bin/govulncheck ./...
(
  cd tools
  CGO_ENABLED=0 ../.cache/bin/govulncheck ./...
)
python3 scripts/ci/check-native.py
sh scripts/check-boundaries.sh

# Expensive Bazel and physical checks run only after fast source qualification.
python3 tools/bootstrap.py exec bazelisk -- --batch build //...
# Physical targets declare exclusive execution for their shared schema/roles.
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json" \
  -- python3 tools/bootstrap.py exec bazelisk -- --batch test //... \
  --test_output=errors --cache_test_results=no

# Selected artifacts only: historical caches are not the active tool graph.
python3 scripts/ci/check-dependencies.py --evidence-dir "$RUNNER_TEMP/foundation-sboms"
python3 scripts/ci/check-image-pins.py
python3 scripts/ci/benchmark.py

# Re-open and re-hash the admitted native inputs after every foundation action.
python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/foundation-toolchain-after.json"
if ! cmp -s \
  "$RUNNER_TEMP/foundation-toolchain.json" \
  "$RUNNER_TEMP/foundation-toolchain-after.json"; then
  printf '%s\n' 'foundation native/tool identity drifted during execution' >&2
  exit 1
fi
