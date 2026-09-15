#!/bin/sh
# Optional arguments are changed repository-relative paths, never commands.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"
if [ "$#" -eq 0 ]; then
  exec make test
fi
native=false
tools=false
setup=false
docs=false
bazel=false
for path do
  case "$path" in
    tools/*.go|tools/go.mod|tools/go.sum|tools/versions.json) tools=true ;;
    build/nativeprobe/*.go) native=true ;;
    go.mod|go.sum|build/toolchains.lock.json|Makefile|scripts/*|tools/bootstrap.py|tools/test_bootstrap.py|.github/*)
      native=true; tools=true; setup=true; docs=true; bazel=true ;;
    MODULE.bazel*|.bazel*|*BUILD.bazel|build/patches/*) bazel=true ;;
    docs/architecture/pm-server/*) docs=true; setup=true ;;
    docs/*|AGENTS.md|.agents/*|codegraph.json) setup=true; docs=true ;;
    *) native=true; tools=true; setup=true; docs=true; bazel=true ;;
  esac
done
set --
if "$tools"; then set -- "$@" test-tools; fi
if "$native"; then set -- "$@" test-native; fi
if "$setup"; then set -- "$@" test-setup; fi
if "$docs"; then set -- "$@" test-docs; fi
if "$bazel"; then set -- "$@" bazel-test; fi
printf 'Selected foundation checks:'
printf ' %s' "$@"
printf '\n'
exec make "$@"
