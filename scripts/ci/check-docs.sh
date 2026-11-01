#!/bin/sh
# Static repository checks for the `checks` CI job and the git hooks. They run
# no behavior test: the Gherkin suite (`bdd`) is the only behavior gate.
#
#   check-docs.sh            run every check below
#   check-docs.sh NAME...    run only the named checks (the git hooks use this)
#
# Checks: atlas docs setup python generate gofmt boundaries tools.
# CI and the Linux git hooks run this script inside the pinned job image
# (scripts/ci/in-ci-image.sh). See docs/development/ci.md.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"
all='atlas docs setup python generate gofmt boundaries tools'
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

if [ "${GITHUB_ACTIONS:-}" = true ]; then
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

pgo() {
  python3 tools/bootstrap.py exec go -- "$@"
}

step() {
  printf 'check-docs: %s\n' "$1"
}

# The tools come from the candidate. A warm cache does not guarantee that a
# previous build left them in .cache/bin.
build_tools() {
  mkdir -p .cache/bin
  for name in "$@"; do
    case "$name" in
      atlas) CGO_ENABLED=0 pgo -C tools build -o ../.cache/bin/atlas ./atlas ;;
      pm-api-generate) CGO_ENABLED=0 pgo -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate ;;
      pmgen) CGO_ENABLED=0 pgo build -o .cache/bin/pmgen ./cmd/pmgen ;;
    esac
  done
}

check_atlas() {
  step 'atlas --check'
  .cache/bin/atlas --root "$repo_root" --check
}

check_docs() {
  step 'architecture docs validator'
  python3 -I -B scripts/install_architecture_docs_dependencies.py --destination "$work/docs-deps"
  PYTHONPATH="$work/docs-deps" python3 -B scripts/validate_architecture_docs.py
  PYTHONPATH="$work/docs-deps" python3 -B scripts/test_validate_architecture_docs.py
}

check_setup() {
  step 'check-setup.py'
  python3 -I -B scripts/ci/check-setup.py --root . --policy scripts/ci/setup-policy.json
}

check_python() {
  # The developer container mounts the verified host scanner. A CI job installs
  # the pinned, checksummed scanner into its cache when it is missing.
  if ! python3 -I -B scripts/dev/secret_scan.py binary >/dev/null 2>&1; then
    if [ "${GITHUB_ACTIONS:-}" != true ]; then
      python3 -I -B scripts/dev/secret_scan.py binary
      exit 1
    fi
    python3 -I -B scripts/dev/secret_scan.py install
  fi
  for test in scripts/ci/test_setup.py scripts/ci/test_runner_admission.py \
    scripts/ci/test_stack_fixture.py tools/test_bootstrap.py scripts/dev/test_secret_scan.py; do
    step "$test"
    python3 -I -B "$test"
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

tools=
for name in "$@"; do
  case "$name" in
    atlas) tools="$tools atlas" ;;
    generate) tools="$tools pm-api-generate pmgen" ;;
  esac
done
# shellcheck disable=SC2086
[ -z "$tools" ] || build_tools $tools
for name in "$@"; do
  "check_$name"
done
step 'passed'
