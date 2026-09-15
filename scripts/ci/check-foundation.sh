#!/bin/sh
# Runs only reviewed foundation source on the existing dedicated trusted runner.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
: "${GITHUB_EVENT_PATH:?Actual pull-request event required}"
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
make test-setup test-docs
python3 scripts/validate_architecture_docs.py --pr-event "$GITHUB_EVENT_PATH"

python3 tools/bootstrap.py print-env > "$RUNNER_TEMP/foundation-toolchain.json"
python3 tools/bootstrap.py exec go -- version
python3 tools/bootstrap.py exec go -- env GOVERSION GOOS GOARCH CGO_ENABLED CC CXX
python3 tools/bootstrap.py exec clang -- --version
python3 tools/bootstrap.py exec clang -- -print-target-triple
python3 tools/bootstrap.py exec clang -- -print-resource-dir
python3 tools/bootstrap.py exec clang -- -print-prog-name=ld
python3 tools/bootstrap.py exec bazelisk -- --batch version
make build test vet
.cache/bin/oapi-codegen -version
.cache/bin/staticcheck -version
.cache/bin/govulncheck -version
python3 scripts/ci/check-native.py
sh scripts/check-boundaries.sh
make bazel-build bazel-test

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
